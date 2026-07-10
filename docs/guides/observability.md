# Observability (OpenTelemetry Metrics & Tracing)

Flow Orchestrator records per-operation performance metrics internally (counts,
durations, in-flight gauges). As of **v0.6 (M6)** those existing metrics can be
exported to any OpenTelemetry-compatible backend through a small, **API-only**
bridge. As of **v0.8 (M8)** the executor can additionally emit a **distributed
trace** — one span per executed node under a parent workflow span — through the
same API-only contract.

This guide documents both: the **metrics bridge** (its exported instruments,
host wiring, and cardinality/availability guarantees) and the **tracing**
integration. Every instrument name, type, and unit below is taken directly from
the source — see [`pkg/workflow/metrics/otel.go`](../../pkg/workflow/metrics/otel.go)
for metrics and [`pkg/workflow/tracing.go`](../../pkg/workflow/tracing.go) for
tracing.

The two are independent: a host can wire metrics, tracing, both, or neither.
Each is off until the host opts in, and each follows the same rule — the library
depends only on the OpenTelemetry **API**, never the SDK; the host owns the SDK,
exporter, and endpoint.

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

> **Lock-contention metrics are not exported.** The per-instance collector still
> defines `RecordLockContention`/`GetLockContentionStats`, but the recording
> apparatus that drove them (the instrumented mutex) was removed as dead code
> (`DEC-CHUNK4`), so no lock-contention statistics are actually collected and
> there is nothing to export. There is no longer any process-global metrics state.
> See the note on `NewOTelBridge`.

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
> *can* record its own operations via the per-instance collector —
> `data.GetMetrics().TrackOperation(metrics.OperationType("…"), fn)` — and any
> value it passes becomes an `operation=` label, which the bridge will export. The
> library
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

// ...run the DAG against `data`; the SDK reader pulls metrics on its cadence.
dag.Execute(context.Background(), data)
```

> **Enabling metrics on the two entry points.** Operation metrics are recorded into the
> collector on the `WorkflowData` instance the engine executes against. On the lower-level
> `DAG.Execute(ctx, data)` path, you own that `data` — pass a metrics-enabled one (above).
> On the higher-level `Workflow.Execute(ctx)` drive (the persistence / durable / saga entry
> point), the `WorkflowData` is built internally — **set `Workflow.MetricsConfig`** (added
> v0.13.0 / REM-02) to opt it into collection, then read the stats after `Execute` with
> `Workflow.GetMetrics()` or export them through the bridge. (Before v0.13.0 the
> `Workflow.Execute` path had no such hook and metrics were effectively off there.)
>
> ```go
> wf, _ := workflow.FromBuilder(builder)
> wf.MetricsConfig = metrics.NewConfig().WithEnabled(true)
> _ = wf.Execute(context.Background())
> collected := wf.GetMetrics() // per-operation stats after the run
> ```

The collector you pass to `NewOTelBridge` is the instance collector returned by
`WorkflowData.GetMetrics()`
([`pkg/workflow/workflow_data.go:455`](../../pkg/workflow/workflow_data.go)) —
the same collector the engine records into. Metrics are **per-instance**: each
`WorkflowData` owns its own `MetricsCollector` and there is no process-global
metrics state (`DEC-CHUNK4`). The collector's `Enable`/`Disable`/`Reset`/
`TrackOperation` are methods on that instance, not package-level functions. Metrics collection must be enabled
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

# Tracing (OpenTelemetry spans)

When a host supplies an OpenTelemetry `TracerProvider`, the executor emits a
**span per executed node** under a single **parent span per workflow run**. This
gives you a trace waterfall of a `DAG.Execute` call — which nodes ran, in what
order across levels, how long each took, and which failed — in any
OTLP-compatible tracing backend (Jaeger, Tempo, etc.).

Tracing is **off by default** and follows the same **API-only** contract as the
metrics bridge: the library imports only `go.opentelemetry.io/otel/trace` (now a
direct dependency), never the SDK. The host owns the SDK, exporter, and endpoint.
Source of truth: [`pkg/workflow/tracing.go`](../../pkg/workflow/tracing.go).

## Wiring it up

Tracing is enabled by handing a `trace.TracerProvider` to the workflow. There are
three equivalent injection points; pick the one that fits how you build:

```go
import "go.opentelemetry.io/otel/trace"

