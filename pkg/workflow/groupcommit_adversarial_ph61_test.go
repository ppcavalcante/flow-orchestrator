package workflow

// M14 ph61 — INDEPENDENT ADVERSARIAL crash-testing of the group-commit
// (Batched(K)) durability core. The author's own bites (groupcommit_ph61_test.go)
// check the happy geometry at ONE offset; this suite attacks the batched crash
// SEMANTICS at every boundary the author could not think to enumerate:
//
//   * the ≤K crash-loss bound at EVERY offset in a K-window (never > K, never
//     off-by-one at count%k==0) + the first-window "no file at all" edge,
//   * torn-read / .tmp-leftover freedom mid-window (strategy (d): no partial file),
//   * pending-clone isolation (a caller mutating after a deferred checkpoint cannot
//     corrupt the forced Sync),
//   * stale-pending: Save clears pending so a later Sync cannot re-flush a stale
//     snapshot; cross-workflow pending isolation on one store,
//   * the park floor under Batched + the ADVERSARIAL park-then-more-checkpoints case,
//   * the completion floor + saga-rollback floor (always-durable Save) under Batched,
//   * resume re-runs the lost ≤K levels idempotently,
//   * K edge values: K=1 (≡Strict), K=2, K>Levels (nothing written until completion),
//   * a universally-quantified property (frontier == floor(n/k)*k) + a fuzz target.
//
// Oracle for each: the design's stated contract (loss ≤ K, torn-safe, floor always
// durable) — the minimum bar is "a crash never loses more than K levels and never
// leaves a torn/partial file, and a park/completion/rollback is always durable."

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- helpers -------------------------------------------------------------------

// advSnapshot builds a WorkflowData whose completed frontier is exactly `frontier`
// (nodes n1..n_frontier Completed). Mimics one level-barrier checkpoint's state.
func advSnapshot(id string, frontier int) *WorkflowData {
	d := NewWorkflowData(id)
	for i := 1; i <= frontier; i++ {
		d.SetNodeStatus(fmt.Sprintf("n%d", i), Completed)
	}
	return d
}

// advDiskFrontier reads the ON-DISK (durable) frontier for id via store.Load, which
// reads the .fb file — so it reflects ONLY what has been fsync-written, never the
// in-memory pending state. That is precisely the post-crash view (a power loss drops
// everything not on disk). Returns exists=false when no durable file is present.
func advDiskFrontier(t *testing.T, store *FlatBuffersStore, id string) (frontier int, exists bool) {
	t.Helper()
	loaded, err := store.Load(id)
	if errors.Is(err, ErrNotFound) {
		return 0, false
	}
	require.NoError(t, err, "on-disk snapshot must be a COMPLETE parseable file, never torn")
	loaded.ForEachNodeStatus(func(n string, s NodeStatus) {
		if s == Completed {
			var idx int
			if _, e := fmt.Sscanf(n, "n%d", &idx); e == nil && idx > frontier {
				frontier = idx
			}
		}
	})
	return frontier, true
}

// --- 1. CRASH-LOSS BOUND at EVERY offset (the core safety property) -------------

// TestAdv_ph61_CrashLossBound_EveryOffset — drive checkpoints 1..2K under Batched(K)
// and, after EACH one, read the on-disk frontier as if the process died right there.
// The invariant the whole feature rests on: loss = j - durableFrontier is ALWAYS in
// [0, K) — NEVER == K, NEVER > K, and NEVER a torn/empty file. Also pins the exact
// durable frontier to floor(j/K)*K (catches any off-by-one at the count%k==0 edge).
func TestAdv_ph61_CrashLossBound_EveryOffset(t *testing.T) {
	for _, k := range []int{1, 2, 3, 8, 16} {
		k := k
		t.Run(fmt.Sprintf("K=%d", k), func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(uint(k))))
			require.NoError(t, err)
			const id = "bound"

			for j := 1; j <= 2*k+1; j++ {
				require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))

				expected := (j / k) * k // last K-boundary at or below j
				frontier, exists := advDiskFrontier(t, store, id)

				if expected == 0 {
					// First window before any boundary: strategy (d) writes NOTHING,
					// so there is NO durable file. Loss is the whole j levels — but
					// j < K here, so loss < K, still within the bound (resume re-runs
					// from scratch, idempotently).
					require.False(t, exists, "K=%d j=%d: no boundary yet → no durable file", k, j)
					require.Less(t, j, k, "no-file case only occurs strictly inside the first window")
					continue
				}

				require.True(t, exists, "K=%d j=%d: a boundary has passed → a durable file MUST exist", k, j)
				require.Equal(t, expected, frontier,
					"K=%d j=%d: durable frontier must be exactly the last K-boundary floor(j/K)*K", k, j)

				loss := j - frontier
				require.GreaterOrEqual(t, loss, 0, "K=%d j=%d: durable frontier can never exceed j", k, j)
				require.Less(t, loss, k,
					"CRASH-LOSS BOUND: loss (%d) must be strictly < K (%d) at j=%d — never ≥K", loss, k, j)
			}
		})
	}
}

