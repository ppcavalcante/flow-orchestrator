package workflow

// M15 ph67 — INDEPENDENT ADVERSARIAL crash/seam/resume-consistency suite for the
// incremental SQLite checkpoint (the NON-NEGOTIABLE independent pass; M10 AF1
// discipline). The engineer's own tests (workflow_store_sqlite_incremental_test.go)
// walk ONE monotonically-growing fixture on ONE well-behaved store instance and use a
// CLEAN Close() as the "crash." This suite attacks the seams that fixture cannot reach:
//
//   * BITE 1 — incremental ≡ full Save under the ADVERSARIAL mutation classes the
//     grow-only fixture never exercises: a value that changes then changes BACK; a data
//     key added→Deleted→re-added; a wait consumed→re-set; rolling_back toggled back; an
//     output-only node that GAINS a status; the node set SHRINKING; two workflow ids
//     interleaved on one store; Save-then-incremental; Delete-then-recheckpoint; and a
//     randomized metamorphic property over all of the above.
//   * BITE 2 — crash-atomicity clean level boundary: an INDEPENDENT durable reader sees
//     every committed level whole (never torn); a crash-before-commit (BeginTx→partial→
//     Rollback, true process-death semantics) leaves exactly the prior committed level,
//     zero torn, shadow un-advanced; the SEEDED SHOULD-FAIL control — a NON-transactional
//     multi-row write DOES leave a torn level — proving the single txn.Commit() is what
//     buys atomicity (the assertion is not vacuous). Includes the Waiting/rolling_back
//     seam and interrupted-reopen.
//   * BITE 3 — the '' output-only sentinel + Waiting/Bypassed/Compensated survive repeated
//     UPSERT (never downgraded to '' nor a phantom Pending).
//   * SHADOW-INV — the novel attack surface: shadow == durable committed frontier. The
//     reproduced DEFECTS below (marked _DEFECT_) show where it does NOT.
//
// ORACLES: (1) byte-identity — a fresh full Save of the same final state is the reference
// (the ph66 hold); (2) totality/atomicity minimum bar — a reopen after any crash yields a
// clean level, never a torn/partial one, never a panic. Runs non-race (single-writer
// conn); the mu-guarded shadow is exercised under -race by the broader suite.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// snapOf returns the canonical Snapshot bytes of a WorkflowData Loaded from store.
func snapOf(t *testing.T, s *SQLiteStore, id string) []byte {
	t.Helper()
	wd, err := s.Load(id)
	require.NoError(t, err)
	b, err := wd.Snapshot()
	require.NoError(t, err)
	return b
}

// fullSaveSnap writes data via a fresh full Save (the clean-overwrite oracle) and
// returns the reloaded Snapshot bytes.
func fullSaveSnap(t *testing.T, dir, name string, data *WorkflowData) []byte {
	t.Helper()
	full, err := NewSQLiteStore(filepath.Join(dir, name))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.Save(data))
	wd, err := full.Load(data.GetWorkflowID())
	require.NoError(t, err)
	b, err := wd.Snapshot()
	require.NoError(t, err)
	return b
}

// requireIncEqFull asserts the store's durable state for id Loads byte-identical to a
// single full Save of data — the BITE 1 oracle.
func requireIncEqFull(t *testing.T, dir string, s *SQLiteStore, data *WorkflowData) {
	t.Helper()
	got := snapOf(t, s, data.GetWorkflowID())
	want := fullSaveSnap(t, dir, "oracle-"+data.GetWorkflowID()+".db", data)
	require.True(t, bytes.Equal(got, want),
		"incremental must Load byte-identical to a full Save\ninc:  %s\nfull: %s", got, want)
}

// ===========================================================================
// BITE 1 — adversarial mutation classes the grow-only fixture never reaches.
// All PASS on correct code (base is properly maintained on one instance).
// ===========================================================================

