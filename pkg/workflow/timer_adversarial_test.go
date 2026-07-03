package workflow

// M10 chunk-2 durable-timer ADVERSARIAL suite (independent of the author's tests).
//
// The author's timer_test.go / timer_determinism_test.go cover the happy spine:
// single park->wake->converge, overdue-on-resume, durable-across-reload, not-due,
// int64 fidelity, the F2 failed/cancelled guards, the overflow clamp, and the
// determinism property. This file attacks the gaps those structurally miss:
//   - multi-timer fan-out (partial-due in a level; timers across levels)
//   - the coe-Failed-sibling boundary (runHasHardFailure must NOT over-reject)
//   - a completed run + a stale-status timer must not be resurrected
//   - clock adversarial (backward skew; the exact fireAt / fireAt-1ns boundary)
//   - true-crash (NOT a checkpoint-flush-error) at-least-once timer RE-FIRE + dedup
//   - zero / negative duration totality
//   - corrupt/forged `waits` rejection + a fireAt round-trip fuzz
//   - determinism over a multi-timer + crash scenario
//   - concurrent DIFFERENT-WorkflowID Tick safety (the documented-safe case)
//
// Oracle for each test is stated inline: a property/invariant, a metamorphic
// relation, or the minimum bar (no panic, no stuck, no corruption).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// crash-injection helper: a Checkpointer store whose SaveCheckpoint fails on a
// chosen call number, so we can model a TRUE process crash (the crashed attempt's
// in-memory data is discarded, NOT saved — unlike a Workflow.Execute flush-error
// which commits the failed state). Used by driving dag.Execute directly.
// ---------------------------------------------------------------------------

type flakyCheckpointStore struct {
	*InMemoryStore
	mu     sync.Mutex
	calls  int
	failAt int // fail the Nth SaveCheckpoint (1-based); 0 = never fail
}

func newFlakyStore(failAt int) *flakyCheckpointStore {
	return &flakyCheckpointStore{InMemoryStore: NewInMemoryStore(), failAt: failAt}
}

func (s *flakyCheckpointStore) SaveCheckpoint(d *WorkflowData) error {
	s.mu.Lock()
	s.calls++
	n, fail := s.calls, s.failAt
	s.mu.Unlock()
	if fail != 0 && n == fail {
		return errors.New("simulated crash during checkpoint flush")
	}
	return s.InMemoryStore.SaveCheckpoint(d)
}

// ===========================================================================
// #1  MULTI-TIMER FAN-OUT
// ===========================================================================

// TestAdv_MultiTimer_PartialDueInOneLevel: two parallel timers in the SAME level
// with DIFFERENT fireAt. A Tick at a `now` that is past timer A's fireAt but before
// timer B's must fire ONLY A and leave B parked; a later Tick past B fires B and the
// run converges. DueTimers must report exactly the due subset at each instant.
//
// Oracle (invariant): a timer fires iff Waiting && fireAt<=now — per-timer, never
// "all or nothing" across a level. Gap vs author tests: they only ever have one
// timer per level.
func TestAdv_MultiTimer_PartialDueInOneLevel(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-multi-partial"
	clk := NewFakeClock(epoch)
	counter := newExecCounter()

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("a", 1*time.Hour))
		mustAddNode(t, d, NewTimerNode("b", 3*time.Hour))
		mustAddNode(t, d, timerDownstream("afterA", counter))
		mustAddNode(t, d, timerDownstream("afterB", counter))
		mustAddDep(t, d, "a", "afterA")
		mustAddDep(t, d, "b", "afterB")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	// Both arm+park on first reach.
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "a", Waiting)
	assertStatus(t, p, "b", Waiting)

	// At epoch+1h: only A is due.
	due, err := build().DueTimers(epoch.Add(time.Hour))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a"}, due, "only timer A is due at +1h")

	// This Tick FIRES A but leaves B parked -> the run is still suspended, so Tick
	// reports fired=true AND err=ErrSuspended ("a resume ran; others remain parked").
	fired, err := build().Tick(context.Background(), epoch.Add(time.Hour))
	require.ErrorIs(t, err, ErrSuspended, "A fired but B is still parked -> the run stays suspended")
	assert.True(t, fired)
	p, _ = store.Load(id)              //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "a", Completed) // A fired
	assertStatus(t, p, "b", Waiting)   // B stayed parked
	// Model A (whole-run suspend at the barrier): even though A fired, level 1 is NOT
	// reached while sibling B is parked in level 0, so A's downstream stays Pending.
	assertStatus(t, p, "afterA", Pending)
	assertStatus(t, p, "afterB", Pending)
	_, bArmed := p.GetWait("b")
	assert.True(t, bArmed, "B keeps its durable fireAt while A fires")
	assert.Equal(t, 0, counter.get("afterA"), "downstream does not advance while a sibling timer is parked (Model A)")
	assert.Equal(t, 0, counter.get("afterB"))

	// At epoch+3h: B is now due; level 0 is fully done -> level 1 runs, run converges.
	fired, err = build().Tick(context.Background(), epoch.Add(3*time.Hour))
	require.NoError(t, err, "the run converges once B fires")
	assert.True(t, fired)
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "b", Completed)
	assertStatus(t, final, "afterA", Completed)
	assertStatus(t, final, "afterB", Completed)
	assert.Equal(t, 1, counter.get("afterA"), "afterA runs exactly once, after the whole level unparks")
	assert.Equal(t, 1, counter.get("afterB"))
}

