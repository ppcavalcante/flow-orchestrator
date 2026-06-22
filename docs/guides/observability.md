# Observability (OpenTelemetry Metrics)

Flow Orchestrator records per-operation performance metrics internally (counts,
durations, in-flight gauges). As of **v0.6 (M6)** those existing metrics can be
exported to any OpenTelemetry-compatible backend through a small, **API-only**
bridge.

This guide documents the bridge, the exported instruments, how a host wires it
up, and the cardinality/availability guarantees. Every instrument name, type, and
unit below is taken directly from the source — see
[`pkg/workflow/metrics/otel.go`](../../pkg/workflow/metrics/otel.go).

---

## The API-only contract

The bridge is deliberately **API-only**. The library depends on the
OpenTelemetry metrics **API** (`go.opentelemetry.io/otel/metric` and
`go.opentelemetry.io/otel/attribute`) and **never** the SDK
(`go.opentelemetry.io/otel/sdk/...`).

What this means for you, the host:

- **You own the SDK, exporter, and OTLP endpoint.** The library has no `/metrics`
  HTTP server, no push loop, and no network surface of its own. It only exposes
  its in-memory aggregates through observable instruments registered on a
  `metric.MeterProvider` that **you** supply.
- **You own the collection cadence.** The bridge registers a single asynchronous
  (observable) callback. Your SDK's reader decides when to invoke it — a
  `PeriodicReader` on a timer, or a `ManualReader` you `Collect()` explicitly.
- **Unwired is a true no-op.** If a host never constructs a bridge, the library
  behaves exactly as before. There is no forced global provider and no panic.
  Enabling the bridge adds **zero cost to the workflow hot path** — the callback
  runs only on the (cold) collection path, never on operation recording.

This boundary is a locked decision (`DEC-M6-otel-api-only`) and is
test-enforced: `TestAPIOnlyBoundary`
([`pkg/workflow/metrics/apiboundary_test.go`](../../pkg/workflow/metrics/apiboundary_test.go))
fails if any non-test library code imports the OTel SDK.

---

## The bridge

The bridge is two functions in package `pkg/workflow/metrics`:

```go
// Construct over an existing collector, registering instruments on a host MeterProvider.
func NewOTelBridge(c *MetricsCollector, mp metric.MeterProvider) (*OTelBridge, error)

// Unregister the bridge's callback. Nil-safe and idempotent.
func (b *OTelBridge) Shutdown(ctx context.Context) error
```

- Both arguments to `NewOTelBridge` must be non-nil; a nil `*MetricsCollector` or
  nil `metric.MeterProvider` returns an error rather than panicking.
- A **no-op** `MeterProvider` (e.g. `noop.NewMeterProvider()`) is valid. The
  instruments become no-ops — a legitimate "wired but off" state, distinct from
  "unwired" (no bridge at all).
- The instrumentation scope reported to OTel is the module path:
  `github.com/ppcavalcante/flow-orchestrator`.

---

## Instrument inventory

The bridge registers **six observable instruments**, all under the
`flow_orchestrator.operation.` namespace, all carrying the single `operation`
attribute. Durations are reported in **seconds** (the OTel semconv base unit for
durations) — internally the engine stores nanoseconds and the bridge divides by
1e9.

Source of truth: `NewOTelBridge` in
[`pkg/workflow/metrics/otel.go`](../../pkg/workflow/metrics/otel.go) (lines
83–135); the value semantics come from `OperationStats`
([`internal/workflow/metrics/metrics.go:335`](../../internal/workflow/metrics/metrics.go)).

| Instrument name | OTel type | Unit | Attributes | Meaning |
|---|---|---|---|---|
| `flow_orchestrator.operation.count` | `Int64ObservableCounter` | `{operation}` | `operation` | Cumulative count of data operations, per operation type. |
| `flow_orchestrator.operation.duration.total` | `Float64ObservableCounter` | `s` | `operation` | Cumulative total duration of operations, in seconds. |
| `flow_orchestrator.operation.active` | `Int64ObservableUpDownCounter` | `{operation}` | `operation` | Number of in-flight operations, per operation type. |
| `flow_orchestrator.operation.duration.avg` | `Float64ObservableGauge` | `s` | `operation` | Average operation duration (total/count snapshot), in seconds. |
| `flow_orchestrator.operation.duration.max` | `Float64ObservableGauge` | `s` | `operation` | Maximum observed operation duration, in seconds. |
| `flow_orchestrator.operation.duration.min` | `Float64ObservableGauge` | `s` | `operation` | Minimum observed operation duration, in seconds. |

> **Lock-contention metrics are not exported in v1.** The engine also records
> lock-contention statistics, but those are recorded to a *global* collector
> while data-operation stats live on a *per-instance* collector. Exporting the
> near-zero per-instance lock data would mislead, so lock-contention export is
> intentionally deferred. See the note in `NewOTelBridge`.

---

## The `operation` attribute and cardinality (OBS-03)

Every data point carries exactly **one** attribute: `operation`, whose value is
an `OperationType` drawn from a **closed, code-defined set**. There is **no
workflow-controlled label** — node IDs, data keys, data values, and error bodies
are never attached. This bounds cardinality at the number of operation types
times your own resource attributes, so a workflow cannot blow up your metrics
backend's series count. This is the availability ceiling guaranteed by OBS-03.

The closed production set has **19 values** (the test-only operations
`test_op`/`op1`/`op2`/`op3` are filtered out of export). It is the union of:

**14 internal engine operations**
([`internal/workflow/metrics/metrics.go:17-46`](../../internal/workflow/metrics/metrics.go)):

