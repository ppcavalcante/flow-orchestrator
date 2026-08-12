package workflow

import (
	"context"
	"runtime/debug"
	"testing"
)

// det-tax GC-independence (ROOT-CAUSE regression, 2026-08-12). The non-durable forward
// drive must allocate a CONSTANT count regardless of GC pressure. The load-flake that
// reddened CI (285/278 under a loaded runner, 284/277 quiescent) was NOT a runner
// artifact: DAG.Execute built its per-level current-level key/value with fmt.Sprintf,
// and fmt recycles its printer structs through an internal sync.Pool (ppFree) that GC
// drains on EVERY cycle — so each per-level fmt call paid a spurious +alloc/op under GC
// pressure. The fix (dag.go) hoists the invariant key out of the loop and builds both
// strings by concat/strconv (no pool), making the drive GC-independent.
//
// This test measures each drive at a HIGH GC percent (quiescent — the pool is never
// drained) and at a LOW GC percent (pressured — the pool drains constantly) and asserts
// the pressured count does NOT exceed the quiescent count. It reproduces the bug: on the
// pre-fix code the DAG drive read ~276 quiescent vs ~280 pressured (FAIL); post-fix it
// reads the same constant at both (PASS). This is the direct root-cause guard, distinct
// from TestPerfCeiling_DetTax (which pins GC to make the ABSOLUTE ceiling deterministic
// but does not itself prove GC-independence).
func TestDetTax_GCIndependent(t *testing.T) {
	if raceEnabled {
		t.Skip("AllocsPerOp is instrumentation-inflated under -race; GC-independence is measured non-race")
	}

	d := benchDiamondDAG(t)
	ctx := context.Background()

	// allocsAtGC measures a drive's AllocsPerOp with GC pinned at the given percent for
	// the whole window, restoring the prior setting after.
	allocsAtGC := func(gcPercent int, drive func()) int64 {
		old := debug.SetGCPercent(gcPercent)
		defer debug.SetGCPercent(old)
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drive()
			}
		}).AllocsPerOp()
	}

	const quiescentGC = 800 // pool never drained
	const pressuredGC = 10  // pool drained on nearly every iteration

	// Workflow (no Store) non-durable drive.
	w := &Workflow{dag: d, WorkflowID: "det-tax-gcindep"}
	wQuiet := allocsAtGC(quiescentGC, func() { benchErrSink = w.Execute(ctx) })
	wPressed := allocsAtGC(pressuredGC, func() { benchErrSink = w.Execute(ctx) })
	if wPressed > wQuiet {
		t.Errorf("Workflow drive is GC-sensitive: %d allocs/op under GC pressure (GOGC=%d) vs %d quiescent (GOGC=%d); a pooled per-drive alloc is being drained by GC — the non-durable drive must be GC-independent",
			wPressed, pressuredGC, wQuiet, quiescentGC)
	}

	// DAG non-durable drive.
	dQuiet := allocsAtGC(quiescentGC, func() { benchErrSink = d.Execute(ctx, NewWorkflowData("wf")) })
	dPressed := allocsAtGC(pressuredGC, func() { benchErrSink = d.Execute(ctx, NewWorkflowData("wf")) })
	if dPressed > dQuiet {
		t.Errorf("DAG drive is GC-sensitive: %d allocs/op under GC pressure (GOGC=%d) vs %d quiescent (GOGC=%d); a pooled per-drive alloc is being drained by GC — the non-durable drive must be GC-independent",
			dPressed, pressuredGC, dQuiet, quiescentGC)
	}
}
