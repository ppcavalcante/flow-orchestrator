package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M10 phase-35 chunk-1 T4 — executor suspend/re-enter integration tests.
//
// These are the deterministic DAG-level guards for the suspend spine (the gopter
// property in T6 generalizes them over random DAGs). They exercise the full
// Workflow.Execute path: barrier-drain -> checkpoint flush -> ErrSuspended, the
// "parked != NodeError" split, WaitingIsLiveNotTerminal, durable-flush-before-
// suspend, and convergence on re-entry. Workflow.Execute owns its WorkflowData
// (loads from / saves to the Store by WorkflowID), so status is inspected by
// loading the persisted state back from the store.

// completingSuspendNode builds a declared suspension node that parks while the
// gate is parked and, once woken, records a run + writes an output so resume
// convergence and "did not re-run completed nodes" are observable.
func completingSuspendNode(name string, gate *parkGate, c *execCounter) *Node {
	return NewNode(name, &suspendingAction{
		gate: gate,
		onComplete: func(_ context.Context, data *WorkflowData) error {
			c.inc(name)
			data.SetOutput(name, "out-"+name)
			return nil
		},
	})
}

// TestSuspend_ParkResumesToConvergence is the headline MH-5 + MH-3 test: a chain
// a -> park -> c under a Checkpointer store parks at the barrier (Execute returns
// ErrSuspended with the checkpoint already flushed), and re-entering Execute after
// the wake converges to the same final state a never-suspended run reaches — with
// the already-completed upstream NOT re-run.
func TestSuspend_ParkResumesToConvergence(t *testing.T) {
	store := NewInMemoryStore()
	const id = "susp-converge"
	gate := newParkGate()
	counter := newExecCounter()

	buildDAG := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, countingNode("a", counter))
		mustAddNode(t, d, completingSuspendNode("park", gate, counter))
		mustAddNode(t, d, countingNode("c", counter))
		mustAddDep(t, d, "a", "park") // a before park
		mustAddDep(t, d, "park", "c") // park before c
		return d
	}

	// --- Phase 1: run until the park. ---
	w1 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store}
	err := w1.Execute(context.Background())

	require.ErrorIs(t, err, ErrSuspended, "a parked run returns the suspend sentinel")
	assert.Equal(t, 1, counter.get("a"))
	assert.Equal(t, 0, counter.get("park"), "park has not completed yet")
	assert.Equal(t, 0, counter.get("c"))

	// MH-3 durable-flush-before-suspend: the checkpoint is already persisted when
	// ErrSuspended returns — loading from the store yields the Waiting frontier.
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr, "the park must have flushed a durable checkpoint before returning")
	assertStatus(t, persisted, "a", Completed)
	assertStatus(t, persisted, "park", Waiting)
	assertStatus(t, persisted, "c", Pending)

	// --- Phase 2: the external event arrives; resume converges. ---
	gate.wake()
	w2 := &Workflow{DAG: buildDAG(), WorkflowID: id, Store: store}
	require.NoError(t, w2.Execute(context.Background()), "re-entry after wake converges")

	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, final, "a", Completed)
	assertStatus(t, final, "park", Completed)
	assertStatus(t, final, "c", Completed)
	assert.Equal(t, 1, counter.get("a"), "a was persisted Completed and must NOT re-run on resume")
	assert.Equal(t, 1, counter.get("park"), "park completes exactly once after wake")
	assert.Equal(t, 1, counter.get("c"), "downstream runs after the parked node wakes")
}

// TestSuspend_DependentStaysPendingNotSkipped is the WaitingIsLiveNotTerminal
// (D-07/MH-2) guard: a node depending on a parked node stays Pending — it is NOT
// Skipped (a Waiting dep is not a skip cause) and does NOT run while parked.
func TestSuspend_DependentStaysPendingNotSkipped(t *testing.T) {
	store := NewInMemoryStore()
	const id = "susp-dependent"
	gate := newParkGate()
	counter := newExecCounter()

	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))
	mustAddNode(t, d, countingNode("dependent", counter))
	mustAddDep(t, d, "park", "dependent")

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())

	require.ErrorIs(t, err, ErrSuspended)
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "park", Waiting)
	assertStatus(t, persisted, "dependent", Pending) // NOT Skipped
	assert.Equal(t, 0, counter.get("dependent"), "a dependent of a parked node must not run")
}

