package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustSQLiteSignals opens a fresh SQLite store for the signal tests.
func mustSQLiteSignals(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sig.db"))
	require.NoError(t, err)
	return s
}

// TestSQLiteSignals_EarlyBuffering — the no-FK decision (DEC-P93-SEPARATE-SIGNALS-TABLE-NO-FK):
// a signal may be delivered for a workflow_id with NO workflows row (deliver-before-exists), and a
// later TakeSignals finds it. Proves the signals table has no FK to workflows.
func TestSQLiteSignals_EarlyBuffering(t *testing.T) {
	store := mustSQLiteSignals(t)
	const wf = "wf-never-saved" // NO Save — the workflows row does not exist

	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "early", Name: "go", Payload: "buffered"}),
		"deliver-before-exists must succeed (no FK to workflows)")
	got, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "early", got[0].ID)
	assert.Equal(t, "buffered", got[0].Payload)
}

// TestSQLiteSignals_Dedupe — idempotent by sig.ID (ON CONFLICT DO UPDATE = LAST-writer-wins, the
// uniform cross-store behavior): re-delivering the same sig_id is one row with the LATEST content;
// a different sig_id adds a row. (The cross-store conformance suite's IdempotentByID pins the
// winner uniformly; this pins the SQLite ON CONFLICT path explicitly.)
func TestSQLiteSignals_Dedupe(t *testing.T) {
	store := mustSQLiteSignals(t)
	const wf = "wf-dedupe"

	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "dup", Name: "n1", Payload: "first"}))
	// Re-deliver the SAME id with DIFFERENT content → ON CONFLICT DO UPDATE keeps the LATEST.
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "dup", Name: "n2", Payload: "second"}))
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "other", Name: "n3", Payload: "x"}))

	got, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, 2, "same sig_id deduped to one row; a distinct id adds one")
	// Sorted by sig_id: "dup" < "other". The LAST delivery's content wins (uniform with the other stores).
	assert.Equal(t, "dup", got[0].ID)
	assert.Equal(t, "n2", got[0].Name, "last-writer-wins: the re-deliver's content replaces the first")
	assert.Equal(t, "second", got[0].Payload)
}

// TestSQLiteSignals_CrashSurvival — durability across process death: deliver a signal, CLOSE the
// store, REOPEN a fresh *SQLiteStore on the same file → TakeSignals still returns it. The mailbox
// is durable (persisted rows), independent of any running instance.
func TestSQLiteSignals_CrashSurvival(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "durable.db")

	store1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	require.NoError(t, store1.DeliverSignal("wf-crash", Signal{ID: "s1", Name: "go", Payload: int64(42)}))
	require.NoError(t, store1.Close())

	// Reopen a NEW store on the same file — the resume path after a crash.
	store2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer store2.Close() //nolint:errcheck // test cleanup
	got, err := store2.TakeSignals("wf-crash")
	require.NoError(t, err)
	require.Len(t, got, 1, "the delivered signal survives a store close+reopen (durable)")
	assert.Equal(t, "s1", got[0].ID)
	iv, cerr := payloadAsInt64(got[0].Payload)
	require.NoError(t, cerr)
	assert.Equal(t, int64(42), iv, "int64 payload survives the durable round-trip")
}

// TestSQLiteSignals_MailboxOutsideSnapshot — the load-bearing MH37-1 invariant: a Save/checkpoint
// (which rewrites the nodes/data_kv snapshot rows) NEVER touches the signals table. A delivered
// signal cannot be clobbered by a concurrent snapshot rewrite. Seed-break: if the mailbox lived in
// a snapshot-rewritten table (data_kv), a Save of a snapshot NOT containing it would drop it — this
// test proves the SEPARATE table is unaffected by a Save.
func TestSQLiteSignals_MailboxOutsideSnapshot(t *testing.T) {
	store := mustSQLiteSignals(t)
	const wf = "wf-outside"

	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "keep"}))

	// A full snapshot Save (rewrites the snapshot tables for wf) must NOT touch the signal.
	data := NewWorkflowData(wf)
	data.SetNodeStatus("someNode", Completed)
	data.Set("someKey", "someVal")
	require.NoError(t, store.Save(data))

	// The signal survives the snapshot rewrite (mailbox-outside-snapshot).
	got, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, 1, "a Save/checkpoint rewrite never clobbers the separate signals table (MH37-1)")
	assert.Equal(t, "keep", got[0].Payload)

	// And a SECOND Save (a re-checkpoint) still leaves it — proving repeated snapshot rewrites are
	// orthogonal to the mailbox (the bite: were the mailbox in data_kv, this Save would drop it).
	require.NoError(t, store.Save(NewWorkflowData(wf))) // an EMPTY snapshot — no signal key in it
	got2, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got2, 1, "even an empty-snapshot Save leaves the mailbox intact (separate table)")
}

