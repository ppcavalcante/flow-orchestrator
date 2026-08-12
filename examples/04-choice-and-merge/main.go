// Command 04-choice-and-merge shows conditional branching: a ChoiceNode routes to
// exactly ONE arm based on the run's data, the arms it did NOT pick end Bypassed
// (an explicit, durable "deliberately not run" — distinct from Skipped), and a
// MergeNode re-converges the branches into a single downstream path.
//
//	                 ┌─▶ premium-price ─┐
//	seed ─▶ route ───┼─▶ standard-price ┼─▶ total (merge)
//	                 └─▶ reject ────────┘
//
// It shows:
//   - AddChoice(...).When(pred, arm).Otherwise(arm) — the routing decision IS the
//     choice's action; it takes no WithActionFunc,
//   - the not-taken arms end Bypassed (M11), while the taken arm ends Completed,
//   - AddMerge(...).From(arms...) — the M11 OR-join: a downstream node may NOT
//     DependsOn several choice arms directly (that is an "unstructured
//     reconvergence" build error); the merge is how branches legally re-converge.
//
// main() runs the SAME graph twice with different seed data so both arms are
// exercised — premium once, standard once.
//
// Run it:
//
//	go run ./examples/04-choice-and-merge
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Keys the nodes read and write in the shared WorkflowData. Named constants keep the
// producer (an arm) and the consumer (the merge) honest about the key they share.
const (
	keyKind     = "order_kind"  // seeded input: "premium" | "standard" | anything else
	keyQuantity = "quantity"    // seeded input: how many units
	keyArmPrice = "arm_price"   // the taken arm writes its per-unit-computed total here
	keyTotal    = "final_total" // the merge publishes the reconverged result here
)

// Per-unit prices, in cents, for the two real arms.
const (
	premiumUnitCents  = 2000
	standardUnitCents = 1000
)

// buildPricingWorkflow constructs the choice+merge graph on the given store, seeded
// with an order kind. It is a standalone helper so the smoke test builds the identical
// graph and asserts its durable result — what ships is exactly what is tested.
func buildPricingWorkflow(store workflow.WorkflowStore, id, kind string) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(id).
		WithStore(store)

	// seed publishes the routing input (kind) and the quantity the arms price.
	b.AddStartNode("seed").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set(keyKind, kind)
		data.Set(keyQuantity, int64(3))
		fmt.Printf("seed: kind=%q quantity=3\n", kind)
		return nil
	})

	// route is a ChoiceNode: its "action" is the routing decision, so it takes When /
	// Otherwise arms, not WithActionFunc. DependsOn("seed") makes the choice evaluate
	// only after seed's writes are visible — without it the choice could read keyKind
	// before seed set it.
	b.AddChoice("route").
		DependsOn("seed").
		When(func(d *workflow.WorkflowData) bool {
			k, _ := d.GetString(keyKind)
			return k == "premium"
		}, "premium-price").
		When(func(d *workflow.WorkflowData) bool {
			k, _ := d.GetString(keyKind)
			return k == "standard"
		}, "standard-price").
		Otherwise("reject")

	// The three arms each DependsOn the choice. Exactly one runs (Completed); the other
	// two end Bypassed. Both real arms write the SAME key (keyArmPrice), so the merge
	// downstream reads one key regardless of which arm won.
	b.AddNode("premium-price").DependsOn("route").WithActionFunc(priceArm(premiumUnitCents))
	b.AddNode("standard-price").DependsOn("route").WithActionFunc(priceArm(standardUnitCents))
	b.AddNode("reject").DependsOn("route").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set(keyArmPrice, int64(0)) // an unknown kind prices to nothing
		fmt.Println("reject: unknown kind → price 0")
		return nil
	})

	// total is a MergeNode that OR-joins the three arms (From). It runs once, after the
	// single taken arm completes (the Bypassed arms satisfy the join without blocking),
	// and publishes the reconverged result. This is the only legal way to re-converge
	// choice arms — a plain node with DependsOn("premium-price","standard-price",...)
	// would be rejected at Build as unstructured reconvergence.
	b.AddMerge("total").
		From("premium-price", "standard-price", "reject").
		WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
			price, ok := data.GetInt64(keyArmPrice)
			if !ok {
				return fmt.Errorf("total: no arm wrote %q", keyArmPrice)
			}
			data.Set(keyTotal, price)
			fmt.Printf("total (merge): reconverged final_total=%d¢\n", price)
			return nil
		})

	return workflow.FromBuilder(b)
}

// priceArm returns an arm action that prices the seeded quantity at the given unit price.
func priceArm(unitCents int64) func(context.Context, *workflow.WorkflowData) error {
	return func(_ context.Context, data *workflow.WorkflowData) error {
		qty, ok := data.GetInt64(keyQuantity)
		if !ok {
			return fmt.Errorf("price arm: %q not seeded", keyQuantity)
		}
		total := qty * unitCents
		data.Set(keyArmPrice, total)
		fmt.Printf("arm: %d units × %d¢ = %d¢\n", qty, unitCents, total)
		return nil
	}
}

// runOne builds and executes the graph for one order kind, then reads the durable result.
func runOne(kind string) error {
	store := workflow.NewInMemoryStore()
	id := "pricing-" + kind

	wf, err := buildPricingWorkflow(store, id, kind)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Every run here is a deterministic success (no injected failure), so a non-nil
	// Execute error is a real defect, not something the example teaches.
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	data, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	total, _ := data.GetInt64(keyTotal)
	fmt.Printf("result[%s]: final_total=%d¢\n\n", kind, total)
	return nil
}

func run() error {
	// Run the identical graph twice with different seed data so BOTH real arms are
	// exercised: premium is taken once, standard is taken once. In each run the two
	// arms that were not chosen end Bypassed.
	for _, kind := range []string{"premium", "standard"} {
		if err := runOne(kind); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("04-choice-and-merge: %v", err)
	}
}