// TestSuspend_ParkDoesNotFailFastSiblings is the Model-A + "parked != NodeError"
// (MH-2) guard: a parked node does not cancel its level siblings and does not
// produce an *ExecutionError. An independent sibling in the same level completes.
func TestSuspend_ParkDoesNotFailFastSiblings(t *testing.T) {
	store := NewInMemoryStore()
	const id = "susp-siblings"
	gate := newParkGate()
	counter := newExecCounter()

	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))
	mustAddNode(t, d, countingNode("sibling", counter)) // independent, same level 0

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())

	require.ErrorIs(t, err, ErrSuspended)
	var execErr *ExecutionError
	assert.False(t, errors.As(err, &execErr), "a park is not a failure — no ExecutionError")
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "park", Waiting)
	assertStatus(t, persisted, "sibling", Completed)
	assert.Equal(t, 1, counter.get("sibling"), "a parked node must not cancel its level siblings")
}

// TestSuspend_NoCheckpointerReturnsConfigError is the checkpoint==nil contract
// (durable-flush-before-suspend): a park with no durable checkpoint wired (here
// DAG.Execute driven directly with no withCheckpoint injection, so
// checkpointFrom(ctx) is nil) is a configuration error, NOT a silently
// non-durable ErrSuspended.
func TestSuspend_NoCheckpointerReturnsConfigError(t *testing.T) {
	gate := newParkGate()
	d := NewDAG("susp-nocp")
	mustAddNode(t, d, newSuspendingNode("park", gate))

	err := d.Execute(context.Background(), NewWorkflowData("susp-nocp"))

	require.ErrorIs(t, err, ErrSuspendRequiresCheckpointer)
	assert.False(t, errors.Is(err, ErrSuspended),
		"a non-durable park must not surface as the success suspend arm")
}

// TestSuspend_ParkWithFailFastClearsWaiting is the Review-F1 guard: when a node
// parks in the SAME level as a non-continue-on-error sibling that hard-fails,
// fail-fast outranks the park (the run returns an *ExecutionError, not
// ErrSuspended) AND the parked node must NOT be persisted Waiting in the failed
// snapshot — it is reset to Pending so a failed run carries no false "suspended"
// frontier. (clearParked on the fail-fast path.)
func TestSuspend_ParkWithFailFastClearsWaiting(t *testing.T) {
	store := NewInMemoryStore()
	const id = "susp-failfast"
	gate := newParkGate()

	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))
	mustAddNode(t, d, NewNode("boom", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("boom")
	}))) // independent, same level 0, non-continue-on-error -> fail-fast

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())

	// Fail-fast outranks park: an ExecutionError, never the suspend arm.
	var execErr *ExecutionError
	require.True(t, errors.As(err, &execErr), "a co-occurring hard failure returns *ExecutionError")
	require.False(t, errors.Is(err, ErrSuspended), "a failing run must not report as suspended")

	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "boom", Failed)
	// The parked node was reset off Waiting — no stray suspended frontier in a
	// failed snapshot.
	assertStatus(t, persisted, "park", Pending)
}

// TestSuspend_FlushErrorClearsWaiting is the AF1-b guard (the 4th and final
// non-suspend exit): when a node parks but the durable checkpoint FLUSH errors,
// the run did NOT suspend (it returns the checkpoint-failed error, not
// ErrSuspended), so the parked node must NOT be left Waiting — otherwise
// Workflow.Execute's failure-save would persist a stray suspended frontier that
// overclaims durability (a store inspector would see "parked" when nothing
// durably parked). Uniform with the cancel / fail-fast / checkpoint==nil exits.
// Bites: without clearParked on this branch the node stays Waiting and this fails.
func TestSuspend_FlushErrorClearsWaiting(t *testing.T) {
	gate := newParkGate()
	d := NewDAG("susp-flush-err")
	mustAddNode(t, d, newSuspendingNode("park", gate))
	// A wired checkpoint (passes the checkpoint!=nil guard) whose flush fails —
	// modelling a durable-store I/O error at the suspend barrier.
	cp := func(_ *WorkflowData) error { return errors.New("flush boom") }

	data := NewWorkflowData("susp-flush-err")
	err := d.Execute(withCheckpoint(context.Background(), cp), data)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrSuspended), "a failed flush did not suspend")
	assert.Contains(t, err.Error(), "checkpoint failed while suspending")
	st, ok := data.GetNodeStatus("park")
	require.True(t, ok)
	assert.Equal(t, Pending, st,
		"a failed-flush (non-suspend) run must not leave a stray Waiting frontier")
}