// TestAdv_MultiTimer_AcrossLevels: a timer whose downstream is ANOTHER timer
// (timer1 -> timer2). The second timer must arm only when REACHED (after timer1
// fires), with fireAt relative to that instant — not pre-armed at run start.
// Successive Ticks advancing the clock converge the chain.
//
// Oracle (metamorphic): timer2.fireAt == (instant timer1 fired) + timer2.duration,
// i.e. the arm is reach-relative. Gap: author tests never chain timers.
func TestAdv_MultiTimer_AcrossLevels(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-multi-levels"
	clk := NewFakeClock(epoch)

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("t1", time.Hour))
		mustAddNode(t, d, NewTimerNode("t2", time.Hour))
		mustAddDep(t, d, "t1", "t2")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	// Run start: t1 arms, t2 never reached.
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "t1", Waiting)
	assertStatus(t, p, "t2", Pending)
	_, t2Armed := p.GetWait("t2")
	require.False(t, t2Armed, "t2 must NOT be armed before it is reached")

	// Tick at t1's fireAt (epoch+1h): t1 fires, t2 is now reached and arms at THIS
	// instant -> fireAt = (epoch+1h)+1h = epoch+2h. The run re-suspends on t2.
	fireT1 := epoch.Add(time.Hour)
	require.ErrorIs(t, mustWorkflowTickSuspends(t, build(), fireT1), ErrSuspended)
	p, _ = store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "t1", Completed)
	assertStatus(t, p, "t2", Waiting)
	t2FireAt, armed := p.GetWait("t2")
	require.True(t, armed)
	assert.Equal(t, fireT1.Add(time.Hour).UnixNano(), t2FireAt,
		"t2 arms reach-relative: fireAt = (t1-fire instant)+duration")

	// A Tick at epoch+90m is past t1's old fireAt but BEFORE t2's new fireAt(epoch+2h):
	// nothing due, no churn.
	fired, err := build().Tick(context.Background(), epoch.Add(90*time.Minute))
	require.NoError(t, err)
	assert.False(t, fired, "t2 is not yet due at epoch+90m")

	// Tick past t2: converge.
	fired, err = build().Tick(context.Background(), epoch.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, fired)
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "t2", Completed)
}

// mustWorkflowTickSuspends drives a Tick and returns the underlying Execute error
// (a Tick that fires a timer but leaves OTHERS parked returns ErrSuspended).
func mustWorkflowTickSuspends(t *testing.T, w *Workflow, now time.Time) error {
	t.Helper()
	fired, err := w.Tick(context.Background(), now)
	require.True(t, fired, "expected the Tick to fire at least one timer")
	return err
}

// TestAdv_MultiTimer_AllDueSingleTick: three timers parked in ONE level, all due
// at a single Tick -> all fire in that one resume, run converges, each downstream
// runs exactly once.
//
// Oracle: a single resume fires every due timer in a level (no leftover parked).
func TestAdv_MultiTimer_AllDueSingleTick(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-multi-alldue"
	clk := NewFakeClock(epoch)
	counter := newExecCounter()

	build := func() *Workflow {
		d := NewDAG(id)
		for i, dur := range []time.Duration{time.Hour, 2 * time.Hour, 30 * time.Minute} {
			name := fmt.Sprintf("t%d", i)
			mustAddNode(t, d, NewTimerNode(name, dur))
			mustAddNode(t, d, timerDownstream("after"+name, counter))
			mustAddDep(t, d, name, "after"+name)
		}
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)

	// One Tick well past every fireAt: all three fire, run converges.
	fired, err := build().Tick(context.Background(), epoch.Add(10*time.Hour))
	require.NoError(t, err, "all due timers fire in a single resume -> converge")
	assert.True(t, fired)
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	for i := 0; i < 3; i++ {
		assertStatus(t, final, fmt.Sprintf("t%d", i), Completed)
		assert.Equal(t, 1, counter.get(fmt.Sprintf("aftert%d", i)))
	}
	due, _ := build().DueTimers(epoch.Add(10 * time.Hour)) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assert.Empty(t, due, "no timers remain due after convergence")
}

// ===========================================================================
// #5  F2 RE-ENTRY BOUNDARY — the highest-value targets
// ===========================================================================

