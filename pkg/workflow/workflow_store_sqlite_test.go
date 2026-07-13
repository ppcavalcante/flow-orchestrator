package workflow

// M15 ph66 — SQLiteStore fidelity (the hard bar). The fixture is the BITE: it carries
// the M10/M11/M12 durable state a plain completed DAG would silently drop (a parked
// Waiting node + wait/fireAt; a Bypassed node; a Compensated node + rolling_back +
// trigger_cause; all 9 NodeStatuses; int64-boundary outputs). A vacuous fixture would
// pass byte-identity while dropping trigger_cause or a wait — this fixture precludes it.
// SQLite tests run non-race (modernc.org/sqlite + testing.T is fine; the store's single
// writer conn serializes) — no race-sensitive shared state introduced.

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fullFixture builds a WorkflowData carrying the whole M10/M11/M12 durable surface +
// all 9 statuses + int64-boundary outputs — the non-vacuous fidelity fixture.
func fullFixture(t *testing.T) *WorkflowData {
	t.Helper()
	d := NewWorkflowData("fidelity-wf")

	// All 9 NodeStatuses present.
	statuses := map[string]NodeStatus{
		"n_pending":   Pending,
		"n_running":   Running,
		"n_completed": Completed,
		"n_failed":    Failed,
		"n_skipped":   Skipped,
		"n_waiting":   Waiting,            // M10: a parked node
		"n_bypassed":  Bypassed,           // M11
		"n_comp":      Compensated,        // M12
		"n_compfail":  CompensationFailed, // M12
	}
	for node, st := range statuses {
		d.SetNodeStatus(node, st)
	}

	// M10: durable timer wait (fireAt) on the parked node — int64 magnitude.
	d.SetWait("n_waiting", int64(1893456000000000000))

	// M12: run-level rollback markers.
	d.SetRollingBack(true)
	d.SetTriggerCause(TriggerCanceled)

	// Outputs: int64 boundary values (through the real INTEGER column path when set as
	// data, and as outputs which are JSON-encoded strings) + a string + a complex.
	d.SetOutput("n_completed", "done")
	d.SetOutput("n_comp", map[string]any{"undone": true})

	// An OUTPUT-ONLY node: an output on a node with NO status entry. The FB store keeps
	// outputs/nodeStatus independent, so this node has an output but no status — the
	// SQLite round-trip must NOT invent a Pending status for it (ph66-F1). In the fixture
	// so the hard-bar byte-identity test covers the divergence class directly.
	d.SetOutput("n_output_only", "orphan-value")

	// Data keys spanning every type the FB store collapses (int→int64, float, bool,
	// string, complex→JSON) + the int64 boundaries (the type-affinity landmine).
	d.Set("balance_max", int64(math.MaxInt64))
	d.Set("balance_min", int64(math.MinInt64))
	d.Set("precise", int64(9007199254740993)) // 2^53+1
	d.Set("legacy_int", 42)                   // int → int64
	d.Set("rate", 3.14159)                    // float64
	d.Set("vip", true)                        // bool
	d.Set("region", "eu-west")                // string
	d.Set("meta", map[string]any{"k": "v"})   // complex → JSON string
	return d
}

// TestSQLite_Fidelity_vs_Snapshot — THE hard bar. A WorkflowData Save→Load through the
// SQLiteStore yields a Snapshot() byte-identical to the same WorkflowData (fresh) — so
// the decomposed round-trip == the snapshot round-trip. The fixture carries the full
// M10/M11/M12 surface, so a dropped trigger_cause/wait/status REDDENS byte-identity.
func TestSQLite_Fidelity_vs_Snapshot(t *testing.T) {
	dir := t.TempDir()
	sq, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer sq.Close() //nolint:errcheck // test cleanup
	fb, err := NewFlatBuffersStore(filepath.Join(dir, "fb"))
	require.NoError(t, err)

	orig := fullFixture(t)

	// The oracle is the EXISTING FB store's round-trip (CONTEXT: "byte-identical to the
	// same WorkflowData through the existing FB/snapshot path"), NOT the original's live
	// Snapshot — both stores collapse a complex value to its JSON string identically, so
	// the comparison must be round-trip-vs-round-trip, not round-trip-vs-original.
	require.NoError(t, fb.Save(orig))
	fbLoaded, err := fb.Load("fidelity-wf")
	require.NoError(t, err)
	fbSnap, err := fbLoaded.Snapshot()
	require.NoError(t, err)

	require.NoError(t, sq.Save(orig))
	loaded, err := sq.Load("fidelity-wf")
	require.NoError(t, err)
	loadedSnap, err := loaded.Snapshot()
	require.NoError(t, err)

	require.True(t, bytes.Equal(fbSnap, loadedSnap),
		"decomposed round-trip Snapshot must be byte-identical to the FB round-trip\nfb:     %s\nsqlite: %s",
		fbSnap, loadedSnap)

	// Spot-assert the load-bearing state explicitly (belt-and-suspenders on the bite):
	require.True(t, loaded.IsRollingBack(), "rolling_back survived")
	require.Equal(t, TriggerCanceled, loaded.TriggerCause(), "trigger_cause survived")
	fireAt, ok := loaded.GetWait("n_waiting")
	require.True(t, ok, "wait/fireAt survived")
	require.Equal(t, int64(1893456000000000000), fireAt, "fireAt int64 byte-exact")
	st, _ := loaded.GetNodeStatus("n_bypassed")
	require.Equal(t, Bypassed, st, "Bypassed status survived")
	v, ok := Get(loaded, NewKey[int64]("balance_max"))
	require.True(t, ok)
	require.Equal(t, int64(math.MaxInt64), v, "MaxInt64 byte-exact through INTEGER affinity")
	vmin, _ := Get(loaded, NewKey[int64]("balance_min"))
	require.Equal(t, int64(math.MinInt64), vmin, "MinInt64 byte-exact")
}

