# Receipt B2 — targeted host-local submit / admit / drive (shared caps + fencing)

**Re:** `openai-workflow/docs/plans/security-workflow-architecture/engine-input-aware-factory-handoff.md` §14.B2 + §15.R1/R2/R5
**Status: DELIVERED (rev 2, after the §15 recheck).** A designated-host binding — **separate from E-01** — so a
specifically requested run executes on **its** host with that host's runner, and generic workers cannot take it.
Additive; the new ownership/persistence contract is documented below. Same-model self-review, not independent
certification.

> **Rev 2 — §15 recheck.** The §15 verdict was FIX FIRST on one blocking gap plus contract-doc corrections:
> - **R1 (blocking, B2-SEC-1)** — `RunNextForHost`/`ClaimNextForHost` were host-scoped **FIFO** (oldest-first), so
>   they could execute an *older* same-host run instead of the requested one. **Fixed** with a by-workflow-ID path
>   (`ClaimSpecificForHost` / `RunSpecificForHost`) that fails closed with no fallback (§2, §4).
> - **R2** — the mixed-version rollout precondition is now **declared** (§7 below + dispatch guide + CHANGELOG).
> - **R5** — the ownership/recovery contract is corrected: **binding at enqueue vs capped admission at claim**;
>   `host` is a routing identity, not an authenticated OS host; a duplicate is a detectable no-op (not a rebind),
>   and the binding is now inspectable via `QueuedSubmission.OwnerHost`; resubmit-on-another-host is a new
>   identity; cancellation and the legacy direct-drive bypass are stated precisely; root binding does not
>   propagate to queued children or schedule-fired rows.

## 1. Exact subject

- **Engine commit:** `ef51e67` on `main` (B2 landed at `a11fa5a`; R1 specific-run + R5 `OwnerHost` at `2246b97`).
- **Module version:** `v0.22.4-alpha.0.20260908053020-ef51e67df4db`.
- **Consume:** `go get github.com/ppcavalcante/flow-orchestrator@ef51e67`.
- Files: `workflow_store_sqlite.go` (schema + migration), `workflow_store_sqlite_workqueue.go`
  (`EnqueueForHost`, `ClaimNextForHost`, `ClaimSpecificForHost`, the host/wantID predicates),
  `workflow_dispatch.go` (`RunNextForHost`, `RunSpecificForHost`), `workflow_store_sqlite_eread.go`
  (`QueuedSubmission.OwnerHost`), `workflow_pool.go` (generic caller), `dispatch_host_local_test.go` (witnesses).

## 2. Public API (minimal)

```go
// Bind a run to a designated host — the binding is set atomically at enqueue (no theft window). host non-empty.
func (s *SQLiteStore) EnqueueForHost(workflowID, typ string, input []byte, host string) (queued bool, err error)

// Claim/reclaim the host's OLDEST bound run (host-scoped FIFO); never generic or other-host rows. host non-empty.
func (s *SQLiteStore) ClaimNextForHost(ownerID, host string, typeFilter ...string) (WorkItem, error)
func RunNextForHost(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID, host string) (ran bool, err error)

// Drive the SPECIFICALLY REQUESTED run by id, and nothing else — fails closed (ErrNoWork, no fallback) when the
// requested run is absent/terminal/other-host/claimed-live/over-cap. host + workflowID non-empty. (§15 R1.)
func (s *SQLiteStore) ClaimSpecificForHost(ownerID, host, workflowID string) (WorkItem, error)
func RunSpecificForHost(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID, host, workflowID string) (ran bool, err error)
```

No private SQL is exposed; the application does not copy `runNext`. The existing `Enqueue` / `ClaimNext` /
`RunNext` / `Pool` are the generic path and are unchanged (a generic claim now simply skips host-bound rows).

## 3. The new ownership / persistence contract

- **New column:** `work_queue.owner_host TEXT` (nullable), added with the established idempotent `ALTER TABLE
  ADD COLUMN` pattern. `NULL` = generic (any worker; every pre-B2 row backfills to `NULL`). Non-`NULL` =
  host-bound. This is the only persisted-format change and it is backward-compatible: an old DB opens and its
  rows are all generic.
- **Binding at enqueue; capped *admission* at claim (§15 R5).** `EnqueueForHost` writes `owner_host` in the same
  INSERT that creates the row, so the row is host-**bound** the instant it exists — no window for a generic
  worker to claim it. This is the *binding*, not execution admission: whether the run may *run* is still gated by
  the shared cap at claim time (an at-cap run stays pending). "admission == enqueue" would be wrong.
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

**Ownership / identity / recovery — precise contract (§15 R5):**

- **`host` is a caller-supplied ROUTING identity, not an authenticated OS host.** The engine does not verify host
  identity; stable, unique host ids and trusted embedders are required. Unique lease owners still apply.
- **A duplicate `EnqueueForHost` returns `(false, nil)`** — a detectable no-op that does **not** change the
  existing row's host/type/input/state. It is **not** a rebind. Inspect the durable binding with
  `InspectSubmission(id).OwnerHost` (added at `2246b97`) to fail closed on identity before a host-local drive.
