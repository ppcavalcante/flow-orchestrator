package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// childProducing builds a definition-value child DAG whose single node "produce" sets
// output outVal (so a parent can declare it as the result via WithResult). spawnCounter,
// when non-nil, is incremented once per child action invocation (to assert idempotent spawn).
func childProducing(t *testing.T, outVal any, spawnCounter *atomic.Int32) *DAG {
	t.Helper()
	cb := NewWorkflowBuilder()
	cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		if spawnCounter != nil {
			spawnCounter.Add(1)
		}
		d.Set("result", outVal) // a child DATA key (value_long-faithful result path)
		return nil
	}))
	dag, err := cb.Build()
	require.NoError(t, err)
	return dag
}

// parentWithSub builds a parent that spawns childDAG at node "sub" and declares the child's
// "result" data key into parent key "result", with a downstream "after" node.
func parentWithSub(t *testing.T, store WorkflowStore, wf string, childDAG *DAG, afterCounter *atomic.Int32) *Workflow {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID(wf)
	pb.AddSubWorkflow("sub", childDAG).WithResult("result", "result")
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(afterCounter))
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = wf
	w.DAG = dag
	return w
}

// TestSubWorkflow_InlineSpawnAwaitResult — hard-bar core: a parent spawns a def-value child
// inline, the child runs under its own deterministic ID (distinct journal), and the parent
// reads the child's declared result downstream.
func TestSubWorkflow_InlineSpawnAwaitResult(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parentWithSub(t, store, "wf-parent", childProducing(t, "child-output", nil), &afterN)

	require.NoError(t, w.Execute(context.Background()))

	final, err := store.Load("wf-parent")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())

	// The declared result key holds the child's output in parent data.
	result, ok := final.Get("result")
	require.True(t, ok, "the child result is populated into the declared parent key")
	assert.Equal(t, "child-output", result)

	// The child persisted under its OWN deterministic ID (distinct journal).
	childID := subWorkflowChildID("wf-parent", "sub")
	childData, err := store.Load(childID)
	require.NoError(t, err, "the child persisted under a distinct workflow ID")
	assertNodeStatus(t, childData, "produce", Completed)
}

// TestSubWorkflow_IdempotentSpawn_ParentReDrive — the WHOLE-run re-drive case. After the
// parent completes, re-driving does not re-execute the child. NOTE: this passes primarily on
// PARENT-node-level idempotency (dag.go leaves the Completed "sub" node as-is), NOT on the
// PIN-2 in-action guard — so it is NOT the seed-break for PIN-2. The crash-mid-child test
// below is the one that isolates and bites PIN-2.
func TestSubWorkflow_IdempotentSpawn_ParentReDrive(t *testing.T) {
	store := NewInMemoryStore()
	var spawnN, afterN atomic.Int32
	w := parentWithSub(t, store, "wf-idem", childProducing(t, "v", &spawnN), &afterN)

	require.NoError(t, w.Execute(context.Background()))
	require.EqualValues(t, 1, spawnN.Load(), "child ran once")

	require.NoError(t, w.Execute(context.Background()))
	require.EqualValues(t, 1, spawnN.Load(), "no re-spawn on a completed-parent re-drive")
}

// TestSubWorkflow_IdempotentSpawn_CrashMidChild — seed-break #1 (the crux durability claim).
// Simulates a crash AFTER the child fully ran (child journal Completed) but BEFORE the parent
// could checkpoint its "sub" node terminal (parent "sub" still Pending). On re-drive, dag.go
// re-invokes the still-Pending "sub" node → subWorkflowAction runs AGAIN and re-drives the
// SAME child (by its DETERMINISTIC ID). The child must NOT re-execute (spawn-count stays 1).
//
// SEED-BREAK: the load-bearing guard is the DETERMINISTIC child ID (+ the child's own
// resume-idempotency: child.Execute on a terminal child journal re-runs nothing). Make the
// child ID non-deterministic (a fresh ID each spawn) → the re-drive spawns a NEW child →
// spawn-count 2 → this reddens. (Verified: mutating subWorkflowChildID to append a timestamp
// breaks exactly this assertion.) The PIN-2 terminal-fast-path is an optimization, not the
// guarantee — removing it leaves the count at 1 because child.Execute is itself idempotent.
func TestSubWorkflow_IdempotentSpawn_CrashMidChild(t *testing.T) {
	store := NewInMemoryStore()
	var spawnN, afterN atomic.Int32
	childDAG := childProducing(t, "v", &spawnN)
	w := parentWithSub(t, store, "wf-crash", childDAG, &afterN)

	// Pre-seed a COMPLETED child journal under the deterministic child ID (as if a prior
	// run's child finished), WITHOUT the parent having recorded "sub" terminal.
	childID := subWorkflowChildID("wf-crash", "sub")
	seeded := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	require.NoError(t, seeded.Execute(context.Background()))
	require.EqualValues(t, 1, spawnN.Load(), "the pre-crash child ran once")

	// The parent's own journal does NOT exist yet (crash before any parent checkpoint), so
	// the parent's "sub" node is Pending on this drive → it WILL re-invoke subWorkflowAction.
	require.NoError(t, w.Execute(context.Background()))

	// PIN-2: the action found the child terminal-Completed and did NOT re-drive it.
	require.EqualValues(t, 1, spawnN.Load(), "PIN-2: crash-mid-child re-drive must NOT re-spawn the child")

	final, err := store.Load("wf-crash")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "v", result)
}