// TestAdv_CoeFailedSiblingDoesNotBlockTimer is the load-bearing boundary the F2
// guard MUST NOT over-reject (CONTEXT #5). A continue-on-error node that FAILS in
// the same level as a legitimately-parked timer is NOT a hard failure, so the run
// is a real suspend and a host Tick MUST still fire the timer and converge.
//
// Oracle (invariant): runHasHardFailure treats a coe-Failed node as NOT hard, so
// DueTimers reports the parked timer due and Tick fires it. If runHasHardFailure
// over-rejected (any Failed => hard), the timer would be STUCK forever — a real
// liveness bug. No existing test exercises a coe-Failed + Waiting combination.
func TestAdv_CoeFailedSiblingDoesNotBlockTimer(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-coe-timer"
	counter := newExecCounter()

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour)) // parks (Waiting+fireAt)
		// A continue-on-error node that fails: tolerated, marked Failed, NOT hard.
		coe := NewNode("flaky", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			return errors.New("coe boom")
		}))
		coe.ContinueOnError = true
		mustAddNode(t, d, coe)
		mustAddNode(t, d, timerDownstream("after", counter))
		mustAddDep(t, d, "sleep", "after")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
	}

	// Level 0 runs the timer (parks) + the coe node (fails, tolerated). The run is a
	// genuine suspend: coe failures are not returned, the park drains to the barrier.
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended,
		"a coe-failure must NOT turn the parked run into a failure")
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "sleep", Waiting)
	assertStatus(t, p, "flaky", Failed)

	// THE BOUNDARY: a coe-Failed run is NOT terminally failed -> the timer is due.
	later := epoch.Add(2 * time.Hour)
	due, err := build().DueTimers(later)
	require.NoError(t, err)
	assert.Equal(t, []string{"sleep"}, due,
		"the parked timer is due despite a coe-Failed sibling (runHasHardFailure must not over-reject)")

	fired, err := build().Tick(context.Background(), later)
	require.NoError(t, err, "the timer fires and the run converges past the tolerated coe failure")
	assert.True(t, fired)
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
	assertStatus(t, final, "flaky", Failed) // the coe failure stays observable
	assert.Equal(t, 1, counter.get("after"))
}

// TestAdv_CompletedRunNotResurrected: a fully-converged run (timer already fired,
// fireAt cleared). DueTimers must report nothing and Tick must be a no-op — a host
// tick loop cannot re-enter / re-run a completed run.
//
// Oracle: a Completed timer carries no fireAt and is not Waiting -> never due.
func TestAdv_CompletedRunNotResurrected(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-completed"
	counter := newExecCounter()

	build := func(now time.Time) *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		mustAddNode(t, d, timerDownstream("after", counter))
		mustAddDep(t, d, "sleep", "after")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(now)}
	}

	require.ErrorIs(t, build(epoch).Execute(context.Background()), ErrSuspended)
	require.NoError(t, build(epoch.Add(2*time.Hour)).Execute(context.Background()), "fires + converges")
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	require.Equal(t, 1, counter.get("after"))

	// Now hammer it with ticks far in the future: nothing must re-run.
	for _, when := range []time.Duration{2 * time.Hour, 100 * time.Hour, 1e6 * time.Hour} {
		due, err := build(epoch.Add(when)).DueTimers(epoch.Add(when))
		require.NoError(t, err)
		assert.Empty(t, due, "a converged run reports no due timers")
		fired, err := build(epoch.Add(when)).Tick(context.Background(), epoch.Add(when))
		require.NoError(t, err)
		assert.False(t, fired, "Tick on a converged run is a no-op")
	}
	assert.Equal(t, 1, counter.get("after"), "downstream must not re-run on a converged run")
}

// TestAdv_SkippedTimerNotDue: a TimerNode downstream of a hard-failing node is
// Skipped (never reached, never armed). DueTimers must report nothing.
//
// Oracle: a Skipped timer has no fireAt and is not Waiting -> never due (and the
// hard failure also trips the boundary layer).
func TestAdv_SkippedTimerNotDue(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-skipped-timer"
	d := NewDAG(id)
	mustAddNode(t, d, NewNode("boom", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("hard boom")
	})))
	mustAddNode(t, d, NewTimerNode("sleep", time.Hour)) // depends on boom -> Skipped
	mustAddDep(t, d, "boom", "sleep")
	w := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}

	var execErr *ExecutionError
	require.True(t, errors.As(w.Execute(context.Background()), &execErr))
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "sleep", Skipped)
	_, armed := p.GetWait("sleep")
	require.False(t, armed, "a Skipped timer never armed")

	due, err := w.DueTimers(epoch.Add(2 * time.Hour))
	require.NoError(t, err)
	assert.Empty(t, due, "a Skipped (never-armed) timer is never due")
}

// ===========================================================================
// #3  CLOCK ADVERSARIAL
// ===========================================================================

