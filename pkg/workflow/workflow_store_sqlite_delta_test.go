package workflow

// M15 ph69 — SaveDeltaCheckpoint (the O(N) fast path) correctness. The bites:
//  (1) fast-path ≡ full SaveCheckpoint byte-identical over the M10/M11/M12 fixture incl the
//      2 hidden KV writes (current_level_*, signal payload);
//  (2) the delta accumulator captures ALL domains via the mutators (node/data/wait);
//  (3) absence⇒DELETE (a consumed wait / removed key vanishes);
//  (4) the O(N) wall-clock is proven by the benchmark (bench file); here: correctness.
// Runs non-race (single-writer conn); the accumulator is w.mu-guarded (exercised -race in suite).

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSQLiteDelta_FailedTxnDoesNotAdvanceShadow — ph69-F1 regression (HARDENED per
// F-M15-P69-QA-2). A delta whose txn fails MID-FLIGHT — AFTER at least one row's UPSERT has
// already executed — must leave s.shadow[id] at the pre-failure committed frontier (the
// ph67-F1 post-commit-advance invariant). The bug this guards: the pre-fix code advanced the
// shadow IN-PLACE as each row was processed, so a row that executed BEFORE a later failure
// leaked into the shadow even though the txn rolled back.
//
// The prior version closed the DB → BeginTx failed BEFORE any row → a row never "succeeded"
// first, so the leak never occurred and the guard was vacuous (the pre-fix bug PASSED it).
// This version installs a trigger that RAISEs on a sentinel data value, so within the delta
// txn: the workflow row UPSERTs, node "a" UPSERTs SUCCESSFULLY (this is the row the pre-fix
// bug would leak), THEN the data_kv UPSERT of the sentinel key fires the trigger and errors —
// a genuine mid-txn failure AFTER a successful row. The correct (post-commit-apply) code keeps
// the shadow at "a"=Completed; the pre-fix code would leak "a"=Failed.
func TestSQLiteDelta_FailedTxnDoesNotAdvanceShadow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	applyDeltaLevel(t, s, d, func(d *WorkflowData) { d.SetNodeStatus("a", Completed) })

	// The committed frontier before the failing level.
	s.mu.Lock()
	frontier := s.shadow["wf"].clone()
	s.mu.Unlock()

	// A trigger that ABORTS any data_kv insert of the sentinel key. nodes UPSERTs happen
	// BEFORE data_kv in SaveDeltaCheckpoint, so node "a" succeeds first, then the sentinel
	// data row fires this → a mid-txn failure after a successful row (the pre-fix leak window).
	_, err = s.db.ExecContext(ctx,
		`CREATE TRIGGER boom BEFORE INSERT ON data_kv WHEN NEW.key='boom'
		 BEGIN SELECT RAISE(ABORT, 'seeded mid-txn failure'); END`)
	require.NoError(t, err)

	d.SetNodeStatus("a", Failed) // node "a" UPSERT will succeed (the row a pre-fix bug leaks)
	d.Set("boom", int64(1))      // this data_kv UPSERT fires the trigger → mid-txn abort
	changed, _ := d.drainDeltaCapture()
	require.Error(t, s.SaveDeltaCheckpoint(changed, d), "mid-txn trigger abort must error + roll back")

	// ph69-fixtail-F1: the bite depends on node "a" UPSERTing BEFORE the boom data row aborts —
	// SaveDeltaCheckpoint processes changed.Nodes before changed.DataKeys (marked load-bearing in
	// delta.go). If a reorder ever broke that, the DB-rollback assertion below (F2) still catches
	// a genuine leak, so this guard does not silently re-vacuum on a reorder.
	s.mu.Lock()
	after := s.shadow["wf"]
	s.mu.Unlock()
	require.Equal(t, frontier.nodes["a"].status, after.nodes["a"].status,
		"ph69-F1: a mid-txn-failed delta must NOT leak the already-UPSERTed node 'a' into the shadow (still the frontier's Completed, not Failed)")
	require.Equal(t, string(Completed), string(after.nodes["a"].status), "sanity: frontier node a is Completed")
	_, leaked := after.data["boom"]
	require.False(t, leaked, "ph69-F1: the failed data change must NOT be in the shadow")

	// ph69-fixtail-F2: also assert the DURABLE DB rolled back (the txn-atomicity half — not just
	// the in-memory shadow). RAISE(ABORT) rolls back the whole txn, so Load must still show the
	// frontier: node "a"=Completed, no boom key.
	loaded, lerr := s.Load("wf")
	require.NoError(t, lerr)
	st, _ := loaded.GetNodeStatus("a")
	require.Equal(t, Completed, st, "ph69-F2: the durable DB must show the rolled-back frontier (a=Completed)")
	_, dbHasBoom := Get(loaded, NewKey[int64]("boom"))
	require.False(t, dbHasBoom, "ph69-F2: the aborted data write must NOT be durable")
}

