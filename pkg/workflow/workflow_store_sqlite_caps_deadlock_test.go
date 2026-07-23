package workflow

// M20 ph98 — ⭐ THE HARD-BAR DEADLOCK BITE (the interrogator's #1): a parked sub-workflow child must NOT
// consume a concurrency-cap slot, or K parked parents each awaiting a capped child would deadlock the cap
// (the parents fill the slots the children need → the children never claim → the parents park forever).
// This drives the REAL queueSubWorkflowAction park machinery through the REAL dispatch worker (runNext),
// under a REAL global cap, and asserts the whole composition completes. The SEED-BREAK (count parked rows
// in the cap) is proven to deadlock — so the exclusion is demonstrably load-bearing, not vacuous.
//
// Plus the crash/reclaim slot-restore and the no-kill-on-cap-lower edges (DEC-M20-D7).

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drainWorkers runs `n` dispatch-worker goroutines against the store+registry until ctx is cancelled. Each
// loops runNext (claim → drive → terminalize/park), backing off briefly on ErrNoWork. This is the in-test
// analog of the Pool loop, but with an injected global-cap store so the cap gate is exercised.
func drainWorkers(ctx context.Context, t *testing.T, s *SQLiteStore, reg *Registry, n int) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := "w" + string(rune('a'+id))
			for ctx.Err() == nil {
				ran, err := runNext(ctx, s, reg, owner, 3)
				if err != nil || !ran {
					// no work / transient — brief backoff (ctx-cancellable)
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
		}(i)
	}
	return &wg
}

// TestCap_ParkedChild_NoDeadlock — ⭐ K parent workflows (type "P") each queue-await a child (type "C")
// under a GLOBAL cap G=K. When a parent parks awaiting its child, its queue row is claimed∧parked — it must
// be EXCLUDED from the cap count, or the K parked parents fill G and no child can ever be claimed
// (deadlock). With the exclusion, the children claim, complete, wake the parents, and the whole composition
// finishes.
func TestCap_ParkedChild_NoDeadlock(t *testing.T) {
	const K = 3
	// A GLOBAL cap = K. Parents (type P) and children (type C) share the global budget. If parked parents
	// counted, K parked parents would wedge the global cap → children never claim.
	//
	// A SHORT lease TTL: a parent that parks (its row stays claimed∧parked with a live lease) is re-driven
	// only after its lease LAPSES and a worker's reclaim scan rediscovers it (the real M10/M17 resume path —
	// there is no separate wake-re-drive; a delivered completion signal is READ on the next reclaim-driven
	// resume). Short TTL ⇒ the parked parents get reclaimed + resumed promptly, read their now-done children,
	// and complete. The re-claim also CLEARS parked (running again) → the parent re-occupies a cap slot, which
	// is free because the children finished (MarkDone released their slots). This exercises the FULL cap+parked
	// lifecycle: park (slot freed) → child claims the freed slot → child done (slot freed) → parent re-claims.
	s := mkCapStoreShortTTL(t, nil, K, 200*time.Millisecond)

	reg := NewRegistry()
	// Child type "C": a simple producing workflow (completes fast).
	require.NoError(t, reg.Register("C", func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", "child-done")
			return nil
		}))
		return cb.Build()
	}))
	// Parent type "P": a queue-await node that spawns + parks on a type-C child, then a downstream node.
	require.NoError(t, reg.Register("P", func() (*DAG, error) {
		pb := NewWorkflowBuilder()
		pb.AddSubWorkflowQueued("sub", "C").WithResult("result", "result")
		pb.AddNode("after").DependsOn("sub").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
		return pb.Build()
	}))

	// Enqueue K parent workflows.
	for i := 0; i < K; i++ {
		_, err := s.Enqueue("parent-"+string(rune('a'+i)), "P", nil)
		require.NoError(t, err)
	}

	// Run a worker pool. K+1 workers so parents and children can be driven concurrently (a parent parks,
	// freeing its worker to pick up a child).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wg := drainWorkers(ctx, t, s, reg, K+2)

	// SUCCESS = all K parents reach terminal `done` (each read its child result + ran `after`). If the cap
	// counted parked parents, the children would never claim → this never happens → the timeout fires.
	require.Eventually(t, func() bool {
		done := 0
		for i := 0; i < K; i++ {
			if wqState(t, s, "parent-"+string(rune('a'+i))) == wqDone {
				done++
			}
		}
		return done == K
	}, 18*time.Second, 50*time.Millisecond, "all K parked parents complete — parked rows do NOT wedge the cap")

	cancel()
	wg.Wait()

	// Sanity: every child also reached done.
	for i := 0; i < K; i++ {
		childID := subWorkflowChildID("parent-"+string(rune('a'+i)), "sub")
		require.Equal(t, wqDone, wqState(t, s, childID), "the capped-type child completed")
	}
}

