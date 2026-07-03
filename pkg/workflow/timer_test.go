package workflow

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M10 chunk 2 — TimerNode + durable timers, deterministic tests.
//
// Every test here drives time through a FakeClock (D36-07): no real sleeping, so
// "fire across a 3h restart" is an instant, deterministic unit test. The clock is
// injected on the Workflow (Execute) or pinned per-Tick (the host-supplied now).

// epoch is a fixed, arbitrary base instant the fake clock starts at. Using a real
// (non-zero) wall-clock value keeps fireAt = now+d in the int64 unix-nanos range
// the persistence path round-trips.
var epoch = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

// recordingNode is a downstream node that records it ran (so "fired then the
// downstream ran" / "did not re-run on resume" is observable).
func timerDownstream(name string, c *execCounter) *Node {
	return NewNode(name, ActionFunc(func(_ context.Context, data *WorkflowData) error {
		c.inc(name)
		data.SetOutput(name, "ran-"+name)
		return nil
	}))
}

// TestTimer_ParksThenFiresOnTick is the headline Layer-1 timer test (acceptance
// #2): a timer set to fire at epoch+1h parks while the clock is before fireAt, and
// fires once a Tick supplies a now at/after fireAt. Deterministic via FakeClock.
func TestTimer_ParksThenFiresOnTick(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-tick"
	clk := NewFakeClock(epoch)
	counter := newExecCounter()

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		mustAddNode(t, d, timerDownstream("after", counter))
		mustAddDep(t, d, "sleep", "after")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	// Reaching the timer parks the run (the process could now exit).
	err := build().Execute(context.Background())
	require.ErrorIs(t, err, ErrSuspended, "a timer parks the run until it is due")

	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "sleep", Waiting)
	assertStatus(t, persisted, "after", Pending)
	fireAt, armed := persisted.GetWait("sleep")
	require.True(t, armed, "the timer persisted an absolute fireAt")
	assert.Equal(t, epoch.Add(time.Hour).UnixNano(), fireAt, "fireAt = arm-time + duration (absolute)")
	assert.Equal(t, 0, counter.get("after"), "downstream must not run while the timer is parked")

	// A Tick BEFORE fireAt fires nothing (spurious-early-wake re-checks fireAt<=now).
	fired, err := build().Tick(context.Background(), epoch.Add(30*time.Minute))
	require.NoError(t, err)
	assert.False(t, fired, "a Tick before fireAt must not fire the timer")
	persisted, _ = store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, persisted, "sleep", Waiting)

	// A Tick AT/AFTER fireAt fires it; the run converges, downstream runs once.
	fired, err = build().Tick(context.Background(), epoch.Add(time.Hour))
	require.NoError(t, err, "the run converges once the only timer fires")
	assert.True(t, fired, "a Tick at/after fireAt fires the timer")

	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
	_, stillArmed := final.GetWait("sleep")
	assert.False(t, stillArmed, "a fired timer clears its durable wait metadata")
	assert.Equal(t, 1, counter.get("after"), "downstream runs exactly once after the timer fires")
}

// TestTimer_OverdueFiresImmediatelyOnResume is acceptance #3: a timer armed in one
// process, with the clock then jumped 3h forward (modeling a long downtime),
// fires IMMEDIATELY on the next resume — fire-at-or-after, overdue fires at once.
func TestTimer_OverdueFiresImmediatelyOnResume(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-overdue"
	clk := NewFakeClock(epoch)
	counter := newExecCounter()

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		mustAddNode(t, d, timerDownstream("after", counter))
		mustAddDep(t, d, "sleep", "after")
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	// Arm (fireAt = epoch+1h) and park.
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)

	// "Restart" 3h later — the timer is now 2h overdue. A plain resume (Execute,
	// not Tick) with the clock advanced fires it immediately on load.
	clk.Advance(3 * time.Hour)
	require.NoError(t, build().Execute(context.Background()), "an overdue timer fires immediately on resume")

	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
	assert.Equal(t, 1, counter.get("after"))
}

