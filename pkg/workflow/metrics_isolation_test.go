package workflow

import (
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// Chunk 4 (de-globalize metrics): these tests PIN that metrics state is
// per-WorkflowData-instance, with no process-global cross-talk. They are the
// regression guard for the removal of the package-level global facade.

// opCount reads the recorded Count for an operation from a collector, 0 if absent.
func opCount(c *metrics.MetricsCollector, op metrics.OperationType) int64 {
	if c == nil {
		return 0
	}
	stats, ok := c.GetOperationStats(op)
	if !ok {
		return 0
	}
	return stats.Count
}

// TestMetrics_TwoInstanceIsolation is the discriminating isolation test: two
// WorkflowData instances with independent metrics configs must not share state.
// Instance A is metrics-ON, instance B is metrics-OFF. Operations on A must NOT
// affect B's collector and vice-versa — no global sink, no cross-talk.
func TestMetrics_TwoInstanceIsolation(t *testing.T) {
	cfgOn := DefaultWorkflowDataConfig().WithMetricsConfig(metrics.NewConfig().WithEnabled(true)) // explicitly enabled
	cfgOff := DefaultWorkflowDataConfig().WithMetricsConfig(metrics.DisabledMetricsConfig())

	a := NewWorkflowDataWithConfig("inst-A", cfgOn)
	b := NewWorkflowDataWithConfig("inst-B", cfgOff)

	// Drive ONLY instance A.
	for i := 0; i < 10; i++ {
		a.Set("k", i)
		_, _ = a.Get("k")
	}

	aSet := opCount(a.GetMetrics(), metrics.OpSet)
	if aSet == 0 {
		t.Fatal("instance A (metrics ON) recorded no OpSet — its own collector should have counted")
	}

	// Instance B was never touched AND is metrics-off: its collector must be
	// pristine. A's activity must not have bled into B.
	if bSet := opCount(b.GetMetrics(), metrics.OpSet); bSet != 0 {
		t.Fatalf("instance B OpSet = %d, want 0 — A's metrics leaked into B (cross-talk)", bSet)
	}

	// The collectors must be distinct objects.
	if a.GetMetrics() == b.GetMetrics() {
		t.Fatal("instances A and B share the same *MetricsCollector — not per-instance")
	}

	// Now drive ONLY instance B (it is metrics-off, so it records nothing) and
	// confirm A's count is unchanged by B's activity.
	aSetBefore := opCount(a.GetMetrics(), metrics.OpSet)
	for i := 0; i < 5; i++ {
		b.Set("k", i)
	}
	if bSet := opCount(b.GetMetrics(), metrics.OpSet); bSet != 0 {
		t.Fatalf("instance B (metrics OFF) recorded OpSet = %d, want 0", bSet)
	}
	if aSetAfter := opCount(a.GetMetrics(), metrics.OpSet); aSetAfter != aSetBefore {
		t.Fatalf("instance A OpSet changed from %d to %d after driving B — B leaked into A", aSetBefore, aSetAfter)
	}
}

// TestMetrics_BothEnabledIndependentCounts: two metrics-ON instances accumulate
// independent counts — driving A N times and B M times must leave A=N, B=M.
func TestMetrics_BothEnabledIndependentCounts(t *testing.T) {
	cfg := func() WorkflowDataConfig {
		return DefaultWorkflowDataConfig().WithMetricsConfig(metrics.NewConfig().WithEnabled(true))
	}
	a := NewWorkflowDataWithConfig("A", cfg())
	b := NewWorkflowDataWithConfig("B", cfg())

	const na, nb = 7, 3
	for i := 0; i < na; i++ {
		a.Set("k", i)
	}
	for i := 0; i < nb; i++ {
		b.Set("k", i)
	}

	if got := opCount(a.GetMetrics(), metrics.OpSet); got != na {
		t.Fatalf("A OpSet = %d, want %d", got, na)
	}
	if got := opCount(b.GetMetrics(), metrics.OpSet); got != nb {
		t.Fatalf("B OpSet = %d, want %d (B must not have absorbed A's %d)", got, nb, na)
	}
}

// TestMetrics_FastPathNoRecordWhenOff is the Phase-A fast-path guard: a
// metrics-OFF instance records nothing (metricsDisabled short-circuits before
// any TrackOperation), and reads/writes still function.
func TestMetrics_FastPathNoRecordWhenOff(t *testing.T) {
	cfgOff := DefaultWorkflowDataConfig().WithMetricsConfig(metrics.DisabledMetricsConfig())
	d := NewWorkflowDataWithConfig("off", cfgOff)

	d.Set("k", 42)
	got, ok := d.Get("k")
	if !ok || got != 42 {
		t.Fatalf("Get after Set on metrics-off instance = (%v,%v), want (42,true) — data plane must work regardless of metrics", got, ok)
	}
	// Metrics off => nothing recorded.
	if c := opCount(d.GetMetrics(), metrics.OpSet); c != 0 {
		t.Fatalf("metrics-off instance recorded OpSet = %d, want 0 (fast-path must short-circuit)", c)
	}
	if d.GetMetrics().IsEnabled() {
		t.Fatal("disabled-config instance reports metrics enabled")
	}
}