// --- 2. TORN-READ / .tmp leftover freedom mid-window ---------------------------

// TestAdv_ph61_NoTornFileOrTmpMidWindow — strategy (d) claims no file is written at
// all between boundaries, so there can be NO partial/torn file and NO leftover .tmp.
// Glob the store dir at every point in the window and assert: only complete .fb
// boundary snapshots ever exist; never a *.tmp-*.
func TestAdv_ph61_NoTornFileOrTmpMidWindow(t *testing.T) {
	dir := t.TempDir()
	const k = 8
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(k)))
	require.NoError(t, err)
	const id = "torn"

	assertNoTmp := func(j int) {
		entries, err := filepath.Glob(filepath.Join(dir, "*"))
		require.NoError(t, err)
		for _, e := range entries {
			base := filepath.Base(e)
			require.NotContains(t, base, ".tmp-",
				"j=%d: a leftover atomic-write temp file must NEVER exist (torn-read window)", j)
		}
	}

	// First window (j=1..7): NO file at all — deferred, live-in-memory only.
	for j := 1; j <= k-1; j++ {
		require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
		assertNoTmp(j)
		fbs, gerr := filepath.Glob(filepath.Join(dir, "*.fb"))
		require.NoError(t, gerr)
		require.Empty(t, fbs, "j=%d: nothing is written before the first boundary (strategy d)", j)
	}
	// Boundary j=8: exactly one complete .fb, no tmp.
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, k)))
	assertNoTmp(k)
	fbs, gerr := filepath.Glob(filepath.Join(dir, "*.fb"))
	require.NoError(t, gerr)
	require.Len(t, fbs, 1, "exactly one canonical snapshot after the boundary")
	// Second window (j=9..15): still exactly the j=8 snapshot, still no tmp.
	for j := k + 1; j <= 2*k-1; j++ {
		require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
		assertNoTmp(j)
		f, exists := advDiskFrontier(t, store, id)
		require.True(t, exists)
		require.Equal(t, k, f, "j=%d: durable frontier stays at the last boundary (8), no torn mid-write", j)
	}
}

// --- 3. PENDING-CLONE ISOLATION -------------------------------------------------

// TestAdv_ph61_PendingCloneIsolation — a deferred checkpoint retains a CLONE of the
// state. A caller that keeps mutating the SAME *WorkflowData after the deferred
// checkpoint must NOT corrupt the snapshot a later forced Sync flushes. Attack: defer
// at frontier 3, then advance the same object to frontier 6, then Sync — disk must be
// the frontier-3 snapshot the checkpoint captured, not the mutated 6.
func TestAdv_ph61_PendingCloneIsolation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)
	const id = "clone"

	d := advSnapshot(id, 3)
	require.NoError(t, store.SaveCheckpoint(d)) // deferred → pending = Clone(frontier 3)

	// Mutate the SAME object AFTER the deferred checkpoint (as the live executor would).
	d.SetNodeStatus("n4", Completed)
	d.SetNodeStatus("n5", Completed)
	d.SetNodeStatus("n6", Completed)

	require.NoError(t, store.Sync(id)) // forces the pending CLONE, not the mutated d
	frontier, exists := advDiskFrontier(t, store, id)
	require.True(t, exists)
	require.Equal(t, 3, frontier,
		"forced Sync must flush the CLONE captured at deferral (frontier 3), immune to later mutation")
}

