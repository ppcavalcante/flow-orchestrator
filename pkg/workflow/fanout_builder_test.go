package workflow

// M21 ph106 — Builder API + hardening tests. The PUBLIC AddFanOut surface end-to-end, each {Q} biting +
// seed-break-proven on all three Checkpointer stores. The typed node[i] result round-trip is tested ON SQLITE
// specifically (the untyped []interface{} path silently corrupts there — the note #1 / cross-store lesson).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// intItemsExpander yields items 0..n-1 (as int) for the public expander signature.
func intItemsExpander(n int) fanOutExpander {
	return func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		out := make([]interface{}, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
}

// doubleItemAction reads its item via FanOutItemKey and sets a TYPED int64 result (item*10) under "out" — the
// branchResultKey WithResults reads. int64 is the value_long-faithful scalar the typed round-trip depends on.
//
// NOTE: the item arrives as json.Number (UseNumber, INT64-FAITHFUL) — the branch calls .Int64() for a concrete
// int. This is documented on AddFanOut; the RESULT the branch Sets is separately typed (int64 → value_long).
func doubleItemAction() Action {
	return ActionFunc(func(_ context.Context, d *WorkflowData) error {
		item, ok := d.Get(FanOutItemKey)
		if !ok {
			return fmt.Errorf("branch did not receive its item under FanOutItemKey")
		}
		num, ok := item.(json.Number) // UseNumber → json.Number (int64-faithful)
		if !ok {
			return fmt.Errorf("item is not a json.Number: %T", item)
		}
		iv, err := num.Int64()
		if err != nil {
			return fmt.Errorf("item .Int64(): %w", err)
		}
		d.Set("out", iv*10) // TYPED int64 result
		return nil
	})
}

// runBuilderFanOut builds + drives a parent whose single node is an AddFanOut, returning the loaded final data.
func runBuilderFanOut(t *testing.T, store WorkflowStore, id string, configure func(*NodeBuilder)) (*WorkflowData, error) {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID(id)
	nb := b.AddFanOut("fan", intItemsExpander(3), doubleItemAction()).WithResults("results", "out")
	if configure != nil {
		configure(nb)
	}
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = id
	w.DAG = dag
	execErr := w.Execute(context.Background())
	final, lerr := store.Load(id)
	require.NoError(t, lerr)
	return final, execErr
}

// --- {Q}1/{Q}2 AddFanOut end-to-end + TYPED result round-trip on 3 stores (incl. SQLite) ----------------------

// TestAddFanOut_TypedResultRoundTrip — the public surface runs N branches and each result reloads TYPED (int64
// stays an integer, NOT a lossy float64/JSON-string) under results[i], in discovery order, on all 3 stores. This
// is the {Q}2 bar. SEED-BREAK: routing results through a single []interface{} aggregate → SQLite reloads a JSON
// string → coerceInt fails (got string) → RED (that regression is exactly what the typed keying replaced).
func TestAddFanOut_TypedResultRoundTrip(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			final, err := runBuilderFanOut(t, st.mk(t), "wf-typed", nil)
			require.NoError(t, err)

			require.Equal(t, 3, fanCount(t, final, "results"), "count key = N")
			got := fanInts(t, final, "results") // coerceInt REFUSES a float64 → proves TYPED (not lossy) on SQLite
			require.Equal(t, []int{0, 10, 20}, got, "results[i] = item*10 in discovery order, typed on %s", st.name)
		})
	}
}

