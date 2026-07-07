package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failSaveStore returns an error from Save once EVERY compensable node has reached a
// terminal compensation status — i.e. on the rollback's final-level checkpoint + the
// finishRollback authoritative save, while every FORWARD save (nodes still Completed)
// succeeds. Models a persist failure that strikes only at rollback-commit time, exercising
// the honesty-critical save-error return paths (finishRollback + the per-level checkpoint).
type failSaveStore struct {
	*InMemoryStore
	compensable []string
	mu          sync.Mutex
	failures    int
}

func (s *failSaveStore) Save(data *WorkflowData) error {
	allTerminal := true
	for _, n := range s.compensable {
		st, _ := data.GetNodeStatus(n)
		if st != Compensated && st != CompensationFailed {
			allTerminal = false
			break
		}
	}
	if allTerminal {
		s.mu.Lock()
		s.failures++
		s.mu.Unlock()
		return errors.New("save boom")
	}
	return s.InMemoryStore.Save(data)
}

func (s *failSaveStore) failed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

// okCompFn is a compensation that always succeeds.
func okCompFn(context.Context, *WorkflowData) error { return nil }

// buildTriggerSaga builds a->{b,c}->d->fail where a,b,c carry compensations `ca,cb,cc`,
// d has none (Completed→skipped), and `fail` hard-fails to trigger the rollback.
func buildTriggerSaga(t *testing.T, store WorkflowStore, id string,
	ca, cb, cc func(context.Context, *WorkflowData) error) *Workflow {
	t.Helper()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(ca)
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(cb).DependsOn("a")
	b.AddNode("c").WithAction(benchNoopAction()).WithCompensation(cc).DependsOn("a")
	b.AddNode("d").WithAction(benchNoopAction()).DependsOn("b", "c")
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("d")
	dag, err := b.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store}
}

// TestWithClock_SetsAndRestores — WithClock sets the clock, returns the workflow for
// chaining, and nil restores the default (nil field → systemClock resolved at drive time).
func TestWithClock_SetsAndRestores(t *testing.T) {
	w := &Workflow{}
	var c Clock = systemClock{}
	require.Same(t, w, w.WithClock(c), "returns the workflow for chaining")
	require.NotNil(t, w.Clock, "clock is set")
	require.Same(t, w, w.WithClock(nil))
	require.Nil(t, w.Clock, "nil restores the default")
}

// TestFinishRollback_CleanRollbackSaveError — a CLEAN rollback (every compensation
// succeeds) whose final persist FAILS must NOT report success: the returned error wraps
// BOTH the trigger cause and the save failure (workflow.go clean-rollback save path). A
// dropped save error here would silently lose the fact that the rolled-back state was
// never durably persisted.
func TestFinishRollback_CleanRollbackSaveError(t *testing.T) {
	store := &failSaveStore{InMemoryStore: NewInMemoryStore(), compensable: []string{"a", "b", "c"}}
	w := buildTriggerSaga(t, store, "clean-save-fail", okCompFn, okCompFn, okCompFn)

	err := w.Execute(context.Background())

	var se *SagaError
	require.False(t, errors.As(err, &se), "all comps succeeded → clean rollback, not a *SagaError")
	require.Error(t, err, "a rolled-back run whose save failed is never reported clean")
	require.ErrorContains(t, err, "save boom", "the save failure is surfaced")
	require.ErrorContains(t, err, "failed to save rollback state", "wrapped as a save error")
	require.Positive(t, store.failed(), "the final rollback save was actually attempted and failed")
}

