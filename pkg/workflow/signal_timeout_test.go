package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M22 ph113 — durable first-of(signal, timer) adversarial suite.
//
// Every test drives time DETERMINISTICALLY through a FakeClock (D36-07): arming,
// downtime, and "fire across a restart" are instant, deterministic unit steps.
// `epoch` (a non-zero wall instant) is shared with timer_test.go so fireAt =
// now+d stays in the int64 unix-nanos range the persistence path round-trips.
//
// The six bites map 1:1 to the phase acceptance and each is SEED-BREAK-PROVEN:
// a named production edit in signal_timeout.go flips the asserted arm and turns
// the test RED (recorded in the finding record), then is reverted.

// buildFirstOf wires a single declared first-of(signal, timer) node "node" over
// the given store/clock — the unit under test, driven exactly like the timer and
// signal suites (a store-backed *Workflow, Execute to arm/resume, Tick to fire).
func buildFirstOf(t *testing.T, store WorkflowStore, id, node, signal string, timeout time.Duration, clk Clock) *Workflow {
	t.Helper()
	d := newDAGForTest(id)
	mustAddNode(t, d, newWaitForSignalOrTimeoutNode(node, signal, timeout))
	return &Workflow{dag: d, WorkflowID: id, Store: store, Clock: clk}
}

// TestSignalTimeout_SignalBeforeTimeout (bite a): a long deadline is armed, the
// named signal is delivered, and the resume applies the payload and disarms the
// timer — timedOutKey stays UNSET and the wait is cleared.
// SEED-BREAK: drop `data.ClearWait(a.nodeName)` on the signal path
// (signal_timeout.go:105) -> the deadline stays armed after the signal wins ->
// the "ClearWait ran" assertion goes RED.
func TestSignalTimeout_SignalBeforeTimeout(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-signal-wins"
	clk := NewFakeClock(epoch)
	w := buildFirstOf(t, store, id, "wait", "approve", time.Hour, clk)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended, "first encounter arms the deadline and parks")

	armedData, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, armedData, "wait", Waiting)
	fireAt, isArmed := armedData.GetWait("wait")
	require.True(t, isArmed, "the deadline is armed while parked")
	assert.Equal(t, epoch.Add(time.Hour).UnixNano(), fireAt, "fireAt = arm-time + duration (absolute)")

	require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "approve", Payload: "ok"}))
	require.NoError(t, w.Execute(context.Background()), "the delivered signal wins and converges the run")

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)

	payload, applied := appliedSignalPayload(t, store, id, "wait")
	require.True(t, applied, "the signal payload is applied idempotently")
	assert.Equal(t, "ok", payload)
	out, ok := final.GetOutput("wait")
	require.True(t, ok, "the node set an output")
	assert.Equal(t, "ok", out, "the node output is the signal payload")

	_, timedOut := final.Get(timedOutKey("wait"))
	assert.False(t, timedOut, "the signal path never sets the timeout disposition")

	_, stillArmed := final.GetWait("wait")
	assert.False(t, stillArmed, "ClearWait ran on the signal path — the deadline is disarmed")
}

// TestSignalTimeout_EarlySignalWinsOnFirstEncounter (bite a', review ph113-F1): a
// signal buffered BEFORE the node is first reached (early-signal buffering, a supported
// DeliverSignal feature) must win on the FIRST encounter — the node completes at once,
// it does NOT park for up to the whole timeout. This requires the mailbox peek to run
// BEFORE the arm-check.
// SEED-BREAK: move the mailbox peek AFTER the `if !armed { arm; park }` block
// (signal_timeout.go) -> the first encounter arms + parks despite the buffered signal ->
// the "no park / Completed on first Execute" assertions go RED.
func TestSignalTimeout_EarlySignalWinsOnFirstEncounter(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-early-signal"
	clk := NewFakeClock(epoch)
	w := buildFirstOf(t, store, id, "wait", "approve", time.Hour, clk)

	// Deliver the signal BEFORE the node is ever executed (early buffering).
	require.NoError(t, w.DeliverSignal(Signal{ID: "e1", Name: "approve", Payload: "early"}))

	// The FIRST Execute must consume it and converge — NOT park (no ErrSuspended).
	require.NoError(t, w.Execute(context.Background()),
		"an early-buffered signal wins on the first encounter — no park up to the timeout")

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)
	payload, applied := appliedSignalPayload(t, store, id, "wait")
	require.True(t, applied, "the early-buffered signal payload is applied on the first encounter")
	assert.Equal(t, "early", payload)
	_, timedOut := final.Get(timedOutKey("wait"))
	assert.False(t, timedOut, "the early signal wins — no timeout disposition")
	_, stillArmed := final.GetWait("wait")
	assert.False(t, stillArmed, "no stale deadline is left armed (the peek-first path never armed, or cleared it)")
}

