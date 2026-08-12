// Command 01-hello-dag is the smallest useful Flow Orchestrator workflow: three nodes
// wired by their data dependencies, run to completion in-memory.
//
//	validate ──▶ price ──▶ confirm
//
// It shows the four things every workflow is made of:
//   - a builder (NewWorkflowBuilder) with a store,
//   - nodes added with AddStartNode / AddNode and wired with DependsOn,
//   - actions (WithActionFunc) that read and write shared WorkflowData,
//   - a *Workflow produced by FromBuilder and driven with Execute.
//
// Run it:
//
//	go run ./examples/01-hello-dag
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Keys under which nodes publish their results into the shared WorkflowData. Using
// named constants (rather than bare strings at each call site) keeps producers and
// consumers honest — the compiler catches a typo that a string literal would not.
const (
	keyItemCount = "item_count"
	keyTotal     = "total_cents"
	keyConfirmed = "confirmed"
)

// buildOrderWorkflow constructs the graph on the given store. It is a standalone
// function — not inlined into run — so the smoke test can build the identical workflow
// and assert on its durable result. Every example in this suite follows that shape: a
// build* helper the test reuses, so what ships is exactly what is tested.
func buildOrderWorkflow(store workflow.WorkflowStore) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID("hello-order").
		WithStore(store)

	// The start node has no dependencies; it seeds the shared data.
	b.AddStartNode("validate").WithActionFunc(validate)

	// price depends on validate, so the executor runs it only after validate completes
	// and its writes are visible.
	b.AddNode("price").WithActionFunc(price).DependsOn("validate")

	// confirm depends on price. validate ▶ price ▶ confirm is a linear chain here, but
	// dependencies form an arbitrary DAG — a node may depend on several, and independent
	// nodes at the same level run in parallel.
	b.AddNode("confirm").WithActionFunc(confirm).DependsOn("price")

	// FromBuilder validates the graph and returns a store-backed *Workflow. (Build()
	// returns a bare *DAG and deliberately REFUSES a store-configured builder, so a
	// store-backed run always goes through FromBuilder.)
	return workflow.FromBuilder(b)
}

// validate seeds the order: three items. A real action would parse a request; here we
// keep the input fixed so the example is deterministic and its test is exact.
func validate(_ context.Context, data *workflow.WorkflowData) error {
	const items = 3
	data.Set(keyItemCount, int64(items))
	fmt.Printf("validate: order has %d items\n", items)
	return nil
}

// price reads the item count and computes a total. It reads the upstream write by key —
// the only contract between nodes is the data, which is what makes the graph, not the Go
// call order, the source of truth (and what makes a crash resumable: see 03).
func price(_ context.Context, data *workflow.WorkflowData) error {
	count, ok := data.GetInt64(keyItemCount)
	if !ok {
		// A missing upstream value is a real defect in THIS example (validate always
		// runs first), so surface it rather than pricing a zero-item order.
		return fmt.Errorf("price: %q not set by validate", keyItemCount)
	}
	const unitCents = 1299
	total := count * unitCents
	data.Set(keyTotal, total)
	fmt.Printf("price: %d items × %d¢ = %d¢\n", count, unitCents, total)
	return nil
}

// confirm reads the computed total and records the terminal fact.
func confirm(_ context.Context, data *workflow.WorkflowData) error {
	total, ok := data.GetInt64(keyTotal)
	if !ok {
		return fmt.Errorf("confirm: %q not set by price", keyTotal)
	}
	data.Set(keyConfirmed, true)
	fmt.Printf("confirm: order confirmed for %d¢\n", total)
	return nil
}

func run() error {
	// The in-memory store keeps the run's data for the process lifetime — perfect for a
	// demo. Swap in NewSQLiteStore (example 03) and the same graph survives a crash.
	store := workflow.NewInMemoryStore()

	wf, err := buildOrderWorkflow(store)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// This example demonstrates a straightforward SUCCESSFUL run — its actions are
	// deterministic with no injected failure — so a non-nil Execute error is a real
	// defect, not something the example is teaching. (Example 02 teaches failure.)
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	// Read the durable result back from the store, exactly as an operator would.
	data, err := store.Load("hello-order")
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	confirmed, _ := data.GetBool(keyConfirmed)
	total, _ := data.GetInt64(keyTotal)
	fmt.Printf("\nresult: confirmed=%v total=%d¢\n", confirmed, total)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("01-hello-dag: %v", err)
	}
}
