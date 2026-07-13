package workflow

// M15 ph69 — INDEPENDENT adversarial suite for the delta-capture accumulator + the
// SaveDeltaCheckpoint fast path (anvil-adversarial-tester, re-run against committed HEAD
// 4eeef7d). The engineer's own tests (workflow_store_sqlite_delta_test.go) cover a single
// scripted M10/M11/M12 fixture + the AF1 Delete regression. This suite attacks the
// COMPLETENESS invariant from theory:
//
//   INVARIANT (the hard bar): for ANY sequence of forward-drive mutations, the union of
//   per-level SaveDeltaCheckpoint calls (fed the accumulator's ChangeSet) must Load
//   BYTE-IDENTICAL to a single full SaveCheckpoint of the same final WorkflowData.
//   Equivalently: every in-place map write reachable during a drive is recorded by SOME
//   mutator, so no touched key is silently omitted from the ChangeSet.
//
// Oracle = the full SaveCheckpoint (ph66/67, independently trusted). Metamorphic relation =
// delta-accumulated(d) ≡ full(d). The randomized walk is the completeness net: a missed
// mutation source shows as a divergence SOMEWHERE in the walk. The MissedSourceReddens test
// is the non-vacuity control — it injects a bypass-recordDelta write and PROVES the oracle
// detects it (else the whole net is vacuous).
//
// Runs non-race (single-writer conn). The accumulator is w.mu-guarded and exercised -race
// by the package suite.

import (
	"bytes"
	"math"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"github.com/stretchr/testify/require"
)

// assertDeltaEqualsFull loads the delta-driven store and a FRESH full SaveCheckpoint of the
// SAME final d, and asserts byte-identical snapshots. The full save from an empty store is
// the independently-trusted oracle. Returns whether they matched (so a should-fail control
// can assert the negative).
func assertDeltaEqualsFull(t *testing.T, deltaStore *SQLiteStore, d *WorkflowData) (equal bool, fastSnap, fullSnap []byte) {
	t.Helper()
	dir := t.TempDir()
	full, err := NewSQLiteStore(filepath.Join(dir, "oracle.db"))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.SaveCheckpoint(d))

	fl, err := deltaStore.Load(d.GetWorkflowID())
	require.NoError(t, err)
	ul, err := full.Load(d.GetWorkflowID())
	require.NoError(t, err)
	fastSnap, err = fl.Snapshot()
	require.NoError(t, err)
	fullSnap, err = ul.Snapshot()
	require.NoError(t, err)
	return bytes.Equal(fastSnap, fullSnap), fastSnap, fullSnap
}

func newDeltaStore(t *testing.T, name string) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
	return s
}

// applyLevel drains + SaveDeltaCheckpoints one level. Shared by the adversarial tests.
func applyLevel(t *testing.T, s *SQLiteStore, d *WorkflowData, mutate func(*WorkflowData)) {
	t.Helper()
	mutate(d)
	changed, active := d.drainDeltaCapture()
	require.True(t, active, "capture must be active during a drive")
	require.NoError(t, s.SaveDeltaCheckpoint(changed, d))
}

// -----------------------------------------------------------------------------
// (0) THE MANDATED SHOULD-FAIL — non-vacuity control (CONTEXT constraint).
// -----------------------------------------------------------------------------

