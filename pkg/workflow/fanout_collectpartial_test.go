package workflow

// M21 ph107 — CollectPartial + saga containment tests. Each {Q} biting + seed-break-proven on 3 stores. The
// partial-resume test (T3) is the ph106-F1 value-aware collision-guard payoff (a completed branch re-writes the
// same baseKey[i] on a non-terminal resume → allowed, not a false collision), on a store-seeded non-terminal
// window (the ph105 crash discipline).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// partialBranch: item i>=failFrom fails; i<failFrom succeeds with int64 result i*10 under "out".
func partialBranch(failIdxs map[int]bool) Action {
	return ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		if failIdxs[int(iv)] {
			return fmt.Errorf("branch %d deliberate failure", iv)
		}
		d.Set("out", iv*10)
		return nil
	})
}

// fanFailed reads the CollectPartial __failed__ index list from loaded data.
func fanFailed(t *testing.T, d *WorkflowData, baseKey string) []int {
	t.Helper()
	v, ok := d.Get(fanOutResultFailedKey(baseKey))
	require.True(t, ok, "the __failed__ key %q is present under CollectPartial", fanOutResultFailedKey(baseKey))
	s, isStr := v.(string)
	require.True(t, isStr, "__failed__ is a store-uniform JSON string (got %T)", v)
	var out []int
	require.NoError(t, json.Unmarshal([]byte(s), &out))
	return out
}

// --- {Q}1/{Q}5 CollectPartial completes with the partition; FailFast vs CollectPartial DISTINCT ---------------

// TestCollectPartial_NodeCompletesWithPartition — k-of-N fail: under CollectPartial ALL N branches run, the fan
// node COMPLETES (not Failed), the partition shows the k failed indices, successes are typed under baseKey[i].
// On all 3 stores. SEED-BREAK for the "distinct" property: the SAME scenario under FailFast (default) → node
// Failed (asserted in the sibling test below).
func TestCollectPartial_NodeCompletesWithPartition(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n = 5
			failIdxs := map[int]bool{1: true, 3: true} // branches 1 and 3 fail
			b := NewWorkflowBuilder().WithWorkflowID("wf-cp")
			b.AddFanOut("fan", intItemsExpander(n), partialBranch(failIdxs)).WithResults("r", "out").WithCollectPartial()
			b.AddNode("after").DependsOn("fan").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-cp"
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()), "CollectPartial: the run COMPLETES despite k failures")

			d, lerr := w.Store.Load("wf-cp")
			require.NoError(t, lerr)
			assertNodeStatus(t, d, "fan", Completed)
			assertNodeStatus(t, d, "after", Completed)
			require.Equal(t, n, fanCount(t, d, "r"))
			require.ElementsMatch(t, []int{1, 3}, fanFailed(t, d, "r"), "the partition lists exactly the failed indices")
			// Successes typed under baseKey[i]; failed indices absent.
			for i := 0; i < n; i++ {
				v, present := d.Get(fanOutResultIndexKey("r", i))
				if failIdxs[i] {
					require.False(t, present, "a failed branch %d writes NO result key", i)
				} else {
					require.True(t, present, "a succeeded branch %d writes its result", i)
					require.Equal(t, int64(i*10), coerceInt64(t, v))
				}
			}
		})
	}
}

// TestFailFast_vs_CollectPartial_Distinct — the SAME k-of-N scenario, two policies: FailFast → node Failed, run
// fails (the default, byte-unchanged); CollectPartial → node Completed, all N ran, partition shows k failed.
func TestFailFast_vs_CollectPartial_Distinct(t *testing.T) {
	const n = 4
	failIdxs := map[int]bool{2: true} // branch 2 fails

	// FailFast (default): the run FAILS, the fan node is Failed.
	t.Run("FailFast-fails", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("wf-ff")
		b.AddFanOut("fan", intItemsExpander(n), partialBranch(failIdxs)).WithResults("r", "out")
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(NewInMemoryStore())
		w.WorkflowID = "wf-ff"
		w.DAG = dag
		require.Error(t, w.Execute(context.Background()), "FailFast: a branch failure fails the run")
		d, lerr := w.Store.Load("wf-ff")
		require.NoError(t, lerr)
		assertNodeStatus(t, d, "fan", Failed)
	})

	// CollectPartial: the run COMPLETES, the fan node is Completed, all N ran.
	t.Run("CollectPartial-completes", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("wf-cp2")
		b.AddFanOut("fan", intItemsExpander(n), partialBranch(failIdxs)).WithResults("r", "out").WithCollectPartial()
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(NewInMemoryStore())
		w.WorkflowID = "wf-cp2"
		w.DAG = dag
		require.NoError(t, w.Execute(context.Background()), "CollectPartial: the run completes")
		d, lerr := w.Store.Load("wf-cp2")
		require.NoError(t, lerr)
		assertNodeStatus(t, d, "fan", Completed)
		require.Equal(t, []int{2}, fanFailed(t, d, "r"))
	})
}

