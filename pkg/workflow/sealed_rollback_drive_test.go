package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// M23 SEAL-06 — THE ROLLBACK ARM, which the (*DAG).Execute check does not cover.
//
// This is the hole that made a second check REQUIRED rather than a nicety, and it was
// found by executing the path rather than by reading it. executeLocked's
// data.IsRollingBack() branch returns into finishRollback, which walks w.dag.nodes /
// GetLevels() directly and invokes CONSUMER COMPENSATIONS via n.compensation.Execute.
// DAG.Execute is never called on that arm, so a token checked only there is absent from
// exactly the path that runs consumer code on an uninspected graph.
//
// The live scenario is resume-into-rollback: a crash after the rolling_back marker is
// durable but before the final Save (ph48), reachable through Tick, DeliverAndResume and
// dispatch reclaim alike.
//
// WHY THIS TEST SEEDS THE MARKER IN THE STORE RATHER THAN DRIVING A REAL CRASH. The
// property under test is "the rollback arm refuses an unbuilt graph", not "a crash
// produces a rollback marker" — the latter is ph48's and is covered there. Seeding the
// durable state IS the resume: executeLocked loads it and takes the branch, which is what
// a real resume does. (This is the store-seed technique the project already uses for
// crash-window tests, because an in-process crash cannot leave a node non-terminal.)
func TestSealed_RollbackArmRefusesUnbuiltDAG(t *testing.T) {
	const wfID = "seal06-rollback-arm"

	// A hand-built DAG carrying a compensation, deliberately WITHOUT the token — this is
	// what a consumer's DAGFactory hands the dispatch path, or what an embedded/zero-value
	// DAG amounts to.
	compensationRan := 0
	dag := newDAG(wfID) // NOT newDAGForTest: unstamped is the whole point
	n := newNode("a", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	n.compensation = ActionFunc(func(context.Context, *WorkflowData) error {
		compensationRan++
		return nil
	})
	require.NoError(t, dag.addNode(n))

	// Seed the durable state a crash-mid-rollback leaves behind: the marker set, and the
	// node Completed so the rollback drive has something compensable to walk.
	store := NewInMemoryStore()
	data := NewWorkflowData(wfID)
	data.SetNodeStatus("a", Completed)
	data.SetRollingBack(true)
	require.NoError(t, store.Save(data))

	w := &Workflow{dag: dag, WorkflowID: wfID, Store: store}
	err := w.Execute(context.Background())

	require.ErrorIs(t, err, ErrDAGNotBuilt,
		"the rollback arm must refuse an unbuilt DAG. This path never reaches DAG.Execute — "+
			"finishRollback walks w.dag.nodes directly — so the (*DAG).Execute token check "+
			"cannot see it, which is why executeLocked carries its own.")

	// THE ASSERTION THAT MAKES IT NON-VACUOUS. A refusal that still ran the consumer's
	// compensation would have refused nothing that mattered: the effect is the point, not
	// the error value.
	require.Equal(t, 0, compensationRan,
		"no consumer compensation may run on a graph the gate never inspected (got %d invocations)",
		compensationRan)
}

// The converse arm, without which the test above proves only that something errored: the
// SAME fixture built through the builder must reach the rollback drive and actually
// compensate. Otherwise a refusal of everything would pass the test above.
func TestSealed_RollbackArmStillRunsForABuiltDAG(t *testing.T) {
	const wfID = "seal06-rollback-arm-ok"

	compensationRan := 0
	b := NewWorkflowBuilder().WithWorkflowID(wfID)
	b.AddNode("a").
		WithActionFunc(func(context.Context, *WorkflowData) error { return nil }).
		WithCompensationFunc(func(context.Context, *WorkflowData) error {
			compensationRan++
			return nil
		})
	dag, err := b.Build()
	require.NoError(t, err)

	store := NewInMemoryStore()
	data := NewWorkflowData(wfID)
	data.SetNodeStatus("a", Completed)
	data.SetRollingBack(true)
	require.NoError(t, store.Save(data))

	w := &Workflow{dag: dag, WorkflowID: wfID, Store: store}
	execErr := w.Execute(context.Background())

	require.NotErrorIs(t, execErr, ErrDAGNotBuilt,
		"a built DAG must not be refused on the rollback arm — the token must gate provenance, "+
			"not disable the rollback drive")
	require.Equal(t, 1, compensationRan,
		"the built fixture must actually reach the compensation, or the refusal arm above is "+
			"passing for the wrong reason (got %d)", compensationRan)
}