func TestPh67Adv_ValueChangesThenChangesBack(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Running)
	d.Set("counter", int64(1))
	require.NoError(t, s.SaveCheckpoint(d))
	d.Set("counter", int64(2))
	d.SetNodeStatus("a", Failed)
	require.NoError(t, s.SaveCheckpoint(d))
	// change BACK to the original values — the diff must re-UPSERT, not skip.
	d.Set("counter", int64(1))
	d.SetNodeStatus("a", Running)
	require.NoError(t, s.SaveCheckpoint(d))

	requireIncEqFull(t, dir, s, d)
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	v, _ := got.GetInt64("counter")
	require.Equal(t, int64(1), v)
	st, _ := got.GetNodeStatus("a")
	require.Equal(t, Running, st)
}

func TestPh67Adv_DataKeyAddedDeletedReadded(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.Set("k", "v1")
	require.NoError(t, s.SaveCheckpoint(d))
	// delete the key — the diff must DELETE the vanished data_kv row.
	require.True(t, d.Delete("k"))
	require.NoError(t, s.SaveCheckpoint(d))
	mid, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	_, has := mid.Get("k")
	require.False(t, has, "deleted key must not linger after an incremental checkpoint")
	requireIncEqFull(t, dir, s, d)
	// re-add with a NEW value.
	d.Set("k", "v2")
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
}

func TestPh67Adv_WaitConsumedThenReset(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("b", Waiting)
	d.SetWait("b", 1000)
	require.NoError(t, s.SaveCheckpoint(d))
	// consume the wait (ClearWait) — DELETE branch of diffWaits.
	d.ClearWait("b")
	d.SetNodeStatus("b", Completed)
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
	// re-park the SAME node on a new wait — UPSERT after a prior DELETE.
	d.SetNodeStatus("b", Waiting)
	d.SetWait("b", 2000)
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	fa, ok := got.GetWait("b")
	require.True(t, ok)
	require.Equal(t, int64(2000), fa)
}

func TestPh67Adv_RollingBackToggledBack(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	require.NoError(t, s.SaveCheckpoint(d))
	d.SetRollingBack(true)
	d.SetTriggerCause(TriggerCanceled)
	require.NoError(t, s.SaveCheckpoint(d))
	// toggle BACK — the workflows-row UPDATE must fire on the reverse transition.
	d.SetRollingBack(false)
	d.SetTriggerCause(TriggerNone)
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	require.False(t, got.IsRollingBack(), "rolling_back must return to false, not stick")
}

func TestPh67Adv_OutputOnlyNodeGainsStatus(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetOutput("orphan", "payload") // output-only: status '' sentinel
	require.NoError(t, s.SaveCheckpoint(d))
	got1, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	_, hasSt := got1.GetNodeStatus("orphan")
	require.False(t, hasSt, "output-only node starts with NO status")
	// the node now GAINS a status (still keeps its output).
	d.SetNodeStatus("orphan", Completed)
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
	got2, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	st, ok := got2.GetNodeStatus("orphan")
	require.True(t, ok)
	require.Equal(t, Completed, st, "'' → completed transition must persist")
	out, ok := got2.GetOutput("orphan")
	require.True(t, ok)
	require.Equal(t, "payload", out)
}

// The node set SHRINKS on the SAME store instance (shadow maintained → diffNodes DELETE
// fires). This is the SAFE counterpart of the empty-shadow DEFECT below: with a live
// shadow the shrink is handled correctly.
func TestPh67Adv_NodeSetShrinks_SameInstance(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.SetNodeStatus("b", Completed)
	d.SetNodeStatus("c", Completed)
	require.NoError(t, s.SaveCheckpoint(d))

	// A DIFFERENT, smaller data for the same id on the SAME instance.
	d2 := NewWorkflowData("wf")
	d2.SetNodeStatus("a", Failed)
	require.NoError(t, s.SaveCheckpoint(d2))
	requireIncEqFull(t, dir, s, d2)
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	_, hasB := got.GetNodeStatus("b")
	_, hasC := got.GetNodeStatus("c")
	require.False(t, hasB, "vanished node b must be DELETEd when the shadow is live")
	require.False(t, hasC, "vanished node c must be DELETEd when the shadow is live")
}