// TestTimer_DurableAcrossStoreReload is acceptance #3 (crash/restart) through the
// real FlatBuffers file store: arm a timer, drop the in-memory workflow entirely,
// reconstruct from the SAME store + WorkflowID, and the timer re-arms from the
// persisted fireAt and fires. Proves the timer is durable DATA, not a live handle.
func TestTimer_DurableAcrossStoreReload(t *testing.T) {
	dir := t.TempDir()
	const id = "timer-durable"

	build := func(clk Clock) *Workflow {
		store, err := NewFlatBuffersStore(dir)
		require.NoError(t, err)
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", 2*time.Hour))
		w := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
		return w
	}

	// Process 1: arm + park, then "crash" (just discard the workflow).
	require.ErrorIs(t, build(NewFakeClock(epoch)).Execute(context.Background()), ErrSuspended)

	// Process 2: a brand-new store handle + workflow over the same dir/ID. The
	// fireAt was persisted to disk; resume past it fires.
	require.NoError(t, build(NewFakeClock(epoch.Add(3*time.Hour))).Execute(context.Background()),
		"a fresh process re-arms the timer from the on-disk fireAt and fires it")

	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	final, err := store.Load(id)
	require.NoError(t, err)
	assertStatus(t, final, "sleep", Completed)
}

// TestTimer_NotDueDoesNotFire pins the "before fireAt -> doesn't" half of
// acceptance #2 through a plain Execute resume (not just Tick): resuming with the
// clock still before fireAt re-parks, never fires.
func TestTimer_NotDueDoesNotFire(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-notdue"
	clk := NewFakeClock(epoch)

	build := func() *Workflow {
		d := NewDAG(id)
		mustAddNode(t, d, NewTimerNode("sleep", time.Hour))
		return &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: clk}
	}

	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended)
	// Advance, but not past fireAt.
	clk.Advance(30 * time.Minute)
	require.ErrorIs(t, build().Execute(context.Background()), ErrSuspended, "still before fireAt -> re-parks")
	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, persisted, "sleep", Waiting)
}

// TestTimer_FireAtInt64Fidelity re-asserts the chunk-3 int64-fidelity property for
// fireAt (acceptance #4): the int64 killer magnitudes (MaxInt64/MinInt64/2^53±)
// round-trip through BOTH the FlatBuffers and JSONFileStore Save/Load paths without
// the float64 corruption. fireAt is unix-nanos int64, so this is on the critical
// line (the v0.7.1 first-CI-run bug).
func TestTimer_FireAtInt64Fidelity(t *testing.T) {
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
			for _, fireAt := range fidelityKillers {
				const id = "fid-timer"
				d := NewWorkflowData(id)
				d.SetWait("sleep", fireAt)
				d.SetNodeStatus("sleep", Waiting) // a parked timer carries both
				require.NoError(t, store.Save(d))

				got, err := store.Load(id)
				require.NoError(t, err)

				gotFireAt, armed := got.GetWait("sleep")
				require.True(t, armed, "fireAt %d must survive the round-trip", fireAt)
				assert.Equal(t, fireAt, gotFireAt,
					"fireAt %d must round-trip at full int64 magnitude (never float64-corrupted)", fireAt)
				st, _ := got.GetNodeStatus("sleep")
				assert.Equal(t, Waiting, st, "the parked Waiting status survives alongside fireAt")
			}
		})
	}
}

// TestTimer_FireAtOverflowClampsFarFuture is the Review-F3 guard: a huge-but-valid
// time.Duration (the type holds ~292y; here ~250y) pushes armNow+dur past the int64
// UnixNano ceiling (~year 2262), wrapping fireAt to a negative value <= now — which
// (pre-fix) fired the timer IMMEDIATELY, the opposite of the request. The overflow
// guard clamps such a fireAt to MaxInt64 (the far future), so the timer PARKS at a
// normal resume time instead of firing at once. Bite: drop the
// `if a.duration > 0 && fireAt <= now.UnixNano()` clamp in timer.go -> fireAt wraps
// negative -> a normal-time resume fires it -> both assertions below fail.
func TestTimer_FireAtOverflowClampsFarFuture(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-overflow"
	armNow := epoch                       // 2026
	hugeDur := 250 * 365 * 24 * time.Hour // ~250y: armNow+hugeDur overflows UnixNano

	d := NewDAG(id)
	mustAddNode(t, d, NewTimerNode("sleep", hugeDur))
	w := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(armNow)}
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	persisted, err := store.Load(id)
	require.NoError(t, err)
	fireAt, armed := persisted.GetWait("sleep")
	require.True(t, armed)
	assert.Equal(t, int64(math.MaxInt64), fireAt,
		"an overflowing duration clamps fireAt to MaxInt64, not a negative wrap")

	// At a normal resume time (1h later), the clamped far-future timer must still
	// PARK, not fire — the opposite of the pre-fix immediate-fire bug.
	w2 := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(armNow.Add(time.Hour))}
	require.ErrorIs(t, w2.Execute(context.Background()), ErrSuspended,
		"a far-future (clamped) timer must not fire at a normal resume time")
}

