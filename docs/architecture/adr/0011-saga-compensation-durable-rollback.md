# 0011. Saga / compensation — durable rollback, reverse-topological order, and the honest `SagaError` partition

## Status

Accepted (milestone v0.12-M12, 2026-07-07). Records the locked M12 design (`DEC-M12`):
first-class, crash-safe saga rollback. A node declares a compensating action; on failure
the engine undoes the `Completed` nodes in reverse-topological order. Completes the
durable-workflow arc — forward durability (M9/M10, [ADR-0009](0009-durable-continuations-waiting-status.md))
+ branching (M11, [ADR-0010](0010-conditional-branching-bypassed-status.md)) + **undo**.

## Context

Through M11 the engine could run a durable, branching workflow forward, and resume it
across a crash — but it had **no undo**. A workflow that reserved inventory, charged a
card, then failed to ship had no first-class way to release the reservation and refund the
charge. Users hand-rolled it: mark upstream nodes `WithContinueOnError()`, then check
`GetNodeStatus(...) == Failed` inside a downstream "compensation" node. That workaround is
fragile (every compensation re-implements the trigger logic), not crash-safe (a crash
mid-undo leaves a half-rolled-back run with no way to resume the rollback), and silent
about partial failure (a compensation that itself fails is invisible).

The design had to add real rollback while preserving the project moat — an **embeddable,
no-server, no-DB, formally-verified** engine with **no determinism tax** — and, critically,
had to make the rollback **itself durable**: a crash during compensation must resume and
finish, not restart the forward run or strand the undo.

Two facts about the existing engine forced parts of the design:
- **The journal persists no per-node completion order.** A checkpoint is one snapshot with a
  single timestamp (`workflow_store.go`) — there is no durable record of *when* each node
  completed. So "undo in reverse completion order" cannot survive a crash.
- **The forward fail-fast path cancels the level context** on the first hard failure
  (`parallel_execution.go`). Running compensations under that already-cancelled context
  would self-destruct them.

## Decision

**Rollback is a declared, durable, best-effort reverse pass over the `Completed` nodes.**
The locked elements (`DEC-M12`):

1. **Declared compensation + two terminal statuses.** A node declares its undo with
   `NodeBuilder.WithCompensation(action)` (an `Action` or a func, symmetric with
   `WithAction`); it lands on `Node.Compensation` (nil = a rollback no-op). Two new terminal
   `NodeStatus` values, both reached only from `Completed`: **`Compensated`** (8th, FB wire
   7 — undo succeeded) and **`CompensationFailed`** (9th, FB wire 8 — undo attempted and
   failed). Additive enum widenings (the M10 `Waiting`/M11 `Bypassed` precedent),
   round-tripping through all three stores. Compensation is *declared data*, never
   synthesized or replayed — the moat.

2. **Reverse-topological order (`DEC-M12-ORDER`, code-forced).** The rollback peels the
   forward levels (`DAG.GetLevels()`) back-to-front, compensating within a level
   concurrently, bounded by `MaxConcurrency`. Reverse-topological is the **only
   crash-durable ordering**: because the journal records no per-node completion time,
   reverse-completion-order cannot be reconstructed after a crash, whereas the topological
   levels are a static property of the DAG. This ordering is what a resumed rollback
   re-derives identically.

3. **Durable rollback state + the forward-vs-rollback switch (`DEC-M12-STATE`, DUR-01).**
   The trigger sets and **persists** a run-level `rolling_back` marker (and, `DEC-M12`-ph49,
   a `triggerCause` discriminator) **before** compensating — in the same snapshot, across
   all three stores. `executeLocked` checks `IsRollingBack()` on entry and re-enters the
   **rollback drive** instead of the forward `DAG.Execute`. The drive checkpoints after each
   reverse level, so a crash mid-rollback resumes straight into the drive and finishes only
   the still-`Completed` compensable nodes. There is **no intermediate `Compensating`
   status** — at-least-once + idempotency covers the crash window instead of a per-node
   in-progress marker.

