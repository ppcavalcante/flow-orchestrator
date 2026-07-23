package workflow

// M20 ph98 — ⭐ M18 READ-MODEL PRESERVATION (red-team BLOCKER-1). The nullable `parked` discriminator must be
// INVISIBLE to the documented M18 read-model shape (QueueCounts / InFlight / Snapshot): a parked row stays
// `state='claimed'`, so it is still counted as `claimed` and still appears in-flight — a parked sub-workflow
// child IS in-flight (semantically correct for observability). This is what makes DEC-P98-PARKED-COLUMN
// preserve the M18 contract BY CONSTRUCTION (no `claimed_*→claimed` collapse needed). These tests pin that the
// parked column changes NOTHING an M18 consumer sees.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadModel_ParkedRowCountsAsClaimed — QueueCounts groups by the raw `state` string; a parked row
// (state='claimed', parked set) is counted under `claimed` exactly like a plain claimed row. The `parked`
// column is invisible to the documented count shape.
func TestReadModel_ParkedRowCountsAsClaimed(t *testing.T) {
	s := mkQueueStore(t) // NO caps — the unset-cap store the moat requires

	// Two type-X rows: one plain-claimed (running), one claimed-then-parked.
	for _, id := range []string{"running", "parked"} {
		_, err := s.Enqueue(id, "X", nil)
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w1", "X")
	require.NoError(t, err)
	_, err = s.ClaimNext("w2", "X")
	require.NoError(t, err)
	// Park the second.
	marked, err := s.markWorkQueueParked("parked")
	require.NoError(t, err)
	require.True(t, marked)

	// QueueCounts: BOTH rows count as `claimed` (the parked row is NOT split out). This is byte-identical to
	// the M18 shape for two claimed rows — the parked column adds no state, changes no count.
	counts, err := s.QueueCounts("")
	require.NoError(t, err)
	require.Equal(t, 2, counts[wqClaimed], "a parked row still counts as claimed (M18 count shape unchanged)")
	require.NotContains(t, counts, "parked", "no new state key leaks into the documented count map")
	require.NotContains(t, counts, "claimed_parked", "no split state key (the rejected state-rename design)")
}

// TestReadModel_ParkedRowIsInFlight — InFlight selects WHERE state='claimed'; a parked child IS in-flight
// (it is claimed, awaiting its child). The parked row appears in the InFlight list exactly like any claimed
// row — the documented in-flight shape is unchanged.
func TestReadModel_ParkedRowIsInFlight(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.Enqueue("pk", "X", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w1", "X")
	require.NoError(t, err)
	_, err = s.markWorkQueueParked("pk")
	require.NoError(t, err)

	inflight, err := s.InFlight()
	require.NoError(t, err)
	require.Len(t, inflight, 1, "a parked child is in-flight (claimed) — appears in InFlight")
	require.Equal(t, "pk", inflight[0].WorkflowID)
	require.Equal(t, "X", inflight[0].Type)
}

// TestReadModel_Snapshot_ParkedInvisible — the aggregate Snapshot() read-model (Counts + InFlight) reflects a
// parked child as a claimed/in-flight row, byte-identical to how it would render a plain claimed row. The
// parked discriminator is invisible across the whole mutually-consistent snapshot.
func TestReadModel_Snapshot_ParkedInvisible(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.Enqueue("sp", "X", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w1", "X")
	require.NoError(t, err)
	_, err = s.markWorkQueueParked("sp")
	require.NoError(t, err)

	snap, err := s.Snapshot(0, []string{"X"})
	require.NoError(t, err)
	require.Equal(t, 1, snap.Counts[wqClaimed], "snapshot counts the parked row as claimed")
	require.Len(t, snap.InFlight, 1, "snapshot lists the parked row as in-flight")
	require.Equal(t, "sp", snap.InFlight[0].WorkflowID)
	require.NotContains(t, snap.Counts, "parked", "no parked state key in the snapshot counts")
}

// TestReadModel_ParkedVsPlainClaimed_Identical — the DEFINITIVE preservation proof: a store with a PARKED
// claimed row produces the SAME read-model output as a store with a PLAIN claimed row (same id/type). The
// parked column is behavior-invisible to the M18 contract — the whole point of DEC-P98-PARKED-COLUMN.
func TestReadModel_ParkedVsPlainClaimed_Identical(t *testing.T) {
	// Store A: a plain claimed row.
	sa := mkQueueStore(t)
	_, err := sa.Enqueue("wf", "X", nil)
	require.NoError(t, err)
	_, err = sa.ClaimNext("w", "X")
	require.NoError(t, err)

	// Store B: the SAME row, then PARKED.
	sb := mkQueueStore(t)
	_, err = sb.Enqueue("wf", "X", nil)
	require.NoError(t, err)
	_, err = sb.ClaimNext("w", "X")
	require.NoError(t, err)
	_, err = sb.markWorkQueueParked("wf")
	require.NoError(t, err)

	// QueueCounts identical.
	ca, err := sa.QueueCounts("")
	require.NoError(t, err)
	cb, err := sb.QueueCounts("")
	require.NoError(t, err)
	require.Equal(t, ca, cb, "parked vs plain-claimed produce byte-identical QueueCounts")

	// InFlight: the parked row is in-flight (claimed) exactly like the plain claimed row — SAME id + type,
	// SAME list length. (attempts is NOT asserted equal: a PARK resets attempts to 0 — the M19 F-P94-01
	// behavior, shipped BEFORE ph98 and independent of the `parked` column; the column itself changes no
	// InFlight field.)
	ia, err := sa.InFlight()
	require.NoError(t, err)
	ib, err := sb.InFlight()
	require.NoError(t, err)
	require.Len(t, ia, 1)
	require.Len(t, ib, 1)
	require.Equal(t, ia[0].WorkflowID, ib[0].WorkflowID, "in-flight id identical")
	require.Equal(t, ia[0].Type, ib[0].Type, "in-flight type identical — the parked row is in-flight like a plain claim")
}
