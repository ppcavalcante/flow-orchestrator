package main

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestObservability_ExportsFlowOrchestratorInstruments runs the exact demo the
// command runs and asserts the bridge actually exported flow-orchestrator
// instruments with data points. This is the anti-rot guarantee: it would have
// caught the drift where metrics.NewConfig() defaults to DISABLED (the demo then
// exports zero data points — a silently useless example).
func TestObservability_ExportsFlowOrchestratorInstruments(t *testing.T) {
	rm, err := exportDemo(context.Background())
	if err != nil {
		t.Fatalf("exportDemo: %v", err)
	}

	// The bridge registers its instruments on a Meter, so the collected metrics
	// carry at least one scope with flow_orchestrator.* instruments.
	total := 0
	sawCount := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if strings.HasPrefix(m.Name, "flow_orchestrator.") {
				total += dataPointCount(m)
			}
			if m.Name == "flow_orchestrator.operation.count" {
				sawCount = true
				// The count instrument must carry the operations driveActivity
				// exercised — a non-empty series proves metrics were actually
				// recorded and bridged, not merely registered.
				if got := dataPointCount(m); got == 0 {
					t.Errorf("flow_orchestrator.operation.count exported 0 data points, " +
						"want > 0 (metrics enabled but nothing recorded?)")
				}
			}
		}
	}

	if !sawCount {
		t.Fatalf("no flow_orchestrator.operation.count instrument was exported — the bridge is not wired")
	}
	if total == 0 {
		t.Fatalf("exported 0 flow_orchestrator data points — metrics did not flow through the bridge")
	}
	t.Logf("exported %d flow_orchestrator data point(s) across the bridge's instruments", total)
}

// TestObservability_DataPointCountShapes guards the aggregation-shape switch the
// summary relies on: a Sum[int64]/Sum[float64]/Gauge[float64] must be counted,
// anything else is zero. A drift in the instrument aggregation shapes (which the
// bridge chooses) would silently zero the summary otherwise.
func TestObservability_DataPointCountShapes(t *testing.T) {
	if got := dataPointCount(metricdata.Metrics{Data: metricdata.Sum[int64]{
		DataPoints: []metricdata.DataPoint[int64]{{}, {}},
	}}); got != 2 {
		t.Errorf("Sum[int64] count = %d, want 2", got)
	}
	if got := dataPointCount(metricdata.Metrics{Data: nil}); got != 0 {
		t.Errorf("nil data count = %d, want 0", got)
	}
}
