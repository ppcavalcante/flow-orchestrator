package workflow

// Chunk 6 (DEC-CHUNK6) — cancellation × continue-on-error semantics.
//
// LOCKED CONTRACT (FORK 1 = a "cancellation wins"; FORK 2 = no Cancelled status):
//  1. Cancellation ALWAYS wins. If ctx.Err() != nil at the point Execute would
//     return, Execute returns the wrapped ctx error (errors.Is reaches
//     context.Canceled / context.DeadlineExceeded), regardless of WHERE the
//     cancel landed and regardless of whether a genuine node failure coexisted.
//     A cancelled run NEVER returns an *ExecutionError; incidental cancel-induced
//     NodeErrors are dropped. Genuine failures stay observable via node status.
//  2. No Cancelled status — in-flight node status stays whatever node.Execute
//     wrote (Failed if its action returned the ctx error; Completed if it raced
//     to finish first).
//  3. No Skip sweep on cancel — unreached AND downstream nodes stay Pending
//     ("stopped before reaching me", not "an upstream you needed failed").
//
// These are example-based tests; the random-DAG cancellation property lives in
// TestCancellationProperty below.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelOnRunAction cancels the supplied context the first time it runs, then
// returns nil (it does NOT itself error — the cancellation is the only event).
func cancelOnRunAction(cancel context.CancelFunc) Action {
	return ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		cancel()
		return nil
	})
}

// ctxObservingAction blocks until the context is cancelled, then returns the
// ctx error (a well-behaved action). If never cancelled it returns nil after
// observing one poll — used to model an in-flight node that sees cancellation.
func ctxObservingAction() Action {
	return ActionFunc(func(ctx context.Context, _ *WorkflowData) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

// TestCancel_MidLevelReturnsCtxErrorNotExecutionError: a node cancels the ctx
// and a sibling in the SAME level observes the cancel and returns ctx.Err().
// Per C1, Execute returns the wrapped ctx error (errors.Is context.Canceled),
// NOT an *ExecutionError — even though the observing sibling ended Failed.
func TestCancel_MidLevelReturnsCtxErrorNotExecutionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dag := NewDAG("mid-level-cancel")
	// Two independent start nodes (same level): one cancels, one observes.
	canceller := NewNode("canceller", cancelOnRunAction(cancel))
	observer := NewNode("observer", ctxObservingAction())
	mustAddNode(t, dag, canceller)
	mustAddNode(t, dag, observer)

	err := dag.Execute(ctx, NewWorkflowData("d"))

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"a cancelled run must return the wrapped ctx error")
	var execErr *ExecutionError
	assert.False(t, errors.As(err, &execErr),
		"a cancelled run must NOT return an *ExecutionError (got %T: %v)", err, err)
}

// TestCancel_WinsOverGenuineFailure: a genuine (non-ctx) failure coexists with a
// cancellation in the same level. Per FORK 1 = (a), cancellation WINS: Execute
// returns the ctx error, not an ExecutionError carrying the genuine failure. The
// genuine failure stays observable via GetNodeStatus==Failed.
func TestCancel_WinsOverGenuineFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var failerRan atomic.Bool
	dag := NewDAG("cancel-vs-failure")
	// Three independent nodes in one level: a genuine failer, a canceller, and a
	// ctx-observer. The failer fails on its own; the canceller cancels; the
	// observer blocks on ctx so the run cannot complete before the cancel lands.
	failer := NewNode("failer", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		failerRan.Store(true)
		return errors.New("genuine action failure")
	}))
	canceller := NewNode("canceller", cancelOnRunAction(cancel))
	observer := NewNode("observer", ctxObservingAction())
	mustAddNode(t, dag, failer)
	mustAddNode(t, dag, canceller)
	mustAddNode(t, dag, observer)

	data := NewWorkflowData("d")
	err := dag.Execute(ctx, data)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"cancellation must win even when a genuine failure coexisted")
	var execErr *ExecutionError
	assert.False(t, errors.As(err, &execErr),
		"a cancelled run must NOT return an *ExecutionError even with a coexisting genuine failure")

	// The genuine failure is still observable via status (not hidden, just not in
	// the error return).
	require.True(t, failerRan.Load(), "precondition: the genuine failer must have run")
	if st, _ := data.GetNodeStatus("failer"); st != Failed {
		t.Errorf("genuine failer status = %v, want Failed (observable via status)", st)
	}
}

// TestCancel_NoNodeSkippedDueToCancel: downstream of a node that ended Failed
// ONLY because it observed the cancellation must NOT be marked Skipped — it
// stays Pending. Per C3 the Skip sweep is suppressed on the cancel path.
func TestCancel_NoNodeSkippedDueToCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dag := NewDAG("no-skip-on-cancel")
	// L0: canceller (cancels) + observer (blocks on ctx → Failed via ctx err).
	// L1: downstream depends on observer.
	canceller := NewNode("canceller", cancelOnRunAction(cancel))
	observer := NewNode("observer", ctxObservingAction())
	downstream := NewNode("downstream", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return nil
	}))
	mustAddNode(t, dag, canceller)
	mustAddNode(t, dag, observer)
	mustAddNode(t, dag, downstream)
	mustAddDep(t, dag, "observer", "downstream") // downstream depends on observer

	data := NewWorkflowData("d")
	err := dag.Execute(ctx, data)

	require.ErrorIs(t, err, context.Canceled)
	// downstream must be Pending, NOT Skipped — the run stopped (cancelled) before
	// reaching it; its upstream "failed" only because of the cancel, which is not a
	// genuine skip cause.
	if st, _ := data.GetNodeStatus("downstream"); st != Pending {
		t.Errorf("downstream status = %v, want Pending (no Skip sweep on cancel)", st)
	}
}