func TestPh67Adv_TwoWorkflowsInterleaved_NoShadowCrossContamination(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	a := NewWorkflowData("A")
	b := NewWorkflowData("B")
	a.SetNodeStatus("x", Pending)
	b.SetNodeStatus("y", Running)
	require.NoError(t, s.SaveCheckpoint(a))
	require.NoError(t, s.SaveCheckpoint(b))
	a.SetNodeStatus("x", Completed)
	a.Set("ka", int64(1))
	require.NoError(t, s.SaveCheckpoint(a))
	b.SetNodeStatus("y", Failed)
	b.Set("kb", "s")
	require.NoError(t, s.SaveCheckpoint(b))
	a.SetNodeStatus("z", Bypassed)
	require.NoError(t, s.SaveCheckpoint(a))

	requireIncEqFull(t, dir, s, a)
	requireIncEqFull(t, dir, s, b)
	// B must not have leaked A's keys/nodes and vice-versa.
	gotB, errLoad := s.Load("B")
	require.NoError(t, errLoad)
	_, hasKa := gotB.Get("ka")
	require.False(t, hasKa, "shadow[A] must not contaminate B")
	_, hasZ := gotB.GetNodeStatus("z")
	require.False(t, hasZ)
}

func TestPh67Adv_FullSaveThenIncremental_SameInstance(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.SetNodeStatus("b", Completed)
	d.Set("k", int64(1))
	require.NoError(t, s.Save(d)) // FULL save seeds the shadow
	// now an incremental delta must diff correctly against the Save-set shadow.
	d.SetNodeStatus("b", Failed)
	require.True(t, d.Delete("k"))
	require.NoError(t, s.SaveCheckpoint(d))
	requireIncEqFull(t, dir, s, d)
}

func TestPh67Adv_DeleteThenRecheckpoint_NoStaleShadow(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.SetNodeStatus("b", Completed)
	d.Set("k", int64(7))
	require.NoError(t, s.SaveCheckpoint(d))
	require.NoError(t, s.Delete("wf")) // drops rows AND shadow

	// Re-checkpoint a fresh, different state under the same id.
	d2 := NewWorkflowData("wf")
	d2.SetNodeStatus("a", Running)
	require.NoError(t, s.SaveCheckpoint(d2))
	requireIncEqFull(t, dir, s, d2)
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	_, hasB := got.GetNodeStatus("b")
	require.False(t, hasB, "Delete must clear the shadow so no stale node b survives")
	_, hasK := got.Get("k")
	require.False(t, hasK)
}

// Metamorphic/property: a random walk of mutation classes on ONE maintained store
// instance must keep incremental Load byte-identical to a full Save at EVERY step.
// Deterministic seed → reproducible; shrinking is by the logged step index.
func TestPh67Adv_MetamorphicRandomWalk(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	rng := rand.New(rand.NewSource(0xF107C1)) //nolint:gosec // deterministic test input
	d := NewWorkflowData("wf")
	statuses := []NodeStatus{Pending, Running, Completed, Failed, Skipped, Waiting, Bypassed, Compensated, CompensationFailed}
	nodes := []string{"n0", "n1", "n2", "n3"}
	keys := []string{"k0", "k1", "k2"}

	for step := 0; step < 60; step++ {
		switch rng.Intn(7) {
		case 0:
			d.SetNodeStatus(nodes[rng.Intn(len(nodes))], statuses[rng.Intn(len(statuses))])
		case 1:
			d.SetOutput(nodes[rng.Intn(len(nodes))], fmt.Sprintf("out-%d", rng.Intn(5)))
		case 2:
			switch rng.Intn(3) {
			case 0:
				d.Set(keys[rng.Intn(len(keys))], int64(rng.Intn(1000)))
			case 1:
				d.Set(keys[rng.Intn(len(keys))], rng.Intn(2) == 0)
			case 2:
				d.Set(keys[rng.Intn(len(keys))], fmt.Sprintf("s%d", rng.Intn(9)))
			}
		case 3:
			d.Delete(keys[rng.Intn(len(keys))])
		case 4:
			n := nodes[rng.Intn(len(nodes))]
			d.SetNodeStatus(n, Waiting)
			d.SetWait(n, int64(rng.Intn(1_000_000)))
		case 5:
			d.ClearWait(nodes[rng.Intn(len(nodes))])
		case 6:
			rb := rng.Intn(2) == 0
			d.SetRollingBack(rb)
			if rb {
				d.SetTriggerCause(TriggerCanceled)
			} else {
				d.SetTriggerCause(TriggerNone)
			}
		}
		require.NoError(t, s.SaveCheckpoint(d), "step %d", step)
		got := snapOf(t, s, "wf")
		want := fullSaveSnap(t, dir, fmt.Sprintf("oracle-step-%d.db", step), d)
		require.True(t, bytes.Equal(got, want),
			"metamorphic divergence at step %d\ninc:  %s\nfull: %s", step, got, want)
	}
}

