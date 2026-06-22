package metrics_test

// OBS-04 API-only boundary check (Phase 17 / M6).
//
// Library NON-TEST code in pkg/ and internal/ must import the OTel metrics API
// (go.opentelemetry.io/otel/metric, .../attribute) ONLY — never the SDK
// (go.opentelemetry.io/otel/sdk/...). The SDK is permitted in _test.go (these
// tests import it) and in examples/ (Phase 18).
//
// This test is the re-runnable mechanical enforcement (OBL-P17-api-only-boundary):
// it shells out to `go list` to compute the NON-TEST import set of every package
// under the two trees and fails if any imports an otel/sdk package. To prove it
// is discriminating, inject an otel/sdk import into a non-test lib file and this
// test goes RED (verified manually at build time; see the SUMMARY).
//
// qa can re-run it with:  go test ./pkg/workflow/metrics/ -run TestAPIOnlyBoundary

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const otelSDKPrefix = "go.opentelemetry.io/otel/sdk"

// goListPkg is the subset of `go list -json` output we need.
type goListPkg struct {
	ImportPath string
	Imports    []string // NON-test imports only (the default Imports field)
	Standard   bool
}

func TestAPIOnlyBoundary(t *testing.T) {
	// List every package under the two library trees, from the module root.
	// We use the default `Imports` field, which is the package's NON-test
	// imports — exactly the boundary D-07 governs (test imports are exempt).
	cmd := exec.Command("go", "list", "-json",
		"github.com/ppcavalcante/flow-orchestrator/pkg/...",
		"github.com/ppcavalcante/flow-orchestrator/internal/...",
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list failed: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var violations []string
	pkgCount := 0
	for dec.More() {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		pkgCount++
		for _, imp := range p.Imports {
			if imp == otelSDKPrefix || strings.HasPrefix(imp, otelSDKPrefix+"/") {
				violations = append(violations, p.ImportPath+" imports "+imp)
			}
		}
	}

	if pkgCount == 0 {
		t.Fatal("go list returned no packages — the boundary check did not actually run")
	}
	if len(violations) > 0 {
		t.Errorf("API-only boundary violated — non-test library code must not import the OTel SDK (%s):\n  %s",
			otelSDKPrefix, strings.Join(violations, "\n  "))
	}
}
