package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// compRecorder records the order in which compensations run (thread-safe — a level
// runs its compensations concurrently, but distinct levels run sequentially).
type compRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *compRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, name)
}

func (r *compRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// buildChainSaga builds a -> b -> c with compensations on a and b; c's action is
// governed by failC (fail => hard *ExecutionError => rollback trigger).
func buildChainSaga(t *testing.T, rec *compRecorder, failC bool, store WorkflowStore, id string) *Workflow {
	t.Helper()
	b := NewWorkflowBuilder()
	comp := func(name string) func(context.Context, *WorkflowData) error {
		return func(context.Context, *WorkflowData) error { rec.record(name); return nil }
	}
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(comp("a"))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(comp("b")).DependsOn("a")
	cAction := benchNoopAction()
	if failC {
		cAction = ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })
	}
	b.AddNode("c").WithAction(cAction).WithCompensation(comp("c")).DependsOn("b")
	dag, err := b.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store}
}

// TestSaga_HardFailTriggersReverseRollback — §4/§5: a hard failure rolls back the
// Completed compensable nodes in REVERSE-topological order; the failed node (never
// Completed) is not compensated.
func TestSaga_HardFailTriggersReverseRollback(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	w := buildChainSaga(t, rec, true /*failC*/, store, "saga-fail")

	err := w.Execute(context.Background())
	require.Error(t, err, "a hard failure must surface (the run failed, then rolled back)")

	got, err := store.Load("saga-fail")
	require.NoError(t, err)
	assertStatus(t, got, "a", Compensated)
	assertStatus(t, got, "b", Compensated)
	assertStatus(t, got, "c", Failed) // c failed -> never Completed -> not compensated
	require.True(t, got.IsRollingBack(), "the persisted run is marked rolling_back")

	// reverse-topological: b (level 1) compensated before a (level 0). c never runs
	// a compensation (it was Failed, not Completed).
	require.Equal(t, []string{"b", "a"}, rec.snapshot(), "compensations must run reverse-topologically, failed node excluded")
}

// TestSaga_ReverseTopologicalOrder_Diamond — §7 reverse-topo property over a diamond
// a -> {b, c} -> d -> fail. Every Completed compensable node is compensated EXACTLY
// once, and each node's compensation runs AFTER all its dependents' — the reverse-
// topological partial order (within a level {b,c} the order is unconstrained; across
// levels d precedes b,c precedes a). Bite: reverse the level loop in driveRollback to
// forward order -> the "dependent compensates before dependency" assertion fails.
func TestSaga_ReverseTopologicalOrder_Diamond(t *testing.T) {
	var mu sync.Mutex
	idx := map[string]int{}
	tick := 0
	comp := func(name string) func(context.Context, *WorkflowData) error {
		return func(context.Context, *WorkflowData) error {
			mu.Lock()
			defer mu.Unlock()
			idx[name] = tick
			tick++
			return nil
		}
	}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(comp("a"))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(comp("b")).DependsOn("a")
	b.AddNode("c").WithAction(benchNoopAction()).WithCompensation(comp("c")).DependsOn("a")
	b.AddNode("d").WithAction(benchNoopAction()).WithCompensation(comp("d")).DependsOn("b", "c")
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("d")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "saga-diamond", Store: store}

	require.Error(t, w.Execute(context.Background()))

	// exactly-once: all four Completed compensable nodes compensated.
	require.Len(t, idx, 4, "every Completed compensable node compensated exactly once")
	// reverse-topological partial order: a dependency compensates AFTER its dependents.
	require.Greater(t, idx["a"], idx["b"], "a (dependency) must compensate after b (dependent)")
	require.Greater(t, idx["a"], idx["c"], "a must compensate after c")
	require.Greater(t, idx["b"], idx["d"], "b must compensate after d (its dependent)")
	require.Greater(t, idx["c"], idx["d"], "c must compensate after d")
}

// TestSaga_ReEntrySwitch_CompensatesNotForward — §6 anti-vacuity discriminator: a
// run persisted as rolling_back, with a forward node's action wired to bump a
// counter, must on resume drive the ROLLBACK (compensations) and NOT re-run the
// forward action. A naive forward re-entry would re-run 'a' and advance the counter.
func TestSaga_ReEntrySwitch_CompensatesNotForward(t *testing.T) {
	var forwardHits int
	rec := &compRecorder{}
	store := NewInMemoryStore()

	b := NewWorkflowBuilder()
	b.AddNode("a").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { forwardHits++; return nil })).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil })
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "saga-reentry", Store: store}

	// Persist a rolling_back snapshot where 'a' already Completed (its forward effect
	// happened once, before the crash we are simulating).
	seed := NewWorkflowData("saga-reentry")
	seed.SetNodeStatus("a", Completed)
	seed.SetRollingBack(true)
	require.NoError(t, store.Save(seed))

	// ph48 never-nil floor: this synthetic rolling_back snapshot has no Failed node
	// (no journaled trigger cause), so a rolled-back run surfaces ErrRolledBack — never
	// nil (the switch still drives compensation, not forward; that is the point below).
	require.ErrorIs(t, w.Execute(context.Background()), ErrRolledBack, "resume into rollback is non-nil (never reports a rolled-back run as success)")

	require.Equal(t, 0, forwardHits, "resume into rollback MUST NOT re-run the forward action")
	require.Equal(t, []string{"a"}, rec.snapshot(), "resume must run the compensation")
	got, err := store.Load("saga-reentry")
	require.NoError(t, err)
	assertStatus(t, got, "a", Compensated)
}

// TestSaga_NonSagaFailure_Inert — moat: a failing DAG that declares NO compensation
// takes the pre-M12 path byte-for-byte — the rollback machinery is inert, so the
// persisted failed run carries NO rolling_back marker.
func TestSaga_NonSagaFailure_Inert(t *testing.T) {
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").WithAction(benchNoopAction())
	b.AddNode("boom").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "non-saga", Store: store}

	require.Error(t, w.Execute(context.Background()), "the run fails")
	got, err := store.Load("non-saga")
	require.NoError(t, err)
	require.False(t, got.IsRollingBack(), "a non-saga DAG must NOT engage the rollback machinery (byte-for-byte old path)")
	assertStatus(t, got, "a", Completed) // untouched: no compensation, no rollback
	assertStatus(t, got, "boom", Failed)
}

// TestSaga_CoeOnly_NoRollback — §4 negative: a continue-on-error-only run returns
// nil (success), so the trigger never fires — no rollback, no Compensated.
func TestSaga_CoeOnly_NoRollback(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder()
	b.AddNode("a").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("soft") })).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil }).
		WithContinueOnError()
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "saga-coe", Store: store}

	require.NoError(t, w.Execute(context.Background()), "a coe-only run succeeds (returns nil)")
	got, err := store.Load("saga-coe")
	require.NoError(t, err)
	require.False(t, got.IsRollingBack(), "a coe-only run must NOT trigger rollback")
	require.Empty(t, rec.snapshot(), "no compensation runs on a coe-only run")
	assertStatus(t, got, "a", Failed) // coe node is Failed, not Compensated
}
