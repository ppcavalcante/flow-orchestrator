package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parkedParent builds a parent with a parked sub-workflow-await node "sub" (declaring the child's
// "result" data key → parent key "result") + a downstream "after" node. The child is def-value.
func parkedParent(t *testing.T, store WorkflowStore, wf string, childDAG *DAG, afterCounter *atomic.Int32) *Workflow {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID(wf)
	pb.AddSubWorkflowParked("sub", childDAG).WithResult("result", "result")
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(afterCounter))
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = wf
	w.DAG = dag
	return w
}

// runChildOutOfBand simulates the ph94 producer: runs childDAG under the parent's deterministic
// child ID on the same store, so the parent's parked node can later observe it terminal.
func runChildOutOfBand(t *testing.T, store WorkflowStore, parentWF, nodeName string, childDAG *DAG) {
	t.Helper()
	childID := subWorkflowChildID(parentWF, nodeName)
	child := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	require.NoError(t, child.Execute(context.Background()))
}

// TestParkedSubWorkflow_ParkThenWake — hard-bar core, per-store (InMemory + FB): the parent parks
// while the child is not terminal; running the child out-of-band + delivering the completion
// signal + DeliverAndResume wakes it, and it reads the child's result data key downstream.
func TestParkedSubWorkflow_ParkThenWake(t *testing.T) {
	for _, sc := range []struct {
		name string
		mk   func() WorkflowStore
	}{
		{"InMemory", func() WorkflowStore { return NewInMemoryStore() }},
		{"FlatBuffers", func() WorkflowStore { s, e := NewFlatBuffersStore(t.TempDir()); require.NoError(t, e); return s }},
	} {
		t.Run(sc.name, func(t *testing.T) {
			store := sc.mk()
			var afterN atomic.Int32
			w := parkedParent(t, store, "wf-park", childProducing(t, "child-result", nil), &afterN)

			// First drive: child not yet run → parked (Waiting), downstream not run.
			require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended, "child not terminal → park")
			parked, err := store.Load("wf-park")
			require.NoError(t, err)
			assertNodeStatus(t, parked, "sub", Waiting)
			require.EqualValues(t, 0, afterN.Load())

			// Run the child out-of-band, then deliver the completion signal + resume.
			runChildOutOfBand(t, store, "wf-park", "sub", childProducing(t, "child-result", nil))
			require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")))

			final, err := store.Load("wf-park")
			require.NoError(t, err)
			assertNodeStatus(t, final, "sub", Completed)
			assertNodeStatus(t, final, "after", Completed)
			require.EqualValues(t, 1, afterN.Load())
			result, ok := final.Get("result")
			require.True(t, ok, "the child result data key is read into the parent key on wake")
			assert.Equal(t, "child-result", result)
		})
	}
}

// TestParkedSubWorkflow_WakeSeedBreak_NoSignalNeverWakes — WAKE is load-bearing: even with the
// child terminal, WITHOUT a completion-signal-driven re-drive the parent stays Waiting. A
// timeout-bounded probe confirms no spontaneous completion (there is no scheduler).
func TestParkedSubWorkflow_WakeSeedBreak_NoSignalNeverWakes(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-nowake", childProducing(t, "v", nil), &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	// Child completes out-of-band, but NO completion signal / DeliverAndResume is issued.
	runChildOutOfBand(t, store, "wf-nowake", "sub", childProducing(t, "v", nil))

	// Without a host re-drive the parent never wakes. (We do NOT call Execute again — that IS the
	// point: the wake is host-driven-via-signal.) Assert it is still Waiting after a brief wait.
	time.Sleep(50 * time.Millisecond)
	parked, err := store.Load("wf-nowake")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sub", Waiting)
	require.EqualValues(t, 0, afterN.Load(), "downstream never runs without the wake")
}

// TestParkedSubWorkflow_NoSignalStore_LoudFail — a parked node driven with no SignalStore fails
// loudly (ErrWaitRequiresSignalStore), never a silent forever-park.
func TestParkedSubWorkflow_NoSignalStore_LoudFail(t *testing.T) {
	// Drive the action directly with no SignalStore injected (bare DAG.Execute).
	d := NewDAG("wf-nostore")
	require.NoError(t, d.AddNode(NewNode("sub", &parkedSubWorkflowAction{nodeName: "sub", child: childProducing(t, "v", nil)})))
	cp := func(*WorkflowData) error { return nil }
	err := d.Execute(withCheckpoint(context.Background(), cp), NewWorkflowData("wf-nostore"))
	require.ErrorIs(t, err, ErrWaitRequiresSignalStore, "no SignalStore → loud failure, never a forever-park")
}

// TestParkedSubWorkflow_ParkSeedBreak_NonTerminalChildParks — the park seed-break for the
// NON-TERMINAL branch (distinct from the not-found branch): a child that EXISTS but is mid-run
// (a node still Pending) must make the parent park. Seeding a non-terminal child journal + driving
// → parked. Mutating the non-terminal park (return nil instead of ErrSuspended) makes the parent
// COMPLETE with the wrong/absent result — proving the park is what forces the wait.
func TestParkedSubWorkflow_ParkSeedBreak_NonTerminalChildParks(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	// A 2-node child; we seed it PARTIALLY run (node "produce" Completed, a second node Pending)
	// so childTerminal is false.
	cb := NewWorkflowBuilder()
	cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", "v")
		return nil
	}))
	cb.AddNode("tail").DependsOn("produce").WithAction(choiceNoop())
	childDAG, err := cb.Build()
	require.NoError(t, err)

	w := parkedParent(t, store, "wf-parkseed", childDAG, &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Seed a NON-TERMINAL child journal (produce Completed, tail Pending) under the child ID.
	childID := subWorkflowChildID("wf-parkseed", "sub")
	cd := NewWorkflowData(childID)
	cd.Set("result", "v")
	cd.SetNodeStatus("produce", Completed)
	cd.SetNodeStatus("tail", Pending) // non-terminal → childTerminal false
	require.NoError(t, store.Save(cd))

	// Re-drive: the child exists but is non-terminal → the parent must re-park (NOT complete).
	require.ErrorIs(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")), ErrSuspended,
		"a non-terminal child → the parent re-parks, does not complete")
	reparked, err := store.Load("wf-parkseed")
	require.NoError(t, err)
	assertNodeStatus(t, reparked, "sub", Waiting)
	require.EqualValues(t, 0, afterN.Load(), "downstream never runs while the child is non-terminal")
}
