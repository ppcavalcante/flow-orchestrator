package workflow

// M18 coverage-recovery (post-ship ratchet round): the Snapshot() component-scan surface (scanStuckTx /
// scanCountsTx / scanInFlightTx / scanWorkersTx / wrapRowsErr) + the StuckWork reason-labeling switch + the
// WorkflowStatus node-tally path. These exercise real branches the ph85 Slice-1/2 tests did not: all THREE
// stuck reasons in one snapshot + a non-empty node journal in WorkflowStatus. Assertions are on the specific
// reason/state, not just "no error". (CancelRunning idempotency is already covered by TestCancelRunning_Idempotent.)

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSnapshot_AllStuckReasons — a single Snapshot() over a store staged so its stuck list contains one of
// EACH reason (lapsed-claimed, unregistered-type, too-old-pending) plus a healthy row that must NOT appear.
// Drives Snapshot -> scanStuckTx's full switch + scanCountsTx/scanInFlightTx/scanWorkersTx in one txn.
func TestSnapshot_AllStuckReasons(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))

	// Stage:
	//  - "lapsed" (type A): claimed by w1, then the lease lapses    -> StuckLapsedClaimed
	//  - "unreg"  (type Z): pending, type Z not in the registered set -> StuckUnregisteredType
	//  - "old"    (type A): pending, enqueued at/before the age cutoff -> StuckTooOldPending
	//  - "fresh"  (type A): pending, enqueued AFTER the cutoff        -> NOT stuck (the default-skip arm)
	for _, e := range []struct{ id, typ string }{{"lapsed", "A"}, {"unreg", "Z"}, {"old", "A"}} {
		_, err := s.Enqueue(e.id, e.typ, nil)
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w1", "A") // claims "lapsed" (oldest A) -> claimed w/ live lease
	require.NoError(t, err)

	// NOTE: enqueued_at uses the WALL clock (unixNanoNow), not the injected lease clock — so the too-old
	// cutoff must be a real timestamp taken between the "old" enqueue (above) and the "fresh" enqueue (below).
	cutoff := time.Now().UnixNano()  // "lapsed","unreg","old" are all enqueued at/before this real instant
	time.Sleep(2 * time.Millisecond) // ensure "fresh" gets a strictly-greater enqueued_at than cutoff
	clk.Advance(6 * time.Second)     // lapse w1's lease (TTL 5s) -> "lapsed" is now lapsed-claimed
	_, err = s.Enqueue("fresh", "A", nil)
	require.NoError(t, err) // enqueued AFTER cutoff -> not too-old

	// registeredTypes = {A} only -> type Z ("unreg") is unregistered. olderThan = cutoff -> "old" is too-old.
	snap, err := s.Snapshot(cutoff, []string{"A"})
	require.NoError(t, err)

	got := map[string]StuckReason{}
	for _, it := range snap.Stuck {
		got[it.WorkflowID] = it.Reason
	}
	require.Equal(t, StuckLapsedClaimed, got["lapsed"], "claimed + lapsed lease")
	require.Equal(t, StuckUnregisteredType, got["unreg"], "pending, type not registered")
	require.Equal(t, StuckTooOldPending, got["old"], "pending, enqueued at/before cutoff")
	require.NotContains(t, got, "fresh", "a fresh pending registered row is NOT stuck (the default-skip arm)")

	// The snapshot's other components are populated + mutually consistent (counts sum == total rows).
	total := 0
	for _, n := range snap.Counts {
		total += n
	}
	require.Equal(t, 4, total, "4 rows staged (lapsed+unreg+old+fresh)")
	require.NotEmpty(t, snap.Workers, "w1 held a lease -> appears in worker health")
}

// TestWorkflowStatus_NodeTally — WorkflowStatus returns the per-status node journal tally (the scan loop that
// the dispatch-only ph85 test left uncovered). Stage a journal via Save, then assert the NodeCounts shape.
func TestWorkflowStatus_NodeTally(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("wf-node", "A", nil)
	require.NoError(t, err)

	// Write a node journal for wf-node: 2 completed + 1 failed.
	d := NewWorkflowData("wf-node")
	d.SetNodeStatus("n1", Completed)
	d.SetNodeStatus("n2", Completed)
	d.SetNodeStatus("n3", Failed)
	require.NoError(t, s.Save(d))

	ws, err := s.WorkflowStatus("wf-node")
	require.NoError(t, err)
	require.True(t, ws.Queued)

	tally := map[string]int{}
	for _, nc := range ws.NodeCounts {
		tally[nc.Status] += nc.Count
	}
	require.Equal(t, 2, tally[string(Completed)], "2 completed nodes in the tally")
	require.Equal(t, 1, tally[string(Failed)], "1 failed node in the tally")
}
