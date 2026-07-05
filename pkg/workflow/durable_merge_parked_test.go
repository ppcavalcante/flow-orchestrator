package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 43 §3 — INV-03 clause (c): a MergeNode whose TAKEN branch-tail is Waiting
// (parked on a signal) is left Pending (not fired, not Bypassed) until the tail
// wakes. This is classifyBlockedStatus's assign=false arm: a Waiting dep is "not
// decidable yet" (D-03a) — neither terminal nor a skip-cause. Deliver the signal +
// resume → the tail Completes and the merge fires.

func mergeBelowParkedWorkflow(t *testing.T, store *InMemoryStore, clk Clock) *Workflow {
	t.Helper()
	const id = "inv03c"
	wb := NewWorkflowBuilder().WithWorkflowID(id)
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("other")
	wb.AddNode("taken").WithAction(choiceNoop())
	wb.AddNode("other").WithAction(choiceNoop())
	// The taken branch's tail parks on a signal.
	wb.AddWaitForSignal("takenWait", "go").DependsOn("taken")
	// The merge reconverges the taken tail (takenWait) + the not-taken branch (other).
	wb.AddMerge("done").From("takenWait", "other")
	wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())
	dag, err := wb.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store, Clock: clk}
}

// TestINV03_c_MergeBelowParkedTakenStaysPending is clause (c).
func TestINV03_c_MergeBelowParkedTakenStaysPending(t *testing.T) {
	store := NewInMemoryStore()
	clk := NewFakeClock(time.Unix(1_700_000_000, 0))
	const id = "inv03c"

	// Run to the park: the taken tail parks (Waiting); the run suspends.
	require.ErrorIs(t, mergeBelowParkedWorkflow(t, store, clk).Execute(context.Background()), ErrSuspended)

	parked, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, parked, "takenWait", Waiting) // the taken tail is parked
	assertNodeStatus(t, parked, "other", Bypassed)    // the not-taken branch
	// (c) the load-bearing assertion: the merge over a parked taken tail is left
	// PENDING — NOT fired (Completed), NOT Bypassed, NOT Skipped.
	assertNodeStatus(t, parked, "done", Pending)
	assertNodeStatus(t, parked, "after", Pending)

	// Deliver the signal + resume → the taken tail wakes, the merge fires.
	require.NoError(t, mergeBelowParkedWorkflow(t, store, clk).DeliverSignal(Signal{ID: "s1", Name: "go"}))
	require.NoError(t, mergeBelowParkedWorkflow(t, store, clk).Execute(context.Background()))

	final, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, final, "takenWait", Completed)
	assertNodeStatus(t, final, "done", Completed) // the merge FIRED once its taken tail woke
	assertNodeStatus(t, final, "after", Completed)
}

// TestINV03_c_ClassifierBackstop is the DIRECT (Build-independent) bite for clause
// (c). Clause (c) is DOUBLE-GUARDED (routed up 2026-07-04):
//   - PRIMARY: whole-run-suspend — when the taken tail parks, the run suspends at
//     THAT level's barrier, so the merge's later level is never reached and it
//     stays Pending (verified by the integration test above; the plan's
//     "mutate classifyBlockedStatus -> the integration property goes RED" bite does
//     NOT falsify, because the suspend stops the run before the merge is classified).
//   - BACKSTOP: classifyBlockedStatus's assign=false arm — IF the merge's level WERE
//     reached with a Waiting tail, the classifier leaves it Pending (a Waiting dep is
//     "not decidable yet", D-03a), never Skipped/Bypassed.
//
// This test bites the BACKSTOP directly (where the classifier IS reached).
// BITE: mutate isSkipCause to treat Waiting as a skip-cause (or move Waiting out of
// classifyBlockedStatus's non-terminal bucket) -> assign becomes true (the merge is
// settled Skipped) -> this test FAILS. Verified to falsify; restore.
func TestINV03_c_ClassifierBackstop(t *testing.T) {
	// A mergeDependent node blocked by a Waiting taken tail + a Bypassed sibling.
	node, data := blockedFixture(map[string]NodeStatus{"parkedTail": Waiting, "bypassedTail": Bypassed}, nil)
	_, assign := classifyBlockedStatus(node, data, mergeDependent)
	assert.False(t, assign, "a merge with a Waiting taken tail is not decidable this pass -> stays Pending (D-03a)")
}
