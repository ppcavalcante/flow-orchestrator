package workflow

// SC-3 pool-drive adversarial bite (M22 ph110 / F-PG-08). The M21 fan-out drive was reworked from "spawn N
// goroutines each blocking on a capN-sized semaphore" (peak goroutines == N) to a BOUNDED WORKER-POOL: min(N,cap)
// workers pull branch indices from ONE feeder goroutine (fanout.go Execute :303-365). Peak goroutines is now
// min(N,cap), NOT N. The M21 suite is BLIND to a concurrency-bound regression: no test drives N >> cap
// (TestFanOut_BoundedParallel_ViaCtxSeam N=12, the gopter property N∈[1,12], the 2-proc kill N=6 — all ≤ cap).
//
// These four bites drive N=1000, cap=8, failure injected at branch k=500 (well inside the run), each RUN under
// -race (the proof is Go, not TLA):
//
//	(a) F-PG-08 — peak goroutines is order-of-cap, NOT ~N (the OOM cliff the rework closes).
//	(b) FailFast cancels un-started + in-flight siblings — FAR fewer than N branch bodies run, the node ERRORS
//	    (not spurious success), and the load-bearing feeder invariant (an un-fed index gets errs[idx]=Canceled, so
//	    the fan-in never reads it as a successful nil result) holds — no silent success aggregate is persisted.
//	(c) Discovery-order aggregate intact at large N — results[i] maps to branch i for all i (index-addressed pool).
//	(d) Crash-resume at large N — already-durable branches are NOT re-executed (exactly-once persistence), the
//	    remaining branches run + complete, the expander runs zero extra times, the final aggregate is complete +
//	    correct across the resume boundary.
//
// Reuses the fanout_test.go harness (fanBranch, intExpander, allCheckpointerStores, fanInts) + the crash-window
// store-seed pattern of TestFanOut_CrashResume_RealWindow / fanout_kill_2proc_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runFanCap drives a single-node fan-out parent workflow with an EXPLICIT MaxConcurrency cap (the withMaxConcurrency
// seam the pool reads). Mirrors runFan but pins the bound so the SC-3 bites drive N >> cap deterministically.
func runFanCap(t *testing.T, store WorkflowStore, parentID string, capN int, a *fanOutAction) error {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID(parentID).WithExecutionConfig(ExecutionConfig{MaxConcurrency: capN})
	pb.AddStartNode(a.nodeName).WithAction(a)
	dag, err := pb.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(store)
	w.WorkflowID = parentID
	w.dag = dag
	return w.Execute(context.Background())
}

// --- (a) F-PG-08: peak goroutines ~ cap, NOT N -----------------------------------------------------------------

// TestFanOut_SC3_PeakGoroutines_OrderOfCap_NotN — N=1000, cap=8. Sample runtime.NumGoroutine() from inside branch
// bodies at steady state (each branch sleeps so ~cap workers overlap) and take the max; the fan-out-attributable
// delta over a pre-run baseline must be order-of-cap (min(n,cap) workers + one feeder + the per-in-flight-branch
// child-node goroutine — ~2·cap), NOT ~N=1000. SEED-BREAK: the OLD "N goroutines each on a semaphore" drive would
// leave ~N live goroutines blocked on the sem → delta ~1000 → RED. This is the F-PG-08 proof.
func TestFanOut_SC3_PeakGoroutines_OrderOfCap_NotN(t *testing.T) {
	const n, capN = 1000, 8
	var peak atomic.Int64
	sample := func() {
		cur := int64(runtime.NumGoroutine())
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				return
			}
		}
	}
	a := &fanOutAction{
		nodeName: "fan", expander: intExpander(n, nil),
		branch: fanBranch(func(_ context.Context, _ int, _ interface{}) (interface{}, error) {
			sample()                         // entry — several workers should be here together at steady state
			time.Sleep(3 * time.Millisecond) // hold the worker so the pool is genuinely saturated
			sample()
			return nil, nil
		}),
	}
	baseline := int64(runtime.NumGoroutine())
	require.NoError(t, runFanCap(t, NewInMemoryStore(), "wf-sc3-pg", capN, a))
	peakDelta := peak.Load() - baseline
	t.Logf("F-PG-08 MEASURED: baseline=%d peak=%d delta=%d (n=%d cap=%d)", baseline, peak.Load(), peakDelta, n, capN)

	require.Positive(t, peakDelta, "the pool spawned worker goroutines")
	require.Less(t, peakDelta, int64(n/4), "peak goroutine delta is order-of-cap, NOT order-of-N (F-PG-08: no ~N-goroutine cliff)")
	require.LessOrEqual(t, peakDelta, int64(8*capN),
		"peak goroutine delta is bounded near min(n,cap) workers + feeder + per-in-flight child, not ~N")
}