// TestSQLite_SnapshotSensitive_NonVacuous — proves the fixture is NON-vacuous at the
// Snapshot level: dropping a single load-bearing field (trigger_cause) changes the
// snapshot bytes. This guards that Snapshot is SENSITIVE to the field (so the fidelity
// test's byte-identity is meaningful); the store's actual preservation is guarded by
// TestSQLite_Fidelity_vs_Snapshot's round-trip + spot-asserts.
func TestSQLite_SnapshotSensitive_NonVacuous(t *testing.T) {
	orig := fullFixture(t)
	origSnap, err := orig.Snapshot()
	require.NoError(t, err)
	// Mutate a COPY missing trigger_cause → its snapshot must differ (proving the field
	// is load-bearing in the byte comparison).
	mutated := fullFixture(t)
	mutated.SetTriggerCause(TriggerNone) // drop it
	mutatedSnap, err := mutated.Snapshot()
	require.NoError(t, err)
	require.False(t, bytes.Equal(origSnap, mutatedSnap),
		"dropping trigger_cause MUST change the snapshot bytes — the fidelity test is non-vacuous")
}

func TestSQLite_ListAndDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // test cleanup

	for _, id := range []string{"a", "b", "c"} {
		d := NewWorkflowData(id)
		d.SetNodeStatus("n", Completed)
		require.NoError(t, store.Save(d))
	}
	ids, err := store.ListWorkflows()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "b", "c"}, ids)

	require.NoError(t, store.Delete("b"))
	ids, err = store.ListWorkflows()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "c"}, ids)

	// Delete-missing → ErrNotFound; Load-missing → ErrNotFound.
	require.ErrorIs(t, store.Delete("nope"), ErrNotFound)
	_, err = store.Load("nope")
	require.ErrorIs(t, err, ErrNotFound)

	// Deleted workflow's rows are gone (no orphans): re-load "b" → ErrNotFound.
	_, err = store.Load("b")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSQLite_CorruptDB_TypedError — a truncated/garbage .db → a clean typed error on
// open/load, NEVER a panic.
func TestSQLite_CorruptDB_TypedError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	// Write garbage that is NOT a valid SQLite header.
	require.NoError(t, os.WriteFile(path, []byte("this is not a sqlite database, just garbage bytes"), 0o600))

	store, err := NewSQLiteStore(path)
	if err != nil {
		// Open/schema failed on the garbage file — a typed error, not a panic. Good.
		require.ErrorIs(t, err, ErrIO)
		return
	}
	// If open somehow succeeded, a Load must still be a clean typed error, never a panic.
	defer store.Close() //nolint:errcheck // test cleanup
	require.NotPanics(t, func() {
		_, lerr := store.Load("anything")
		require.Error(t, lerr) // ErrNotFound or ErrCorruptData — either is a clean typed error
	})
}

// TestSQLite_CorruptValidDB_TypedError — a VALID SQLite DB carrying a garbage node
// status / unknown data kind → Load returns ErrCorruptData (not a panic, not a silent
// bad value). Exercises the isKnownStatus + unknown-kind guards in the codec directly.
func TestSQLite_CorruptValidDB_TypedError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // test cleanup

	// Seed a legit workflow, then corrupt one node's status via a raw driver write.
	require.NoError(t, store.Save(func() *WorkflowData {
		d := NewWorkflowData("garbage-status")
		d.SetNodeStatus("n", Completed)
		return d
	}()))
	ctx := context.Background()
	_, err = store.db.ExecContext(ctx,
		`UPDATE nodes SET status='not-a-real-status' WHERE workflow_id='garbage-status'`)
	require.NoError(t, err)
	_, err = store.Load("garbage-status")
	require.ErrorIs(t, err, ErrCorruptData, "garbage node status → ErrCorruptData")

	// A garbage data kind → ErrCorruptData too.
	require.NoError(t, store.Save(func() *WorkflowData {
		d := NewWorkflowData("garbage-kind")
		d.Set("k", int64(1))
		return d
	}()))
	_, err = store.db.ExecContext(ctx,
		`UPDATE data_kv SET kind=99 WHERE workflow_id='garbage-kind'`)
	require.NoError(t, err)
	_, err = store.Load("garbage-kind")
	require.ErrorIs(t, err, ErrCorruptData, "unknown data kind → ErrCorruptData")
}
