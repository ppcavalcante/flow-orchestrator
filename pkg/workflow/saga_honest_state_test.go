package workflow

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failComp returns a compensation that always errors (records the attempt).
func failComp(rec *compRecorder, name string) func(context.Context, *WorkflowData) error {
	return func(context.Context, *WorkflowData) error { rec.record(name); return errors.New("comp boom: " + name) }
}

// buildHonestDiamond: a->{b,c}->d->fail. b's compensation FAILS, a & c succeed, d has
// NO compensation (Completed, nothing to undo). fail is the hard trigger (Failed).
// Completed set = {a,b,c,d}; expected partition: Compensated {a,c} ⊎ Failed {b} ⊎ Skipped {d}.
func buildHonestDiamond(t *testing.T, rec *compRecorder, store WorkflowStore, id string) *Workflow {
	t.Helper()
	okComp := func(name string) func(context.Context, *WorkflowData) error {
		return func(context.Context, *WorkflowData) error { rec.record(name); return nil }
	}
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(okComp("a"))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(failComp(rec, "b")).DependsOn("a")
	b.AddNode("c").WithAction(benchNoopAction()).WithCompensation(okComp("c")).DependsOn("a")
	b.AddNode("d").WithAction(benchNoopAction()).DependsOn("b", "c") // no compensation -> skipped
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("d")
	dag, err := b.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store}
}

func failedNames(nes []NodeError) []string {
	out := make([]string, len(nes))
	for i, ne := range nes {
		out[i] = ne.NodeName
	}
	sort.Strings(out)
	return out
}

// TestSagaHonest_PartitionExact — THE phase property. A partial rollback returns a
// *SagaError whose {Compensated ⊎ FailedToCompensate ⊎ Skipped} is an EXACT partition
// of the run's Completed nodes (union == completed set, pairwise disjoint), and every
// non-failing compensation still ran (best-effort). Bites: abort-on-first → "later
// comps ran" RED; drop-a-failed-node → exact-partition RED.
func TestSagaHonest_PartitionExact(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	w := buildHonestDiamond(t, rec, store, "honest-partition")

	err := w.Execute(context.Background())

	var sagaErr *SagaError
	require.ErrorAs(t, err, &sagaErr, "a partial rollback returns a *SagaError")
	// exact partition of the Completed set {a,b,c,d}.
	comp := append([]string(nil), sagaErr.Compensated...)
	sort.Strings(comp)
	require.Equal(t, []string{"a", "c"}, comp, "compensated = the succeeding comps")
	require.Equal(t, []string{"b"}, failedNames(sagaErr.FailedToCompensate), "failed = the failing comp")
	require.Equal(t, []string{"d"}, sagaErr.Skipped, "skipped = Completed-but-no-compensation")

	// union == the completed set, pairwise disjoint (no node missing / double-counted).
	union := map[string]int{}
	for _, n := range sagaErr.Compensated {
		union[n]++
	}
	for _, ne := range sagaErr.FailedToCompensate {
		union[ne.NodeName]++
	}
	for _, n := range sagaErr.Skipped {
		union[n]++
	}
	require.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1}, union,
		"exact partition: every Completed node in exactly one set")

	// best-effort: every non-failing compensation ran (a, c) AND the failing one was
	// attempted (b) — i.e. b's failure did not suppress a later compensation.
	ran := rec.snapshot()
	require.Contains(t, ran, "a")
	require.Contains(t, ran, "c")
	require.Contains(t, ran, "b")

	// durable: the failed node persists as CompensationFailed.
	got, lerr := store.Load("honest-partition")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
	assertStatus(t, got, "b", CompensationFailed)
	assertStatus(t, got, "c", Compensated)
	assertStatus(t, got, "d", Completed) // skipped by rollback, stays Completed
}

// TestSagaHonest_NeverFalseClean — a fully-clean rollback returns the ORIGINAL trigger
// error (errors.As reaches *ExecutionError), NOT a *SagaError. A partial rollback
// returns a *SagaError that STILL unwraps to the *ExecutionError cause.
func TestSagaHonest_NeverFalseClean(t *testing.T) {
	// clean: all comps succeed -> bare *ExecutionError, not a *SagaError.
	recClean := &compRecorder{}
	wClean := buildChainSaga(t, recClean, true, NewInMemoryStore(), "honest-clean")
	errClean := wClean.Execute(context.Background())
	var se *SagaError
	require.False(t, errors.As(errClean, &se), "a clean rollback must NOT return a *SagaError")
	var ee *ExecutionError
	require.ErrorAs(t, errClean, &ee, "a clean rollback returns the original *ExecutionError")

	// partial: a *SagaError that unwraps to the *ExecutionError cause (caller sees both).
	recPart := &compRecorder{}
	errPart := buildHonestDiamond(t, recPart, NewInMemoryStore(), "honest-partial").Execute(context.Background())
	require.ErrorAs(t, errPart, &se, "a partial rollback returns a *SagaError")
	require.ErrorAs(t, errPart, &ee, "the *SagaError still unwraps to the *ExecutionError cause")
}