// TestSQLiteDelta_Adv_MissedSourceReddens PROVES the completeness oracle has teeth. It
// simulates a hypothetical 7th mutation source that writes an in-place map WITHOUT recording
// into the accumulator (exactly the ph69-AF1 class of defect, generalized). The direct map
// write `d.data["ghost"]=...` bypasses recordDelta, so "ghost" is ABSENT from the ChangeSet
// handed to SaveDeltaCheckpoint → the fast path never persists it → the delta DB DIVERGES
// from a full SaveCheckpoint (which scans all of d and includes "ghost").
//
// This test ASSERTS the divergence. If it ever stops diverging (snapshots equal), the
// fidelity oracle used by every other test in this file is VACUOUS and must be fixed before
// any "HOLDS" verdict is trustworthy.
func TestSQLiteDelta_Adv_MissedSourceReddens(t *testing.T) {
	s := newDeltaStore(t, "missed.db")
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	// One honest level through the recording mutators.
	applyLevel(t, s, d, func(d *WorkflowData) {
		d.Set("real", int64(1))
		d.SetNodeStatus("a", Completed)
	})

	// SEED THE MISSED SOURCE: write straight to the backing map, bypassing recordDelta (as a
	// non-recording mutator would). Guarded by w.mu to mirror a real mutator's locking.
	d.mu.Lock()
	d.data[d.internKey("ghost")] = int64(999)
	d.mu.Unlock()

	// Drain (ghost NOT captured) + save. The fast path is blind to ghost.
	changed, active := d.drainDeltaCapture()
	require.True(t, active)
	require.NotContains(t, changed.DataKeys, "ghost", "the seeded bypass must be absent from the ChangeSet")
	require.NoError(t, s.SaveDeltaCheckpoint(changed, d))

	equal, fast, full := assertDeltaEqualsFull(t, s, d)
	require.False(t, equal,
		"MANDATED should-fail: a missed mutation source MUST make the fast path diverge from a full\n"+
			"SaveCheckpoint — proving the completeness oracle is non-vacuous.\nfast: %s\nfull: %s", fast, full)
}

// -----------------------------------------------------------------------------
// (1) THE METAMORPHIC RANDOM WALK — the completeness net (attack #2).
// -----------------------------------------------------------------------------

// nodeStatuses is the full 9-status alphabet a forward drive can assign.
var nodeStatuses = []NodeStatus{
	Pending, Running, Completed, Failed, Skipped, Waiting, Bypassed, Compensated, CompensationFailed,
}

// runMetamorphicWalk drives K levels of random ops over a small key universe through the 6
// recording mutators, and asserts delta ≡ full at EVERY level. A missed source or an encoding
// divergence reddens at the first level it manifests. Deterministic (fixed seed).
func runMetamorphicWalk(t *testing.T, seed int64, levels int, metricsEnabled bool) {
	t.Helper()
	s := newDeltaStore(t, "walk.db")
	var d *WorkflowData
	if metricsEnabled {
		// Exercise the metrics-ENABLED branch of every mutator (the closure path of
		// recordDelta the engineer's tests never take). Full sampling → every op records.
		cfg := metrics.NewConfig().WithEnabled(true).WithSamplingRate(1.0)
		d = NewWorkflowDataWithConfig("wf", DefaultWorkflowDataConfig().WithMetricsConfig(cfg))
		require.True(t, d.GetMetrics().IsEnabled(), "metrics must be enabled for this walk")
	} else {
		d = NewWorkflowData("wf")
	}
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test input, not security
	nodes := []string{"n0", "n1", "n2", "n3"}
	dataKeys := []string{"k0", "k1", "k2", "k3"}
	waitNodes := []string{"w0", "w1", "w2"}

	for lvl := 0; lvl < levels; lvl++ {
		ops := 1 + rng.Intn(5)
		mutate := func(d *WorkflowData) {
			for o := 0; o < ops; o++ {
				switch rng.Intn(6) {
				case 0:
					d.SetNodeStatus(nodes[rng.Intn(len(nodes))], nodeStatuses[rng.Intn(len(nodeStatuses))])
				case 1:
					d.SetOutput(nodes[rng.Intn(len(nodes))], randValue(rng))
				case 2:
					d.Set(dataKeys[rng.Intn(len(dataKeys))], randValue(rng))
				case 3:
					d.Delete(dataKeys[rng.Intn(len(dataKeys))])
				case 4:
					d.SetWait(waitNodes[rng.Intn(len(waitNodes))], rng.Int63())
				case 5:
					d.ClearWait(waitNodes[rng.Intn(len(waitNodes))])
				}
			}
		}
		applyLevel(t, s, d, mutate)

		equal, fast, full := assertDeltaEqualsFull(t, s, d)
		require.True(t, equal,
			"metamorphic walk diverged at level %d (seed %d, metrics=%v) — a mutation source is missed\n"+
				"or an encoding differs\nfast: %s\nfull: %s", lvl, seed, metricsEnabled, fast, full)
	}
}

