package metrics_test

// OTel bridge tests (Phase 17 / M6). These tests MAY import the OTel SDK
// (go.opentelemetry.io/otel/sdk/...) — that is the host/test side and is
// permitted in _test.go per D-07. Library NON-TEST code must never import the
// SDK; that boundary is enforced by the API-only check (TestAPIOnlyBoundary in
// apiboundary_test.go).
//
// Coverage:
//   - TestOTelBridge_OBS02_Values    — value assertions via a real SDK ManualReader
//   - TestOTelBridge_OBS03_Cardinality — only the bounded `operation` attribute, closed set
//   - TestOTelBridge_OBS01_DisabledNoOp — disabled collector ⇒ no flow_orchestrator data points

import (
	"context"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// productionOperationSet is the closed enumeration of OperationType values the
// bridge may export as `operation=` attribute values. This is the DOC-02 source
// of truth for the cardinality contract (OBS-03). It is the full production set
// (incl. the 5 lazily-created public typed-string getters) and EXCLUDES the 4
// test-only values (test_op/op1/op2/op3), which the bridge filters out.
var productionOperationSet = map[string]struct{}{
	// data operations
	"set":    {},
	"get":    {},
	"delete": {},
	// public typed-string getters (created lazily on first use)
	"get_bool":    {},
	"get_string":  {},
	"get_float64": {},
	"get_int":     {},
	"get_int64":   {},
	// node status
	"set_status": {},
	"get_status": {},
	// node output
	"set_output": {},
	"get_output": {},
	// dependency resolution
	"is_node_runnable": {},
	// serialization
	"snapshot":      {},
	"load_snapshot": {},
	// lock operations
	"lock_acquire":  {},
	"lock_release":  {},
	"rlock_acquire": {},
	"rlock_release": {},
}

// flowInstrumentNames is the exact set of instrument names the bridge registers,
// with the unit each must carry.
var flowInstrumentNames = map[string]string{
	"flow_orchestrator.operation.count":          "{operation}",
	"flow_orchestrator.operation.duration.total": "s",
	"flow_orchestrator.operation.active":         "{operation}",
	"flow_orchestrator.operation.duration.avg":   "s",
	"flow_orchestrator.operation.duration.max":   "s",
	"flow_orchestrator.operation.duration.min":   "s",
}

// newManualHarness builds a ManualReader-backed MeterProvider and wires a bridge
// over the given collector. The caller drives collector activity, then calls
// collect() to trigger the bridge callback and read back the metrics.
func newManualHarness(t *testing.T, c *metrics.MetricsCollector) (collect func() metricdata.ResourceMetrics, shutdown func()) {
	t.Helper()
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	b, err := metrics.NewOTelBridge(c, mp)
	if err != nil {
		t.Fatalf("NewOTelBridge: %v", err)
	}
	collect = func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := rdr.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return rm
	}
	shutdown = func() {
		if err := b.Shutdown(context.Background()); err != nil {
			t.Errorf("bridge Shutdown: %v", err)
		}
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("MeterProvider Shutdown: %v", err)
		}
	}
	return collect, shutdown
}

// flowMetrics returns the flow_orchestrator.* metrics from a collected snapshot,
// keyed by instrument name.
func flowMetrics(rm metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	out := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if _, ok := flowInstrumentNames[m.Name]; ok {
				out[m.Name] = m
			}
		}
	}
	return out
}

// dataPointAttrs extracts the attribute sets and the `operation` value from any
// of the supported aggregation shapes (Sum[int64|float64], Gauge[float64]).
type pointAttr struct {
	keys      []string // all attribute keys present on the point
	operation string   // the operation attribute value ("" if absent)
}

func extractAttrs(t *testing.T, m metricdata.Metrics) []pointAttr {
	t.Helper()
	var pts []pointAttr
	collectSet := func(set attribute.Set) {
		var keys []string
		op := ""
		it := set.Iter()
		for it.Next() {
			kv := it.Attribute()
			keys = append(keys, string(kv.Key))
			if kv.Key == "operation" {
				op = kv.Value.AsString()
			}
		}
		pts = append(pts, pointAttr{keys: keys, operation: op})
	}
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			collectSet(dp.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			collectSet(dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			collectSet(dp.Attributes)
		}
	default:
		t.Fatalf("unexpected aggregation type for %s: %T", m.Name, m.Data)
	}
	return pts
}

// countForOp returns the int64 count data point value for the given operation
// from the flow_orchestrator.operation.count Sum.
func countForOp(t *testing.T, m metricdata.Metrics, op string) (int64, bool) {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("operation.count is not Sum[int64], got %T", m.Data)
	}
	for _, dp := range sum.DataPoints {
		if v, present := dp.Attributes.Value("operation"); present && v.AsString() == op {
			return dp.Value, true
		}
	}
	return 0, false
}