4. **Fresh context; whole-run trigger; bounded (`DEC-M12-CTX`/`TRIGGER`).** Rollback runs
   under a **fresh** `context.Background()` (with the workflow `Clock` injected) — a
   caller-cancel *triggers* rollback but must not *abort* it. It triggers on a hard
   `*ExecutionError` (fail-fast) **or** a caller cancel/deadline, gated on
   `hasCompensations()`; it deliberately does **not** trigger on `ErrSuspended`, a
   continue-on-error-only run (returns `nil`), a checkpoint/persist error, or a
   validation/load error. A DAG with no compensation takes the exact pre-M12 failure path,
   byte-for-byte. The whole pass shares a deadline (`WithRollbackTimeout`, default 5 min;
   negative = unbounded) — even a compensation that ignores its context is bounded (it runs
   in a child goroutine raced against `ctx.Done()`), so a hung compensation can never hang
   the run.

5. **At-least-once — compensations MUST be idempotent.** A crash mid-rollback re-runs the
   compensation of any node still `Completed`, so a compensation can run more than once.
   Compensations must be idempotent; the drive exposes a stable dedup handle,
   `CompensationIdempotencyKey(ctx)` = `IdempotencyKey(data, nodeName)` (workflow ID + node
   name, byte-identical across a resume) to drive downstream deduplication. Compensations
   may not park (bounded/terminal keeps the rollback finite and the TLA model tractable);
   they reuse the node's `RetryCount`.

6. **Best-effort + the honest `SagaError` partition (`DEC-M12-COMPFAIL`).** A compensation
   that fails (after retries) does not abort the pass — the node is `CompensationFailed`,
   every other compensation still runs, and the run returns a typed `*SagaError` reporting
   the **exact partition** of `Completed` nodes across `{Compensated ⊎ FailedToCompensate ⊎
   Skipped}`. `SagaError.Unwrap()` returns the trigger `Cause`, so `errors.As` reaches both
   the `*SagaError` and the original `*ExecutionError`. A `*SagaError` is returned **only**
   when at least one compensation failed; a fully-clean rollback returns the original trigger
   error — so a caller can always tell a clean rollback from a partial one. A rolled-back run
   is **never** reported as success: when the trigger cause cannot be reconstructed after a
   crash (a cancel/deadline leaves no persisted `Failed` node), the never-nil floor
   `ErrRolledBack` is surfaced.

## Consequences

- **The moat holds.** No server, no DB, no new dependency; compensation is declared data,
  never replayed. A run with no `WithCompensation` is byte-for-byte the pre-M12 path (the
  trigger machinery is gated on `hasCompensations()`, scanned only on the failure path) —
  the non-saga forward benchmark is unchanged (283 / 277 allocs), so **no determinism tax**.
- **All additive.** `WithCompensation`, `Node.Compensation`, `Compensated`,
  `CompensationFailed`, `SagaError`, `ErrRolledBack`, `WithRollbackTimeout`,
  `CompensationIdempotencyKey`, and the durable `rolling_back`/`triggerCause` fields are new
  symbols; no existing exported signature changed. The FB enum/field additions are
  backward-compatible (old buffers never carry the new wire values/fields).
- **Rollback is durable.** The `rolling_back` marker + per-reverse-level checkpoint make a
  crash during rollback resume and finish (DUR-01). The partition and the trigger cause are
  **reconstructed from durable node statuses** on resume, so a resumed rollback reports the
  whole run's honest outcome, never a false-clean `nil`.
- **Verification extended, bite-proven.** The compensation/abort arm is machine-checked in
  TLA+ (`specs/MCM12Saga.tla` with the `M12Saga` / `M12SagaCancel` / `M12SagaCompFail`
  configs), exhaustive under crashes at `MaxCrashes=1`, with the M9/M10/M11 invariants
  retained plus the saga invariants (every-Completed-compensated-once, reverse-topological
  order respected, no-uncompleted-node-compensated, crash-safe rollback, honest partition) —
  each mutation-proven to bite. gopter property suites over real `Workflow.Execute` prove the
  reverse order and the exact partition.