// TestAdv_ClockBackwardNeverFiresEarly: NTP skew. The clock jumps BACKWARD between
// arm and the wake check. A not-yet-due re-check must keep the timer parked; firing
// only ever happens at fireAt<=now, so a backward jump can only DELAY a fire.
//
// Oracle (safety): no early fire under any backward clock movement.
func TestAdv_ClockBackwardNeverFiresEarly(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-clock-back"
	clk := NewFakeClock(epoch)
	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended) // fireAt=epoch+1h

	// Clock goes backward to epoch-2h: a resume must re-park (still < fireAt).
	clk.Set(epoch.Add(-2 * time.Hour))
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended,
		"a backward clock must never fire the timer early")
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p, "sleep", Waiting)

	// DueTimers with a now in the past reports nothing.
	due, err := build().DueTimers(epoch.Add(-2 * time.Hour))
	require.NoError(t, err)
	assert.Empty(t, due)

	// A Tick with a past now is a no-op.
	fired, err := build().Tick(context.Background(), epoch.Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, fired, "a backward-now Tick fires nothing")

	// Recovery: clock forward past fireAt -> fires.
	clk.Set(epoch.Add(time.Hour))
	require.NoError(t, build().Execute(context.Background()))
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
}

// TestAdv_FireAtBoundaryExact pins the fire predicate at the exact instant: at
// now==fireAt the timer fires (>=); at now==fireAt-1ns it parks. The single-ns
// boundary the author tests skip (they use whole-hour offsets).
//
// Oracle (boundary value analysis): fire iff now >= fireAt, to the nanosecond.
func TestAdv_FireAtBoundaryExact(t *testing.T) {
	const id = "adv-boundary"
	dur := time.Hour
	fireAt := epoch.Add(dur).UnixNano()

	// now == fireAt-1ns -> parks.
	{
		store := NewInMemoryStore()
		build := func(now time.Time) *Workflow {
			d := NewDAG(id)
			mustAddNode(t, d, NewTimerNode("sleep", dur))
			return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(now)}
		}
		require.ErrorIs(t, build(epoch).Execute(context.Background()), ErrSuspended)
		require.ErrorIs(t, build(time.Unix(0, fireAt-1)).Execute(context.Background()), ErrSuspended,
			"fireAt-1ns must NOT fire")
		p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
		assertStatus(t, p, "sleep", Waiting)
	}

	// now == fireAt exactly -> fires.
	{
		store := NewInMemoryStore()
		build := func(now time.Time) *Workflow {
			d := NewDAG(id)
			mustAddNode(t, d, NewTimerNode("sleep", dur))
			return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(now)}
		}
		require.ErrorIs(t, build(epoch).Execute(context.Background()), ErrSuspended)
		require.NoError(t, build(time.Unix(0, fireAt)).Execute(context.Background()),
			"fireAt exactly must fire (>=)")
		p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
		assertStatus(t, p, "sleep", Completed)
	}
}

// ===========================================================================
// #2  TRUE-CRASH × TIMER — at-least-once RE-FIRE + idempotent convergence
// ===========================================================================

// TestAdv_TrueCrashBeforeFirePersisted_RefiresAndConverges models a genuine process
// death (NOT a Workflow.Execute flush-error, which would commit the fired state):
// the timer fires in memory, the crash discards that in-memory data before the fire
// is persisted, so the on-disk state is still Waiting+fireAt -> resume RE-FIRES.
//
// Oracle (at-least-once + idempotent): the run converges to the SAME final as a
// clean run; the downstream side effect lands exactly once (the crash hit before
// the downstream level, so the re-fire does not double it).
func TestAdv_TrueCrashBeforeFirePersisted_RefiresAndConverges(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-truecrash"
	counter := newExecCounter()

	buildDAG := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		mustAddNode(t, d, timerDownstream("after", counter))
		mustAddDep(t, d, "sleep", "after")
		return d
	}

	// Run 1: arm + park (persists sleep=Waiting + fireAt).
	w1 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)

	// Run 2 (the "tick that crashes"): drive dag.Execute DIRECTLY with a pinned clock
	// and a checkpoint that dies on its FIRST call — i.e. right after the timer fires
	// (ClearWait+Completed in memory) but before that is persisted. We DISCARD the
	// in-memory data (true crash), so the store still holds Waiting+fireAt.
	fireTime := epoch.Add(2 * time.Hour)
	crashDAG := buildDAG()
	crashCP := func(_ *WorkflowData) error {
		return errors.New("crash right after the timer fired, before persist")
	}
	data2, err := store.Load(id)
	require.NoError(t, err)
	crashErr := crashDAG.Execute(withCheckpoint(withClock(context.Background(), fixedClock(fireTime)), crashCP), data2)
	require.Error(t, crashErr, "the crash surfaces as an error")
	require.NotErrorIs(t, crashErr, ErrSuspended)
	// IMPORTANT: data2 is NOT saved — modelling process death.

	// The persisted state is untouched: still Waiting + armed.
	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, persisted, "sleep", Waiting)
	_, armed := persisted.GetWait("sleep")
	require.True(t, armed, "fireAt survives the crash (the fire was never persisted)")
	assert.Equal(t, 0, counter.get("after"), "the downstream never ran in the crashed attempt")

	// Run 3: clean resume -> the timer RE-FIRES (at-least-once), downstream runs once,
	// run converges to the clean-run final.
	w3 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(fireTime)}
	require.NoError(t, w3.Execute(context.Background()), "resume re-fires the timer and converges")
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
	_, stillArmed := final.GetWait("sleep")
	assert.False(t, stillArmed, "the re-fire clears the durable wait")
	assert.Equal(t, 1, counter.get("after"), "downstream side effect lands exactly once despite the re-fire")
}

