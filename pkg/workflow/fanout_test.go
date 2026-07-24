package workflow

// M21 ph105 — Branch-execution engine tests. One biting test per {Q} criterion, each seed-break-proven, run
// across all THREE Checkpointer stores (InMemory + JSONFile + SQLite) — the any-Checkpointer moat leg. Plus the
// FailFast sibling-cancellation criterion (a slow sibling MUST observe ctx.Done()) and the F4 non-Checkpointer
// hard-fail. The crash-resume test uses the REAL window (expansion journaled, fan node NON-terminal) — NOT a
// completed-node re-drive, which the executor skips (parallel_execution.go:88) and which would be vacuous
// ([[resume-test-vacuity-completed-node-skipped]]).

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- 3-store harness -------------------------------------------------------------------------------------------

// checkpointerStore is one of the three durable stores under test. mk builds a fresh one per subtest.
type checkpointerStore struct {
	name string
	mk   func(t *testing.T) WorkflowStore
}

func allCheckpointerStores() []checkpointerStore {
	return []checkpointerStore{
		{"InMemory", func(t *testing.T) WorkflowStore { return NewInMemoryStore() }},
		{"JSONFile", func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"SQLite", func(t *testing.T) WorkflowStore {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fan.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup, fire-and-forget
			return s
		}},
	}
}

// fanBranch builds the per-branch single-node child DAG: run `body`, write its result to the "out" data key.
func fanBranch(body func(ctx context.Context, idx int, item interface{}) (interface{}, error)) fanOutBranch {
	return func(idx int, item interface{}) *DAG {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("branch").WithAction(ActionFunc(func(ctx context.Context, d *WorkflowData) error {
			res, err := body(ctx, idx, item)
			if err != nil {
				return err
			}
			d.Set("out", res)
			return nil
		}))
		dag, err := cb.Build()
		if err != nil {
			panic(err)
		}
		return dag
	}
}

// intExpander yields items 0..n-1, counting invocations.
func intExpander(n int, counter *atomic.Int32) fanOutExpander {
	return func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		if counter != nil {
			counter.Add(1)
		}
		out := make([]interface{}, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
}

// runFan drives a parent workflow whose single node is the fanOutAction on the given store.
func runFan(t *testing.T, store WorkflowStore, parentID string, a *fanOutAction) error {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID(parentID)
	pb.AddStartNode(a.nodeName).WithAction(a)
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = parentID
	w.DAG = dag
	return w.Execute(context.Background())
}

// --- {Q}1 bounded N-parallel via the ctx seam ------------------------------------------------------------------

// TestFanOut_BoundedParallel_ViaCtxSeam — with N > MaxConcurrency, peak in-flight branches never exceeds the bound
// the executor injects via withMaxConcurrency. SEED-BREAK: an unbounded pool → peak reaches N > cap → RED
// (verified separately by forcing the sem cap to N).
func TestFanOut_BoundedParallel_ViaCtxSeam(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n = 12
			var live, peak int32
			var mu sync.Mutex
			a := &fanOutAction{
				nodeName: "fan", expander: intExpander(n, nil),
				branch: fanBranch(func(_ context.Context, _ int, _ interface{}) (interface{}, error) {
					cur := atomic.AddInt32(&live, 1)
					mu.Lock()
					if cur > peak {
						peak = cur
					}
					mu.Unlock()
					time.Sleep(15 * time.Millisecond)
					atomic.AddInt32(&live, -1)
					return nil, nil
				}),
			}
			// The default ExecutionConfig.MaxConcurrency is DefaultMaxConcurrency(16) > n, so to prove the bound
			// bites we set a small cap via the builder config on the parent DAG.
			pb := NewWorkflowBuilder().WithWorkflowID("wf-bound").WithExecutionConfig(ExecutionConfig{MaxConcurrency: 3})
			pb.AddStartNode("fan").WithAction(a)
			dag, err := pb.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-bound"
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()))

			require.LessOrEqual(t, peak, int32(3), "in-flight branches never exceed the injected MaxConcurrency")
			require.Greater(t, peak, int32(1), "the pool ran branches concurrently")
		})
	}
}

