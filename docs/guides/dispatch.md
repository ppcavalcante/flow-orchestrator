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

## Operator observability (added M18)

M18 adds an opt-in, type-asserted **`Observability`** read-model over the dispatch queue — the
operator's "what's happening" view. It is a **purely additive read side**: the write path
(`Enqueue`/`ClaimNext`/the terminal transitions) is byte-unchanged. Four of the five granular
methods are **one atomic SELECT** each; `WorkflowStatus` composes **two** (its dispatch row + its
node tally), so its two halves can reflect different instants under a concurrent *cross-process*
writer — use `Snapshot()` when you need every part to agree (it wraps all its component queries in
one consistent read-transaction). Requires an mp store.

```go
// A WithMultiProcess SQLiteStore implements Observability (type-assert like Checkpointer/ClaimStore).
obs := store.(workflow.Observability)

counts, _ := obs.QueueCounts("")            // per-state row counts: {"pending":3,"claimed":2,"done":10,…}
inflight, _ := obs.InFlight()               // claimed items + lease owner + freshness (LeaseLive)
stuck, _ := obs.StuckWork(cutoff, regTypes) // wedged items, each with a StuckReason
ws, _ := obs.WorkflowStatus("order-4711")   // one workflow's dispatch state + per-node journal tally
workers, _ := obs.WorkerHealth()            // per-owner held/live lease counts

// One mutually-consistent snapshot (BEGIN DEFERRED read-txn — all parts reflect one instant).
snap, _ := store.Snapshot(cutoff, regTypes) // QueueSnapshot{Counts, InFlight, Stuck, Workers}
```

- **`QueueCounts(typ)`** — per-`state` counts (`typ==""` = whole queue; else filtered to that type).
- **`InFlight()`** — the `claimed` items, each with `OwnerID` + `Expiry` + `LeaseLive` (`expiry >= now`
  on the injected lease clock — a *liveness* read, never a safety input; the read-model only reports it).
- **`StuckWork(olderThan, registeredTypes)`** — wedged items classified by `StuckReason`:
  `unregistered_type` (no worker can build it), `too_old_pending` (enqueued before the cutoff, still
  unclaimed), or `lapsed_claimed` (a dead worker's abandoned claim awaiting reclaim). `registeredTypes=nil`
  skips the unregistered-type check.
- **`WorkflowStatus(id)`** — one workflow's dispatch fields (`State`/`Attempts`/`OwnerID`) plus its
  per-node-status journal tally (`NodeCounts`); `Queued=false` when the id has no `work_queue` row.
- **`WorkerHealth()`** — per-owner `TotalHeld` / `LiveHeld` lease counts.
- **`Snapshot(olderThan, registeredTypes)`** — the four aggregate views in **one** `BEGIN DEFERRED`
  read-txn, so a concurrent writer's flip is either fully visible or fully absent across all parts —
  never torn. Use it when you need the counts, in-flight list, stuck items, and worker health to *agree*.

## Cancelling a running workflow (added M18)

M17's `CancelPending` only cancels a *pending* item. M18 adds **`CancelRunning(workflowID)`** to cancel
a **claimed, mid-`Execute`** workflow — it ends terminally `cancelled` and is **never resumed**.

```go
requested, _ := store.CancelRunning("order-4711")
// requested == true if this call set the cancel flag; false if already-cancelled or already-terminal.
```

**How it works — a durable intent flag, not a terminal flip.** `CancelRunning` sets a durable
`cancel_requested` column on the `work_queue` row; it does **not** itself terminalize. That intent flag
is what distinguishes an **operator cancel** (→ terminal `cancelled`, never resumed) from an **AF1
graceful drain or a crash** (→ leave `claimed`, resume from the committed frontier later) — the two are
byte-identical `context.Canceled` at the queue layer, so the durable flag is the only thing that tells
them apart. The actual terminalization is done by the token-holding owner (or, if the owner crashed, by
a reclaimer inside `ClaimNext`) — always **fencing-token-gated**, so a cancel can never clobber a live
worker's completion.

**Delivery is cooperative, not an instant kill (document this honestly).** A background watcher polls
the flag every **500 ms** and cancels the running `Execute`'s context; `Execute` observes it at its
**next level barrier**. So cancel latency is **the next level barrier + ≤500 ms** — a long single node
already mid-level runs to that level's completion; it is not interrupted. This is a graceful cooperative
cancel (safe, no torn state), not a hard abort.

**Authority — no token required (an operator surface).** Any holder of a store handle (a CLI, an admin
process, a pool worker) may call `CancelRunning` — setting a flag on a separate column is fencing-safe by
construction (it cannot lose to a live `MarkDone` nor clobber a reclaimer's completion; only the
token-holder terminalizes). It is idempotent: a double-cancel or a cancel-of-terminal is a detectable
0-row no-op (`requested=false`). Formally, `WorkQueue.tla` carries a machine-checked **`NoResumeAfterCancel`**
invariant — a `cancel_requested` workflow is never resumed-from-frontier. See
[ADR-0017](../architecture/adr/0017-cancel-of-running-workflow.md).

## Dispatch metrics (added M18)

Optional in-process **`DispatchMetrics`** — event counters over the dispatch layer, distinct from the
M14 per-workflow-run metrics (`Workflow.MetricsConfig` / `metrics.Collector` — that path is unchanged;
see the [Observability guide](observability.md)). **`nil` = zero-cost**: unset, every increment site is
a single nil check.

```go
dm := workflow.NewDispatchMetrics()
store, _ := workflow.NewSQLiteStore("./shared.db",
    workflow.WithMultiProcess(),
    workflow.WithDispatchMetrics(dm), // opt-in; omit for zero-cost
)
// … run the pool …
dm.ReclaimAfterDeath(); dm.FenceRejections(); dm.SupersededAborts(); dm.RetriesAttempted(); dm.DeadLetters()
```

The five counters are **events, not durable state** — `reclaimAfterDeath`, `fenceRejections`,
`supersededAborts`, `retriesAttempted`, `deadLetters` — and they **reset on process restart** (this is
correct: they count what *this* process observed, not a durable tally). For OpenTelemetry, wire the
**`OTelDispatchBridge`** — it exposes the five counters as observable event counters **plus** live state
gauges read from the read-model (`QueueCounts`/`InFlight`), all through the OTel **API** (the host owns
the SDK/exporter):

```go
bridge, _ := workflow.NewOTelDispatchBridge(store.(workflow.Observability), dm, meterProvider)
defer bridge.Shutdown(ctx)
```

## Requirements

- Dispatch requires a **`WithMultiProcess()`** SQLite store — `Enqueue`/`ClaimNext`/the transitions
  all reject a single-process store with `ErrValidation`.
- The `work_queue` table is **additive** — shipped tables and the FlatBuffers format are untouched;
  dispatch engages only when you use it.
