# 0015. Multi-process safety — SQLite leases + monotonic fencing tokens (competing consumers)

## Status

Accepted (milestone M16 "Multi-Process Safety", 2026-07). Records the locked M16 scope
(`DEC-M16-D2/D3/D4/D6/D7`, requirements `MP-01..06`): add **opt-in cross-process safety** to
`SQLiteStore` so N worker processes can share one `.db` file as **competing consumers** —
each claiming a distinct workflow, driving it, and safely handing off after a crash — without
two live workers ever corrupting one run's journal. It is **fully additive over the frozen
`WorkflowStore` base** and engages only under `WithMultiProcess()`, so it keeps M17 = 1.0
clean and leaves the single-process path byte-for-byte unchanged.

## Context

M15 shipped `SQLiteStore` as **single-process only** (a single-writer connection + an
in-process `Locker`; see [ADR-0014](0014-decomposed-sqlite-store.md)). The in-process
`Locker` serializes concurrent drives of one `WorkflowID` *within* one OS process, but says
nothing across processes: point two processes at the same `.db` and their interleaved
load→run→checkpoint→save spans race, and a slow/stalled worker's stale write can clobber a
run another worker has already advanced. The competing-consumers pattern — a pool of workers
pulling work off a shared store — was the load-bearing missing capability.

The hazard has two distinct halves, and conflating them is the classic distributed-lock bug:

- **Liveness** — "when may another worker take over a workflow whose owner died or stalled?"
  This is a *timing heuristic*: wait long enough and re-claim.
- **Safety** — "a stale/zombie worker's write must NEVER land after it has been superseded."
  This must be an **exact, non-timing** mechanism. A lease *alone* is unsafe: a worker can
  pause (GC, VM freeze) past its lease, a second worker re-claims, and the first wakes and
  writes — a classic lease-expiry race. Fencing, not the lease, is what makes that write fail.

## Decision

Split the two halves and give each its own mechanism.

### 1. Leases carry liveness (a timing heuristic — never safety)

A `leases(workflow_id PRIMARY KEY, owner_id, expiry, fencing_token)` table. A `Claim` writes
the row with an `expiry = now + leaseTTL`; a lease is re-claimable once `now > expiry`. The
TTL is read through an **injected clock** so lapse is deterministically testable, and the
clock is **liveness-only — never a safety input** (`DEC-M16-D3`). Sizing the TTL wrong only
costs a redo, never correctness (see the operator contract below).

### 2. Monotonic fencing tokens carry safety (an exact CAS — never a timestamp)

Every `Claim` returns a fresh **monotonically increasing** `FencingToken int64` (`DEC-M16-D3`
— it is a counter, **never** a timestamp; wall-clock is never a safety input). The token is
stamped into the lease row. On **every** checkpoint write, inside the same `BEGIN IMMEDIATE`
transaction as the row `UPSERT`, the store re-reads the durable `fencing_token` and rejects
the write if the token this process holds is **strictly less than** the durable current token
— i.e. a re-claim has bumped it. This is `checkFencingLocked`: the **compare-and-swap that is
the keystone of the whole model.** A superseded (zombie) write returns `ErrFencedOut` and is
atomically rolled back — it never lands, regardless of how long the zombie slept.

The oracle is **per-level** (`DEC-M16-D4`, build-constraint C4): the violation is a *stale
overwrite of an already-advanced level*, not any historical write. A committed row written
under an old token is **legal durable state** — re-claim resumes *from* it. Only an
out-of-order overwrite by a superseded token is rejected. (This is why re-claim-after-death
resumes from the last committed frontier rather than resetting to empty — MAJOR-4.)

### 3. Renew-on-checkpoint — no separate heartbeat

The lease `expiry` is bumped **inside the checkpoint CAS transaction** (`checkFencingLocked`
renews under the held token on the success path). So a worker that is making progress
(checkpointing per level) renews its lease for free, atomically, with no background heartbeat
goroutine and no extra write. An explicit `Renew(workflowID, token)` exists for the
out-of-band lifecycle (a level that runs longer than the TTL without a checkpoint), and it
returns `ErrFencedOut` if superseded — the drive must then abort, never continue uninsured.

### 4. The executor wiring is a `Locker` adapter — the core is unchanged

`Execute` already acquires an in-process drive lease via the `Locker` seam
(`w.locker().Acquire(ctx, WorkflowID)`). Cross-process mode swaps in an MP `Locker`
(`claimLocker`) whose `Acquire` = `Claim` (which sets the per-process token state the
checkpoint CAS reads) and whose returned `release` = `Release`. The executor core does not
change; only the injected `Locker` differs. Abort-on-supersession needs no new drive logic
either: a fenced checkpoint returns `ErrFencedOut`, which propagates up through the drive as a
normal error return, so a superseded worker stops writing and returns without touching further
levels.

### 5. The token-bridge invariant (the m16-ph76-F1 footgun)

