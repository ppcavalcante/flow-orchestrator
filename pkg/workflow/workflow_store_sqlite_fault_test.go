package workflow

// M16 ph78 — CLASS-A fault injection (task #126): the SQLite store's DB-error and fsync-failure
// branches. Every `if err != nil { return ErrIO }` after an Exec/Query/Commit is unreachable with
// the real driver; the fault driver (faultdriver_test.go) injects a chosen error on the Nth matching
// call so each branch is EXERCISED and asserts (a) the TYPED error surfaces and (b) the store stays
// CONSISTENT — the txn rolled back, no partial write, no panic.
//
// ANTI-VACUITY (ph78 bar): each test's error-handling guard is load-bearing — if the store SWALLOWED
// the injected error (removed the `if err != nil` return), the txn would commit partial state / a
// later Load would see it, and the consistency assertion below reddens. The injected error IS the
// seed-break (it forces the exact failure the guard defends against).

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// errFaultInjected is a synthetic driver error the injected faults return.
var errFaultInjected = errors.New("faultdriver: injected DB error")

// mkFaultStore opens a store through the fault driver (real DB file, injectable errors).
func mkFaultStore(t *testing.T, opts ...SQLiteOption) *SQLiteStore {
	t.Helper()
	drv := registerFaultDriver()
	all := append([]SQLiteOption{withSQLiteDriverName(drv)}, opts...)
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fault.db"), all...)
	require.NoError(t, err, "open through the fault driver (no fault armed yet)")
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// TestFault_SaveInsertError_ErrIO_Rollback — an INSERT failure mid-Save → Save returns ErrIO AND the
// txn rolls back (no partial workflow row lands). Covers Save's insert-error branch.
func TestFault_SaveInsertError_ErrIO_Rollback(t *testing.T) {
	s := mkFaultStore(t)

	// Seed a clean prior state so we can prove the failed Save did NOT corrupt it.
	clean := NewWorkflowData("wf")
	clean.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(clean), "clean baseline Save")

	// Arm: fail the first INSERT INTO nodes on the NEXT Save.
	disarm := armFault(&faultPlan{kind: "exec", sqlSubstr: "INSERT INTO nodes", err: errFaultInjected})
	defer disarm()

	bad := NewWorkflowData("wf")
	bad.SetNodeStatus("n0", Failed) // would overwrite the baseline if it landed
	bad.SetNodeStatus("n1", Running)
	saveErr := s.Save(bad)

	require.Error(t, saveErr, "a mid-txn INSERT failure must surface, not be swallowed")
	require.ErrorIs(t, saveErr, ErrIO, "an injected DB Exec error maps to ErrIO")
	disarm() // stop injecting so the verification Load runs clean

	// Consistency: the failed Save rolled back → the durable state is STILL the clean baseline
	// (no partial write of the Failed/Running rows). This is the seed-break bite: if Save swallowed
	// the error, the partial rows would be here and this reddens.
	got, err := s.Load("wf")
	require.NoError(t, err)
	st, _ := got.GetNodeStatus("n0")
	require.Equal(t, Completed, st, "rollback: n0 stays Completed (the baseline), NOT the failed Save's Failed")
	_, has := got.GetNodeStatus("n1")
	require.False(t, has, "rollback: n1 (only in the failed Save) did NOT land — no partial write")
}

// TestFault_SaveBeginError_ErrIO — a BeginTx failure → Save returns ErrIO (via classifyTxErr("begin")),
// nothing written. modernc starts the txn via ConnBeginTx (not an Exec("BEGIN")), so this uses the
// fault driver's "begin" kind, injected in beginCtx (review ph78-F3 — previously this test skipped and
// the begin-branch was uncovered).
func TestFault_SaveBeginError_ErrIO(t *testing.T) {
	s := mkFaultStore(t)
	disarm := armFault(&faultPlan{kind: "begin", err: errFaultInjected})
	defer disarm()

	err := s.Save(NewWorkflowData("wf"))
	require.Error(t, err, "a BeginTx failure must surface")
	require.ErrorIs(t, err, ErrIO, "a generic begin failure maps to ErrIO (classifyTxErr non-busy arm)")
}