// TestSignalTimeout_TimeoutThenLateSignalNoop (bite b): the deadline passes and
// the timeout arm wins (timedOutKey true, sentinel output); a signal delivered
// AFTER the node is terminal is a NO-OP — the payload is never applied and the
// output stays the timeout sentinel (guarded by the executor's terminal-node
// skip; the first-of node completes on the timeout path).
// SEED-BREAK: change the timeout path's `return nil` (signal_timeout.go:115) to
// `return ErrSuspended` -> the timeout never completes the node -> the late
// DeliverAndResume re-runs the action and applies "LATE" -> the "still terminal /
// payload NOT applied" assertions go RED.
func TestSignalTimeout_TimeoutThenLateSignalNoop(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-timeout-wins"
	clk := NewFakeClock(epoch)
	w := buildFirstOf(t, store, id, "wait", "approve", time.Hour, clk)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	clk.Advance(2 * time.Hour) // past the epoch+1h deadline
	require.NoError(t, w.Execute(context.Background()), "an overdue deadline fires the timeout arm on resume")

	mid, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, mid, "wait", Completed)
	timedOut, ok := mid.Get(timedOutKey("wait"))
	require.True(t, ok, "the timeout disposition is recorded")
	assert.Equal(t, true, timedOut)
	out, ok := mid.GetOutput("wait")
	require.True(t, ok)
	// AUD-026: a node output reloads in its canonical string form on every store (the
	// bool sentinel `true` becomes "true"). The disposition DATA key above stays a bool.
	assert.Equal(t, "true", out, "the node output is the timeout sentinel")
	_, applied := appliedSignalPayload(t, store, id, "wait")
	assert.False(t, applied, "no signal payload is applied on the timeout path")
	_, stillArmed := mid.GetWait("wait")
	assert.False(t, stillArmed, "the fired timeout clears its durable wait")

	// A signal delivered AFTER the timeout won is a NO-OP: the node is terminal.
	require.NoError(t, w.DeliverAndResume(context.Background(), Signal{ID: "late", Name: "approve", Payload: "LATE"}))

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)
	timedOut2, _ := final.Get(timedOutKey("wait"))
	assert.Equal(t, true, timedOut2, "the timeout disposition is unchanged by the late signal")
	out2, _ := final.GetOutput("wait")
	assert.Equal(t, "true", out2, "the output is still the timeout sentinel, not the late payload")
	_, applied2 := appliedSignalPayload(t, store, id, "wait")
	assert.False(t, applied2, "the late signal payload is NOT applied — the node was terminal")
}

