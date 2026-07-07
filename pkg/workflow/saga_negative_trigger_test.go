package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// compNode builds a Completed-capable node that records its compensation into rec.
func compNode(t *testing.T, name string, rec *compRecorder) *Node {
	t.Helper()
	n := NewNode(name, benchNoopAction())
	n.Compensation = ActionFunc(func(context.Context, *WorkflowData) error { rec.record(name); return nil })
	return n
}

// The M12 trigger fires ONLY on a hard *ExecutionError or a caller-cancel — never on
// "Execute returned non-nil". Each bite below wires a saga (so the trigger COULD
// fire) then a NON-triggering error, and asserts NO rollback. Each flips RED if the
// trigger predicate is widened to "err != nil" (or the ErrSuspended short-circuit is
// removed): rolling_back would be set and the compensation would run.

// TestSagaNeg_ErrSuspended_NoRollback — a run that PARKS (returns ErrSuspended,
// intending to resume) must not roll back.
func TestSagaNeg_ErrSuspended_NoRollback(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore() // a Checkpointer, so the park flushes + returns ErrSuspended
	const id = "saga-neg-suspend"
	d := NewDAG(id)
	gate := newParkGate()
	mustAddNode(t, d, compNode(t, "a", rec))
	park := newSuspendingNode("park", gate)
	mustAddNode(t, d, park)
	require.NoError(t, d.AddDependency("a", "park"))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())
	require.ErrorIs(t, err, ErrSuspended, "the run parks (intends to resume), not fails")

	got, lerr := store.Load(id)
	require.NoError(t, lerr)
	require.False(t, got.IsRollingBack(), "a parked run MUST NOT trigger rollback")
	require.Empty(t, rec.snapshot(), "no compensation runs on a park")
	assertStatus(t, got, "a", Completed) // Completed, NOT Compensated
}

// TestSagaNeg_FlushError_NoRollback — a checkpoint-flush error (a Save/IO error, not
// an *ExecutionError) must not roll back.
func TestSagaNeg_FlushError_NoRollback(t *testing.T) {
	rec := &compRecorder{}
	store := &flushErrorCheckpointer{InMemoryStore: NewInMemoryStore()}
	const id = "saga-neg-flush"
	d := NewDAG(id)
	gate := newParkGate()
	mustAddNode(t, d, compNode(t, "a", rec))
	mustAddNode(t, d, newSuspendingNode("park", gate))
	require.NoError(t, d.AddDependency("a", "park"))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrSuspended), "a failed flush did not suspend")

	got, lerr := store.Load(id)
	require.NoError(t, lerr)
	require.False(t, got.IsRollingBack(), "a checkpoint-flush error MUST NOT trigger rollback (not an *ExecutionError)")
	require.Empty(t, rec.snapshot(), "no compensation runs on a flush error")
}

// TestSagaNeg_CorruptLoad_NoRollback — a corrupt/IO load error surfaces BEFORE
// DAG.Execute, so the trigger is never reached: no rollback, nothing persisted over
// the corrupt state.
func TestSagaNeg_CorruptLoad_NoRollback(t *testing.T) {
	store := &errLoadStore{loadErr: ErrCorruptData}
	const id = "saga-neg-corrupt"
	d := NewDAG(id)
	rec := &compRecorder{}
	mustAddNode(t, d, compNode(t, "a", rec))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())
	require.ErrorIs(t, err, ErrCorruptData, "a corrupt load surfaces, not a rollback")
	require.Empty(t, rec.snapshot(), "no compensation runs on a corrupt load")
	require.Zero(t, store.saveCalls.Load(), "a corrupt load must not persist anything (no rollback marker save)")
}

// TestSaga_ExcludesNonCompleted — SAGA-04 / DEC-M12-SUSPEND: a rollback compensates
// ONLY Completed compensable nodes; Bypassed / Skipped / never-run nodes are never
// compensated even when they declare a compensation.
func TestSaga_ExcludesNonCompleted(t *testing.T) {
	rec := &compRecorder{}
	data := NewWorkflowData("exclude")
	data.SetNodeStatus("done", Completed)
	data.SetNodeStatus("bypass", Bypassed)
	data.SetNodeStatus("skip", Skipped)
	data.SetNodeStatus("pend", Pending) // never ran

	// A single level containing all four, each declaring a compensation.
	level := []*Node{
		compNode(t, "done", rec),
		compNode(t, "bypass", rec),
		compNode(t, "skip", rec),
		compNode(t, "pend", rec),
	}
	compensateLevel(context.Background(), level, data, 4, &sagaOutcome{})

	require.Equal(t, []string{"done"}, rec.snapshot(), "only the Completed node is compensated")
	assertStatus(t, data, "done", Compensated)
	assertStatus(t, data, "bypass", Bypassed) // untouched
	assertStatus(t, data, "skip", Skipped)    // untouched
	assertStatus(t, data, "pend", Pending)    // untouched
}
