package workflow

// M14 ph61/ph64 (F-M14-P61-QA-1) — the group-commit win, CI-re-runnable. Strict
// (every-level fsync = the pre-M14 durable contract) vs Batched(64) (fsync every 64
// levels — group-commit) over the REAL FlatBuffersStore + Execute path at deep-1000.
// ph64 measured Strict 10.58s → Batched(64) 0.248s ≈ 43-48× (hardware-varying; the
// fsync-count reduction is the invariant). Run:
//   go test -bench 'BenchmarkGroupCommit' -benchtime=3x -run '^$' ./pkg/workflow
// This replaces the ph61 throwaway harness so the win is guarded, not one-off.

import (
	"context"
	"testing"
)

func benchGroupCommit(b *testing.B, mode DurabilityOption) {
	dag := buildDeep(1000) // the deep-1000 durable cost center (shared perf builder)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		store, err := NewFlatBuffersStore(dir, WithDurabilityMode(mode))
		if err != nil {
			b.Fatal(err)
		}
		wf := &Workflow{DAG: dag, WorkflowID: "gcbench", Store: store}
		perfSink = wf.Execute(context.Background())
	}
}

// BenchmarkGroupCommit_Deep1000_Strict is the pre-M14 baseline: every level fsync'd.
func BenchmarkGroupCommit_Deep1000_Strict(b *testing.B) { benchGroupCommit(b, Strict()) }

// BenchmarkGroupCommit_Deep1000_Batched64 is group-commit: fsync every 64 levels. The
// deep-durable time drops ~an order of magnitude vs Strict (the fsync count, not the
// serialized bytes, is the cost — DEC-M14-GROUPCOMMIT).
func BenchmarkGroupCommit_Deep1000_Batched64(b *testing.B) { benchGroupCommit(b, Batched(64)) }