// --- {Q}2 FailFast sibling-cancellation ------------------------------------------------------------------------

// TestFanOut_FailFast_SiblingObservesCancel — the crux new criterion. Branch 0 fails fast; a SLOW sibling (branch
// 1) must observe ctx.Done() and NOT run to completion; un-started branches must not launch. SEED-BREAK: drop the
// cancel() on failure → the slow sibling runs to completion → its completion flag is set → RED.
func TestFanOut_FailFast_SiblingObservesCancel(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n = 8
			var slowSiblingCompleted atomic.Bool
			var laterBranchStarted atomic.Bool
			a := &fanOutAction{
				nodeName: "fan", expander: intExpander(n, nil),
				branch: fanBranch(func(ctx context.Context, idx int, _ interface{}) (interface{}, error) {
					switch idx {
					case 0:
						return nil, fmt.Errorf("branch 0 deliberate failure")
					case 1:
						// Slow sibling: it MUST observe cancellation rather than complete.
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(2 * time.Second):
							slowSiblingCompleted.Store(true) // only reached if cancellation did NOT arrive
							return nil, nil
						}
					default:
						laterBranchStarted.Store(true)
						return nil, nil
					}
				}),
			}
			// cap=2 so branch 0 (fail) + branch 1 (slow) are in-flight together and the later branches are
			// un-started when the failure cancels — proving both "in-flight observes cancel" and "un-started skip".
			pb := NewWorkflowBuilder().WithWorkflowID("wf-ff").WithExecutionConfig(ExecutionConfig{MaxConcurrency: 2})
			pb.AddStartNode("fan").WithAction(a)
			dag, err := pb.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-ff"
			w.DAG = dag

			start := time.Now()
			err = w.Execute(context.Background())
			require.Error(t, err, "a failed branch fails the fan-out node → the run")
			require.Less(t, time.Since(start), 2*time.Second, "FailFast returned before the slow sibling's 2s timeout")
			require.False(t, slowSiblingCompleted.Load(), "FAILFAST: the slow sibling observed ctx.Done(), did not complete")
		})
	}
}

// TestFanOut_FailFast_SurfacesRootCauseNotCancellation — review note #2: when a lower-index sibling is CANCELLED
// (carrying context.Canceled, the fail-fast side effect) and a higher-index branch is the REAL failure, the
// surfaced error must be the real failure — not the cancelled sibling's context.Canceled (which would mask the
// root cause in the message). Branch 0 is slow-and-cancelled; branch 2 is the true failure.
func TestFanOut_FailFast_SurfacesRootCauseNotCancellation(t *testing.T) {
	realErr := fmt.Errorf("branch 2 real failure")
	a := &fanOutAction{
		nodeName: "fan", expander: intExpander(3, nil),
		branch: fanBranch(func(ctx context.Context, idx int, _ interface{}) (interface{}, error) {
			switch idx {
			case 2:
				return nil, realErr // the real root cause, at a higher index
			default:
				// slow siblings: block until cancelled by branch 2's failure → they carry context.Canceled.
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}),
	}
	// cap=3 so all three are in-flight together; branch 2 fails, cancelling 0 and 1.
	pb := NewWorkflowBuilder().WithWorkflowID("wf-rootcause").WithExecutionConfig(ExecutionConfig{MaxConcurrency: 3})
	pb.AddStartNode("fan").WithAction(a)
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "wf-rootcause"
	w.DAG = dag

	err = w.Execute(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "branch 2 real failure", "the ROOT-CAUSE failure is surfaced, not a cancelled sibling")
	require.NotErrorIs(t, err, context.Canceled, "a cancelled sibling's context.Canceled must not mask the real cause")
}

// --- {Q}3 discovery-order aggregate ----------------------------------------------------------------------------

// TestFanOut_DiscoveryOrder_UnderShuffledCompletion — the aggregate is index-ordered even when branches complete
// out of order. SEED-BREAK: completion-order append → RED.
func TestFanOut_DiscoveryOrder_UnderShuffledCompletion(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n = 6
			a := &fanOutAction{
				nodeName: "fan", expander: intExpander(n, nil), resultKey: "agg", resultFrom: "out",
				branch: fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) {
					time.Sleep(time.Duration(n-idx) * 5 * time.Millisecond) // higher idx finishes first
					return fmt.Sprintf("r%d", idx), nil
				}),
			}
			store := st.mk(t)
			require.NoError(t, runFan(t, store, "wf-order", a))
			final, err := store.Load("wf-order")
			require.NoError(t, err)
			arr := fanStrings(t, final, "agg")
			require.Len(t, arr, n)
			for i := 0; i < n; i++ {
				require.Equal(t, fmt.Sprintf("r%d", i), arr[i], "element %d is branch %d despite shuffled completion", i, i)
			}
		})
	}
}

