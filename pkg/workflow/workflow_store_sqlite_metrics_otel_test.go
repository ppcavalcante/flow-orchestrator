package workflow

// M18 ph86 Slice 2 — the OTel dispatch-bridge bites: BITE 3 (state gauge == read-model at that instant) +
// best-effort provider isolation (a nil/failing MeterProvider must NOT break dispatch; the atomics still
// work). Uses the OTel SDK ManualReader (permitted in _test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectDispatch builds a ManualReader MeterProvider, wires the dispatch bridge, and returns a collect
// func + shutdown.
func collectDispatch(t *testing.T, store Observability, m *DispatchMetrics) (func() metricdata.ResourceMetrics, func()) {
	t.Helper()
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	bridge, err := NewOTelDispatchBridge(store, m, mp)
	require.NoError(t, err)
	collect := func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		require.NoError(t, rdr.Collect(context.Background(), &rm))
		return rm
	}
	return collect, func() { _ = bridge.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) } //nolint:errcheck // test cleanup
}

// gaugeByState pulls the queue.depth gauge points into a state->value map.
func gaugeByState(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "flow_orchestrator.dispatch.queue.depth" {
				continue
			}
			g, ok := md.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "queue.depth is an Int64 gauge")
			for _, dp := range g.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == stateAttrKey {
						out[kv.Value.AsString()] = dp.Value
					}
				}
			}
		}
	}
	return out
}

// TestOTelDispatch_GaugeMatchesReadModel (BITE 3) — the queue.depth gauge callback returns EXACTLY what
// QueueCounts() returns at that instant (the gauge IS that call). Stage a known population + assert equal.
func TestOTelDispatch_GaugeMatchesReadModel(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s, m := mkMetricsStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	// stage: 3 pending, 2 claimed.
	for i := 0; i < 5; i++ {
		_, err := s.Enqueue("g"+itoa(i), "T", nil)
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w")
	require.NoError(t, err)
	_, err = s.ClaimNext("w")
	require.NoError(t, err)

	collect, shutdown := collectDispatch(t, s, m)
	defer shutdown()

	gauge := gaugeByState(t, collect())
	counts, err := s.QueueCounts("")
	require.NoError(t, err)

	// BITE 3: the gauge value for each state == the read-model count for that state, at this instant.
	require.Equal(t, int64(counts["pending"]), gauge["pending"], "BITE 3: gauge[pending] == QueueCounts[pending]")
	require.Equal(t, int64(counts["claimed"]), gauge["claimed"], "BITE 3: gauge[claimed] == QueueCounts[claimed]")
	require.Equal(t, int64(3), gauge["pending"])
	require.Equal(t, int64(2), gauge["claimed"])

	// LIVE (not cached): claim one more pending → the NEXT collect's gauge reflects the CHANGE (pending 3->2,
	// claimed 2->3). A cached gauge would still read 3/2 → this proves the gauge IS the live QueueCounts call.
	_, err = s.ClaimNext("w")
	require.NoError(t, err)
	gauge2 := gaugeByState(t, collect())
	require.Equal(t, int64(2), gauge2["pending"], "BITE 3 live: after a claim, gauge[pending] dropped 3->2 (not a stale cache)")
	require.Equal(t, int64(3), gauge2["claimed"], "BITE 3 live: gauge[claimed] rose 2->3")
}

// TestOTelDispatch_ProviderIsolation — best-effort: a nil MeterProvider makes the bridge constructor return
// an ERROR (never panic), and the in-process atomics STILL work + dispatch proceeds. Metrics degrade to the
// queryable counters; the dispatch path is never affected.
func TestOTelDispatch_ProviderIsolation(t *testing.T) {
	s, m := mkMetricsStore(t)

	// nil provider → error, NOT panic.
	bridge, err := NewOTelDispatchBridge(s, m, nil)
	require.Error(t, err, "a nil MeterProvider is a bridge-construction error (host logs it)")
	require.Nil(t, bridge)

	// dispatch STILL works + the atomics STILL increment despite no OTel bridge — stage a fence-rejection.
	clk := NewFakeClock(time.Unix(1000, 0))
	s2, m2 := mkMetricsStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err = s2.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s2.ClaimNext("A")
	require.NoError(t, err)
	clk.Advance(6 * time.Second)
	_, err = s2.ClaimNext("B")
	require.NoError(t, err)
	s2.setToken("wf", 1)
	_, err = s2.MarkFailed("wf") // stale-token → fence-rejection
	require.NoError(t, err)
	require.Equal(t, int64(1), m2.FenceRejections(), "the in-process counter STILL works with NO OTel bridge — metrics degrade gracefully, dispatch never breaks")
	_ = m // the nil-provider m is unused after the error path; kept for symmetry
}

// counterByName pulls a Sum[int64] counter's single data point value by metric name.
func counterByName(t *testing.T, rm metricdata.ResourceMetrics, name string) (int64, bool) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			require.True(t, ok, name+" is an Int64 sum/counter")
			if len(sum.DataPoints) > 0 {
				return sum.DataPoints[0].Value, true
			}
		}
	}
	return 0, false
}

// TestOTelDispatch_AllFiveCountersExported (review ph86-F1) — the bridge must export ALL five event
// counters it documents, not a subset. A host alerting on dead_letters must receive it.
func TestOTelDispatch_AllFiveCountersExported(t *testing.T) {
	s, m := mkMetricsStore(t)
	// tick each counter once so it has a nonzero, findable data point.
	m.reclaimAfterDeath.Add(1)
	m.fenceRejections.Add(1)
	m.supersededAborts.Add(1)
	m.retriesAttempted.Add(1)
	m.deadLetters.Add(1)

	collect, shutdown := collectDispatch(t, s, m)
	defer shutdown()
	rm := collect()
	for _, name := range []string{
		"flow_orchestrator.dispatch.reclaim_after_death",
		"flow_orchestrator.dispatch.fence_rejections",
		"flow_orchestrator.dispatch.superseded_aborts",
		"flow_orchestrator.dispatch.retries_attempted",
		"flow_orchestrator.dispatch.dead_letters",
	} {
		v, found := counterByName(t, rm, name)
		require.True(t, found, "F1: counter %s MUST be exported (all 5 event counters, not a subset)", name)
		require.Equal(t, int64(1), v, "%s reflects its atomic", name)
	}
}
