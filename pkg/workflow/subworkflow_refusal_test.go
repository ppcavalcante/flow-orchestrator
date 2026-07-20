package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// childWithSuspendable builds a child DAG with a top-level suspendable node (an approval),
// so an inline spawn of it must be refused at build.
func childWithSuspendable(t *testing.T) *DAG {
	t.Helper()
	cb := NewWorkflowBuilder()
	cb.AddApproval("gate") // a suspendable node
	dag, err := cb.Build()
	require.NoError(t, err)
	return dag
}

// TestSubWorkflow_DirectSuspendableChild_Refused — seed-break #2: a child with a top-level
// suspendable node on the inline path is refused loudly AT BUILD (an inline child blocks the
// parent, so it can never park), never a hang.
func TestSubWorkflow_DirectSuspendableChild_Refused(t *testing.T) {
	pb := NewWorkflowBuilder().WithWorkflowID("wf-direct-susp")
	pb.AddSubWorkflow("sub", childWithSuspendable(t))
	_, err := pb.Build()
	require.Error(t, err, "an inline child with a suspendable node must fail Build")
	require.ErrorIs(t, err, ErrSubWorkflowSuspendableChild)
}

// TestSubWorkflow_TransitiveSuspendableGrandchild_Refused — seed-break #3 (the recursion
// bite): a non-suspendable child that itself sub-workflows a suspendable GRANDCHILD must be
// refused when the PARENT declares it. This proves the parent's AddSubWorkflow closure-scan
// RECURSES into a nested sub-workflow's own child, not just the shallow top level.
//
// The child is assembled via NewDAG+AddNode (NOT the builder), so its nested
// subWorkflowAction is NOT pre-scanned — the ONLY thing that can catch the deep suspendable
// is the parent's recursive scan. That makes the recursion load-bearing: a shallow scan (see
// the bite in the SUMMARY) sees the child's node as a non-suspendable *subWorkflowAction and
// passes, while the recursive scan descends into its .child and finds the suspendable
// grandchild. Bite: make scanChildInlineSafe shallow → this test STOPS reddening.
func TestSubWorkflow_TransitiveSuspendableGrandchild_Refused(t *testing.T) {
	// grandchild: has a suspendable node.
	grandDAG := childWithSuspendable(t)

	// child: assembled RAW (NewDAG+AddNode, bypassing the builder's per-AddSubWorkflow scan)
	// so it carries an UN-scanned nested sub-workflow pointing at the suspendable grandchild.
	// The child's own nodes are non-suspendable — a shallow scan of it passes.
	childDAG := NewDAG("raw-child")
	require.NoError(t, childDAG.AddNode(NewNode("spawn-grand", &subWorkflowAction{nodeName: "spawn-grand", child: grandDAG})))

	// The PARENT declares the child inline — its recursive closure-scan must descend through
	// the child's non-suspendable subWorkflowAction into the suspendable grandchild and refuse.
	pb := NewWorkflowBuilder().WithWorkflowID("wf-transitive")
	pb.AddSubWorkflow("sub", childDAG)
	_, err := pb.Build()
	require.Error(t, err, "the parent must refuse a child that transitively contains a suspendable grandchild")
	require.ErrorIs(t, err, ErrSubWorkflowSuspendableChild)
}

// countingNoSuspendChild builds a plain non-suspendable child (used by the cycle test).
func countingNoSuspendChild(t *testing.T, counter *atomic.Int32) *DAG {
	t.Helper()
	return childProducing(t, "v", counter)
}

// TestSubWorkflow_SameAncestorID_RefusedBeforeLease — seed-break #4: if a child's
// deterministic ID equals an ID already on the drive stack (an ancestor), the spawn is
// refused loudly (ErrSubWorkflowCycle) BEFORE child.Execute acquires the non-reentrant
// per-ID lease. Without the guard the drive self-deadlocks (re-locking the ancestor's held
// mutex). We prove the guard fires fast AND that the unguarded path would hang, via a
// timeout-bounded probe.
//
// We construct the collision directly: drive subWorkflowAction with a ctx whose drive-stack
// already contains the child ID it will compute (simulating an ancestor with that ID).
func TestSubWorkflow_SameAncestorID_RefusedBeforeLease(t *testing.T) {
	store := NewInMemoryStore()
	var spawnN atomic.Int32
	action := &subWorkflowAction{nodeName: "sub", child: countingNoSuspendChild(t, &spawnN)}
	parentData := NewWorkflowData("wf-cycle")

	// The ID this action WILL compute for its child.
	childID := subWorkflowChildID("wf-cycle", "sub")
	// Simulate an ancestor already driving that exact ID: seed the drive-stack with it.
	ctx := withParentStore(withDriveID(context.Background(), childID), store)

	// The guard must refuse — fast — with ErrSubWorkflowCycle, and NOT run the child.
	done := make(chan error, 1)
	go func() { done <- action.Execute(ctx, parentData) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrSubWorkflowCycle, "a child ID == an ancestor drive-stack ID is refused")
		require.EqualValues(t, 0, spawnN.Load(), "the child is NOT run when the cycle is refused")
	case <-time.After(3 * time.Second):
		t.Fatal("cycle guard did not fire — the spawn hung (the guard is what prevents the self-deadlock)")
	}
}

// TestSubWorkflow_SameAncestorID_UnguardedHangs — the OTHER half of seed-break #4: prove the
// guard is load-bearing by showing that WITHOUT it, the same scenario DEADLOCKS. We can't
// remove the guard from source in a test, so we reproduce the raw hazard directly: acquire a
// workflow ID's drive lease, then (from the same goroutine) try to acquire it AGAIN — the
// non-reentrant per-ID mutex blocks forever. A timeout-bounded probe asserts it does NOT
// complete (i.e. the guard the real code has is exactly what averts this).
func TestSubWorkflow_SameAncestorID_UnguardedHangs(t *testing.T) {
	locker := NewInProcessLocker()
	release, err := locker.Acquire(context.Background(), "same-id")
	require.NoError(t, err)
	defer release()

	reacquired := make(chan struct{})
	go func() {
		r2, _ := locker.Acquire(context.Background(), "same-id") //nolint:errcheck // this line BLOCKS forever (non-reentrant) — that's the point
		r2()
		close(reacquired)
	}()
	select {
	case <-reacquired:
		t.Fatal("re-acquiring the same per-ID lease returned — it should self-deadlock (proving the guard is needed)")
	case <-time.After(500 * time.Millisecond):
		// Expected: the second Acquire of the same ID blocks → the cycle guard in
		// subWorkflowAction (tested above) is what averts exactly this deadlock.
	}
}
