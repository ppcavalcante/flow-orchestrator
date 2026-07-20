package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// M19 ph95 — Slice C: the static type-cycle check + the child-failure↔parent-compensation boundary.

// TestSubWorkflowTypeCycle_Detected (test 4) — registered types forming a declarable spawn cycle
// (A queues B queues A) are rejected by ValidateNoTypeCycles at BUILD with ErrSubWorkflowTypeCycle.
// A DAG (acyclic type graph) passes.
func TestSubWorkflowTypeCycle_Detected(t *testing.T) {
	// Acyclic: A -> B -> (leaf). No cycle → nil.
	acyclic := NewRegistry()
	require.NoError(t, acyclic.Register("A", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("sub", "B")
		return b.Build()
	}))
	require.NoError(t, acyclic.Register("B", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddStartNode("leaf").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		return b.Build()
	}))
	require.NoError(t, acyclic.ValidateNoTypeCycles(), "an acyclic type graph passes")

	// Cyclic: A -> B -> A. ValidateNoTypeCycles rejects.
	cyclic := NewRegistry()
	require.NoError(t, cyclic.Register("A", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("sub", "B")
		return b.Build()
	}))
	require.NoError(t, cyclic.Register("B", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("sub", "A")
		return b.Build()
	}))
	require.ErrorIs(t, cyclic.ValidateNoTypeCycles(), ErrSubWorkflowTypeCycle, "a declarable A->B->A cycle is rejected at build")

	// Self-cycle: A -> A. Also rejected.
	self := NewRegistry()
	require.NoError(t, self.Register("A", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("sub", "A")
		return b.Build()
	}))
	require.ErrorIs(t, self.ValidateNoTypeCycles(), ErrSubWorkflowTypeCycle, "a self-referential type is rejected")
}

// TestSubWorkflowCompensationBoundary (test 5, ⭐) — a failed child fails the parent node (INV-01), the
// parent's M12 reverse-topo compensation runs over the PARENT's completed compensable nodes ONLY, and
// NEVER reaches into the child's journal (the child owns its own saga; distinct workflows, one-writer).
// Asserts BOTH sides: (a) the parent's own compensable node IS Compensated (not a vacuous no-op
// rollback), (b) the child journal's nodes are NOT Compensated (the boundary held).
func TestSubWorkflowCompensationBoundary(t *testing.T) {
	store := NewInMemoryStore()

	// The child DAG: a "work" node that SUCCEEDS (so the child has a completed node in its journal) then a
	// "boom" node that FAILS → the child run fails → the parent sub-workflow node fails (INV-01).
	childDAG := func() *DAG {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("work").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("did", "work")
			return nil
		}))
		cb.AddNode("boom").DependsOn("work").WithAction(ActionFunc(func(context.Context, *WorkflowData) error {
			return errors.New("child boom")
		}))
		d, err := cb.Build()
		require.NoError(t, err)
		return d
	}()

	// The parent: a compensable "step" (succeeds) → the sub-workflow node "sub" (its child fails). When
	// "sub" fails, the parent rolls back → "step" is compensated.
	compensatedFlag := false
	pb := NewWorkflowBuilder().WithWorkflowID("comp-parent")
	pb.AddStartNode("step").
		WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error { d.Set("step", "ok"); return nil })).
		WithCompensation(func(context.Context, *WorkflowData) error { compensatedFlag = true; return nil })
	pb.AddSubWorkflow("sub", childDAG).DependsOn("step")
	pdag, err := pb.Build()
	require.NoError(t, err)

	pw := NewWorkflow(store)
	pw.WorkflowID = "comp-parent"
	pw.DAG = pdag
	perr := pw.Execute(context.Background())
	require.Error(t, perr, "the child failed → the parent node fails → the run fails and rolls back")

	// (a) the parent's own compensable node WAS compensated — proves the rollback actually ran (non-vacuous).
	require.True(t, compensatedFlag, "the parent's compensable 'step' node was compensated (rollback ran)")
	parentFinal, err := store.Load("comp-parent")
	require.NoError(t, err)
	stepStatus, _ := parentFinal.GetNodeStatus("step")
	require.Equal(t, Compensated, stepStatus, "the parent 'step' node is Compensated")

	// (b) THE BOUNDARY: the child's journal nodes are NOT Compensated — the parent's compensation drive
	// iterates the PARENT's DAG.Nodes only (saga_rollback.go), so it structurally cannot reach the child.
	childID := subWorkflowChildID("comp-parent", "sub")
	childFinal, err := store.Load(childID)
	require.NoError(t, err)
	for node, st := range childFinal.GetAllNodeStatuses() {
		require.NotEqual(t, Compensated, st,
			"child node %q must NOT be Compensated — the parent's compensation never crosses into the child's saga", node)
	}
}