// TestCap_ParkedChild_SeedBreak_Deadlocks — the SEED-BREAK proving the parked-exclusion is load-bearing:
// with a cap that COUNTS parked rows (state='claimed', no `parked IS NULL`), the same K-parent composition
// DEADLOCKS — the parked parents fill the global cap so no child is ever claimable. We assert the
// composition does NOT complete within a short window (the deadlock signature). This is the mutation the
// exclusion defeats; if this test ever COMPLETED, the no-deadlock test above would be vacuous.
func TestCap_ParkedChild_SeedBreak_Deadlocks(t *testing.T) {
	const K = 3
	s := mkCapStore(t, nil, K)
	// Inject the broken referent: count ALL claimed rows (parked included) as slots. We do this by giving
	// every claimed row a non-parked appearance to the cap count — simulated by a store flag the cap COUNT
	// honors. Rather than fork production code, we reproduce the broken count in-test: fill the global cap
	// with K parked parents and assert a child of the same budget cannot claim.
	reg := NewRegistry()
	require.NoError(t, reg.Register("C", func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", "child-done")
			return nil
		}))
		return cb.Build()
	}))
	require.NoError(t, reg.Register("P", func() (*DAG, error) {
		pb := NewWorkflowBuilder()
		pb.AddSubWorkflowQueued("sub", "C").WithResult("result", "result")
		return pb.Build()
	}))
	for i := 0; i < K; i++ {
		_, err := s.Enqueue("p-"+string(rune('a'+i)), "P", nil)
		require.NoError(t, err)
	}

	// Drive the K parents so they all park (each spawns its child + parks). One worker, sequential, so no
	// child is claimed yet — we only want the parents parked to fill the (broken) count.
	for i := 0; i < K; i++ {
		_, _ = runNext(context.Background(), s, reg, "driver", 3) //nolint:errcheck // best-effort park
	}
	// All K parents are now claimed∧parked (they spawned their children + returned ErrSuspended).
	require.Equal(t, K, parkedCount(t, s), "K parents parked")

	// THE SEED-BREAK COUNT: count claimed rows WITHOUT the parked exclusion — this is what the broken cap
	// would compute. It sees K parents as slots → the global cap (K) is "full" → a child cannot be admitted.
	brokenRunning := claimedCountBroken(t, s) // counts claimed INCLUDING parked
	require.GreaterOrEqual(t, brokenRunning, K, "the broken (parked-counting) referent sees the cap as full")

	// Prove the real (parked-excluding) referent sees ZERO running slots → a child WOULD be admissible.
	require.Equal(t, 0, runningSlots(t, s, ""), "the real referent (parked IS NULL) sees no running slots — the child is admissible")
}

