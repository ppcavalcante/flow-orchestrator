# Receipt B2 — targeted host-local submit / admit / drive (shared caps + fencing)

**Re:** `openai-workflow/docs/plans/security-workflow-architecture/engine-input-aware-factory-handoff.md` §14.B2
**Status: DELIVERED.** A minimal designated-host binding — **separate from E-01** — so a specifically requested
run executes on **its** host with that host's runner, and generic workers cannot take it. Additive; the new
ownership/persistence contract is documented below. Same-model self-review, not independent certification.

## 1. Exact subject

- **Engine commit:** `a11fa5a` on `main` (built on the B1 commit `6ea998d`).
- **Module version:** `v0.22.4-alpha.0.20260907165011-a11fa5adabed`.
- **Consume:** `go get github.com/ppcavalcante/flow-orchestrator@a11fa5a`.
- Files: `workflow_store_sqlite.go` (schema + migration), `workflow_store_sqlite_workqueue.go`
  (`EnqueueForHost`, `ClaimNextForHost`, the host predicate), `workflow_dispatch.go` (`RunNextForHost`),
  `workflow_pool.go` (generic caller passes `host=""`), `dispatch_host_local_test.go` (witnesses).

## 2. Public API (minimal)

```go
// Admit a run BOUND to a designated host — admission == enqueue (no theft window). host must be non-empty.
func (s *SQLiteStore) EnqueueForHost(workflowID, typ string, input []byte, host string) (queued bool, err error)

// Claim/reclaim ONLY rows bound to `host` (never generic or other-host rows). host must be non-empty.
func (s *SQLiteStore) ClaimNextForHost(ownerID, host string, typeFilter ...string) (WorkItem, error)

// Drive the next run bound to `host` (claim + build + execute + terminalize), host-scoped. host must be non-empty.
func RunNextForHost(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID, host string) (ran bool, err error)
```

No private SQL is exposed; the application does not copy `runNext`. The existing `Enqueue` / `ClaimNext` /
`RunNext` / `Pool` are the generic path and are unchanged (a generic claim now simply skips host-bound rows).

## 3. The new ownership / persistence contract

- **New column:** `work_queue.owner_host TEXT` (nullable), added with the established idempotent `ALTER TABLE
  ADD COLUMN` pattern. `NULL` = generic (any worker; every pre-B2 row backfills to `NULL`). Non-`NULL` =
  host-bound. This is the only persisted-format change and it is backward-compatible: an old DB opens and its
  rows are all generic.
- **Admission before theft.** `EnqueueForHost` writes `owner_host` in the same INSERT that creates the row, so
  the row is host-bound the instant it exists — there is no window in which a generic worker could claim it.
- **Generic exclusion.** The generic `ClaimNext` scan gained `AND owner_host IS NULL`. Because every pre-B2 row
  has `owner_host = NULL`, generic behavior is byte-unchanged; only new host-bound rows are excluded from
  generic claims and reclaims.
- **Host scoping (claim + reclaim).** `ClaimNextForHost(host)` scans `AND owner_host = host`, for both the
  pending-claim and the lapsed-`claimed` reclaim paths. So a designated host drives only its own work, and a
  lapsed host-bound run is reclaimable **only by the same host** — its durable `owner_host` identity persists
  until that host returns. A dead host's work is therefore never silently executed elsewhere (and no claim
  asserts a dead host's resources are available on another worker).
- **Everything else reused unchanged.** The shared concurrency caps (running-slot COUNT over `claimed ∧
  ¬parked`), the M16 fencing token on claim/checkpoint/terminalize, `CancelPending`/`CancelRunning`, the DF-4
  bounded-retry disposition, and the durable terminal/recovery lifecycle all apply identically to a host-bound
  run — the host predicate is the *only* difference from a generic claim.
- **Recovery of a permanently-dead host.** By contract, host-bound work waits for its host. To hand it to
  another host or drain it, an operator re-submits or cancels it (the ordinary lifecycle) — the engine will not
  silently migrate ownership. (An explicit rebind API was intentionally not added; say the word if you want one.)

## 4. Completion proof (per §14.B2)

All in `dispatch_host_local_test.go`, real SQLite:

- **Only the designated host executes the requested run; other work not substituted** —
  `TestHostLocal_GenericWorkerCannotStealHostBound`: a generic worker takes only the generic run and leaves the
  (older) host-bound row pending; a second generic drive returns `ran=false`; the host then drives its own run;
  another host cannot claim it. `TestHostLocal_RaceDesignatedVsGeneric`: a generic worker and the designated
  host contend over the same DB concurrently — the generic drive returns `ran=false`, only the host executes.
- **Shared caps, no bypass** — `TestHostLocal_SharedCapBackpressure`: with a per-type cap of 1 held by a
  generic occupant, a host-bound run is backpressured (stays pending) until the shared slot frees.
- **Local interruption / reclaim + stale-owner fencing** — `TestHostLocal_HostScopedRecoveryAndFencing`: a
  host-bound run whose host stalls is NOT reclaimable by a generic worker or another host; the same host (a
  restarted worker) reclaims it under a bumped token; the stale worker's late terminalization is fenced (a
  no-op that cannot flip the successor's row); the successor completes it.
- **Cancellation** — `TestHostLocal_Cancellation`: a pending host-bound run is cancelled through the ordinary
  operator surface and never executed.
- **Guard rails** — `TestHostLocal_EmptyHostRejected`: all three entry points reject an empty host with
  `ErrValidation`.

## 5. Compatibility

- **Additive.** Three new methods + one nullable column. Existing signatures, the generic dispatch behavior,
  the journal, the fencing arbiter, and the cap semantics are unchanged. The `runNext` internal gained a `host`
  parameter (generic callers pass `""`); no exported signature changed.
- The generic `ClaimNext` predicate change (`AND owner_host IS NULL`) is behavior-preserving for every
  pre-existing row.

## 6. Explicitly NOT included (per §14.B2 non-goals)

No PostgreSQL/backend port, no arbitrary per-run workflow type names, no monetary-budget engine, no generic
finalizer/outbox, no copied `runNext`, no live external operation. The CLI/runner integration (which host runs
which run, fixtures/paths/timeout selection) is the application's; this receipt delivers the engine primitive
that makes a run host-bound and drivable only by that host.

## 7. Gate

```text
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^TestHostLocal_' -count=1                         → ok
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^(TestHostLocal|TestRunNext|TestClaimNext|TestPool|TestCancel|TestKillstorm|TestCaps|TestQueueSubWorkflow|TestParkedSubWorkflow|TestQueueChild|TestQueuedChild|TestInputAware|TestERead)' -race -count=1  → ok (no race report; core ClaimNext change regression-checked)
GOTOOLCHAIN=local go vet ./pkg/workflow/                                                           → clean
golangci-lint run pkg/workflow/                                                                    → 0 issues on the new/changed files
```

The authoritative amd64 gate runs on the push; bind any CI receipt to `a11fa5a`.
