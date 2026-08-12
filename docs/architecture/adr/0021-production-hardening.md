# 0021. Production hardening — a bounded fan-out pool, per-branch retry, capped backoff+jitter, and durable first-of(signal, timer)

## Status

Accepted (milestone M22 "Production Hardening", v0.21.0-alpha, 2026-07-24). Records the locked
design for the envelope + ergonomics hardening that came out of a whole-surface proving-ground
stress pass. Every change is **additive**: the static-DAG executor and the M16/M17 dispatch and
fencing machinery stay **0-diff**, `go.mod`/`go.sum` are byte-identical (no new dependency), and no
new entry point or network surface is introduced.

## Context

M21 shipped the headline `map`-over-runtime-N fan-out ([ADR-0020](0020-dynamic-fan-out.md)). Driving
the whole public surface through a proving-ground stress pass then surfaced four production-facing
gaps — none a correctness bug in the moat, all in the *envelope* around it:

1. **The fan-out footprint cliff.** The M21 fan-out spawned **one goroutine per branch**, each
   blocking on a cap-sized semaphore. Peak live goroutines was therefore `N`, not the cap — a
   100k-item fan-out held ~100k goroutines (~70 GB), an OOM cliff on the headline feature
   (`F-PG-08`).
2. **No per-branch recovery.** A single transient branch failure failed the whole fan-out node.
   The only recourse was a full crash-resume re-drive — there was no way to re-drive **just** the
   failed branch without re-expanding the fan-out or re-running its succeeded siblings (`F-PG-10`).
3. **Unbounded retry backoff.** `RetryableAction` retried on a fixed delay with no ceiling and no
   jitter — a thundering-herd / unbounded-wait hazard under load.
4. **No durable "wait for a signal OR a deadline".** `AddWaitForSignal` parked forever; `AddTimer`
   fired unconditionally. A human-in-the-loop "approve within 24h, else escalate" needed a
   first-of(signal, timer) primitive whose deadline survives a restart.

## Decision

Ship four additive capabilities, each sized so zero values reproduce prior behavior byte-for-byte.

1. **A bounded fan-out worker pool.** The fan-out node drives its branches through a
   `min(N, MaxConcurrency)`-worker pool: it spawns **at most `cap` goroutines**, not one per item.
   `min(N, cap)` workers pull indices off a work channel; peak live goroutines is provably
   `min(N, cap)`, not `N`. Observable behavior is byte-identical to M21 — FailFast timing,
   discovery-order results, and CollectPartial partitions are unchanged. The un-fed-index discipline
   (every index ends with a real result **or** a non-nil error, never nil/nil) and the FailFast
   termination proof are preserved. (This is the M21 ADR's ph110 addendum, ratified here as the M22
   consequence.)

2. **Per-branch fan-out retry — `WithBranchRetries(count, delay, ...opts)`** on an `AddFanOut` node.
   A failed branch re-drives up to `count` times with a bounded (capped-exponential + jittered)
   backoff, **without re-expanding** the fan-out and **without re-running** succeeded siblings.
   Retry sits **below** the deterministic child-ID journal, so exactly-once **persistence** is
   untouched — it multiplies the at-least-once **execution** axis only. A sibling FailFast cancels an
   in-backoff branch within the backoff window.

3. **Capped backoff + jitter on `RetryableAction` — `WithMaxDelay(d)` / `WithJitter(f)`.** New
   additive builders that bound the exponential backoff and add jitter. Zero values reproduce the
   prior fixed-delay behavior byte-for-byte. Non-retryable classification stays the existing
   `WithRetryIf` predicate (returns false → exactly one attempt).

4. **Durable first-of(signal, timer) — `AddWaitForSignalTimeout(name, signalName, timeout)`.** Parks
   until **either** the named signal arrives **or** an absolute deadline passes — exactly one wins
   (signal-first on a same-encounter tie). The deadline is **durable-remaining across restart** (it
   is not reset on resume), and the winner is observable: a timeout sets a disposition key a
   downstream `ChoiceNode` can branch on. It is an additive sibling — `AddWaitForSignal` / `AddTimer`
   / `WithTimeout` are byte-unchanged.

## Consequences

- **Bounded footprint on the headline feature.** A wide fan-out (large `N`, small `MaxConcurrency`)
  no longer spikes goroutine count to `N`; peak is `min(N, cap)`. The OOM cliff is closed. See the
  [fan-out guide → Concurrency + memory footprint](../../guides/fanout.md#concurrency--memory-footprint--a-bounded-minn-maxconcurrency-worker-pool).
- **Idempotency is the standing invariant.** Per-branch retry **multiplies** at-least-once execution:
  retry K× and a crash-resume re-drive compound. The durable contract is unchanged and stated
  honestly — **at-least-once EXECUTION + exactly-once PERSISTENCE** (a crashed node's action re-runs
  on resume; its persisted result is written once). Prior wording implying "zero re-work" was
  corrected in the docs. Make non-idempotent branch effects idempotent (`IdempotencyKey` or a stable
  per-unit dedupe key).
- **The moat holds.** The static-DAG executor (`Execute`, `parallel_execution.go`, `workflow.go`) and
  the dispatch/fencing machinery are 0-diff; one-writer-per-workflow (M16 fencing) is preserved. 1.0
  stays earnable.
- **A few adjacent behavior changes rode the same tranche** (documented, additive-or-loud):
  `AddApproval("")` now fails loudly at `Build` with a typed validation error naming the fix
  (previously it built silently into an unsatisfiable node); an implicit OR-join now names the
  specific offending branch entries + the fix (`use AddMerge`); every `-race` Make target carries an
  explicit `-timeout 30m` (a hard ceiling on a genuine hang, with headroom for the heavy race build).
- **Throughput note (no new API).** On the SQLite dispatch path, group-commit durability
  (`NewSQLiteStore(path, WithSQLiteDurability(SQLiteBatched(K)))` in your `StoreFactory`) amortizes
  the fsync cost for a large fan-out throughput win over `Strict`.

## Alternatives Considered

- **A one-goroutine-per-branch pool with a larger semaphore.** Rejected — the footprint is inherent
  to spawning `N` goroutines regardless of the semaphore size; only a work-channel worker pool bounds
  peak goroutines by the cap.
- **Retry at the fan-out node level (re-drive the whole node).** Rejected for per-branch recovery —
  it re-expands and re-runs succeeded siblings, defeating exactly-once persistence. Placing retry
  below the child-ID journal keeps persistence single-valued.
- **A non-durable in-memory timeout for the signal wait.** Rejected — a deadline that resets on
  restart is not a durable contract; a restart near the deadline could double the effective wait or
  never fire. The remaining-deadline must be persisted.

## References

- [ADR-0020](0020-dynamic-fan-out.md) — the M21 fan-out this hardens (the worker-pool footprint fix
  is its ph110 addendum).
- [ADR-0009](0009-durable-continuations-waiting-status.md) — the durable `Waiting` continuation that
  `AddWaitForSignalTimeout` builds on.
- [ADR-0018](0018-sub-workflow-composition-and-approvals.md) — the completion-signal WAKE + SQLite
  signal mailbox the timeout-wait rides.
- [Fan-out guide](../../guides/fanout.md) · [API reference](../../reference/api-reference.md).
