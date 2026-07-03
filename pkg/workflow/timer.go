package workflow

import (
	"context"
	"errors"
	"math"
	"time"
)

// timerAction is the action of a declared TimerNode (M10 chunk 2, D36-03/D36-05).
// It is a declared suspension node: it satisfies the package-internal
// suspendableAction marker, so node.Execute honors its ErrSuspended return as an
// intentional PARK (bypassing the retry/timeout wrappers) rather than a failure.
// It carries the chunk-1 suspend seam forward, built once.
//
// # The timer is DATA, this action is the disposable "live timer"
//
// A durable timer is a persisted absolute fireAt (unix nanos, the WorkflowData
// `waits` section), NOT a live time.Timer. This action is the live timer
// re-derived on every run: it does not sleep, it does not block, it RETURNS a
// park-or-fire decision computed purely from (persisted fireAt, injected clock).
// That is what keeps timers durable with no running service and dodges the
// determinism tax — the action reads the clock to DECIDE, it never records or
// replays it.
//
// # The three-outcome decision (each run)
//
//   - Not yet armed (no persisted fireAt for this node): this is the first
//     encounter. Compute fireAt = clock.Now() + duration (an ABSOLUTE instant,
//     never a stored duration — on resume you cannot tell how much already
//     elapsed), persist it, and PARK. The process may now exit with zero compute.
//   - Armed and due (clock.Now() >= fireAt): FIRE. Clear the wait metadata and
//     return nil so node.Execute marks the node Completed; the level's checkpoint
//     then commits "fired + out of Waiting" in one atomic snapshot write.
//   - Armed but not due (clock.Now() < fireAt): RE-PARK. A spurious early wake
//     (a Tick with a stale now, clock skew) re-checks fireAt and parks again, so
//     the live mechanism can never fire before the durable fireAt.
//
// On resume the SAME logic runs: an overdue timer (fireAt <= now after a long
// downtime) satisfies the due branch and fires immediately. Firing is at-least-
// once: a crash between "fire" (returned nil, node Completed in memory) and the
// checkpoint leaves the node persisted Waiting with fireAt still armed, so resume
// re-runs the action and re-fires; a crash after the checkpoint leaves the node
// Completed (terminal), so the executor skips it and it never re-fires.
//
// # Contract: fire-at-or-after fireAt, never exactly-at
//
// A durable timer is a LOWER BOUND on a wake-up, not a real-time deadline. The
// first encounter always parks (so the fireAt is durably recorded and the process
// can exit) — even a zero or already-elapsed duration parks once, then fires on
// the next resume/Tick. Hosts must not model hard real-time SLAs on a sleep.
type timerAction struct {
	// nodeName keys this timer's fireAt in the WorkflowData `waits` section. The
	// action carries its own name because Action.Execute is not given the node
	// name; the builder/constructor wires it at definition time.
	nodeName string
	// duration is the relative sleep; fireAt = clock.Now() + duration is computed
	// ONCE, at the first (arming) encounter.
	duration time.Duration
}

// suspendable marks timerAction as a declared suspension node. The empty body is
// the whole point — the method's mere presence is the static capability gate the
// chunk-1 marker checks (and, being unexported, it can only be satisfied here in
// package workflow). (DEC-M10-mechanism.)
func (a *timerAction) suspendable() {}

