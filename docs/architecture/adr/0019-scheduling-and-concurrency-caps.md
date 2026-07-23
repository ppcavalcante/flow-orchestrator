# 0019. Scheduling + concurrency caps — in-txn re-check as the no-double-fire arbiter, COUNT-in-txn as the cap arbiter

## Status

Accepted (milestone M20 "Scheduling + Concurrency Caps", 2026-07). Records the locked design for two
opt-in additions over the M16 fencing / M17 dispatch queue: **scheduled workflows** (hand-rolled
cron / interval / one-shot, fired across N processes) and **concurrency caps** ("at most K of type X
running at once", per-type and global). Additive and opt-in — **the executor is byte-unchanged**
(`Execute` / `dag.go` / `workflow.go` untouched across all of M20); an unset cap leaves the M17 claim
path byte-behavior-unchanged; the cron parser adds **zero new dependency** and the poller adds **zero
new entry point** (an in-process goroutine, no network/IPC). The four load-bearing decisions below are
each formally proven in `specs/M20*.tla` (`run_m20_formal.sh`: 3 real invariants hold + 3 seeded
breaks falsify).

## Context

Two capabilities the platform program needs before 1.0, both of which have to hold **across
processes** on the shared SQLite store, with no server and no new attack surface:

1. **Schedules.** A workflow should run on a cron/interval/one-shot cadence. The obvious hazard is
   **double-firing**: N poller processes all see the same due slot and each enqueues a run. A fire is
   required to be **exactly-once-enqueue per due slot** (execution then inherits M17's
   at-least-once-invocation — a scheduled workflow must be idempotent, exactly like a dispatched one).
2. **Concurrency caps.** An operator should bound "K of type X running at once" (per-type) plus an
   optional global cap. The hazard is a **TOCTOU slot leak**: two claimers each read `count < cap`,
   then both claim, and the cap is exceeded. A second, subtler hazard is a **cap wedge**: M19
   sub-workflow parents park while awaiting a child, and if a parked-but-claimed parent counted
   against the cap, K parked parents each awaiting a capped child would **deadlock the cap** — nobody
   can run because everybody is waiting.

Both reduce to the same question: *what is the actual safety arbiter?* The M16 lineage offers
fencing tokens and leases, and it is tempting to reach for them. This ADR records why the arbiter is
**not** the fence in either case.

## Decision

### 1. The no-double-fire arbiter is the in-txn `next_fire_time` re-check — NOT the fence

The whole fire — re-read the schedule row, fence-claim the schedule's lease, `capAdmits`, enqueue the
run, advance `next_fire_time` (or delete a one-shot) — runs in **one `BEGIN IMMEDIATE` transaction**
(`fireDueLocked`, `schedule.go`). N racing pollers serialize on the SQLite write lock. Inside the
txn, after taking the lock, the fire **re-reads `next_fire_time` and skips if it is no longer `≤ now`**
(a sibling already fired and advanced the slot), then advances `next_fire_time` **atomically in the
same txn**. That serialized-txn + in-txn re-check + atomic advance is the safety property: exactly one
poller observes the slot as due, enqueues it, and moves the pointer past it.

The **fence is liveness, not safety**: the per-schedule lease (`schedule:<id>`, reusing the M16
primitive — `DEC-P100-FENCE-PER-SCHEDULE`, no second scheme) gives one-writer-per-schedule and
crash-reclaim (a dead poller's fire is retried within the lease TTL). But a lease **expiry is a
liveness signal, never a safety one** (the M16 discipline — expiry ≠ correctness), so it cannot be the
double-fire arbiter. This was corrected mid-build (`DEC-P100-RECHECK-IS-ARBITER`, `OBS-P100-Q1`): an
earlier framing called the fence the guard; the code and the TLA both make the re-check the guard.

Defense is three layers, each with a distinct job:

- **Re-check (safety)** — the serialized in-txn `next_fire_time > now` skip + atomic advance.
- **Fence (liveness)** — one-writer-per-schedule + crash-reclaim within the lease TTL.
- **Per-slot run-id + `ON CONFLICT` (idempotency belt)** — a fire mints a deterministic run id
  `sched:<schedule-id>:<slot>`, so a crash-retry of the *same* slot enqueues the *same* id and the
  `work_queue` `ON CONFLICT DO NOTHING` dedupes it. This belt is deliberately independent of the
  re-check: it is why the 2-process test alone does **not** isolate the arbiter (it passes even with
  the re-check removed, because the run-id dedup masks the double-fire at the queue level) — only a
  discriminator that counts `fired=true` *decisions* isolates the real safety guard.

`specs/M20Scheduling.tla` models this: the real config (atomic advance) holds `NoDoubleFire` (14
states); the seeded break (deferred, non-atomic advance) lets a second poller re-enqueue the same slot
→ `enqueued[s]=2` → falsifies.

### 2. The cap arbiter is a row-COUNT inside the claim txn — NOT a fenced counter

A cap is enforced at **claim time** as backpressure: `ClaimNext`, inside its `BEGIN IMMEDIATE` txn,
runs `COUNT(*)` over the running rows of the candidate's type (and globally) and **skips** the
candidate if admitting it would exceed the cap (`claimWouldExceedCap`, `workflow_store_sqlite_workqueue.go`).
An at-cap item is a **no-op skip, left claimable-later — never a failed workflow**. Because the COUNT
runs inside the same serialized write txn as the claim, the count-then-claim is atomic: no two
claimers can both read `count < cap` and both claim (`DEC-M20-D3` / `DEC-P98-COUNT-IN-TXN`).

We explicitly **rejected a fenced counter.** Fencing guards per-**row** state, not an **aggregate**; a
counter maintained alongside the rows desyncs on crash/reclaim and leaks slots — the
fencing-extends-to-derived-state trap. A row-COUNT is inherently crash- and reclaim-consistent (the
rows *are* the truth), so there is no counter to reconcile and no reconciler to run.

Caps are **immutable store config** (`WithCaps` at `NewSQLiteStore`, defensively copied). "Lowering a
cap" (`DEC-M20-D7` no-kill) is reopening the store/pool with a lower value on the same DB: in-flight
runs finish, and the COUNT admits nothing new until the running count drops below the new cap. No
runtime mutation, no reconciler. Both gates AND (`count(type) < cap(type)` **and** `count(*) <
Global`); an untyped run (`type=""`) is governed by the global cap only (`DEC-M20-D6`).

`specs/M20Caps.tla` models this: atomic COUNT-in-txn holds `CapNeverExceeded` (7 states); the seeded
break (decide-then-commit, non-atomic) lets three claimers decide against `count<Cap` and all commit →
`running=3 > Cap=2` → falsifies.

### 3. The cap counts RUNNING slots only — a parked sub-workflow child is cap-EXEMPT

The cap referent is `state='claimed' AND parked IS NULL` — a **running** slot. A parked M19
sub-workflow child is claimed-but-not-a-slot (`DEC-P98-PARKED-COLUMN`: a nullable `work_queue.parked`
marker, **not** a state rename, so the M18 read-model is byte-unchanged by construction). Counting
parked children would deadlock the cap (K parked parents each awaiting a capped child → nobody runs).
Making parked rows cap-exempt dissolves the wedge.

`specs/M20Parked.tla` models this: parked-exempt holds `NoWedge` (32 states, deadlock-free); the
seeded break (parked parent holds a slot) leaves the child stuck `pending` forever → `NoWedge`
violated at the wedge state.

### 4. A one-shot blocked by a cap is RETAINED (fires exactly once when the cap drains) — not dropped

When a **recurring** schedule fires at cap, it is a **missed slot**: the schedule advances past it and
nothing is enqueued (`DEC-M20-D2` — enqueuing instead would grow the backlog unbounded, the
catch-up-all footgun). Advancing-past assumes there *is* a next slot to skip to. A **one-shot has
none**: it is an explicit run-**once** instruction, and dropping it on a **transient** cap is silent
data loss.

(Note: the general missed-run behavior — for a poller that was *down* across several slots, not a
cap-block — is that missed slots **coalesce into a single fire**; `advanceSchedule` fires once and
jumps to the next future slot. `WithCatchupOnce` is a **reserved** API that is a no-op today, `==` the
default; a distinct per-missed-slot catch-up is a deferred increment. See the guide.)

So a **cap-blocked one-shot is RETAINED** (`DEC-P101-ONESHOT-AT-CAP-RETAIN`, architect-ruled,
**user-ratified at close**): `next_fire_time` is left unchanged (still `≤ now`, still due), so it
retries on the next poller tick and **fires exactly once when the cap drains**. This costs nothing
`D2` guards against — it is a single durable `schedules` row that does not advance, **not** a growing
`work_queue` backlog — and the cap is still respected (it does not fire while full). A one-shot that
*does* fire (admitted) auto-deletes on the fire-commit and releases its lease in the same txn
(`DEC-M20-D7`).

### 5. Operator visibility: the missed-fire metric

A due fire the cap blocked (`doEnqueue && !admit`, for **both** the recurring missed-slot **and** the
one-shot retain case) increments **`flow_orchestrator.dispatch.missed_fires`** (`DispatchMetrics.MissedFires()`,
exported via `WithDispatchMetrics` / the OTel bridge) — operator visibility that a schedule is starved
by a saturated cap. Opt-in and nil-safe (no metrics configured → no cost). The increment is **deferred
to after the txn commits** (uniform with `incReclaimAfterDeath`): a rolled-back fire must not
over-count the miss. Not counted on not-due / paused / lost-claim / corrupt-spec — all of those exit
before the fire point.

## Consequences

- **Additive / opt-in.** No cap configured ⇒ the M17 claim path is byte-behavior-unchanged. No poller
  started ⇒ no schedules fire. `Execute` / `dag.go` / `workflow.go` are 0-diff since M19.
- **Zero new dependency** — the 5-field cron parser (`cron.go`) is hand-rolled (ranges / steps /
  lists; no `L`/`W`/`#`), preserving the zero-attack-surface moat. It evaluates in **UTC** and handles
  DST correctly: spring-forward slots are skipped, fall-back does **not** double-fire (a
  reconstruct-canonical `Next`, bite-proven — a naive real-instant advance double-fires at the
  fall-back hour). A spec with no fire in a bounded horizon returns `ErrCronNoFire` rather than hanging.
- **Zero new entry point** — the poller is an in-process goroutine an embedder opts into; it has no
  network or IPC surface. Poller cadence is pure liveness: a missed tick delays a fire, never drops or
  doubles it.
- **Exactly-once-enqueue, at-least-once-invocation.** A scheduled run enqueues exactly once per due
  slot but, like any dispatched run, may be *invoked* more than once (crash/reclaim) — the target
  workflow must be idempotent.
- **A corrupt schedule row fails safe.** An unparseable stored spec (only reachable via direct DB
  tamper — the constructors validate) pauses the row and commits the pause, rather than spinning the
  poller forever. Operator-visible as `paused`.
- **Formal capstone.** All four decisions are machine-checked (`specs/M20Scheduling.tla` /
  `M20Caps.tla` / `M20Parked.tla`), each with a seeded discriminator that must falsify, plus a gopter
  arm over the real `SQLiteStore`.

## References

- Guide: `docs/guides/scheduling.md` (how to schedule, set caps, the RETAIN + missed-run + missed-fire
  semantics).
- API: `docs/reference/api-reference.md` — Scheduling + Concurrency Caps.
- Prior art: ADR-0015 (leases + fencing), ADR-0016 (work dispatch queue), ADR-0018 (sub-workflow
  composition — the parked children this cap exempts).
- Specs: `specs/M20Scheduling.tla`, `specs/M20Caps.tla`, `specs/M20Parked.tla`, `specs/run_m20_formal.sh`.