// randValue returns a value across every encodeKV branch, including the byte-identity
// hazards (-0.0, NaN, MinInt64, empty string).
func randValue(rng *rand.Rand) interface{} {
	switch rng.Intn(8) {
	case 0:
		return int64(rng.Int63()) - (1 << 62)
	case 1:
		return math.MinInt64
	case 2:
		return math.MaxInt64
	case 3:
		return rng.NormFloat64()
	case 4:
		return math.Copysign(0, -1) // -0.0 (the ph67-F2 sign-bit hazard)
	case 5:
		return math.NaN()
	case 6:
		return rng.Intn(2) == 0 // bool
	default:
		if rng.Intn(3) == 0 {
			return "" // empty string
		}
		return string(rune('a' + rng.Intn(26)))
	}
}

func TestSQLiteDelta_Adv_MetamorphicRandomWalk(t *testing.T) {
	// Several seeds → broad coverage of interleavings, absence-deletes, and re-sets.
	for _, seed := range []int64{1, 7, 42, 1337, 2024} {
		seed := seed
		t.Run("seed", func(t *testing.T) { runMetamorphicWalk(t, seed, 120, false) })
	}
}

func TestSQLiteDelta_Adv_MetamorphicRandomWalk_MetricsEnabled(t *testing.T) {
	// The mutators' recordDelta lives in a SECOND branch under metrics — never exercised by
	// the engineer's tests. Prove completeness holds there too.
	runMetamorphicWalk(t, 99, 120, true)
}

// -----------------------------------------------------------------------------
// (2) SCALAR-ONLY LEVEL — the always-UPSERT workflow row (attack #1).
// -----------------------------------------------------------------------------

// TestSQLiteDelta_Adv_ScalarOnlyLevel — SetRollingBack/SetTriggerCause do NOT record into the
// accumulator (they are run-level scalars, not map keys). SaveDeltaCheckpoint UPSERTs the
// workflows row UNCONDITIONALLY, so the scalars must ride along even when the ChangeSet is
// entirely empty. Verify a scalar-only level (empty changed-set, capture still active) Loads
// byte-identical to a full SaveCheckpoint.
func TestSQLiteDelta_Adv_ScalarOnlyLevel(t *testing.T) {
	s := newDeltaStore(t, "scalar.db")
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	applyLevel(t, s, d, func(d *WorkflowData) {
		d.SetNodeStatus("a", Running)
		d.Set("k", int64(1))
	})

	// A level that mutates ONLY run-level scalars — the ChangeSet drains EMPTY.
	mutate := func(d *WorkflowData) {
		d.SetRollingBack(true)
		d.SetTriggerCause(TriggerCanceled)
	}
	mutate(d)
	changed, active := d.drainDeltaCapture()
	require.True(t, active)
	require.Empty(t, changed.Nodes, "scalar-only level: no node touched")
	require.Empty(t, changed.DataKeys, "scalar-only level: no data touched")
	require.Empty(t, changed.WaitKeys, "scalar-only level: no wait touched")
	require.NoError(t, s.SaveDeltaCheckpoint(changed, d))

	equal, fast, full := assertDeltaEqualsFull(t, s, d)
	require.True(t, equal,
		"a scalar-only level must persist rolling_back/trigger_cause via the always-UPSERT workflow row\nfast: %s\nfull: %s", fast, full)

	// And the scalars actually round-tripped.
	loaded, err := s.Load("wf")
	require.NoError(t, err)
	require.True(t, loaded.IsRollingBack(), "rolling_back rode the empty-changeset delta")
	require.Equal(t, TriggerCanceled, loaded.TriggerCause(), "trigger_cause rode the empty-changeset delta")
}

// -----------------------------------------------------------------------------
// (3) BOUNDARY / PARTITION TABLE (attack #3).
// -----------------------------------------------------------------------------