// TestCollectPartial_ExternalCancelNotFailure — code-review F1: an EXTERNAL parent-ctx cancel mid-fan-out must
// NOT bucket the interrupted branches into __failed__ + complete the node with a poisoned partition. The node
// must propagate the cancellation (stay non-terminal), so a resume re-drives cleanly. A cancelled branch is NOT a
// failure. SEED-BREAK: remove the ctx.Err() guard → the node Completes with __failed__=[all interrupted] → RED.
func TestCollectPartial_ExternalCancelNotFailure(t *testing.T) {
	const n = 6
	started := make(chan struct{}, n)
	ctx, cancel := context.WithCancel(context.Background())
	branch := ActionFunc(func(bctx context.Context, d *WorkflowData) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-bctx.Done():
			return bctx.Err()
		case <-time.After(2 * time.Second):
			raw, _ := d.Get(FanOutItemKey)
			num, ok := raw.(json.Number)
			if !ok {
				return fmt.Errorf("item is not json.Number: %T", raw)
			}
			iv, err := num.Int64()
			if err != nil {
				return err
			}
			d.Set("out", iv*10)
			return nil
		}
	})
	b := NewWorkflowBuilder().WithWorkflowID("wf-extcancel")
	b.AddFanOut("fan", intItemsExpander(n), branch).WithResults("r", "out").WithCollectPartial()
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "wf-extcancel"
	w.DAG = dag

	go func() { <-started; cancel() }() // cancel the parent ctx once branches are in-flight
	err = w.Execute(ctx)
	require.Error(t, err, "an external cancel propagates — the fan node does NOT falsely Complete")
	require.ErrorIs(t, err, context.Canceled)

	d, lerr := w.Store.Load("wf-extcancel")
	require.NoError(t, lerr)
	st, _ := d.GetNodeStatus("fan")
	require.NotEqual(t, Completed, st, "the fan node stays NON-terminal on external cancel (no poisoned partition persisted)")
}

// --- {Q}3 partial-result resume (the ph106-F1 payoff) --------------------------------------------------------

// TestCollectPartial_PartialResume_F1Payoff — a CollectPartial fan-out crashes mid-flight with SOME branches
// having written their durable results (the fan node still NON-terminal). On resume the completed branches
// re-write the SAME baseKey[i] value → the value-aware collision guard ALLOWS it (no false collision — the exact
// ph106-F1 trap). Store-seeded non-terminal window (ph105 discipline). SEED-BREAK: a presence-only guard would
// refuse the re-write → the resume errors → RED (the F1 fix is what makes this green).
func TestCollectPartial_PartialResume_F1Payoff(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			const n = 4
			store := st.mk(t)
			parentID := "wf-cp-resume"

			// Seed the durable partial state: the expansion journal + a completed branch 0 child (deterministic ID)
			// + parent data carrying baseKey[0] (branch 0's result already flushed) — fan node still Pending.
			items := make([]json.RawMessage, n)
			for i := range items {
				items[i] = json.RawMessage(fmt.Sprintf("%d", i))
			}
			journal, merr := json.Marshal(fanOutJournal{N: n, Items: items})
			require.NoError(t, merr)
			seed := NewWorkflowData(parentID)
			seed.Set(fanOutItemsKey("fan"), string(journal))
			seed.Set(fanOutResultIndexKey("r", 0), int64(0)) // branch 0 result (0*10=0) already durable pre-crash
			require.NoError(t, store.Save(seed))

			// Pre-seed branch 0's child as Completed (so driveBranch's terminal-fast-path returns its result 0 → the
			// SAME value already in baseKey[0] → the value-aware guard must ALLOW the re-write).
			seedBranch := fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) { return int64(idx * 10), nil })
			child0 := &Workflow{DAG: seedBranch(0, 0), WorkflowID: subFanOutChildID(parentID, "fan", 0), Store: store}
			require.NoError(t, child0.Execute(context.Background()))

			// Resume: drive the CollectPartial fan-out. Branch 0 re-writes baseKey[0]=0 (SAME) → allowed; branches
			// 1-3 run fresh. The run COMPLETES (no false collision).
			b := NewWorkflowBuilder().WithWorkflowID(parentID)
			b.AddFanOut("fan", intItemsExpander(n), ActionFunc(func(_ context.Context, d *WorkflowData) error {
				raw, _ := d.Get(FanOutItemKey)
				num, ok := raw.(json.Number)
				if !ok {
					return fmt.Errorf("item is not json.Number: %T", raw)
				}
				iv, err := num.Int64()
				if err != nil {
					return err
				}
				d.Set("out", iv*10)
				return nil
			})).WithResults("r", "out").WithCollectPartial()
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(store)
			w.WorkflowID = parentID
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()), "F1 PAYOFF: the re-written baseKey[0]=same value is allowed, no false collision")

			d, lerr := store.Load(parentID)
			require.NoError(t, lerr)
			require.Equal(t, n, fanCount(t, d, "r"))
			for i := 0; i < n; i++ {
				v, _ := d.Get(fanOutResultIndexKey("r", i))
				require.Equal(t, int64(i*10), coerceInt64(t, v), "branch %d result present + typed after partial resume", i)
			}
		})
	}
}

// --- {Q}4 saga containment (M12) — CollectPartial does NOT trigger parent compensation ------------------------

// TestCollectPartial_SagaContainment — containment (b), BY CONSTRUCTION: a parent with WithCompensation + a
// CollectPartial fan-out that has k branch failures → the fan node COMPLETES → the run does NOT fail → the
// parent's reverse-topo M12 compensation is NEVER triggered. Proven by a compensation counter that stays 0.
func TestCollectPartial_SagaContainment(t *testing.T) {
	var compensated int
	b := NewWorkflowBuilder().WithWorkflowID("wf-saga")
	b.AddStartNode("pre").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })).
		WithCompensation(ActionFunc(func(_ context.Context, _ *WorkflowData) error { compensated++; return nil }))
	b.AddFanOut("fan", intItemsExpander(3), partialBranch(map[int]bool{0: true, 2: true})).
		WithResults("r", "out").WithCollectPartial().DependsOn("pre")
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "wf-saga"
	w.DAG = dag
	require.NoError(t, w.Execute(context.Background()), "CollectPartial with failures: the run COMPLETES")
	require.Zero(t, compensated, "CONTAINMENT (b): a partial branch failure does NOT trigger the parent's M12 compensation")
}