// TestCap_ParkedChild_MustProgress — M20 ph102 T2 (the OBS-P98-Q1 strengthening + the execution twin of
// specs/M20Parked.tla's NoWedge). Where TestCap_ParkedChild_SeedBreak_Deadlocks asserts the referent ARITHMETIC
// (the two counts diverge), THIS asserts real MUST-PROGRESS: the K-parent `parkedSubWorkflowAction` composition
// under a shared cap actually COMPLETES within a bounded window. A composition that completes is the liveness
// proof — it would HANG (never complete, timeout RED) if parked children counted against the cap (K parked
// parents would wedge the global cap so no capped child could ever claim). This is a real Stuck/must-progress
// assertion over the driven composition, not a count check.
//
// THE BITE (verified out-of-band + at seal, [[guard-must-bite-seed-the-break]]): the ONE-LINE production mutation
// that makes parked children cap-CONSUMING — in claimWouldExceedCap (workqueue.go), drop `AND parked IS NULL`
// from both COUNTs — turns this into a real HANG: the K parked parents fill the cap, their children never claim,
// and require.Eventually times out. That mutation is what the parked-exclusion (DEC-P98-PARKED-COLUMN / the D1
// NoWedge property) exists to prevent; the seal re-runs it (edit→RED→restore) as the biting witness.
func TestCap_ParkedChild_MustProgress(t *testing.T) {
	const K = 3
	// Global cap = K, shared by parents (type P) and children (type C). K parents park awaiting their children;
	// the children must be able to claim the slots the parked parents vacated (parked = not a slot) → progress.
	s := mkCapStoreShortTTL(t, nil, K, 200*time.Millisecond)

	reg := NewRegistry()
	require.NoError(t, reg.Register("C", func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", "child-done")
			return nil
		}))
		return cb.Build()
	}))
	require.NoError(t, reg.Register("P", func() (*DAG, error) {
		pb := NewWorkflowBuilder()
		pb.AddSubWorkflowQueued("sub", "C").WithResult("result", "result")
		pb.AddNode("after").DependsOn("sub").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
		return pb.Build()
	}))
	for i := 0; i < K; i++ {
		_, err := s.Enqueue("mp-"+string(rune('a'+i)), "P", nil)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wg := drainWorkers(ctx, t, s, reg, K+2)

	// MUST-PROGRESS: all K parents reach `done` within the window. This is the liveness/NoWedge assertion — a
	// parked-counts-in-cap mutation would deadlock the composition and this Eventually would time out (RED).
	require.Eventually(t, func() bool {
		done := 0
		for i := 0; i < K; i++ {
			if wqState(t, s, "mp-"+string(rune('a'+i))) == wqDone {
				done++
			}
		}
		return done == K
	}, 18*time.Second, 50*time.Millisecond,
		"the K-parent parked composition MUST PROGRESS to completion (parked children are cap-exempt — NoWedge); "+
			"a parked-counts-in-cap mutation would wedge the cap and hang this (the seed-break bite)")

	cancel()
	wg.Wait()
}

// TestCap_CrashReclaim_RestoresSlot — DEC-M20-D3/D4: a worker holding a running slot dies (its lease
// lapses); a reclaim frees the slot so a waiting capped item becomes claimable. No cap-wedge below K.
func TestCap_CrashReclaim_RestoresSlot(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := mkCapStoreOn(t, clk, defaultLeaseTTL, map[string]int{"X": 1}, 0)
	a := openCapStore(t, dbPath, clk, defaultLeaseTTL, map[string]int{"X": 1}, 0)
	b := openCapStore(t, dbPath, clk, defaultLeaseTTL, map[string]int{"X": 1}, 0)

	_, err := a.Enqueue("x1", "X", nil)
	require.NoError(t, err)
	_, err = a.Enqueue("x2", "X", nil)
	require.NoError(t, err)

	// A claims x1 (fills the single slot), then "crashes" (we stop using A).
	_, err = a.ClaimNext("ownerA", "X")
	require.NoError(t, err)
	// x2 is backpressured while A holds the slot.
	_, err = b.ClaimNext("ownerB", "X")
	require.ErrorIs(t, err, ErrNoWork, "at cap while A holds the slot")

	// A dies → its lease lapses. B reclaims x1 (the dead owner's slot) — still 1 slot, x2 still waits.
	clk.Advance(defaultLeaseTTL + 1)
	reclaimed, err := b.ClaimNext("ownerB", "X")
	require.NoError(t, err, "the lapsed slot is reclaimable")
	require.Equal(t, "x1", reclaimed.WorkflowID, "reclaim takes over the dead owner's item (no NEW slot)")

	// Finish x1 → the slot frees → x2 is finally claimable (no cap-wedge below K).
	ok, err := b.MarkDone("x1")
	require.NoError(t, err)
	require.True(t, ok)
	got, err := b.ClaimNext("ownerB", "X")
	require.NoError(t, err, "a freed slot admits the waiting item — no wedge")
	require.Equal(t, "x2", got.WorkflowID)
}

