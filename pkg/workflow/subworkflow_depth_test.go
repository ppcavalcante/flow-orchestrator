package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// M19 ph95 — Slice A: the inline nesting-DoS ceiling. A def-value sub-workflow chain nesting past
// the ceiling is refused with ErrSubWorkflowMaxDepth (loud, not a park, not a silent cap).

// nestedInlineDAG builds a chain of `depth` inline sub-workflow nodes: the top DAG has a sub-workflow
// node whose child has a sub-workflow node whose child ... `depth` levels, the innermost a plain leaf.
// Executing the top drives depth nested child.Execute calls, each pushing one drive-stack ID.
func nestedInlineDAG(t *testing.T, depth int) *DAG {
	t.Helper()
	// innermost: a plain leaf DAG (no sub-workflow).
	cur := func() *DAG {
		b := NewWorkflowBuilder()
		b.AddStartNode("leaf").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		d, err := b.Build()
		require.NoError(t, err)
		return d
	}()
	for range depth {
		b := NewWorkflowBuilder()
		b.AddSubWorkflow("sub", cur)
		d, err := b.Build()
		require.NoError(t, err)
		cur = d
	}
	return cur
}

// runNestedInline drives a `depth`-deep inline chain with the given ceiling (0 = default 8) and
// returns the Execute error. Uses an InMemoryStore (inline path needs a parent store).
func runNestedInline(t *testing.T, depth, ceiling int) error {
	t.Helper()
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "root"
	w.DAG = nestedInlineDAG(t, depth)
	w.MaxSubWorkflowDepth = ceiling
	return w.Execute(context.Background())
}

// TestSubWorkflowDepth_InlineChainPastCeiling_Refused (test 1, the hard bar) — a chain deeper than the
// default ceiling (8) is refused with ErrSubWorkflowMaxDepth; a chain within the ceiling succeeds.
// SEED-BREAK: remove the depthExceeded check in subWorkflowAction.Execute → the deep chain no longer
// errors (it recurses all the way) → this reddens.
func TestSubWorkflowDepth_InlineChainPastCeiling_Refused(t *testing.T) {
	// depth 7 (ancestors 0..6 at the deepest spawn) is within ceiling 8 → success.
	require.NoError(t, runNestedInline(t, 7, 0), "a chain within the ceiling completes")

	// depth 9 breaches ceiling 8 → the 9th spawn (at depth 8) is refused, loud.
	err := runNestedInline(t, 9, 0)
	require.ErrorIs(t, err, ErrSubWorkflowMaxDepth, "a chain past the ceiling is refused with the typed error")
}

// TestSubWorkflowDepth_OverridableCeiling (test 3) — Workflow.MaxSubWorkflowDepth overrides the default:
// ceiling 2 refuses at depth 2; ceiling 0 (=default 8) admits depth 7.
func TestSubWorkflowDepth_OverridableCeiling(t *testing.T) {
	// ceiling 2: ancestors 0,1 allowed; the spawn at depth 2 is refused.
	require.ErrorIs(t, runNestedInline(t, 3, 2), ErrSubWorkflowMaxDepth, "a low override refuses sooner")
	require.NoError(t, runNestedInline(t, 2, 2), "depth within the low override completes")

	// ceiling 0 normalizes to the default 8 → depth 7 completes.
	require.NoError(t, runNestedInline(t, 7, 0), "0 => default ceiling 8 admits depth 7")
}