| Group | Values |
|---|---|
| Data | `set`, `get`, `delete` |
| Node status | `set_status`, `get_status` |
| Node output | `set_output`, `get_output` |
| Dependency resolution | `is_node_runnable` |
| Serialization | `snapshot`, `load_snapshot` |
| Lock operations | `lock_acquire`, `lock_release`, `rlock_acquire`, `rlock_release` |

**5 lazily-created public typed-string getters**
([`pkg/workflow/metrics/constants.go:18-26`](../../pkg/workflow/metrics/constants.go)):
`get_bool`, `get_string`, `get_float64`, `get_int`, `get_int64`. These are
created on first use, so they appear in exported telemetry only after the
corresponding typed getter has been called at least once.

The bridge builds the `operation` attribute **dynamically from the live stat-map
keys** (not a hardcoded list), so the lazy getters are covered automatically
while the set stays closed. The exact 19-value contract is pinned by
`productionOperationSet` in
[`pkg/workflow/metrics/otel_test.go`](../../pkg/workflow/metrics/otel_test.go) and
verified against the real enum by `TestOTelBridge_OBS03_Cardinality`.

> **Host guidance (cardinality safety).** The set above is closed only for the
> operations the engine records itself. The metrics API is public, so a host
> *can* record its own operations via
> `metrics.TrackOperation(metrics.OperationType("…"), fn)` — and any value it
> passes becomes an `operation=` label, which the bridge will export. The library
> cannot bound a host-supplied string (consistent with the caller-controlled
> contract, `DEC-M1`). **Record only a small, fixed set of library-defined or
> well-known operation types — never a per-request or user-derived string** — or
> you reintroduce unbounded cardinality on your own metrics backend.

---

## Interpreting the values (caveats)

The instruments mirror the engine's in-memory aggregates faithfully, but their
semantics carry a few honest caveats a host should know when building
dashboards/alerts:

- **`count` and `duration.total` are cumulative *since the last `Reset()`*, not
  since process start.** They are modeled as observable counters, which assume
  monotonic growth. The metrics API exposes a public `Reset()` on the collector;
  if a host calls it mid-run, the counters drop to zero and **break counter
  monotonicity** for downstream rate calculations. Don't call `Reset()` on a
  collector that is being exported, or expect your backend to register a counter
  reset.
- **`duration.min` reads 0 until the first sample.** Internally `min` is seeded
  with a `MaxInt64` sentinel; the value is normalized to `0` by the stats
  accessor before export, so an operation with no samples reports `min = 0`, not
  a huge number. (The bridge reads through the accessor, never the raw atomics,
  precisely to apply this normalization — and to compute `avg`.)
- **`duration.avg` is a snapshot of `total/count` at collection time**, computed
  on read — it is a derived gauge, not an independently accumulated value.

---

## Wiring it up (host how-to)

The host is responsible for three things: (1) building an OTel SDK
`MeterProvider` with a reader/exporter, (2) enabling metrics collection on the
workflow data, and (3) constructing the bridge over that collector.

```go
import (
    "context"

    "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
    "github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"

    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
    // ...plus an exporter, e.g. stdoutmetric or otlpmetricgrpc
)

// 1. Host builds the SDK MeterProvider (the library never does this).
reader := sdkmetric.NewManualReader() // or NewPeriodicReader(exporter)
mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
defer mp.Shutdown(context.Background())

// 2. Enable metrics collection on the workflow data.
//    metrics.NewConfig() is enabled with sampling rate 1.0; use
//    metrics.ProductionConfig() for production-tuned sampling/thresholds.
cfg := workflow.DefaultWorkflowDataConfig().
    WithMetricsConfig(metrics.NewConfig())
data := workflow.NewWorkflowDataWithConfig("my-workflow", cfg)

// 3. Bridge the workflow's collector to the host MeterProvider.
bridge, err := metrics.NewOTelBridge(data.GetMetrics(), mp)
if err != nil {
    // handle: nil collector or nil provider
}
defer bridge.Shutdown(context.Background())

// ...run the workflow against `data`; the SDK reader pulls metrics on its cadence.
```

The collector you pass to `NewOTelBridge` is the instance collector returned by
`WorkflowData.GetMetrics()`
([`pkg/workflow/workflow_data.go:405`](../../pkg/workflow/workflow_data.go)) —
the same collector the engine records into. Metrics collection must be enabled
(via `WithMetricsConfig`); when the collector is disabled, the callback reports
nothing.

### Choosing an exporter

- **Production: OTLP.** Use `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/...`
  with a `PeriodicReader` to push to an OpenTelemetry Collector or any
  OTLP-compatible backend. Prometheus interop is available host-side via OTel's
  Prometheus exporter — the library stays vendor-neutral.
- **Demo / determinism:** the runnable example uses a `ManualReader` plus the
  `stdout/stdoutmetric` exporter and an explicit `Collect()`, so it needs no
  network and is deterministic in CI.

The bridge code is byte-identical across these choices — only the host's reader
and exporter differ. The library pins the OTel **API** at `v1.40.0` (the last
release on `go 1.24.0`); a host should use matching `v1.40.0` SDK packages to
stay on a single, consistent OTel release.

### Runnable example

A complete, runnable host-wiring example lives at
[`examples/observability/`](../../examples/observability/). It is a **separate Go
module** (its own `go.mod` with a `replace` directive back to the library) so that
the SDK imports it needs never enter the library's own dependency graph — keeping
the API-only boundary (OBS-04) intact. Build and run it with:

```sh
cd examples/observability
go run .
```

---

## See also

- [Performance Optimization](./performance-optimization.md) — the metrics the
  engine records and how to tune collection.
- [`pkg/workflow/metrics/otel.go`](../../pkg/workflow/metrics/otel.go) — the
  bridge source (the authoritative instrument definitions).
- `DEC-M6-otel-api-only` — the locked decision behind the API-only shape.