The held token lives in the **store instance's in-memory `tokenState`** (keyed by
`workflowID`), set at `Claim`. The checkpoint CAS reads `tokenState` on `Workflow.Store`. So
the `ClaimStore` the MP `Locker` claims through and the `Workflow.Store` the drive checkpoints
through **MUST be the identical `*SQLiteStore` instance** — not merely two handles on the same
`.db` file. If they differ, `Claim` sets the token on one instance and the CAS reads `held ==
0` on the other → fencing silently no-ops → a zombie write lands with no error. The sole public
MP entry point, `Workflow.WithMultiProcessLocker(ownerID)`, **derives** the `Locker` from
`w.Store`, making the mismatch **unrepresentable** — a caller cannot pass a different store
instance, so the footgun is structurally removed from the public API (it panics loudly if
`w.Store` is not a `ClaimStore`, i.e. not a `WithMultiProcess` SQLiteStore — fail-loud, never a
silently-unfenced drive). The internal raw constructor (`newMultiProcessLocker`) that takes a
`ClaimStore` directly is unexported precisely so no caller can reach the mismatch (F-M16-SEC-1).

## The honesty contract

**Exactly-once state PERSISTENCE across processes; at-least-once EFFECTS.** Fencing guarantees
the durable journal is written exactly once per level even under competing consumers — a
superseded write never lands. It does **not** make side effects exactly-once: a worker can
complete a node's side effect, then die before its checkpoint commits; the re-claimer re-runs
that node. Side effects must be **idempotent** (the same at-least-once discipline the M9
crash-resume model already requires — `IdempotencyKey`). This is stated unqualified nowhere:
never "exactly-once" without the PERSISTENCE/EFFECTS split.

## Error taxonomy — retry vs abort

Two typed sentinels, both `errors.Is`-reachable through the `%w`-wrapped drive return, and
deliberately kept distinct so a caller can key the right recovery:

- **`ErrFencedOut`** — *superseded*. A re-claim owns this workflow now. The caller **must NOT
  retry** — a live successor is driving it. Abort and move to the next workflow.
- **`ErrClaimLost`** — a `Claim` the caller did not win (a live foreign lease). The competing
  consumer that lost the race simply does not run this workflow.
- **`ErrBusy`** — *transient*. A write txn could not acquire the SQLite write lock within
  `busy_timeout` (`SQLITE_BUSY`). The caller **MAY retry** — it is contention, not
  supersession. Kept `errors.Is`-distinct from `ErrFencedOut` so "abort" and "retry" never
  collapse.

## The operator contract — sizing `WithLeaseTTL`

`WithLeaseTTL(d)` sets the lease duration (default 30s). **Size it above your longest expected
single-level compute.** A single level whose compute exceeds the TTL *without* an intervening
checkpoint may have its lease lapse, be re-claimed by another worker, and its in-flight work
**fenced + redone** — never double-committed. That is a **liveness** cost (a wasted redo), not
a safety failure: the fencing CAS guarantees the superseded work never corrupts the journal.
Undersizing wastes work; it never loses or duplicates durable state.

## Consequences

- **Additive + opt-in.** The frozen 4-method `WorkflowStore` base is untouched; `ClaimStore`
  is an optional interface, type-asserted exactly like `Checkpointer`/`Syncer`/`WorkflowQuery`.
  MP mode engages only under `WithMultiProcess()`; the single-process path is byte-for-byte
  unchanged and the determinism tax is unaffected. M17 = 1.0 stays clean.
- **Build constraints (from the ph73 spike).** MP mode opens the DSN with `_txlock=immediate`
  (C1 — DEFERRED txns deadlock on lock-upgrade with two OS-process writers); `busy_timeout` is
  a PRAGMA, not a DSN param (C2 — modernc ignores the DSN form); schema DDL runs once at
  construction (C3).
- **Formally verified.** `MPFencing.tla` models ≥2 processes with nondeterministic lease-lapse
  (no clock in the model — `DEC-M16-D3`), the fencing CAS + concurrent checkpoint, and the
  per-level `NoStaleOverwrite` invariant; exhaustive at a bounded config, bite-proven (removing
  the fencing CAS falsifies it). A **real 2-OS-process** test corroborates: two `*sql.DB` opens,
  pause past the lease → `ErrFencedOut` + a byte-checked journal, with a sustained-contention
  variant that drives past `busy_timeout` to prove `ErrBusy` surfaces and is `errors.Is`-reachable.
- **Trust model.** The `leases` row is untrusted input (a `T5`-class input-TCB extension, like
  the store itself): a forged `owner_id`/`expiry`/`fencing_token` grants no new capability —
  `expiry` is liveness-only (never authorization), and the monotonic token CAS still arbitrates
  safety. See the M16 threat-model addendum.

## Alternatives considered

- **Lease without fencing.** Rejected — unsafe under the pause-past-lease race (the whole
  reason fencing exists). A lease is a liveness hint, not a safety mechanism.
- **Timestamp as the fencing token.** Rejected (`DEC-M16-D3`) — wall-clock is never a safety
  input; clock skew across processes would make a stale write look fresh. A monotonic counter
  from the DB's own serialized `Claim` txn is the only safe token.
- **Thread the token through `WorkflowData` / a new `Checkpointer` signature.** Rejected — the
  first touches the frozen data model (and its determinism-tax budget), the second breaks the
  `func(*WorkflowData) error` checkpoint seam. Per-process `tokenState` keyed by `workflowID`
  keeps the seam intact; the DB CAS (not a cross-process shared variable) is the arbiter, so
  there is no cross-process TOCTOU.

## Related

- [ADR-0014](0014-decomposed-sqlite-store.md) — the decomposed SQLite store this builds on.
- [ADR-0009](0009-durable-continuations-waiting-status.md) — the `Locker` seam and the
  in-process single-writer lease this extends.
- [Persistence guide → Multi-process safety](../../guides/persistence.md#multi-process-safety-competing-consumers).