// ===========================================================================
// BITE 3 — sentinel + status survive REPEATED UPSERT.
// ===========================================================================

func TestPh67Adv_SentinelAndStatusSurviveManyUpserts(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	d.SetOutput("orphan", "v")    // '' sentinel
	d.SetNodeStatus("w", Waiting) // Waiting must survive
	d.SetNodeStatus("cx", Compensated)
	require.NoError(t, s.SaveCheckpoint(d))

	// touch UNRELATED nodes 10 times; orphan/w/cx are never re-set → must be untouched.
	for i := 0; i < 10; i++ {
		d.SetNodeStatus(fmt.Sprintf("t%d", i), Completed)
		require.NoError(t, s.SaveCheckpoint(d))
	}
	got, errLoad := s.Load("wf")
	require.NoError(t, errLoad)
	_, hasSt := got.GetNodeStatus("orphan")
	require.False(t, hasSt, "output-only sentinel must NOT gain a phantom Pending across UPSERTs")
	st, _ := got.GetNodeStatus("w")
	require.Equal(t, Waiting, st)
	st2, _ := got.GetNodeStatus("cx")
	require.Equal(t, Compensated, st2)
	requireIncEqFull(t, dir, s, d)
}

// ===========================================================================
// BITE 2 — crash-atomicity: clean level boundary + the SEEDED should-fail control.
// ===========================================================================

// advLevels returns a sequence of evolving states with a Waiting+rolling_back level,
// so the durable-reader sweep and the crash seams cover those markers.
func advLevels() []func(*WorkflowData) {
	return []func(*WorkflowData){
		func(d *WorkflowData) { d.SetNodeStatus("a", Pending); d.SetNodeStatus("b", Pending) },
		func(d *WorkflowData) {
			d.SetNodeStatus("a", Completed)
			d.SetOutput("a", int64(math.MaxInt64))
			d.Set("c", int64(1))
		},
		func(d *WorkflowData) {
			d.SetNodeStatus("b", Waiting)
			d.SetWait("b", 1893456000000000000)
			d.SetRollingBack(true)
			d.SetTriggerCause(TriggerCanceled)
		},
		func(d *WorkflowData) {
			d.SetNodeStatus("b", Completed)
			d.ClearWait("b")
			d.SetRollingBack(false)
			d.SetTriggerCause(TriggerNone)
		},
	}
}

// BITE 2a — an INDEPENDENT durable reader (a separate store over the same file) sees
// every committed level WHOLE. Models "crash between levels": reopening at any boundary
// yields exactly K clean levels, zero torn/partial.
func TestPh67Adv_EveryCommittedLevelIsCleanToAnIndependentReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	d := NewWorkflowData("wf")
	for lvl, step := range advLevels() {
		step(d)
		require.NoError(t, s.SaveCheckpoint(d))
		// Independent reader = a SECOND store opened over the same file (the post-crash
		// view: only durably-committed rows are visible).
		reader, err := NewSQLiteStore(path)
		require.NoError(t, err)
		got := snapOf(t, reader, "wf")
		require.NoError(t, reader.Close())
		want := fullSaveSnap(t, dir, fmt.Sprintf("clean-%d.db", lvl), d)
		require.True(t, bytes.Equal(got, want),
			"level %d must be durable & WHOLE to an independent reader (zero torn)\ngot:  %s\nwant: %s", lvl, got, want)
	}
}

