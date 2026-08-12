package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// THE PHASE'S HEADLINE CLAIM, driven through the production path.
//
// The M17 finding is not "a token can be checked". It is that
// runNext (workflow_dispatch.go) builds a Workflow around whatever a consumer's
// DAGFactory hands back and drives it, validating NOTHING. So this test goes through RunNext —
// real SQLiteStore, real work queue, real claim, real drive — rather than calling the
// token check directly. A unit-level assertion that (*DAG).Execute refuses an unstamped
// graph would pass even if the dispatch path never reached that check.
//
// # The factory returns a POPULATED graph, and that is the whole design of this test
//
// The refusal predicate is PROVENANCE — built == false — not emptiness. If the fixture
// were an empty &DAG{}, a passing test would be equally consistent with an engine that
// merely refuses zero-node graphs, which is NOT the property and would be a much weaker
// one. So the factory below hand-assembles a graph with a real node and a real action
// via newDAG/addNode: it is well-formed, it is non-empty, it would execute perfectly
// well, and it is refused anyway — because it never went through build().
//
// The in-package constructors are the right fixture even though T6 just sealed them:
// before T6 this is EXACTLY the shape an external consumer's factory could return, and
// after T6 it is the shape any in-package caller can still produce. Sealing changed who
// can write this factory; it did not make the graph it returns acceptable, and the
// second claim is the one that matters.
//
// # Bite — and it corrected what this comment first claimed
//
// The first draft said "removing the token guard from (*DAG).Execute turns this GREEN".
// IT DOES NOT, and the measurement is more interesting than the guess:
//
//	guard removed                      this test
//	(*DAG).Execute only                PASSES
//	(*Workflow).executeLocked only     PASSES
//	BOTH                               REDS
//
// On the dispatch path the two token checks are MUTUALLY REDUNDANT — either one alone
// closes it — so this test proves THE PATH IS SHUT without discriminating which check
// shuts it. That is worth stating plainly, because a single-guard bite passing here
// looks exactly like a dead check and is not one.
//
// Neither is dead, and the second check was verified rather than assumed: seeding ONLY
// the executeLocked guard reds TestSealed_RollbackArmRefusesUnbuiltDAG with "the
// rollback arm must refuse an unbuilt DAG. This path never reaches DAG.Execute —
// finishRollback walks the nodes directly". So the checks are belt-and-braces HERE and
// genuinely disjoint THERE, which is exactly the two-mediation-point design T6 part 1
// landed. This file is evidence for the path; that file is evidence for the split.
//
// Under BOTH removed, the hand-built graph runs to completion and the work item
// terminalizes as a SUCCESS — the pre-T6 behaviour, and the finding restated: a silent,
// durably-recorded success on a graph the engine never inspected.
//
// The positive control below is not decoration: every assertion here is also satisfied
// by an engine that refuses EVERYTHING on the dispatch path.
func TestDispatch_UnvalidatedDAGFromFactoryIsRefused(t *testing.T) {
	store := mkDispatchStore(t)

	ran := false
	reg := NewRegistry()
	require.NoError(t, reg.Register("handRolled", func() (*DAG, error) {
		// A consumer factory that assembles its graph directly instead of through
		// WorkflowBuilder — well-formed, non-empty, and never stamped.
		d := newDAG("hand-rolled")
		require.NoError(t, d.addNode(newNode("work", ActionFunc(
			func(context.Context, *WorkflowData) error {
				ran = true
				return nil
			}))))
		return d, nil
	}))

	_, err := store.Enqueue("wf-unvalidated", "handRolled", nil)
	require.NoError(t, err)

	_, err = RunNext(context.Background(), store, reg, "worker-1")

	require.ErrorIs(t, err, ErrDAGNotBuilt,
		"the dispatch path must refuse the factory's graph with the NAMED sentinel")

	require.False(t, ran,
		"THE LOAD-BEARING ASSERTION: the consumer's action must never have been invoked. "+
			"A refusal that still ran the graph's work would close nothing — the side "+
			"effects are precisely what an unvalidated graph must not be allowed to perform")

	// BOTH, and I had predicted only the second. The first draft of this test asserted
	// require.NoError here, on the assumption that a refused drive would be absorbed into
	// the row's disposition the way an ordinary action failure is. It is not: RunNext
	// PROPAGATES the refusal to the worker loop AND terminalizes the row. Recorded as
	// measured, because the difference matters operationally — a worker sees this one
	// rather than silently logging a failed item — and because assuming it would have
	// shipped a comment that was wrong about the engine.
	require.Equal(t, wqFailed, wqState(t, store, "wf-unvalidated"),
		"and the row must TERMINALIZE, not sit claimed: a refusal that stranded the item "+
			"in `claimed` would trade a vacuous success for a stuck queue")
}

// The positive control, and it is not decoration. Every assertion above is satisfied by
// an engine that refuses EVERYTHING on the dispatch path — a factory whose graph is
// rejected unconditionally would leave `ran` false and the row failed just as happily.
// This proves the same store, the same registry shape and the same RunNext call DO drive
// a workflow to completion when the factory uses the sanctioned builder, so the refusal
// above is attributable to provenance and to nothing else.
func TestDispatch_BuiltDAGFromFactoryRuns(t *testing.T) {
	store := mkDispatchStore(t)

	ran := false
	reg := NewRegistry()
	require.NoError(t, reg.Register("built", func() (*DAG, error) {
		b := NewWorkflowBuilder().WithWorkflowID("built")
		b.AddStartNode("work").WithAction(ActionFunc(
			func(context.Context, *WorkflowData) error {
				ran = true
				return nil
			}))
		return b.Build()
	}))

	_, err := store.Enqueue("wf-built", "built", nil)
	require.NoError(t, err)

	_, err = RunNext(context.Background(), store, reg, "worker-1")
	require.NoError(t, err)

	require.True(t, ran, "a builder-produced graph must actually run on the dispatch path")
	require.Equal(t, wqDone, wqState(t, store, "wf-built"),
		"and terminalize as completed — the contrast that makes the refusal above meaningful")
}