// --- 4. STALE-PENDING + CROSS-WORKFLOW ISOLATION --------------------------------

// TestAdv_ph61_SaveClearsPending_NoStaleReflush — Save (final/rollback/external) is
// always fsync-durable AND supersedes any deferred checkpoint. A subsequent Sync must
// NOT resurrect the now-stale pending snapshot. Attack: defer frontier 3 → Save
// frontier 7 → Sync → disk must be 7, never a re-flushed stale 3.
func TestAdv_ph61_SaveClearsPending_NoStaleReflush(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)
	const id = "stale"

	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 3))) // deferred → pending = 3
	require.NoError(t, store.Save(advSnapshot(id, 7)))           // durable 7, clears pending
	require.NoError(t, store.Sync(id))                           // MUST be a no-op, not a re-flush of 3

	frontier, exists := advDiskFrontier(t, store, id)
	require.True(t, exists)
	require.Equal(t, 7, frontier, "Sync after Save must not re-flush the stale pending (3) — disk stays 7")
}

// TestAdv_ph61_CrossWorkflowPendingIsolation — two workflowIDs share one store with
// interleaved batched checkpoints. Sync(A) must flush ONLY A's pending; B's deferred
// state must remain un-flushed (its own Sync flushes it). No pending cross-leak.
func TestAdv_ph61_CrossWorkflowPendingIsolation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)

	// Interleave deferred checkpoints for A and B (all inside the K=64 window).
	require.NoError(t, store.SaveCheckpoint(advSnapshot("A", 1)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot("B", 1)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot("A", 2)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot("B", 2)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot("A", 3)))

	// Sync ONLY A.
	require.NoError(t, store.Sync("A"))

	fa, aExists := advDiskFrontier(t, store, "A")
	require.True(t, aExists, "A's forced Sync makes A durable")
	require.Equal(t, 3, fa, "A flushes its own latest pending (3)")

	_, bExists := advDiskFrontier(t, store, "B")
	require.False(t, bExists, "B's pending must NOT leak into A's Sync — B stays un-flushed")

	// Now B's own Sync flushes B and only B.
	require.NoError(t, store.Sync("B"))
	fb, bExists2 := advDiskFrontier(t, store, "B")
	require.True(t, bExists2)
	require.Equal(t, 2, fb, "B flushes its own latest pending (2), unaffected by A")
	fa2, _ := advDiskFrontier(t, store, "A")
	require.Equal(t, 3, fa2, "A's durable state is untouched by B's Sync")
}

// --- 5. PARK FLOOR + the ADVERSARIAL park-then-more-checkpoints case -------------

// TestAdv_ph61_ParkForcedSync_ThenMoreDeferred_NotClobbered — the park's forced Sync
// writes the parked frontier durably even deep inside a K-window. The ADVERSARIAL
// twist the author's park test misses: AFTER the forced Sync, MORE (deferred)
// checkpoints arrive (as a resumed run would produce). A crash before the next
// boundary must STILL find the parked state on disk — the later deferred writes must
// not clobber or lose it.
func TestAdv_ph61_ParkForcedSync_ThenMoreDeferred_NotClobbered(t *testing.T) {
	dir := t.TempDir()
	const k = 8
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(k)))
	require.NoError(t, err)
	const id = "parkmore"

	// Two deferred checkpoints, then a "park" checkpoint + forced Sync (the floor).
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 1)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 2)))
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 3))) // the park's checkpoint (deferred)
	require.NoError(t, store.Sync(id))                           // park floor: force it durable NOW
	f, exists := advDiskFrontier(t, store, id)
	require.True(t, exists)
	require.Equal(t, 3, f, "park floor: the parked frontier is durable immediately")

	// Resume produces MORE deferred checkpoints (4,5,6,7 — none a boundary yet).
	for j := 4; j <= 7; j++ {
		require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
		// Crash right here: disk must STILL be the parked frontier (3) — never lost,
		// never a torn newer write.
		fj, existsJ := advDiskFrontier(t, store, id)
		require.True(t, existsJ, "j=%d: the parked snapshot is never removed by a later deferred write", j)
		require.Equal(t, 3, fj, "j=%d: park stays durable until the next boundary/Sync overwrites it", j)
	}
	// The next boundary (count reaches k=8) advances it atomically.
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 8)))
	f8, _ := advDiskFrontier(t, store, id)
	require.Equal(t, 8, f8, "at the boundary the frontier advances durably")
}