// TestSQLiteDelta_BatchedCadenceFires — ph69-F2 regression (HARDENED per F-M15-P69-QA-3).
// The delta path under SQLiteBatched(K) must fire the every-Kth wal_checkpoint(TRUNCATE)
// (else the ≤K power-loss bound is hollow + the WAL grows unbounded).
//
// The prior version measured "pending frames" via `wal_checkpoint(TRUNCATE)` — which itself
// truncates the WAL, so it read 0 REGARDLESS of whether the cadence fired (a vacuous guard:
// qa removed the cadence and it stayed green). This version observes the WAL *file size on
// disk* — a NON-mutating measurement: before the Kth delta the -wal file has grown (frames
// pending); the Kth delta's TRUNCATE checkpoint shrinks it back to ~empty. If the cadence
// does NOT fire, the file stays grown → the test REDDENS. (Bite-verified: removing the F2
// cadence block leaves the -wal file grown after the Kth level → this assertion fails.)
func TestSQLiteDelta_BatchedCadenceFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	walPath := path + "-wal"
	const k = 5
	s, err := NewSQLiteStore(path, SQLiteBatched(k))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	walSize := func() int64 {
		fi, statErr := os.Stat(walPath)
		if statErr != nil {
			return 0 // -wal absent = 0 pending
		}
		return fi.Size()
	}

	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	// Levels 1..K-1: the cadence has NOT fired yet → the WAL grows.
	for i := 0; i < k-1; i++ {
		applyDeltaLevel(t, s, d, func(d *WorkflowData) { d.SetNodeStatus(fmt.Sprintf("n%d", i), Completed) })
	}
	grown := walSize()
	require.Greater(t, grown, int64(0), "before the cadence fires, the WAL must have pending frames on disk")

	// The Kth level: the cadence fires wal_checkpoint(TRUNCATE) → the -wal file shrinks.
	applyDeltaLevel(t, s, d, func(d *WorkflowData) { d.SetNodeStatus(fmt.Sprintf("n%d", k-1), Completed) })
	afterTruncate := walSize()
	require.Less(t, afterTruncate, grown,
		"ph69-F2: the Kth Batched delta must fire wal_checkpoint(TRUNCATE), shrinking the -wal file (got %d, was %d)",
		afterTruncate, grown)
}

// applyDeltaLevel simulates one executor level: mutate d (recording into the accumulator),
// drain the changed-set, and SaveDeltaCheckpoint it. Returns nothing; d + store advance.
func applyDeltaLevel(t *testing.T, s *SQLiteStore, d *WorkflowData, mutate func(*WorkflowData)) {
	t.Helper()
	mutate(d)
	changed, active := d.drainDeltaCapture()
	require.True(t, active, "capture must be active")
	require.NoError(t, s.SaveDeltaCheckpoint(changed, d))
}

