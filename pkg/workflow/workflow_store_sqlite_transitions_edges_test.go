package workflow

// M18 coverage-recovery (post-ship ratchet round): the flipState (via CancelPending) + MarkForRetry edge
// branches — the mp guard, the CAS 0-row no-op (wrong-state / budget-exhausted), and the success flip. These
// are the state-transition guards the happy-path dispatch tests skip. Assertions bite the specific flipped/
// requeued bool + ErrValidation, so a regression in the CAS guard or the mp check would redden.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCancelPending_FlipStateEdges drives flipState via CancelPending: the pending->cancelled success flip,
// the claimed-row REJECT (CAS from='pending' matches 0 rows -> not cancellable mid-flight), and the mp guard.
func TestCancelPending_FlipStateEdges(t *testing.T) {
	s := mkDispatchStore(t)

	// Non-mp store -> ErrValidation (the flipState mp guard).
	ns := newNonMPStore(t)
	_, err := ns.CancelPending("wf-x")
	require.ErrorIs(t, err, ErrValidation)

	// Pending row -> cancel succeeds (pending -> cancelled, flipped=true).
	_, err = s.Enqueue("wf-pending", "A", nil)
	require.NoError(t, err)
	flipped, err := s.CancelPending("wf-pending")
	require.NoError(t, err)
	require.True(t, flipped, "a pending row is cancellable")

	// Re-cancel the now-cancelled row -> CAS from='pending' matches 0 rows -> no-op false.
	flipped, err = s.CancelPending("wf-pending")
	require.NoError(t, err)
	require.False(t, flipped, "an already-cancelled row is a 0-row no-op")

	// A CLAIMED row is NOT cancellable via CancelPending (DEC-M17-CANCEL: no mid-flight interrupt here) ->
	// the from='pending' CAS matches 0 rows -> flipped=false, never an error.
	_, err = s.Enqueue("wf-claimed", "A", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w1", "A") // wf-claimed -> claimed
	require.NoError(t, err)
	flipped, err = s.CancelPending("wf-claimed")
	require.NoError(t, err)
	require.False(t, flipped, "a claimed row is not cancellable via CancelPending")
}

// TestMarkForRetry_Edges drives MarkForRetry's guards: the mp guard, the budget-exhausted 0-row no-op
// (attempts >= maxAttempts), and a successful requeue of a claimed row under the held token.
func TestMarkForRetry_Edges(t *testing.T) {
	s := mkDispatchStore(t)

	// Non-mp store -> ErrValidation.
	ns := newNonMPStore(t)
	_, err := ns.MarkForRetry("wf-x", 3)
	require.ErrorIs(t, err, ErrValidation)

	_, err = s.Enqueue("wf-retry", "A", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w1", "A") // wf-retry -> claimed, attempts=1
	require.NoError(t, err)
	s.setToken("wf-retry", 1) // this process holds the current fencing token

	// Budget exhausted: maxAttempts=0 means attempts(1) < 0 is false -> CAS matches 0 rows -> no requeue.
	requeued, err := s.MarkForRetry("wf-retry", 0)
	require.NoError(t, err)
	require.False(t, requeued, "budget-exhausted (attempts >= maxAttempts) is a 0-row no-op")

	// Under budget: maxAttempts=5 -> attempts(1) < 5 -> requeue claimed->pending under the held token.
	requeued, err = s.MarkForRetry("wf-retry", 5)
	require.NoError(t, err)
	require.True(t, requeued, "a claimed row under budget + held token is requeued")
}
