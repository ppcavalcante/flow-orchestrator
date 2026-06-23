package workflow

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecute_CancelBetweenLevels covers C3 (commit 6afa338): the between-levels
// ctx.Err() guard at the top of DAG.Execute's level loop. The level barrier only
// bounds work WITHIN a level; without this guard the executor would keep launching
// every remaining level after the caller cancelled. This builds a two-level DAG
// (L0 = "first", L1 = "second" depending on first). The L0 action cancels the
// context as its last act; we then assert (a) the L1 node's action NEVER runs and
// (b) Execute returns an error that is errors.Is(context.Canceled) — the
// cancellation is surfaced, not swallowed by running to completion.
func TestExecute_CancelBetweenLevels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var secondRan atomic.Bool

	dag := NewDAG("cancel-between-levels")

	first := NewNode("first", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		// Cancel the workflow as the level-0 work completes. The next loop
		// iteration's ctx.Err() check must short-circuit before launching level 1.
		cancel()
		return nil
	}))
	second := NewNode("second", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		secondRan.Store(true)
		return nil
	}))

	mustAddNode(t, dag, first)
	mustAddNode(t, dag, second)
	// AddDependency(from, to) => `to` depends on `from`. "second" must depend on
	// "first" so first is level 0 (runs + cancels) and second is level 1 (the
	// level the between-levels ctx.Err() guard must prevent from launching).
	mustAddDep(t, dag, "first", "second")

	data := NewWorkflowData("cancel-between-levels")
	err := dag.Execute(ctx, data)

	require.Error(t, err, "cancellation between levels must surface as an error")
	assert.ErrorIs(t, err, context.Canceled,
		"Execute must return the wrapped ctx error (errors.Is(context.Canceled))")
	assert.False(t, secondRan.Load(),
		"level-1 node must NOT run once the ctx was cancelled after level 0")
}

// errLoadStore is a WorkflowStore stub whose Load returns a configurable error,
// used to drive Workflow.Execute's resume load-error branch (C4). Save records the
// number of saves so we can assert whether Execute fell through to a fresh run.
type errLoadStore struct {
	loadErr   error
	saveCalls atomic.Int32
}

func (s *errLoadStore) Save(_ *WorkflowData) error { s.saveCalls.Add(1); return nil }
func (s *errLoadStore) Load(_ string) (*WorkflowData, error) {
	return nil, s.loadErr
}
func (s *errLoadStore) ListWorkflows() ([]string, error) { return nil, nil }
func (s *errLoadStore) Delete(_ string) error            { return nil }

// TestExecute_LoadErrorPropagates covers C4 (commit fc6f47f): on resume, a
// non-ErrNotFound Store.Load error (e.g. ErrCorruptData from a malformed persisted
// payload) must be PROPAGATED, not swallowed. Swallowing it would silently start
// fresh and overwrite the persisted state on the next Save — losing it. We wire a
// store whose Load returns ErrCorruptData and a node that would run on a fresh
// start; we assert Execute returns the load error (errors.Is(ErrCorruptData)) and
// that the DAG body never executed (no fresh run) and nothing was saved over the
// corrupt state.
func TestExecute_LoadErrorPropagates(t *testing.T) {
	store := &errLoadStore{loadErr: ErrCorruptData}

	var bodyRan atomic.Bool
	dag := NewDAG("resume")
	node := NewNode("work", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		bodyRan.Store(true)
		return nil
	}))
	mustAddNode(t, dag, node)

	wf := &Workflow{DAG: dag, WorkflowID: "wf-corrupt", Store: store}
	err := wf.Execute(context.Background())

	require.Error(t, err, "a corrupt-resume load error must surface")
	assert.ErrorIs(t, err, ErrCorruptData,
		"Execute must propagate the Store.Load error (errors.Is(ErrCorruptData))")
	assert.False(t, bodyRan.Load(),
		"Execute must NOT fall through to a fresh DAG run when load fails")
	assert.Equal(t, int32(0), store.saveCalls.Load(),
		"Execute must NOT Save over the corrupt persisted state after a load failure")
}

// TestExecute_NotFoundStartsFresh pins the complementary contract: ErrNotFound is
// the expected "no prior state" case and must NOT propagate — Execute starts fresh.
// This guards against an over-eager C4 fix that would turn the normal first-run
// path into an error. The wrapped error satisfies errors.Is(ErrNotFound), matching
// the real store Load not-found path (fmt.Errorf("%w: %s", ErrNotFound, id)).
func TestExecute_NotFoundStartsFresh(t *testing.T) {
	store := &errLoadStore{loadErr: fmt.Errorf("%w: %s", ErrNotFound, "wf-fresh")}

	var bodyRan atomic.Bool
	dag := NewDAG("fresh")
	node := NewNode("work", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		bodyRan.Store(true)
		return nil
	}))
	mustAddNode(t, dag, node)

	wf := &Workflow{DAG: dag, WorkflowID: "wf-fresh", Store: store}
	err := wf.Execute(context.Background())

	require.NoError(t, err, "ErrNotFound must be treated as 'start fresh', not an error")
	assert.True(t, bodyRan.Load(), "fresh run must execute the DAG body")
	assert.Equal(t, int32(1), store.saveCalls.Load(), "fresh run must Save final state once")
}
