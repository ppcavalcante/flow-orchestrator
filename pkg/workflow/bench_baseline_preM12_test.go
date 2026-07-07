package workflow

import (
	"context"
	"testing"
)

// BenchmarkWorkflowExecutePreM12NonSaga freezes the pre-M12 Workflow-drive hot
// path — executeLocked's forward span (the future rollback trigger + forward-vs-
// rollback switch land HERE) — over a non-saga diamond with NO compensation
// declared. This is phase 49's VER-03 comparison point (DEC-M12-BENCH): a run
// with no compensation MUST stay byte-for-byte this path (the trigger/switch
// branches are inert). Store=nil isolates the executor + the branch checks from
// I/O; reuse the same *Workflow across iterations (no state persists with a nil
// Store, so every Execute is a fresh forward drive). Reuses benchDiamondDAG /
// benchErrSink from the M11 baseline harness (same package).
//
// FROZEN BASELINE (see BENCH-BASELINE-preM12.txt for the committed numbers +
// the benchstat-ready command).
func BenchmarkWorkflowExecutePreM12NonSaga(b *testing.B) {
	d := benchDiamondDAG(b)
	w := &Workflow{DAG: d, WorkflowID: "bench-presaga-nonsaga"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchErrSink = w.Execute(ctx)
	}
}

// BenchmarkDAGExecutePreM12NonSaga mirrors the M11 BenchmarkExecuteNonChoice
// (DAG-level, no Workflow drive) so the M12 baseline is directly comparable to
// the M11 committed number on the same diamond shape.
func BenchmarkDAGExecutePreM12NonSaga(b *testing.B) {
	d := benchDiamondDAG(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchErrSink = d.Execute(ctx, NewWorkflowData("wf"))
	}
}
