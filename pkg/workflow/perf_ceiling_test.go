package workflow

// Phase 54 §C — the RATIFIED performance ceilings, asserted as tests that FAIL if a ceiling
// is breached. Every constant is measurement-derived (§A / BASELINE.md) + architect-ratified
// (a ceiling must be ratified, not self-set). Arch: arm64 (DEC-M13-ARCH). MEASURE-not-tune:
// a breach is a finding, never a here-fix.

import (
	"context"
	"runtime/debug"
	"sync"
	"testing"
)

// det-tax: the non-durable forward hot path allocates AT MOST the frozen CEILING of 283
// (Workflow drive) / 277 (DAG drive) — the ratified arm64 numbers (DEC-M13-ARCH; frozen in
// bench_baseline_preM12_test.go). A CEILING, not a hard equality: the guard's purpose is to
// catch an ADDED alloc (the determinism tax the moat promises never to pay), so it reddens on
// any count ABOVE the ceiling. It deliberately TOLERATES a count at-or-below: amd64 reads 1
// lower (282/276) after the M14 ph60 interner right-sizing legitimately removed a hot-path
// alloc — a REDUCTION is never a determinism-tax regression, and exact-equality mis-flagged it
// as a "BREACH" on amd64 CI. arm64 (the ratified measure arch) still reads 283/277 == ceiling.
// A +1 regression on arm64 (284/278) still reddens — the moat guard is fully preserved.
func TestPerfCeiling_DetTax(t *testing.T) {
	// Skip under -race: the det-tax reading comes from testing.Benchmark().AllocsPerOp(),
	// which the race detector inflates by a fixed instrumentation overhead (~+9 allocs),
	// making the alloc count meaningless as a zero-determinism-tax measure. The guard is
	// enforced NON-race (locally + a dedicated BLOCKING non-race CI step). (raceEnabled is
	// build-tagged in race_on_test.go / race_off_test.go.)
	if raceEnabled {
		t.Skip("det-tax alloc ceiling is measured non-race; -race AllocsPerOp is instrumentation-inflated")
	}
	// Skip before Go 1.25: the absolute alloc budget (283/277) is calibrated for the
	// reference toolchain (arm64 Go 1.25.1); Go 1.24 has a different alloc baseline
	// (amd64 1.24 reads 288/282) — a per-toolchain artifact, not our determinism tax. A
	// real regression from our code adds allocs on EVERY toolchain, so enforcing on 1.25
	// still catches it. (detTaxToolchainCalibrated is build-tagged in go125_on/off_test.go.)
	if !detTaxToolchainCalibrated {
		t.Skip("det-tax absolute alloc budget is calibrated for Go 1.25+ (arm64 283/277); an older toolchain has a different baseline — a real regression still shows on 1.25")
	}

	// Pin GC high for the whole measurement window. AllocsPerOp counts a per-drive sync.Pool
	// MISS as an allocation; under GC pressure — a loaded CI runner — the pool is evicted
	// BETWEEN benchmark iterations, adding a spurious +1..+3 the ceiling was never calibrated
	// for (the ceiling is the quiescent / GOGC=800 reading; amd64 unloaded reads even 1 LOWER).
	// This is the documented det-tax load-sensitivity: measuring at a raised GC percent (the
	// "prove innocence with GOGC=800" condition) makes the count deterministic regardless of
	// runner load, so the gate reflects the ENGINE's allocation, not the runner's. A real +1
	// regression still shows on top of the true baseline and reddens. (CI det-tax flake, 2026-08-12.)
	defer debug.SetGCPercent(debug.SetGCPercent(800))

	d := benchDiamondDAG(t)
	ctx := context.Background()

	// Measure the SAME WAY the frozen bench does (testing.Benchmark → AllocsPerOp), so the
	// numbers match bench_baseline_preM12_test.go's 283/277 (AllocsPerRun warms away the
	// first-call lazy-init allocs and would read 280/274 — a different measurement).
	w := &Workflow{dag: d, WorkflowID: "det-tax"}
	wr := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErrSink = w.Execute(ctx)
		}
	})
	// RE-RATIFIED 283 -> 284 (M24 DEC-M24-MEDIATION, user-approved 2026-08-11). The
	// complete-mediation sealed view shares the run's lock, which required WorkflowData.mu
	// to become a *sync.RWMutex; on the Workflow drive that is exactly +1 alloc/op. This is
	// the MEASURED cost of a deliberate feature recorded transparently, NOT a silent tune to
	// hide a regression: the DAG drive is unchanged (277 below), the sealed view itself is
	// allocation-free (channel freelist), and the zero-REPLAY-tax moat is untouched. A real
	// regression still adds allocs on TOP of this and reddens here.
	if got := wr.AllocsPerOp(); got > 284 {
		t.Errorf("det-tax BREACH: Workflow non-durable drive = %d allocs/op, want <= 284 (arm64 ceiling; 283 + 1 for M24 mediation's shared *sync.RWMutex)", got)
	}

	dr := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErrSink = d.Execute(ctx, NewWorkflowData("wf"))
		}
	})
	if got := dr.AllocsPerOp(); got > 277 {
		t.Errorf("det-tax BREACH: DAG non-durable drive = %d allocs/op, want <= 277 (frozen arm64 ceiling)", got)
	}
}

