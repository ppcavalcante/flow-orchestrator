# Scheduling + concurrency caps

Added in **M20**. Two **opt-in** layers on top of the multi-process SQLite store and the
[work dispatch queue](dispatch.md):

- **Schedules** — run a workflow on a cron / interval / one-shot cadence. A schedule fires a run of a
  target workflow `type` onto the dispatch `work_queue` when its due time passes. An in-process
  **poller** (an embedder-started goroutine, no server) drives it; many pollers across many processes
  fire the same schedule safely, exactly once per due slot.
- **Concurrency caps** — bound "at most K of type X running at once" (per-type) plus an optional
  global cap, enforced at claim time as backpressure across all worker processes.

Both are additive: no cap configured leaves the M17 claim path unchanged, and no poller started means
no schedules fire. See [ADR-0019](../architecture/adr/0019-scheduling-and-concurrency-caps.md).

## Prerequisites

Both features live on the **multi-process** SQLite store (they coordinate across processes through the
shared `.db` file and the M16 fencing model). `CreateSchedule`, the poller, and cross-process caps all
require an mp store — see [Multi-process safety](persistence.md#multi-process-safety-competing-consumers).

A scheduled run reaches a worker through the [dispatch queue](dispatch.md), so the workflow `type` a
schedule targets must be **registered** on the workers that should run it, exactly like any dispatched
workflow.

## Scheduling a workflow

Build a `ScheduleSpec` with one of the three constructors, register it with `CreateSchedule`, and run a
`SchedulePoller`:

```go
// A cron schedule: fire "nightly-report" runs at 02:30 UTC every day.
spec, err := workflow.NewCronSchedule("nightly", "nightly-report", "30 2 * * *", time.Now())
if err != nil { /* bad cron spec / empty id or type */ }

created, err := store.CreateSchedule(spec) // created=false ⇒ this id already existed (idempotent)

// Start a poller (one per process is typical). Run blocks until ctx is cancelled.
poller, err := workflow.NewSchedulePoller(store, "poller-A")
go poller.Run(ctx)
```

The three schedule kinds:

| Constructor | Fires |
|---|---|
| `NewCronSchedule(id, typ, spec, anchor)` | per the 5-field cron `spec`, first slot strictly after `anchor` |
| `NewIntervalSchedule(id, typ, period, anchor)` | every `period` starting at `anchor + period` (`period` > 0) |
| `NewOneshotSchedule(id, typ, fireAt)` | once at `fireAt`, then auto-deletes |

`anchor` / `fireAt` are evaluated in **UTC** — see [Cron + DST](#cron--dst) below.

### Lifecycle

| Method | Effect |
|---|---|
| `CreateSchedule(spec)` | durably register. Re-registering an existing id is a **no-op** (`created=false`) — the fire state is left byte-unchanged, never a silent `next_fire` reset that would double-fire. |
| `PauseSchedule(id)` | stop firing (idempotent; a missing id is a 0-row no-op). |
| `ResumeSchedule(id)` | fire again on the next due slot. |
| `DeleteSchedule(id)` | remove the schedule and release its lease so the id is immediately re-registerable. |

### The poller

`NewSchedulePoller(store, ownerID, opts...)` builds a poller; `Run(ctx)` scans due schedules and fires
each every `pollInterval` (default 1s; set with `WithSchedulePollInterval`) until `ctx` is cancelled.

- **`ownerID` must be distinct per process/poller** — it is the fencing owner id that arbitrates
  cross-process races.
- The poll cadence is **pure liveness**. Miss a tick and the slot fires on the next one; the stored
  due time and the fence are the safety arbiters, not the cadence. A longer interval only delays a
  fire, never drops or doubles it.
- The first scan runs immediately on `Run` (no initial delay), so an already-due schedule fires
  promptly on start.
- A per-schedule error (a transient busy DB, a lost race) is swallowed and retried next tick — the
  poller never dies on one bad schedule.

### Fire semantics — exactly-once-enqueue, at-least-once-invocation

A due slot is **enqueued exactly once**, even under N racing pollers across N processes (the fire runs
in one serialized transaction; see the ADR). But a scheduled run, like any dispatched run, may be
**invoked** more than once (worker crash → reclaim). **The target workflow must be idempotent** — the
scheduled run gets a deterministic id (`sched:<schedule-id>:<slot>`), which is your idempotency key.

### Missed runs

If the poller is down and comes back after several slots have elapsed, **all missed slots coalesce
into a single fire**: the schedule fires **once**, then advances to the next future slot. There is
deliberately no catch-up-all — replaying every missed slot would grow the backlog unbounded.

`WithCatchupOnce()` is a **reserved** API. Today it is a **no-op** — the coalesce-to-one behavior
above is the only missed-run behavior, and setting it does not change anything (a distinct
fire-once-per-missed-window catch-up is a deferred increment). Do not rely on it selecting a different
behavior yet.

## Concurrency caps

Bound how many runs of a type may be **running at once**. Set caps at store construction with
`WithCaps` (immutable thereafter):

```go
store, err := workflow.NewSQLiteStore(path, workflow.WithMultiProcess(),
    workflow.WithCaps(workflow.Caps{
        PerType: map[string]int{"report": 3, "export": 1}, // ≤3 reports, ≤1 export running at once
        Global:  10,                                        // ≤10 runs of any type running at once
    }),
)
```

- **Per-type and global gates both apply (AND).** A claim is admitted iff `count(type) < PerType[type]`
  (if that type is capped) **and** `count(*) < Global` (if a global cap is set).
- A type **absent** from `PerType` (or mapped to `≤ 0`) is **unbounded** for that type. An **untyped**
  run (`type=""`) is governed by the **global** cap only.
- The zero-value `Caps` is fully unbounded — the claim path is byte-behavior-unchanged from M17.

### How caps behave

- **A cap is backpressure, not rejection.** A worker that would exceed a cap **skips** the candidate
  and leaves it claimable-later. An at-cap item is a **no-op skip, never a failed workflow** — it
  simply waits in the queue until a running slot frees up.
- **Parked sub-workflow children are cap-exempt.** A cap counts only *running* slots. An
  [M19 sub-workflow](sub-workflows.md) parent that is parked awaiting a child does not consume a slot
  — otherwise K parked parents each awaiting a capped child would deadlock the cap.
- **Lowering a cap does not kill in-flight runs.** Caps are immutable store config; to change one,
  reopen the store/pool with a new value on the same DB file. In-flight runs finish; nothing new is
  admitted until the running count drops below the new cap.

### Schedules × caps

When a schedule fires while its target type is at cap:

- **A recurring schedule** treats it as a **missed slot**: the fire is **not** enqueued and the
  schedule advances to its next slot. Enqueuing anyway would grow the backlog unbounded.
- **A one-shot is RETAINED**, not dropped. A one-shot is a run-**once** instruction with no next slot
  to advance to — dropping it on a *transient* cap would be silent data loss. So it stays due and
  **fires exactly once when the cap drains**. This costs no backlog (it is one durable `schedules` row
  that does not advance, not a growing queue), and the cap is still respected (it does not fire while
  full).

### Observing missed fires

A due fire the cap blocked — for **both** the recurring missed-slot and the one-shot retain cases —
increments the **`flow_orchestrator.dispatch.missed_fires`** metric, so an operator can see that a
schedule is starved by a saturated cap. It is opt-in via `WithDispatchMetrics` (and the OTel bridge);
with no metrics configured there is no cost. See [Observability](observability.md).

```go
m := &workflow.DispatchMetrics{}
store, _ := workflow.NewSQLiteStore(path,
    workflow.WithMultiProcess(),
    workflow.WithCaps(workflow.Caps{PerType: map[string]int{"report": 1}}),
    workflow.WithDispatchMetrics(m),
)
// ... later, from an operator/health endpoint:
starved := m.MissedFires() // cumulative due-fires blocked by a saturated cap
```

## Cron + DST

The cron parser is a hand-rolled **5-field** subset — minute, hour, day-of-month, month, day-of-week —
supporting `*`, ranges (`1-5`), steps (`*/15`, `0-30/10`), and lists (`1,15,30`). It does **not**
support the extended `L` / `W` / `#` operators (this keeps the parser dependency-free and its surface
small). When both day-of-month and day-of-week are restricted, a slot matching **either** fires (the
standard cron OR-union).

Cron specs are evaluated in **UTC**, and the constructor anchors in UTC too — this is load-bearing for
consistency. If the first fire were computed on your local calendar but every subsequent fire on the
UTC calendar, the schedule would silently shift by your zone offset after the first fire.

DST is handled correctly:

- **Spring-forward** — a slot in the skipped hour never occurs, so it does not fire.
- **Fall-back** — a slot in the repeated hour fires **once**, not twice (a naive real-instant advance
  would double-fire at the fall-back hour; the parser reconstructs the canonical slot instead).

A cron spec with no fire within a bounded search horizon (e.g. a spec that can never match) returns
`ErrCronNoFire` rather than hanging the poller.

## Guarantees and limits

- **Exactly-once-enqueue** per due slot across N processes; **at-least-once-invocation** (make the
  workflow idempotent).
- **Zero new dependency** (the cron parser is hand-rolled) and **zero new network entry point** (the
  poller is an in-process goroutine).
- Firing an **unregistered** target type still enqueues the run — it will sit `pending` and visible in
  the queue until a worker that registered the type claims it (the stranded-row risk of a typo'd
  target type is on you to catch).
- A **corrupt** stored schedule spec (only reachable via direct DB tamper — the constructors validate)
  pauses the row rather than spinning the poller, and is operator-visible as `paused`.

See also: [Work dispatch](dispatch.md), [Sub-workflows](sub-workflows.md),
[Persistence](persistence.md), [Observability](observability.md).
