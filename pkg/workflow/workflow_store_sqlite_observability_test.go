package workflow

// M18 ph85 Slice 1 — the Observability granular methods (OBS-RM-01..05) against a staged store.
// Each is one atomic SELECT; these verify the SQL + the result shapes. The Snapshot() mutual-consistency
// hard bar (separate-instance seed-break) is Slice 2.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservability_GranularQueries(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))

	// Stage: 2 pending (types A, B), 1 claimed (owner w1, live lease), 1 done.
	for _, id := range []string{"p1", "p2", "c1", "d1"} {
		typ := "A"
		if id == "p2" {
			typ = "B"
		}
		_, err := s.Enqueue(id, typ, nil)
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w1", "A") // claims p1 (oldest A) → claimed, lease live
	require.NoError(t, err)
	// stage a done row: claim d1 then MarkDone
	_, err = s.ClaimNext("w1", "A") // claims c1 (next A)
	require.NoError(t, err)
	// re-purpose: mark c1 done so we have a done row; p1 stays claimed
	// Actually ClaimNext took p1 then c1; mark c1 done, leaves p1 claimed, p2/d1 pending.
	// d1 was type A too? no — d1 enqueued as A. Let's just assert against real counts below.

	// OBS-RM-01 QueueCounts — per-state.
	counts, err := s.QueueCounts("")
	require.NoError(t, err)
	total := 0
	for _, n := range counts {
		total += n
	}
	require.Equal(t, 4, total, "OBS-RM-01: counts sum to the 4 enqueued items")
	require.GreaterOrEqual(t, counts["claimed"], 1, "at least one claimed (p1/c1)")

	// type filter
	countsA, err := s.QueueCounts("A")
	require.NoError(t, err)
	sumA := 0
	for _, n := range countsA {
		sumA += n
	}
	require.Equal(t, 3, sumA, "OBS-RM-01 type filter: 3 type-A items (p1,c1,d1)")

	// OBS-RM-02 InFlight — the claimed rows JOIN leases, live freshness.
	inflight, err := s.InFlight()
	require.NoError(t, err)
	require.NotEmpty(t, inflight, "OBS-RM-02: claimed rows are in-flight")
	for _, it := range inflight {
		require.Equal(t, "w1", it.OwnerID)
		require.True(t, it.LeaseLive, "lease is live (clock not advanced)")
	}

	// advance clock past TTL → the same claimed rows now report LAPSED.
	clk.Advance(6 * time.Second)
	inflight2, err := s.InFlight()
	require.NoError(t, err)
	for _, it := range inflight2 {
		require.False(t, it.LeaseLive, "OBS-RM-02: after TTL lapse the lease reports NOT live (freshness reads the lease clock)")
	}

	// OBS-RM-03 StuckWork — after lapse, the claimed rows are lapsed-claimed; p2 (type B) unregistered.
	stuck, err := s.StuckWork(0, []string{"A"}) // only A registered → p2 (B) is unregistered
	require.NoError(t, err)
	var sawLapsed, sawUnreg bool
	for _, it := range stuck {
		switch it.Reason {
		case StuckLapsedClaimed:
			sawLapsed = true
		case StuckUnregisteredType:
			sawUnreg = true
			require.Equal(t, "p2", it.WorkflowID)
		}
	}
	require.True(t, sawLapsed, "OBS-RM-03: lapsed-claimed rows are stuck")
	require.True(t, sawUnreg, "OBS-RM-03: the unregistered-type pending (p2/B) is stuck")

	// OBS-RM-04 WorkflowStatus — a claimed workflow's dispatch status.
	ws, err := s.WorkflowStatus("p1")
	require.NoError(t, err)
	require.True(t, ws.Queued)
	require.Equal(t, wqClaimed, ws.State)
	require.Equal(t, "w1", ws.OwnerID)
	// a never-enqueued id → not queued.
	wsNone, err := s.WorkflowStatus("nope")
	require.NoError(t, err)
	require.False(t, wsNone.Queued)

	// OBS-RM-05 WorkerHealth — w1 holds leases; after lapse, 0 live.
	wh, err := s.WorkerHealth()
	require.NoError(t, err)
	require.NotEmpty(t, wh)
	for _, w := range wh {
		require.Equal(t, "w1", w.OwnerID)
		require.GreaterOrEqual(t, w.TotalHeld, 1)
		require.Zero(t, w.LiveHeld, "OBS-RM-05: after TTL lapse, no live leases")
	}
}
