package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// DEC-CHUNK3-status tests. Contract:
//   - Pending is the explicit initial state: a node queried before it runs is
//     Pending (present in the map), not absent.
//   - Skipped (S1, narrow): a node is Skipped iff it did NOT run AND >=1 dep is
//     terminal non-resolving — Failed (non-coe) OR itself Skipped. Transitive.
//     A coe-resolved failed dep (coe && Failed) does NOT skip the dependent.
//   - Independent un-run-on-halt nodes stay Pending (not Skipped).
//   - Skipped nodes are NOT failures: never in ExecutionError.FailedNodes.

// TestStatus_PendingIsInitial: Execute initializes every DAG node to Pending, so
// status is total over the DAG — after a run, NO node is absent from the map, and
// a node that was never reached (no failed/skipped ancestor) reads Pending rather
// than absent. (Pending is initialized at Execute start, not at build time.)
func TestStatus_PendingIsInitial(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("pending-init")
	// Two independent chains so the run halts on one but leaves the other's later
	// node genuinely unreached (and unrelated to the failure).
	b.AddStartNode("a").WithAction(failAction("boom")) // fail-fast halts at level 0
	b.AddNode("downstream").DependsOn("a").
		WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
	b.AddStartNode("root"). // independent root, runs at level 0
				WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
	b.AddNode("leaf").DependsOn("root"). // independent, unreached after halt
						WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("pending-init")

	if err := dag.Execute(context.Background(), data); err == nil {
		t.Fatal("Execute = nil, want error (a failed)")
	}

	// Totality: every node is present in the status map after Execute.
	for _, name := range []string{"a", "downstream", "root", "leaf"} {
		if _, ok := data.GetNodeStatus(name); !ok {
			t.Fatalf("node %q absent from status map after Execute; status must be total over the DAG", name)
		}
	}
	// The independent, never-reached node reads Pending (not absent, not Skipped).
	if st, _ := data.GetNodeStatus("leaf"); st != Pending {
		t.Fatalf("leaf = %q, want Pending (initialized, never reached, no failed ancestor)", st)
	}
}

// TestStatus_TransitiveSkipOnFailFast: a -> b -> c, a (normal) fails. b and c
// never run; both must be Skipped (transitively), not Pending/absent.
func TestStatus_TransitiveSkipOnFailFast(t *testing.T) {
	var bRan, cRan atomic.Bool
	b := NewWorkflowBuilder().WithWorkflowID("transitive-skip")
	b.AddStartNode("a").WithAction(failAction("boom"))
	b.AddNode("b").DependsOn("a").WithAction(markAction(&bRan))
	b.AddNode("c").DependsOn("b").WithAction(markAction(&cRan))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("transitive-skip")

	execErr := dag.Execute(context.Background(), data)
	if execErr == nil {
		t.Fatal("Execute = nil, want error (a failed)")
	}
	if bRan.Load() || cRan.Load() {
		t.Fatal("b or c ran; both are downstream of a failure")
	}

	if st, _ := data.GetNodeStatus("a"); st != Failed {
		t.Fatalf("a = %q, want Failed", st)
	}
	if st, _ := data.GetNodeStatus("b"); st != Skipped {
		t.Fatalf("b = %q, want Skipped (direct dependent of failed a)", st)
	}
	if st, _ := data.GetNodeStatus("c"); st != Skipped {
		t.Fatalf("c = %q, want Skipped (transitive: depends on Skipped b)", st)
	}
}