// TestSuspend_ChokepointDoesNotOverClearSuccess is the OVER-CLEAR bite for the
// suspend chokepoint — the load-bearing direction (an over-clear is a silent bug
// a per-exit reset could never cause). It inspects the IN-MEMORY data after a
// DIRECT DAG.Execute ErrSuspended, NOT the persisted store, on purpose: the
// durable checkpoint flush PRECEDES the chokepoint defer (D-10 flush-before-
// return), so the store already holds Waiting before the defer runs — an
// over-clearing defer therefore canNOT corrupt the persisted frontier (a
// store.Load assertion would pass vacuously). What it CAN corrupt is the
// in-memory parked node a direct caller inspects. So the guard's real job is
// in-memory observability: a successfully-parked node must read Waiting in data.
// Bite: drop the `if !errors.Is(retErr, ErrSuspended)` guard (clear
// unconditionally) → in-memory park == Pending → this fails. (D36-02.)
func TestSuspend_ChokepointDoesNotOverClearSuccess(t *testing.T) {
	gate := newParkGate()
	d := NewDAG("susp-overclear")
	mustAddNode(t, d, newSuspendingNode("park", gate))
	// A wired checkpointer so the park is a real durable suspend (ErrSuspended),
	// not the no-checkpointer config error. Direct DAG.Execute so the in-memory
	// data the chokepoint defer leaves behind is observable.
	store := NewInMemoryStore()
	cp := func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }

	data := NewWorkflowData("susp-overclear")
	err := d.Execute(withCheckpoint(context.Background(), cp), data)

	require.ErrorIs(t, err, ErrSuspended, "a durable park returns ErrSuspended")
	st, ok := data.GetNodeStatus("park")
	require.True(t, ok)
	assert.Equal(t, Waiting, st,
		"the chokepoint must NOT over-clear a successfully-parked node (guard on ErrSuspended)")
}

// flushErrorCheckpointer is an InMemoryStore whose SaveCheckpoint (the mid-run
// suspend flush) always errors, while its plain Save (the failure-save path)
// still works — modelling a durable-store I/O error exactly at the suspend
// barrier. It implements Checkpointer, so Workflow.Execute wires the flush.
type flushErrorCheckpointer struct {
	*InMemoryStore
}

func (s *flushErrorCheckpointer) SaveCheckpoint(_ *WorkflowData) error {
	return errors.New("flush boom")
}

// TestSuspend_FlushErrorViaWorkflowExecute is the chokepoint guard for the
// flush-error exit driven THROUGH Workflow.Execute (where the failure-save
// w.Store.Save actually persists, the path qa's inner-unit property missed): a
// park whose checkpoint flush errors returns a real error (not ErrSuspended),
// and the suspend chokepoint resets the parked node off Waiting so the
// failure-save persists no stray suspended frontier. (FIND-M10-P35-N2.)
func TestSuspend_FlushErrorViaWorkflowExecute(t *testing.T) {
	store := &flushErrorCheckpointer{InMemoryStore: NewInMemoryStore()}
	const id = "susp-flush-we"
	gate := newParkGate()
	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())

	require.Error(t, err)
	require.False(t, errors.Is(err, ErrSuspended), "a failed flush did not suspend")
	assert.Contains(t, err.Error(), "checkpoint failed while suspending")
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "park", Pending) // chokepoint cleared it; no stray Waiting
}

// TestSuspend_CancelWithParkViaWorkflowExecute is the chokepoint guard for the
// cancel exit driven THROUGH Workflow.Execute: a level holds a parking node and
// a sibling that cancels the run. Cancel outranks park (the run returns a
// cancellation, not ErrSuspended), and the suspend chokepoint resets the parked
// node off Waiting so the failure-save persists no stray suspended frontier.
func TestSuspend_CancelWithParkViaWorkflowExecute(t *testing.T) {
	store := NewInMemoryStore()
	const id = "susp-cancel-we"
	gate := newParkGate()
	ctx, cancel := context.WithCancel(context.Background())

	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))
	// Independent level-0 sibling that cancels the run from inside the level, so
	// the post-level ctx check fires with a node already parked.
	mustAddNode(t, d, NewNode("canceller", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		cancel()
		return nil
	})))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(ctx)

	require.Error(t, err)
	require.False(t, errors.Is(err, ErrSuspended), "a cancelled run did not suspend")
	require.True(t, errors.Is(err, context.Canceled), "cancel outranks park")
	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "park", Pending) // chokepoint cleared it; no stray Waiting
}

// assertStatus is a small helper asserting a node's current status.
func assertStatus(t *testing.T, data *WorkflowData, node string, want NodeStatus) {
	t.Helper()
	got, ok := data.GetNodeStatus(node)
	require.True(t, ok, "node %q must have a status", node)
	assert.Equal(t, want, got, "node %q status", node)
}
