package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestApproval_ParkWrongNonceThenApprove asserts the whole approval contract on the exact
// graph the command builds: the run parks (Waiting), a wrong-nonce decision leaves it
// parked, and only the correctly-correlated decision resumes it to Completed.
func TestApproval_ParkWrongNonceThenApprove(t *testing.T) {
	store := workflow.NewInMemoryStore()
	wf, err := buildApprovalWorkflow(store)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// (1) The run PARKS: Execute returns ErrSuspended and the gate is Waiting while its
	// downstream stays Pending (never runs behind a park).
	if err := wf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatalf("first Execute = %v, want ErrSuspended (parked)", err)
	}
	parked, err := store.Load(approvalWorkflowID)
	if err != nil {
		t.Fatalf("load parked: %v", err)
	}
	if st, ok := parked.GetNodeStatus(nodeGate); !ok || st != workflow.Waiting {
		t.Errorf("gate status = %v (ok=%v), want Waiting", st, ok)
	}
	if st, ok := parked.GetNodeStatus(nodePublish); !ok || st != workflow.Pending {
		t.Errorf("publish status = %v (ok=%v), want Pending while parked", st, ok)
	}

	// The store-derived nonce must equal the live one (the dispatcher path agrees with
	// the in-process path).
	nonce := wf.ApprovalNonce(nodeGate)
	if nonce == "" {
		t.Fatal("live nonce is empty")
	}
	storeNonce, err := workflow.ApprovalNonceFromStore(store, approvalWorkflowID, nodeGate)
	if err != nil {
		t.Fatalf("nonce from store: %v", err)
	}
	if nonce != storeNonce {
		t.Errorf("store nonce %q != live nonce %q", storeNonce, nonce)
	}

	// (2) A WRONG-nonce approval is inert: Execute still reports ErrSuspended and the gate
	// stays Waiting — a forged/misdirected decision cannot release the gate.
	forged := workflow.ApproveSignal(nodeGate, "mallory", "forged", "sig-forged", "not-the-nonce")
	if err := wf.DeliverAndResume(context.Background(), forged); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatalf("wrong-nonce DeliverAndResume = %v, want ErrSuspended (still parked)", err)
	}
	stillParked, err := store.Load(approvalWorkflowID)
	if err != nil {
		t.Fatalf("load still-parked: %v", err)
	}
	if st, _ := stillParked.GetNodeStatus(nodeGate); st != workflow.Waiting {
		t.Errorf("gate after wrong nonce = %v, want Waiting", st)
	}
	if published, _ := stillParked.GetBool(keyPublished); published {
		t.Error("published set while still parked — downstream ran behind the gate")
	}

	// (3) The correct nonce resumes the run to Completed.
	ok := workflow.ApproveSignal(nodeGate, "alice", "ship it", "sig-ok", nonce)
	if err := wf.DeliverAndResume(context.Background(), ok); err != nil {
		t.Fatalf("correct-nonce DeliverAndResume: %v", err)
	}
	final, err := store.Load(approvalWorkflowID)
	if err != nil {
		t.Fatalf("load final: %v", err)
	}
	for _, node := range []string{nodeGate, nodePublish} {
		if st, ok := final.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
	if published, ok := final.GetBool(keyPublished); !ok || !published {
		t.Errorf("published = %v (ok=%v), want true", published, ok)
	}
}

// TestFirstOf_SignalArmWins: a payment signal delivered before the deadline wins — the
// wait resolves to the signal payload and the run converges.
func TestFirstOf_SignalArmWins(t *testing.T) {
	store := workflow.NewInMemoryStore()
	clk := workflow.NewFakeClock(time.Unix(0, 0))
	wf, err := buildFirstOfWorkflow(store, clk)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if err := wf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatalf("first Execute = %v, want ErrSuspended (parked)", err)
	}
	// Deliver while the clock is still before the deadline → the signal arm wins.
	paid := workflow.Signal{ID: "pay-1", Name: signalPayment, Payload: "paid-in-full"}
	if err := wf.DeliverAndResume(context.Background(), paid); err != nil {
		t.Fatalf("signal DeliverAndResume: %v", err)
	}

	final, err := store.Load(firstOfWorkflowID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, node := range []string{nodeAwait, nodeSettle} {
		if st, ok := final.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
	// The signal arm applies the payload as the node output (never the timeout sentinel).
	if out, ok := final.GetOutput(nodeAwait); !ok || out != "paid-in-full" {
		t.Errorf("await output = %q (ok=%v), want the signal payload", out, ok)
	}
}

// TestFirstOf_TimerArmFires: with NO signal, advancing the durable clock past the absolute
// deadline fires the timer arm — the wait completes on the timeout path and the run
// converges. The timeout sentinel output ("true") distinguishes it from the signal arm.
func TestFirstOf_TimerArmFires(t *testing.T) {
	store := workflow.NewInMemoryStore()
	clk := workflow.NewFakeClock(time.Unix(0, 0))
	wf, err := buildFirstOfWorkflow(store, clk)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if err := wf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatalf("first Execute = %v, want ErrSuspended (parked on the deadline)", err)
	}
	// No signal is ever delivered. Jump past the absolute deadline and re-drive: the timer
	// arm resolves the wait (a nil Execute — the run completed, not parked).
	clk.Advance(2 * paymentTimeout)
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("second Execute after deadline = %v, want nil (timer fired, run completed)", err)
	}

	final, err := store.Load(firstOfWorkflowID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, node := range []string{nodeAwait, nodeSettle} {
		if st, ok := final.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
	// The timeout arm's node output is the sentinel "true", not a signal payload.
	if out, ok := final.GetOutput(nodeAwait); !ok || out != "true" {
		t.Errorf("await output = %q (ok=%v), want the timeout sentinel \"true\"", out, ok)
	}
}