// TestFinishRollback_PartialRollbackSaveError — a PARTIAL rollback (b's comp fails) whose
// final persist ALSO fails must STILL surface the *SagaError partition (the operator-
// critical {compensated ⊎ failed ⊎ skipped} survives) with the save error FOLDED into the
// cause, not replacing the partition (ph47-F2).
func TestFinishRollback_PartialRollbackSaveError(t *testing.T) {
	rec := &compRecorder{}
	store := &failSaveStore{InMemoryStore: NewInMemoryStore(), compensable: []string{"a", "b", "c"}}
	w := buildTriggerSaga(t, store, "partial-save-fail", okCompFn, failComp(rec, "b"), okCompFn)

	err := w.Execute(context.Background())

	var se *SagaError
	require.ErrorAs(t, err, &se, "a partial rollback still returns a *SagaError even when the save fails")
	require.Equal(t, []string{"a", "c"}, se.Compensated, "partition survives the save failure")
	require.Equal(t, []string{"b"}, failedNames(se.FailedToCompensate))
	require.Equal(t, []string{"d"}, se.Skipped)
	require.ErrorContains(t, se.Cause, "save boom", "the save error is folded into Cause, not dropped")
}

// TestDriveRollback_PerLevelCheckpointErrorSwallowed — a TRANSIENT per-level checkpoint
// save error during the reverse pass, when the final authoritative save SUCCEEDS, is NOT
// surfaced (review ph48-F3): the fully-persisted run reports its honest clean outcome. This
// exercises the driveRollback per-level save-error branch AND asserts it is correctly
// subsumed by a clean final save.
func TestDriveRollback_PerLevelCheckpointErrorSwallowed(t *testing.T) {
	// Fail Save ONLY on the first attempt that sees a compensated node (a mid-rollback
	// per-level checkpoint), succeed on every later save (incl. the final authoritative one).
	inner := NewInMemoryStore()
	store := &firstCompSaveFailStore{InMemoryStore: inner}
	w := buildTriggerSaga(t, store, "per-level-swallow", okCompFn, okCompFn, okCompFn)

	err := w.Execute(context.Background())

	var se *SagaError
	require.False(t, errors.As(err, &se), "a clean rollback with only a transient checkpoint error is not a *SagaError")
	require.True(t, store.hitTransient, "the per-level checkpoint save-error branch was exercised")
	// The final authoritative save succeeded → the durable state is the honest rolled-back one.
	got, lerr := inner.Load("per-level-swallow")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
}

// firstCompSaveFailStore fails Save exactly once — on the first save where any node is
// already Compensated (a mid-rollback per-level checkpoint) — then never again, so the
// final authoritative finishRollback save succeeds.
type firstCompSaveFailStore struct {
	*InMemoryStore
	hitTransient bool
}

func (s *firstCompSaveFailStore) Save(data *WorkflowData) error {
	if !s.hitTransient {
		for _, n := range []string{"a", "b", "c"} {
			if st, _ := data.GetNodeStatus(n); st == Compensated {
				s.hitTransient = true
				return errors.New("transient checkpoint boom")
			}
		}
	}
	return s.InMemoryStore.Save(data)
}

// TestRunCompensation_DeadlineExpiredAtStart — when the rollback deadline has ALREADY
// expired by the time a compensation attempt begins, the compensation is not run and the
// node is recorded CompensationFailed (the deadline can never falsely report success).
// Exercises the attempt-start ctx.Err() guard in runCompensationWithRetry.
func TestRunCompensation_DeadlineExpiredAtStart(t *testing.T) {
	store := NewInMemoryStore()
	ran := false
	w := buildTriggerSaga(t, store, "deadline-at-start",
		func(context.Context, *WorkflowData) error { ran = true; return nil },
		okCompFn, okCompFn)
	w.WithRollbackTimeout(1 * time.Nanosecond) // expired before the first compensation attempt

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case err := <-done:
		var se *SagaError
		require.ErrorAs(t, err, &se, "an expired-deadline rollback → *SagaError")
		require.NotEmpty(t, se.FailedToCompensate, "the un-run compensation is CompensationFailed, not silently skipped")
	case <-time.After(5 * time.Second):
		t.Fatal("rollback hung on an already-expired deadline")
	}
	require.False(t, ran, "the compensation did not run past the expired deadline")
}

