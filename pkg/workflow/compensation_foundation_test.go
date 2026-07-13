package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeAllStores returns the production stores, each rooted in a fresh temp dir, so a
// round-trip assertion runs identically across FB / JSON / in-mem / SQLite. The SQLite
// store (M15) is included so every AllStores round-trip is part of the indistinguishable-
// under-test bar (ph71 SQL-01 whole-suite). Its Close is registered via t.Cleanup.
func makeAllStores(t *testing.T) map[string]WorkflowStore {
	t.Helper()
	fb, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	js, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	sq, err := NewSQLiteStore(t.TempDir() + "/wf.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sq.Close() }) //nolint:errcheck // test cleanup
	return map[string]WorkflowStore{
		"flatbuffers": fb,
		"json":        js,
		"inmemory":    NewInMemoryStore(),
		"sqlite":      sq,
	}
}

// TestCompensated_RoundTrip_AllStores — §1: the 8th Compensated status survives a
// Save/Load on every store (FB wire 7, JSON, in-mem), alongside the other statuses,
// with no clobber. The unknown-status safe default (obs-3) is unaffected.
func TestCompensated_RoundTrip_AllStores(t *testing.T) {
	for name, store := range makeAllStores(t) {
		t.Run(name, func(t *testing.T) {
			id := "saga-status-" + name
			d := NewWorkflowData(id)
			d.SetNodeStatus("done", Completed)
			d.SetNodeStatus("comp", Compensated)
			d.SetNodeStatus("bypass", Bypassed)
			d.SetNodeStatus("fail", Failed)
			require.NoError(t, store.Save(d))

			got, err := store.Load(id)
			require.NoError(t, err)
			for node, want := range map[string]NodeStatus{
				"done": Completed, "comp": Compensated, "bypass": Bypassed, "fail": Failed,
			} {
				st, ok := got.GetNodeStatus(node)
				require.True(t, ok, "status for %q missing after round-trip", node)
				require.Equal(t, want, st, "status for %q changed across %s round-trip", node, name)
			}
		})
	}
}

// TestRollingBack_DefaultFalseAndRoundTrip — §3: a fresh run is not rolling back;
// the run-level rolling_back marker round-trips true on every store (the durable
// re-entry seam the BLOCKER folds whole into ph46). A forward run (false) is
// omitted from the snapshot so it stays byte-compatible.
func TestRollingBack_DefaultFalseAndRoundTrip(t *testing.T) {
	require.False(t, NewWorkflowData("fresh").IsRollingBack(), "a fresh run must not be rolling back")

	for name, store := range makeAllStores(t) {
		t.Run(name, func(t *testing.T) {
			id := "saga-marker-" + name

			// forward run: marker false round-trips false.
			fwd := NewWorkflowData(id + "-fwd")
			require.NoError(t, store.Save(fwd))
			gotFwd, err := store.Load(id + "-fwd")
			require.NoError(t, err)
			require.False(t, gotFwd.IsRollingBack(), "%s: forward run loaded as rolling back", name)

			// rolling-back run: marker true round-trips true.
			rb := NewWorkflowData(id)
			rb.SetNodeStatus("a", Completed)
			rb.SetRollingBack(true)
			require.NoError(t, store.Save(rb))
			got, err := store.Load(id)
			require.NoError(t, err)
			require.True(t, got.IsRollingBack(), "%s: rolling_back marker lost across round-trip", name)
		})
	}
}

// TestWithCompensation_SetsNodeField — §2: WithCompensation stores the compensating
// action on the built Node (both the Action and the func form); a node without it
// is a rollback no-op (nil compensation).
func TestWithCompensation_SetsNodeField(t *testing.T) {
	b := NewWorkflowBuilder()
	b.AddNode("with-action").WithAction(benchNoopAction()).
		WithCompensation(benchNoopAction())
	b.AddNode("with-func").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { return nil })
	b.AddNode("no-comp").WithAction(benchNoopAction())

	dag, err := b.Build()
	require.NoError(t, err)

	for _, name := range []string{"with-action", "with-func"} {
		n, ok := dag.GetNode(name)
		require.True(t, ok)
		require.NotNil(t, n.Compensation, "%s should carry a compensation", name)
	}
	noComp, ok := dag.GetNode("no-comp")
	require.True(t, ok)
	require.Nil(t, noComp.Compensation, "a node with no WithCompensation is a rollback no-op (nil)")
}

// TestWithCompensation_UnsupportedType_BuildError — §2: an unsupported compensation
// type is reported by Build (mirrors WithAction's actionErr discipline).
func TestWithCompensation_UnsupportedType_BuildError(t *testing.T) {
	b := NewWorkflowBuilder()
	b.AddNode("bad").WithAction(benchNoopAction()).WithCompensation(42)
	_, err := b.Build()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid compensation")
}

// TestCompensated_IsTerminal — §1: Compensated is terminal (never re-armed).
func TestCompensated_IsTerminal(t *testing.T) {
	require.True(t, isTerminalStatus(Compensated), "Compensated must be terminal")
}