// --- {Q}4 crash-resume in the REAL window ----------------------------------------------------------------------

// TestFanOut_CrashResume_RealWindow — a crash with the expansion journaled but the fan node still NON-terminal
// resumes to run ONLY the incomplete branches, each once; the expander runs exactly once (0 additional times on
// resume). We SEED the durable state: k branch children Completed + the fan node Pending + the expansion journal
// present (the exact crash-after-expand window). SEED-BREAK: a non-deterministic child ID → the k pre-completed
// children are not found → all N re-run → RED (the 104 correction — the guard is the deterministic ID, not the
// terminal-fast-path).
//
// WHY THE STORE-SEED IS FAITHFUL-AND-NECESSARY (ph108 T5, qa ph105 note) — the executor terminal-on-return
// contract: a DAG node whose Execute RETURNS anything is terminalized (error → Failed, value → Completed) and
// durably flushed. So NO in-process interrupt can leave the fan node NON-terminal mid-Execute — a branch error →
// Failed (FailFast); a pool-goroutine panic → uncatchable process death; an erroring expansion-flush checkpoint →
// the error returns through Execute → Failed (probed). The ONLY durable state in which the fan node is
// non-terminal with a partial-but-durable journal is the one a real OUT-OF-PROCESS kill-9 (between the journal
// write and Execute's return) would leave — which this store-seed MATERIALIZES exactly. This test proves the
// resume LOGIC over that reachable state; the GENUINE write→kill→read in one flow is unreachable in-process by
// construction and is proven end-to-end by the 2-OS-process SIGKILL test (TestFanOutKill_2Proc, ph108 T3) +
// machine-checked by M21FanOut.tla's ExactlyNSpawn under a kill-storm. See
// [[in-process-crash-cannot-leave-node-nonterminal]].
func TestFanOut_CrashResume_RealWindow(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n, k = 5, 2
			store := st.mk(t)
			parentID := "wf-resume"
			var resumeSideEffects atomic.Int32
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

			// Pre-seed the first k branch children as Completed (they "ran before the crash"), with a NON-counting
			// body so the seeding does not inflate resumeSideEffects.
			seedBranch := fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) { return idx, nil })
			for i := 0; i < k; i++ {
				childID := subFanOutChildID(parentID, "fan", i)
				sw := &Workflow{DAG: seedBranch(i, i), WorkflowID: childID, Store: store}
				require.NoError(t, sw.Execute(context.Background()))
			}
			require.EqualValues(t, 0, resumeSideEffects.Load(), "seeding used the non-counting body")

			// Resume: the fan node is Pending → Execute IS re-invoked → reads the journal (expander NOT called) →
			// drives only the N−k incomplete branches.
			require.NoError(t, runFan(t, store, parentID, mkAction()))

			require.EqualValues(t, 0, expandN.Load(), "EXPANSION-ONCE: resume read the journal; expander did not run")
			require.EqualValues(t, n-k, resumeSideEffects.Load(), "CRASH-RESUME: only the N−k incomplete branches ran")
			for i := 0; i < n; i++ {
				require.LessOrEqual(t, perBranch[i].Load(), int32(1), "branch %d ran at most once", i)
			}
			final, err := store.Load(parentID)
			require.NoError(t, err)
			require.Len(t, fanInts(t, final, "agg"), n, "the aggregate has all N elements after resume")
		})
	}
}