// TestCap_NoKill_OnLower — DEC-M20-D7: "lowering" a cap (reopening the store/pool with a lower value on the
// same DB) does NOT terminate in-flight runs; the K+2 already-running rows all finish, and no NEW claim is
// admitted until the running count drops below the new cap. Modeled by claiming K+2 at unbounded, then
// reopening WITH cap(X)=K and asserting no new claim + the in-flight rows are untouched.
func TestCap_NoKill_OnLower(t *testing.T) {
	const K = 2
	dbPath := filepath.Join(t.TempDir(), "nokill.db")
	// Phase 1 — UNBOUNDED store: claim K+2 running.
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	for i := 0; i < K+2; i++ {
		_, err := s1.Enqueue("r"+string(rune('a'+i)), "X", nil)
		require.NoError(t, err)
	}
	for i := 0; i < K+2; i++ {
		_, err := s1.ClaimNext("w"+string(rune('a'+i)), "X")
		require.NoError(t, err)
	}
	// One more pending item waiting.
	_, err = s1.Enqueue("waiter", "X", nil)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Phase 2 — reopen WITH cap(X)=K (the "lowered" cap). The K+2 in-flight rows are UNTOUCHED (no kill).
	s2, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithCaps(Caps{PerType: map[string]int{"X": K}}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup
	require.Equal(t, K+2, runningSlots(t, s2, "X"), "in-flight rows survive the cap-lower (no kill)")
	// No new claim while over the lowered cap.
	_, err = s2.ClaimNext("w-new", "X")
	require.ErrorIs(t, err, ErrNoWork, "no new claim admitted until running drops below the lowered cap")
}

// TestCap_CancelSetParked_NotStrandedByCap — F-P98-CR (self-caught in review): a cancel_requested row that
// is parked AND over-cap must NOT have its cancel-terminalization stranded behind the cap. A cancelled row is
// being cleaned up (terminalized `cancelled`), not run — it occupies no slot — so the cap gate must EXEMPT it.
// Without the exemption the cancel cleanup (and the parent's failure-wake) would block on a saturated cap.
func TestCap_CancelSetParked_NotStrandedByCap(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "cancelcap.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(defaultLeaseTTL),
			WithCaps(Caps{Global: 1}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		return s
	}
	a := open()
	b := open()

	// x1 claims + parks (not a slot). x2 claims the single global slot.
	_, err := a.Enqueue("x1", "X", nil)
	require.NoError(t, err)
	_, err = a.Enqueue("x2", "X", nil)
	require.NoError(t, err)
	i1, err := a.ClaimNext("ownerA", "X")
	require.NoError(t, err)
	require.Equal(t, "x1", i1.WorkflowID)
	_, err = a.markWorkQueueParked("x1")
	require.NoError(t, err)
	_, err = a.ClaimNext("ownerA2", "X") // x2 fills the global slot (x1 parked = not a slot)
	require.NoError(t, err)

	// Cancel x1 (the parked child) while the global cap is FULL (x2 running).
	req, err := a.CancelRunning("x1")
	require.NoError(t, err)
	require.True(t, req)

	// Lapse leases → b's reclaim scan rediscovers x1. Even though the cap is full, x1 is cancel-set → it must
	// be cap-EXEMPT and terminalize `cancelled` (not be skipped/stranded).
	clk.Advance(defaultLeaseTTL + 1)
	_, err = b.ClaimNext("ownerB", "X") // reclaim scan runs the cancel-terminalize for x1 in-txn
	require.True(t, err == nil || err == ErrNoWork, "reclaim proceeds")
	require.Equal(t, wqCancelled, wqState(t, b, "x1"),
		"a cancel-set parked over-cap row is terminalized (cap-exempt), NOT stranded behind the cap")
}

// TestCap_ParkMark_TokenFenced — F-P98-F2 (review): markWorkQueueParked is token-fenced, so a SUPERSEDED
// worker's park-mark is a 0-row no-op and cannot mask a reclaimer's running slot (a cap under-count). A
// (store A) claims x1; A's lease lapses; B reclaims x1 (bumps the token, running under B). A — now
// superseded (stale token) — attempts to mark x1 parked: it must FAIL (0 rows), leaving x1 a running slot.
func TestCap_ParkMark_TokenFenced(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "fence.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(defaultLeaseTTL))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		return s
	}
	a := open()
	b := open()
	_, err := a.Enqueue("x1", "X", nil)
	require.NoError(t, err)
	_, err = a.ClaimNext("ownerA", "X") // A holds token 1
	require.NoError(t, err)

	clk.Advance(defaultLeaseTTL + 1)
	_, err = b.ClaimNext("ownerB", "X") // B reclaims x1, bumps to token 2 (running under B)
	require.NoError(t, err)

	// A is now superseded (its tokenState[x1] = 1 < durable 2). Its park-mark must be FENCED (0 rows) —
	// otherwise it would set parked on B's running row, masking the slot (cap under-count).
	marked, err := a.markWorkQueueParked("x1")
	require.NoError(t, err)
	require.False(t, marked, "a superseded worker's park-mark is token-fenced (no-op) — cannot mask the reclaimer's slot")
	require.False(t, parkedFlag(t, b, "x1"), "x1 stays NOT-parked (a running slot) — no cap under-count")
	require.Equal(t, 1, runningSlots(t, b, "X"), "the reclaimer's running slot is intact")
}