// TestSignalTimeout_SameEncounterSignalWins (bite c, THE tie-break): one resume
// sees BOTH a delivered signal AND an already-overdue deadline. The
// mailbox-before-due ordering makes the SIGNAL win — the payload is applied and
// timedOutKey stays UNSET.
// SEED-BREAK: reorder Execute to run the due-check (signal_timeout.go:109-116)
// BEFORE the mailbox peek (:90-107) -> the timeout wins the tie -> "payload
// applied / timedOut unset" go RED. This proves the mailbox-before-due ordering.
func TestSignalTimeout_SameEncounterSignalWins(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-tiebreak"
	clk := NewFakeClock(epoch)
	w := buildFirstOf(t, store, id, "wait", "approve", time.Hour, clk)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	clk.Advance(2 * time.Hour) // the deadline is now overdue
	require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "approve", Payload: "SIG"}))
	require.NoError(t, w.Execute(context.Background()), "one resume with BOTH ready: signal wins the tie")

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)
	payload, applied := appliedSignalPayload(t, store, id, "wait")
	require.True(t, applied, "the mailbox-before-due ordering applies the signal payload")
	assert.Equal(t, "SIG", payload)
	out, _ := final.GetOutput("wait")
	assert.Equal(t, "SIG", out)
	_, timedOut := final.Get(timedOutKey("wait"))
	assert.False(t, timedOut, "on a same-encounter tie the timeout disposition is NOT set — signal wins")
}

// TestSignalTimeout_DurableRemainingDeadline (bite d, absolute-instant): the
// deadline is an ABSOLUTE instant frozen at first encounter. A restart at
// epoch+T/2 (fresh workflow, same store, re-reading the persisted fireAt) does
// NOT fire — the original deadline (epoch+T) has not passed. It fires only at/
// after that original instant, not T/2-later.
// SEED-BREAK: drop the armed-check (signal_timeout.go:66, `if !armed`) so every
// encounter re-arms -> the restart resets fireAt to (epoch+T/2)+T -> the "fireAt
// == epoch+T" and "fires at epoch+T" assertions go RED.
func TestSignalTimeout_DurableRemainingDeadline(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-durable"
	const T = time.Hour
	wantFireAt := epoch.Add(T).UnixNano()

	// Process 1: arm at epoch, deadline = epoch+T, then park (the process exits).
	require.ErrorIs(t,
		buildFirstOf(t, store, id, "wait", "approve", T, NewFakeClock(epoch)).Execute(context.Background()),
		ErrSuspended)
	p1, lerr := store.Load(id)
	require.NoError(t, lerr)
	f1, armed := p1.GetWait("wait")
	require.True(t, armed)
	assert.Equal(t, wantFireAt, f1, "the absolute deadline is epoch+T")

	// "Restart" at epoch+T/2 (downtime): a fresh workflow over the SAME store
	// re-reads the persisted deadline; it is not yet due, so it re-parks.
	require.ErrorIs(t,
		buildFirstOf(t, store, id, "wait", "approve", T, NewFakeClock(epoch.Add(T/2))).Execute(context.Background()),
		ErrSuspended,
		"at epoch+T/2 the original deadline has not passed — re-park, do not fire")
	p2, lerr := store.Load(id)
	require.NoError(t, lerr)
	f2, armed2 := p2.GetWait("wait")
	require.True(t, armed2, "the deadline is still armed after the restart")
	assert.Equal(t, wantFireAt, f2, "the deadline is the ORIGINAL absolute instant, NOT reset to restart+T")

	// Advance to the original deadline: it fires at epoch+T, not T/2 later.
	require.NoError(t,
		buildFirstOf(t, store, id, "wait", "approve", T, NewFakeClock(epoch.Add(T))).Execute(context.Background()),
		"at the original deadline the timeout fires")
	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)
	timedOut, ok := final.Get(timedOutKey("wait"))
	require.True(t, ok)
	assert.Equal(t, true, timedOut, "the timeout arm won at the original absolute deadline")
}