// TestOTelBridge_OBS02_Values drives real collector activity through the public
// API (including a lazy typed-string op) and asserts the six instruments are
// present with correct names + units, and that a known operation's count data
// point matches the aggregate read straight from the collector.
func TestOTelBridge_OBS02_Values(t *testing.T) {
	c := metrics.NewMetricsCollector() // SamplingRate 1.0 ⇒ deterministic count
	collect, shutdown := newManualHarness(t, c)
	defer shutdown()

	// Drive deterministic activity: N gets, M sets, and one lazy typed-string op.
	const nGet, nSet, nInt64 = 5, 3, 2
	for i := 0; i < nGet; i++ {
		c.TrackOperation(metrics.OpGet, func() { time.Sleep(time.Microsecond) })
	}
	for i := 0; i < nSet; i++ {
		c.TrackOperation(metrics.OpSet, func() {})
	}
	// OpGetInt64 is one of the lazily-created public typed-string ops (Landmine #4).
	for i := 0; i < nInt64; i++ {
		c.TrackOperation(metrics.OpGetInt64, func() {})
	}

	rm := collect()
	fm := flowMetrics(rm)

	// All six instruments present with correct names + units.
	for name, wantUnit := range flowInstrumentNames {
		m, ok := fm[name]
		if !ok {
			t.Errorf("instrument %q missing from collected metrics", name)
			continue
		}
		if m.Unit != wantUnit {
			t.Errorf("instrument %q: unit = %q, want %q", name, m.Unit, wantUnit)
		}
	}

	// Count values match what we recorded (deterministic at sampling rate 1.0).
	countM := fm["flow_orchestrator.operation.count"]
	for op, want := range map[string]int64{
		"get":       nGet,
		"set":       nSet,
		"get_int64": nInt64,
	} {
		got, ok := countForOp(t, countM, op)
		if !ok {
			t.Errorf("operation.count missing data point for operation=%q", op)
			continue
		}
		if got != want {
			t.Errorf("operation.count[%q] = %d, want %d", op, got, want)
		}
	}

	// duration.total for "get" should match the collector's own aggregate
	// (sum of recorded ns, in seconds). We read the aggregate directly and
	// compare to the exported value rather than asserting an absolute duration.
	stats := c.GetAllOperationStats()
	wantTotalGetSec := float64(stats[metrics.OpGet].TotalTimeNs) / 1e9
	durTotalM := fm["flow_orchestrator.operation.duration.total"]
	sum, ok := durTotalM.Data.(metricdata.Sum[float64])
	if !ok {
		t.Fatalf("duration.total is not Sum[float64], got %T", durTotalM.Data)
	}
	var gotTotalGetSec float64
	var found bool
	for _, dp := range sum.DataPoints {
		if v, present := dp.Attributes.Value("operation"); present && v.AsString() == "get" {
			gotTotalGetSec = dp.Value
			found = true
		}
	}
	if !found {
		t.Fatal("duration.total missing data point for operation=get")
	}
	if gotTotalGetSec != wantTotalGetSec {
		t.Errorf("duration.total[get] = %v s, want %v s (collector aggregate)", gotTotalGetSec, wantTotalGetSec)
	}
}

// TestOTelBridge_OBS03_Cardinality asserts every emitted data point carries
// exactly the single `operation` attribute, and every operation value is in the
// closed production set (test ops excluded). No workflow-controlled string.
func TestOTelBridge_OBS03_Cardinality(t *testing.T) {
	c := metrics.NewMetricsCollector()
	collect, shutdown := newManualHarness(t, c)
	defer shutdown()

	// Exercise a broad spread, plus a test-only op (must be filtered out) and
	// all five lazy typed-string ops (must appear).
	for _, op := range []metrics.OperationType{
		metrics.OpGet, metrics.OpSet, metrics.OpDelete,
		metrics.OpSetStatus, metrics.OpGetStatus,
		metrics.OpSetOutput, metrics.OpGetOutput,
		metrics.OpIsNodeRunnable, metrics.OpSnapshot, metrics.OpLoadSnapshot,
		metrics.OpLockAcquire, metrics.OpLockRelease, metrics.OpRLockAcquire, metrics.OpRLockRelease,
		metrics.OpGetBool, metrics.OpGetString, metrics.OpGetFloat64, metrics.OpGetInt, metrics.OpGetInt64,
		metrics.OperationType("test_op"), // MUST be excluded from export
	} {
		c.TrackOperation(op, func() {})
	}

	rm := collect()
	fm := flowMetrics(rm)
	if len(fm) == 0 {
		t.Fatal("no flow_orchestrator instruments collected")
	}

	sawTestOp := false
	for name, m := range fm {
		for _, p := range extractAttrs(t, m) {
			// Exactly one attribute key, and it is "operation".
			if len(p.keys) != 1 || p.keys[0] != "operation" {
				t.Errorf("%s: data point attribute set = %v, want exactly [operation]", name, p.keys)
			}
			// Operation value is in the closed production set.
			if _, ok := productionOperationSet[p.operation]; !ok {
				t.Errorf("%s: operation=%q is not in the closed production set (unbounded/unexpected label)", name, p.operation)
			}
			if p.operation == "test_op" || p.operation == "op1" || p.operation == "op2" || p.operation == "op3" {
				sawTestOp = true
			}
		}
	}
	if sawTestOp {
		t.Error("a test-only operation was exported; the bridge must exclude test_op/op1/op2/op3")
	}
}

// TestOTelBridge_OBS01_DisabledNoOp asserts that a disabled collector produces
// NO flow_orchestrator data points (the nil-guard path — Landmine #2). This is
// the production disabled-wiring pattern.
func TestOTelBridge_OBS01_DisabledNoOp(t *testing.T) {
	disabled := metrics.NewMetricsCollectorWithConfig(metrics.DisabledMetricsConfig().GetInternalConfig())
	collect, shutdown := newManualHarness(t, disabled)
	defer shutdown()

	// Attempt to drive activity — a disabled collector records nothing, and the
	// bridge callback must return early on the nil map.
	for i := 0; i < 10; i++ {
		disabled.TrackOperation(metrics.OpGet, func() {})
	}

	rm := collect()
	fm := flowMetrics(rm)
	for name, m := range fm {
		pts := extractAttrs(t, m)
		if len(pts) != 0 {
			t.Errorf("disabled collector exported %d data points for %s, want 0", len(pts), name)
		}
	}
}
