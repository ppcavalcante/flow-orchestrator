package workflow

// M14 ph60 (REM-01 inmem + REM-02) — clone-metrics co-design bites. Each bite
// must be able to REDDEN a real regression (not a tautology). The bar-#1 alloc
// drop and bar-#3 det-tax (283/277) live in the existing perf benches
// (perf_baseline_bench_test.go / perf_ceiling_test.go) and are cited in SUMMARY;
// these are the correctness + N1 bites the perf benches don't cover.

import (
	"bytes"
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"github.com/stretchr/testify/require"
)

// Bite #2 — Snapshot() bytes are BYTE-IDENTICAL before and after a Clone(). The
// clone-interner cheapening must not perturb serialized state (⇒ the durable
// format, gopter suite, and ph55 goldens hold by construction). Reddens if the
// clone ever changed a persisted field.
func TestPh60_Bite2_SnapshotByteIdenticalAcrossClone(t *testing.T) {
	src := NewWorkflowData("snap-identity")
	src.Set("region", "eu-west")
	src.Set("count", int64(9007199254740993)) // 2^53+1 — int64 fidelity path
	src.SetNodeStatus("n1", Completed)
	src.SetOutput("n1", map[string]any{"ok": true})

	before, err := src.Snapshot()
	require.NoError(t, err)

	clone := src.Clone()
	cloneBytes, err := clone.Snapshot()
	require.NoError(t, err)

	// The clone's snapshot must equal the source's — the cheapening touched only
	// the interner ALLOCATION, never the state it holds.
	require.True(t, bytes.Equal(before, cloneBytes),
		"clone Snapshot() must be byte-identical to source (persisted state untouched)")

	// And the source's own snapshot is unchanged after cloning (no mutation).
	after, err := src.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after), "source Snapshot() unchanged by Clone()")
}

// Bite #4 — first metrics op immediately after a Clone() does NOT nil-panic and
// real stats accrue. Guards the never-nil w.metrics invariant the cheapening
// touches (the clone now builds its collector from the source config, not a bare
// enabled one). Uses an ENABLED source so the clone's collector is enabled.
func TestPh60_Bite4_FirstUseAfterCloneNoPanic(t *testing.T) {
	cfg := metrics.NewConfig().WithEnabled(true)
	src := NewWorkflowDataWithConfig("enabled-src", DefaultWorkflowDataConfig().WithMetricsConfig(cfg))
	require.True(t, src.GetMetrics().IsEnabled(), "source collector enabled")

	clone := src.Clone()
	// N1: the enabled state SURVIVES the clone (not silently disabled).
	require.True(t, clone.GetMetrics().IsEnabled(), "enabled state survives Clone() (N1)")

	// First-use after clone: a metrics-tracked op must not nil-panic and must accrue.
	require.NotPanics(t, func() {
		clone.Set("k", "v")
		clone.SetNodeStatus("n", Completed)
		if _, err := clone.Snapshot(); err != nil {
			t.Errorf("snapshot after clone errored: %v", err)
		}
	}, "first metrics op after Clone() must not nil-panic")

	stats := clone.GetMetrics().GetAllOperationStats()
	var total int64
	for _, s := range stats {
		total += s.Count
	}
	require.Greater(t, total, int64(0), "real op-stats accrue on the enabled clone after first use")
}

// Bite (quirk fix) — a clone of a DISABLED source must NOT become enabled. The
// prior Clone() always built a bare NewMetricsCollector() (Enabled=true), so a
// disabled run's clone silently flipped enabled. Reddens if the quirk returns.
func TestPh60_DisabledSourceCloneStaysDisabled(t *testing.T) {
	src := NewWorkflowData("disabled-default") // default = metrics disabled
	require.False(t, src.GetMetrics().IsEnabled(), "default source is metrics-disabled")

	clone := src.Clone()
	require.False(t, clone.GetMetrics().IsEnabled(),
		"clone of a disabled source must stay disabled (no bare-enabled-collector quirk)")
}

// Bite #5 — resume-carries-enabled-metrics: a checkpoint→resume on the ENABLED
// path (InMemoryStore, in-process — metrics are not serialized, so this is the
// only path that can carry them) reads NON-ZERO op-stats after the resumed run,
// not a silently zeroed collector. This is the N1 invariant as a test.
func TestPh60_Bite5_ResumeCarriesEnabledMetrics(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("resume-metrics")
	b.AddStartNode("a").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("a", "done")
		return nil
	}))
	b.AddNode("z").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("z", "done")
		return nil
	})).DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)

	store := NewInMemoryStore()
	wf := &Workflow{
		DAG:           dag,
		WorkflowID:    "resume-metrics",
		Store:         store,
		MetricsConfig: metrics.NewConfig().WithEnabled(true),
	}
	require.NoError(t, wf.Execute(context.Background()))

	// A second Execute resumes from the checkpointed (InMemoryStore.Clone'd) state.
	// The enabled collector must survive the checkpoint clone → the resumed run's
	// GetMetrics reads real (non-zero) op-stats, not a zeroed collector.
	require.NoError(t, wf.Execute(context.Background()))

	stats := wf.GetMetrics().GetAllOperationStats()
	var total int64
	for _, s := range stats {
		total += s.Count
	}
	require.Greater(t, total, int64(0),
		"resumed enabled run reads non-zero op-stats (enabled collector survived the checkpoint clone — N1)")
}

// Bite #6 — public-API enable example (ph52 dogfood pattern): a consumer enables
// metrics through the PUBLIC Workflow API, reads non-zero op-stats after a real
// Execute, and can export via the OTel bridge. Pre-hook the consumer could not
// reach the stats at all (Execute built the data internally with no read-back).
func TestPh60_Bite6_PublicAPIEnableExample(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("public-metrics")
	b.AddStartNode("start").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("processed", int64(1))
		d.SetOutput("start", "ok")
		return nil
	}))
	dag, err := b.Build()
	require.NoError(t, err)

	// PUBLIC enable-hook: set MetricsConfig on the Workflow.
	wf := &Workflow{
		DAG:           dag,
		WorkflowID:    "public-metrics",
		MetricsConfig: metrics.ProductionConfig().WithEnabled(true).WithSamplingRate(1.0),
	}
	require.NoError(t, wf.Execute(context.Background()))

	// Read-back through the public accessor.
	mc := wf.GetMetrics()
	require.NotNil(t, mc, "GetMetrics is reachable after Execute (pre-hook: unreachable)")
	require.True(t, mc.IsEnabled(), "metrics enabled via the public hook")
	stats := mc.GetAllOperationStats()
	var total int64
	for _, s := range stats {
		total += s.Count
	}
	require.Greater(t, total, int64(0), "consumer reads non-zero op-stats after a real Execute")
}