// Execute is the timer's park-or-fire decision (see the type doc). All time is
// read through the injected Clock (clockFrom(ctx)); it NEVER calls time.Now()
// directly — the no-determinism-tax discipline (D36-07) the analyzer-spec and the
// determinism property enforce.
func (a *timerAction) Execute(ctx context.Context, data *WorkflowData) error {
	clock := clockFrom(ctx)

	fireAt, armed := data.GetWait(a.nodeName)
	if !armed {
		// First encounter: arm an ABSOLUTE due instant and park. (If the node was
		// reset off Waiting by the chokepoint on a cancelled/failed run, the
		// persisted fireAt survives — GetWait still returns armed — so we do NOT
		// re-arm: the original absolute deadline is durable across the stop.)
		now := clock.Now()
		fireAt = now.Add(a.duration).UnixNano()
		// Overflow guard (Review F3): time.Time.Add / UnixNano are undefined past
		// ~year 2262, so a huge-but-valid Duration (time.Duration holds ~292y)
		// wraps fireAt to a negative/garbage value that is <= now — which would
		// fire the timer IMMEDIATELY, the opposite of the request. Clamp a wrapped
		// positive-duration fireAt to MaxInt64 (the far future) so an overlong
		// sleep parks effectively forever rather than firing at once.
		if a.duration > 0 && fireAt <= now.UnixNano() {
			// Positive overflow: a huge future duration wrapped to a past instant —
			// clamp to the far future so it parks (not fires immediately).
			fireAt = math.MaxInt64
		} else if a.duration < 0 && fireAt > now.UnixNano() {
			// Negative UNDERFLOW (Review AF1): an already-elapsed (negative) duration
			// under a pre-epoch clock wrapped to a FUTURE instant — which would park
			// forever instead of firing immediately (a negative duration means "due
			// now"). Clamp to the far past so fireAt <= now and it fires at once,
			// symmetric to the positive-overflow clamp. (Practically unreachable —
			// needs a pre-~1970 clock — but keeps the overflow handling total.)
			fireAt = math.MinInt64
		}
		data.SetWait(a.nodeName, fireAt)
		return ErrSuspended
	}

	if clock.Now().UnixNano() >= fireAt {
		// Due: fire. Clear the durable wait so a Completed timer carries no stale
		// fireAt; node.Execute marks the node Completed on this nil return.
		data.ClearWait(a.nodeName)
		return nil
	}

	// Armed but not yet due — re-park (the durable fireAt is the source of truth;
	// this live re-check is what makes a spurious early wake harmless).
	return ErrSuspended
}

// NewTimerNode builds a declared TimerNode: a node that, when reached, sleeps
// until an absolute due-time (clock.Now()+d, frozen at the first encounter) that
// survives crash/suspend and re-arms from the persisted fireAt on load. It parks
// the run (Waiting) via the chunk-1 suspend seam; on resume or a host Tick where
// the due-time has passed, it fires (transitions out of Waiting) and the run
// converges. See timerAction for the full semantics. The action is set DIRECTLY
// (not via the middleware stack) so the suspension marker is visible to
// node.Execute (middleware wrapping would hide it — chunk-1 forward constraint).
func NewTimerNode(name string, d time.Duration) *Node {
	return NewNode(name, &timerAction{nodeName: name, duration: d})
}