// TestFault_SaveBeginBusy_ErrBusy — the M16 competing-consumers branch (review ph78-F3): a contended
// BEGIN IMMEDIATE that surfaces SQLITE_BUSY at the begin boundary must classify as ErrBusy (transient,
// retryable), NOT ErrIO — so a competing consumer retries the drive rather than aborting. Exercises
// classifyTxErr("begin")'s busy arm, which the generic-begin test above does not.
func TestFault_SaveBeginBusy_ErrBusy(t *testing.T) {
	s := mkFaultStore(t, WithMultiProcess())
	// A synthetic SQLITE_BUSY at begin (the isBusy string fallback matches the token).
	disarm := armFault(&faultPlan{kind: "begin", err: errors.New("SQLITE_BUSY: database is locked")})
	defer disarm()

	err := s.Save(NewWorkflowData("wf"))
	require.Error(t, err, "a busy BeginTx must surface")
	require.ErrorIs(t, err, ErrBusy, "a SQLITE_BUSY at begin classifies as ErrBusy (transient), not ErrIO")
	require.NotErrorIs(t, err, ErrIO, "the busy-begin path is ErrBusy, not opaque ErrIO")
}

// TestFault_LoadQueryError_ErrCorruptData — a QueryContext failure during Load → Load returns
// ErrCorruptData (not a panic, not partial data). Covers Load's query-error branch. NOTE the store's
// deliberate TWO-DOMAIN taxonomy: a WRITE failure (Save/Delete/Sync) is ErrIO, but a READ failure on
// Load is ErrCorruptData ("the durable journal could not be read" = the corrupt-data domain, per the
// M15 store error design). This test pins that boundary rather than assuming a single ErrIO.
func TestFault_LoadQueryError_ErrCorruptData(t *testing.T) {
	s := mkFaultStore(t)
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d))

	// Fail the nodes SELECT on Load.
	disarm := armFault(&faultPlan{kind: "query", sqlSubstr: "FROM nodes", err: errFaultInjected})
	defer disarm()

	_, err := s.Load("wf")
	require.Error(t, err, "a query failure during Load must surface")
	require.ErrorIs(t, err, ErrCorruptData, "an injected Load-query error maps to ErrCorruptData (the read domain), not ErrIO")
	require.NotErrorIs(t, err, ErrIO, "the read-failure domain is ErrCorruptData, distinct from the write-failure ErrIO")
}

// TestFault_SyncFsyncError_ErrIO — the FSYNC-FAILURE path: wal_checkpoint(TRUNCATE) fails → Sync
// returns ErrIO. Covers walCheckpointFullLocked's error branch (the durability floor's fault path).
func TestFault_SyncFsyncError_ErrIO(t *testing.T) {
	s := mkFaultStore(t, SQLiteBatched(64)) // Batched → Sync does a real wal_checkpoint
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d))

	// Fail the wal_checkpoint PRAGMA (the fsync boundary).
	disarm := armFault(&faultPlan{kind: "exec", sqlSubstr: "wal_checkpoint", err: errFaultInjected})
	defer disarm()

	err := s.Sync("wf")
	require.Error(t, err, "an fsync (wal_checkpoint) failure must surface — a swallowed one is a silent durability lie")
	require.ErrorIs(t, err, ErrIO, "the fsync-failure path returns ErrIO")
}

// TestFault_DeleteError_ErrIO — a DELETE failure → Delete returns ErrIO.
func TestFault_DeleteError_ErrIO(t *testing.T) {
	s := mkFaultStore(t)
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d))

	disarm := armFault(&faultPlan{kind: "exec", sqlSubstr: "DELETE", err: errFaultInjected})
	defer disarm()

	err := s.Delete("wf")
	require.Error(t, err, "a DELETE failure must surface")
	require.ErrorIs(t, err, ErrIO, "an injected Delete error maps to ErrIO")
}