// TestAddFanOut_ItemInt64Fidelity — the ITEM axis fidelity fix: a large int64 item (above 2^53) survives to the
// branch as its exact value via json.Number, NOT a rounded float64. Without UseNumber the expansion journal's
// default decode into interface{} corrupts it (the [[first-ci-run-saga]] bug on the item axis). SEED-BREAK: a
// default (non-UseNumber) decode in itemForBranch → the item arrives float64-rounded → the branch's .Int64()
// assertion (or the type assert) fails → RED. Tested on all 3 stores.
func TestAddFanOut_ItemInt64Fidelity(t *testing.T) {
	bigItems := []int64{math.MaxInt64, (1 << 53) + 1, 1234567890123456789}
	bigExpander := func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		out := make([]interface{}, len(bigItems))
		for i, v := range bigItems {
			out[i] = v
		}
		return out, nil
	}
	// The branch echoes its item back as an int64 result — so the round-trip proves BOTH item fidelity (json.Number
	// → exact Int64) AND result fidelity (int64 result → value_long on all stores).
	echoAction := ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return fmt.Errorf("item .Int64() lost fidelity: %w", err) // a float64-rounded value may not parse as an exact int64
		}
		d.Set("out", iv)
		return nil
	})
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			b := NewWorkflowBuilder().WithWorkflowID("wf-big")
			b.AddFanOut("fan", bigExpander, echoAction).WithResults("results", "out")
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-big"
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()))

			final, lerr := w.Store.Load("wf-big")
			require.NoError(t, lerr)
			require.Equal(t, len(bigItems), fanCount(t, final, "results"))
			for i, want := range bigItems {
				v, ok := final.Get(fanOutResultIndexKey("results", i))
				require.True(t, ok)
				require.Equal(t, want, coerceInt64(t, v), "item %d (%d) survives int64-faithful (not float64-rounded) on %s", i, want, st.name)
			}
		})
	}
}

// --- {Q}3 width cap -------------------------------------------------------------------------------------------

// TestAddFanOut_WidthCap_LoudFail — an expander resolving > the cap fails loud with ErrFanOutMaxWidth, BEFORE any
// branch runs. Tested with a low override via WithMaxWidth. SEED-BREAK: removing the cap check → cap+1 branches
// run → RED (the branch-run counter would exceed the cap).
func TestAddFanOut_WidthCap_LoudFail(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			var branchRuns int
			var mu = make(chan struct{}, 1)
			mu <- struct{}{}
			countingBranch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
				<-mu
				branchRuns++
				mu <- struct{}{}
				d.Set("out", int64(1))
				return nil
			})
			b := NewWorkflowBuilder().WithWorkflowID("wf-cap")
			b.AddFanOut("fan", intItemsExpander(5), countingBranch).WithResults("r", "out").WithMaxWidth(3)
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-cap"
			w.DAG = dag

			err = w.Execute(context.Background())
			require.ErrorIs(t, err, ErrFanOutMaxWidth, "5 branches > cap 3 → loud ErrFanOutMaxWidth")
			require.Zero(t, branchRuns, "the cap fires BEFORE any branch runs (enforced after N, before branch 1)")
		})
	}
}

// TestAddFanOut_WidthCap_DefaultCeiling — the default cap is DefaultFanOutMaxWidth (1024); N at the ceiling is
// allowed, N over it is refused. (We test the boundary cheaply with a low override matching the default logic:
// a resolved N == cap passes, N == cap+1 fails — exercised via WithMaxWidth to keep the branch count small.)
func TestAddFanOut_WidthCap_BoundaryExactVsOver(t *testing.T) {
	noResultBranch := ActionFunc(func(_ context.Context, d *WorkflowData) error { return nil })
	mk := func(width, cap int) error {
		b := NewWorkflowBuilder().WithWorkflowID("wf-bound")
		b.AddFanOut("fan", intItemsExpander(width), noResultBranch).WithMaxWidth(cap)
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(NewInMemoryStore())
		w.WorkflowID = "wf-bound"
		w.DAG = dag
		return w.Execute(context.Background())
	}
	require.NoError(t, mk(4, 4), "N == cap is allowed (boundary inclusive)")
	require.ErrorIs(t, mk(5, 4), ErrFanOutMaxWidth, "N == cap+1 is refused")
}

// --- {Q}4 N=0 / N=1 boundary ----------------------------------------------------------------------------------

// TestAddFanOut_ZeroWidth — N=0 → the node Completes with count 0 + no indexed keys in a single Execute, a
// downstream dependent runs normally (no hang). On all 3 stores.
func TestAddFanOut_ZeroWidth(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			var downstreamRan bool
			b := NewWorkflowBuilder().WithWorkflowID("wf-zero")
			b.AddFanOut("fan", intItemsExpander(0), doubleItemAction()).WithResults("results", "out")
			b.AddNode("after").DependsOn("fan").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
				downstreamRan = true
				return nil
			}))
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-zero"
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()))

			require.True(t, downstreamRan, "N=0 does not hang the downstream dependent")
			final := store2Load(t, w)
			require.Equal(t, 0, fanCount(t, final, "results"), "N=0 → count 0")
			_, hasIdx0 := final.Get(fanOutResultIndexKey("results", 0))
			require.False(t, hasIdx0, "N=0 → no indexed keys")
		})
	}
}

