package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestResilient_RecoversAndContinues asserts the three resilience properties the pipeline
// teaches: a retried node ends Completed on the expected attempt, a continue-on-error node
// ends Failed WITHOUT aborting the run, and its downstream still runs.
func TestResilient_RecoversAndContinues(t *testing.T) {
	store := workflow.NewInMemoryStore()

	wf, err := buildResilientWorkflow(store)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The resilient pipeline is designed to SUCCEED overall — the only failure is a
	// continue-on-error node, which does not fail the workflow.
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("execute returned error, want nil (pipeline should survive): %v", err)
	}

	data, err := store.Load(resilientID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The retried node recovered and is Completed, on exactly attempt 2.
	if st, ok := data.GetNodeStatus(nodeRetry); !ok || st != workflow.Completed {
		t.Errorf("%s status = %v (ok=%v), want Completed", nodeRetry, st, ok)
	}
	if got, ok := data.GetInt64(keyRetryAttempts); !ok || got != 2 {
		t.Errorf("%s = %d (ok=%v), want 2", keyRetryAttempts, got, ok)
	}

	// The backoff node recovered and is Completed, on exactly attempt 3.
	if st, ok := data.GetNodeStatus(nodeBackoff); !ok || st != workflow.Completed {
		t.Errorf("%s status = %v (ok=%v), want Completed", nodeBackoff, st, ok)
	}
	if got, ok := data.GetInt64(keyBackoffAttempts); !ok || got != 3 {
		t.Errorf("%s = %d (ok=%v), want 3", keyBackoffAttempts, got, ok)
	}

	// The non-critical node Failed — but that did not abort the run.
	if st, ok := data.GetNodeStatus(nodeOptional); !ok || st != workflow.Failed {
		t.Errorf("%s status = %v (ok=%v), want Failed", nodeOptional, st, ok)
	}

	// The proof that a continue-on-error failure does NOT abort: its downstream ran.
	if st, ok := data.GetNodeStatus(nodeSummarize); !ok || st != workflow.Completed {
		t.Errorf("%s status = %v (ok=%v), want Completed (downstream of a continue-on-error node must still run)", nodeSummarize, st, ok)
	}
	if done, ok := data.GetBool(keySummary); !ok || !done {
		t.Errorf("%s = %v (ok=%v), want true", keySummary, done, ok)
	}
}

// TestFatal_SurfacesActionError asserts that a genuinely-fatal node fails the whole run,
// its downstream does not run, and the returned error carries the action-domain sentinel.
func TestFatal_SurfacesActionError(t *testing.T) {
	store := workflow.NewInMemoryStore()

	wf, err := buildFatalWorkflow(store)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	err = wf.Execute(context.Background())
	if err == nil {
		t.Fatal("execute returned nil, want an error (charge-card must fail the run)")
	}

	// The error reaches through to the action sentinel — ErrInputNotFound, not a store error.
	if !errors.Is(err, workflow.ErrInputNotFound) {
		t.Errorf("errors.Is(err, ErrInputNotFound) = false, want true (err=%v)", err)
	}
	// And it is NOT the store's not-found sentinel — the two domains are distinct.
	if errors.Is(err, workflow.ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = true, want false — an action error must not read as a store error")
	}

	// The aggregate names the failed node.
	var execErr *workflow.ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("errors.As(*ExecutionError) = false, want true (err=%v)", err)
	}
	var sawChargeCard bool
	for _, ne := range execErr.FailedNodes {
		if ne.NodeName == nodeChargeCard {
			sawChargeCard = true
		}
	}
	if !sawChargeCard {
		t.Errorf("FailedNodes = %+v, want it to include %q", execErr.FailedNodes, nodeChargeCard)
	}

	// Downstream of the fatal node did NOT run to completion.
	data, err := store.Load(fatalID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, _ := data.GetNodeStatus(nodeChargeCard); st != workflow.Failed {
		t.Errorf("%s status = %v, want Failed", nodeChargeCard, st)
	}
	if st, _ := data.GetNodeStatus(nodeShip); st == workflow.Completed {
		t.Errorf("%s status = Completed, want it NOT to have run", nodeShip)
	}
}

// TestStoreDomain_MissingIsNotFound pins the two-domain taxonomy: a missing workflow is a
// STORE error (ErrNotFound), never an action error.
func TestStoreDomain_MissingIsNotFound(t *testing.T) {
	store := workflow.NewInMemoryStore()
	_, err := store.Load("definitely-absent")
	if err == nil {
		t.Fatal("Load(absent) returned nil error, want ErrNotFound")
	}
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false, want true (err=%v)", err)
	}
	if errors.Is(err, workflow.ErrInputNotFound) {
		t.Errorf("errors.Is(err, ErrInputNotFound) = true, want false — store domain must not read as action domain")
	}
}