// On the builder (applied to the DAG that Build produces):
dag, _ := workflow.NewWorkflowBuilder().
    WithWorkflowID("my-workflow").
    // ...AddNode(...)...
    WithTracerProvider(tp). // tp is your *sdktrace.TracerProvider (a trace.TracerProvider)
    Build()

// Or directly on a DAG:
dag.WithTracerProvider(tp)

// Or via ExecutionConfig (e.g. when you set MaxConcurrency too):
cfg := workflow.DefaultConfig()
cfg.TracerProvider = tp
dag.WithExecutionConfig(cfg)
```

- All three set the same underlying `ExecutionConfig.TracerProvider`. On the
  builder, `WithTracerProvider` is applied **after** `WithExecutionConfig`, so a
  custom config passed to the builder will not clobber a tracer provider you also
  set there — the builder's `WithTracerProvider` is the source of truth.
- **Passing `nil` (the default/zero value) disables tracing.** A nil provider
  resolves once, up front, to a no-op tracer, so the run is byte-identical and
  zero-alloc versus not tracing at all (measured: identical allocations). There
  is no global provider read and no panic when unwired.

The host builds the SDK `TracerProvider` exactly as it would for any OTel app
(the library never does this):

```go
import (
    "context"

    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    // ...plus an exporter, e.g. stdouttrace or otlptracegrpc
)

exporter, _ := stdouttrace.New() // or an OTLP exporter
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
defer tp.Shutdown(context.Background())

dag.WithTracerProvider(tp)
// ...run dag.Execute(ctx, data); spans flow to your exporter.
```

## Span model

- **Parent span: `workflow.execute`.** Opened once at the start of
  `DAG.Execute` and closed when the run returns. Its context flows down so every
  node span is a child of it.
- **One child span per executed node, named after the node** (span name = the
  node's name — a readable waterfall, e.g. `fetch-user`, not a generic verb). The
  span opens when the node's goroutine starts in the level executor and closes
  when the node finishes (after retries).
- **Skipped nodes get no span.** A span implies execution; a node marked
  `Skipped` (transitively blocked by a failed dependency, `DEC-CHUNK3-status`)
  never ran, so it emits no span. The count of skipped nodes is surfaced on the
  **parent** span instead, via the `workflow.skipped_count` attribute (computed at
  span close, after the final status sweep).

## Span attributes (closed set, no-leak)

Attribute keys are a **closed, code-defined set** — never workflow-controlled
strings — so a trace backend sees bounded key cardinality (`DEC-CHUNK5`):

| Span | Attribute | Type | Set when |
|---|---|---|---|
| node | `node.status` | string | always (final `NodeStatus`: `completed`/`failed`) |
| node | `node.retry_count` | int | only when the node's configured retry count > 0 |
| `workflow.execute` (parent) | `workflow.skipped_count` | int | always (count of nodes that ended `Skipped`) |

The **no-leak discipline from chunk 2 extends to traces**: span attributes read
only engine-controlled values (the node name, its final status, its configured
retry count). No `WorkflowData` values, keys, paths, or arbitrary state are ever
attached. The one host-influenced string that reaches a span is the **action's
own error**, recorded via `span.RecordError` at the call site — that is the
action's own contract, exactly as it is for the returned error. Nothing more from
the library's side. (See `nodeSpanAttributes` in
[`pkg/workflow/tracing.go`](../../pkg/workflow/tracing.go).)

## Cancellation and tracing

If the run is cancelled, the parent `workflow.execute` span still closes
normally and the spans of nodes that already ran are emitted as usual. Nodes that
never started emit no span (consistent with the cancellation contract: unreached
and downstream nodes stay `Pending`, not `Skipped` — see
[Error Handling → Context cancellation](./error-handling.md#context-cancellation-and-timeouts)).

## Instrumentation scope

The tracer's instrumentation scope is the module path
`github.com/ppcavalcante/flow-orchestrator` — the **same scope as the metrics
bridge**, so traces and metrics from this library carry a consistent
instrumentation identity in your backend.

---

## See also

- [Performance Optimization](./performance-optimization.md) — the metrics the
  engine records and how to tune collection.
- [`pkg/workflow/metrics/otel.go`](../../pkg/workflow/metrics/otel.go) — the
  metrics bridge source (the authoritative instrument definitions).
- [`pkg/workflow/tracing.go`](../../pkg/workflow/tracing.go) — the tracing source
  (span names, attribute keys, the noop-resolution contract).
- `DEC-M6-otel-api-only` / `DEC-CHUNK5` — the locked decisions behind the
  API-only shape (metrics and tracing respectively).