// TestSQLiteDelta_Adv_Boundaries drives the boundary partitions the author's single fixture
// misses, each asserting delta ≡ full. Every case runs on its own store+data.
func TestSQLiteDelta_Adv_Boundaries(t *testing.T) {
	cases := []struct {
		name  string
		steps []func(*WorkflowData)
	}{
		{
			// Domain confusion: a node and a data key share the SAME name. They land in
			// distinct tables (nodes vs data_kv) — must not collide.
			name: "node_name_equals_data_key",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) {
					d.SetNodeStatus("x", Completed)
					d.SetOutput("x", "node-out")
					d.Set("x", int64(7)) // data key "x" — different table
				},
			},
		},
		{
			// Output-only node (no status, '' sentinel ph66-F1) that LATER gains a status,
			// driven through the delta path across two levels.
			name: "output_only_then_status",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) { d.SetOutput("y", int64(1)) },      // '' sentinel row
				func(d *WorkflowData) { d.SetNodeStatus("y", Completed) }, // gains status
			},
		},
		{
			// A wait set AND cleared inside ONE level. drain dedups to one WaitKey; re-read
			// finds it absent ⇒ DELETE (0 rows, never persisted). Must equal full (no wait).
			name: "wait_set_and_cleared_same_level",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) {
					d.SetNodeStatus("w", Waiting)
					d.SetWait("w", 12345)
					d.ClearWait("w")
					d.SetNodeStatus("w", Completed)
				},
			},
		},
		{
			// Set then Delete the SAME key in ONE level → absent ⇒ DELETE. Equals full (gone).
			name: "set_then_delete_same_level",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) {
					d.Set("gone", int64(5))
					d.Set("stay", int64(9))
					d.Delete("gone")
				},
			},
		},
		{
			// Delete a NEVER-set key: records the touch (AF1), re-reads absent, DELETEs 0 rows.
			name: "delete_never_set_key",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) {
					d.Set("present", int64(1))
					d.Delete("never")
				},
			},
		},
		{
			// int64 fidelity boundaries + the '' data key + empty string value.
			name: "value_boundaries",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) {
					d.Set("min", int64(math.MinInt64))
					d.Set("max", int64(math.MaxInt64))
					d.Set("", "empty-key") // empty string KEY
					d.Set("emptyval", "")  // empty string VALUE
					d.Set("negzero", math.Copysign(0, -1))
					d.Set("nan", math.NaN())
				},
			},
		},
		{
			// A key re-set to a DIFFERENT type across levels (int64 → string → float64).
			name: "type_churn_same_key",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) { d.Set("t", int64(1)) },
				func(d *WorkflowData) { d.Set("t", "two") },
				func(d *WorkflowData) { d.Set("t", 3.5) },
			},
		},
		{
			// Delete then re-Set the same key (resurrection) across levels.
			name: "delete_then_reset",
			steps: []func(*WorkflowData){
				func(d *WorkflowData) { d.Set("r", int64(1)) },
				func(d *WorkflowData) { d.Delete("r") },
				func(d *WorkflowData) { d.Set("r", int64(2)) },
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := newDeltaStore(t, "b.db")
			d := NewWorkflowData("wf")
			d.beginDeltaCapture()
			defer d.endDeltaCapture()
			for _, step := range tc.steps {
				applyLevel(t, s, d, step)
			}
			equal, fast, full := assertDeltaEqualsFull(t, s, d)
			require.True(t, equal, "%s: delta must equal full\nfast: %s\nfull: %s", tc.name, fast, full)
		})
	}
}

// -----------------------------------------------------------------------------
// (4) CROSS-PATH SHADOW COHERENCE (attack #4).
// -----------------------------------------------------------------------------

