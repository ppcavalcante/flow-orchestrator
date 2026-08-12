// Command 07-signals-timers-approvals is human-in-the-loop and durable waiting: a run
// PARKS on an external event (a human decision or a clock) and resumes later — possibly
// in a different process — with no work replayed. It shows two suspension primitives:
//
//   - AddApproval — a decision gate. The run parks (Waiting) until an approve/reject
//     decision is delivered to the durable mailbox. Releasing it requires the AUD-025
//     correlation nonce: a decision carrying the wrong (or no) nonce is INERT — the run
//     stays parked — so a stale or forged approval can neither approve nor reject.
//
//   - AddWaitForSignalTimeout — a durable first-of(signal, timer). The run parks until
//     EITHER the named signal arrives OR an absolute deadline (frozen at first encounter,
//     so it is durable-remaining across a crash) passes. Whichever comes first wins.
//
// The suspend contract is a THREE-outcome Execute: nil = completed, ErrSuspended = parked
// (a success arm — the checkpoint is durably flushed, the process may exit), any other
// error = a real failure. Callers branch the park with errors.Is(err, ErrSuspended).
//
// Run it:
//
//	go run ./examples/07-signals-timers-approvals
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Named keys and identifiers. The signal name a wait/approval consumes EQUALS the node
// name (AddApproval / AddWaitForSignalTimeout derive it 1:1), so the node-name constants
// double as signal-name constants and the two can never drift.
const (
	approvalWorkflowID = "release-approval"
	nodeGate           = "gate"    // the AddApproval node — parks until a decision
	nodePublish        = "publish" // downstream of gate — must not run while parked
	keyPublished       = "published"

	firstOfWorkflowID = "await-payment"
	nodeAwait         = "await"   // the AddWaitForSignalTimeout node
	signalPayment     = "payment" // the signal nodeAwait waits for
	nodeSettle        = "settle"  // downstream of await — runs on either arm
	keySettled        = "settled"
)

// paymentTimeout is the first-of deadline. It is a DURABLE absolute instant (frozen at
// first encounter), not a live countdown — this example advances a FakeClock past it to
// fire the timer deterministically and instantly, the way a real "3h later" resume would.
const paymentTimeout = time.Hour

// buildApprovalWorkflow wires an approval gate with a downstream publish node over store.
// It is a standalone helper so the smoke test builds the identical graph. AddApproval
// requires a Store implementing SignalStore (InMemoryStore does); the durable, store-backed
// run goes through FromBuilder (Build() refuses a store-configured builder).
func buildApprovalWorkflow(store workflow.WorkflowStore) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(approvalWorkflowID).
		WithStore(store)

	// The approval node PARKS the run (Waiting) when reached. Its action is set by
	// AddApproval directly — do not also call WithAction (that would replace it).
	b.AddApproval(nodeGate)

	// publish depends on gate, so it cannot run until the gate is approved. This is what
	// makes the gate a real barrier: while parked, downstream stays Pending.
	b.AddNode(nodePublish).WithActionFunc(publish).DependsOn(nodeGate)

	return workflow.FromBuilder(b)
}

// publish records the terminal fact once the gate is approved.
func publish(_ context.Context, data *workflow.WorkflowData) error {
	data.Set(keyPublished, true)
	fmt.Println("publish: release published (gate was approved)")
	return nil
}

// buildFirstOfWorkflow wires a first-of(signal, timer) node with a downstream settle node
// over store, driven by clk so durable time is deterministic. The wait resolves on the
// payment signal OR the timeout, whichever comes first; settle runs on either arm.
func buildFirstOfWorkflow(store workflow.WorkflowStore, clk workflow.Clock) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(firstOfWorkflowID).
		WithStore(store).
		WithClock(clk) // the clock the durable timer reads; a FakeClock makes "later" instant

	b.AddWaitForSignalTimeout(nodeAwait, signalPayment, paymentTimeout)
	b.AddNode(nodeSettle).WithActionFunc(settle).DependsOn(nodeAwait)

	return workflow.FromBuilder(b)
}

// settle records that the run converged, regardless of which arm won. The awaited node's
// own output (readable via GetOutput) is the signal payload on the signal arm and the
// sentinel "true" on the timeout arm — the test reads that to tell the arms apart.
func settle(_ context.Context, data *workflow.WorkflowData) error {
	data.Set(keySettled, true)
	fmt.Println("settle: run converged past the first-of wait")
	return nil
}

