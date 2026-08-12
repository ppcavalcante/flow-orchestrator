package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-003 / P-08: an empty ownerID collapses distinct processes onto one lease —
// claimLocked reads equal owner strings as a re-entrant re-claim, so two
// independent multi-process stores claiming with ownerID=="" both got the same
// live token. Claim must reject the empty owner.
func TestAUD003_ClaimRejectsEmptyOwner(t *testing.T) {
	s, err := NewSQLiteStore(t.TempDir()+"/wf.db", WithMultiProcess())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	_, err = s.Claim(context.Background(), "wf", "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
}

// The audit's exact repro: two independent MP stores on the same DB both claiming
// the same workflow with an empty owner used to BOTH succeed with token 1. Now
// both are rejected, so the identity collapse is impossible.
func TestAUD003_TwoStoresEmptyOwnerCannotCollapse(t *testing.T) {
	dbPath := t.TempDir() + "/wf.db"
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer s1.Close() //nolint:errcheck // test cleanup
	s2, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup

	_, err1 := s1.Claim(context.Background(), "wf", "")
	_, err2 := s2.Claim(context.Background(), "wf", "")
	require.ErrorIs(t, err1, ErrValidation)
	require.ErrorIs(t, err2, ErrValidation)
}

// Converse guard: distinct VALID owners must NOT be collapsed — the first wins
// the live lease, the second is told the lease is held (ErrClaimLost). Ensures
// the empty-owner rejection did not over-reach.
func TestAUD003_DistinctOwnersAreNotCollapsed(t *testing.T) {
	dbPath := t.TempDir() + "/wf.db"
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer s1.Close() //nolint:errcheck // test cleanup
	s2, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup

	tok1, err := s1.Claim(context.Background(), "wf", "ownerA")
	require.NoError(t, err)
	require.NotZero(t, tok1)

	_, err = s2.Claim(context.Background(), "wf", "ownerB")
	require.ErrorIs(t, err, ErrClaimLost)
}

// WithMultiProcessLocker must fail loud on an empty owner (same programmer-error
// class as a non-ClaimStore store), not silently configure a collapsing locker.
func TestAUD003_WithMultiProcessLockerPanicsOnEmptyOwner(t *testing.T) {
	s, err := NewSQLiteStore(t.TempDir()+"/wf.db", WithMultiProcess())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	w := &Workflow{Store: s}
	require.Panics(t, func() { w.WithMultiProcessLocker("") })
}