// TestStatus_IndependentUnreachedStaysPending: on a fail-fast halt, a node that
// does NOT depend (transitively) on the failure stays Pending, not Skipped.
// fail (level 0) blocks dependentOfFail; independent sits in level 1 behind a
// separate root that also can't be scheduled after halt — it must stay Pending.
func TestStatus_IndependentUnreachedStaysPending(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("independent-pending")
	// Failure chain: fail -> blocked
	b.AddStartNode("fail").WithAction(failAction("boom"))
	b.AddNode("blocked").DependsOn("fail").
		WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
	// Independent chain: root -> leaf, no relation to fail. root is at level 0
	// and runs; leaf is at level 1 and is unreached after halt.
	var rootRan atomic.Bool
	b.AddStartNode("root").WithAction(markAction(&rootRan))
	b.AddNode("leaf").DependsOn("root").
		WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("independent-pending")

	if err := dag.Execute(context.Background(), data); err == nil {
		t.Fatal("Execute = nil, want error (fail failed)")
	}

	if st, _ := data.GetNodeStatus("blocked"); st != Skipped {
		t.Fatalf("blocked = %q, want Skipped (depends on failed fail)", st)
	}
	// leaf does not depend on fail; it was just never reached after halt.
	if st, _ := data.GetNodeStatus("leaf"); st != Pending {
		t.Fatalf("leaf = %q, want Pending (independent, unreached — NOT Skipped)", st)
	}
}

// TestStatus_CoeResolvedDepDoesNotSkip: a coe node fails; its dependent RUNS
// (coe-Failed resolves the dep) and ends Completed, NOT Skipped.
func TestStatus_CoeResolvedDepDoesNotSkip(t *testing.T) {
	var depRan atomic.Bool
	b := NewWorkflowBuilder().WithWorkflowID("coe-no-skip")
	b.AddStartNode("soft").WithAction(failAction("boom")).WithContinueOnError()
	b.AddNode("dependent").DependsOn("soft").WithAction(markAction(&depRan))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("coe-no-skip")

	if err := dag.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute = %v, want nil (only failure is coe)", err)
	}
	if !depRan.Load() {
		t.Fatal("dependent did not run; a coe-resolved failed dep must NOT skip it")
	}
	if st, _ := data.GetNodeStatus("soft"); st != Failed {
		t.Fatalf("soft = %q, want Failed", st)
	}
	if st, _ := data.GetNodeStatus("dependent"); st != Completed {
		t.Fatalf("dependent = %q, want Completed (ran normally), never Skipped", st)
	}
}

// TestStatus_SkippedNotInExecutionError: Skipped nodes are NOT failures and must
// not appear in the chunk-2 ExecutionError aggregate.
func TestStatus_SkippedNotInExecutionError(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("skip-not-fail")
	b.AddStartNode("a").WithAction(failAction("boom"))
	b.AddNode("b").DependsOn("a").
		WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("skip-not-fail")

	execErr := dag.Execute(context.Background(), data)
	var ee *ExecutionError
	if !errors.As(execErr, &ee) {
		t.Fatalf("Execute error is not *ExecutionError: %T", execErr)
	}
	// Only "a" failed; "b" was skipped and must not be in FailedNodes.
	if len(ee.FailedNodes) != 1 || ee.FailedNodes[0].NodeName != "a" {
		t.Fatalf("FailedNodes = %+v, want exactly [a] (skipped b excluded)", ee.FailedNodes)
	}
	if st, _ := data.GetNodeStatus("b"); st != Skipped {
		t.Fatalf("b = %q, want Skipped", st)
	}
}

// TestStatus_SkippedSurvivesFBRoundTrip: a Skipped status persists through a
// FlatBuffers Save -> Load (wire slot 4 already exists).
func TestStatus_SkippedSurvivesFBRoundTrip(t *testing.T) {
	store, err := NewFlatBuffersStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	data := NewWorkflowData("fb-skip")
	data.SetNodeStatus("s", Skipped)
	data.SetNodeStatus("p", Pending)

	if err := store.Save(data); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.Load("fb-skip")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, _ := loaded.GetNodeStatus("s"); st != Skipped {
		t.Fatalf("loaded s = %q, want Skipped (must survive FB round-trip)", st)
	}
	if st, _ := loaded.GetNodeStatus("p"); st != Pending {
		t.Fatalf("loaded p = %q, want Pending", st)
	}
}
