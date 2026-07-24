# Dynamic Fan-out (M21)

Map a branch action over **N items discovered at run time** → N parallel branches → fan-in / aggregate.
N is unknown at `Build()`. Crash-safe, no replay tax, runs on any `Checkpointer` store (InMemory / JSON /
SQLite). See [ADR-0020](../architecture/adr/0020-dynamic-fan-out.md) for the design rationale.

## The shape

A fan-out is a **single ordinary DAG node**. At run time its `Execute` resolves the item list once
(journaling it durably), then runs the branch action once per item in the node's own
`MaxConcurrency`-bounded pool, and aggregates the results as the tail of the same `Execute`.

```go
b := workflow.NewWorkflowBuilder()

b.AddFanOut("process-rows",
    // expander: resolves the N items at run time. Runs EXACTLY ONCE across a crash+resume.
    func(ctx context.Context, parent *workflow.WorkflowData) ([]interface{}, error) {
        return []interface{}{101, 102, 103}, nil // e.g. IDs a query returned
    },
    // branchAction: runs once per item. Reads its item under FanOutItemKey.
    workflow.ActionFunc(func(ctx context.Context, d *workflow.WorkflowData) error {
        item, _ := d.Get(workflow.FanOutItemKey)
        id, _ := item.(json.Number).Int64() // see "Item typing" below
        result, err := process(id)
        if err != nil {
            return err
        }
        d.Set("branch-result", result) // read back by WithResults
        return nil
    }),
).WithResults("row-results", "branch-result").
  WithMaxWidth(500)

dag, _ := b.Build()
wf := &workflow.Workflow{DAG: dag, WorkflowID: "run-1", Store: store} // store MUST be a Checkpointer
err := wf.Execute(ctx)
```

## Load-bearing contracts

### Item typing — read the item as `json.Number`

The expansion is journaled as a JSON string so it survives a crash **store-uniformly** (a raw
`[]interface{}` does not round-trip — SQLite reloads a complex value as a JSON string). On the branch side
the item is decoded with `UseNumber()`, so:

| item in the expander | item the branch reads via `data.Get(FanOutItemKey)` |
|---|---|
| a number (`101`, `3.14`) | **`json.Number`** — call `.Int64()` or `.Float64()` |
| a string | `string` |
| an object | `map[string]interface{}` |

**Read a numeric item as `json.Number` and call `.Int64()`.** A default JSON decode into `interface{}`
yields `float64` and **corrupts an int64 item above 2^53** (a large ID, a nanos timestamp). `UseNumber()`
keeps full int64 range.

### Result typing — `WithResults` writes typed, indexed, in discovery order

```go
.WithResults(baseKey, branchKey)
```

Each branch's `branchKey` DATA value (a scalar the branch action `Set`s) is written into parent data under
`baseKey[i]` in **discovery order** (the journaled item order, NOT completion order), **typed** — an int64
reloads as an int64 on all three stores. Plus a count key `baseKey.__count__` = N. Without `WithResults` the
branches run for effect only (no indexed keys).

```go
count, _ := parent.Get("row-results.__count__") // = N
r0, _    := parent.Get("row-results[0]")        // typed result for branch 0
```

### Width cap — `WithMaxWidth` (default 1024)

```go
.WithMaxWidth(500) // non-positive restores the default DefaultFanOutMaxWidth (1024)
```

A resolved N exceeding the cap → loud `ErrFanOutMaxWidth`. Enforced **after** the expander resolves N but
**before** branch 1 (or any child ID) — an expander returning millions of items fails loud + cheap, never a
park, never a silent truncation. Note: the expansion is already journaled when the cap fires, so an over-wide
re-drive fails again deterministically (the intended permanent refusal).

### Fan-in policy — FailFast (default) vs CollectPartial

**FailFast (default):** the first branch failure fails the fan-out node and cancels in-flight / un-started
siblings. The surfaced error is the first real failure (a cancelled sibling's `context.Canceled` is the
side effect, not the cause).

**CollectPartial (`WithCollectPartial`):** all N branches run to completion (no sibling cancellation); the
node **Completes** even with k failures, exposing a partition:

```go
.WithCollectPartial()
// after the run:
count,  _ := parent.Get("row-results.__count__")  // = N
failed, _ := parent.Get("row-results.__failed__") // JSON string, e.g. "[2,5]" — the failed branch indices
r0,     _ := parent.Get("row-results[0]")         // typed result for a SUCCEEDED branch (ABSENT for a failed one)
```

To learn **why** a branch failed under CollectPartial, load that branch's child journal by its deterministic
ID (see below). A partial failure does **not** fail the node → it does **not** trigger a parent-level
[saga compensation](./error-handling.md) rollback (containment). An **external** cancel (the parent ctx is
cancelled/times out) is distinct — it propagates under both policies and the node stays non-terminal, never
recording a poisoned partition.

### Crash-resume — expansion-once

The expander runs **exactly once**. `{N + items}` is journaled durably **before** branch 1; on resume the
node reads that journal and **never re-runs the expander** (a different N would break resume). This requires a
`Checkpointer` store — a non-Checkpointer store fails loudly with `ErrFanOutRequiresCheckpointer` at run
time. Each branch is a child workflow under a deterministic ID `(parentID, nodeName, index)`, so a branch
already durably complete is a no-op on resume (crash-after-branch-k idempotency, N-wide).

## Boundary + known semantics

- **N=0** → the node completes immediately with an empty aggregate (`baseKey.__count__` = 0; under
  CollectPartial also `baseKey.__failed__` = `"[]"`). No branch runs.
- **N=1** → identical path to N>1 (no special case).
- **Single-level only.** A branch action that itself fans out is an explicit **non-goal** for this release.
- **Single-process.** Branches run in-process, `MaxConcurrency`-bounded. Cross-process fan-out is deferred
  (M22).
- A resumed run whose branches are all terminally **Failed** returns nil (the journal records the Failed
  status — inspect it, don't rely on the return being non-nil).
- A fan-out failure may leave **orphan cancelled-sibling child journals** — dead data, harmless.

## Errors

| Error | When |
|---|---|
| `ErrFanOutRequiresCheckpointer` | the store is not a durable `Checkpointer` (expansion-once has no durable N) |
| `ErrFanOutMaxWidth` | the expander resolved more branches than `WithMaxWidth` (default 1024) allows |
| `ErrFanOutResultKeyCollision` | a declared result key (`baseKey`, `baseKey[i]`, or the count/failed key) collides with a pre-existing foreign parent data key |
| `ErrValidation` (via `AddFanOut`) | a nil expander or branch action; `WithResults`/`WithMaxWidth`/`WithCollectPartial` on a non-fan-out node |
