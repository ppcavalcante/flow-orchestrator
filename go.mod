module github.com/ppcavalcante/flow-orchestrator

go 1.25.0

// toolchain (AUD-051): a CONTRIBUTOR building with GOTOOLCHAIN=auto (the default)
// is transparently switched to go1.25.11 — the patched release toolchain that
// closes the reachable stdlib advisories a bare 1.25.0/1.25.1 dev toolchain still
// carries (govulncheck-clean; see DEC-M5-toolchain). The `go 1.25.0` line above
// remains the LANGUAGE floor. CI pins exact versions via setup-go and runs with
// GOTOOLCHAIN=local so this directive never auto-upgrades the 1.25.0 matrix arm
// (which exists to test the true floor).
toolchain go1.25.11

require github.com/google/flatbuffers v25.2.10+incompatible

require (
	github.com/leanovate/gopter v0.2.11
	go.opentelemetry.io/otel v1.40.0
	go.opentelemetry.io/otel/metric v1.40.0
	go.opentelemetry.io/otel/sdk/metric v1.40.0
	go.opentelemetry.io/otel/trace v1.40.0
	modernc.org/sqlite v1.53.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/sdk v1.40.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