- **"Resubmit on another host" is a NEW run identity, not a rebind** of the same id. A new id is a new execution
  identity; do not treat it as transparent recovery, and do not ignore the old run's possible execution.
- **Cancellation.** `CancelPending` on a *pending* host-bound row terminalizes it (`cancelled`). `CancelRunning`
  on a *claimed* row records **intent** only — the owner (or an eligible same-host reclaimer) still completes the
  lifecycle. Cancellation does **not** instantly drain a permanently-unavailable host's claimed work.
- **Legacy direct-drive bypass (pre-existing limitation).** The legacy direct `Claim` / `WithMultiProcessLocker(...)`
  `.Execute` path runs outside the host-bound queue/cap protocol, so it can drive a host-bound id ignoring the
  binding and caps. This is a *pre-existing* protocol limitation, not a new B2 exploit; application cutover must
  **not** use it as a host-binding workaround.
- **Binding scope.** Binding a root run does **not** propagate `owner_host` to its queued sub-workflow children
  or schedule-fired rows — those existing enqueue paths remain unbound (generic). Route child/scheduled locality
  explicitly if required; this feature alone does not prove whole-run-tree locality.

## 4. Completion proof (per §14.B2)

All in `dispatch_host_local_test.go`, real SQLite:

- **The SPECIFICALLY requested run executes, not older same-host work (§15 R1/B2-SEC-1)** —
  `TestB2Security_SpecificRequestedRun` (the consumer's exact reproducer): two same-host/same-type pending rows,
  `RunSpecificForHost(...,"requested")` executes ONLY `requested`; the older row stays untouched.
  `TestB2Security_SpecificRun_FailsClosed` covers the fail-closed cases (absent id, wrong-host binding —
  validated via `InspectSubmission(...).OwnerHost`, terminal id, over shared cap): each returns `ran=false` with
  no fallback to other work. `TestB2Security_SpecificRun_ReclaimByID`: the same host reclaims a lapsed requested
  run by id.
- **Only the designated host executes; other work not substituted** —
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

- **Additive.** Five new host-facing entry points (`EnqueueForHost`, `ClaimNextForHost`, `RunNextForHost`,
  `ClaimSpecificForHost`, `RunSpecificForHost`) + one nullable column. Existing signatures, the generic dispatch behavior,
  the journal, the fencing arbiter, and the cap semantics are unchanged. The `runNext` internal gained a `host`
  parameter (generic callers pass `""`); no exported signature changed.
- The generic `ClaimNext` predicate change (`AND owner_host IS NULL`) is behavior-preserving for every
  pre-existing row.

## 5b. Mixed-version rollout precondition (§15 R2 — REQUIRED)

The generic `ClaimNext` predicate that excludes host-bound rows exists **only in this (M25+) engine**. An
**older binary** (e.g. `53cfb98`) opening the same database does **not** know `owner_host` and will run a
host-bound row as ordinary generic work (the §15 R2 recheck observed an old binary dispatching a host-bound
`upgrade-bound` row with no schema/open/claim refusal). The reverse direction is safe (the new engine opens an
old DB, preserves generic rows, excludes newly bound rows) — but **old-database readability is not safe
mixed-version dispatch**.

**Rollout rule (consumer adoption must enforce):** stop/drain and upgrade **every** participating engine claimer
to M25+ **before** enabling any `EnqueueForHost` submissions; do **not** dispatch (or roll back to) an older
binary against a database that has active host-bound work unless a demonstrated safe procedure exists. This is a
coordinated-upgrade contract, not a hostile-client security layer or a version-gating framework. It is also
recorded in the dispatch guide (`## Targeted host-local dispatch`) and the CHANGELOG **Compatibility notes**.

## 6. Explicitly NOT included (per §14.B2 non-goals)

No PostgreSQL/backend port, no arbitrary per-run workflow type names, no monetary-budget engine, no generic
finalizer/outbox, no copied `runNext`, no live external operation. The CLI/runner integration (which host runs
which run, fixtures/paths/timeout selection) is the application's; this receipt delivers the engine primitive
that makes a run host-bound and drivable only by that host.

## 7. Gate

```text
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^(TestHostLocal_|TestB2Security_)' -count=1        → ok
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^(TestHostLocal|TestB2Security|TestRunNext|TestClaimNext|TestPool|TestCancel|TestKillstorm|TestCaps|TestQueueSubWorkflow|TestParkedSubWorkflow|TestQueueChild|TestQueuedChild|TestInputAware|TestERead|TestAdversarial)' -race -count=1  → ok (no race report; core ClaimNext change regression-checked)
GOTOOLCHAIN=local go test ./pkg/workflow/ -count=1 -timeout=1800s                                  → ok (full suite)
GOTOOLCHAIN=local go vet ./pkg/workflow/                                                           → clean
golangci-lint run pkg/workflow/                                                                    → 0 issues on the new/changed files
```

The authoritative amd64 gate runs on the push; bind any CI receipt to `ef51e67`.
