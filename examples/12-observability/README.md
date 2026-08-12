# Observability example — wiring the OTel metrics bridge

A runnable, self-contained demonstration of exporting flow-orchestrator's
existing in-memory metrics to OpenTelemetry.

## Run it

```sh
cd examples/12-observability
GOTOOLCHAIN=local go run .
```

It prints the collected metrics to **stdout** as JSON (via the
`stdoutmetric` exporter) and a short human-readable summary to **stderr**.

## Test it

This example is a **separate Go module** (it imports the OTel SDK), so its smoke
test runs from inside the example directory, not via the root module's
`./examples/...`:

```sh
cd examples/12-observability
GOTOOLCHAIN=local go test ./... -count=1
```

The test runs the demo and asserts the bridge actually exported
`flow_orchestrator.*` instruments with non-zero data points — it would fail if
the metrics never reach the bridge.

## What it shows — the API-only contract

The library follows an **API-only** observability contract:

- The **library** depends on the OpenTelemetry metrics *API*
  (`go.opentelemetry.io/otel/metric`) only. It never imports the SDK, never
  starts a `/metrics` server, and opens no network connections.
- The **host** (this program) owns everything else: it builds the SDK
  `MeterProvider`, picks the reader and exporter, and controls the collection
  and shutdown lifecycle.

Because this example imports the OTel **SDK**, it lives in its **own Go module**
(`examples/12-observability/go.mod`, with a `replace` to the local checkout). If a
non-test `main.go` in the root module imported the SDK, the SDK would enter the
library's non-test dependency graph and break the API-only boundary. Keeping the
example in a nested module keeps `go list -deps ./...` on the library clean.

## The wiring, step by step

1. Build a host SDK `MeterProvider` (here: a `ManualReader` + `stdoutmetric`
   exporter — deterministic and offline, ideal for an example/CI).
2. Enable the library's metrics on a `WorkflowData` via
   `WithMetricsConfig(metrics.NewConfig().WithEnabled(true))`. Metrics are
   **opt-in** — `metrics.NewConfig()` is disabled by default (the hot path pays
   nothing unless you ask for them), and `WithEnabled(true)` keeps its
   deterministic sampling rate of 1.0.
3. Drive workflow-data activity (Set/Get/SetNodeStatus/...).
4. Bridge the collector to OTel: `metrics.NewOTelBridge(wd.GetMetrics(), mp)`.
5. Trigger collection (`reader.Collect`) and export (`exporter.Export`).
6. Shut down the bridge and the MeterProvider.

## Production note

This demo uses a `ManualReader` + stdout exporter so it is deterministic and
needs no network. In production you would typically use a `PeriodicReader`
feeding an **OTLP** exporter pointed at your collector — the bridge code is
identical. See `docs/guides/observability.md` for the full guide, the
instrument inventory, and the cardinality contract.