// TestSQLiteDelta_FastPathEqualsFullSaveCheckpoint — BITE 1. A sequence of delta levels
// (via the accumulator) Loads byte-identical to a full SaveCheckpoint of the same final
// state — over the full M10/M11/M12 durable surface + the 2 hidden KV writes.
func TestSQLiteDelta_FastPathEqualsFullSaveCheckpoint(t *testing.T) {
	dir := t.TempDir()
	fast, err := NewSQLiteStore(filepath.Join(dir, "fast.db"))
	require.NoError(t, err)
	defer fast.Close() //nolint:errcheck // test cleanup

	d := NewWorkflowData("wf")
	d.beginDeltaCapture()

	// The evolve sequence mirrors evolveFixture + the 2 HIDDEN KV writes the executor makes.
	steps := []func(*WorkflowData){
		func(d *WorkflowData) {
			d.Set("current_level_wf", "Level 0") // HIDDEN KV #1 (dag.go)
			d.SetNodeStatus("a", Pending)
			d.SetNodeStatus("b", Pending)
		},
		func(d *WorkflowData) {
			d.Set("current_level_wf", "Level 1")
			d.SetNodeStatus("a", Completed)
			d.SetOutput("a", int64(math.MaxInt64))
			d.Set("counter", int64(1))
		},
		func(d *WorkflowData) {
			d.SetNodeStatus("b", Waiting)
			d.SetWait("b", int64(1893456000000000000))
			d.Set("sig:b:payload", "hello") // HIDDEN KV #2 (signal.go — user-arbitrary key)
			d.Set("counter", int64(2))
		},
		func(d *WorkflowData) {
			d.SetNodeStatus("b", Completed)
			d.ClearWait("b") // consumed → absence⇒DELETE
			d.SetNodeStatus("c", Bypassed)
		},
		func(d *WorkflowData) {
			d.SetNodeStatus("a", Compensated)
			d.SetRollingBack(true)
			d.SetTriggerCause(TriggerCanceled)
			d.SetNodeStatus("d", CompensationFailed)
		},
	}
	for _, step := range steps {
		applyDeltaLevel(t, fast, d, step)
	}
	d.endDeltaCapture()

	// Oracle: a fresh store, ONE full SaveCheckpoint of the final state.
	full, err := NewSQLiteStore(filepath.Join(dir, "full.db"))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.SaveCheckpoint(d))

	fastLoaded, err := fast.Load("wf")
	require.NoError(t, err)
	fullLoaded, err := full.Load("wf")
	require.NoError(t, err)
	fastSnap, err := fastLoaded.Snapshot()
	require.NoError(t, err)
	fullSnap, err := fullLoaded.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(fastSnap, fullSnap),
		"delta fast-path must Load byte-identical to a full SaveCheckpoint\nfast: %s\nfull: %s", fastSnap, fullSnap)

	// Spot-assert the 2 hidden KVs survived the fast path.
	lvl, ok := Get(fastLoaded, NewKey[string]("current_level_wf"))
	require.True(t, ok)
	require.Equal(t, "Level 1", lvl, "hidden KV #1 (current_level) captured via Set")
	pay, ok := Get(fastLoaded, NewKey[string]("sig:b:payload"))
	require.True(t, ok)
	require.Equal(t, "hello", pay, "hidden KV #2 (signal payload) captured via Set")
}

// TestSQLiteDelta_AbsenceDeletes — BITE 3. A reported key absent from d ⇒ the store DELETEs
// its row. A consumed wait (ClearWait) and a node that loses all state must vanish.
func TestSQLiteDelta_AbsenceDeletes(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	applyDeltaLevel(t, s, d, func(d *WorkflowData) {
		d.SetNodeStatus("w", Waiting)
		d.SetWait("w", 12345)
	})
	// The wait is present.
	loaded, err := s.Load("wf")
	require.NoError(t, err)
	_, ok := loaded.GetWait("w")
	require.True(t, ok, "wait present after first delta")

	// Consume it → ClearWait records the touch; d no longer has it ⇒ DELETE.
	applyDeltaLevel(t, s, d, func(d *WorkflowData) {
		d.SetNodeStatus("w", Completed)
		d.ClearWait("w")
	})
	loaded, err = s.Load("wf")
	require.NoError(t, err)
	_, ok = loaded.GetWait("w")
	require.False(t, ok, "consumed wait must be DELETEd (absence⇒delete)")
	st, _ := loaded.GetNodeStatus("w")
	require.Equal(t, Completed, st, "the node's status update survived the same delta")
}