// TestSQLiteDelta_Adv_CrossPathInterleave interleaves delta and full SaveCheckpoints on the
// SAME store, asserting delta ≡ full after every save. A shadow desync (the delta path
// advancing the shadow wrongly, or the full path diffing a stale frontier) would surface as a
// divergence — the ph67-F1 shadow-coherence invariant, now stressed across BOTH write paths.
func TestSQLiteDelta_Adv_CrossPathInterleave(t *testing.T) {
	s := newDeltaStore(t, "interleave.db")
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	// Alternate: delta, full, delta, full, ... A full save must see the frontier the deltas
	// advanced; a delta after a full must see the frontier the full advanced.
	saveFull := func() { require.NoError(t, s.SaveCheckpoint(d)) }
	saveDelta := func() {
		changed, active := d.drainDeltaCapture()
		require.True(t, active)
		require.NoError(t, s.SaveDeltaCheckpoint(changed, d))
	}

	seq := []struct {
		mutate func(*WorkflowData)
		full   bool
	}{
		{func(d *WorkflowData) { d.SetNodeStatus("a", Pending); d.Set("k", int64(1)) }, false},
		{func(d *WorkflowData) { d.SetNodeStatus("a", Completed); d.SetOutput("a", "out"); d.Set("k", int64(2)) }, true},
		{func(d *WorkflowData) { d.SetNodeStatus("b", Waiting); d.SetWait("b", 555) }, false},
		{func(d *WorkflowData) { d.ClearWait("b"); d.SetNodeStatus("b", Completed); d.Delete("k") }, true},
		{func(d *WorkflowData) { d.Set("k2", 1.5); d.SetNodeStatus("c", Bypassed) }, false},
		{func(d *WorkflowData) { d.SetRollingBack(true); d.SetTriggerCause(TriggerDeadlineExceeded) }, true},
		{func(d *WorkflowData) { d.SetNodeStatus("a", Compensated); d.Set("k2", 2.5) }, false},
	}
	for i, step := range seq {
		step.mutate(d)
		if step.full {
			// The full path ignores the accumulator; drain to keep the two in lockstep
			// (mirrors the executor's fallback: an inactive/first-warm level uses full Save).
			_, _ = d.drainDeltaCapture()
			saveFull()
		} else {
			saveDelta()
		}
		equal, fast, full := assertDeltaEqualsFull(t, s, d)
		require.True(t, equal, "cross-path interleave diverged at step %d (full=%v)\nfast: %s\nfull: %s", i, step.full, fast, full)
	}
}

// TestSQLiteDelta_Adv_ColdShadowHydrateOnDelta drives the delta path's OWN cold-shadow
// hydrate branch (SaveDeltaCheckpoint, base==nil → hydrateShadowFromDB): reopen a store that
// already holds durable rows WITHOUT calling Load first, then issue a delta. The hydrate must
// reconstruct the committed frontier so the shadow stays coherent — else a later full Save
// leaks a stale row (the ph67-AF1 hazard, on the delta path this time).
func TestSQLiteDelta_Adv_ColdShadowHydrateOnDelta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cold.db")

	// Process 1: write some durable state, close.
	s1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	d1 := NewWorkflowData("wf")
	d1.beginDeltaCapture()
	applyLevel(t, s1, d1, func(d *WorkflowData) {
		d.SetNodeStatus("a", Completed)
		d.Set("stale", int64(1))
		d.Set("keep", int64(2))
	})
	d1.endDeltaCapture()
	require.NoError(t, s1.Close())

	// Process 2: reopen (shadow COLD). Do NOT Load. Reconstruct the full logical state by
	// hand (as a resume would have loaded it) and issue a delta that MUTATES keep and DELETEs
	// stale — the delta path must hydrate the shadow from the DB to stay coherent.
	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup
	// d2 carries the full logical state (as the loaded run would): a still present, keep
	// updated, stale removed.
	d2 := NewWorkflowData("wf")
	d2.SetNodeStatus("a", Completed)
	d2.Set("keep", int64(20))
	d2.beginDeltaCapture()
	defer d2.endDeltaCapture()
	// Now the drive: delta touches keep (changed) + stale (deleted). "a" is UNTOUCHED this
	// level and must remain from the cold DB.
	applyLevel(t, s2, d2, func(d *WorkflowData) {
		d.Set("keep", int64(20))
		d.Delete("stale")
	})

	loaded, err := s2.Load("wf")
	require.NoError(t, err)
	// stale must be gone (absence⇒delete against the hydrated frontier).
	_, ok := Get(loaded, NewKey[int64]("stale"))
	require.False(t, ok, "cold-hydrate delta must DELETE the stale key")
	v, ok := Get(loaded, NewKey[int64]("keep"))
	require.True(t, ok)
	require.Equal(t, int64(20), v, "keep updated via the cold-hydrate delta")
	st, ok := loaded.GetNodeStatus("a")
	require.True(t, ok, "the untouched node from the cold DB survives")
	require.Equal(t, Completed, st)

	// A subsequent FULL save must diff against a coherent shadow (no stale-row leak).
	require.NoError(t, s2.SaveCheckpoint(d2))
	equal, fast, full := assertDeltaEqualsFull(t, s2, d2)
	require.True(t, equal, "cold-hydrate then full save must stay coherent\nfast: %s\nfull: %s", fast, full)
}