// TestAdv_ph61_RealPark_DurableUnderBatched_ThenExtraCheckpoints — end-to-end: a real
// WaitForSignal park under Batched(64) is durable on disk (the forced Sync in
// dag.go), and driving additional raw checkpoints for the same ID afterward does not
// silently drop the durable park before a boundary.
func TestAdv_ph61_RealPark_DurableUnderBatched(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)

	b := NewWorkflowBuilder().WithWorkflowID("rp")
	b.AddStartNode("a").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("a", "done")
		return nil
	}))
	b.AddWaitForSignal("waiter", "go").DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)

	wf := &Workflow{DAG: dag, WorkflowID: "rp", Store: store}
	require.ErrorIs(t, wf.Execute(context.Background()), ErrSuspended)

	loaded, err := store.Load("rp")
	require.NoError(t, err, "the park is fsync-durable on disk under Batched (forced Sync)")
	st, ok := loaded.GetNodeStatus("waiter")
	require.True(t, ok)
	require.Equal(t, Waiting, st, "park floor held: waiter is durably Waiting")
}

// --- 6. K EDGE VALUES -----------------------------------------------------------

// TestAdv_ph61_K1_IsStrict — K=1 must be bit-behaviorally Strict: EVERY SaveCheckpoint
// is immediately durable, pending is never populated.
func TestAdv_ph61_K1_IsStrict(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(1)))
	require.NoError(t, err)
	const id = "k1"
	for j := 1; j <= 5; j++ {
		require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
		f, exists := advDiskFrontier(t, store, id)
		require.True(t, exists, "K=1 j=%d: every checkpoint is immediately durable", j)
		require.Equal(t, j, f, "K=1 j=%d: no loss window at all (≡ Strict)", j)
	}
	// A Sync with nothing pending is a clean no-op.
	require.NoError(t, store.Sync(id))
}

// TestAdv_ph61_KGreaterThanLevels — huge K (> total levels): NOTHING is written mid-run
// (all deferred), so a mid-run crash loses everything — but ≤K trivially. The completion
// Save (always durable) then persists the WHOLE run's progress in one snapshot. Verifies
// that the "nothing until completion" case is safe: mid-run absent, post-completion full.
func TestAdv_ph61_KGreaterThanLevels(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(1000)))
	require.NoError(t, err)
	const id = "hugek"
	const levels = 10

	for j := 1; j <= levels; j++ {
		require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
		_, exists := advDiskFrontier(t, store, id)
		require.False(t, exists, "j=%d: with K>levels nothing is fsync'd mid-run (all deferred)", j)
	}
	// Completion Save (the always-durable floor) persists the whole run at once.
	require.NoError(t, store.Save(advSnapshot(id, levels)))
	f, exists := advDiskFrontier(t, store, id)
	require.True(t, exists, "completion Save persists the whole run's progress")
	require.Equal(t, levels, f, "the full frontier is durable after the completion Save")
}

// TestAdv_ph61_CompletionFloorAfterAllDeferred — end-to-end: a real run under
// Batched(K) with K larger than the level count defers every level checkpoint, yet the
// completed run is FULLY persisted (the completion Save floor). Loads to the complete
// final state.
func TestAdv_ph61_CompletionFloorAfterAllDeferred(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(10_000)))
	require.NoError(t, err)
	dag := buildLinearDAG(t, "cf", 50)
	wf := &Workflow{DAG: dag, WorkflowID: "cf", Store: store}
	require.NoError(t, wf.Execute(context.Background()))

	loaded, err := store.Load("cf")
	require.NoError(t, err)
	n := 0
	loaded.ForEachNodeStatus(func(_ string, s NodeStatus) {
		if s == Completed {
			n++
		}
	})
	require.Equal(t, 50, n, "completion floor: all 50 levels durable despite every checkpoint being deferred")
}

// --- 7. RESUME RE-RUNS THE LOST ≤K LEVELS IDEMPOTENTLY --------------------------