// TestAdv_TrueCrashAfterFireBeforeDownstreamPersist: the timer fired AND was
// persisted (level-0 barrier), the downstream ran, then the crash hit before the
// downstream's level was persisted. Resume must NOT re-fire the timer (Completed,
// terminal) and the downstream re-runs (the documented at-least-once on the
// in-flight level) — the run still converges, no double-completion / corruption.
//
// Oracle: a crash-AFTER (timer persisted Completed) never re-fires the timer.
func TestAdv_TrueCrashAfterFireBeforeDownstreamPersist(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-truecrash-after"
	timerRuns := newExecCounter()
	downRuns := newExecCounter()

	buildDAG := func() *DAG {
		d := NewDAG(id)
		// A timer whose action also counts how many times it runs the FIRE branch.
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		mustAddNode(t, d, NewNode("after", ActionFunc(func(_ context.Context, data *WorkflowData) error {
			downRuns.inc("after")
			data.SetOutput("after", "ran")
			return nil
		})))
		mustAddDep(t, d, "sleep", "after")
		_ = timerRuns
		return d
	}

	// Run 1: arm + park.
	w1 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)

	// Run 2: crash at the SECOND checkpoint call. Call #1 = level-0 barrier (timer
	// fired -> persisted Completed). Call #2 = level-1 barrier (after the downstream
	// ran) -> dies. Persisted = timer Completed, downstream not yet persisted.
	fireTime := epoch.Add(2 * time.Hour)
	crashDAG := buildDAG()
	calls := 0
	crashCP := func(d *WorkflowData) error {
		calls++
		if calls >= 2 {
			return errors.New("crash after downstream ran, before persist")
		}
		return store.SaveCheckpoint(d)
	}
	data2, err := store.Load(id)
	require.NoError(t, err)
	require.Error(t, crashDAG.Execute(withCheckpoint(withClock(context.Background(), fixedClock(fireTime)), crashCP), data2))

	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, persisted, "sleep", Completed)
	_, armed := persisted.GetWait("sleep")
	require.False(t, armed, "the fired timer cleared its wait and was persisted Completed")
	require.Equal(t, 1, downRuns.get("after"), "downstream ran once in the crashed attempt")

	// Run 3: clean resume. The timer is Completed -> NOT re-fired; downstream re-runs
	// (at-least-once on the in-flight level) and the run converges.
	w3 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(fireTime)}
	require.NoError(t, w3.Execute(context.Background()))
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
	assert.Equal(t, 2, downRuns.get("after"), "downstream re-ran once on resume (at-least-once); timer never re-fired")
}

// TestAdv_MultiTimer_TrueCrashRefiresBoth: two timers fire together in level 0, the
// crash discards them before persist, resume re-fires BOTH and converges.
//
// Oracle: at-least-once re-fire holds for a fan-out of timers, not just one.
func TestAdv_MultiTimer_TrueCrashRefiresBoth(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-multi-truecrash"
	counter := newExecCounter()

	buildDAG := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("a", time.Hour))
		mustAddNode(t, d, NewTimerNode("b", 2*time.Hour))
		mustAddNode(t, d, timerDownstream("afterA", counter))
		mustAddNode(t, d, timerDownstream("afterB", counter))
		mustAddDep(t, d, "a", "afterA")
		mustAddDep(t, d, "b", "afterB")
		return d
	}
	require.ErrorIs(t,
		(&Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}).Execute(context.Background()),
		ErrSuspended)

	fireTime := epoch.Add(3 * time.Hour) // past both fireAts
	crashDAG := buildDAG()
	crashCP := func(_ *WorkflowData) error { return errors.New("crash before persist") }
	data2, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	require.Error(t, crashDAG.Execute(withCheckpoint(withClock(context.Background(), fixedClock(fireTime)), crashCP), data2))

	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, persisted, "a", Waiting)
	assertStatus(t, persisted, "b", Waiting)

	w3 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store, Clock: NewFakeClock(fireTime)}
	require.NoError(t, w3.Execute(context.Background()))
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "a", Completed)
	assertStatus(t, final, "b", Completed)
	assert.Equal(t, 1, counter.get("afterA"))
	assert.Equal(t, 1, counter.get("afterB"))
}

// ===========================================================================
// #6  DURATION TOTALITY — zero / negative / underflow
// ===========================================================================

