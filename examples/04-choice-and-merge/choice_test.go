package main

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestChoiceAndMerge_RoutesOneArm_BypassesTheRest runs the exact graph the command
// builds, once per kind, and asserts the durable effect: the taken arm ran (Completed),
// the two not-taken arms ended Bypassed, the merge reconverged (Completed), and the
// published total matches the arm that won. This is the anti-rot guarantee — the example
// is actually executed, not merely compiled.
func TestChoiceAndMerge_RoutesOneArm_BypassesTheRest(t *testing.T) {
	cases := []struct {
		kind      string
		takenArm  string   // the arm expected Completed
		bypassed  []string // the arms expected Bypassed
		wantTotal int64    // 3 units × the arm's unit price
	}{
		{"premium", "premium-price", []string{"standard-price", "reject"}, 3 * premiumUnitCents},
		{"standard", "standard-price", []string{"premium-price", "reject"}, 3 * standardUnitCents},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			store := workflow.NewInMemoryStore()
			id := "pricing-" + tc.kind

			wf, err := buildPricingWorkflow(store, id, tc.kind)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if err := wf.Execute(context.Background()); err != nil {
				t.Fatalf("execute: %v", err)
			}

			data, err := store.Load(id)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			// The merge published the taken arm's total.
			if total, ok := data.GetInt64(keyTotal); !ok || total != tc.wantTotal {
				t.Errorf("final_total = %d (ok=%v), want %d", total, ok, tc.wantTotal)
			}

			// The choice, the taken arm, and the merge all reached Completed.
			for _, node := range []string{"route", tc.takenArm, "total"} {
				if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Completed {
					t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
				}
			}

			// The arms the choice did NOT pick ended Bypassed — the explicit "deliberately
			// not run" status, not Skipped and not Pending.
			for _, node := range tc.bypassed {
				if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Bypassed {
					t.Errorf("node %q status = %v (ok=%v), want Bypassed", node, st, ok)
				}
			}
		})
	}
}
