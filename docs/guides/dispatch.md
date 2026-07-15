# Work dispatch (competing consumers → a zero-infra distributed job queue)

Added in **M17**. Dispatch is an **opt-in** layer on top of the multi-process SQLite store
([Multi-process safety](persistence.md#multi-process-safety-competing-consumers)) that turns the
library into a **zero-infra distributed job queue**: enqueue workflows durably, run a pool of
worker processes that each claim → rebuild → run → terminalize work items off the shared `.db`
file, and survive worker death via reclaim-after-death. No server, no broker — just the shared
SQLite file and the M16 fencing model.

It is built from three pieces:
- a durable **`work_queue`** table + the atomic **`ClaimNext`** primitive (the queue),
- a per-worker **`Registry`** mapping a workflow `type` → a DAG factory (the "how to build it"),
- a **`Pool`** of store-per-worker goroutines that drain registered work (the runtime).

See [ADR-0016](../architecture/adr/0016-work-dispatch-queue-registry-pool.md).

## Why a registry — actions are CODE, only `type` + `input` are DATA

The store persists a run's execution **state** (the per-node journal), never its **definition**
(the DAG topology and the node actions). Actions are Go closures — inherently non-serializable —
so a worker that pulls a workflow off the queue **cannot reconstruct its DAG from the store.** It
must be told how to build it. That is the registry's job, and it is the moat holding: the *shape*
of a workflow (`type`) and its *input* travel as DATA through the queue; the *behavior* (the DAG
factory's action closures) stays as CODE, registered in-process on each worker.

```go
reg := workflow.NewRegistry()
reg.Register("order-processing", func() (*workflow.DAG, error) {
    b := workflow.NewWorkflowBuilder()
    b.AddNode("charge").WithAction(chargeCard)
    b.AddNode("ship").WithAction(shipOrder).DependsOn("charge")
    return b.Build()
})
```

`Register(type, factory)` fails loud on an empty type, a nil factory, or a **duplicate**
registration (a silent overwrite would make dispatch depend on registration order). A `type` with
**no** registered factory is simply **un-runnable by that worker** — a worker claims only types it
has registered (the `ClaimNext` type filter), so an unregistered item stays `pending` and
**visible** in the queue (see [stuck-work visibility](#stuck-work-visibility)), never a
claim-fail-release loop.

> **Version-skew limitation (DEC-M17-VERSIONSKEW).** The resume guard (`checkGraphIdentity`) is
> **identity-only** — it checks that persisted node *names* still exist in the current DAG, not
> that the DAG's *shape* matches. So **all live workers must share the same factory version** for a
> given type. If two workers register structurally different DAGs under one type, a reclaim across
> them can drift; the drifted resume is dead-lettered (`ErrValidation`), not silently mis-run.
> Rolling a factory change out means draining the pool, not a mixed-version fleet.

## Enqueue → claim → run → terminalize

```go
// Producer: durably submit work. Idempotent by workflow_id.
store, _ := workflow.NewSQLiteStore("./shared.db", workflow.WithMultiProcess())
queued, _ := store.Enqueue("order-4711", "order-processing", []byte(`{"amount":100}`))
// queued == true if a new pending row landed; false if that id already existed (a visible no-op).
```

- **`Enqueue(workflowID, type, input)`** inserts a `pending` `work_queue` row. It is **idempotent
  and detectable** (`INSERT … ON CONFLICT(workflow_id) DO NOTHING`): re-submitting any existing id
  — pending, claimed, or terminal — returns `queued=false` with the existing row byte-unchanged
  (a `failed` id stays `failed`), never a silent black hole. `input` is an opaque payload, nullable.
- **`ClaimNext(ownerID, types…)`** atomically claims the **oldest** claimable item of a matching
  type and returns a `WorkItem` (or `ErrNoWork`). The whole scan → claim-the-lease → flip-state is
  **one `BEGIN IMMEDIATE` transaction**, so N contending workers never double-claim — exactly one
  wins each row on the write lock. A terminal row (`done`/`failed`/`cancelled`) is never returned.
- The worker rebuilds the DAG from the registry, seeds the input (on a fresh run only — see below),
  drives `Execute` **on the same store instance**, and terminalizes the row: **`MarkDone`** on
  success, **`MarkForRetry`** (bounded) or **`MarkFailed`** on failure. `RunNext` /
  `Pool` do all of this for you.

The high-level entry is `RunNext` (one item) or the `Pool` (a whole fleet); you rarely call
`ClaimNext`/`MarkDone` directly.

```go
// Consumer: run one claimable item of a registered type.
ran, err := workflow.RunNext(ctx, store, reg, "worker-1")
// ran == false + nil err  → nothing claimable right now (ErrNoWork, back off).
// ran == true             → an item was driven; err carries an Execute failure if any.
```

## The worker `Pool` — store-per-worker

For a fleet, use `Pool`: N goroutines that each open their **own** `*SQLiteStore` on the shared
`.db` file and loop `claim → run → terminalize`, backing off when the queue is empty.

```go
factory := func() (*workflow.SQLiteStore, error) {
    return workflow.NewSQLiteStore("./shared.db", workflow.WithMultiProcess())
}
pool, _ := workflow.NewPool(factory, reg, "host-A",
    workflow.WithPoolSize(8),
    workflow.WithMaxAttempts(3),
    workflow.WithPollInterval(100*time.Millisecond),
)
err := pool.Run(ctx) // blocks until ctx is cancelled AND every worker has drained cleanly.
```

> **Store-per-worker is LOAD-BEARING for safety, not ergonomics.** Each worker MUST open its own
> `*SQLiteStore` (the `StoreFactory` returns a **new** instance per call). The fencing token a
> worker holds lives in that store instance's in-memory `tokenState`. If N workers *shared* one
> store, a reclaim would clobber the shared `tokenState[workflowID]` — worker B reclaiming a
> workflow would overwrite worker A's token in the same map, and A's stale checkpoint write would
> land **unfenced** (a double-commit). Separate store instances isolate `tokenState` exactly as
> separate OS processes do — this is the M16 real-multi-process model applied **in-process**, over
> one shared `leases` table as the fencing arbiter. A factory that returns one shared instance
> silently un-fences the pool.

Each worker derives a distinct `ownerID` from the prefix (`"host-A-0"`, `"host-A-1"`, …).
`WithPoolSize` has no upper cap — the caller owns capacity policy (each worker holds one `*sql.DB`
handle; the write lock + `busy_timeout` serialize contention regardless).

## The honesty contract — exactly-once PERSISTENCE, at-least-once INVOCATION

**The durable journal is written exactly once per level even under competing consumers** (the M16
fencing CAS rejects a superseded worker's write). But **an action BODY may run more than once**:
a worker can execute a node's action, then die before its checkpoint commits, and the re-claimer
re-runs that node from the last committed frontier. So the guarantee is:

- **Exactly-once state PERSISTENCE** — the per-node journal is written once (fencing).
- **At-least-once INVOCATION** — an action body runs **≥1** time, **bounded by `maxAttempts`**
  before the item is dead-lettered.

**Actions must be idempotent** — the same discipline the crash-resume model already requires (use
`IdempotencyKey` / `CompensationIdempotencyKey` to dedupe downstream effects). Never read this as
unqualified exactly-once. Reclaim-after-death resumes from the **last committed frontier**, not a
reset to empty: a fresh run seeds its input, but a re-claim of a workflow with an existing journal
**skips the re-seed** and resumes — committed levels are not re-executed with side effects, not lost.

## Retry vs dead-letter (fail-closed)

When a drive fails, the disposition is **fail-closed** — anything not provably transient is
dead-lettered, so no error class can spin a hot re-claim loop:

| Failure | Disposition |
|---|---|
| Transient infra (`ErrBusy` / `ErrIO` — a store write-lock or IO fault, **outside** node logic) | **Retry** — requeued `claimed`→`pending` while `attempts < maxAttempts`; then dead-lettered. |
| Poison (`*ExecutionError` — a node's action logic failed) | **Dead-letter** immediately (`MarkFailed`) — re-running the same DAG on the same input re-fails. |
| Topology drift (`ErrValidation` from the resume guard) | **Dead-letter** — a drifted DAG always re-drifts (version-skew is out of scope, above). |
| Superseded (`ErrFencedOut`) | **Abort silently, touch no queue state** — a re-claimer owns the item now and will terminalize it under its own token. |
| Unclassified | **Dead-letter** (fail-closed) — never route an unknown error to retry. |

Two precedence rules make this robust. **`ErrFencedOut` is checked first** — a superseded worker
aborts before any other disposition. And **poison is checked before shutdown**: a node whose own
action returns `context.Canceled`/`DeadlineExceeded` (from its *own* internal timeout, while the
parent context stays live) surfaces as an `*ExecutionError` and is treated as poison — only a
*bare* cancel/deadline (a genuine parent-context shutdown, below) is treated as a drain rather than
a failure.

## Graceful drain does not lose in-flight work

When you cancel the `Pool`'s context, each worker finishes its **in-flight** `runNext` first, then
stops claiming new work and closes its store. For a workflow that is **mid-drive** when the cancel
lands, `Execute` returns a bare `context.Canceled` — and dispatch treats that as a **shutdown, not
a failure**: the item is left `claimed` (**not** dead-lettered), and the lease is **not** released.
The lease lapses on its TTL, the reclaim scan rediscovers the `claimed`-but-lapsed row, and a
successor resumes it from the committed frontier. **Graceful shutdown never loses in-flight work** —
a drain is indistinguishable from a crash at the queue layer, by design.

(The nuance that makes this safe: a *released* lease row would fall out of the reclaim scan and
strand the item `claimed` forever, so the drain deliberately leaves the lease intact to lapse.)

## Stuck-work visibility

Because an unregistered type or a too-old item stays `pending` forever, an operator can inspect the
queue:

```go
// Pending items enqueued at or before `olderThan` (unix-nanos; <=0 = all pending), oldest-first.
stuck, _ := store.ListPending(time.Now().Add(-1*time.Hour).UnixNano())
for _, it := range stuck {
    // it.Type unregistered by any worker? it.EnqueuedAt very old? it.Attempts climbing toward maxAttempts?
}
```

- **`ListPending(olderThan)`** returns pending items (FIFO), each with `Type`, `EnqueuedAt`, and
  `Attempts` (a climbing count = a poison item cycling toward its dead-letter). Read-only, indexed.
- **`CancelPending(workflowID)`** cancels a **pending** item (`pending`→`cancelled`, CAS-guarded).
  A cancel of a **claimed** (already-running) item is **rejected** (a detectable `flipped=false`) —
  dispatch does not interrupt mid-flight work.

## Error taxonomy

| Sentinel | Meaning | A poller should |
|---|---|---|
| `ErrNoWork` | `ClaimNext` found nothing claimable (empty / no type match / all contended away). | **Back off** (sleep), not retry — it is not an error. |
| `ErrBusy` | Transient `SQLITE_BUSY` write-lock contention. | **Retry** the claim. |
| `ErrFencedOut` | The worker was superseded by a re-claim mid-drive. | **Abort** the drive; the successor owns it. |

`ErrNoWork` is `errors.Is`-distinct from `ErrBusy`/`ErrIO` so a poller sleeps on an empty queue but
retries on transient contention.

## Requirements

- Dispatch requires a **`WithMultiProcess()`** SQLite store — `Enqueue`/`ClaimNext`/the transitions
  all reject a single-process store with `ErrValidation`.
- The `work_queue` table is **additive** — shipped tables and the FlatBuffers format are untouched;
  dispatch engages only when you use it.