// TestAdv_ph61_BatchedCrashResume_ReRunsLostLevelsIdempotently — the durability
// contract's teeth: after a REAL Batched(K) crash mid-run, resume re-runs ONLY the
// lost ≤K levels and reaches the correct final state. Phase 1 runs the DAG for real
// under Batched(4) and "crashes" (a checkpoint that dies, modelling process death) at
// the checkpoint after level 5 — so the durable frontier is the last K-boundary
// (n0..n3), while n4,n5 executed pre-crash but were NOT durable. Phase 2 resumes on a
// FRESH store (counter reset): durably-Completed nodes are skipped; levels that
// executed-but-were-lost re-run (at-least-once, the documented crash contract); levels
// never reached run once. This is the honest crash geometry — no synthetic state.
func TestAdv_ph61_BatchedCrashResume_ReRunsLostLevelsIdempotently(t *testing.T) {
	dir := t.TempDir()
	const id = "resume"
	const k = 4
	const total = 10

	var mu sync.Mutex
	runs := map[string]int{}
	count := func(name string) {
		mu.Lock()
		runs[name]++
		mu.Unlock()
	}
	buildCountingDAG := func() *DAG {
		b := NewWorkflowBuilder().WithWorkflowID(id)
		prev := "n0"
		n0 := prev // capture by value — `prev` is reassigned in the loop below
		b.AddStartNode(prev).WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			count(n0)
			d.SetOutput(n0, "x")
			return nil
		}))
		for i := 1; i < total; i++ {
			name := fmt.Sprintf("n%d", i)
			nm := name
			b.AddNode(name).WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
				count(nm)
				d.SetOutput(nm, "x")
				return nil
			})).DependsOn(prev)
			prev = name
		}
		dag, err := b.Build()
		require.NoError(t, err)
		return dag
	}

	// --- Phase 1: run FOR REAL under Batched(4), crash at the checkpoint after level 5.
	// checkpoint call #(i+1) fires after level i; the crash returns an error at call #6
	// (before it reaches the store), so levels 0..5 executed but only the last
	// K-boundary (call #4 → n0..n3) is durable. n4,n5 executed but were LOST. ---
	store1, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(k)))
	require.NoError(t, err)
	calls := 0
	cp := func(d *WorkflowData) error {
		calls++
		if calls >= 6 {
			return errors.New("simulated power loss")
		}
		return store1.SaveCheckpoint(d)
	}
	sync := func() error { return store1.Sync(id) }
	ctx1 := withSync(withCheckpoint(context.Background(), cp), sync)
	require.Error(t, buildCountingDAG().Execute(ctx1, NewWorkflowData(id)),
		"the simulated crash surfaces as an error")

	f, exists := advDiskFrontier(t, store1, id)
	require.True(t, exists)
	require.Equal(t, 3, f, "crashed durable frontier is the last K-boundary (n0..n3); n4,n5 lost (≤K=4)")

	// --- Phase 2: resume on a FRESH store (counter reset — like a new process).
	// Workflow.Execute auto-loads the persisted state by WorkflowID and resumes. ---
	store2, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(k)))
	require.NoError(t, err)
	wf := &Workflow{DAG: buildCountingDAG(), WorkflowID: id, Store: store2}
	require.NoError(t, wf.Execute(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	// n0..n3 were durably Completed → skipped on resume: exactly one total run (phase 1).
	for i := 0; i <= 3; i++ {
		require.Equalf(t, 1, runs[fmt.Sprintf("n%d", i)],
			"durably-Completed node n%d ran once (phase 1) and is skipped on resume", i)
	}
	// n4,n5 executed pre-crash but were LOST (deferred, past the boundary) → re-run on
	// resume: two total runs. This IS the ≤K at-least-once crash contract (an
	// IdempotencyKey makes the side effect exactly-once; the raw execution count is 2).
	for i := 4; i <= 5; i++ {
		require.Equalf(t, 2, runs[fmt.Sprintf("n%d", i)],
			"lost-but-executed level n%d re-runs on resume (at-least-once, ≤K bound)", i)
	}
	// n6..n9 were never reached pre-crash → run exactly once, on resume.
	for i := 6; i < total; i++ {
		require.Equalf(t, 1, runs[fmt.Sprintf("n%d", i)],
			"never-reached level n%d runs exactly once on resume", i)
	}
	// Final state: every node durably Completed.
	loaded, err := store2.Load(id)
	require.NoError(t, err)
	for i := 0; i < total; i++ {
		st, ok := loaded.GetNodeStatus(fmt.Sprintf("n%d", i))
		require.True(t, ok)
		require.Equalf(t, Completed, st, "n%d Completed in the final durable state", i)
	}
}

// --- 8. SAGA ROLLBACK FLOOR UNDER BATCHED ---------------------------------------

// TestAdv_ph61_SagaRollbackDurableUnderBatched — a saga that fails and rolls back
// under Batched(2) must persist its rolling_back marker + Compensated statuses
// durably, despite prior deferred forward checkpoints. The rollback path uses Save
// (always fsync-durable), so deferral must never leave the rollback un-persisted.
func TestAdv_ph61_SagaRollbackDurableUnderBatched(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(2)))
	require.NoError(t, err)

	var comp int64
	b := NewWorkflowBuilder().WithWorkflowID("saga")
	b.AddStartNode("a").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil })).
		WithCompensation(func(context.Context, *WorkflowData) error { atomic.AddInt64(&comp, 1); return nil })
	b.AddNode("b").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil })).
		WithCompensation(func(context.Context, *WorkflowData) error { atomic.AddInt64(&comp, 1); return nil }).
		DependsOn("a")
	b.AddNode("boom").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("b")
	dag, err := b.Build()
	require.NoError(t, err)

	wf := &Workflow{DAG: dag, WorkflowID: "saga", Store: store}
	require.Error(t, wf.Execute(context.Background()), "the saga fails at 'boom'")
	require.Equal(t, int64(2), atomic.LoadInt64(&comp), "a and b compensate")

	loaded, err := store.Load("saga")
	require.NoError(t, err, "the rolled-back state is durable under Batched (Save floor)")
	require.True(t, loaded.IsRollingBack(), "rolling_back marker durable despite Batched deferral")
	for _, n := range []string{"a", "b"} {
		st, ok := loaded.GetNodeStatus(n)
		require.True(t, ok)
		require.Equal(t, Compensated, st, "node %s durably Compensated under Batched", n)
	}
}