// TestTimer_NoClobberRoundTrip is the chunk-1 D-03 discipline re-run for the
// `waits` schema add: a parked timer (Waiting + fireAt) survives the FlatBuffers
// TestTimer_DueTimersDoesNotResurrectFailedRun is the DEC-M10-P36-F2 load-bearing
// guard (auto-triggered failed->completed masking). A TimerNode arms+parks in the
// SAME level as a node that hard-fails; fail-fast outranks park, so the run returns
// an *ExecutionError and the chokepoint resets the timer's status Waiting->Pending
// while PRESERVING its fireAt. A naive "tick all due timers" host loop must NOT
// re-enter this failed run and drive it failed->completed. Both F2 layers protect
// it (root-cause status!=Waiting + boundary hard-failure).
// Bite: revert DueTimers to the pre-fix blind `fireAt<=now` scan -> the preserved
// timer is reported due -> Tick re-enters -> the timer fires -> Execute returns nil
// -> failed->completed. Both assertions below then fail.
func TestTimer_DueTimersDoesNotResurrectFailedRun(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-failed-run"
	d := NewDAG(id)
	mustAddNode(t, d, NewTimerNode("sleep", time.Hour)) // level 0: arms+parks (Waiting+fireAt)
	mustAddNode(t, d, NewNode("boom", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("boom") // level 0: hard fail -> fail-fast outranks the park
	})))
	w := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
	err := w.Execute(context.Background())

	var execErr *ExecutionError
	require.True(t, errors.As(err, &execErr), "the run failed (hard failure)")

	later := epoch.Add(2 * time.Hour) // well past the 1h fireAt
	due, derr := w.DueTimers(later)
	require.NoError(t, derr)
	assert.Empty(t, due, "a failed run's preserved-fireAt timer must not be reported due")

	w2 := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(later)}
	fired, terr := w2.Tick(context.Background(), later)
	require.NoError(t, terr)
	assert.False(t, fired, "Tick must not fire a timer in a failed run")
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	st, _ := persisted.GetNodeStatus("boom")
	assert.Equal(t, Failed, st, "the failed node stays Failed — the run was not resurrected")
}

// TestTimer_DueTimersRequiresWaitingStatus bites the F2 ROOT-CAUSE layer in
// isolation (no hard failure, so the boundary layer does not fire): a CANCELLED
// run leaves its timer Pending (chokepoint reset) with fireAt preserved. DueTimers
// must report it NOT due — only a status==Waiting (legitimately-parked) timer is
// due; a host loop must not auto-resurrect a cancelled run.
// Bite: drop the `st == Waiting` filter in DueTimers -> the Pending-but-armed timer
// is reported due (nothing else catches it here) -> this fails.
func TestTimer_DueTimersRequiresWaitingStatus(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timer-cancelled-run"
	ctx, cancel := context.WithCancel(context.Background())
	d := NewDAG(id)
	mustAddNode(t, d, NewTimerNode("sleep", time.Hour)) // level 0: arms+parks
	mustAddNode(t, d, NewNode("canceller", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		cancel() // level 0: cancel the run (no hard failure)
		return nil
	})))
	w := &Workflow{DAG: d, WorkflowID: id, Store: store, Clock: NewFakeClock(epoch)}
	err := w.Execute(ctx)
	require.True(t, errors.Is(err, context.Canceled), "the run was cancelled, not failed")

	// Timer is now Pending (chokepoint) with fireAt preserved; no hard failure, so
	// only the status==Waiting root-cause filter keeps it out of DueTimers.
	later := epoch.Add(2 * time.Hour)
	due, derr := w.DueTimers(later)
	require.NoError(t, derr)
	assert.Empty(t, due, "a cancelled run's Pending-but-armed timer is not due (status!=Waiting)")
}

