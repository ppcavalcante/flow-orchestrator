// Package metrics — OTel bridge (Phase 17 / M6 Observability).
//
// This file is the OpenTelemetry integration seam. It is deliberately
// API-only: it imports the OTel metrics *API* (go.opentelemetry.io/otel/metric
// and go.opentelemetry.io/otel/attribute) and NEVER the SDK
// (go.opentelemetry.io/otel/sdk/...). The host application owns the SDK,
// the exporter, and the OTLP endpoint; this library only exposes its existing
// in-memory aggregates through host-provided observable instruments.
package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// instrumentationScope is the OTel instrumentation scope name for this
// library's self-instrumentation (the module path, per OTel convention).
const instrumentationScope = "github.com/ppcavalcante/flow-orchestrator"

// operationAttrKey is the single, bounded attribute key attached to every
// observed data point. Its value is always an OperationType from the closed,
// code-defined set (see the cardinality test in otel_test.go) — never a
// workflow-controlled string. This bound is the OBS-03 cardinality contract.
const operationAttrKey = "operation"

// testOnlyOps are the OperationType values used only by the metrics package's
// own tests. They are excluded from OTel export so production telemetry never
// carries synthetic operations.
var testOnlyOps = map[OperationType]struct{}{
	OperationType("test_op"): {},
	OperationType("op1"):     {},
	OperationType("op2"):     {},
	OperationType("op3"):     {},
}

// OTelBridge exports a MetricsCollector's per-operation aggregates to an
// OpenTelemetry MeterProvider via observable (asynchronous) instruments.
//
// The bridge registers a single callback that, at the host SDK's collection
// cadence, reads the collector's current aggregates and reports them. It adds
// ZERO cost to the workflow hot path: the callback runs only on the (cold)
// collection path, never on operation recording.
//
// A bridge is obtained via NewOTelBridge and released via Shutdown. If a host
// never constructs a bridge, the library behaves exactly as before (no-op).
type OTelBridge struct {
	reg metric.Registration
}

// NewOTelBridge constructs an OTelBridge over the given collector, registering
// observable instruments on a Meter from the host-provided MeterProvider.
//
// Both arguments must be non-nil; a nil collector or MeterProvider returns an
// error rather than panicking. A no-op MeterProvider (e.g. noop.NewMeterProvider)
// is valid — the instruments become no-ops, which is a legitimate "wired but
// off" state distinct from "unwired" (no bridge at all).
//
// The exported instruments (all keyed by the single `operation` attribute,
// durations in seconds):
//   - flow_orchestrator.operation.count          Int64ObservableCounter        {operation}
//   - flow_orchestrator.operation.duration.total Float64ObservableCounter      s
//   - flow_orchestrator.operation.active         Int64ObservableUpDownCounter  {operation}
//   - flow_orchestrator.operation.duration.avg   Float64ObservableGauge        s
//   - flow_orchestrator.operation.duration.max   Float64ObservableGauge        s
//   - flow_orchestrator.operation.duration.min   Float64ObservableGauge        s
//
// Lock-contention is not exported: the lock-contention recording apparatus
// (the instrumented mutex) was removed as dead code, so no lock-contention
// metrics are collected.
func NewOTelBridge(c *MetricsCollector, mp metric.MeterProvider) (*OTelBridge, error) {
	if c == nil {
		return nil, fmt.Errorf("metrics: NewOTelBridge requires a non-nil *MetricsCollector")
	}
	if mp == nil {
		return nil, fmt.Errorf("metrics: NewOTelBridge requires a non-nil metric.MeterProvider")
	}

	meter := mp.Meter(instrumentationScope)

	countInst, err := meter.Int64ObservableCounter(
		"flow_orchestrator.operation.count",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Cumulative count of workflow data operations, by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.count instrument: %w", err)
	}

	durTotalInst, err := meter.Float64ObservableCounter(
		"flow_orchestrator.operation.duration.total",
		metric.WithUnit("s"),
		metric.WithDescription("Cumulative total duration of workflow data operations in seconds, by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.duration.total instrument: %w", err)
	}

	activeInst, err := meter.Int64ObservableUpDownCounter(
		"flow_orchestrator.operation.active",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Number of in-flight workflow data operations, by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.active instrument: %w", err)
	}

	avgInst, err := meter.Float64ObservableGauge(
		"flow_orchestrator.operation.duration.avg",
		metric.WithUnit("s"),
		metric.WithDescription("Average duration of workflow data operations in seconds (total/count snapshot), by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.duration.avg instrument: %w", err)
	}

	maxInst, err := meter.Float64ObservableGauge(
		"flow_orchestrator.operation.duration.max",
		metric.WithUnit("s"),
		metric.WithDescription("Maximum observed duration of workflow data operations in seconds, by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.duration.max instrument: %w", err)
	}

	minInst, err := meter.Float64ObservableGauge(
		"flow_orchestrator.operation.duration.min",
		metric.WithUnit("s"),
		metric.WithDescription("Minimum observed duration of workflow data operations in seconds, by operation type."),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create operation.duration.min instrument: %w", err)
	}

	// One callback over all six instruments. It reads GetAllOperationStats()
	// ONCE per collection, then observes every instrument for each operation.
	// Reading through the accessor (never the raw atomics) is required: the
	// accessor normalizes the MaxInt64 min-sentinel to 0 and computes the
	// average — raw atomics would export garbage.
	reg, err := meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			stats := c.GetAllOperationStats()
			// GetAllOperationStats returns nil when the collector is disabled
			// (or nil). Ranging a nil map is fine in Go, but guarding here is
			// explicit about the "report nothing when disabled" contract and
			// avoids any per-operation work on the disabled path.
			if stats == nil {
				return nil
			}
			for op, s := range stats {
				if _, isTest := testOnlyOps[op]; isTest {
					continue
				}
				// The single bounded attribute: operation=<OperationType>,
				// built dynamically from the map keys so the lazily-created
				// public typed-string ops (get_bool/get_string/...) are covered
				// without a hardcoded list. Still a closed, code-defined set.
				attrs := metric.WithAttributes(attribute.String(operationAttrKey, string(op)))
				o.ObserveInt64(countInst, s.Count, attrs)
				o.ObserveFloat64(durTotalInst, nsToSeconds(s.TotalTimeNs), attrs)
				o.ObserveInt64(activeInst, s.Active, attrs)
				o.ObserveFloat64(avgInst, nsToSeconds(s.AvgTimeNs), attrs)
				o.ObserveFloat64(maxInst, nsToSeconds(s.MaxTimeNs), attrs)
				o.ObserveFloat64(minInst, nsToSeconds(s.MinTimeNs), attrs)
			}
			return nil
		},
		countInst, durTotalInst, activeInst, avgInst, maxInst, minInst,
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register OTel callback: %w", err)
	}

	return &OTelBridge{reg: reg}, nil
}

// Shutdown unregisters the bridge's callback from its Meter. It is nil-safe and
// idempotent (the underlying Registration.Unregister is idempotent per the OTel
// contract). After Shutdown the bridge reports nothing.
func (b *OTelBridge) Shutdown(_ context.Context) error {
	if b == nil || b.reg == nil {
		return nil
	}
	return b.reg.Unregister()
}

// nsToSeconds converts a nanosecond duration to seconds (the OTel semconv base
// unit for durations).
func nsToSeconds(ns int64) float64 {
	return float64(ns) / 1e9
}