// TestApprovals_OnSQLite_GapClosed — ⭐ THE HEADLINE gap-closure (closes the ph90 approvals-on-
// SQLite gap that my ph91 grounding brief flagged). Before ph93, an approval node on a *SQLiteStore
// loudly refused (ErrWaitRequiresSignalStore) because SQLite had no mailbox. Now it parks + resumes
// durably. This re-runs the ph90 approval hard bar (park→approve; reject→fail-fast) against SQLite.
func TestApprovals_OnSQLite_GapClosed(t *testing.T) {
	newSQLite := func(t *testing.T) *SQLiteStore {
		s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "approvals.db"))
		require.NoError(t, err)
		return s
	}

	t.Run("park_then_approve", func(t *testing.T) {
		store := newSQLite(t)
		var afterN atomic.Int32
		w := buildApprovalWorkflow(t, store, "wf-sql-appr", &afterN)

		// The gap-closure assertion: park (Waiting), NOT ErrWaitRequiresSignalStore.
		err := w.Execute(context.Background())
		require.ErrorIs(t, err, ErrSuspended, "on SQLite the approval now PARKS (no ErrWaitRequiresSignalStore)")
		require.NotErrorIs(t, err, ErrWaitRequiresSignalStore, "the ph90 gap is closed — SQLite has a mailbox")
		parked, lerr := store.Load("wf-sql-appr")
		require.NoError(t, lerr)
		assertNodeStatus(t, parked, "gate", Waiting)

		// Approve → resume to terminal success, durably on SQLite.
		require.NoError(t, w.DeliverAndResume(context.Background(), ApproveSignal("gate", "alice", "ship", "d1")))
		final, lerr := store.Load("wf-sql-appr")
		require.NoError(t, lerr)
		assertNodeStatus(t, final, "gate", Completed)
		assertNodeStatus(t, final, "after", Completed)
		require.EqualValues(t, 1, afterN.Load())
	})

	t.Run("reject_fail_fast", func(t *testing.T) {
		store := newSQLite(t)
		var afterN atomic.Int32
		w := buildApprovalWorkflow(t, store, "wf-sql-rej", &afterN)
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

		err := w.DeliverAndResume(context.Background(), RejectSignal("gate", "bob", "no", "d1"))
		require.Error(t, err)
		var rej *ApprovalRejectedError
		require.True(t, errors.As(err, &rej), "reject → *ApprovalRejectedError on SQLite too")
		require.EqualValues(t, 0, afterN.Load(), "reject fail-fast: downstream never runs")
	})
}

// TestParkedSubWorkflow_OnSQLite_Smoke — ph92 parked-await works on SQLite (the WAKE mechanism on
// the durable mailbox): park → run child out-of-band → deliver completion → wake → read result.
func TestParkedSubWorkflow_OnSQLite_Smoke(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "parked.db"))
	require.NoError(t, err)
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-sql-parked", childProducing(t, int64(7), nil), &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	runChildOutOfBand(t, store, "wf-sql-parked", "sub", childProducing(t, int64(7), nil))
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")))

	final, err := store.Load("wf-sql-parked")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	got, ok := final.GetInt64("result")
	require.True(t, ok)
	assert.Equal(t, int64(7), got, "ph92 parked-await + int64 result fidelity works on SQLite")
}
