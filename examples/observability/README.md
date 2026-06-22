# Observability example — wiring the OTel metrics bridge

A runnable, self-contained demonstration of exporting flow-orchestrator's
existing in-memory metrics to OpenTelemetry.

## Run it

```sh
cd examples/observability
go run .
```

It prints the collected metrics to **stdout** as JSON (via the
`stdoutmetric` exporter) and a short human-readable summary to **stderr**.

## What it shows — the API-only contract

The library follows an **API-only** observability contract:

- The **library** depends on the OpenTelemetry metrics *API*
  (`go.opentelemetry.io/otel/metric`) only. It never imports the SDK, never
  starts a `/metrics` server, and opens no network connections.
- The **host** (this program) owns everything else: it builds the SDK
  `MeterProvider`, picks the reader and exporter, and controls the collection
  and shutdown lifecycle.

Because this example imports the OTel **SDK**, it lives in its **own Go module**
(`examples/observability/go.mod`, with a `replace` to the local checkout). If a
non-test `main.go` in the root module imported the SDK, the SDK would enter the
library's non-test dependency graph and break the API-only boundary. Keeping the
example in a nested module keeps `go list -deps ./...` on the library clean.

## The wiring, step by step

1. Build a host SDK `MeterProvider` (here: a `ManualReader` + `stdoutmetric`
   exporter — deterministic and offline, ideal for an example/CI).
2. Enable the library's metrics on a `WorkflowData` via
   `WithMetricsConfig(metrics.NewConfig())`.
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