// TestSQLiteDelta_DeleteCaptured — ph69-AF1 regression (the adversarial-tester's find). A
// data key removed via Delete (a 6th mutator, NOT one of the original 4) MUST be recorded
// by the accumulator so the fast path re-reads d, finds it absent, and emits the DELETE —
// else the stale row survives and the fast-path DB diverges from a full SaveCheckpoint.
func TestSQLiteDelta_DeleteCaptured(t *testing.T) {
	dir := t.TempDir()
	fast, err := NewSQLiteStore(filepath.Join(dir, "fast.db"))
	require.NoError(t, err)
	defer fast.Close() //nolint:errcheck // test cleanup

	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	applyDeltaLevel(t, fast, d, func(d *WorkflowData) {
		d.Set("k", int64(1))
		d.Set("keep", int64(9))
		d.SetNodeStatus("a", Completed)
	})
	// Delete k via the 6th mutator; keep "keep".
	applyDeltaLevel(t, fast, d, func(d *WorkflowData) { d.Delete("k") })
	d.endDeltaCapture()

	full, err := NewSQLiteStore(filepath.Join(dir, "full.db"))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.SaveCheckpoint(d))

	fl, err := fast.Load("wf")
	require.NoError(t, err)
	ul, err := full.Load("wf")
	require.NoError(t, err)
	fs, err := fl.Snapshot()
	require.NoError(t, err)
	us, err := ul.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(us, fs),
		"Delete must be captured so the fast path emits the DELETE\nfast: %s\nfull: %s", fs, us)
	_, ok := Get(fl, NewKey[int64]("k"))
	require.False(t, ok, "deleted key k must be gone from the fast-path DB")
	v, ok := Get(fl, NewKey[int64]("keep"))
	require.True(t, ok)
	require.Equal(t, int64(9), v, "untouched key survives")
}

// TestSQLiteDelta_ResumeThenDelta — resume-into on the fast path: partial deltas, close
// (crash), reopen, Load (rehydrates the shadow), continue with more deltas → byte-identical
// to a full SaveCheckpoint of the final state (the cross-path shadow coherence holds).
func TestSQLiteDelta_ResumeThenDelta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")

	s1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	applyDeltaLevel(t, s1, d, func(d *WorkflowData) { d.SetNodeStatus("a", Completed); d.Set("k", int64(1)) })
	applyDeltaLevel(t, s1, d, func(d *WorkflowData) { d.SetNodeStatus("b", Running); d.Set("k", int64(2)) })
	require.NoError(t, s1.Close())

	// Reopen, Load (rehydrates shadow), continue with a fresh capture on the loaded data.
	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup
	resumed, err := s2.Load("wf")
	require.NoError(t, err)
	resumed.beginDeltaCapture()
	applyDeltaLevel(t, s2, resumed, func(d *WorkflowData) { d.SetNodeStatus("b", Completed); d.Set("k", int64(3)) })
	resumed.endDeltaCapture()

	full, err := NewSQLiteStore(filepath.Join(dir, "full.db"))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.SaveCheckpoint(resumed))

	got, err := s2.Load("wf")
	require.NoError(t, err)
	want, err := full.Load("wf")
	require.NoError(t, err)
	gs, err := got.Snapshot()
	require.NoError(t, err)
	ws, err := want.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(gs, ws),
		"resume-then-delta must Load byte-identical to a full SaveCheckpoint\ngot:  %s\nwant: %s", gs, ws)
}
