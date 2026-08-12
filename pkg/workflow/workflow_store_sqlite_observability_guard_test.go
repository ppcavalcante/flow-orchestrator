package workflow

// M18 coverage-recovery (post-ship ratchet round): the mp-mode guard on every Observability method +
// CancelRunning. Each granular read-model method (and CancelRunning) requires a multi-process store — on a
// plain (non-mp) SQLiteStore it MUST return ErrValidation, never touch the (absent) work_queue/leases tables.
// These are BITING guards: the assertion is the specific ErrValidation sentinel, not just "an error".

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newNonMPStore builds a default SQLiteStore WITHOUT WithMultiProcess — so s.dur.mp is false and the
// work_queue/leases tables do not exist. Every Observability method must reject it at the mp guard.
func newNonMPStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "nonmp.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

func TestObservability_NonMPStore_RejectsWithValidation(t *testing.T) {
	s := newNonMPStore(t)

	// Each call must return ErrValidation (the mp guard) — the read-model surface is mp-only.
	t.Run("QueueCounts", func(t *testing.T) {
		_, err := s.QueueCounts("")
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("InFlight", func(t *testing.T) {
		_, err := s.InFlight()
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("StuckWork", func(t *testing.T) {
		_, err := s.StuckWork(0, nil)
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("WorkflowStatus", func(t *testing.T) {
		_, err := s.WorkflowStatus("wf-1")
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("WorkerHealth", func(t *testing.T) {
		_, err := s.WorkerHealth()
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("Snapshot", func(t *testing.T) {
		_, err := s.Snapshot(0, nil)
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("CancelRunning", func(t *testing.T) {
		ok, err := s.CancelRunning("wf-1")
		require.ErrorIs(t, err, ErrValidation)
		require.False(t, ok)
	})
}

// TestObservability_InvalidWorkflowID — the id-validating methods reject a malformed workflow id BEFORE any
// query (a distinct guard from the mp check: mp store, bad id). Confirms validateWorkflowID is wired in.
func TestObservability_InvalidWorkflowID(t *testing.T) {
	s := mkDispatchStore(t)

	// An empty id is invalid (validateWorkflowID rejects it) — not an ErrValidation-from-mp, a real id error.
	_, err := s.WorkflowStatus("")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "workflow ID cannot be empty",
		"must be the ID guard, not the mp guard — a bare require.Error cannot tell them apart")

	ok, err := s.CancelRunning("")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "workflow ID cannot be empty")
	require.False(t, ok)
}
