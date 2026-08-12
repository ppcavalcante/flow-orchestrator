# 10 — Scheduling and Caps (durable timers, concurrency caps)

Two governance primitives that make a durable engine safe to run unattended: durable
**schedules** and cross-process concurrency **caps**.

## Part A — durable schedule (deterministic clock)

Each scheduled job is its own **durable one-shot schedule**: a workflow that parks on a
single `AddTimer` node and fires once, at its own absolute due time. A timer is durable
**DATA** — a persisted `fireAt`, not a live `time.Timer` — so it survives a crash and fires
on resume.

A host **poller loop** drives them deterministically:

1. advance an injected **`FakeClock`** (no wall-clock sleeping),
2. ask **`DueTimers(now)`** which timers are due,
3. call **`Tick(ctx, now)`** to fire them.

Because the clock is injected, the whole "3-hour schedule" runs as an **instant, deterministic
test**. The three schedules (+1h/+2h/+3h) fire in due-time order over the tick loop, and the
`SurvivesStoreReopen` test proves the parked timers survive being armed on one store handle
and driven to completion by a **fresh** handle to the same DB file (a stand-in for a restart).

> Note: the shipped `SchedulePoller` / `NewIntervalSchedule` / `NewCronSchedule` machinery
> fires `ScheduleSpec`s onto the work queue on the wall clock; its clock seam is internal, so
> it is not deterministically testable from outside the package. This example uses the
> **durable-timer primitive** (`AddTimer` + `DueTimers`/`Tick` + `WithClock`), which is the
> public, injectable-clock surface — and the same durable-DATA mechanism a schedule fires.

## Part B — cross-process concurrency cap

Many `transcode` jobs sit on a shared queue; a fleet of **more workers than the cap** drains
them. The cap (`WithCaps`: at most `K` transcodes RUNNING at once) is enforced inside the
store's **atomic `ClaimNext`** across every worker/process — a claim that would exceed `K` is
refused until a running one finishes. Each job holds a work window open and brackets it, so
the demo sweeps live concurrency and proves the **peak never exceeds K** (and, because the
window guarantees overlap, actually **reaches K** — the assertion is not vacuous).

## How to run

```bash
GOTOOLCHAIN=local go run ./examples/10-scheduling-and-caps
GOTOOLCHAIN=local go test ./examples/10-scheduling-and-caps/ -count=1
```

## Key API calls

| Call | Role |
|---|---|
| `b.AddTimer(name, delay)` | a durable timer node (absolute `fireAt`, persisted) |
| `wf.WithClock(clk)` / `workflow.NewFakeClock(t)` / `clk.Advance(d)` | the injected, controllable clock |
| `wf.Execute(ctx)` → `ErrSuspended` | arm the timer and park the run |
| `wf.DueTimers(now)` → `[]string` | which timers are due at `now` (read-only) |
| `wf.Tick(ctx, now)` → `(fired bool, err error)` | fire due timers; `nil` err = converged |
| `workflow.WithCaps(Caps{PerType: {"transcode": K}})` | at most K of a type running at once |
| `workflow.RunNext(ctx, store, reg, ownerID)` | claim (cap-gated) + drive the next item |

## Expected output

```
durable schedule: 3 jobs fired over 8 ticks of a controllable clock (fires=3)
fire order: [job-1 job-2 job-3]

concurrency cap: 10 transcodes, 6 workers, cap K=2 → peak concurrency observed = 2
all 10 transcodes completed exactly once, peak never exceeded K=2
```