// TestAdv_ZeroDuration_ParksOnceThenFires: a zero-duration timer must PARK on first
// encounter (fireAt = arm-now), then fire on the very next resume/Tick — even when
// the clock has NOT advanced (now == fireAt -> fires via >=).
//
// Oracle (contract): "even a zero or already-elapsed duration parks once, then
// fires." First encounter always parks; the second (same instant) fires.
func TestAdv_ZeroDuration_ParksOnceThenFires(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-zero"
	clk := NewFakeClock(epoch) // clock never advances
	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", 0))
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended, "zero-duration parks once")
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	fireAt, armed := p.GetWait("sleep")
	require.True(t, armed)
	assert.Equal(t, epoch.UnixNano(), fireAt, "fireAt = arm-now for a zero duration")

	// Resume at the SAME instant -> fires (now >= fireAt).
	require.NoError(t, build().Execute(context.Background()), "zero-duration fires on the next resume, clock unchanged")
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
}

// TestAdv_NegativeDuration_ParksThenFiresImmediately: a negative (already-elapsed)
// duration arms a fireAt in the PAST, parks once, then fires on the next resume.
//
// Oracle (totality): a negative duration is a defined input — parks once, fires
// immediately thereafter; it must never hang or fire before the park.
func TestAdv_NegativeDuration_ParksThenFiresImmediately(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-neg"
	clk := NewFakeClock(epoch)
	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", -time.Hour))
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended, "negative-duration parks once")
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	fireAt, armed := p.GetWait("sleep")
	require.True(t, armed)
	assert.Equal(t, epoch.Add(-time.Hour).UnixNano(), fireAt, "fireAt is in the past for a negative duration")

	require.NoError(t, build().Execute(context.Background()), "an already-elapsed timer fires on the next resume")
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
}

// TestAdv_NegativeDuration_UnderflowClampFiresImmediately is the AF1 guard (FIXED).
// The overflow clamp in timer.go is now SYMMETRIC. With a clock before ~year 1970
// and a near-min negative duration, now.Add(d) lands before the int64 UnixNano floor
// (~year 1678) and UnixNano would WRAP to a positive value in the FUTURE relative to
// `now` — which (pre-fix) parked the already-elapsed timer FOREVER, the opposite of
// its "parks once then fires immediately" contract. The AF1 fix clamps a negative
// duration whose fireAt wraps > now to MinInt64 (far past), the mirror of the F3
// positive-overflow clamp, so fireAt <= now and the timer fires immediately.
//
// Reachable only with a pre-~1970 clock (no real workflow uses one), so this is a
// totality completeness guard rather than a live-defect fix. Bite: drop the
// `a.duration < 0` underflow clamp in timer.go -> the wrap returns and the timer
// parks forever -> this fails.
func TestAdv_NegativeDuration_UnderflowClampFiresImmediately(t *testing.T) {
	store := NewInMemoryStore()
	const id = "adv-neg-underflow"
	early := time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC)
	hugeNeg := -290 * 365 * 24 * time.Hour // ~-290y: 1700-290 underflows the UnixNano floor
	clk := NewFakeClock(early)
	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", hugeNeg))
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}
	// First encounter parks once (records the clamped fireAt).
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)
	p, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	fireAt, armed := p.GetWait("sleep")
	require.True(t, armed)

	// FIXED: the underflow wrap is clamped to the far past, so fireAt <= now — the
	// already-elapsed timer can fire (not a future wrap that parks forever).
	assert.Equal(t, int64(math.MinInt64), fireAt,
		"AF1 fix: a negative-duration underflow clamps fireAt to MinInt64 (mirror of the positive MaxInt64 clamp)")
	require.LessOrEqual(t, fireAt, early.UnixNano(), "clamped fireAt is at/before now")

	// Re-entry fires immediately (not stuck-parked).
	require.NoError(t, build().Execute(context.Background()),
		"the clamped already-elapsed timer fires on the next resume, not parks forever")
}

// ===========================================================================
// #4  PERSISTENCE / FIDELITY — corrupt `waits` rejection + fuzz
// ===========================================================================