- **Honest scope / limits.** Rollback is at-least-once (compensations must be idempotent);
  it is whole-run (no scoped/nested saga); a compensation may not suspend; a
  `CompensationFailed` node's effect is genuinely not undone and needs operator attention
  (the `SagaError` surfaces it). Full retry-policy hardening (capped backoff, jitter,
  non-retryable classification) is deferred to a later milestone.

## Alternatives Considered

- **An intermediate `Compensating` status per node.** Rejected: at-least-once + idempotency
  already covers the crash window (a node still `Completed` on resume is simply
  re-compensated), so a per-node in-progress marker adds durable state and a re-entry case
  for no gain.
- **Undo in reverse *completion* order.** Rejected — **not crash-durable**: the journal
  records no per-node completion time, so reverse-completion-order cannot survive a crash.
  Reverse-topological order is a static DAG property and reconstructs identically on resume.
- **Abort the rollback on the first compensation failure.** Rejected: it would leave later
  compensations un-run and hide the partial outcome. Best-effort-continue + the exact
  `SagaError` partition is the honest contract — a saga that half-rolls-back must *say* so.
- **Run compensations under the caller/forward context.** Rejected: the forward fail-fast
  cancels the level context, and a caller-cancel is itself a trigger — running the undo under
  that cancelled context self-destructs it. A fresh context (bounded by `WithRollbackTimeout`)
  is the fix.
- **A per-node "on failure, suspend and compensate" trigger.** Rejected: rollback is a
  whole-run decision (a hard failure or a caller cancel), not a per-node event; compensations
  do not park.
- **Synthesized / replayed compensations (derive undo from forward code).** Rejected:
  imports the replay/determinism tax and un-analyzable behavior the project exists to avoid.
  Compensation is explicitly declared data.
- **Return `nil` for a rolled-back run whose cause can't be reconstructed.** Rejected: a
  rolled-back run must never look like success. The `ErrRolledBack` never-nil floor guarantees
  it, and journaling the `triggerCause` (ph49) recovers the true cancel-vs-failure cause
  across a crash.

## References

- `pkg/workflow/saga_rollback.go` (`driveRollback`, `compensateLevel`,
  `runCompensationWithRetry`, `reconstructOutcome`, `reconstructCause`,
  `CompensationIdempotencyKey`, `TriggerCause`, `DefaultRollbackTimeout`),
  `pkg/workflow/saga_error.go` (`SagaError`, `ErrRolledBack`)
- `pkg/workflow/node.go` (`Compensated`, `CompensationFailed` statuses, `Node.Compensation`),
  `pkg/workflow/builder.go` (`WithCompensation`)
- `pkg/workflow/workflow.go` (`finishRollback`, the `IsRollingBack` forward-vs-rollback
  switch, the trigger in `executeLocked`, `WithRollbackTimeout`, `RollbackTimeout`)
- `pkg/workflow/workflow_data.go` (`SetRollingBack`/`IsRollingBack`,
  `SetTriggerCause`/`TriggerCause` durable fields), `pkg/workflow/workflow_store.go`
  (3-store round-trip), `internal/workflow/fb/workflow/NodeStatus.go`
  (`NodeStatusCompensated` = 7, `NodeStatusCompensationFailed` = 8)
- `specs/MCM12Saga.tla` (+ `M12Saga.cfg`, `M12SagaCancel.cfg`, `M12SagaCompFail.cfg`) and
  [`specs/README.md`](../../../specs/README.md)
- [ADR-0009](0009-durable-continuations-waiting-status.md) (the durable seam this reuses),
  [ADR-0010](0010-conditional-branching-bypassed-status.md) (the status-vocabulary precedent)
- [DAG Execution → Node State Transitions](../dag-execution.md#node-state-transitions),
  [API reference → Saga / Compensation](../../reference/api-reference.md#saga--compensation-added-v0120),
  [Workflow patterns → Saga / Compensation](../../guides/workflow-patterns.md#saga--compensation-durable-rollback)