// BITE 2b — a crash BEFORE commit (BeginTx → partial writes → Rollback: true
// process-death semantics for a single-txn checkpoint) leaves EXACTLY the prior committed
// level and does NOT advance the shadow. Includes the Waiting/rolling_back seam.
func TestPh67Adv_CrashBeforeCommit_ZeroTorn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)

	d := NewWorkflowData("wf")
	steps := advLevels()
	// Commit levels 0..2 for real (level 2 is the Waiting + rolling_back frontier).
	for _, step := range steps[:3] {
		step(d)
		require.NoError(t, s.SaveCheckpoint(d))
	}
	committed := snapOf(t, s, "wf")
	shadowBefore := shadowFromData(d) // == the durable frontier after level 2

	// Now simulate a CRASH during level 3: build the target, open a txn, write SOME of
	// the delta, then die before commit (Rollback). This is exactly what the real
	// single-txn saveIncremental does up to — but never reaching — tx.Commit().
	step := steps[3]
	step(d)
	target := shadowFromData(d)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	// partial: write only the changed node rows, then crash (no data/wait/workflow update).
	for node, row := range target.nodes {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET status=excluded.status, output=excluded.output, has_output=excluded.has_output`,
			"wf", node, row.status, row.output, boolToInt(row.hasOutput))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Rollback()) // <-- crash before commit

	// Reopen (fresh instance = post-crash view) and Load: must equal the level-2 frontier.
	require.NoError(t, s.Close())
	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck
	after := snapOf(t, s2, "wf")
	require.True(t, bytes.Equal(after, committed),
		"a crash before commit must leave EXACTLY the prior committed level (zero torn)\nafter: %s\nlvl2:  %s", after, committed)
	// and the rebuilt shadow equals that same durable frontier.
	s2.mu.Lock()
	rebuilt := s2.shadow["wf"]
	s2.mu.Unlock()
	require.NotNil(t, rebuilt)
	require.Equal(t, shadowBefore.rollingBack, rebuilt.rollingBack)
	require.Len(t, rebuilt.nodes, len(shadowBefore.nodes))
}

// BITE 2 SEEDED SHOULD-FAIL CONTROL — proves the atomicity assertion is NOT vacuous.
// A NON-transactional multi-row level write (autocommit per statement) that crashes
// partway MUST be able to leave a TORN level: some rows advanced, others stale — a state
// that is neither the prior clean level nor the next one. If this control ever STOPPED
// reddening, BITE 2a/2b would be meaningless. (It asserts tornness IS reachable without a
// wrapping txn — the single txn.Commit() in the real path is precisely what prevents it.)
func TestPh67Adv_SeededShouldFail_NonTxnLeavesTornLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)

	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.Set("k", int64(1))
	require.NoError(t, s.SaveCheckpoint(d)) // clean level 0
	clean0 := snapOf(t, s, "wf")

	// Level 1 changes BOTH the node and the data key. Write them NON-transactionally
	// (autocommit) and crash AFTER the node but BEFORE the data.
	d.SetNodeStatus("a", Failed)
	d.Set("k", int64(2))
	target := shadowFromData(d)
	clean1 := fullSaveSnap(t, dir, "clean1.db", d)
	ctx := context.Background()
	// node write (autocommit — durable immediately, no enclosing txn).
	row := target.nodes["a"]
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)
		 ON CONFLICT(workflow_id, node_name) DO UPDATE SET status=excluded.status, output=excluded.output, has_output=excluded.has_output`,
		"wf", "a", row.status, row.output, boolToInt(row.hasOutput))
	require.NoError(t, err)
	// <-- crash here: the data_kv update for "k" never happens.
	require.NoError(t, s.Close())

	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck
	torn := snapOf(t, s2, "wf")

	// The should-fail REDDENS: the reopened state is TORN — equal to NEITHER clean level.
	require.False(t, bytes.Equal(torn, clean0),
		"non-txn write should have advanced the node past clean level 0")
	require.False(t, bytes.Equal(torn, clean1),
		"non-txn write is TORN — it must NOT equal the clean next level (that is the whole point of the control)")
	t.Logf("CONTROL confirmed torn (node advanced, data stale): %s", torn)
}