// --- 8b. REPRODUCED DEFECT: Delete is not total under Batched --------------------

// TestAdv_ph61_DEFECT_DeleteLeavesPending_SyncResurrects — [DEFECT, reproduced]
// FlatBuffersStore.Save clears the pending deferred checkpoint (workflow_store.go:583),
// but Delete does NOT. So under Batched(K), a workflow with a deferred (un-fsync'd)
// checkpoint that is then Delete()d leaves its pending CLONE retained in memory — and a
// later Sync() (the durability floor is a PUBLIC method) RE-CREATES the deleted
// workflow's snapshot file on disk. A deleted workflow is resurrected. Delete also
// never removes the ckptCount entry (an unbounded per-ID growth on a long-lived store).
//
// Oracle: the store contract — Delete removes a workflow; after it, the workflow is
// gone and no operation may bring it back. This test asserts the CORRECT behavior and
// currently FAILS (the reproduced defect); it becomes a permanent regression guard once
// Delete clears pending + ckptCount.
//
// Fix: FlatBuffersStore.Delete must clear s.pending[id] (via clearPendingCheckpoint)
// and delete(s.ckptCount, id) under the lock, mirroring Save's pending clear.
func TestAdv_ph61_DEFECT_DeleteLeavesPending_SyncResurrects(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)
	const id = "ghost"

	// A deferred checkpoint under Batched → nothing on disk yet; pending retained.
	require.NoError(t, store.SaveCheckpoint(advSnapshot(id, 3)))
	_, exists := advDiskFrontier(t, store, id)
	require.False(t, exists, "precondition: deferred checkpoint is not yet on disk")

	// Delete the workflow. No .fb exists yet (the checkpoint was deferred), so
	// ErrNotFound is expected/benign — the point of the test is the in-memory
	// pending + counter cleanup Delete must do regardless of the file's presence.
	require.ErrorIs(t, store.Delete(id), ErrNotFound)

	// White-box root cause: Delete left the pending clone AND the counter behind.
	require.Nil(t, store.pending[id],
		"[DEFECT] Delete must clear the pending deferred checkpoint (it does not)")
	_, counterKept := store.ckptCount[id]
	require.False(t, counterKept,
		"[DEFECT] Delete must remove the ckptCount entry (unbounded growth otherwise)")

	// Observable consequence: a later Sync resurrects the deleted workflow on disk.
	require.NoError(t, store.Sync(id))
	_, resurrected := advDiskFrontier(t, store, id)
	require.False(t, resurrected,
		"[DEFECT] a deleted workflow is RESURRECTED on disk by a later Sync (pending survived Delete)")
}