// --- (b) FailFast cancels un-started + in-flight (+ the feeder nil/nil invariant) ------------------------------

// TestFanOut_SC3_FailFast_CancelsUnstartedAndInflight — N=1000, cap=8, branch k=500 fails mid-run. Count branch-body
// invocations with an atomic counter: FAR fewer than N must run (un-started siblings never launch; in-flight ones
// observe ctx.Done()). The node must ERROR (not spurious success) and surface the ROOT CAUSE (branch 500), never a
// cancelled sibling's context.Canceled. LOAD-BEARING FEEDER INVARIANT: on cancel the feeder sets errs[idx]=Canceled
// for every un-fed index — an un-fed nil/nil would be read by the fan-in as a SUCCESS keying results[idx]==nil (a
// silent wrong-result). We assert no success aggregate is persisted (Execute returns at the FailFast error, before
// any result keying) — so no un-fed index was treated as a successful nil result. SEED-BREAK: drop the feeder's
// per-index cancel-fill → an un-fed index stays nil/nil → the fan-in's error scan skips it → if k's own worker had
// not recorded the error the node would return success → RED.
func TestFanOut_SC3_FailFast_CancelsUnstartedAndInflight(t *testing.T) {
	const n, capN, k = 1000, 8, 500
	var invocations atomic.Int64
	a := &fanOutAction{
		nodeName: "fan", expander: intExpander(n, nil), resultKey: "agg", resultFrom: "out",
		branch: fanBranch(func(ctx context.Context, idx int, _ interface{}) (interface{}, error) {
			invocations.Add(1)
			if idx == k {
				return nil, fmt.Errorf("branch %d deliberate mid-run failure", k) // the real root cause
			}
			// Brief work so siblings are genuinely in-flight when k fails and can observe the cancel.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Millisecond):
				return idx, nil
			}
		}),
	}
	store := NewInMemoryStore()
	err := runFanCap(t, store, "wf-sc3-ff", capN, a)
	inv := invocations.Load()
	t.Logf("FailFast MEASURED: invocations=%d (n=%d cap=%d k=%d)", inv, n, capN, k)

	require.Error(t, err, "a mid-run branch failure FAILS the fan-out node (no spurious success)")
	require.ErrorContains(t, err, fmt.Sprintf("branch %d", k), "the ROOT-CAUSE failure (branch k) is surfaced")
	require.NotErrorIs(t, err, context.Canceled, "a cancelled un-fed/in-flight sibling must not mask the root cause")
	require.Less(t, inv, int64(n), "FAR fewer than N branch bodies ran (un-started siblings skipped)")
	require.LessOrEqual(t, inv, int64(k+4*capN), "invocations bounded near k+cap, NOT ~N")
	require.GreaterOrEqual(t, inv, int64(k-2*capN), "the failure was well INSIDE the run (~k branches ran before it)")

	// LOAD-BEARING INVARIANT: no un-fed index silently keyed as a successful nil result. Execute returns the
	// FailFast error BEFORE result keying, so the count key (fanout.go :420, success path only) must be ABSENT.
	final, lerr := store.Load("wf-sc3-ff")
	require.NoError(t, lerr)
	_, countPresent := final.Get(fanOutResultCountKey("agg"))
	require.False(t, countPresent, "FailFast errored before keying → no silent success aggregate persisted (no nil/nil un-fed index read as success)")
}

// --- (c) discovery-order aggregate intact at large N -----------------------------------------------------------

// TestFanOut_SC3_DiscoveryOrder_LargeN_IndexAddressed — SUCCESS variant, N=1000, cap=8, each branch's result = its
// own index. Under the pool, results[i] MUST map to branch i for every i (index-addressed, assembled after the pool
// drains). SEED-BREAK: a completion-order append (instead of results[idx]=) → shuffled aggregate → RED. Run across
// all three Checkpointer stores (the aggregate persistence is store-relevant — the typed round-trip moat leg).
func TestFanOut_SC3_DiscoveryOrder_LargeN_IndexAddressed(t *testing.T) {
	const n, capN = 1000, 8
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			a := &fanOutAction{
				nodeName: "fan", expander: intExpander(n, nil), resultKey: "agg", resultFrom: "out",
				branch: fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) {
					return idx, nil
				}),
			}
			store := st.mk(t)
			require.NoError(t, runFanCap(t, store, "wf-sc3-order", capN, a))
			final, err := store.Load("wf-sc3-order")
			require.NoError(t, err)
			arr := fanInts(t, final, "agg")
			require.Len(t, arr, n, "the aggregate has all N elements")
			for i := range n {
				require.Equal(t, i, arr[i], "results[%d] maps to branch %d under the pool (index-addressed)", i, i)
			}
		})
	}
}

