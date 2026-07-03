# 0009. Durable continuations — the `Waiting` status and "suspend is a crash you chose"

## Status

Accepted (milestone v0.10-M10, 2026-07-01). Records the locked M10 design (`DEC-M10`):
the model by which a workflow suspends on an external event and resumes later. Builds on
the M9 crash-resume core (the optional `Checkpointer` seam). `ChoiceNode` / data-driven
routing was cut from M10 scope and **deferred to M11** (`DEC-M10-P38-DEFER`) — it is not
part of this decision.

## Context

M9 made the DAG executor durable: a run is checkpointed at each completed level barrier,
and resume is re-running `Workflow.Execute` with the same `WorkflowID`+store (completed
nodes skipped and rehydrated). M10 ("Durable Continuations") turns that durable executor
into a durable *workflow engine*: a workflow must be able to **wait** — sleep a real-world
duration, block on an inbound signal (a human approval, an external event), or block on a
data condition — for arbitrarily long, across process restarts, without holding a process
open.

The constraint that shaped the design: preserve the project's moat — an **embeddable,
no-server, no-DB** engine whose core is **formally verified**, delivering durability with
**no determinism tax** (the workflow is static DAG *data*, never replayed code). A naive
"wait" implementation (a live `time.Timer`, a blocking receive, a background scheduler
goroutine that must always be running) would import exactly the server/daemon dependency
and the replay tax the project exists to avoid. The verification question was load-bearing:
the `Waiting` state is precisely where a liveness property can go **hollow** (a parked
workflow trivially "makes progress" by resting forever).

## Decision

**Suspend is modeled as "a crash you chose" — it reuses the M9 seam, adding no new
persistence path and no mandatory background service.** Four locked elements:

1. **A 6th, non-terminal `NodeStatus`: `Waiting`.** A node that must wait *parks*: its
   action returns the internal `ErrSuspended` sentinel, and the executor records the node
   `Waiting` instead of treating the return as a failure. `Waiting` is **non-terminal and
   non-failing** — it never trips fail-fast, never causes dependents to be `Skipped`, and
   is never counted as terminal. It is treated as runnable (like `Pending`) on the next
   entry. The FlatBuffers wire schema gains value 5 for `Waiting` (additive; old buffers
   never carry it).

2. **Park → drain → checkpoint → return `ErrSuspended`.** When a node parks, its level
   drains to the barrier normally; the M9 `Checkpointer` flush persists the state
   **carrying the `Waiting` status** (and, for a timer, the absolute `fireAt`); then
   `Workflow.Execute` returns `ErrSuspended` (not `nil`, not `*ExecutionError`). The
   process may exit with zero further compute. Suspending without a `Checkpointer` store
   is a real error (`ErrSuspendRequiresCheckpointer`) — there would be nowhere durable to
   park.

3. **Wake = re-enter the M9 resume path.** Waking is just re-running the executor on the
   same `WorkflowID`+store; the `Waiting` node re-runs its action, which re-checks its
   event and either re-parks or fires. Three host-driven entry points do this: plain
   `Workflow.Execute` (startup), `Workflow.Tick(now)` (due timers), and
   `Workflow.DeliverAndResume(sig)` (signals). **There is no mandatory background
   goroutine** — the host drives waking on its own schedule.

4. **Declared suspension nodes; timers are data; signals are decoupled.** Suspension is a
   *declared node type* (`AddTimer`/`AddWaitForSignal`/`AddWaitForCondition`, statically
   marked via a package-internal capability marker), not a runtime surprise from an
   arbitrary action. A **durable timer** persists an *absolute* due-time
   (`clock.Now()+d`, frozen at the first encounter, read through an injectable `Clock`) —
   so it survives crash/suspend, an overdue timer fires immediately on resume, and no
   wall-clock value is ever recorded or replayed (no determinism tax). A **signal** is
   delivered to a durable mailbox that lives **outside** the `WorkflowData` snapshot (so an
   external deliverer can never clobber a running checkpoint); delivery is decoupled from
   any running process (**at-least-once**), and consuming applies the payload
   **idempotently** in the order take → apply → `Completed` → checkpoint → ack. A `Locker`
   lease serializes concurrent drives of one `WorkflowID` within a process.

## Consequences

- **The moat holds.** No server, no DB, no background service, no replay: waiting is
  persisted data re-derived on entry. A workflow that uses no suspension node behaves
  exactly as M9.