// TestAddFanOut_SingleWidth — N=1 takes the identical path to N>1 (no special-case): one branch runs, one typed
// result under results[0], count 1.
func TestAddFanOut_SingleWidth(t *testing.T) {
	for _, st := range allCheckpointerStores() {
		t.Run(st.name, func(t *testing.T) {
			b := NewWorkflowBuilder().WithWorkflowID("wf-one")
			b.AddFanOut("fan", intItemsExpander(1), doubleItemAction()).WithResults("results", "out")
			dag, err := b.Build()
			require.NoError(t, err)
			w := NewWorkflow(st.mk(t))
			w.WorkflowID = "wf-one"
			w.DAG = dag
			require.NoError(t, w.Execute(context.Background()))
			final := store2Load(t, w)
			require.Equal(t, 1, fanCount(t, final, "results"))
			require.Equal(t, []int{0}, fanInts(t, final, "results"), "N=1 → results[0] = item 0 * 10")
		})
	}
}

// --- {Q} collision guard --------------------------------------------------------------------------------------

// TestAddFanOut_ResultKeyCollision — a declared base key colliding with a pre-existing FOREIGN parent key is a
// loud ErrFanOutResultKeyCollision, refused before branches run. We seed the parent data with the base key via a
// prior node that Sets it.
func TestAddFanOut_ResultKeyCollision(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("wf-collide")
	b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("results", "pre-existing foreign value") // collides with the fan-out base key
		return nil
	}))
	b.AddFanOut("fan", intItemsExpander(3), doubleItemAction()).WithResults("results", "out").DependsOn("seed")
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "wf-collide"
	w.DAG = dag
	err = w.Execute(context.Background())
	require.ErrorIs(t, err, ErrFanOutResultKeyCollision, "a foreign pre-existing base key is a loud collision")
}

// TestAddFanOut_IndexedKeyValueAwareCollision — review F1: the per-index collision check is VALUE-AWARE. An
// indexed key baseKey[i] pre-existing with the SAME value this node would write is an idempotent re-apply
// (allowed, the resume case); a DIFFERENT foreign value is a loud collision. Pre-empts the ph107 CollectPartial
// trap (which writes partials while non-terminal). We drive a fan-out, then re-run it against seeded parent data:
// (a) results[0] seeded to the value the branch produces → no collision; (b) seeded to a foreign value → collision.
func TestAddFanOut_IndexedKeyValueAwareCollision(t *testing.T) {
	// (a) idempotent re-apply: seed results[0] = 0 (what branch 0 produces: item 0 * 10 = 0). Allowed.
	t.Run("equal-reapply-allowed", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("wf-eq")
		b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set(fanOutResultIndexKey("results", 0), int64(0)) // == branch 0's result → idempotent
			return nil
		}))
		b.AddFanOut("fan", intItemsExpander(3), doubleItemAction()).WithResults("results", "out").DependsOn("seed")
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(NewInMemoryStore())
		w.WorkflowID = "wf-eq"
		w.DAG = dag
		require.NoError(t, w.Execute(context.Background()), "an equal pre-existing indexed value is an idempotent re-apply")
	})
	// (b) foreign value: seed results[1] = 999 (branch 1 produces 10). Loud collision.
	t.Run("foreign-value-refused", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("wf-foreign")
		b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set(fanOutResultIndexKey("results", 1), int64(999)) // != branch 1's result (10) → foreign
			return nil
		}))
		b.AddFanOut("fan", intItemsExpander(3), doubleItemAction()).WithResults("results", "out").DependsOn("seed")
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(NewInMemoryStore())
		w.WorkflowID = "wf-foreign"
		w.DAG = dag
		require.ErrorIs(t, w.Execute(context.Background()), ErrFanOutResultKeyCollision, "a foreign indexed value is a loud collision")
	})
}

// store2Load reloads a workflow's final data (a tiny helper to keep the subtests terse).
func store2Load(t *testing.T, w *Workflow) *WorkflowData {
	t.Helper()
	d, err := w.Store.Load(w.WorkflowID)
	require.NoError(t, err)
	return d
}