// -----------------------------------------------------------------------------
// (5) CAPTURE LIFECYCLE (attack #5).
// -----------------------------------------------------------------------------

// TestSQLiteDelta_Adv_CaptureLifecycle exercises the accumulator's lifecycle edges:
//   - endDeltaCapture then a mutator → recordDelta on a nil capture: no panic, no record;
//   - drainDeltaCapture after end → active=false (the executor's full-Save fallback signal);
//   - drain resets to a FRESH capture: a key touched at level N is NOT re-emitted at N+1.
func TestSQLiteDelta_Adv_CaptureLifecycle(t *testing.T) {
	d := NewWorkflowData("wf")

	// Capture OFF: every mutator must be a zero-record no-panic (the hot path).
	require.NotPanics(t, func() {
		d.Set("a", int64(1))
		d.SetNodeStatus("n", Completed)
		d.SetOutput("n", "o")
		d.SetWait("w", 1)
		d.ClearWait("w")
		d.Delete("a")
	}, "mutators must not panic when capture is inactive (nil accumulator)")
	_, active := d.drainDeltaCapture()
	require.False(t, active, "drain must report inactive when capture is off")

	// Arm, touch "a" at level N, drain.
	d.beginDeltaCapture()
	d.Set("a", int64(1))
	cs, active := d.drainDeltaCapture()
	require.True(t, active)
	require.ElementsMatch(t, []string{"a"}, cs.DataKeys, "level N captured a")

	// Level N+1 touches only "b": the drain must NOT re-emit "a" (fresh accumulator).
	d.Set("b", int64(2))
	cs2, active := d.drainDeltaCapture()
	require.True(t, active)
	require.ElementsMatch(t, []string{"b"}, cs2.DataKeys, "level N+1 must not re-emit a key touched only at N")

	// endDeltaCapture then a mutator → nil-capture path again, still no panic, still inactive.
	d.endDeltaCapture()
	require.NotPanics(t, func() { d.Set("c", int64(3)) }, "mutator after endDeltaCapture must not panic")
	_, active = d.drainDeltaCapture()
	require.False(t, active, "drain inactive after endDeltaCapture")
}

// TestSQLiteDelta_Adv_AF1Holds re-confirms the ph69-AF1 fix (Delete records its touch) at
// RUNTIME with a stronger multi-key scenario than the engineer's single-key regression: some
// keys deleted, some survive, some deleted-then-resurrected — all through the fast path,
// byte-identical to full.
func TestSQLiteDelta_Adv_AF1Holds(t *testing.T) {
	s := newDeltaStore(t, "af1.db")
	d := NewWorkflowData("wf")
	d.beginDeltaCapture()
	defer d.endDeltaCapture()

	applyLevel(t, s, d, func(d *WorkflowData) {
		d.Set("del1", int64(1))
		d.Set("del2", int64(2))
		d.Set("keep1", int64(3))
		d.Set("resurrect", int64(4))
		d.SetNodeStatus("a", Completed)
	})
	applyLevel(t, s, d, func(d *WorkflowData) {
		d.Delete("del1")
		d.Delete("del2")
		d.Delete("resurrect")
	})
	applyLevel(t, s, d, func(d *WorkflowData) {
		d.Set("resurrect", int64(40)) // resurrect after delete
	})

	equal, fast, full := assertDeltaEqualsFull(t, s, d)
	require.True(t, equal, "AF1 fix must hold: Delete captured across a multi-key scenario\nfast: %s\nfull: %s", fast, full)

	loaded, err := s.Load("wf")
	require.NoError(t, err)
	for _, gone := range []string{"del1", "del2"} {
		_, ok := Get(loaded, NewKey[int64](gone))
		require.False(t, ok, "%s must be DELETEd via the fast path", gone)
	}
	v, ok := Get(loaded, NewKey[int64]("resurrect"))
	require.True(t, ok)
	require.Equal(t, int64(40), v, "resurrected key holds its new value")
}
