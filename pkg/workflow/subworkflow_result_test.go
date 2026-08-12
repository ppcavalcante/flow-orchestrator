package workflow

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subWorkflowStoreFactories returns a fresh store per name for ALL THREE persistence
// backends. The result round-trip must be value_long-faithful on every one (the F-1 axis) —
// InMemory keeps typed values, FlatBuffers + SQLite serialize + reload, and a data key
// preserves an int64 through that reload where a node output would not.
func subWorkflowStoreFactories(t *testing.T) []struct {
	name string
	mk   func() WorkflowStore
} {
	t.Helper()
	return []struct {
		name string
		mk   func() WorkflowStore
	}{
		{"InMemory", func() WorkflowStore { return NewInMemoryStore() }},
		{"FlatBuffers", func() WorkflowStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"SQLite", func() WorkflowStore {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sub.db"))
			require.NoError(t, err)
			return s
		}},
	}
}

// TestSubWorkflow_ResultRoundTrip_AllStores — the F-1 3-store AXIS (the load-bearing fidelity
// claim): an int64 child result survives value_long into the declared parent key on EVERY
// store, NOT just InMemory. This is the multi-store blind spot F-1 exposed: reading the result
// from a child DATA key (value_long-faithful) rather than a node OUTPUT (which reloads as a
// string on FB/SQLite) is what keeps the int64 an int64 across the round-trip.
func TestSubWorkflow_ResultRoundTrip_AllStores(t *testing.T) {
	const bigInt = int64(9223372036854775807) // max int64 — the value_long fidelity witness

	for _, sc := range subWorkflowStoreFactories(t) {
		t.Run(sc.name, func(t *testing.T) {
			store := sc.mk()
			var afterN atomic.Int32
			w := parentWithSub(t, store, "wf-fidelity", childProducing(t, bigInt, nil), &afterN)

			require.NoError(t, w.Execute(context.Background()))

			final, err := store.Load("wf-fidelity")
			require.NoError(t, err)
			assertNodeStatus(t, final, "sub", Completed)

			// The int64 result survived value_long into the parent key on THIS store.
			got, ok := final.GetInt64("result")
			require.True(t, ok, "%s: the child int64 result must be a value_long-faithful int64 in the parent, not a string", sc.name)
			assert.Equal(t, bigInt, got, "%s: max-int64 child result round-trips faithfully", sc.name)
		})
	}
}

// TestSubWorkflow_ResultKeyCollision_Loud — a declared result key equal to a PRE-EXISTING
// parent data key (a foreign value) is a loud ErrSubWorkflowResultKeyCollision, not a silent
// last-writer-wins overwrite.
func TestSubWorkflow_ResultKeyCollision_Loud(t *testing.T) {
	store := NewInMemoryStore()

	// A parent whose "before" node pre-populates the same key the sub-workflow declares.
	pb := NewWorkflowBuilder().WithWorkflowID("wf-collide")
	pb.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", "pre-existing-foreign-value")
		return nil
	}))
	pb.AddSubWorkflow("sub", childProducing(t, "child-value", nil)).DependsOn("before").WithResult("result", "result")
	dag, err := pb.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(store)
	w.WorkflowID = "wf-collide"
	w.dag = dag

	err = w.Execute(context.Background())
	require.Error(t, err, "a result-key collision must fail the run, not overwrite")
	require.ErrorIs(t, err, ErrSubWorkflowResultKeyCollision)

	// The pre-existing value is untouched (not last-writer-wins).
	final, lerr := store.Load("wf-collide")
	require.NoError(t, lerr)
	v, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "pre-existing-foreign-value", v, "the foreign value is not overwritten")
}

// TestSubWorkflow_ResultCollision_NonComparable — the collision check must use a total
// equality (reflect.DeepEqual), not !=, so a non-comparable child result (a slice) does not
// PANIC the comparison. AUD-026: the child result arrives via store.Load and so reloads as its
// canonical JSON string, which does not DeepEqual the live pre-existing parent slice — the check
// resolves SAFELY to a collision (no panic), uniformly on every store. (Before AUD-026 InMemory
// alone preserved the slice, giving an idempotent re-apply that FB/SQLite never did.)
func TestSubWorkflow_ResultCollision_NonComparable(t *testing.T) {
	store := NewInMemoryStore()
	pb := NewWorkflowBuilder().WithWorkflowID("wf-slice")
	pb.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", []int{1, 2, 3}) // a non-comparable pre-existing value
		return nil
	}))
	pb.AddSubWorkflow("sub", childProducing(t, []int{1, 2, 3}, nil)).DependsOn("before").WithResult("result", "result")
	dag, err := pb.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(store)
	w.WorkflowID = "wf-slice"
	w.dag = dag
	// The reloaded string result != the live parent slice → a SAFE collision, never a panic.
	err = w.Execute(context.Background())
	require.ErrorIs(t, err, ErrSubWorkflowResultKeyCollision,
		"a non-comparable result canonicalizes to a string and resolves to a safe collision, not a panic")
}