// TestSubWorkflowDepth_QueueTypeRefChainPastCeiling_Bounded (test 2, THE ⭐ hard bar — F-P94-04) — a
// TYPE-REF chain (each registered type enqueues the next) is BOUNDED by the ceiling across the dispatch,
// even though each child runs in a fresh worker (RunNext) whose ctx does NOT inherit the parent's
// drive-stack. Depth is carried via the work_queue.depth column + re-seeded in RunNext. SEED-BREAK:
// make RunNext seed depth 0 (drop withDepthSeed) → each worker sees depth 0 → the chain nests unbounded
// (the ceiling never fires; a t{ceiling+1} row gets enqueued). Here we assert it STOPS: the spawn at the
// ceiling is refused, so no deeper child is ever enqueued.
func TestSubWorkflowDepth_QueueTypeRefChainPastCeiling_Bounded(t *testing.T) {
	// The ceiling is a global policy (the default), enforced by every RunNext-built worker — RunNext builds
	// a bare Workflow, so the DEFAULT ceiling is what bounds the queue chain (a per-workflow override does
	// not cross the dispatch; only the DEPTH is carried). Drive the chain against the default.
	const ceiling = defaultMaxSubWorkflowDepth
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "chain.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // cleanup

	// Register types tier0..tier(ceiling+2): each tier's DAG is a single queued sub-workflow node that
	// spawns the NEXT tier. A self-perpetuating type-ref chain — the depth guard is the only thing that
	// stops it. The MaxSubWorkflowDepth override is carried on the Workflow the worker builds via RunNext...
	// but RunNext builds a bare Workflow, so the ceiling must ride the DEFAULT or be injected. We drive the
	// chain with a low DEFAULT by overriding on the ROOT only and relying on the carried depth; to make the
	// ceiling itself low without touching the default, register a leaf at tier(ceiling) so the chain has a
	// natural end IF the guard fails — then assert the guard stopped it BEFORE that leaf.
	reg := NewRegistry()
	maxTier := ceiling + 2
	for i := 0; i < maxTier; i++ {
		next := fmt.Sprintf("tier%d", i+1)
		typeName := fmt.Sprintf("tier%d", i)
		require.NoError(t, reg.Register(typeName, func() (*DAG, error) {
			b := NewWorkflowBuilder()
			b.AddSubWorkflowQueued("sub", next)
			return b.Build()
		}))
	}
	// a terminal leaf tier so a runaway chain would eventually resolve (proving the STOP is the guard, not
	// exhaustion of registered types).
	require.NoError(t, reg.Register(fmt.Sprintf("tier%d", maxTier), func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddStartNode("leaf").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		return b.Build()
	}))

	// Kick off the chain: enqueue tier1 at depth 1 as if a depth-0 root spawned it. Then drive workers in a
	// loop; each RunNext runs one queued item (which enqueues the next tier at depth+1, or refuses at the
	// ceiling). Bounded loop so a BUG (unbounded nesting) fails the test via the iteration cap, not a hang.
	_, err = store.EnqueueSubWorkflow("tier1-child", "tier1", nil, "root", "subworkflow-complete:sub", 1)
	require.NoError(t, err)

	// Drive up to a generous cap. With the ceiling firing, the chain terminates well before the cap. A
	// worker whose child spawn is refused at the ceiling returns that ErrSubWorkflowMaxDepth as execErr
	// (RunNext dead-letters the row) — that is the EXPECTED chain-terminator, not a test failure; we assert
	// it fired at least once. Any OTHER error is a real fault.
	deepestEnqueued := 1
	ceilingFired := false
	for iter := 0; iter < 60; iter++ {
		ran, rerr := RunNext(context.Background(), store, reg, "worker")
		if rerr != nil {
			require.ErrorIs(t, rerr, ErrSubWorkflowMaxDepth, "the only expected drive error is the depth-ceiling refusal")
			ceilingFired = true
		}
		if !ran {
			break // no more claimable work → the chain has terminated
		}
		// track the deepest tier that ever got a queue row (proof the chain didn't exceed the ceiling)
		for d := deepestEnqueued + 1; d <= maxTier+1; d++ {
			var n int
			require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE type=?`, fmt.Sprintf("tier%d", d)).Scan(&n))
			if n > 0 {
				deepestEnqueued = d
			}
		}
	}
	require.True(t, ceilingFired, "the depth ceiling refused a spawn (the chain was actively stopped, not exhausted)")

	// THE BITE: the ceiling stopped the chain. A spawn is refused when depth >= ceiling, so the deepest
	// child EVER enqueued is at depth == ceiling (the tier whose parent was at depth ceiling-1 and spawned
	// it; the tier AT depth ceiling then refuses to spawn its own child). No tier beyond `ceiling` exists.
	require.LessOrEqual(t, deepestEnqueued, ceiling,
		"F-P94-04: the carried depth bounds the type-ref chain at the ceiling; no deeper child is enqueued (drop withDepthSeed -> unbounded -> this fails)")
	require.Greater(t, deepestEnqueued, 1, "the chain DID advance (not a vacuous stop at tier1)")
}

// TestSubWorkflowDepth_CorruptDepthRow_SkippedNotStorm (F-P95-01 + F-P95-03) — a forged/bit-rotted
// work_queue row with a depth beyond maxSubWorkflowDepthCap is fail-safe SKIPPED by ClaimNext (never
// claimed, never fed to withDepthSeed as a giant seed → no allocation storm), and it does NOT wedge the
// queue: a healthy row behind it is still claimable. SEED-BREAK (the cap): drop the `> cap` clause →
// ClaimNext would return the huge-depth row and RunNext's withDepthSeed would allocate ~depth entries.
func TestSubWorkflowDepth_CorruptDepthRow_SkippedNotStorm(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "corrupt.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // cleanup

	// A healthy plain row + a forged huge-depth row. Enqueue the healthy one FIRST (older) so ORDER BY
	// enqueued_at would offer it first anyway; then forge the corrupt row and also a healthy newer one to
	// prove the queue keeps flowing past the corrupt entry.
	_, err = store.Enqueue("healthy-old", "T", nil)
	require.NoError(t, err)
	// forge a corrupt row directly (depth beyond the cap — an engine path can never write this).
	_, err = store.db.Exec(`INSERT INTO work_queue(workflow_id,type,input,enqueued_at,state,attempts,updated_at,depth)
	                        VALUES('corrupt', 'T', NULL, 2, 'pending', 0, 2, ?)`, maxSubWorkflowDepthCap+1000000)
	require.NoError(t, err)
	_, err = store.db.Exec(`INSERT INTO work_queue(workflow_id,type,input,enqueued_at,state,attempts,updated_at,depth)
	                        VALUES('healthy-new', 'T', NULL, 3, 'pending', 0, 3, 0)`)
	require.NoError(t, err)

	// Claim repeatedly: the two healthy rows are claimable; the corrupt row is SKIPPED (never returned).
	claimed := map[string]bool{}
	for i := 0; i < 3; i++ {
		it, cerr := store.ClaimNext("w", "T")
		if errors.Is(cerr, ErrNoWork) {
			break
		}
		require.NoError(t, cerr)
		claimed[it.WorkflowID] = true
	}
	require.True(t, claimed["healthy-old"], "the healthy older row was claimed")
	require.True(t, claimed["healthy-new"], "the healthy newer row was claimed — the corrupt row did NOT wedge the queue")
	require.False(t, claimed["corrupt"], "the forged huge-depth row was fail-safe SKIPPED, never claimed (no allocation storm)")
}
