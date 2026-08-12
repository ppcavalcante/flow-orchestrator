package workflow

// M18 coverage-recovery (post-ship ratchet round): the Enqueue + ListPending edge branches the happy-path
// dispatch tests skip — Enqueue's empty-type guard, its duplicate-id visible-no-op (n==0 → false), the mp
// guard on both; ListPending's olderThan age-filter arm + populated result. Real assertions on the specific
// return (false/ErrValidation/filtered set), not coverage-padding.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnqueue_EdgeCases(t *testing.T) {
	s := mkDispatchStore(t)

	// Empty type → ErrValidation (Enqueue requires a non-empty type).
	ok, err := s.Enqueue("wf-1", "", nil)
	require.ErrorIs(t, err, ErrValidation)
	require.False(t, ok)

	// First enqueue of a fresh id → a new pending row landed → true.
	ok, err = s.Enqueue("wf-dup", "A", nil)
	require.NoError(t, err)
	require.True(t, ok, "first enqueue lands a new pending row")

	// Re-enqueue the SAME id → the row already exists → 0-row visible no-op → false (not an error).
	ok, err = s.Enqueue("wf-dup", "A", nil)
	require.NoError(t, err)
	require.False(t, ok, "re-enqueue of an existing id is a visible no-op")

	// Non-mp store → ErrValidation (mp guard, before any table access).
	ns := newNonMPStore(t)
	ok, err = ns.Enqueue("wf-2", "A", nil)
	require.ErrorIs(t, err, ErrValidation)
	require.False(t, ok)
}

func TestListPending_AgeFilter(t *testing.T) {
	s := mkDispatchStore(t)

	// Non-mp store → ErrValidation.
	ns := newNonMPStore(t)
	_, err := ns.ListPending(0)
	require.ErrorIs(t, err, ErrValidation)

	// Stage two pending rows, capture a cutoff between them (enqueued_at uses the wall clock).
	_, err = s.Enqueue("old-1", "A", nil)
	require.NoError(t, err)
	cutoff := time.Now().UnixNano()
	time.Sleep(2 * time.Millisecond)
	_, err = s.Enqueue("new-1", "A", nil)
	require.NoError(t, err)

	// No filter (olderThan<=0) → both pending rows.
	all, err := s.ListPending(0)
	require.NoError(t, err)
	require.Len(t, all, 2, "no age filter → all pending")
	gotIDs := []string{all[0].WorkflowID, all[1].WorkflowID}
	require.ElementsMatch(t, []string{"old-1", "new-1"}, gotIDs,
		"a count alone does not pin WHICH rows came back")

	// olderThan = cutoff → only "old-1" (enqueued at/before the cutoff); "new-1" is excluded.
	old, err := s.ListPending(cutoff)
	require.NoError(t, err)
	require.Len(t, old, 1, "age filter → only the pre-cutoff pending row")
	require.Equal(t, "old-1", old[0].WorkflowID)
}