// Save/Load round-trip through BOTH conversion switches (statusToFBStatus /
// fbStatusToNodeStatus) with neither the status NOR the fireAt clobbered. (A
// missed switch arm silently resets Waiting->Pending — the AF1/N2 class of bug.)
func TestTimer_NoClobberRoundTrip(t *testing.T) {
	store, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	const id = "noclobber"

	d := NewWorkflowData(id)
	// A mix: one parked timer, plus every other status present, so the round-trip
	// exercises every arm of both conversion switches alongside the new waits.
	d.SetNodeStatus("done", Completed)
	d.SetNodeStatus("fail", Failed)
	d.SetNodeStatus("skip", Skipped)
	d.SetNodeStatus("run", Running)
	d.SetNodeStatus("pend", Pending)
	d.SetNodeStatus("timer", Waiting)
	d.SetWait("timer", epoch.Add(time.Hour).UnixNano())
	require.NoError(t, store.Save(d))

	got, err := store.Load(id)
	require.NoError(t, err)
	for node, want := range map[string]NodeStatus{
		"done": Completed, "fail": Failed, "skip": Skipped,
		"run": Running, "pend": Pending, "timer": Waiting,
	} {
		st, ok := got.GetNodeStatus(node)
		require.True(t, ok, "node %s missing after round-trip", node)
		assert.Equal(t, want, st, "node %s status clobbered across FB round-trip", node)
	}
	fireAt, armed := got.GetWait("timer")
	require.True(t, armed, "the timer's fireAt must survive alongside its Waiting status")
	assert.Equal(t, epoch.Add(time.Hour).UnixNano(), fireAt)
}

// TestTimer_IsDeclaredSuspensionNode guards that the timer action parks via the
// chunk-1 marker seam (not as a generic failure) and bypasses retry/timeout — the
// declared-suspension contract carried forward from chunk-1.
func TestTimer_IsDeclaredSuspensionNode(t *testing.T) {
	node := NewTimerNode("sleep", time.Hour)
	_, ok := node.Action.(suspendableAction)
	require.True(t, ok, "a TimerNode action must satisfy the suspendableAction marker")

	// Direct node.Execute (clock falls back to system clock): first run parks.
	data := NewWorkflowData("wf")
	node.RetryCount = 3
	node.Timeout = time.Nanosecond // would trip instantly if the park were timed out
	err := node.Execute(context.Background(), data)
	require.ErrorIs(t, err, ErrSuspended, "a timer parks (bypassing retry/timeout), not fails")
	st, _ := data.GetNodeStatus("sleep")
	assert.Equal(t, Waiting, st)
}

// TestTimer_BuilderAddTimer covers the public builder surface: AddTimer wires a
// declared timer that parks, and DependsOn ordering holds.
func TestTimer_BuilderAddTimer(t *testing.T) {
	store := NewInMemoryStore()
	const id = "builder-timer"
	clk := NewFakeClock(epoch)

	mk := func() *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID(id).WithStore(store).WithClock(clk)
		b.AddTimer("sleep", time.Minute)
		b.AddNode("after").WithAction(func(_ context.Context, _ *WorkflowData) error { return nil }).DependsOn("sleep")
		w, err := FromBuilder(b)
		require.NoError(t, err)
		return w
	}

	require.ErrorIs(t, mk().Execute(context.Background()), ErrSuspended)
	clk.Advance(time.Minute)
	require.NoError(t, mk().Execute(context.Background()))
	final, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, final, "sleep", Completed)
	assertStatus(t, final, "after", Completed)
}

// sanity: epoch+killer math stays in range (guards the test data itself).
func TestTimer_KillerRangeSanity(t *testing.T) {
	assert.Greater(t, int64(math.MaxInt64), epoch.UnixNano())
}
