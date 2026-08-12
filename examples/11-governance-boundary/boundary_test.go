package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestGovernance_HonestTopologyBuildsAndRuns proves the correctly-gated topology
// is NOT over-rejected: it builds clean and executes to its accepted terminal.
func TestGovernance_HonestTopologyBuildsAndRuns(t *testing.T) {
	store := workflow.NewInMemoryStore()

	wf, err := buildBoundaryWorkflow(store, false)
	if err != nil {
		t.Fatalf("honest topology was refused at Build (guard over-rejects): %v", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, err := store.Load("governance-boundary")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if accepted, ok := data.GetBool(keyAccepted); !ok || !accepted {
		t.Errorf("accepted = %v (ok=%v), want true", accepted, ok)
	}
	// Every node ran through the gate — nothing skipped.
	for _, node := range []string{nodeSeed, nodeDoer, nodeVerify, nodeAccept} {
		if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
}

// TestGovernance_BypassRefusedAtBuild is the point of the example: declaring the
// boundary turns the verify-bypass footgun (accept.DependsOn(doer)) into a
// Build-time ErrValidation that names the dominance violation. Because the
// rejection is the taught behavior, the test asserts it — it is a success, not a
// failure.
func TestGovernance_BypassRefusedAtBuild(t *testing.T) {
	_, err := buildBoundaryWorkflow(workflow.NewInMemoryStore(), true)
	if err == nil {
		t.Fatal("bypass footgun built clean — the boundary guard did NOT fire")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		t.Fatalf("bypass refusal is not an ErrValidation: %v", err)
	}
	if !strings.Contains(err.Error(), "without passing verifier") {
		t.Errorf("refusal message %q does not name the dominance violation "+
			"(want it to contain %q)", err.Error(), "without passing verifier")
	}
}