// TestSignalTimeout_ExactlyOneAcrossCrash (bite e): store-seed the ambiguous
// state a crash-BETWEEN-fire-and-checkpoint leaves — the node Waiting, an OVERDUE
// fireAt, AND the signal already in the mailbox. The recovery resume must pick the
// SAME single winner the stable tie-break dictates (signal); no path yields BOTH
// timedOutKey AND an applied payload.
// SEED-BREAK: drop the signal path's `return nil` (signal_timeout.go:106) so it
// falls through to the due-check after applying the payload -> BOTH the payload
// AND timedOutKey are set -> the "not both / timedOut unset" assertion goes RED.
func TestSignalTimeout_ExactlyOneAcrossCrash(t *testing.T) {
	store := NewInMemoryStore()
	const id = "st-crash"

	// Store-seed the ambiguous crash-before-checkpoint state directly (dag.go
	// re-runs a persisted non-terminal Waiting node with resolved deps).
	seed := NewWorkflowData(id)
	seed.SetNodeStatus("wait", Waiting)
	seed.SetWait("wait", epoch.UnixNano()) // overdue relative to the resume clock
	require.NoError(t, store.Save(seed))
	require.NoError(t, store.DeliverSignal(id, Signal{ID: "s1", Name: "approve", Payload: "P"}))

	// Resume well past the deadline: exactly one arm must win.
	w := buildFirstOf(t, store, id, "wait", "approve", time.Hour, NewFakeClock(epoch.Add(time.Hour)))
	require.NoError(t, w.Execute(context.Background()), "the crash-recovery resume converges to a single winner")

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "wait", Completed)

	payload, applied := appliedSignalPayload(t, store, id, "wait")
	_, timedOut := final.Get(timedOutKey("wait"))

	// The stable tie-break makes the SIGNAL the single winner; NEVER both arms.
	assert.True(t, applied, "the signal is the single winner across the crash")
	assert.Equal(t, "P", payload)
	assert.False(t, timedOut, "no path yields BOTH an applied payload AND the timeout disposition")
}

// TestSignalTimeout_BothWakePaths (bite f): the first-of node inherits BOTH wake
// mechanisms for free. (i) DeliverAndResume wakes the signal arm; (ii) Tick/
// DueTimers wakes the timeout arm because the node registers its fireAt in `waits`
// like a timer and DueTimers does not special-case timerAction.
// SEED-BREAK: drop `data.SetWait(a.nodeName, fireAt)` on arming
// (signal_timeout.go:81) -> the node never registers a durable deadline ->
// DueTimers reports nothing (subtest ii RED) and the node never becomes armed so
// DeliverAndResume cannot wake it either (subtest i RED).
func TestSignalTimeout_BothWakePaths(t *testing.T) {
	// (i) DeliverAndResume wakes the SIGNAL arm.
	t.Run("deliver_and_resume_wakes_signal", func(t *testing.T) {
		store := NewInMemoryStore()
		const id = "st-wake-signal"
		w := buildFirstOf(t, store, id, "wait", "go", time.Hour, NewFakeClock(epoch))
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
		require.NoError(t, w.DeliverAndResume(context.Background(), Signal{ID: "g1", Name: "go", Payload: "v"}))
		final, lerr := store.Load(id)
		require.NoError(t, lerr)
		assertStatus(t, final, "wait", Completed)
		payload, applied := appliedSignalPayload(t, store, id, "wait")
		require.True(t, applied, "DeliverAndResume woke the signal arm")
		assert.Equal(t, "v", payload)
	})

	// (ii) Tick/DueTimers wakes the TIMEOUT arm.
	t.Run("tick_wakes_timeout", func(t *testing.T) {
		store := NewInMemoryStore()
		const id = "st-wake-timeout"
		w := buildFirstOf(t, store, id, "wait", "go", time.Hour, NewFakeClock(epoch))
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

		before, err := w.DueTimers(epoch.Add(30 * time.Minute))
		require.NoError(t, err)
		assert.NotContains(t, before, "wait", "before the deadline the node is not due")

		due, err := w.DueTimers(epoch.Add(time.Hour))
		require.NoError(t, err)
		assert.Contains(t, due, "wait", "DueTimers reports the parked first-of node once its deadline is due")

		fired, err := w.Tick(context.Background(), epoch.Add(time.Hour))
		require.NoError(t, err)
		assert.True(t, fired, "a Tick at/after the deadline fires the timeout arm")

		final, lerr := store.Load(id)
		require.NoError(t, lerr)
		assertStatus(t, final, "wait", Completed)
		timedOut, ok := final.Get(timedOutKey("wait"))
		require.True(t, ok)
		assert.Equal(t, true, timedOut, "the Tick fired the timeout arm's disposition")
	})
}
