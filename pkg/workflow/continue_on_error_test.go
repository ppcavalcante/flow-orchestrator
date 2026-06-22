package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// failAction returns an action that always errors.
func failAction(msg string) func(context.Context, *WorkflowData) error {
	return func(_ context.Context, _ *WorkflowData) error {
		return errors.New(msg)
	}
}

// markAction returns an action that records that it ran (into ran) and succeeds.
func markAction(ran *atomic.Bool) func(context.Context, *WorkflowData) error {
	return func(_ context.Context, _ *WorkflowData) error {
		ran.Store(true)
		return nil
	}
}

// TestContinueOnError_FailedNodeDoesNotFailWorkflow: a continue-on-error node
// fails; its sibling AND its dependent still run; Execute returns nil; the
// failed node's status is Failed; the dependent can read that Failed status.
func TestContinueOnError_FailedNodeDoesNotFailWorkflow(t *testing.T) {
	var siblingRan, dependentRan atomic.Bool
	var dependentSawFailed atomic.Bool

	b := NewWorkflowBuilder().WithWorkflowID("coe")
	b.AddStartNode("flaky").
		WithAction(failAction("boom")).
		WithContinueOnError()
	b.AddStartNode("sibling").
		WithAction(markAction(&siblingRan))
	b.AddNode("downstream").
		DependsOn("flaky").
		WithAction(func(_ context.Context, d *WorkflowData) error {
			dependentRan.Store(true)
			if st, ok := d.GetNodeStatus("flaky"); ok && st == Failed {
				dependentSawFailed.Store(true)
			}
			return nil
		})

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("coe")

	if err := dag.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute returned %v, want nil (continue-on-error failure must not fail the workflow)", err)
	}

	if st, _ := data.GetNodeStatus("flaky"); st != Failed {
		t.Fatalf("flaky status = %q, want Failed", st)
	}
	if !siblingRan.Load() {
		t.Fatalf("sibling did not run; continue-on-error must not cancel siblings")
	}
	if !dependentRan.Load() {
		t.Fatalf("downstream did not run; dependents of a continue-on-error failure must still run")
	}
	if !dependentSawFailed.Load() {
		t.Fatalf("downstream could not read upstream Failed status")
	}
}

// TestContinueOnError_DistinctFromNormalFailedDep pins DEC-P21-depguard exactly:
// a dependent of a Failed CONTINUE-ON-ERROR dep RUNS; a dependent of a Failed
// NORMAL dep does NOT (fail-fast halts the workflow before the next level).
func TestContinueOnError_DistinctFromNormalFailedDep(t *testing.T) {
	t.Run("continue-on-error dep -> dependent runs", func(t *testing.T) {
		var dependentRan atomic.Bool
		b := NewWorkflowBuilder().WithWorkflowID("coe-dep")
		b.AddStartNode("up").WithAction(failAction("boom")).WithContinueOnError()
		b.AddNode("down").DependsOn("up").WithAction(markAction(&dependentRan))

		dag, err := b.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		data := NewWorkflowData("coe-dep")
		if err := dag.Execute(context.Background(), data); err != nil {
			t.Fatalf("Execute = %v, want nil", err)
		}
		if !dependentRan.Load() {
			t.Fatalf("dependent of continue-on-error Failed dep did not run")
		}
	})

	t.Run("normal dep -> dependent does NOT run", func(t *testing.T) {
		var dependentRan atomic.Bool
		b := NewWorkflowBuilder().WithWorkflowID("normal-dep")
		b.AddStartNode("up").WithAction(failAction("boom")) // no WithContinueOnError
		b.AddNode("down").DependsOn("up").WithAction(markAction(&dependentRan))

		dag, err := b.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		data := NewWorkflowData("normal-dep")
		if err := dag.Execute(context.Background(), data); err == nil {
			t.Fatalf("Execute = nil, want error (normal failure is fail-fast)")
		}
		if dependentRan.Load() {
			t.Fatalf("dependent of a normal Failed dep ran; fail-fast must halt before the next level")
		}
		if st, _ := data.GetNodeStatus("up"); st != Failed {
			t.Fatalf("up status = %q, want Failed", st)
		}
	})
}

// TestContinueOnError_MixedLevelNormalFailureWins: in a single level, one
// continue-on-error node fails AND one normal node fails. The normal failure
// must still halt the workflow (fail-fast wins): Execute errors.
func TestContinueOnError_MixedLevelNormalFailureWins(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("mixed")
	b.AddStartNode("soft").WithAction(failAction("soft-boom")).WithContinueOnError()
	b.AddStartNode("hard").WithAction(failAction("hard-boom")) // normal -> fail-fast

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("mixed")

	err = dag.Execute(context.Background(), data)
	if err == nil {
		t.Fatalf("Execute = nil, want error (a normal failure in the level must fail the workflow)")
	}
	// The reported error must be the hard node's failure, not the soft one.
	if got := err.Error(); !contains(got, "hard") {
		t.Fatalf("Execute error = %q, want it to reference the hard (normal) node failure", got)
	}
	// Both failing nodes are recorded Failed regardless.
	if st, _ := data.GetNodeStatus("hard"); st != Failed {
		t.Fatalf("hard status = %q, want Failed", st)
	}
	if st, _ := data.GetNodeStatus("soft"); st != Failed {
		t.Fatalf("soft status = %q, want Failed", st)
	}
}

// TestContinueOnError_DefaultUnchanged is the regression guard: a normal node
// failure with NO flag cancels siblings' observable progress where possible and
// halts the workflow (today's behavior). We assert Execute errors and the
// downstream node is not run.
func TestContinueOnError_DefaultUnchanged(t *testing.T) {
	var downstreamRan atomic.Bool
	b := NewWorkflowBuilder().WithWorkflowID("default")
	b.AddStartNode("a").WithAction(failAction("boom"))
	b.AddNode("b").DependsOn("a").WithAction(markAction(&downstreamRan))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("default")

	if err := dag.Execute(context.Background(), data); err == nil {
		t.Fatalf("Execute = nil, want error (default fail-fast)")
	}
	if downstreamRan.Load() {
		t.Fatalf("downstream ran after a default fail-fast failure")
	}
}

// TestContinueOnError_ConcurrentFanoutRace fans out many continue-on-error
// nodes that all fail concurrently in one level, plus a sibling that succeeds.
// Must be green under -race and the workflow must still complete (Execute nil).
func TestContinueOnError_ConcurrentFanoutRace(t *testing.T) {
	const n = 32
	b := NewWorkflowBuilder().WithWorkflowID("fanout")
	for i := 0; i < n; i++ {
		b.AddStartNode(fmt.Sprintf("f%d", i)).
			WithAction(failAction("boom")).
			WithContinueOnError()
	}
	var okRan atomic.Bool
	b.AddStartNode("ok").WithAction(markAction(&okRan))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("fanout")

	if err := dag.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute = %v, want nil (all failures are continue-on-error)", err)
	}
	if !okRan.Load() {
		t.Fatalf("the succeeding sibling did not run")
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%d", i)
		if st, _ := data.GetNodeStatus(name); st != Failed {
			t.Fatalf("%s status = %q, want Failed", name, st)
		}
	}
}

// contains is a tiny substring helper to avoid importing strings just for one check.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