// DueTimers returns the names of this workflow's armed timers whose persisted
// fireAt is at or before now — the timers a resume/Tick would fire. It is a
// read-only inspection of the persisted state (it loads, it does not mutate or
// re-enter), the query a host loop uses to decide whether to call Tick. A
// workflow with no persisted state (never run, or no Store) yields no due timers.
func (w *Workflow) DueTimers(now time.Time) ([]string, error) {
	if w.Store == nil {
		return nil, nil
	}
	data, err := w.Store.Load(w.WorkflowID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	nowNanos := now.UnixNano()

	// Collect the timers whose absolute fireAt has passed (under the wait lock).
	// Status is checked AFTER, outside ForEachWait, because ForEachWait holds the
	// read lock and GetNodeStatus would re-enter it (the non-reentrant-RWMutex
	// gotcha — see ForEachWait's doc).
	var candidates []string
	data.ForEachWait(func(nodeName string, fireAt int64) {
		if fireAt <= nowNanos {
			candidates = append(candidates, nodeName)
		}
	})
	if len(candidates) == 0 {
		return nil, nil
	}

	// F2 BOUNDARY (DEC-M10-P36-F2 layer 2, defense-in-depth): a terminally-failed
	// run is NOT resumable — never auto-resurrect it via the timer loop, even if a
	// stray Waiting somehow persisted. A hard (non-continue-on-error) Failed node
	// means the run failed; report no due timers so a host "tick all due" loop
	// cannot drive a failed run to completed. (Single boundary point.)
	if w.runHasHardFailure(data) {
		return nil, nil
	}

	// F2 ROOT-CAUSE (DEC-M10-P36-F2 layer 1): a timer is due only if its node is
	// actually parked — status == Waiting. A fireAt on a non-Waiting node (Pending
	// after a chokepoint reset on a cancel/fail, or already terminal) is inert by
	// construction: the suspend chokepoint guarantees the only node left Waiting
	// after a non-suspend exit is a legitimately-suspended run. This is the
	// timer-layer enforcement of WokeOnlyWhenReady (drop it and a failed run can
	// be flipped to completed through the public Tick loop — the load-bearing bite).
	var due []string
	for _, name := range candidates {
		if st, ok := data.GetNodeStatus(name); ok && st == Waiting {
			due = append(due, name)
		}
	}
	return due, nil
}

// runHasHardFailure reports whether the persisted run contains a hard
// (non-continue-on-error) Failed node — i.e. the run failed and is not a
// resumable suspend. Used by the F2 boundary in DueTimers to refuse to
// timer-wake a terminally-failed run. The continue-on-error flag lives on the
// DAG node (not the snapshot), so it is read from w.DAG; ForEachNodeStatus holds
// only the WorkflowData read lock and GetNode touches a different structure, so
// there is no lock nesting.
func (w *Workflow) runHasHardFailure(data *WorkflowData) bool {
	hard := false
	data.ForEachNodeStatus(func(name string, st NodeStatus) {
		if st == Failed {
			if node, ok := w.DAG.GetNode(name); ok && !node.ContinueOnError {
				hard = true
			}
		}
	})
	return hard
}

// Tick is the host-driven wake API (M10 chunk 2, D36-06a): the host calls it on
// its own schedule (a main loop, a cron) with the current instant, and the engine
// fires any timer that is due at now. It is the default wake mechanism — there is
// NO mandatory background goroutine; the engine never self-wakes unless told to
// ("no mandatory service" stays literal).
//
// Concurrency (M10 ph37 — the ph36 single-writer caveat is now DISCHARGED):
// Tick drives through Workflow.Execute, which holds the per-WorkflowID lease
// (Workflow.Locker, default in-process) for the whole run. So concurrent
// Tick/Execute/DeliverAndResume calls for the SAME WorkflowID are SERIALIZED by
// the engine within the process — they take turns rather than racing the
// load→run→save span (the checkpoint callback is also per-Execute ctx-scoped now,
// not shared, so there is no field to race). Different WorkflowIDs are independent.
// Cross-PROCESS serialization remains the host's responsibility until the SQLite
// store's claimed_at lease (DEC-M10(B)). (Discharges OBL-M10-P37-LEASE-F1.)
//
// Tick returns fired=true iff at least one timer was DUE at now and a resume was
// therefore re-entered — it signals "a resume ran", NOT "a run completed". The
// run's actual outcome is the returned error: nil = the run completed, ErrSuspended
// = other timers are still parked, or a real failure. (A due timer that the resume
// could not advance — e.g. blocked deps — still reports fired=true; key off err for
// the outcome.) A Tick whose now is before every fireAt is a no-op (fired=false, no
// wasted resume). When a timer is due, Tick re-enters with the clock pinned to now
// so the fire decision uses exactly the host-supplied now.
//
// Tick is idempotent under the M9 at-least-once contract: a Tick that fires a
// timer and crashes before the checkpoint leaves the timer armed, so the next
// Tick re-fires it (and downstream side effects dedupe on the (workflowID,
// nodeName) idempotency key). A spurious early Tick (now < fireAt for every
// timer) does nothing.
func (w *Workflow) Tick(ctx context.Context, now time.Time) (fired bool, err error) {
	// Acquire the per-WorkflowID lease at this public drive entry, then delegate to
	// the lease-free executeLocked (single-funnel discipline — Tick does NOT call
	// public Execute, so the non-reentrant lease is acquired exactly once even
	// though Tick drives the workflow). Held across the DueTimers read + the resume.
	release, lerr := w.locker().Acquire(ctx, w.WorkflowID)
	if lerr != nil {
		return false, lerr
	}
	defer release()

	due, err := w.DueTimers(now)
	if err != nil {
		return false, err
	}
	if len(due) == 0 {
		// Nothing is due at now — do not re-enter the resume (no work, no churn).
		return false, nil
	}
	// Pin the clock to the host-supplied now for this resume so the timer action's
	// fireAt<=now decision agrees with the DueTimers check above, then re-enter the
	// M9 resume path. A returned ErrSuspended means OTHER timers remain parked —
	// the due ones still fired, so fired=true.
	execErr := w.executeLocked(withClock(ctx, fixedClock(now)))
	return true, execErr
}

// fixedClock is a Clock pinned to a single instant — the per-invocation clock
// Tick injects so a host-supplied now drives the fire decision deterministically.
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }
