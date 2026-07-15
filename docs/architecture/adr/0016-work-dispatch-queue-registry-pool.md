# 0016. Work dispatch — durable work-queue, type→factory registry, and a store-per-worker pool

## Status

Accepted (milestone M17 "Work dispatch", 2026-07). Records the locked M17 scope (`DISP-01..06`,
the DF-1 queue-representation fork + `DEC-M17-*`): add an **opt-in** competing-consumers dispatch
layer on the multi-process SQLite store so a pool of worker processes can drain durable work off a
shared `.db` file — a **zero-infra distributed job queue**. Fully additive over the frozen
`WorkflowStore` base and the M16 fencing model; engages only when used, so 1.0 stays earnable.

## Context

M16 shipped cross-process safety (leases + fencing) — but it was *point* claim: `Claim(workflowID,
ownerID)` takes an explicit id. There was no way to ask "give me the next claimable workflow," and
two deeper gaps the M17 intake map surfaced:

1. **The DAG is not store-recoverable.** The store persists execution **state** (the per-node
   journal), never the workflow **definition** (topology + actions). Actions are Go closures —
   non-serializable. `checkGraphIdentity` proves the DAG is a caller-supplied input validated
   against, not a store output. So a worker that pulls a workflow off a queue cannot rebuild it
   without being told how.
2. **The store knows *started* work, not *queued* work.** `ListWorkflows` returns only ids with
   persisted execution state (a row appears on the first checkpoint). Work that exists but has
   never run is invisible to enumeration — so a dispatch layer needs a **durable enqueue** concept,
   not just a query.

Together these turned "a poller" into "a queue + a registry + a pool."

## Decision

Three additive pieces, all opt-in under `WithMultiProcess()`.

### 1. A durable `work_queue` table + atomic `ClaimNext` (DF-1)

`work_queue(workflow_id PRIMARY KEY, type, input, enqueued_at, state, attempts, updated_at)` with a
**partial** index `idx_wq_claimable ON (type, enqueued_at) WHERE state='pending'` (the claim scan is
fully index-covered; terminal rows never bloat it). `state` ∈ {pending, claimed, done, failed,
cancelled}.

- **`Enqueue`** — `INSERT … ON CONFLICT(workflow_id) DO NOTHING`, returning `queued` true iff a new
  row landed. Idempotent + detectable by workflow_id (`DEC-M17-REENQUEUE`): re-submitting any
  existing id (pending/claimed/terminal) is a *visible* no-op, never a silent black hole.
- **`ClaimNext(ownerID, types…)`** — the load-bearing primitive. **One `BEGIN IMMEDIATE`
  transaction** does scan-oldest-pending → claim the M16 fencing lease (the sole safety arbiter,
  untouched) → flip `pending`→`claimed` (CAS-guarded). N contenders serialize on the write lock, so
  exactly one claims each row (constraint C1). A terminal row is never returned (the `state`
  filter — the C2 no-infinite-reclaim guard). Returns `ErrNoWork` (not an error — a poller backs
  off) when nothing is claimable.
- **Reclaim-after-death (DISP-05)** — the scan is broadened to also offer a `claimed` row whose
  lease has **lapsed** (`EXISTS leases.expiry < now`, the M16 liveness heuristic). The lease clock,
  not wall-clock, drives lapse. A lapsed-`claimed` reclaim keeps the row `claimed` under a bumped
  token and bumps `attempts`; the drive then resumes from the committed frontier. A **live**-lease
  `claimed` row fails the `EXISTS` predicate — never offered, never stolen (`claimLocked` remains
  the safety arbiter; a re-lived lease under another owner → `ErrClaimLost` → skip).
- **Terminal transitions are CAS + token-guarded.** `MarkDone`/`MarkFailed`/`MarkForRetry` flip
  only from `claimed`, and in mp mode only if this process still holds the current fencing token
  (`AND EXISTS leases.fencing_token <= held`) — so a superseded worker's terminal write is a 0-row
  no-op and can never clobber a live reclaimer's row (a structural belt to the fencing suspenders).
  A successful terminal flip also **releases** the lease (so a successor need not wait out the TTL).

### 2. A per-worker type→DAG-factory `Registry`

`Register(type, DAGFactory)` maps a DATA `type` string to CODE (`func() (*DAG, error)` — action
closures). The moat: only `type` + `input` travel through the queue as DATA; behavior stays CODE,
registered in-process. A worker claims **only registered types** (the `ClaimNext` type filter,
`DEC-M17-TYPEFILTER`), so an unregistered item stays `pending` + visible rather than claim-failing.
Register fails loud on empty type / nil factory / **duplicate** (a silent overwrite would make
dispatch order-dependent). Input is seeded as KV **on a fresh run only** — a re-claim of a workflow
with an existing journal skips the re-seed and resumes (re-seeding would blindly overwrite the
journal → lost work).

### 3. A store-per-worker `Pool`

`Pool` runs N goroutines that each open their **own** `*SQLiteStore` (`StoreFactory` returns a new
instance per call) on the shared `.db` file and loop claim→run→terminalize, backing off
(ctx-cancellable) on `ErrNoWork`. **Store-per-worker is a safety requirement, not ergonomics**
(`DEC-M17-STOREPERWORKER`): the fencing token lives in the store instance's in-memory `tokenState`;
sharing one store across workers would let a reclaim clobber the shared `tokenState[workflowID]`
and un-fence a superseded write. Separate instances isolate `tokenState` exactly as separate OS
processes do — the M16 real-multi-process model in-process, over one `leases` fencing arbiter. The
worker builds `&Workflow{DAG, WorkflowID, Store}` **without** a Locker (ClaimNext already claimed; a
`WithMultiProcessLocker.Acquire` would double-claim) — the same-instance token bridge (C3) is what
fences the drive.

## The honesty contract

**Exactly-once state PERSISTENCE, at-least-once INVOCATION.** The durable journal is written once
per level even under competing consumers (fencing). An action *body* may run **> once** on a
reclaim (a worker ran a node, died before its checkpoint committed → the re-claimer re-runs it),
**bounded by `maxAttempts`** before dead-lettering. Actions must be idempotent (the crash-resume
discipline). This is the M17 refinement of M16's "at-least-once EFFECTS": M16 was about a single
run's effects surviving a crash; M17 is about the same effect possibly re-invoked across a pool
hand-off. Stated unqualified nowhere.

## Retry vs dead-letter (fail-closed, DF-4)

Anything not provably transient is dead-lettered, so no error class spins a hot re-claim loop.
Transient infra (`ErrBusy`/`ErrIO` **outside** node logic) → retry (bounded); poison
(`*ExecutionError` — checked FIRST, even if a node's cause wraps IO) → immediate dead-letter;
topology drift (`ErrValidation`) → dead-letter; superseded (`ErrFencedOut`) → abort silently,
touch no queue state (the reclaimer owns the row); unclassified → dead-letter.

## Graceful drain (ph82-AF1)

A `Pool` drain cancels in-flight drives; a mid-drive `context.Canceled` is treated as **shutdown,
not failure** — the item is left `claimed` and the lease is **not** released, so it lapses on TTL,
the reclaim scan finds it, and a successor resumes from the committed frontier. **Graceful
shutdown does not lose in-flight work.** (Releasing the lease would drop the row out of the reclaim
scan and strand it `claimed` forever — the drain deliberately leaves it to lapse.) The poison-first
check ensures a node's own cancel/deadline (wrapped in `*ExecutionError`) is not mistaken for a
parent shutdown.

## Consequences

- **Additive + opt-in.** The frozen `WorkflowStore` base and the FlatBuffers format are untouched;
  the `work_queue` table and the dispatch surface engage only under `WithMultiProcess()` + explicit
  use. 1.0 stays earnable.
- **Formally verified.** `specs/WorkQueue.tla` models the queue lifecycle with ≥2 processes and
  nondeterministic lease-lapse; invariants **TypeOK**, **NoReclaimOfTerminal** (C2),
  **AtMostOneClaimedWriter** (the M16 fencing re-check), **NoSupersededTerminalize**, and
  **NoLostWork** hold exhaustively at a bounded config, each **bite-proven** by a seed-break
  (`WorkQueueBreakC2.cfg` / `BreakF1` / `BreakWriter` / `BreakLostWork` — remove the guard →
  the invariant Falsifies). The M16 `MPFencing.tla` + prior arms are preserved and re-run.
- **Documented limitation — version-skew (`DEC-M17-VERSIONSKEW`).** `checkGraphIdentity` is
  identity-only; all live workers must share the factory version for a type. A drifted resume is
  dead-lettered, not silently mis-run; rolling a factory change means draining the pool.

## Alternatives considered

- **Recover the DAG from the store** (store-recoverable dispatch). Rejected — impossible without
  serializing action closures, which the "workflow is DATA, actions are CODE" moat refuses. The
  registry is the forced consequence.
- **A separate lease heartbeat / a `ClaimNext` outside the fencing txn.** Rejected — the scan +
  lease-claim + state-flip must be one `IMMEDIATE` txn (C1), or two workers double-claim. Renewal
  rides the checkpoint CAS (M16), not a heartbeat.
- **Shared store across pool workers.** Rejected — un-fences superseded writes (the store-per-worker
  rationale above). The per-worker cost (one `*sql.DB` handle each) is the price of correctness.

## Related

- [ADR-0015](0015-multi-process-safety-leases-fencing.md) — the M16 leases + fencing model this
  dispatch layer rides on.
- [ADR-0014](0014-decomposed-sqlite-store.md) — the decomposed SQLite store underneath.
- [Dispatch guide](../../guides/dispatch.md) · [Persistence guide → Multi-process
  safety](../../guides/persistence.md#multi-process-safety-competing-consumers).