// TestAdv_JSON_CorruptWaitsRejectedOrIgnored exercises the JSON `waits` load path
// with adversarial payloads: a forged non-numeric fireAt and an out-of-int64
// magnitude must be rejected as ErrCorruptData (never silently coerced through a
// lossy float64); a valid fireAt loads; a wrong-shaped section and a pre-M10
// snapshot (no waits) load cleanly with no timers.
//
// Oracle: totality of the loader — every malformed input yields a clean typed error
// or a well-defined empty result, never a panic or a corrupted int64.
func TestAdv_JSON_CorruptWaitsRejectedOrIgnored(t *testing.T) {
	corrupt := []struct {
		name string
		json string
	}{
		{"non-numeric fireAt", `{"id":"x","waits":{"t":"not-a-number"}}`},
		{"float fireAt", `{"id":"x","waits":{"t":1.5}}`},
		{"scientific fireAt", `{"id":"x","waits":{"t":1.5e3}}`},
		{"overflow fireAt", `{"id":"x","waits":{"t":99999999999999999999999}}`},
		{"bool fireAt", `{"id":"x","waits":{"t":true}}`},
		{"null fireAt", `{"id":"x","waits":{"t":null}}`},
	}
	for _, c := range corrupt {
		t.Run("reject/"+c.name, func(t *testing.T) {
			d := NewWorkflowData("x")
			err := d.LoadSnapshot([]byte(c.json))
			require.Error(t, err, "a forged fireAt must be rejected, not silently coerced")
			assert.ErrorIs(t, err, ErrCorruptData, "rejection must be the typed ErrCorruptData")
		})
	}

	// Valid fireAt (including the int64 extremes) loads exactly.
	for _, v := range []int64{0, 1234567890, math.MaxInt64, math.MinInt64} {
		t.Run(fmt.Sprintf("valid/%d", v), func(t *testing.T) {
			d := NewWorkflowData("x")
			js := fmt.Sprintf(`{"id":"x","waits":{"t":%d}}`, v)
			require.NoError(t, d.LoadSnapshot([]byte(js)))
			got, armed := d.GetWait("t")
			require.True(t, armed)
			assert.Equal(t, v, got)
		})
	}

	// Pre-M10 snapshot (no waits section) loads cleanly with zero timers.
	t.Run("pre-m10/no-waits", func(t *testing.T) {
		d := NewWorkflowData("x")
		require.NoError(t, d.LoadSnapshot([]byte(`{"id":"x","nodeStatus":{"n":"completed"}}`)))
		_, armed := d.GetWait("anything")
		assert.False(t, armed)
	})

	// Wrong-shaped waits (array instead of map): defined behavior — the section is
	// ignored (consistent with data/nodeStatus/outputs shape-tolerance), no panic,
	// no error, no timers. Documents the loader's actual contract.
	t.Run("wrong-shape/array", func(t *testing.T) {
		d := NewWorkflowData("x")
		err := d.LoadSnapshot([]byte(`{"id":"x","waits":[1,2,3]}`))
		require.NoError(t, err)
		_, armed := d.GetWait("0")
		assert.False(t, armed)
	})

	// Truncated JSON -> a decode error (totality: an error, never a panic).
	t.Run("truncated", func(t *testing.T) {
		d := NewWorkflowData("x")
		assert.Error(t, d.LoadSnapshot([]byte(`{"id":"x","waits":{"t":12`)))
	})
}

// TestAdv_StoreReload_StatusVsFireAtIndependence: a node's fireAt is independent of
// its status. A node persisted with a fireAt but a NON-Waiting status (e.g. Pending
// after a chokepoint reset on a cancelled run) must round-trip BOTH facts through
// all three stores intact — the F2 root-cause filter depends on reading them
// independently.
//
// Oracle (round-trip): (status, fireAt) survives Save/Load unchanged and uncoupled.
func TestAdv_StoreReload_StatusVsFireAtIndependence(t *testing.T) {
	stores := map[string]func(t *testing.T) WorkflowStore{
		"FlatBuffers": func(t *testing.T) WorkflowStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
		"JSONFile": func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
		"InMemory": func(t *testing.T) WorkflowStore { return NewInMemoryStore() },
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			store := mk(t)
			const id = "indep"
			d := NewWorkflowData(id)
			// fireAt armed but status Pending (the "preserved-fireAt on a non-Waiting
			// node" case the F2 root-cause filter must distinguish).
			d.SetNodeStatus("t", Pending)
			d.SetWait("t", epoch.Add(time.Hour).UnixNano())
			require.NoError(t, store.Save(d))

			got, err := store.Load(id)
			require.NoError(t, err)
			st, ok := got.GetNodeStatus("t")
			require.True(t, ok)
			assert.Equal(t, Pending, st, "status round-trips independently of fireAt")
			fa, armed := got.GetWait("t")
			require.True(t, armed, "fireAt round-trips even on a non-Waiting node")
			assert.Equal(t, epoch.Add(time.Hour).UnixNano(), fa)
		})
	}
}

