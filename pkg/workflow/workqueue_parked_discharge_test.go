package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// M22 ph114 — F-PG-04 (HARD-09) DISCHARGE bite-proof. The proving-ground finding
// "a parked dispatched run holds its lease / slot forever" is ALREADY DISCHARGED by
// M19-ph94 (attempts-reset on park) + M20-ph98 (parked slots cap-excluded). This suite
// proves the three discharge legs, each failing if its mechanism were removed — NO
// production change (this seals the ratified Option D: prove-and-document, not rebuild).
//
// Force-park uses the in-package markWorkQueueParked directly (the exact call the
// dispatch park path makes at workflow_dispatch.go:217) — no new production helper.
// Row inspection reuses the existing wqState / wqAttempts test helpers.

// TestFPG04_ParkResetsAttempts (leg a): a park resets attempts to 0 — a park is durable
// PROGRESS, not a failed attempt, so it never bleeds the retry budget.
// SEED-BREAK: drop `attempts=0` from markWorkQueueParked's UPDATE (wq:668/674) -> the
// claimed attempts=1 survives the park -> this assertion goes RED.
func TestFPG04_ParkResetsAttempts(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("child", "sub", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("owner") // claimed, attempts=1
	require.NoError(t, err)
	require.Equal(t, 1, wqAttempts(t, s, "child"), "a fresh claim bumps attempts to 1")

	marked, err := s.markWorkQueueParked("child")
	require.NoError(t, err)
	require.True(t, marked, "the claimed row is park-marked")

	require.Equal(t, 0, wqAttempts(t, s, "child"), "PARK resets attempts to 0 — a park is durable progress, not a failed attempt")
	require.Equal(t, wqClaimed, wqState(t, s, "child"), "the row stays claimed (the strand-forever trap is respected — reclaim can still find it)")
}

// TestFPG04_ParkedSlotIsCapExcluded (leg b): a parked child does NOT consume a running
// cap slot — K parked parents awaiting a cap-K child do NOT deadlock. Cap PerType[sub]=1:
// claim+park one "sub", then a SECOND "sub" must STILL be claimable (the parked one is
// excluded from the running-slot COUNT).
// SEED-BREAK: drop `AND parked IS NULL` from the cap COUNT (wq:429/440) -> the parked row
// counts as a running slot -> the second claim is cap-blocked (deadlock) -> RED.
func TestFPG04_ParkedSlotIsCapExcluded(t *testing.T) {
	s := mkDispatchStore(t, WithCaps(Caps{PerType: map[string]int{"sub": 1}}))
	// Two "sub" children enqueued; the cap is 1 running at a time.
	_, err := s.Enqueue("child1", "sub", nil)
	require.NoError(t, err)
	_, err = s.Enqueue("child2", "sub", nil)
	require.NoError(t, err)

	// Claim child1 (fills the cap-1 slot), then park it.
	it1, err := s.ClaimNext("owner", "sub")
	require.NoError(t, err)
	require.Equal(t, "child1", it1.WorkflowID)
	marked, err := s.markWorkQueueParked("child1")
	require.NoError(t, err)
	require.True(t, marked)

	// The parked child1 must NOT count against the cap → child2 is claimable.
	it2, err := s.ClaimNext("owner", "sub")
	require.NoError(t, err, "a parked child does not hold a running-slot — the cap-1 child is NOT deadlocked by the parked parent")
	require.Equal(t, "child2", it2.WorkflowID, "the second sub claims into the slot the parked one vacated")
}

// TestFPG04_ParkedRowStaysReclaimable (leg c): a parked row stays `claimed` with a
// lapsing lease, so the reclaim scan still finds it after the TTL — no strand-forever.
// SEED-BREAK: (this leg is guarded by the row staying `claimed` after park — if a park
// moved the row to a terminal/pending state the reclaim below would not bump a token).
func TestFPG04_ParkedRowStaysReclaimable(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("child", "sub", nil)
	require.NoError(t, err)

	itA, err := s.ClaimNext("A")
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), itA.Token, "A holds token 1")

	marked, err := s.markWorkQueueParked("child")
	require.NoError(t, err)
	require.True(t, marked)
	require.Equal(t, wqClaimed, wqState(t, s, "child"), "parked row stays claimed (reclaimable), not terminalized")

	// A stalls past the lease TTL; the reclaim scan must still DISCOVER the parked-claimed row.
	clk.Advance(6 * time.Second)
	itB, err := s.ClaimNext("B")
	require.NoError(t, err)
	require.Equal(t, "child", itB.WorkflowID, "the reclaim scan discovers the lapsed parked-claimed row — no strand-forever")
	require.Equal(t, FencingToken(2), itB.Token, "the reclaim bumped the fencing token (A fenced)")
}
