package workflow

import (
	"context"
	"testing"
)

// benchNoopAction is a trivial always-succeeds action for hot-path benchmarks.
// It touches no WorkflowData so the benchmark measures the executor's
// dependency-resolution + scheduling cost, not action work.
func benchNoopAction() Action {
	return ActionFunc(func(context.Context, *WorkflowData) error { return nil })
}

// benchDiamondDAG builds a representative non-choice diamond:
//
//	root -> {a, b, c, d} -> sink
//
// The sink depends on all four middle nodes, so its launch decision walks the
// full DependsOn set through the cause-aware gate loop — the exact path the M11
// classifier restructures. This is the pre-M11 baseline shape (VER-03, phase 44
// compares ±2% against the committed number here).
func benchDiamondDAG(tb testing.TB) *DAG {
	tb.Helper()
	d := NewDAG("bench-diamond")
	if err := d.AddNode(NewNode("root", benchNoopAction())); err != nil {
		tb.Fatalf("AddNode(root): %v", err)
	}
	mids := []string{"a", "b", "c", "d"}
	for _, n := range mids {
		if err := d.AddNode(NewNode(n, benchNoopAction())); err != nil {
			tb.Fatalf("AddNode(%s): %v", n, err)
		}
		if err := d.AddDependency("root", n); err != nil {
			tb.Fatalf("AddDependency(root->%s): %v", n, err)
		}
	}
	if err := d.AddNode(NewNode("sink", benchNoopAction())); err != nil {
		tb.Fatalf("AddNode(sink): %v", err)
	}
	for _, n := range mids {
		if err := d.AddDependency(n, "sink"); err != nil {
			tb.Fatalf("AddDependency(%s->sink): %v", n, err)
		}
	}
	return d
}

// BenchmarkExecuteNonChoice is the pre-M11 hot-path baseline over a non-choice
// diamond DAG. Frozen before any Bypassed/ChoiceNode change (D-12, phase 41 T0)
// so phase 44 (VER-03) has a committed point of comparison for the ±2% moat
// budget. No ChoiceNode, no Bypassed — pure AND-gate throughput.
func BenchmarkExecuteNonChoice(b *testing.B) {
	d := benchDiamondDAG(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchErrSink = d.Execute(ctx, NewWorkflowData("wf"))
	}
}
