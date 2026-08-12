package main

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestDynamicFanOut_AllBranchEffectsLandExactlyOnce runs the exact graph the command
// builds and asserts the durable per-branch effect: the count key equals N, every
// branch result base[i] is present exactly once with its expected typed value, and the
// fan-out node itself reached Completed. The anti-rot guarantee — actually executed.
func TestDynamicFanOut_AllBranchEffectsLandExactlyOnce(t *testing.T) {
	const id = "fanout-doc"
	const n = 5

	store := workflow.NewInMemoryStore()
	wf, err := buildFanOutWorkflow(store, id, n)
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

	// The collected count matches the runtime-discovered width.
	if count, ok := data.GetInt64(fanOutCountKey(keyResults)); !ok || count != n {
		t.Fatalf("%s = %d (ok=%v), want %d", fanOutCountKey(keyResults), count, ok, n)
	}

	// Each branch landed EXACTLY ONCE: base[i] present, typed int64, equal to (i+1)*100.
	// A missing index (branch dropped) or a doubled effect (a lossy re-run) fails here.
	for i := 0; i < n; i++ {
		want := int64((i + 1) * 100)
		got, ok := data.GetInt64(fanOutIndexKey(keyResults, i))
		if !ok {
			t.Errorf("branch result %s missing", fanOutIndexKey(keyResults, i))
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", fanOutIndexKey(keyResults, i), got, want)
		}
	}

	// The fan-out node completed once every branch was collected.
	if st, ok := data.GetNodeStatus("render"); !ok || st != workflow.Completed {
		t.Errorf("node %q status = %v (ok=%v), want Completed", "render", st, ok)
	}
}