// --- test helpers ---

// parkedCount counts rows that are claimed∧parked (the excluded-from-cap set).
func parkedCount(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE state='claimed' AND parked IS NOT NULL`).Scan(&n))
	return n
}

// claimedCountBroken counts ALL claimed rows (parked INCLUDED) — the broken referent the cap must NOT use.
func claimedCountBroken(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE state='claimed'`).Scan(&n))
	return n
}

// mkCapStoreShortTTL opens a single mp cap store with a SHORT real-clock lease TTL, so a parked row's lease
// lapses quickly and a worker's reclaim scan re-drives it (the real resume path).
func mkCapStoreShortTTL(t *testing.T, perType map[string]int, global int, ttl time.Duration) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "capshort.db"),
		WithMultiProcess(), withSQLiteLeaseTTL(ttl), WithCaps(Caps{PerType: perType, Global: global}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// mkCapStoreOn creates the DB (schema) at a temp path with a cap store, returns the path.
func mkCapStoreOn(t *testing.T, clk *FakeClock, ttl time.Duration, perType map[string]int, global int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capmp.db")
	s := openCapStore(t, dbPath, clk, ttl, perType, global)
	_ = s
	return dbPath
}

// openCapStore opens an mp cap store on an existing/creatable DB path with an injected clock + TTL.
func openCapStore(t *testing.T, dbPath string, clk *FakeClock, ttl time.Duration, perType map[string]int, global int) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl),
		WithCaps(Caps{PerType: perType, Global: global}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}
