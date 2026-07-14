package workflow

// ph74 store-level fencing-substrate tests: prove the token→checkpoint-txn CAS works BEFORE
// ph75 builds any claim policy. NO executor involved — the tests drive the store directly.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// mkMPStore opens an M16 multi-process SQLiteStore under t.TempDir.
func mkMPStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "mp.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
	return s
}

// seedLease directly writes a lease row (owner + token) — the ph74 substrate test stand-in for
// ph75's Claim, so the CAS can be exercised without claim policy.
func seedLease(t *testing.T, s *SQLiteStore, wf, owner string, token FencingToken) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES (?,?,?,?)
		 ON CONFLICT(workflow_id) DO UPDATE SET owner_id=excluded.owner_id, expiry=excluded.expiry, fencing_token=excluded.fencing_token`,
		wf, owner, s.leaseExpiryNanos(), int64(token))
	require.NoError(t, err)
}

// TestFencingCAS_StaleTokenRejected — THE ph74 hard-bar store-level bite. Process holds token
// T1; the lease's fencing_token is bumped to T2 (simulating a re-claim by another worker); a
// checkpoint write under the stale T1 → the CAS-in-the-IMMEDIATE-txn REJECTS it with ErrFencedOut,
// and NO row lands. With fencing correct this holds; the seed-break (below) proves it bites.
func TestFencingCAS_StaleTokenRejected(t *testing.T) {
	s := mkMPStore(t)
	const wf = "wfA"

	// This process claimed at token 1; write level 0 legitimately.
	seedLease(t, s, wf, "procA", 1)
	s.setToken(wf, 1)
	d := NewWorkflowData(wf)
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d), "write under the CURRENT token must succeed")

	// A re-claim by another worker bumps the durable fencing_token to 2 (procA is now a zombie).
	seedLease(t, s, wf, "procB", 2)

	// procA (still holding stale token 1) attempts another checkpoint → FENCED OUT, no row lands.
	d.SetNodeStatus("n1", Completed)
	err := s.Save(d)
	require.ErrorIs(t, err, ErrFencedOut, "a stale-token write must be fenced")

	// The durable node set must NOT reflect procA's fenced write (n1 never landed under stale token).
	got, err := s.Load(wf)
	require.NoError(t, err)
	_, hasN1 := got.GetNodeStatus("n1")
	require.False(t, hasN1, "the fenced-out stale write must NOT have landed")
}

// TestFencingCAS_SeedTheBreak — the guard-must-bite, done WITHOUT a production off-switch
// (reviewer ph74-F1: a mutable disable-flag on the sole safety arbiter must not ship). The
// differential proof: the EXACT same stale sequence that an mp store REJECTS (ErrFencedOut,
// TestFencingCAS_StaleTokenRejected) instead LANDS on a NON-mp store (fencing disabled by
// construction — checkFencingLocked returns nil at its first line). So the mp fencing CAS is
// provably the load-bearing guard: remove it (drop to single-process config) → the stale write
// is no longer rejected. A guard you cannot make NOT-fire proves nothing; this makes it not-fire
// via the honest config knob, not a shipped kill-switch on the predicate.
func TestFencingCAS_SeedTheBreak(t *testing.T) {
	// NON-mp store — fencing is off by construction (the honest "no fencing" configuration).
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "np.db"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }() //nolint:errcheck // test cleanup
	const wf = "wfA"

	seedLease(t, s, wf, "procA", 1)
	s.setToken(wf, 1) // token is set, but dur.mp=false → checkFencingLocked is a no-op
	d := NewWorkflowData(wf)
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d))

	seedLease(t, s, wf, "procB", 2) // re-claim bumps token; procA (still token 1) is now stale

	// Without the fencing CAS (mp off), procA's stale write is NOT rejected — it LANDS. This is
	// exactly the write ErrFencedOut prevents on an mp store: the CAS is the difference.
	d.SetNodeStatus("n1", Completed)
	err = s.Save(d)
	require.NoError(t, err, "seed-break: fencing off → the stale write must NOT be rejected")
	got, err := s.Load(wf)
	require.NoError(t, err)
	_, hasN1 := got.GetNodeStatus("n1")
	require.True(t, hasN1, "seed-break: fencing off → the stale write LANDS — the mp fencing CAS is the load-bearing guard that prevents this")
}

// TestFencing_NoOpWhenUnclaimed — a drive that holds NO token (the common single-process-on-an-
// mp-store, or a Save outside any claim) is never fenced: no lease row, no token → the CAS is a
// clean no-op and the write proceeds.
func TestFencing_NoOpWhenUnclaimed(t *testing.T) {
	s := mkMPStore(t)
	const wf = "wfFree"
	// No seedLease, no setToken → heldToken == 0 → checkFencingLocked returns nil.
	d := NewWorkflowData(wf)
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d), "an unclaimed drive must write freely (no fence)")
	got, err := s.Load(wf)
	require.NoError(t, err)
	st, ok := got.GetNodeStatus("n0")
	require.True(t, ok)
	require.Equal(t, Completed, st)
}

// TestFencing_RenewOnCheckpoint — a successful checkpoint under the current token BUMPS the
// lease expiry (DEC-M16-D4 renew-on-checkpoint), so a live worker's lease does not lapse while
// it makes progress.
func TestFencing_RenewOnCheckpoint(t *testing.T) {
	s := mkMPStore(t)
	const wf = "wfRenew"
	// Seed a lease with a SHORT (already-near) expiry, token 1.
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES (?,?,?,?)`,
		wf, "procA", int64(1), int64(1)) // expiry=1ns (ancient)
	require.NoError(t, err)
	s.setToken(wf, 1)

	before := int64(1)
	d := NewWorkflowData(wf)
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d)) // the CAS renews expiry inside the txn

	var after int64
	require.NoError(t, s.db.QueryRow(`SELECT expiry FROM leases WHERE workflow_id=?`, wf).Scan(&after))
	require.Greater(t, after, before, "a checkpoint under the current token must renew (bump) the lease expiry")
}

