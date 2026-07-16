# 0017. Cancel-of-running — a durable intent flag, cooperative delivery, and reclaim-terminalizes

## Status

Accepted (milestone M18 "Operability", 2026-07). Records the locked cancel-of-running design
(`OBS-CAN`, `DEC-M18-CANCEL` / `-CANCEL-FENCING` / `-FORMAL`): let an operator cancel a **running**
(claimed, mid-`Execute`) dispatch workflow so it ends terminally `cancelled` and is **never
resumed**. Additive and opt-in over the M17 dispatch queue; **the executor is byte-unchanged**
(`Execute` / `dag.go` / `workflow.go` untouched across all of M18).

## Context

M17's `CancelPending` cancels only a *pending* item (a simple `pending`→`cancelled` CAS). But an
operator often needs to stop a workflow that is **already claimed and mid-drive** — a wedged run, a
mistaken submission, a poison item chewing attempts. Two hazards make this non-trivial:

1. **Cancel must not be confused with a drain or a crash.** A `Pool` drain (AF1) and a worker crash
   both surface as `context.Canceled` at the queue layer, and both must **leave the row `claimed`**
   so a successor resumes it from the committed frontier (at-least-once). An operator cancel is the
   *opposite* — the workflow must terminalize `cancelled` and **never** resume. The three are
   byte-identical `context.Canceled` at the point `Execute` returns, so something durable must tell
   them apart.
2. **Cancel must be fencing-safe.** It cannot be allowed to lose to a live `MarkDone`, nor to clobber
   a reclaimer that legitimately owns the workflow now.

## Decision

### 1. Cancel is a durable INTENT flag, not a terminal flip

`CancelRunning(workflowID)` sets a durable **`cancel_requested`** column (unix-nanos; `NULL` = not
requested) on the `work_queue` row — it does **not** terminalize. The flag is the discriminator: at
the point a drive returns `context.Canceled`, the disposition gate re-reads `cancel_requested` —
**set** → terminalize `cancelled`; **unset** → it was a drain/crash → leave `claimed`, resume later.
This is the load-bearing design choice: a *flag*, not an error-shape, because the same
`context.Canceled` can mean three different things, and only durable intent disambiguates them. (It
also means a cancel of a *poison*-flavored run still terminalizes `cancelled`, while an unset-flag
poison still dead-letters — the flag, not the error, decides.)

The column is an **additive migration** (`ALTER TABLE work_queue ADD COLUMN cancel_requested`,
tolerant of "already exists"); the M17 tables and the FlatBuffers format are untouched.

### 2. Delivery is cooperative — next level barrier + ≤500 ms

A background **ctx-watcher** goroutine (started per drive in `runNext`) polls `cancel_requested`
every **500 ms** and cancels the running `Execute`'s context on observing it. `Execute` acts on the
cancelled context at its **next level barrier**. So the cancel latency is **the next level barrier +
≤500 ms** — bounded, but **not instant**: a long single node already mid-level runs to that level's
completion; it is not interrupted. This is a **graceful cooperative cancel** (no torn state, safe by
construction), documented honestly as such — not a hard kill. (500 ms is well under the default 30 s
lease TTL, so a cancel is delivered long before the lease could lapse.)

### 3. Terminalization is fencing-gated; reclaim terminalizes too (the liveness fix)

The disposition gate flips `cancelled` via the **token-guarded** `flipTerminalFenced` — so a
superseded worker's flip is a fencing-safe 0-row no-op. Crucially, if the owner **crashes** before
delivering the cancel, the row must not strand: a **reclaimer** inside `ClaimNext`, on finding a
lapsed-`claimed` row whose `cancel_requested` is set, **terminalizes it `cancelled`** rather than
resuming it. Without this the cancelled-but-orphaned row would sit `claimed` forever (the liveness
gap the ph87 plan-gate caught). A pre-claim short-circuit likewise terminalizes `cancelled` instead
of running a freshly-claimed item whose flag is already set.

### 4. No token to request (an operator control surface)

`CancelRunning` requires **no fencing token** — any store-handle holder (a CLI, an admin process, a
pool worker) may request cancel (`DEC-M18-CANCEL-FENCING`). Setting a flag on a *separate* column is
fencing-safe by construction: it cannot lose to a live `MarkDone` nor clobber a reclaimer's
completion, because **only the token-holder terminalizes**. The request is idempotent — it sets the
flag only `WHERE cancel_requested IS NULL AND state IN ('pending','claimed')`, so a double-cancel or
a cancel-of-terminal is a detectable 0-row no-op (`requested=false`). The trust boundary is the same
as `Enqueue`/`Claim` ("holds a store handle"); terminalization stays token-gated.

## Consequences

- **Additive + opt-in + moat intact.** The `cancel_requested` column is an additive migration; the
  executor (`Execute`/`dag.go`/`workflow.go`) is byte-unchanged; single-process and non-cancel paths
  are unaffected. 1.0 stays earnable.
- **Formally verified (`DEC-M18-FORMAL`).** `WorkQueue.tla` is extended with the `cancel_requested`
  transition (frame-complete across all 9 actions) and a `resumedAfterCancel` ghost, guarding a new
  **`NoResumeAfterCancel`** invariant — *a `cancel_requested` workflow is never resumed-from-frontier*.
  It is bite-proven by `WorkQueueBreakCancel.cfg` (a `CancelGate=FALSE` break lets a reclaim resume a
  cancelled lapsed row → the invariant Falsifies) and is **non-redundant** with C2 / F1 (it catches a
  distinct property — a lapsed-claimed cancelled row is non-terminal, exactly where the
  terminal-quantified invariants are blind). The prior five invariants stayed EXACT on the enlarged
  (218-state exhaustive) space; specs-only, zero production code.
- **Honest limitation.** Cancel is **cooperative and best-effort**, bounded by the level granularity
  + the 500 ms poll — not an instant abort. A host needing sub-level cancel would need node-level
  cooperative checks, which the DAG executor does not currently expose.

## Alternatives considered

- **Terminalize directly in `CancelRunning`** (flip `claimed`→`cancelled` on the spot). Rejected —
  it would race a live `MarkDone`/reclaimer without the token it deliberately doesn't require, and it
  couldn't stop the in-flight `Execute` (which holds no queue lock). The flag + owner-terminalizes
  split is what keeps it fencing-safe and stops the running drive.
- **An error-shape discriminator** (a distinct cancel error from `Execute`). Rejected — a drain, a
  crash, and an operator cancel are byte-identical `context.Canceled`; only *durable intent* can tell
  them apart across a process boundary.
- **A hard kill / context with a shorter deadline.** Rejected — it would risk torn state mid-level;
  the level barrier is the safe cancellation point the durable model already guarantees.

## Related

- [ADR-0016](0016-work-dispatch-queue-registry-pool.md) — the M17 dispatch queue this cancels within
  (and the AF1 graceful-drain `context.Canceled` that cancel must be distinguished from).
- [ADR-0015](0015-multi-process-safety-leases-fencing.md) — the fencing model that gates
  terminalization.
- [Dispatch guide → Cancelling a running workflow](../../guides/dispatch.md#cancelling-a-running-workflow-added-m18).