// TestRunCompensation_DeadlineDuringBackoff — a compensation that fails and is being
// retried gets its inter-attempt backoff cut short by the rollback deadline, and the node
// is recorded CompensationFailed (the retry loop is bounded by the deadline, ph47-F1).
// Exercises the ctx.Done() branch of the backoff select.
func TestRunCompensation_DeadlineDuringBackoff(t *testing.T) {
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).
		WithRetries(3). // would retry with a 1s backoff between attempts…
		WithCompensation(func(context.Context, *WorkflowData) error {
			return errors.New("always fails") // …so it enters the backoff after attempt 0
		})
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "deadline-in-backoff", Store: store}
	w.WithRollbackTimeout(50 * time.Millisecond) // fires DURING the 1s backoff

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case err := <-done:
		var se *SagaError
		require.ErrorAs(t, err, &se)
		require.Equal(t, []string{"a"}, failedNames(se.FailedToCompensate), "the retried comp is bounded to CompensationFailed")
		require.Less(t, time.Since(start), 900*time.Millisecond, "the deadline cut the 1s backoff short (not waited out)")
	case <-time.After(5 * time.Second):
		t.Fatal("rollback hung in the retry backoff")
	}
}

// TestDriveRollback_MaxConcurrencyCoerced — a non-positive MaxConcurrency must not deadlock
// the rollback: driveRollback coerces it to the default (review ph46-F1). The saga still
// compensates and returns the honest outcome.
func TestDriveRollback_MaxConcurrencyCoerced(t *testing.T) {
	store := NewInMemoryStore()
	w := buildTriggerSaga(t, store, "maxconc-coerce", okCompFn, okCompFn, okCompFn)
	w.DAG.config.MaxConcurrency = 0 // non-positive → the coerce must save it from a deadlock

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case <-done:
		got, lerr := store.Load("maxconc-coerce")
		require.NoError(t, lerr)
		assertStatus(t, got, "a", Compensated) // rollback ran despite MaxConcurrency=0
	case <-time.After(5 * time.Second):
		t.Fatal("rollback deadlocked on MaxConcurrency=0 — the defensive coerce failed")
	}
}

// TestCompensateLevel_MaxConcurrencyCoerced — the compensateLevel defensive coerce directly
// (unreachable via driveRollback, which pre-coerces): a maxConc of 0 must not build an
// unbuffered semaphore and deadlock the first send (review ph46-F1).
func TestCompensateLevel_MaxConcurrencyCoerced(t *testing.T) {
	b := NewWorkflowBuilder()
	b.AddNode("x").WithAction(benchNoopAction()).WithCompensation(okCompFn)
	dag, err := b.Build()
	require.NoError(t, err)
	data := NewWorkflowData("cl")
	data.SetNodeStatus("x", Completed)
	out := &sagaOutcome{}

	fin := make(chan struct{})
	go func() {
		compensateLevel(context.Background(), dag.GetLevels()[0], data, 0, out) // maxConc=0 → coerce
		close(fin)
	}()
	select {
	case <-fin:
		require.Equal(t, []string{"x"}, out.compensated, "x compensated despite maxConc=0")
		st, _ := data.GetNodeStatus("x")
		require.Equal(t, Compensated, st)
	case <-time.After(5 * time.Second):
		t.Fatal("compensateLevel deadlocked on maxConc=0 — the defensive coerce failed")
	}
}

// TestReconstructOutcome_ResidualCompletedCompensableIsFailed — a Completed node that
// DECLARES a compensation but is still Completed (a defensive residual — a reverse level
// that a complete pass would have reached) is reported as FailedToCompensate, NEVER
// silently dropped from the honest partition.
func TestReconstructOutcome_ResidualCompletedCompensableIsFailed(t *testing.T) {
	b := NewWorkflowBuilder()
	b.AddNode("x").WithAction(benchNoopAction()).WithCompensation(okCompFn)
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "ro"}
	data := NewWorkflowData("ro")
	data.SetNodeStatus("x", Completed) // Completed + has a compensation, but un-compensated

	out := w.reconstructOutcome(data, &sagaOutcome{})

	require.Equal(t, []string{"x"}, failedNames(out.failedToCompensate),
		"a residual Completed-compensable node is surfaced as failed, never dropped from the partition")
	require.Empty(t, out.compensated)
	require.Empty(t, out.skipped)
}
