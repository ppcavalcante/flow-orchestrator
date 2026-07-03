package workflow

// M10 phase-35 chunk-1 — CONCURRENCY adversarial tests for the suspend spine.
//
// Run these under `-race`. They drive MANY suspendable nodes parking
// concurrently in the SAME level alongside completing, continue-on-error-failing,
// and hard-failing siblings at high MaxConcurrency, to attack the parkChan /
// failChan / cancel interplay in executeNodesInLevel and the clearParked reset on
// the fail-fast path. The author's suite only ever has a single parking node and a
// single sibling; these stress the buffered-channel drain and the concurrent
// SetNodeStatus path on WorkflowData under contention.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuspend_Adversarial_ConcurrentParksConverge: a level with many parking
// nodes, a completing sibling, and a continue-on-error sibling that FAILS. A park
// is not a failure and a continue-on-error failure is tolerated, so the level has
// no fail-fast failure → the run suspends. On wake the run converges: every parker
// completes exactly once, the completing sibling and the coe-failed sibling are
// persisted terminal and are NOT re-run on resume.
func TestSuspend_Adversarial_ConcurrentParksConverge(t *testing.T) {
	const nParkers = 12
	const id = "concurrent-parks"
	store := NewInMemoryStore()

	gates := make([]*parkGate, nParkers)
	for i := range gates {
		gates[i] = newParkGate()
	}
	counter := newExecCounter()
	coeRuns := newExecCounter()

	build := func() *DAG {
		d := NewDAG(id)
		d.config.MaxConcurrency = 16 // high concurrency vs the 12+2 nodes in level 0
		for i := 0; i < nParkers; i++ {
			mustAddNode(t, d, completingSuspendNode(fmt.Sprintf("park%d", i), gates[i], counter))
		}
		mustAddNode(t, d, countingNode("sib", counter))
		coe := NewNode("coe", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			coeRuns.inc("coe")
			return errors.New("coe boom")
		}))
		coe.ContinueOnError = true
		mustAddNode(t, d, coe)
		return d
	}

	// Phase 1: concurrent parks; the run suspends.
	w1 := &Workflow{DAG: build(), WorkflowID: id, Store: store}
	err := w1.Execute(context.Background())
	require.ErrorIs(t, err, ErrSuspended, "concurrent parks (no fail-fast) suspend the run")
	var execErr *ExecutionError
	require.False(t, errors.As(err, &execErr), "a tolerated coe failure must not make a park an *ExecutionError")

	p1, lerr := store.Load(id)
	require.NoError(t, lerr)
	for i := 0; i < nParkers; i++ {
		assertStatus(t, p1, fmt.Sprintf("park%d", i), Waiting)
	}
	assertStatus(t, p1, "sib", Completed)
	assertStatus(t, p1, "coe", Failed)
	assert.Equal(t, 1, coeRuns.get("coe"), "coe ran once in phase 1")

	// Phase 2: wake every parker; converge.
	for _, g := range gates {
		g.wake()
	}
	w2 := &Workflow{DAG: build(), WorkflowID: id, Store: store}
	require.NoError(t, w2.Execute(context.Background()))

	p2, lerr := store.Load(id)
	require.NoError(t, lerr)
	for i := 0; i < nParkers; i++ {
		name := fmt.Sprintf("park%d", i)
		assertStatus(t, p2, name, Completed)
		assert.Equalf(t, 1, counter.get(name), "%s completes exactly once after wake", name)
	}
	assertStatus(t, p2, "sib", Completed)
	assertStatus(t, p2, "coe", Failed)
	assert.Equal(t, 1, counter.get("sib"), "the completing sibling must not re-run on resume")
	assert.Equal(t, 1, coeRuns.get("coe"), "a persisted-Failed coe sibling must not re-run on resume")
}

// TestSuspend_Adversarial_ConcurrentParkWithFailFast: many parking nodes share a
// level with a hard (non-continue-on-error) failing node. Fail-fast OUTRANKS park:
// the run returns an *ExecutionError (never ErrSuspended) and every concurrently
// parked node must be cleared off Waiting (clearParked) so the failed snapshot
// carries no stray suspended frontier — at scale, under -race.
func TestSuspend_Adversarial_ConcurrentParkWithFailFast(t *testing.T) {
	const nParkers = 12
	const id = "concurrent-failfast"
	store := NewInMemoryStore()

	gates := make([]*parkGate, nParkers)
	for i := range gates {
		gates[i] = newParkGate()
	}

	d := NewDAG(id)
	d.config.MaxConcurrency = 16
	for i := 0; i < nParkers; i++ {
		mustAddNode(t, d, newSuspendingNode(fmt.Sprintf("park%d", i), gates[i]))
	}
	mustAddNode(t, d, NewNode("boom", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("hard boom")
	})))

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(context.Background())

	var execErr *ExecutionError
	require.True(t, errors.As(err, &execErr), "a hard fail-fast outranks concurrent parks")
	require.False(t, errors.Is(err, ErrSuspended), "a failing run must not report as suspended")

	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	assertStatus(t, persisted, "boom", Failed)
	for i := 0; i < nParkers; i++ {
		name := fmt.Sprintf("park%d", i)
		got, _ := persisted.GetNodeStatus(name)
		assert.NotEqualf(t, Waiting, got,
			"%s must be cleared off Waiting in a failed snapshot (clearParked); got %q", name, got)
	}
}
