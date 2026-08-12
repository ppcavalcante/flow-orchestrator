// Command 06-saga-compensation shows durable, reverse-order rollback. A chain of forward
// steps each declares a compensation (its undo). When a later step FAILS, the engine
// rolls the run back: it runs the compensations of the already-completed steps in
// REVERSE-topological order, and each compensated step ends in the terminal Compensated
// status. A compensation that itself fails ends CompensationFailed — an honestly
// un-undone effect the run reports rather than hides.
//
//	reserve-seat ─▶ charge-card ─▶ issue-ticket ─▶ finalize (FAILS)
//	     └───────────── rolled back in reverse: issue-ticket, charge-card, reserve-seat
//
// It shows:
//   - WithCompensationFunc(fn) — attach an undo to a forward step,
//   - a hard failure downstream triggers the saga rollback of the COMPLETED steps,
//   - compensations run reverse-topologically (a step is undone AFTER its dependents),
//   - a clean rollback returns the trigger cause wrapped in ErrRolledBack; a rollback
//     where a compensation failed returns a *SagaError partitioning the outcome.
//
// The reverse order is recorded durably into the run's WorkflowData, so it is read back
// from the store — the same way the terminal statuses are.
//
// Run it:
//
//	go run ./examples/06-saga-compensation
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

const keyCompLog = "compensation_order" // comma-joined names in the order compensations ran

// forwardSteps is the chain, in forward (dependency) order. finalize is the trigger: it
// always fails, so the three steps before it roll back.
var forwardSteps = []string{"reserve-seat", "charge-card", "issue-ticket"}

// buildBookingSaga constructs the saga on the given store. failCompOf, when non-empty,
// names one step whose COMPENSATION fails — demonstrating the CompensationFailed path;
// "" makes every compensation succeed (the clean reverse rollback). Standalone so the
// smoke test builds the identical graph and asserts the durable rollback effect.
func buildBookingSaga(store workflow.WorkflowStore, id, failCompOf string) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(id).
		WithStore(store)

	// Each forward step succeeds and declares its undo. The steps form a linear chain,
	// so the reverse-topological rollback order is total and deterministic.
	prev := ""
	for _, step := range forwardSteps {
		nb := b.AddNode(step).
			WithActionFunc(forwardAction(step)).
			WithCompensationFunc(compensation(step, step == failCompOf))
		if prev != "" {
			nb.DependsOn(prev)
		}
		prev = step
	}

	// finalize is the trigger: it depends on the last step and always fails, which rolls
	// back every completed compensable step above it. It declares no compensation — it
	// never Completed, so there is nothing to undo.
	b.AddNode("finalize").
		DependsOn(prev).
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
			fmt.Println("finalize: FAILED — triggering rollback")
			return errors.New("booking gateway rejected the confirmation")
		})

	return workflow.FromBuilder(b)
}

// forwardAction is a step's forward work: it just announces itself here (a real step would
// reserve the seat, charge the card, etc.).
func forwardAction(step string) func(context.Context, *workflow.WorkflowData) error {
	return func(_ context.Context, _ *workflow.WorkflowData) error {
		fmt.Printf("forward: %s ok\n", step)
		return nil
	}
}

// compensation is a step's undo. It appends its name to the durable reverse-order log so
// the order is readable from the store afterwards. When shouldFail is set it returns an
// error WITHOUT logging — modelling an undo that could not complete (CompensationFailed).
func compensation(step string, shouldFail bool) func(context.Context, *workflow.WorkflowData) error {
	return func(_ context.Context, data *workflow.WorkflowData) error {
		if shouldFail {
			fmt.Printf("compensate: %s FAILED (effect NOT undone)\n", step)
			return fmt.Errorf("could not undo %s", step)
		}
		// Read-modify-write is safe here: the chain is linear, so compensations run one
		// at a time in reverse order — no two ever touch the log concurrently.
		cur, _ := data.GetString(keyCompLog)
		if cur != "" {
			cur += ","
		}
		data.Set(keyCompLog, cur+step)
		fmt.Printf("compensate: %s undone\n", step)
		return nil
	}
}

// runOne builds and drives one scenario, reporting the rollback outcome. The rollback is
// the DEMONSTRATED behaviour, so a non-nil Execute error here is expected — it is not an
// infrastructure failure and must not abort the program.
func runOne(id, failCompOf string) error {
	store := workflow.NewInMemoryStore()

	wf, err := buildBookingSaga(store, id, failCompOf)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	execErr := wf.Execute(context.Background())

	data, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	// Classify the outcome. A partial rollback (some compensation failed) is a *SagaError;
	// a clean rollback wraps the trigger cause in ErrRolledBack.
	var sagaErr *workflow.SagaError
	switch {
	case errors.As(execErr, &sagaErr):
		fmt.Printf("outcome: PARTIAL rollback — compensated=%v failed-to-compensate=%v\n",
			sagaErr.Compensated, names(sagaErr.FailedToCompensate))
	case errors.Is(execErr, workflow.ErrRolledBack):
		fmt.Println("outcome: clean rollback (every compensation succeeded)")
	case execErr != nil:
		return fmt.Errorf("unexpected execute error: %w", execErr)
	default:
		return fmt.Errorf("expected a rollback, got success")
	}

	order, _ := data.GetString(keyCompLog)
	fmt.Printf("compensation order (reverse): %s\n\n", order)
	return nil
}

// names extracts the node names from a NodeError slice for display.
func names(nes []workflow.NodeError) []string {
	out := make([]string, len(nes))
	for i, ne := range nes {
		out[i] = ne.NodeName
	}
	return out
}

func run() error {
	fmt.Println("── scenario A: every compensation succeeds ──")
	if err := runOne("booking-clean", ""); err != nil {
		return err
	}
	fmt.Println("── scenario B: charge-card's compensation fails ──")
	return runOne("booking-partial", "charge-card")
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("06-saga-compensation: %v", err)
	}
}