// Interrupted reopen: opening the file repeatedly (a crash during reopen is just an
// interrupted NewSQLiteStore — schema is CREATE IF NOT EXISTS, idempotent) never corrupts
// or loses the committed frontier.
func TestPh67Adv_InterruptedReopen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	s0, err := NewSQLiteStore(path)
	require.NoError(t, err)
	d := NewWorkflowData("wf")
	d.SetNodeStatus("a", Completed)
	d.Set("k", int64(5))
	require.NoError(t, s0.SaveCheckpoint(d))
	want := snapOf(t, s0, "wf")
	require.NoError(t, s0.Close())

	for i := 0; i < 4; i++ {
		si, err := NewSQLiteStore(path) // reopen (idempotent schema) — no Load/checkpoint
		require.NoError(t, err)
		got := snapOf(t, si, "wf")
		require.True(t, bytes.Equal(got, want), "reopen %d must preserve the committed frontier", i)
		require.NoError(t, si.Close())
	}
}

// ===========================================================================
// REPRODUCED DEFECTS (SHADOW-INV violated). These FAIL on current ph67 code — they are
// the evidence. After the fix (make the empty-baseline path a clean overwrite, matching
// full Save), they become durable regression coverage.
// ===========================================================================

// DEFECT 1 — empty shadow over a NON-EMPTY DB (reopen WITHOUT Load) leaks stale rows.
// The code comment claims this "degenerates to a correct full write"; it does not — with
// base==nil the diff's DELETE-vanished loops iterate an empty map and delete nothing,
// while a real full Save clean-overwrites. Any durable row absent from the new state
// leaks → Load diverges from a full Save (BITE 1 broken across a reopen).
func TestPh67Adv_DEFECT_EmptyShadowOverNonEmptyDB_LeaksStaleRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")

	s1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	d1 := NewWorkflowData("wf")
	d1.SetNodeStatus("a", Completed)
	d1.SetNodeStatus("b", Completed)
	d1.SetNodeStatus("c", Completed)
	d1.Set("k1", int64(1))
	d1.Set("k2", int64(2))
	require.NoError(t, s1.SaveCheckpoint(d1))
	require.NoError(t, s1.Close())

	// Reopen (empty shadow). Continue with a SMALLER state WITHOUT Load.
	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck
	d2 := NewWorkflowData("wf")
	d2.SetNodeStatus("a", Failed)
	d2.Set("k1", int64(9))
	require.NoError(t, s2.SaveCheckpoint(d2))

	requireIncEqFull(t, dir, s2, d2) // FAILS: b, c, k2 leak from the prior process.
}

// DEFECT 2 — same root cause, reached via the INTENDED resume path when a store hosts
// MORE THAN ONE workflow: after a reopen you Load workflow A (rebuilding shadow[A]) and
// then checkpoint a shrunken workflow B you did NOT Load — shadow[B] is nil, so B's
// vanished durable rows leak. Shows the defect is not limited to "never Load anything."
func TestPh67Adv_DEFECT_ReopenLoadA_CheckpointB_LeaksBsStaleRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")

	s1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	a := NewWorkflowData("A")
	a.SetNodeStatus("ax", Completed)
	b := NewWorkflowData("B")
	b.SetNodeStatus("bx", Completed)
	b.SetNodeStatus("by", Completed)
	b.Set("bk", int64(1))
	require.NoError(t, s1.SaveCheckpoint(a))
	require.NoError(t, s1.SaveCheckpoint(b))
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close()      //nolint:errcheck
	_, err = s2.Load("A") // resume A → shadow[A] rebuilt, shadow[B] still nil
	require.NoError(t, err)

	// Continue B with a shrunken state (only bx) WITHOUT having Loaded B.
	b2 := NewWorkflowData("B")
	b2.SetNodeStatus("bx", Failed)
	require.NoError(t, s2.SaveCheckpoint(b2))

	requireIncEqFull(t, dir, s2, b2) // FAILS: by and bk leak.
}
