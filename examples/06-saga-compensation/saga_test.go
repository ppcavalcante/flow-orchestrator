package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestSaga_CleanRollback_ReverseOrder runs the exact graph the command builds and asserts
// the durable rollback effect: every completed step ended Compensated, the failing trigger
// ended Failed, the compensations ran in REVERSE-topological order (recorded durably), and
// Execute reported a clean rollback (the trigger cause wrapped in ErrRolledBack).
func TestSaga_CleanRollback_ReverseOrder(t *testing.T) {
	const id = "booking-clean"
	store := workflow.NewInMemoryStore()

	wf, err := buildBookingSaga(store, id, "" /* no compensation fails */)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// A clean rollback returns the trigger cause wrapped in ErrRolledBack — never nil,
	// never a *SagaError (which is reserved for a rollback that could not fully undo).
	execErr := wf.Execute(context.Background())
	if !errors.Is(execErr, workflow.ErrRolledBack) {
		t.Fatalf("execute error = %v, want it to wrap ErrRolledBack", execErr)
	}
	var sagaErr *workflow.SagaError
	if errors.As(execErr, &sagaErr) {
		t.Fatalf("clean rollback must NOT be a *SagaError, got %v", sagaErr)
	}

	data, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Every completed forward step ended Compensated.
	for _, step := range forwardSteps {
		if st, ok := data.GetNodeStatus(step); !ok || st != workflow.Compensated {
			t.Errorf("node %q status = %v (ok=%v), want Compensated", step, st, ok)
		}
	}
	// The trigger failed and was never compensated (it never Completed).
	if st, ok := data.GetNodeStatus("finalize"); !ok || st != workflow.Failed {
		t.Errorf("node %q status = %v (ok=%v), want Failed", "finalize", st, ok)
	}

	// Compensations ran reverse-topologically: the last step is undone first.
	if got, ok := data.GetString(keyCompLog); !ok || got != "issue-ticket,charge-card,reserve-seat" {
		t.Errorf("compensation order = %q (ok=%v), want reverse order", got, ok)
	}
}

// TestSaga_PartialRollback_CompensationFailed asserts the honest partial-rollback path:
// when one step's compensation fails, that step ends CompensationFailed and Execute
// returns a *SagaError that names it in FailedToCompensate, while the OTHER steps still
// compensate (Compensated) — the effect that could not be undone is reported, not hidden.
func TestSaga_PartialRollback_CompensationFailed(t *testing.T) {
	const id = "booking-partial"
	store := workflow.NewInMemoryStore()

	wf, err := buildBookingSaga(store, id, "charge-card" /* its compensation fails */)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	execErr := wf.Execute(context.Background())
	var sagaErr *workflow.SagaError
	if !errors.As(execErr, &sagaErr) {
		t.Fatalf("execute error = %v, want a *SagaError (a compensation failed)", execErr)
	}
	if got := names(sagaErr.FailedToCompensate); len(got) != 1 || got[0] != "charge-card" {
		t.Errorf("FailedToCompensate = %v, want [charge-card]", got)
	}

	data, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The failing step ended CompensationFailed; its neighbours still Compensated.
	want := map[string]workflow.NodeStatus{
		"reserve-seat": workflow.Compensated,
		"charge-card":  workflow.CompensationFailed,
		"issue-ticket": workflow.Compensated,
	}
	for node, st := range want {
		if got, ok := data.GetNodeStatus(node); !ok || got != st {
			t.Errorf("node %q status = %v (ok=%v), want %v", node, got, ok, st)
		}
	}
}