- **All additive.** `Waiting`, the suspension nodes, `Clock`, `Locker`, `Signal`,
  `SignalStore`, `Checkpointer`, and the new sentinels are new symbols; no existing
  exported signature changed. `Checkpointer`/`SignalStore` are optional type-asserted
  interfaces (a store that omits them keeps prior behavior).
- **Verification stays honest — the hollow-liveness trap is dodged.** The M10 model
  (`specs/M10DurableExecutor.tla`) refines the M9 model with `Waiting` and
  `Suspend`/`Wake`/`FireTimer`/`SendSignal`, and uses a **`WakeReady`-conditioned `Stuck`
  arm**: a parked node whose wake event *has* fired is `Stuck` (the engine MUST wake it),
  while a node still legitimately waiting is allowed to rest. It is TLC-checked
  exhaustively at `MaxCrashes=1` with all M9 safety invariants retained plus
  `WaitingSound`, `NoDoubleFire`, `NoSignalLost`, `NoDoubleApply`, `SuspendPreservesJournal`,
  and `NoResurrection`, and the `WokeOnlyWhenReady` temporal property — each mutation-proven
  to bite.
- **Honest scope carried forward from M9.** Delivery is at-least-once, so signal/timer
  side effects must be idempotent (drive downstream dedupe with `IdempotencyKey`).
  Cross-*process* serialization of one `WorkflowID` is the host's responsibility (the
  in-process `Locker` covers within-process); exactly-once is deferred to a future
  same-transaction (SQLite) store. A plain resume of a hard-failed run can return `nil`
  while the failed node stays terminal `Failed`/observable — inspect node status, not just
  the `Execute` return (the failure is never resurrected; `Tick` refuses a hard-failed
  run outright).

## Alternatives Considered

- **A background scheduler / timer-wheel goroutine that is always running.** Rejected: it
  is the daemon the project exists to avoid, and it makes durability depend on process
  liveness. Waking is host-driven instead (`Tick`/`DeliverAndResume`); an optional
  contrib poller may ship later but must never become mandatory.
- **A live `time.Timer` / blocking receive per wait.** Rejected: does not survive a crash,
  and blocks a process for the wait duration. The timer is persisted absolute data.
- **Event-sourcing / replay of workflow code (the Temporal/DBOS-code model).** Rejected:
  imports the determinism tax. The workflow is static DAG data; we re-derive, never replay.
- **A runtime `ErrSuspend` from any action (not a declared node type).** Rejected: makes
  suspension an implicit, un-analyzable property; the declared-node marker keeps the
  suspend capability static and visible to the executor and the formal model.
- **Signals stored inside the `WorkflowData` snapshot.** Rejected: a wholesale snapshot
  rewrite at each checkpoint would race an external deliverer; the mailbox is an
  independent channel outside the snapshot.
- **Ship `ChoiceNode` (data-driven routing) in M10.** Deferred to M11 — the common
  converge-after-choice pattern needs a real OR-join, a materially larger feature that
  touches fail-fast interaction and the formally-verified core (`DEC-M10-P38-DEFER`).

## References

- `pkg/workflow/node.go` (`Waiting` status), `pkg/workflow/suspend.go` (`ErrSuspended`,
  `ErrSuspendRequiresCheckpointer`)
- `pkg/workflow/timer.go` (`NewTimerNode`, `Tick`, `DueTimers`), `pkg/workflow/clock.go`
  (`Clock`, `SystemClock`, `FakeClock`)
- `pkg/workflow/signal.go` + `pkg/workflow/signal_store.go` (`NewWaitForSignalNode`,
  `NewWaitForConditionNode`, `Signal`, `SignalStore`, `DeliverSignal`, `DeliverAndResume`)
- `pkg/workflow/lease.go` (`Locker`, `NewInProcessLocker`)
- `specs/M10DurableExecutor.tla` (+ `MCM10DurableExecutor.tla`) and
  [`specs/README.md`](../../../specs/README.md)
- [ADR-0007](0007-error-taxonomy.md) (the error-taxonomy this extends with the
  durable-continuation sentinels)
- [DAG Execution → Suspend and resume](../dag-execution.md#suspend-and-resume-durable-continuations),
  [Persistence guide → Durable Continuations](../../guides/persistence.md#durable-continuations-suspend--resume)
