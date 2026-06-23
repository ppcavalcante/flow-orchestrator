package workflow

import (
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataPlaneMetricsBothPaths covers the C1 metrics fast-path branch on BOTH
// sides of every data-plane method. As of the C1 change (commit 66d8dcc) metrics
// default to OFF, so Set/Get/Delete/SetNodeStatus/GetNodeStatus/SetOutput/GetOutput
// each branch on metricsDisabled(): the disabled branch takes a metrics-free direct
// lock+map op, the enabled branch routes through metrics.TrackOperation. Default
// (metrics OFF) exercises the fast path; an explicitly-enabled, fully-sampled
// (rate 1.0) WorkflowData exercises the TrackOperation path. The contract under
// test: the two paths are FUNCTIONALLY IDENTICAL — metrics only observe, never
// change behavior. We assert that by running the same operation sequence against
// both and requiring byte-identical observable results.
func TestDataPlaneMetricsBothPaths(t *testing.T) {
	// newEnabled builds a WorkflowData with metrics explicitly enabled at sampling
	// rate 1.0, so metricsDisabled()==false on every op (no sampling skip) and the
	// TrackOperation arm is deterministically taken.
	newEnabled := func() *WorkflowData {
		return NewWorkflowDataWithConfig("metrics-on",
			WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)})
	}
	// newDisabled builds a default WorkflowData (post-C1 default = metrics OFF), so
	// every op takes the metrics-free fast path.
	newDisabled := func() *WorkflowData { return NewWorkflowData("metrics-off") }

	// Confirm the two seams actually exercise the two branches, so this test can't
	// silently pass while testing only one arm.
	require.False(t, newDisabled().GetMetrics().IsEnabled(),
		"disabled-data must report metrics OFF (fast path)")
	require.True(t, newEnabled().GetMetrics().IsEnabled(),
		"enabled-data must report metrics ON (TrackOperation path)")
	require.False(t, newEnabled().metricsDisabled(),
		"enabled+rate1.0 must never sample out — TrackOperation arm is taken")

	// observed captures the full observable state produced by the op sequence, so
	// the two branches can be compared for exact functional equality.
	type observed struct {
		getVal       interface{}
		getOK        bool
		getMissingOK bool
		delExisted   bool
		delMissing   bool
		afterDelOK   bool
		status       NodeStatus
		statusOK     bool
		missStatusOK bool
		output       interface{}
		outputOK     bool
		missOutOK    bool
	}
	exercise := func(d *WorkflowData) observed {
		var o observed
		// Set + Get round-trip (both the write and read fast/slow paths).
		d.Set("k", 42)
		o.getVal, o.getOK = d.Get("k")
		_, o.getMissingOK = d.Get("absent")

		// Delete: existing key -> true, then missing -> false, then Get -> gone.
		o.delExisted = d.Delete("k")
		o.delMissing = d.Delete("absent")
		_, o.afterDelOK = d.Get("k")

		// Node status set/get + missing.
		d.SetNodeStatus("n1", Completed)
		o.status, o.statusOK = d.GetNodeStatus("n1")
		_, o.missStatusOK = d.GetNodeStatus("absent")

		// Output set/get + missing.
		d.SetOutput("n1", "out-value")
		o.output, o.outputOK = d.GetOutput("n1")
		_, o.missOutOK = d.GetOutput("absent")
		return o
	}

	disabledResult := exercise(newDisabled())
	enabledResult := exercise(newEnabled())

	// Functional equivalence: the metrics-on path must observe exactly what the
	// metrics-off fast path observes. A divergence here would mean the C1 fast
	// path changed behavior, not just instrumentation.
	assert.Equal(t, disabledResult, enabledResult,
		"metrics-OFF fast path and metrics-ON TrackOperation path must be functionally identical")

	// Spot-check the concrete expected values (so the test pins behavior, not just
	// equality of two possibly-both-wrong runs).
	assert.Equal(t, 42, enabledResult.getVal)
	assert.True(t, enabledResult.getOK)
	assert.False(t, enabledResult.getMissingOK)
	assert.True(t, enabledResult.delExisted)
	assert.False(t, enabledResult.delMissing)
	assert.False(t, enabledResult.afterDelOK)
	assert.Equal(t, Completed, enabledResult.status)
	assert.True(t, enabledResult.statusOK)
	assert.False(t, enabledResult.missStatusOK)
	assert.Equal(t, "out-value", enabledResult.output)
	assert.True(t, enabledResult.outputOK)
	assert.False(t, enabledResult.missOutOK)
}

// TestDataPlaneMetricsEnabledRecordsOps confirms the metrics-ON path is not just
// functionally equivalent but actually routes through the collector — i.e. the
// TrackOperation arm really executes (guarding against a future refactor that
// accidentally takes the fast path even when enabled, which would make the
// both-paths test above pass while only exercising one branch).
func TestDataPlaneMetricsEnabledRecordsOps(t *testing.T) {
	d := NewWorkflowDataWithConfig("rec",
		WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)})

	// With metrics enabled and full sampling, metricsDisabled() must be false for
	// every op so the TrackOperation closure runs. This is the observable proof
	// the enabled arm is taken; the functional result is asserted above.
	require.True(t, d.GetMetrics().IsEnabled())
	require.False(t, d.metricsDisabled())

	d.Set("a", 1)
	v, ok := d.Get("a")
	require.True(t, ok)
	require.Equal(t, 1, v)
}