// --- 9. PROPERTY (universal quantification of the bound) ------------------------

// TestAdv_ph61_Property_FrontierEqualsFloor — for a spread of (K, n), the durable
// frontier after driving n checkpoints (no Sync/Save) is EXACTLY floor(n/K)*K, and a
// crash never yields a torn file. This is the ∀-quantified form of bar #3, sampled
// densely across boundary neighborhoods.
func TestAdv_ph61_Property_FrontierEqualsFloor(t *testing.T) {
	ks := []int{1, 2, 3, 5, 7, 8, 13, 16, 32}
	for _, k := range ks {
		for _, n := range []int{0, 1, k - 1, k, k + 1, 2*k - 1, 2 * k, 2*k + 1, 3 * k, 3*k + 1, 50} {
			if n < 0 {
				continue
			}
			k, n := k, n
			dir := t.TempDir()
			store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(uint(k))))
			require.NoError(t, err)
			const id = "prop"
			for j := 1; j <= n; j++ {
				require.NoError(t, store.SaveCheckpoint(advSnapshot(id, j)))
			}
			expected := (n / k) * k
			frontier, exists := advDiskFrontier(t, store, id)
			if expected == 0 {
				require.False(t, exists, "K=%d n=%d: no boundary → no durable file", k, n)
				continue
			}
			require.True(t, exists, "K=%d n=%d: boundary passed → durable file exists", k, n)
			require.Equal(t, expected, frontier, "K=%d n=%d: frontier == floor(n/K)*K", k, n)
			require.Less(t, n-frontier, k, "K=%d n=%d: loss strictly < K", k, n)
		}
	}
}

// --- 10. FUZZ: no input (K, n) breaks the bound or tears a file -----------------

// FuzzGroupCommitFrontier — drive n batched checkpoints under Batched(K) for
// fuzzer-chosen (K, n) and assert the durability invariant holds for ALL inputs: the
// on-disk file (if any) is a COMPLETE parseable snapshot, its frontier is exactly
// floor(n/K)*K, and the crash-loss is strictly < K. Run: go test -run x -fuzz
// FuzzGroupCommitFrontier ./pkg/workflow.
func FuzzGroupCommitFrontier(f *testing.F) {
	// Seed from the boundary neighborhoods where bugs cluster.
	seeds := [][2]uint{{1, 0}, {1, 1}, {2, 1}, {2, 2}, {2, 3}, {8, 7}, {8, 8}, {8, 9}, {3, 10}, {16, 33}}
	for _, s := range seeds {
		f.Add(uint8(s[0]), uint8(s[1]))
	}
	f.Fuzz(func(t *testing.T, kRaw uint8, nRaw uint8) {
		k := int(kRaw)%32 + 1 // K in [1,32]
		n := int(nRaw) % 97   // n in [0,96]
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(uint(k))))
		if err != nil {
			t.Skip()
		}
		const id = "fz"
		for j := 1; j <= n; j++ {
			if err := store.SaveCheckpoint(advSnapshot(id, j)); err != nil {
				t.Fatalf("SaveCheckpoint(%d) errored: %v", j, err)
			}
		}
		expected := (n / k) * k
		frontier, exists := advDiskFrontier(t, store, id)
		if expected == 0 {
			if exists {
				t.Fatalf("K=%d n=%d: durable file exists before the first boundary", k, n)
			}
			return
		}
		if !exists {
			t.Fatalf("K=%d n=%d: no durable file after a boundary passed", k, n)
		}
		if frontier != expected {
			t.Fatalf("K=%d n=%d: frontier=%d want floor=%d", k, n, frontier, expected)
		}
		if n-frontier >= k {
			t.Fatalf("K=%d n=%d: crash-loss %d ≥ K %d (bound violated)", k, n, n-frontier, k)
		}
	})
}