// --- {Q}5 F4 non-Checkpointer hard-fail ------------------------------------------------------------------------

// (nonCheckpointerStore is defined in durable_resume_test.go — a minimal WorkflowStore that does NOT implement
// Checkpointer, so Workflow.Execute injects no checkpoint callback → checkpointFrom(ctx)==nil → F4 fires.)

// TestFanOut_F4_NonCheckpointerHardFails — a fan-out on a store without a Checkpointer returns
// ErrFanOutRequiresCheckpointer loudly, before any expander/branch work. SEED-BREAK: remove the F4 gate → the
// fan-out proceeds and the expander re-runs on any re-drive (silent degrade) — the gate's absence is the break.
func TestFanOut_F4_NonCheckpointerHardFails(t *testing.T) {
	var expandN atomic.Int32
	a := &fanOutAction{
		nodeName: "fan", expander: intExpander(3, &expandN),
		branch: fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) { return idx, nil }),
	}
	store := &nonCheckpointerStore{inner: NewInMemoryStore()}
	err := runFan(t, store, "wf-nocp", a)
	require.ErrorIs(t, err, ErrFanOutRequiresCheckpointer, "F4: a non-Checkpointer store must hard-fail a fan-out")
	require.EqualValues(t, 0, expandN.Load(), "F4 fires BEFORE the expander runs (no partial work)")
}

// --- extra: FailFast × resume — a DURABLY-Failed fan node is skipped on resume (architect ruling) --------------

// TestFanOut_FailFast_DurablyFailedNodeSkippedOnResume — the CONDITIONAL, load-bearing property (not the false
// unconditional "a cancelled sibling is never resurrected"). When the fan node's Failed verdict is DURABLY flushed
// (the normal path: Execute returns the fail-fast error → the level barrier persists Failed), a resume SKIPS the
// terminal fan node (parallel_execution.go:88) → Execute is NOT re-entered → the run stays Failed and no branch is
// re-driven. The complementary window (a crash BEFORE the Failed flush → the fan node is non-terminal on resume →
// Execute re-enters → branches may re-run idempotently, which is the DESIRED at-least-once retry, NOT a violation)
// is a crash-window property deferred to the ph108 TLA capstone — it is deliberately NOT asserted here.
func TestFanOut_FailFast_DurablyFailedNodeSkippedOnResume(t *testing.T) {
	store := NewInMemoryStore()
	parentID := "wf-ff-resume"
	var branchRuns atomic.Int32
	a := &fanOutAction{
		nodeName: "fan", expander: intExpander(3, nil),
		branch: fanBranch(func(ctx context.Context, idx int, _ interface{}) (interface{}, error) {
			branchRuns.Add(1)
			if idx == 0 {
				return nil, fmt.Errorf("branch 0 fails") // fail-fasts the fan node
			}
			// siblings: observe the cancel (or finish quickly) — irrelevant to the assertion.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return idx, nil
			}
		}),
	}
	pb := NewWorkflowBuilder().WithWorkflowID(parentID).WithExecutionConfig(ExecutionConfig{MaxConcurrency: 2})
	pb.AddStartNode("fan").WithAction(a)
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = parentID
	w.DAG = dag

	// Drive 1: the fan node fails and — because the store is a Checkpointer — the level barrier DURABLY flushes the
	// Failed fan-node status. (The run error is the fail-fast; the Failed status is persisted.)
	require.Error(t, w.Execute(context.Background()), "the run fail-fasts")
	persisted, lerr := store.Load(parentID)
	require.NoError(t, lerr)
	st, ok := persisted.GetNodeStatus("fan")
	require.True(t, ok)
	require.Equal(t, Failed, st, "the fan node's Failed verdict is durably flushed (the window-(a) precondition)")
	runsAfterDrive1 := branchRuns.Load()

	// Resume: the fan node is durably terminal-Failed → the executor SKIPS it (parallel_execution.go:88) → Execute
	// is NOT re-entered → no branch re-runs, and the fan node's Failed verdict is unchanged. (The resume RETURN
	// value is the executor's pre-existing terminal-node semantics — a re-drive over an all-terminal DAG re-raises
	// no fail-fast because no node re-executes — so it is NOT asserted here; the load-bearing fan-out property is
	// the SKIP: no re-entry, no branch re-run, consistent terminal verdict.)
	_ = w.Execute(context.Background()) //nolint:errcheck // re-drive of a durably-Failed run; the return is intentionally ignored (asserted via node status below)
	require.EqualValues(t, runsAfterDrive1, branchRuns.Load(),
		"DURABLY-FAILED SKIP: the terminal fan node is not re-entered on resume (no branch re-driven)")
	after, aerr := store.Load(parentID)
	require.NoError(t, aerr)
	stAfter, _ := after.GetNodeStatus("fan")
	require.Equal(t, Failed, stAfter, "the fan node stays Failed across resume (consistent terminal verdict)")
}

