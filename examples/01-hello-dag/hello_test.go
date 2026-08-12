package main

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestHelloDAG_ProducesConfirmedTotal runs the exact graph the command builds and asserts
// its durable result. This is the anti-rot guarantee: the example cannot compile-but-break,
// because `go test ./examples/...` actually executes it and checks the effect.
func TestHelloDAG_ProducesConfirmedTotal(t *testing.T) {
	store := workflow.NewInMemoryStore()

	wf, err := buildOrderWorkflow(store)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, err := store.Load("hello-order")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// 3 items × 1299¢ = 3897¢, computed by the price node from validate's write.
	if total, ok := data.GetInt64(keyTotal); !ok || total != 3897 {
		t.Errorf("total = %d (ok=%v), want 3897", total, ok)
	}
	if confirmed, ok := data.GetBool(keyConfirmed); !ok || !confirmed {
		t.Errorf("confirmed = %v (ok=%v), want true", confirmed, ok)
	}

	// Every node reached Completed — the chain ran end to end, nothing was skipped.
	for _, node := range []string{"validate", "price", "confirm"} {
		if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
}