// --- (d) crash-resume at large N: exactly-once persistence -----------------------------------------------------

// TestFanOut_SC3_CrashResume_ExactlyOncePersisted_LargeN — N=1000, cap=8, k=500 branch children pre-seeded durably
// Complete (the crash-after-branch-k window: expansion journaled, fan node Pending). Re-drive: the k already-durable
// branches are NOT re-executed (exactly-once persistence via the deterministic FanOutChildID no-op), the
// remaining N−k branches run + complete exactly once, the expander runs ZERO extra times (expansion-once), and the
// final aggregate is complete + index-correct across the resume boundary. Store-seed + re-drive mirrors
// TestFanOut_CrashResume_RealWindow (the reachable state a real out-of-process kill would leave). Run across all
// three Checkpointer stores. SEED-BREAK: a non-deterministic child ID → the k pre-completed children are not found →
// all N re-run → RED.
func TestFanOut_SC3_CrashResume_ExactlyOncePersisted_LargeN(t *testing.T) {
	const n, k, capN = 1000, 500, 8
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			store := st.mk(t)
			parentID := "wf-sc3-resume"
			var resumeSideEffects atomic.Int64
			perBranch := make([]atomic.Int32, n)
			var expandN atomic.Int32

			body := func(_ context.Context, idx int, _ interface{}) (interface{}, error) {
				perBranch[idx].Add(1)
				resumeSideEffects.Add(1)
				return idx, nil
			}
			mkAction := func() *fanOutAction {
				return &fanOutAction{
					nodeName: "fan", expander: intExpander(n, &expandN),
					branch: fanBranch(body), resultKey: "agg", resultFrom: "out",
				}
			}

			// Seed the durable expansion journal (JSON string) + fan node Pending — the REAL crash window.
			items := make([]json.RawMessage, n)
			for i := range items {
				items[i] = json.RawMessage(fmt.Sprintf("%d", i))
			}
			journal, merr := json.Marshal(fanOutJournal{N: n, Items: items})
			require.NoError(t, merr)
			seed := NewWorkflowData(parentID)
			seed.Set(fanOutItemsKey("fan"), string(journal))
			require.NoError(t, store.Save(seed))

			// Pre-seed the first k branch children as Completed (non-counting body → seeding does not inflate
			// resumeSideEffects). Their result key "out"=idx is read back on resume via the terminal-fast-path.
			seedBranch := fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) { return idx, nil })
			for i := range k {
				childID := FanOutChildID(parentID, "fan", i)
				sw := &Workflow{dag: seedBranch(i, i), WorkflowID: childID, Store: store}
				require.NoError(t, sw.Execute(context.Background()))
			}
			require.EqualValues(t, 0, resumeSideEffects.Load(), "seeding used the non-counting body")

			// Resume: the fan node is Pending → Execute re-enters → reads the journal (expander NOT called) → drives
			// only the N−k incomplete branches under the pool.
			require.NoError(t, runFanCap(t, store, parentID, capN, mkAction()))

			require.EqualValues(t, 0, expandN.Load(), "EXPANSION-ONCE: resume read the journal; expander did not run")
			require.EqualValues(t, n-k, resumeSideEffects.Load(), "CRASH-RESUME: only the N−k incomplete branches ran")
			for i := range k {
				require.EqualValues(t, 0, perBranch[i].Load(), "completed branch %d NOT re-executed (exactly-once persistence)", i)
			}
			for i := k; i < n; i++ {
				require.EqualValues(t, 1, perBranch[i].Load(), "remaining branch %d ran exactly once", i)
			}
			final, err := store.Load(parentID)
			require.NoError(t, err)
			arr := fanInts(t, final, "agg")
			require.Len(t, arr, n, "the final aggregate is complete after resume")
			for i := range n {
				require.Equal(t, i, arr[i], "aggregate[%d] == branch %d (correct across the resume boundary)", i, i)
			}
		})
	}
}