// not-superlinear (NON-DURABLE): across 10x more nodes the forward cost grows <= 12x — near-
// linear, NOT superlinear (an ideal O(N) executor is ~10x for 10x nodes). Measured on the WIDE
// series — the HIGHEST-ratio non-durable shape (§A wide 8.0x vs deep 7.1x), i.e. the BINDING
// constraint for this ceiling. An O(n^2) regression (a scan over all nodes per node) pushes
// cost(1000)/cost(100) toward ~100x and reddens this (proven: injected scan → 27.7x). Time-
// based with a generous, ratified bound: only >=~O(N^2) super-linearity is in scope (a mild
// O(N^1.1) drift can pass under 12x — a deliberate coverage limit, review ph54-F2).
func TestPerfCeiling_NotSuperlinearNonDurable(t *testing.T) {
	ctx := context.Background()
	nsPerOp := func(dag *DAG) float64 {
		r := testing.Benchmark(func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := &Workflow{dag: dag, WorkflowID: "ns"}
				perfSink = w.Execute(ctx)
			}
		})
		return float64(r.NsPerOp())
	}
	ratio := nsPerOp(buildWide(1000)) / nsPerOp(buildWide(100))
	const ceiling = 12.0
	if ratio > ceiling {
		t.Errorf("not-superlinear BREACH: non-durable wide cost(1000)/cost(100) = %.1fx, want <= %.0fx (10x nodes)", ratio, ceiling)
	}
	t.Logf("not-superlinear: non-durable wide cost(1000)/cost(100) = %.1fx (ceiling %.0fx)", ratio, ceiling)
}

// wide-durability (single-level/bounded-fan-out shape — RATIFIED option-a): the run
// checkpoints EXACTLY once per level and no more — the durability WORK is the minimum, no
// redundant checkpointing. buildWide is 2 levels (root + the fan-out) → exactly 2 checkpoints;
// a doubled-checkpoint regression → 4 → reddens. This is the deterministic, non-flaky form of
// "wide/single-level durability overhead is bounded" (measured ~35 ms fixed on arm64, one FB
// atomic write per level). The DEEP/multi-level O(N^2) is DELIBERATELY NOT ceiling'd — it is
// finding F-M13-P54-1 (a blocker-candidate), not a blessed ceiling.
func TestPerfCeiling_WideDurabilityBounded(t *testing.T) {
	cc := &countingCheckpointer{InMemoryStore: NewInMemoryStore()}
	w := &Workflow{dag: buildWide(100), WorkflowID: "wide-dur", Store: cc}
	if err := w.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	const wantLevels = 2 // root level + the fan-out level
	if got := cc.count(); got != wantLevels {
		t.Errorf("wide-durability BREACH: %d checkpoints for a %d-level wide workflow, want EXACTLY %d (one per level; a doubled checkpoint = 2x durability overhead)", got, wantLevels, wantLevels)
	}
}

// countingCheckpointer wraps an InMemoryStore and counts SaveCheckpoint calls.
type countingCheckpointer struct {
	*InMemoryStore
	mu sync.Mutex
	n  int
}

func (c *countingCheckpointer) SaveCheckpoint(d *WorkflowData) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.InMemoryStore.SaveCheckpoint(d)
}

func (c *countingCheckpointer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