// TestFault_DeltaCheckpointExecError_ErrIO_ShadowNotAdvanced — a mid-txn Exec failure during the
// INCREMENTAL fast path (SaveDeltaCheckpoint → saveIncremental) → ErrIO AND the shadow stays at the
// committed frontier (no leaked delta). Covers saveIncremental's diff-Exec-error branches (the big
// uncovered chunk) + the ph67-F1 post-commit-advance invariant under a driver-injected fault.
func TestFault_DeltaCheckpointExecError_ErrIO_ShadowNotAdvanced(t *testing.T) {
	s := mkFaultStore(t)

	// Commit a first delta level cleanly so the shadow has a frontier + the incremental path is live.
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	d.SetNodeStatus("a", Completed)
	changed, _ := d.drainDeltaCapture()
	require.NoError(t, s.SaveDeltaCheckpoint(changed, d), "clean first delta level")

	s.mu.Lock()
	frontier := s.shadow["wf"].clone()
	s.mu.Unlock()

	// Arm: fail the nodes UPSERT on the NEXT delta (the incremental diff write).
	disarm := armFault(&faultPlan{kind: "exec", sqlSubstr: "INTO nodes", err: errFaultInjected})
	defer disarm()

	d.beginDeltaCapture()
	d.SetNodeStatus("a", Failed) // a delta that must NOT land
	d.SetNodeStatus("b", Running)
	changed2, _ := d.drainDeltaCapture()
	err := s.SaveDeltaCheckpoint(changed2, d)
	require.Error(t, err, "a mid-delta Exec failure must surface, not silently drop the level")
	require.ErrorIs(t, err, ErrIO, "an injected delta Exec error maps to ErrIO")
	disarm()

	// Consistency (the seed-break bite): the failed delta rolled back → the shadow is UNCHANGED at
	// the frontier (a="Completed"), NOT the failed delta's a="Failed"/b. A swallowed error would
	// leak the partial delta and this reddens.
	s.mu.Lock()
	after := s.shadow["wf"]
	s.mu.Unlock()
	require.Equal(t, frontier.nodes["a"].status, after.nodes["a"].status,
		"rollback: the mid-txn-failed delta did NOT advance the shadow (a stays Completed, not Failed)")

	// And the durable journal likewise stayed at the frontier.
	got, err := s.Load("wf")
	require.NoError(t, err)
	stA, _ := got.GetNodeStatus("a")
	require.Equal(t, Completed, stA, "durable rollback: a stays Completed")
	_, hasB := got.GetNodeStatus("b")
	require.False(t, hasB, "durable rollback: b (only in the failed delta) did not land")
}

// TestFault_ListWorkflowsQueryError_ErrIO — a query failure in ListWorkflows → ErrIO (not a panic /
// partial list). Covers ListWorkflows' query-error branch.
func TestFault_ListWorkflowsQueryError_ErrIO(t *testing.T) {
	s := mkFaultStore(t)
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d))

	disarm := armFault(&faultPlan{kind: "query", sqlSubstr: "FROM workflows", err: errFaultInjected})
	defer disarm()

	_, err := s.ListWorkflows()
	require.Error(t, err, "a query failure in ListWorkflows must surface")
	require.ErrorIs(t, err, ErrIO, "an injected ListWorkflows query error maps to ErrIO")
}

// TestFault_DeltaDataAndWaitsExecError — the delta path's data_kv and waits diff-Exec error branches
// (diffData/diffWaits). A fault on each surfaces ErrIO and rolls back. Covers the diff helpers'
// uncovered error arms.
func TestFault_DeltaDataAndWaitsExecError(t *testing.T) {
	for _, tc := range []struct{ name, substr string }{
		{"data_kv", "INTO data_kv"},
		{"waits", "INTO waits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mkFaultStore(t)
			d := NewWorkflowData("wf")
			d.beginDeltaCapture()
			d.SetNodeStatus("a", Completed)
			changed, _ := d.drainDeltaCapture()
			require.NoError(t, s.SaveDeltaCheckpoint(changed, d), "clean first level")

			disarm := armFault(&faultPlan{kind: "exec", sqlSubstr: tc.substr, err: errFaultInjected})
			defer disarm()

			d.beginDeltaCapture()
			d.Set("k", int64(7))  // drives an INTO data_kv
			d.SetWait("a", 12345) // drives an INTO waits
			changed2, _ := d.drainDeltaCapture()
			err := s.SaveDeltaCheckpoint(changed2, d)
			require.Error(t, err, "a %s diff-Exec failure must surface", tc.name)
			require.ErrorIs(t, err, ErrIO, "an injected %s diff error maps to ErrIO", tc.name)
		})
	}
}

