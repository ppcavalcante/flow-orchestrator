package workflow

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"github.com/stretchr/testify/require"
)

// AUD-016 / P-05: a workflow with enabled MetricsConfig produced an enabled
// collector on its first (JSON/file-backed) run and a DISABLED one on resume —
// file stores do not persist metrics config, and the loaded WorkflowData replaced
// the preconfigured (enabled) object. Metrics config belongs to the Runner
// (Workflow.MetricsConfig); it must be re-attached after Load. InMemory happened
// to preserve it through Clone, so tests that only covered InMemory missed this.
func TestAUD016_MetricsStayEnabledOnFileResume(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	build := func() *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID("wf")
		b.AddStartNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		w, err := FromBuilder(b)
		require.NoError(t, err)
		w.Store = store
		w.MetricsConfig = metrics.NewConfig().WithEnabled(true).WithSamplingRate(1.0)
		return w
	}

	// First run persists state; metrics enabled.
	w1 := build()
	require.NoError(t, w1.Execute(context.Background()))
	require.True(t, w1.GetMetrics().IsEnabled(), "first run: metrics must be enabled")

	// Resume onto the file-backed state: metrics MUST stay enabled.
	w2 := build()
	require.NoError(t, w2.Execute(context.Background()))
	require.True(t, w2.GetMetrics().IsEnabled(),
		"resume: an enabled workflow must not silently resume with metrics OFF (AUD-016)")
}