// TestFencing_ClearTokenDropsTheFence — clearToken (ph75's Release plumbing) drops this
// process's held token, so a subsequent write is no longer fenced against the (now foreign)
// lease row. Proves the token-state lifecycle: set → fenced; clear → un-fenced.
func TestFencing_ClearTokenDropsTheFence(t *testing.T) {
	s := mkMPStore(t)
	const wf = "wfClear"

	seedLease(t, s, wf, "procA", 1)
	s.setToken(wf, 1)
	seedLease(t, s, wf, "procB", 2) // procA is now stale (token 1 < durable 2)

	d := NewWorkflowData(wf)
	d.SetNodeStatus("n0", Completed)
	require.ErrorIs(t, s.Save(d), ErrFencedOut, "while holding the stale token, the write is fenced")

	// clearToken (what Release does to this process's state): drop our claim knowledge → the CAS
	// no-ops (held == 0) → the write is no longer OUR fenced write. (In real use a process only
	// writes a workflow it owns; this asserts the state lifecycle, not a policy.)
	s.clearToken(wf)
	require.NoError(t, s.Save(d), "after clearToken, this process no longer holds a fenceable claim")
}

// TestClaimStore_InterfaceAssertion — an mp SQLiteStore satisfies the ClaimStore contract shape
// at the type level (the methods are defined by ph75; ph74 defines the interface). ph74 does NOT
// implement Claim/Renew/Release yet, so this only checks the interface is well-formed + the
// FencingToken/errors are exported. (A compile-time contract check.)
func TestClaimStore_ContractDefined(t *testing.T) {
	// The interface + types compile and are exported (contract frozen for ph75).
	var _ FencingToken = 1
	require.True(t, errors.Is(ErrFencedOut, ErrFencedOut))
	require.True(t, errors.Is(ErrClaimLost, ErrClaimLost))
	// ClaimStore is defined; ph75 will make *SQLiteStore satisfy it. Assert the interface exists
	// by declaring a nil of it (compiles iff the interface type is defined).
	var _ ClaimStore
}