// TestFault_HydrateShadowQueryError — a cold-cache delta (reopen WITHOUT Load, then
// SaveDeltaCheckpoint) hydrates the shadow from the DB first; a query failure there surfaces
// (not a panic). Covers hydrateShadowFromDB's error branch.
func TestFault_HydrateShadowQueryError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hydrate.db")
	drv := registerFaultDriver()

	// Commit a workflow with a clean store (no fault).
	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	require.NoError(t, s1.Save(d))
	require.NoError(t, s1.Close())

	// Reopen through the fault driver — a COLD shadow (no Load). The next delta must hydrate.
	s2, err := NewSQLiteStore(dbPath, withSQLiteDriverName(drv))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup

	// Fail a hydrate query (the shadow rebuild reads nodes/data/waits).
	disarm := armFault(&faultPlan{kind: "query", sqlSubstr: "FROM nodes", err: errFaultInjected})
	defer disarm()

	d2 := NewWorkflowData("wf")
	d2.beginDeltaCapture()
	d2.SetNodeStatus("b", Running)
	changed, _ := d2.drainDeltaCapture()
	err = s2.SaveDeltaCheckpoint(changed, d2)
	require.Error(t, err, "a hydrate query failure on a cold-cache delta must surface, not panic")
}

// TestFault_SaveCommitError_ErrIO — a Commit failure at the end of Save → ErrIO surfaces to the
// caller (covers Save's commit-error branch). SCOPE (review ph78-F4): this validates ERROR-BRANCH
// PROPAGATION ONLY, not real-commit-failure durability. The fault driver's faultTx.Commit returns the
// synthetic error WITHOUT calling the real Commit — so the underlying driver txn is abandoned (neither
// committed nor rolled back through the wrapper) and the post-state is IMPLEMENTATION-DEFINED (an
// abandoned txn on the maxOpenConns=1 pooled conn, reset-on-reuse dependent). Hence we assert only
// that (a) the typed ErrIO reaches the caller — so a real caller learns the commit was uncertain and
// re-Loads — and (b) the store stays readable (no corruption/panic). We deliberately do NOT assert
// all-or-nothing rollback: at a genuine SQLite commit boundary the write may or may not have landed
// (Rollback-after-failed-Commit is a no-op / ErrTxDone), so a rollback assertion would be unsound.
func TestFault_SaveCommitError_ErrIO(t *testing.T) {
	s := mkFaultStore(t)
	clean := NewWorkflowData("wf")
	clean.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(clean), "baseline")

	disarm := armFault(&faultPlan{kind: "commit", err: errFaultInjected})
	defer disarm()
	bad := NewWorkflowData("wf")
	bad.SetNodeStatus("n0", Failed)
	err := s.Save(bad)
	require.Error(t, err, "a commit failure must surface")
	require.ErrorIs(t, err, ErrIO, "an injected commit error maps to ErrIO — the caller learns the commit was uncertain")
	require.NotErrorIs(t, err, ErrBusy, "a generic commit failure is ErrIO, not ErrBusy")
	disarm()

	// The store stays READABLE after a commit fault (no corruption / panic on the next Load) — the
	// consistency property we CAN guarantee at the commit boundary.
	_, loadErr := s.Load("wf")
	require.NoError(t, loadErr, "the store is still readable after a commit fault (no corruption)")
}

// TestDeltaNodeDelete_RemovedNodeVanishes — a node present at level N but absent at level N+1 is
// DELETEd by the diff (absence⇒delete). Covers diffNodes' delete-branch (real behavior, not a fault).
func TestDeltaNodeDelete_RemovedNodeVanishes(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "del.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup

	// Level 1: two nodes.
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	d.SetNodeStatus("a", Completed)
	d.SetNodeStatus("b", Completed)
	c1, _ := d.drainDeltaCapture()
	require.NoError(t, s.SaveDeltaCheckpoint(c1, d))

	// Level 2: a fresh snapshot with only "a" (b removed) → the delta must DELETE b.
	d2 := NewWorkflowData("wf")
	d2.beginDeltaCapture()
	d2.SetNodeStatus("a", Completed)
	// re-drive against the store's shadow: mark b absent by diffing a snapshot without it.
	c2, _ := d2.drainDeltaCapture()
	// The changed-set from d2 alone won't carry b's removal; drive a full SaveCheckpoint which diffs
	// the whole snapshot against the shadow → b (in shadow, not in d2) is DELETEd.
	_ = c2
	require.NoError(t, s.SaveCheckpoint(d2), "full checkpoint diffs the snapshot → removed b is deleted")

	got, err := s.Load("wf")
	require.NoError(t, err)
	_, hasB := got.GetNodeStatus("b")
	require.False(t, hasB, "node b, absent at level 2, was DELETEd by the diff")
	stA, _ := got.GetNodeStatus("a")
	require.Equal(t, Completed, stA, "node a survives")
}