// TestSubWorkflow_RequiresStore — PIN-1 honesty: a sub-workflow node driven with no parent
// Store in scope (bare DAG.Execute, no injection) fails loudly, never a silent spawn.
func TestSubWorkflow_RequiresStore(t *testing.T) {
	child := childProducing(t, "x", nil)
	d := NewDAG("wf-nostore")
	require.NoError(t, d.AddNode(NewNode("sub", &subWorkflowAction{nodeName: "sub", child: child})))
	// Drive DAG.Execute directly with no withParentStore injection.
	err := d.Execute(context.Background(), NewWorkflowData("wf-nostore"))
	require.ErrorIs(t, err, ErrSubWorkflowRequiresStore, "no parent store → loud failure, never a silent spawn")
}

// TestSubWorkflow_ChildFail_ParentTerminalNoOp — INV-01 inheritance (the ph90 contract at
// the composition level): a child that fails → parent "sub" node Failed, downstream never
// runs, and a re-drive is a no-op (terminal run).
func TestSubWorkflow_ChildFail_ParentTerminalNoOp(t *testing.T) {
	store := NewInMemoryStore()
	// A child whose node fails.
	cb := NewWorkflowBuilder()
	cb.AddStartNode("boom").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("child boom")
	}))
	childDAG, err := cb.Build()
	require.NoError(t, err)

	var afterN atomic.Int32
	w := parentWithSub(t, store, "wf-childfail", childDAG, &afterN)

	err1 := w.Execute(context.Background())
	require.Error(t, err1, "child failure fails the parent run (INV-01 fail-fast)")

	final, lerr := store.Load("wf-childfail")
	require.NoError(t, lerr)
	assertNodeStatus(t, final, "sub", Failed)
	require.EqualValues(t, 0, afterN.Load(), "downstream never runs after a child-fail")

	// Re-drive: terminal-failed run → no-op, node stays Failed (the ph90 terminal contract).
	err2 := w.Execute(context.Background())
	require.NoError(t, err2, "re-driving a terminal-failed run is a no-op")
	final2, _ := store.Load("wf-childfail") //nolint:errcheck // asserted below
	assertNodeStatus(t, final2, "sub", Failed)
	require.EqualValues(t, 0, afterN.Load())
}

// TestSubWorkflow_CoeChild_CrashResumeConsistent — FIND-P91-R1 regression. A child with a
// ContinueOnError node that FAILS leaves that node status Failed, yet DAG.Execute returns
// SUCCESS (the coe contract). The terminal fast-path must NOT route such a child as a parent
// failure on the crash-resume window — it must defer to child.Execute (the coe authority) so
// the crash path and the non-crash path AGREE: the parent COMPLETES.
func TestSubWorkflow_CoeChild_CrashResumeConsistent(t *testing.T) {
	store := NewInMemoryStore()
	// A child: a coe node that fails, plus a node that sets the result data key.
	buildCoeChild := func() *DAG {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("coe-fail").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			return errors.New("tolerated failure")
		})).WithContinueOnError()
		cb.AddNode("produce").DependsOn("coe-fail").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", "ok-despite-coe")
			return nil
		}))
		dag, err := cb.Build()
		require.NoError(t, err)
		return dag
	}

	var afterN atomic.Int32
	w := parentWithSub(t, store, "wf-coe", buildCoeChild(), &afterN)

	// Non-crash path: the coe child succeeds overall → parent completes.
	require.NoError(t, w.Execute(context.Background()), "a coe child that tolerates a failure succeeds → parent completes")
	final, err := store.Load("wf-coe")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	require.EqualValues(t, 1, afterN.Load())

	// Crash-resume path: pre-seed the coe child's terminal journal (coe-fail Failed, produce
	// Completed) under the deterministic ID, with a FRESH parent whose "sub" is Pending, and
	// re-drive. The fast-path must NOT mistake the Failed coe node for a child failure.
	store2 := NewInMemoryStore()
	childID := subWorkflowChildID("wf-coe2", "sub")
	seeded := &Workflow{DAG: buildCoeChild(), WorkflowID: childID, Store: store2}
	require.NoError(t, seeded.Execute(context.Background()), "the pre-crash coe child succeeded")

	var afterN2 atomic.Int32
	w2 := parentWithSub(t, store2, "wf-coe2", buildCoeChild(), &afterN2)
	require.NoError(t, w2.Execute(context.Background()), "FIND-P91-R1: crash-resume must not route a coe child as a failure")
	final2, err := store2.Load("wf-coe2")
	require.NoError(t, err)
	assertNodeStatus(t, final2, "sub", Completed)
	require.EqualValues(t, 1, afterN2.Load(), "downstream runs — the coe child is a success on BOTH paths")
}
