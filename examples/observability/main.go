// Command observability is a runnable, self-contained example of wiring the
// flow-orchestrator OTel metrics bridge to a real OpenTelemetry SDK.
//
// It demonstrates the library's API-only observability contract:
//
//   - The LIBRARY depends on the OpenTelemetry metrics *API*
//     (go.opentelemetry.io/otel/metric) only. It never imports the SDK, never
//     starts a /metrics server, and never opens a network connection.
//   - The HOST (this program) owns everything else: it constructs the SDK
//     MeterProvider, chooses the reader/exporter, and controls the collection
//     and shutdown lifecycle.
//
// This example uses a deterministic, network-free pipeline so it is safe to run
// in CI: a ManualReader (the host triggers collection explicitly) feeding a
// stdoutmetric exporter (the metrics are printed to stdout as JSON). In
// production you would typically swap the ManualReader+stdout for a
// PeriodicReader feeding an OTLP exporter pointed at your collector — see the
// observability guide (docs/guides/observability.md). The bridge code is
// identical regardless of which reader/exporter the host picks.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"

	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("observability example: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	// --- HOST OWNS THE SDK ---------------------------------------------------
	// The host builds the exporter and the MeterProvider. The library is not
	// involved in any of this: it only ever sees the metric.MeterProvider
	// interface handed to NewOTelBridge below.
	//
	// stdoutmetric writes collected metrics to stdout as JSON — deterministic
	// and offline, ideal for an example/CI. WithPrettyPrint keeps the output
	// readable. A ManualReader means WE decide exactly when collection happens
	// (rdr.Collect below), so the demo's output is reproducible.
	exporter, err := stdoutmetric.New(stdoutmetric.WithPrettyPrint())
	if err != nil {
		return fmt.Errorf("create stdout exporter: %w", err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
	)
	// The host owns the MeterProvider lifecycle. Shutting it down flushes and
	// releases the reader/exporter. (We also Shutdown the bridge below; the two
	// are independent — the bridge only unregisters its callback.)
	defer func() {
		if sErr := mp.Shutdown(ctx); sErr != nil {
			log.Printf("meter provider shutdown: %v", sErr)
		}
	}()

	// --- LIBRARY USAGE (METRICS ENABLED) -------------------------------------
	// Enable the library's existing in-memory metrics collection on a
	// WorkflowData. This is the normal, SDK-free way to turn metrics on:
	// WithMetricsConfig accepts a *metrics.Config, no OTel types involved.
	cfg := workflow.DefaultWorkflowDataConfig().
		WithMetricsConfig(metrics.NewConfig()) // enabled, sampling rate 1.0
	wd := workflow.NewWorkflowDataWithConfig("observability-demo", cfg)

	// Drive some real workflow-data activity. Every Set/Get/SetStatus/... call
	// records into the WorkflowData's metrics collector. We exercise a few
	// operation types so the exported telemetry has more than one series.
	driveActivity(wd)

	// --- BRIDGE THE LIBRARY METRICS TO OTEL ----------------------------------
	// GetMetrics() returns the WorkflowData's own collector — the exact set of
	// aggregates we just populated. NewOTelBridge registers observable
	// instruments on a Meter from the host's MeterProvider; it adds ZERO cost
	// to the workflow hot path (the callback runs only at collection time).
	bridge, err := metrics.NewOTelBridge(wd.GetMetrics(), mp)
	if err != nil {
		return fmt.Errorf("create OTel bridge: %w", err)
	}
	// The host owns the bridge lifecycle too. Shutdown unregisters the callback;
	// it is nil-safe and idempotent.
	defer func() {
		if sErr := bridge.Shutdown(ctx); sErr != nil {
			log.Printf("bridge shutdown: %v", sErr)
		}
	}()

	// --- COLLECT + EXPORT ----------------------------------------------------
	// With a ManualReader the host triggers collection explicitly. This fires
	// the bridge's callback, which snapshots the collector's aggregates and
	// observes them on the instruments; the ManualReader then hands the result
	// to the stdout exporter (printed below as JSON).
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		return fmt.Errorf("collect metrics: %w", err)
	}
	if err := exporter.Export(ctx, &rm); err != nil {
		return fmt.Errorf("export metrics: %w", err)
	}

	// A short human-readable summary so the example is obviously "working" even
	// without reading the JSON: confirm we exported non-zero instrument values.
	printSummary(rm)
	return nil
}

// driveActivity performs a deterministic spread of workflow-data operations so
// the exported metrics contain several operation series with non-zero counts.
func driveActivity(wd *workflow.WorkflowData) {
	const writes = 5
	for i := 0; i < writes; i++ {
		wd.Set(fmt.Sprintf("key-%d", i), i)
	}
	for i := 0; i < writes; i++ {
		// A read with a tiny sleep so duration.* instruments carry a measurable
		// (non-zero) value — purely for a richer demo, not required for counts.
		_, _ = wd.Get(fmt.Sprintf("key-%d", i))
	}
	// Touch node status/output paths too, for a broader operation spread.
	wd.SetNodeStatus("demo-node", workflow.Completed)
	_, _ = wd.GetNodeStatus("demo-node")
	wd.SetOutput("demo-node", "ok")
	_, _ = wd.GetOutput("demo-node")
	// A small delay makes the duration aggregates non-trivial.
	time.Sleep(time.Millisecond)
}

// printSummary writes a brief, human-readable confirmation to stderr that the
// flow_orchestrator.* instruments were exported with data points. The full
// metric data is already on stdout via the exporter.
func printSummary(rm metricdata.ResourceMetrics) {
	fmt.Fprintln(os.Stderr, "--- summary: flow_orchestrator instruments exported ---")
	total := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			n := dataPointCount(m)
			total += n
			fmt.Fprintf(os.Stderr, "  %-46s %d data point(s)\n", m.Name, n)
		}
	}
	fmt.Fprintf(os.Stderr, "--- total data points: %d ---\n", total)
}

// dataPointCount returns the number of data points across the supported
// aggregation shapes the bridge emits (Sum[int64], Sum[float64], Gauge[float64]).
func dataPointCount(m metricdata.Metrics) int {
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		return len(d.DataPoints)
	case metricdata.Sum[float64]:
		return len(d.DataPoints)
	case metricdata.Gauge[float64]:
		return len(d.DataPoints)
	default:
		return 0
	}
}
