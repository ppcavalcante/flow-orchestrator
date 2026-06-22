package metrics_test

// OBS-01 zero-overhead benchmark (Phase 17 / M6).
//
// The substantive claim is true by construction: NewOTelBridge only registers
// an OBSERVABLE callback that fires on the host SDK's COLLECTION path. It adds
// ZERO code to the operation-recording hot path (MetricsCollector.TrackOperation).
// These benchmarks confirm it: the per-operation cost of TrackOperation is
// statistically identical whether or not a bridge is registered.
//
// The apples-to-apples library-level baseline for the full WorkflowData hot path
// is BenchmarkWorkflowDataSetGet in internal/workflow/benchmark/ (Set+Get go
// through w.metrics.TrackOperation). The SUMMARY records before/after for that
// baseline; the bridge is additive-only on the cold path, so the two are
// indistinguishable. The benchmarks here isolate the same TrackOperation hot
// path within the metrics package so the no-overhead property is directly
// measurable with and without a registered bridge.

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// BenchmarkTrackOperation_NoBridge measures the recording hot path with no OTel
// bridge present at all (the "unwired" / today's behavior).
func BenchmarkTrackOperation_NoBridge(b *testing.B) {
	c := metrics.NewMetricsCollector()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.TrackOperation(metrics.OpGet, func() {})
	}
}

// BenchmarkTrackOperation_WithBridge measures the SAME recording hot path while
// an OTel bridge is registered over the collector (but never collected). It must
// be statistically identical to the NoBridge case — the bridge touches nothing
// on TrackOperation; its callback only fires on collection.
func BenchmarkTrackOperation_WithBridge(b *testing.B) {
	c := metrics.NewMetricsCollector()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	bridge, err := metrics.NewOTelBridge(c, mp)
	if err != nil {
		b.Fatalf("NewOTelBridge: %v", err)
	}
	defer func() {
		if err := bridge.Shutdown(context.Background()); err != nil {
			b.Errorf("bridge Shutdown: %v", err)
		}
		if err := mp.Shutdown(context.Background()); err != nil {
			b.Errorf("MeterProvider Shutdown: %v", err)
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.TrackOperation(metrics.OpGet, func() {})
	}
}
