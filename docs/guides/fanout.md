# Dynamic Fan-out (M21)

Map a branch action over **N items discovered at run time** → N parallel branches → fan-in / aggregate.
N is unknown at `Build()`. Crash-safe, no replay tax, runs on any `Checkpointer` store (InMemory / JSON /
SQLite). See [ADR-0020](../architecture/adr/0020-dynamic-fan-out.md) for the design rationale.

## The shape

A fan-out is a **single ordinary DAG node**. At run time its `Execute` resolves the item list once
(journaling it durably), then runs the branch action once per item **per attempt** in the node's own
`MaxConcurrency`-bounded pool, and aggregates the results as the tail of the same `Execute`. ("Per
attempt": a branch interrupted mid-flight by a crash **re-executes** on resume — see
[Crash-resume](#crash-resume--expansion-once) for the at-least-once execution contract.)

```go
b := workflow.NewWorkflowBuilder()

b.AddFanOut("process-rows",
    // expander: resolves the N items at run time. Its RESULT is journaled exactly once; its
    // EXECUTION is at-least-once (a crash before the journal flush re-runs it) — keep it
    // side-effect-free or idempotent. See "Crash-resume" below.
    func(ctx context.Context, parent *workflow.WorkflowData) ([]interface{}, error) {
        return []interface{}{101, 102, 103}, nil // e.g. IDs a query returned
    },
    // branchAction: runs once per item per attempt (re-runs on crash-resume — make its
    // effects idempotent). Reads its item under FanOutItemKey.
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

b.WithWorkflowID("run-1").WithStore(store) // store MUST be a Checkpointer
wf, err := workflow.FromBuilder(b)
if err != nil {
    log.Fatal(err)
}
err = wf.Execute(ctx)
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
b.AddFanOut("rows", expander, branchAction).
    WithResults(baseKey, branchKey)
```

Each branch's `branchKey` DATA value (a scalar the branch action `Set`s) is written into parent data under
`baseKey[i]` in **discovery order** (the journaled item order, NOT completion order), **typed** — an int64
reloads as an int64 on all four stores. Plus a count key `baseKey.__count__` = N. Without `WithResults` the
branches run for effect only (no indexed keys).

```go
count, _ := parent.Get("row-results.__count__") // = N
r0, _    := parent.Get("row-results[0]")        // typed result for branch 0
```

### Width cap — `WithMaxWidth` (default 1024)

```go
b.AddFanOut("rows", expander, branchAction).
    WithMaxWidth(500) // non-positive restores the default DefaultFanOutMaxWidth (1024)
```

A resolved N exceeding the cap → loud `ErrFanOutMaxWidth`. Enforced **after** the expander resolves N but
**before** branch 1 (or any child ID) — an expander returning millions of items fails loud + cheap, never a
park, never a silent truncation. Note: the expansion is already journaled when the cap fires, so an over-wide
re-drive fails again deterministically (the intended permanent refusal).

### Concurrency + memory footprint — a bounded `min(N, MaxConcurrency)` worker pool

The N branches run in the node's **own bounded worker pool**: exactly `min(N, MaxConcurrency)` worker
goroutines pull branch indices from a work channel, so **peak live goroutines == `min(N, cap)`, not N**. A
100k-item fan-out on a `MaxConcurrency` of 16 runs 16 branch goroutines at a time, not 100k — the memory
footprint is bounded by the cap, independent of N. (The cap defaults to `DefaultMaxConcurrency` = 16; it is
the node's `MaxConcurrency`, the same knob the level executor reads.) The observable behavior is unchanged
from a per-branch launch — same discovery-order results, same FailFast cancel timing, same CollectPartial
"all run" — only the goroutine/memory ceiling is bounded.

### Fan-in policy — FailFast (default) vs CollectPartial

**FailFast (default):** the first branch failure fails the fan-out node and cancels in-flight / un-started
siblings. The surfaced error is the first real failure (a cancelled sibling's `context.Canceled` is the
side effect, not the cause).

**CollectPartial (`WithCollectPartial`):** all N branches run to completion (no sibling cancellation); the
node **Completes** even with k failures, exposing a partition:

```go
b.AddFanOut("rows", expander, branchAction).WithCollectPartial()
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

### Per-branch retry — `WithBranchRetries` (M22)

Opt the fan-out node into per-branch retry: a failed branch re-drives up to `count` extra attempts (total
≤ `count+1`) with a **bounded backoff** (capped exponential + jitter by default), **without** re-expanding
the fan-out and **without** re-running succeeded siblings. The re-drive reuses the same deterministic
child ID, so retry rides the same no-replay path as crash-resume and the result still persists exactly-once.

```go
b.AddFanOut("rows", expander, branchAction).
    WithBranchRetries(3, 100*time.Millisecond) // ≤ 3 retries per branch, 100ms base backoff (capped+jittered)
```

Tune the policy — or mark permanent errors non-retryable — by passing `RetryableAction` option hooks:

```go
b.AddFanOut("rows", expander, branchAction).
    WithBranchRetries(3, 100*time.Millisecond,
        func(r *workflow.RetryableAction) { r.WithRetryIf(isTransient) }, // a non-retryable error → exactly 1 attempt
        func(r *workflow.RetryableAction) { r.WithMaxDelay(5 * time.Second) },
    )
```

- `count <= 0` clears the policy (back to the no-retry default; branch drive is byte-identical to no retry).
- **Interplay with the fan-in policy.** Under **FailFast** a branch's retries exhaust *before* its terminal
  error reaches the pool's sibling-cancel; a concurrent sibling's FailFast cancels this branch's in-flight
  backoff within the window (the branch sub-context is cancelled). Under **CollectPartial** each failing
  branch retries before it lands in the `__failed__` partition.
- **Retry MULTIPLIES at-least-once execution** (see [Crash-resume](#crash-resume--expansion-once)). Retry
  re-runs the branch effect K× *within* a run; a crash then re-drives the in-flight attempt again on resume.
  The two compound — a retried, crash-interrupted branch effect can fire more than K times. Idempotent
  branch effects are what makes both safe. Exactly-once **persistence** is untouched (retry sits *below*
  the deterministic child-ID journal).

`WithBranchRetries` is valid **only** on an `AddFanOut` node (else `ErrValidation`). The same
`RetryableAction` (`WithMaxDelay` / `WithJitter` / `WithRetryIf` / `WithBackoff`) is also usable directly on
an ordinary node's action — see the [API reference](../reference/api-reference.md#retryableaction).

### Crash-resume — expansion-once

The expander's **result** (`{N + items}`) is journaled **exactly once** and, once journaled, is never
recomputed: it is flushed durably **before** branch 1, and on resume the node reads that journal and
**never re-runs the expander** (a different N would break resume). But the expander's **execution** is
**at-least-once**, not exactly-once: if a crash lands in the window *after* the expander returns and *before*
the journal flush completes, resume finds no journal and **re-runs the expander**. So a side-effecting
expander (a query that mutates, a counter increment) can run more than once across a crash — keep the
expander **side-effect-free or idempotent** (this is the same at-least-once-EXECUTION + exactly-once-PERSISTENCE
contract as the branches, below — it applies to the expander too, F-PG-13). This requires a `Checkpointer`
store — a non-Checkpointer store fails loudly with `ErrFanOutRequiresCheckpointer` at run time. Each branch
is a child workflow under a deterministic ID `(parentID, nodeName, index)`, so a branch already **durably
complete** is a no-op on resume (crash-after-branch-k idempotency, N-wide).

**The execution contract — at-least-once EXECUTION + exactly-once PERSISTENCE.** This is the general
durable-execution theorem (the same contract as a plain checkpointed node — it is **not** fan-out-specific).
The engine guarantees each branch's *persisted result* exactly once, but a branch that was **in flight** when
the process crashed (its effect ran but its `Completed` checkpoint had not yet committed) **re-executes** on
resume. So a branch runs **at least once**, not exactly once. A non-idempotent branch effect (an INSERT
without a dedupe key, a charge, an unconditional send) therefore **double-acts** across a crash-resume. Make
branch effects idempotent — key them on the branch's stable unit id, or use `IdempotencyKey(data, nodeName)`
(a crash-resume-stable dedupe key). The moat is intact: crash-resume re-runs only the crashed **node**, never
the whole workflow — no replay tax, no determinism tax; only "zero re-work" was ever an overclaim.

> **Retry multiplies at-least-once.** [Per-branch retry](#per-branch-retry--withbranchretries-m22)
> (`WithBranchRetries`) re-drives a failed branch K× *within* a run; a crash then re-drives the in-flight
> attempt again on resume. The two compound — a retried, crash-interrupted branch effect can fire more than
> K times. Idempotent branch effects are the invariant
> that makes both safe.

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
