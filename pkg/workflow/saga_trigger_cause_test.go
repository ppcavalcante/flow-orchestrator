package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTriggerCause_RoundTrip_AllStores — §1: the journaled trigger cause round-trips
// all 3 stores; a never-rolled-back run is TriggerNone (omitted, backward-compatible).
func TestTriggerCause_RoundTrip_AllStores(t *testing.T) {
	for name, store := range makeAllStores(t) {
		t.Run(name, func(t *testing.T) {
			// a rolled-back run: cause round-trips.
			for _, tc := range []TriggerCause{TriggerFailure, TriggerCanceled, TriggerDeadlineExceeded} {
				id := "cause-" + name + "-" + string(rune('0'+tc))
				d := NewWorkflowData(id)
				d.SetRollingBack(true)
				d.SetTriggerCause(tc)
				require.NoError(t, store.Save(d))
				got, err := store.Load(id)
				require.NoError(t, err)
				require.Equal(t, tc, got.TriggerCause(), "%s: trigger cause %d must round-trip", name, tc)
			}
			// a non-saga run: TriggerNone (the default), byte-compatible.
			fwd := NewWorkflowData("cause-none-" + name)
			require.NoError(t, store.Save(fwd))
			gotFwd, err := store.Load("cause-none-" + name)
			require.NoError(t, err)
			require.Equal(t, TriggerNone, gotFwd.TriggerCause())
		})
	}
}

// buildCauseSaga: a (compensable) -> b. A resume-only DAG for the seeded-snapshot tests.
func buildCauseSaga(t *testing.T, store WorkflowStore, id string, rec *compRecorder) *Workflow {
	t.Helper()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil })
	b.AddNode("b").WithAction(benchNoopAction()).DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store}
}

// seedRollingBack persists a mid-rollback snapshot ACROSS the store boundary (the
// anti-vacuity requirement): a=Completed (undo pending), b=Failed (a mid-level cancel
// marks its in-flight node Failed — the spurious source inference would misread), plus
// rolling_back + the journaled cause. The live error is DROPPED — resume must recover
// the cause from durable state alone.
func seedRollingBack(t *testing.T, store WorkflowStore, id string, cause TriggerCause, withFailedNode bool) {
	t.Helper()
	seed := NewWorkflowData(id)
	seed.SetNodeStatus("a", Completed)
	if withFailedNode {
		seed.SetNodeStatus("b", Failed)
	}
	seed.SetRollingBack(true)
	seed.SetTriggerCause(cause)
	require.NoError(t, store.Save(seed))
}

// TestSagaCause_CanceledResume_MatchesCanceled — THE F2 bite (crux). A cancel-triggered
// rollback, persisted with rolling_back + triggerCause=canceled AND a Failed node (the
// mid-level-cancel witness), resumed from the STORE with the live error DROPPED, must
// report errors.Is(context.Canceled) TRUE — NOT a spurious node-failure. Bite: make
// reconstructCause ignore the journaled cause -> inference reads the Failed node ->
// *ExecutionError -> errors.Is(Canceled) flips FALSE -> RED. (A no-crash cancel would
// pass even broken — this test MUST cross the store/resume boundary, which it does.)
func TestSagaCause_CanceledResume_MatchesCanceled(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	seedRollingBack(t, store, "cause-cancel", TriggerCanceled, true /*withFailedNode*/)

	err := buildCauseSaga(t, store, "cause-cancel", rec).Execute(context.Background())

	require.Error(t, err, "a rolled-back run is never nil")
	require.True(t, errors.Is(err, context.Canceled),
		"resume MUST recover the cancel cause from durable state (F2), not infer a node-failure")
	require.False(t, errors.Is(err, ErrExecutionFailed),
		"the resumed cause is a cancel, not a node failure")
	require.Equal(t, []string{"a"}, rec.snapshot(), "the compensation still ran")
	got, lerr := store.Load("cause-cancel")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
}

// TestSagaCause_DeadlineResume_MatchesDeadline — the deadline variant of the F2 bite.
func TestSagaCause_DeadlineResume_MatchesDeadline(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	seedRollingBack(t, store, "cause-deadline", TriggerDeadlineExceeded, true)

	err := buildCauseSaga(t, store, "cause-deadline", rec).Execute(context.Background())
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"resume MUST recover the deadline cause from durable state, not infer a node-failure")
}

// TestSagaCause_FailureResume_Faithful — a failure-triggered resume still yields a
// faithful *ExecutionError (the discriminator only ROUTES; the Failed node is the witness).
func TestSagaCause_FailureResume_Faithful(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	seedRollingBack(t, store, "cause-failure", TriggerFailure, true /*Failed node = the real failure*/)

	err := buildCauseSaga(t, store, "cause-failure", rec).Execute(context.Background())
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "a failure-triggered resume reconstructs the *ExecutionError")
	require.False(t, errors.Is(err, context.Canceled), "a failure is not a cancel")
}

// TestSagaCause_NoneResume_Floor — a cause-less resume (TriggerNone, no Failed node, e.g.
// a pre-ph49 snapshot) still returns ErrRolledBack (never nil) via the floor.
func TestSagaCause_NoneResume_Floor(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	seedRollingBack(t, store, "cause-none", TriggerNone, false /*no Failed node*/)

	err := buildCauseSaga(t, store, "cause-none", rec).Execute(context.Background())
	require.ErrorIs(t, err, ErrRolledBack, "a cause-less rolled-back run returns the never-nil floor")
}
