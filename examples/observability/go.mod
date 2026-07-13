// This example lives in its OWN Go module — deliberately separate from the
// root library module. The example wires a real OpenTelemetry SDK + exporter,
// which means it imports go.opentelemetry.io/otel/sdk/... . If a non-test
// main.go in the ROOT module imported the SDK, the SDK would land in the
// library's non-test dependency graph and break the API-only boundary (OBS-04).
// Keeping the example in a nested module with a `replace` to the local checkout
// keeps `go list -deps ./...` on the library clean while still demonstrating a
// full, runnable host wiring.
//
// Versions are pinned to v1.40.0 to MATCH the library's otel/metric pin
// (DEC-P17-otel-pin): v1.40.0 is the last OTel release whose go.mod declares
// `go 1.24.0`, so this example module can also stay on the 1.24.0 floor.
module github.com/ppcavalcante/flow-orchestrator/examples/observability

go 1.25.0

replace github.com/ppcavalcante/flow-orchestrator => ../..

require (
	github.com/ppcavalcante/flow-orchestrator v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.40.0
	go.opentelemetry.io/otel/sdk/metric v1.40.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.40.0 // indirect
	go.opentelemetry.io/otel/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk v1.40.0 // indirect
	go.opentelemetry.io/otel/trace v1.40.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.53.0 // indirect
)