// TestCancel_PureCoeLevelStillReturnsCtxError: in a continue-on-error run where
// no genuine fail-fast failure occurs, a cancellation still surfaces as the ctx
// error (the cancel path must catch it even though the coe path returns no
// fail-fast failures).
func TestCancel_PureCoeLevelStillReturnsCtxError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dag := NewDAG("coe-cancel")
	canceller := NewNode("canceller", cancelOnRunAction(cancel)).WithContinueOnError()
	observer := NewNode("observer", ctxObservingAction()).WithContinueOnError()
	mustAddNode(t, dag, canceller)
	mustAddNode(t, dag, observer)

	err := dag.Execute(ctx, NewWorkflowData("d"))

	require.Error(t, err, "a cancelled coe run must still surface the cancellation")
	assert.ErrorIs(t, err, context.Canceled)
}

// TestCancellationProperty is the Layer-1 random-DAG property for DEC-CHUNK6.
// Over random acyclic DAGs (reusing the invariant suite's generator) it injects
// a cancellation two ways — a PRE-cancelled context (the between-levels guard
// path) and a cancel triggered by a node DURING the run (the mid-level path) —
// and asserts the locked contract holds for EVERY generated DAG:
//
//	(1) Execute returns an error that errors.Is(context.Canceled) — the cancel
//	    is always surfaced;
//	(2) Execute does NOT return an *ExecutionError on a cancelled run
//	    (cancellation wins — FORK 1 = a);
//	(3) NO node ends Skipped — the Skip sweep is suppressed on the cancel path
//	    (downstream/unreached stay Pending, DEC-CHUNK3 distinction preserved).
//
// Mutation-bite (verified manually, see chunk-6 SUMMARY):
//   - revert the cancel-wins check (return the *ExecutionError on cancel) →
//     (2) fails (the mid-level variant returns *ExecutionError);
//   - re-enable markSkippedFrom on the cancel path → (3) fails (a downstream
//     node appears Skipped on a cancelled run).
func TestCancellationProperty(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 300
	params.MaxShrinkCount = 100
	properties := gopter.NewProperties(params)

	// checkCancelInvariant runs `dag` under `ctx` and asserts the three-part
	// cancel contract over the n nodes (named invNodeName(0..n-1)). Returns true
	// iff the contract holds.
	checkCancelInvariant := func(dag *DAG, ctx context.Context, data *WorkflowData, n int) bool {
		err := dag.Execute(ctx, data)
		// (1) cancellation is surfaced.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		// (2) no *ExecutionError on a cancelled run.
		var execErr *ExecutionError
		if errors.As(err, &execErr) {
			return false
		}
		// (3) no node ends Skipped due to the cancellation.
		for i := 0; i < n; i++ {
			if st, ok := data.GetNodeStatus(invNodeName(i)); ok && st == Skipped {
				return false
			}
		}
		return true
	}

	// Variant A — PRE-cancelled context. Every action observes ctx (returns its
	// error promptly) so a misbehaving action can't mask the cancel; but the
	// between-levels guard at level 0 fires first regardless. Holds for every DAG.
	// RED if: the between-levels guard is removed/weakened, or the cancel return
	// is replaced by an ExecutionError.
	properties.Property("pre-cancelled ctx -> ctx error, no ExecutionError, no Skipped", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			dag, ok := s.buildDAG(t, true, func(int) func(context.Context, *WorkflowData) error {
				return func(ctx context.Context, _ *WorkflowData) error { return ctx.Err() }
			})
			if !ok {
				return false
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // pre-cancelled
			return checkCancelInvariant(dag, ctx, NewWorkflowData("cancel-pre"), s.n)
		},
		dagSpecGens()...,
	))

	// Variant B — cancel DURING the run. One start node cancels the context when
	// it runs; every action blocks on ctx.Done() (so the run cannot complete
	// before the cancel propagates) and returns ctx.Err(). This exercises the
	// mid-level cancel path (the new ctx.Err() check after executeNodesInLevel)
	// across random DAGs. RED if: the mid-level cancel-wins check is reverted
	// (returns the *ExecutionError carrying the cancel-induced NodeErrors), or the
	// Skip sweep runs on the cancel path (a downstream node becomes Skipped).
	properties.Property("mid-run cancel -> ctx error, no ExecutionError, no Skipped", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Pick the lowest-index start node (deps empty) as the canceller; if
			// none (shouldn't happen — node 0 always has empty deps), fall back to 0.
			cancellerIdx := 0
			for i := 0; i < s.n; i++ {
				if len(s.deps[i]) == 0 {
					cancellerIdx = i
					break
				}
			}
			dag, ok := s.buildDAG(t, true, func(i int) func(context.Context, *WorkflowData) error {
				if i == cancellerIdx {
					return func(_ context.Context, _ *WorkflowData) error {
						cancel()
						return nil
					}
				}
				return func(ctx context.Context, _ *WorkflowData) error {
					<-ctx.Done()
					return ctx.Err()
				}
			})
			if !ok {
				return false
			}
			return checkCancelInvariant(dag, ctx, NewWorkflowData("cancel-mid"), s.n)
		},
		dagSpecGens()...,
	))

	properties.TestingRun(t)
}