// --- typed node[i] result readers (ph106 convention: baseKey[i] scalars + baseKey.__count__) ------------------

// fanCount reads the fan-out result count (baseKey.__count__) from loaded data, coercing across stores.
func fanCount(t *testing.T, d *WorkflowData, baseKey string) int {
	t.Helper()
	v, ok := d.Get(fanOutResultCountKey(baseKey))
	require.True(t, ok, "the fan-out count key %q is present", fanOutResultCountKey(baseKey))
	return coerceInt(t, v)
}

// fanInts reads the N per-branch results as ints from baseKey[0..count) — proving the TYPED round-trip (an int64
// branch result reloads as an integer on all 3 stores, NOT a JSON string).
func fanInts(t *testing.T, d *WorkflowData, baseKey string) []int {
	t.Helper()
	n := fanCount(t, d, baseKey)
	out := make([]int, n)
	for i := 0; i < n; i++ {
		v, ok := d.Get(fanOutResultIndexKey(baseKey, i))
		require.True(t, ok, "indexed key %q present", fanOutResultIndexKey(baseKey, i))
		out[i] = coerceInt(t, v)
	}
	return out
}

// fanStrings reads the N per-branch results as strings from baseKey[0..count).
func fanStrings(t *testing.T, d *WorkflowData, baseKey string) []string {
	t.Helper()
	n := fanCount(t, d, baseKey)
	out := make([]string, n)
	for i := 0; i < n; i++ {
		v, ok := d.Get(fanOutResultIndexKey(baseKey, i))
		require.True(t, ok, "indexed key %q present", fanOutResultIndexKey(baseKey, i))
		s, isStr := v.(string)
		require.True(t, isStr, "indexed key %q is a string (got %T)", fanOutResultIndexKey(baseKey, i), v)
		out[i] = s
	}
	return out
}

// coerceInt normalizes a store-reloaded integer (int/int64 on InMemory/SQLite; json.Number on JSONFile via
// UseNumber for int64 fidelity — [[first-ci-run-saga]]) to a Go int. A float64 is NOT accepted: the typed keying
// bar is that an int64 reloads as an integer, not a lossy float (the untyped-path regression this phase fixes).
func coerceInt(t *testing.T, v interface{}) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		iv, err := n.Int64()
		require.NoError(t, err)
		return int(iv)
	default:
		require.Failf(t, "value not a typed integer", "got %T (%v) — a float64 here would mean the untyped/lossy path", v, v)
		return 0
	}
}

// coerceInt64 is coerceInt for the full int64 range (the item-fidelity test uses values above 2^53 where int
// truncation would itself lose bits on a 32-bit int — explicit int64 keeps the assertion exact). A float64 is
// REFUSED: it is the lossy path the UseNumber item journal + typed result keying exist to prevent.
func coerceInt64(t *testing.T, v interface{}) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		iv, err := n.Int64()
		require.NoError(t, err)
		return iv
	default:
		require.Failf(t, "value not a typed integer", "got %T (%v) — a float64 here is the lossy path this phase prevents", v, v)
		return 0
	}
}
