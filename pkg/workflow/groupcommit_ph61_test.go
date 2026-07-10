package workflow

// M14 ph61 group-commit bites (DEC-M14-GROUPCOMMIT / B1). Bar #1 (win), #3
// (crash-loss ≤K, torn-safe), #4 (suspend floor stays durable under Batched). Strict
// bit-identity (#2) is covered by the existing durable/saga/resume suite passing
// UNCHANGED under the default; det-tax (#5) by TestPerfCeiling_DetTax.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildLinearDAG builds an n-level linear chain (each node depends on the prior), so
// a run produces exactly n level-barrier checkpoints — the group-commit cadence unit.
func buildLinearDAG(t *testing.T, id string, n int) *DAG {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID(id)
	prev := "n0"
	b.AddStartNode(prev).WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput(prev, "done")
		return nil
	}))
	for i := 1; i < n; i++ {
		name := fmt.Sprintf("n%d", i)
		nm := name
		b.AddNode(name).WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.SetOutput(nm, "done")
			return nil
		})).DependsOn(prev)
		prev = name
	}
	dag, err := b.Build()
	require.NoError(t, err)
	return dag
}

// Test_ph61_Batched_ResumeEquivalent — a completed run under Batched(K) resumes to
// the SAME final state as Strict (the run-completion final Save is always durable, so
// a cleanly-completed Batched run is fully persisted). Reconstruction correctness.
func Test_ph61_Batched_ResumeEquivalent(t *testing.T) {
	run := func(mode DurabilityOption) map[string]NodeStatus {
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir, WithDurabilityMode(mode))
		require.NoError(t, err)
		dag := buildLinearDAG(t, "eq", 200)
		wf := &Workflow{DAG: dag, WorkflowID: "eq", Store: store}
		require.NoError(t, wf.Execute(context.Background()))
		loaded, err := store.Load("eq")
		require.NoError(t, err)
		out := map[string]NodeStatus{}
		loaded.ForEachNodeStatus(func(n string, s NodeStatus) { out[n] = s })
		return out
	}
	strict := run(Strict())
	batched := run(Batched(64))
	require.Equal(t, strict, batched, "Batched(64) completed run resumes to the same final state as Strict")
	require.Len(t, batched, 200, "all 200 nodes present")
}

// Test_ph61_Batched_CrashLossBoundedByK (bar #3) — simulate a power-loss mid-run
// under Batched(K): the on-disk file is a COMPLETE snapshot ≤K levels old, NEVER torn
// or empty, and resume re-runs the lost ≤K levels idempotently. We drive K+3 levels
// via checkpoints, then read the on-disk state WITHOUT the completion Save (mid-run
// crash), asserting the persisted frontier is a clean prefix within K of the last.
func Test_ph61_Batched_CrashLossBoundedByK(t *testing.T) {
	dir := t.TempDir()
	const k = 8
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(k)))
	require.NoError(t, err)

	// Drive raw checkpoints (mimicking the level barrier) WITHOUT a completion Save,
	// so we observe the mid-run durable frontier exactly.
	total := 20
	for lvl := 1; lvl <= total; lvl++ {
		d := NewWorkflowData("crash")
		for i := 1; i <= lvl; i++ {
			d.SetNodeStatus(fmt.Sprintf("n%d", i), Completed)
		}
		require.NoError(t, store.SaveCheckpoint(d))
	}

	// The on-disk file must exist, be a COMPLETE snapshot (parses), and its frontier
	// (highest completed node) must be within K of `total` — the ≤K crash-loss bound.
	raw, err := os.ReadFile(filepath.Join(dir, "crash.fb"))
	require.NoError(t, err)
	require.NotEmpty(t, raw, "on-disk snapshot is never empty/torn")

	loaded, err := store.Load("crash")
	require.NoError(t, err, "on-disk snapshot is a COMPLETE parseable file (torn-safe)")
	frontier := 0
	loaded.ForEachNodeStatus(func(n string, s NodeStatus) {
		if s == Completed {
			var idx int
			if _, err := fmt.Sscanf(n, "n%d", &idx); err == nil && idx > frontier {
				frontier = idx
			}
		}
	})
	// Last fsync boundary is the largest multiple of K ≤ total = 16; frontier == 16,
	// so total-frontier = 4 ≤ K. The bound: loss is ≤ K.
	require.LessOrEqual(t, total-frontier, k, "crash-loss ≤ K levels (never more)")
	require.Equal(t, (total/k)*k, frontier, "durable frontier is exactly the last K-boundary (16)")
}

// Test_ph61_Batched_SuspendFloorDurable (bar #4) — a park under Batched(K) MUST be
// fsync-durable immediately (the forced Sync), NOT deferred: after a WaitForSignal
// park, the on-disk state must already carry the Waiting frontier so a crash right
// after the park finds it on resume. Bite: without the forced Sync, the park's
// checkpoint would be deferred and lost.
func Test_ph61_Batched_SuspendFloorDurable(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(64)))
	require.NoError(t, err)

	// A few completed levels then a signal-wait park — all within one K-window (64),
	// so an unforced park checkpoint WOULD be deferred (count < 64). The floor must
	// force it durable anyway.
	b := NewWorkflowBuilder().WithWorkflowID("park")
	b.AddStartNode("a").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("a", "done")
		return nil
	}))
	b.AddWaitForSignal("waiter", "go").DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)

	wf := &Workflow{DAG: dag, WorkflowID: "park", Store: store}
	err = wf.Execute(context.Background())
	require.ErrorIs(t, err, ErrSuspended, "the run parks on the signal wait")

	// The park MUST be durable on disk NOW (forced Sync), even though we are far
	// inside the K=64 window. Load the persisted state and confirm the waiter is
	// durably Waiting — a crash here would find the park.
	loaded, err := store.Load("park")
	require.NoError(t, err, "the parked state is durably on disk (forced Sync under Batched)")
	st, ok := loaded.GetNodeStatus("waiter")
	require.True(t, ok, "waiter node persisted")
	require.Equal(t, Waiting, st, "the park is fsync-durable under Batched(64) — floor held")
}

// Test_ph61_F2_GetMetricsNonNilOnDisabledDefault (review F2 regression guard) — a
// default (metrics-disabled) Workflow's GetMetrics() returns a NON-NIL disabled
// collector after Execute, per the documented contract. The ph61 race-fix guard
// (retain only when MetricsConfig != nil) must NOT leave GetMetrics returning nil —
// a contract-following caller derefs it. Reddens if GetMetrics returns nil.
func Test_ph61_F2_GetMetricsNonNilOnDisabledDefault(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	dag := buildLinearDAG(t, "m", 3)
	wf := &Workflow{DAG: dag, WorkflowID: "m", Store: store} // MetricsConfig nil (disabled default)
	require.NoError(t, wf.Execute(context.Background()))

	mc := wf.GetMetrics()
	require.NotNil(t, mc, "GetMetrics returns a non-nil disabled collector on the default path (never nil)")
	require.False(t, mc.IsEnabled(), "the default collector is disabled")
	// The documented contract: stats read zero, and the caller can dereference safely.
	require.NotPanics(t, func() { _ = mc.GetAllOperationStats() }, "the disabled collector is safely dereferenceable")
}