// FuzzTimer_FireAtRoundTrip drives arbitrary fireAt int64 magnitudes through every
// store's Save/Load. The minimum bar: no panic, and a parked timer's fireAt comes
// back at full int64 magnitude (never float64-corrupted). Seeded from the killers.
func FuzzTimer_FireAtRoundTrip(f *testing.F) {
	for _, k := range fidelityKillers {
		f.Add(k)
	}
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(-1))
	f.Fuzz(func(t *testing.T, fireAt int64) {
		mkStores := []struct {
			name string
			mk   func() (WorkflowStore, error)
		}{
			{"InMemory", func() (WorkflowStore, error) { return NewInMemoryStore(), nil }},
			{"FlatBuffers", func() (WorkflowStore, error) { return NewFlatBuffersStore(t.TempDir()) }},
			{"JSONFile", func() (WorkflowStore, error) { return NewJSONFileStore(t.TempDir()) }},
		}
		for _, s := range mkStores {
			store, err := s.mk()
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			const id = "fuzz-fireat"
			d := NewWorkflowData(id)
			d.SetWait("sleep", fireAt)
			d.SetNodeStatus("sleep", Waiting)
			if err := store.Save(d); err != nil {
				t.Fatalf("%s Save: %v", s.name, err)
			}
			got, err := store.Load(id)
			if err != nil {
				t.Fatalf("%s Load: %v", s.name, err)
			}
			gotFireAt, armed := got.GetWait("sleep")
			if !armed {
				t.Fatalf("%s: fireAt %d lost across round-trip", s.name, fireAt)
			}
			if gotFireAt != fireAt {
				t.Fatalf("%s: fireAt %d corrupted to %d", s.name, fireAt, gotFireAt)
			}
		}
	})
}

// ===========================================================================
// #7  DETERMINISM over a MULTI-TIMER scenario
// ===========================================================================

// TestAdv_Determinism_MultiTimerByteIdentical: two independent runs of a 3-timer,
// 2-level scenario under identical FakeClock schedules produce byte-identical
// converged snapshots — the no-determinism-tax invariant extended past the author's
// single-timer determinism property.
//
// Oracle (metamorphic): identical injected clock schedule => identical persisted
// bytes, regardless of timer count or levels.
func TestAdv_Determinism_MultiTimerByteIdentical(t *testing.T) {
	runOnce := func() []byte {
		store := NewInMemoryStore()
		const id = "adv-det-multi"
		build := func(now time.Time) *Workflow {
			d := NewDAG(id)
			mustAddNode(t, d, NewTimerNode("a", time.Hour))
			mustAddNode(t, d, NewTimerNode("b", 2*time.Hour))
			mustAddNode(t, d, NewTimerNode("c", time.Hour)) // depends on a -> level 1
			mustAddNode(t, d, NewNode("sink", ActionFunc(func(_ context.Context, data *WorkflowData) error {
				data.SetOutput("sink", "done")
				return nil
			})))
			mustAddDep(t, d, "a", "c")
			mustAddDep(t, d, "b", "sink")
			mustAddDep(t, d, "c", "sink")
			return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(now)}
		}
		require.ErrorIs(t, build(epoch).Execute(context.Background()), ErrSuspended)
		// Successive ticks at fixed instants drive the chain to convergence. An
		// intermediate tick that fires some timers while others remain parked returns
		// ErrSuspended (documented); only a non-suspend, non-nil error is a failure.
		for _, when := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour} {
			_, err := build(epoch.Add(when)).Tick(context.Background(), epoch.Add(when))
			if err != nil && !errors.Is(err, ErrSuspended) {
				require.NoError(t, err)
			}
		}
		final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
		snap, err := final.Snapshot()
		require.NoError(t, err)
		return snap
	}
	assert.Equal(t, runOnce(), runOnce(),
		"identical FakeClock schedule => byte-identical converged snapshot (no determinism tax)")
}

// ===========================================================================
// #1/concurrency  DIFFERENT-ID Tick safety (documented-safe; run under -race)
// ===========================================================================

// TestAdv_ConcurrentDifferentIDs_Safe: the F1 precondition forbids concurrent
// SAME-WorkflowID Tick/Execute, but explicitly declares DIFFERENT WorkflowIDs safe
// to run in parallel. This drives many distinct-ID timer workflows' Tick
// concurrently; under -race it must show no data race and each converges correctly.
//
// Oracle (isolation): independent WorkflowIDs share no mutable state.
func TestAdv_ConcurrentDifferentIDs_Safe(t *testing.T) {
	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	finals := make([]NodeStatus, n)
	var fmu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("adv-conc-%d", i)
			store := NewInMemoryStore()
			build := func(now time.Time) *Workflow {
				d := NewDAG(id)
				mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
				mustAddNode(t, d, NewNode("after", ActionFunc(func(_ context.Context, data *WorkflowData) error {
					data.SetOutput("after", "ran")
					return nil
				})))
				mustAddDep(t, d, "sleep", "after")
				return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
			}
			if err := build(epoch).Execute(context.Background()); !errors.Is(err, ErrSuspended) {
				errs[i] = fmt.Errorf("arm: %v", err)
				return
			}
			if _, err := build(epoch.Add(2*time.Hour)).Tick(context.Background(), epoch.Add(2*time.Hour)); err != nil {
				errs[i] = fmt.Errorf("tick: %v", err)
				return
			}
			final, err := store.Load(id)
			if err != nil {
				errs[i] = err
				return
			}
			st, _ := final.GetNodeStatus("sleep")
			fmu.Lock()
			finals[i] = st
			fmu.Unlock()
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "workflow %d", i)
		assert.Equal(t, Completed, finals[i], "workflow %d converged", i)
	}
}