// runApproval demonstrates the park → wrong-nonce-inert → correct-nonce-resumes narrative.
func runApproval() error {
	store := workflow.NewInMemoryStore()
	wf, err := buildApprovalWorkflow(store)
	if err != nil {
		return fmt.Errorf("build approval: %w", err)
	}

	// First Execute PARKS: no decision has been delivered. ErrSuspended is the SUCCESS
	// arm (the run is durably checkpointed), so we branch it with errors.Is — not treat
	// it as a failure.
	if err := wf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		return fmt.Errorf("approval: expected ErrSuspended park, got %w", err)
	}
	fmt.Printf("approval: run parked, %q is Waiting\n", nodeGate)

	// The nonce correlates a decision to THIS park. Derive it two equivalent ways: from
	// the live Workflow, and from the store alone (what a signal-pump / dispatcher that
	// holds no *Workflow would use). They must agree.
	nonce := wf.ApprovalNonce(nodeGate)
	storeNonce, err := workflow.ApprovalNonceFromStore(store, approvalWorkflowID, nodeGate)
	if err != nil {
		return fmt.Errorf("approval: nonce from store: %w", err)
	}
	if nonce != storeNonce {
		return fmt.Errorf("approval: live nonce %q != store nonce %q", nonce, storeNonce)
	}

	// A decision carrying the WRONG nonce is inert: the run stays parked. This is the
	// security-relevant behavior — a misdirected or forged approval cannot release the gate.
	forged := workflow.ApproveSignal(nodeGate, "mallory", "forged", "sig-forged", "not-the-nonce")
	if err := wf.DeliverAndResume(context.Background(), forged); !errors.Is(err, workflow.ErrSuspended) {
		return fmt.Errorf("approval: a wrong-nonce approval must leave the run parked, got %w", err)
	}
	fmt.Println("approval: wrong-nonce decision was INERT — still parked")

	// The correctly-correlated decision resumes the run to completion.
	ok := workflow.ApproveSignal(nodeGate, "alice", "ship it", "sig-ok", nonce)
	if err := wf.DeliverAndResume(context.Background(), ok); err != nil {
		return fmt.Errorf("approval: correct-nonce approve should complete, got %w", err)
	}

	data, err := store.Load(approvalWorkflowID)
	if err != nil {
		return fmt.Errorf("approval: load: %w", err)
	}
	published, _ := data.GetBool(keyPublished)
	fmt.Printf("approval: resumed to completion, published=%v\n\n", published)
	return nil
}

// runFirstOf demonstrates both arms of the durable first-of: the signal winning, and the
// timer winning when no signal arrives.
func runFirstOf() error {
	// --- signal arm: deliver the payment signal before the deadline; the signal wins. ---
	signalStore := workflow.NewInMemoryStore()
	clk := workflow.NewFakeClock(time.Unix(0, 0))
	sigWf, err := buildFirstOfWorkflow(signalStore, clk)
	if err != nil {
		return fmt.Errorf("build first-of (signal): %w", err)
	}
	if err := sigWf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		return fmt.Errorf("first-of: expected park, got %w", err)
	}
	// Deliver the signal while the clock is still before the deadline → the signal wins.
	paid := workflow.Signal{ID: "pay-1", Name: signalPayment, Payload: "paid-in-full"}
	if err := sigWf.DeliverAndResume(context.Background(), paid); err != nil {
		return fmt.Errorf("first-of: signal resume: %w", err)
	}
	sd, err := signalStore.Load(firstOfWorkflowID)
	if err != nil {
		return fmt.Errorf("first-of: load signal: %w", err)
	}
	sigOut, _ := sd.GetOutput(nodeAwait)
	fmt.Printf("first-of: signal arm won, await output=%q\n", sigOut)

	// --- timer arm: deliver NOTHING; advance the clock past the deadline; the timer fires. ---
	timerStore := workflow.NewInMemoryStore()
	tclk := workflow.NewFakeClock(time.Unix(0, 0))
	timerWf, err := buildFirstOfWorkflow(timerStore, tclk)
	if err != nil {
		return fmt.Errorf("build first-of (timer): %w", err)
	}
	if err := timerWf.Execute(context.Background()); !errors.Is(err, workflow.ErrSuspended) {
		return fmt.Errorf("first-of: expected park, got %w", err)
	}
	tclk.Advance(2 * paymentTimeout) // jump past the absolute deadline — instant "later"
	if err := timerWf.Execute(context.Background()); err != nil {
		return fmt.Errorf("first-of: overdue deadline should fire the timer arm, got %w", err)
	}
	td, err := timerStore.Load(firstOfWorkflowID)
	if err != nil {
		return fmt.Errorf("first-of: load timer: %w", err)
	}
	timerOut, _ := td.GetOutput(nodeAwait)
	fmt.Printf("first-of: timer arm won (no signal), await output=%q (the timeout sentinel)\n", timerOut)
	return nil
}

func run() error {
	if err := runApproval(); err != nil {
		return err
	}
	return runFirstOf()
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("07-signals-timers-approvals: %v", err)
	}
}