// TestCompensationFailed_RoundTrip_AllStores — §1: the 9th status round-trips all 3
// stores and is terminal.
func TestCompensationFailed_RoundTrip_AllStores(t *testing.T) {
	require.True(t, isTerminalStatus(CompensationFailed), "CompensationFailed must be terminal")
	for name, store := range makeAllStores(t) {
		t.Run(name, func(t *testing.T) {
			id := "compfailed-" + name
			d := NewWorkflowData(id)
			d.SetNodeStatus("cf", CompensationFailed)
			d.SetNodeStatus("ok", Compensated)
			require.NoError(t, store.Save(d))
			got, err := store.Load(id)
			require.NoError(t, err)
			assertStatus(t, got, "cf", CompensationFailed)
			assertStatus(t, got, "ok", Compensated)
		})
	}
}

// TestSagaHonest_RetryCount — a transient compensation failure is retried per
// RetryCount then succeeds (Compensated); one that always fails is CompensationFailed
// after RetryCount+1 attempts.
func TestSagaHonest_RetryCount(t *testing.T) {
	var attempts int
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).
		WithRetries(3).
		WithCompensation(func(context.Context, *WorkflowData) error {
			attempts++
			if attempts < 3 { // fail twice, succeed on the 3rd attempt
				return errors.New("transient")
			}
			return nil
		})
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "honest-retry", Store: store}

	err = w.Execute(context.Background())
	var se *SagaError
	require.False(t, errors.As(err, &se), "the transient comp eventually succeeded -> clean rollback")
	require.Equal(t, 3, attempts, "compensation retried per RetryCount (3 attempts)")
	got, lerr := store.Load("honest-retry")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
}

// TestSagaHonest_BoundedDeadline — a compensation that blocks past the rollback
// deadline is recorded CompensationFailed and the rollback completes (a hung comp does
// not hang the run). Uses a tiny RollbackTimeout + a compensation that waits on ctx.
func TestSagaHonest_BoundedDeadline(t *testing.T) {
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).
		WithCompensation(func(ctx context.Context, _ *WorkflowData) error {
			<-ctx.Done() // block until the rollback deadline cancels the ctx
			return ctx.Err()
		})
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "honest-deadline", Store: store}
	w.WithRollbackTimeout(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case err := <-done:
		var se *SagaError
		require.ErrorAs(t, err, &se, "a hung compensation past the deadline -> *SagaError")
		require.Equal(t, []string{"a"}, failedNames(se.FailedToCompensate), "the blocked comp is CompensationFailed")
	case <-time.After(5 * time.Second):
		t.Fatal("rollback hung past its deadline — a blocking compensation hung the run")
	}
	got, lerr := store.Load("honest-deadline")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", CompensationFailed)
}

// TestSagaHonest_HungCompensationDoesNotHangRun — review ph47-F1: a compensation that
// IGNORES ctx and blocks forever must NOT hang the run. The goroutine-bounded Execute
// lets the rollback deadline fire, the node is recorded CompensationFailed, and Execute
// returns (the ignoring comp's goroutine leaks — a documented user-bug tradeoff).
func TestSagaHonest_HungCompensationDoesNotHangRun(t *testing.T) {
	block := make(chan struct{}) // never signalled; the comp ignores ctx and waits on it
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error {
			<-block // ignore ctx ENTIRELY — block forever
			return nil
		})
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "honest-hung", Store: store}
	w.WithRollbackTimeout(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case err := <-done:
		var se *SagaError
		require.ErrorAs(t, err, &se, "a ctx-ignoring hung comp still yields a *SagaError; the run completes")
		require.Equal(t, []string{"a"}, failedNames(se.FailedToCompensate), "the hung comp is CompensationFailed")
	case <-time.After(5 * time.Second):
		t.Fatal("ph47-F1 regression: a ctx-ignoring compensation hung the run past its deadline")
	}
	close(block) // release the abandoned goroutine so the test process stays clean
}

// TestSagaNeg_Validation_NoRollback — V46-N1 (qa carry): a validation error surfaces
// BEFORE DAG.Execute, so the trigger is never reached — no rollback.
func TestSagaNeg_Validation_NoRollback(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	d := NewDAG("honest-validation")
	// Build a CYCLE directly at the Node level (Node.AddDependency bypasses the DAG-
	// level cycle guard), so DAG.Validate() fails. Both nodes declare a compensation,
	// so the trigger COULD fire if a validation error were (wrongly) reached past the
	// pre-Execute return.
	a := compNode(t, "a", rec)
	b := compNode(t, "b", rec)
	a.AddDependency(b)
	b.AddDependency(a) // a <-> b cycle
	mustAddNode(t, d, a)
	mustAddNode(t, d, b)

	w := &Workflow{DAG: d, WorkflowID: "honest-validation", Store: store}
	err := w.Execute(context.Background())
	require.Error(t, err, "an invalid (cyclic) DAG fails validation")
	require.NotErrorAs(t, err, new(*SagaError), "a validation error must NOT trigger rollback")
	require.Empty(t, rec.snapshot(), "no compensation runs on a validation error")
}
