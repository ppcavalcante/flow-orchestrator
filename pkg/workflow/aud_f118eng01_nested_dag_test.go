package workflow_test

import (
	"context"
	"errors"
	"testing"

	workflow "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// F118-ENG-01: a compiled *DAG satisfies the Action interface (DAG.Execute has the
// Action signature), so a consumer can smuggle a built graph into WithAction /
// WithCompensation. Doing so bypasses the sub-workflow machinery (depth bounds,
// gates, journaling) and recurses to a stack overflow. Build() must reject it and
// point the consumer at AddSubWorkflow*.

func buildChildDAG(t *testing.T) *workflow.DAG {
	t.Helper()
	b := workflow.NewWorkflowBuilder().WithWorkflowID("child")
	b.AddStartNode("child-start").
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	dag, err := b.Build()
	if err != nil {
		t.Fatalf("child build: %v", err)
	}
	return dag
}

func TestBoundary_WithActionRejectsNestedDAG(t *testing.T) {
	child := buildChildDAG(t)

	b := workflow.NewWorkflowBuilder().WithWorkflowID("parent")
	b.AddStartNode("parent-start").WithAction(child) // *DAG satisfies Action
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected Build to reject a compiled *DAG passed as a node action")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestBoundary_WithActionRejectsDAGInComposite(t *testing.T) {
	child := buildChildDAG(t)
	comp := workflow.NewCompositeAction(child) // wraps the *DAG

	b := workflow.NewWorkflowBuilder().WithWorkflowID("parent-composite")
	b.AddStartNode("pc-start").WithAction(comp)
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected Build to reject a *DAG wrapped in a CompositeAction")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestBoundary_MergeWithActionRejectsNestedDAG(t *testing.T) {
	child := buildChildDAG(t)

	b := workflow.NewWorkflowBuilder().WithWorkflowID("parent-merge")
	b.AddStartNode("seed").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	b.AddChoice("route").DependsOn("seed").
		When(func(*workflow.WorkflowData) bool { return true }, "a").
		Otherwise("b")
	b.AddNode("a").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	b.AddNode("b").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	b.AddMerge("done").From("a", "b").WithAction(child) // *DAG as merge user action
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected Build to reject a compiled *DAG passed as a merge user action")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestBoundary_WithCompensationRejectsNestedDAG(t *testing.T) {
	child := buildChildDAG(t)

	b := workflow.NewWorkflowBuilder().WithWorkflowID("parent-comp")
	b.AddStartNode("pcomp-start").
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil }).
		WithCompensation(child)
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected Build to reject a compiled *DAG passed as a compensation")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
