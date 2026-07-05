package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 43 §2 — INV-03 clause (b): bypassed-durable-never-arms, incl. the
// re-arm-on-load path. A durable node (TimerNode / WaitForSignalNode) inside a
// BYPASSED branch never runs its Execute (the gate sets it Bypassed), so it never
// persists a fireAt (SetWait is only reached inside timerAction.Execute) and never
// consumes from the per-workflow mailbox. After a suspend+resume, there are ZERO
// armable records attributable to any bypassed node.

// bypassDurableWorkflow builds:
//
//	seed -> route --When(true)--> taken -> takenTimer (arms + parks -> the run suspends)
//	             --Otherwise---> dead  -> deadTimer  (Bypassed, must NOT arm)
//	                                   \-> deadSignal (Bypassed, must NOT consume)
//
// The taken TimerNode and the two bypassed durables sit at the SAME level, so the
// bypassed durables are classified Bypassed in the very pass where the taken timer
// parks (they are reached, not left Pending).
func bypassDurableWorkflow(t *testing.T, store *InMemoryStore, clk Clock) *Workflow {
	t.Helper()
	const id = "inv03b"
	wb := NewWorkflowBuilder().WithWorkflowID(id)
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("dead")
	wb.AddNode("taken").WithAction(choiceNoop())
	wb.AddNode("dead").WithAction(choiceNoop())
	wb.AddTimer("takenTimer", time.Hour).DependsOn("taken") // taken branch parks here
	wb.AddTimer("deadTimer", time.Hour).DependsOn("dead")   // bypassed durable
	wb.AddWaitForSignal("deadSignal", "wakeup").DependsOn("dead")
	dag, err := wb.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store, Clock: clk}
}

// armedNodes returns the set of node names holding a persisted wait (fireAt) —
// the armed timer-wheel. The "zero-entries for bypassed" framing of (b) is then
// "the armed set contains ONLY taken timers, no bypassed-branch node". (This is
// non-vacuous where the earlier {Bypassed}∩{has-fireAt} framing was not: a timer
// that WRONGLY arms parks Waiting, not Bypassed, so it must be caught by the
// armed SET, not by intersecting with Bypassed — code-review 43-F1.)
func armedNodes(t *testing.T, store *InMemoryStore, id string) map[string]bool {
	t.Helper()
	data, err := store.Load(id)
	require.NoError(t, err)
	set := map[string]bool{}
	data.ForEachWait(func(nodeName string, _ int64) { set[nodeName] = true })
	return set
}

// TestINV03_b_BypassedDurableNeverArms is clause (b). The bypassed branch's timer
// + signal never arm across the suspend+resume; the taken timer arms normally.
func TestINV03_b_BypassedDurableNeverArms(t *testing.T) {
	store := NewInMemoryStore()
	start := time.Unix(1_700_000_000, 0)
	clk := NewFakeClock(start)
	const id = "inv03b"

	// Run: the taken branch parks on its timer; the run suspends.
	require.ErrorIs(t, bypassDurableWorkflow(t, store, clk).Execute(context.Background()), ErrSuspended)

	persisted, err := store.Load(id)
	require.NoError(t, err)

	// The bypassed branch (entry + its two durables) is Bypassed.
	for _, n := range []string{"dead", "deadTimer", "deadSignal"} {
		assertNodeStatus(t, persisted, n, Bypassed)
	}
	// The taken timer armed + parked (the positive control — proves durables DO
	// arm when taken, so "never arms" for bypassed is meaningful).
	assertNodeStatus(t, persisted, "takenTimer", Waiting)
	_, takenArmed := persisted.GetWait("takenTimer")
	require.True(t, takenArmed, "the TAKEN timer armed a fireAt")

	// (b) the load-bearing assertions — bypassed durables never armed:
	// 1. the armed timer-wheel contains ONLY the taken timer — no bypassed-branch
	//    node armed. This is the falsifiable "zero-entries for bypassed" check: if a
	//    bypassed timer wrongly armed (the §2 bite), it joins this set (as Waiting)
	//    and the assertion reddens.
	assert.Equal(t, map[string]bool{"takenTimer": true}, armedNodes(t, store, id),
		"only the taken timer is armed; no bypassed-branch node holds a fireAt")
	// 2. the bypassed timer specifically has no fireAt.
	_, deadArmed := persisted.GetWait("deadTimer")
	assert.False(t, deadArmed, "a bypassed TimerNode must not persist a fireAt")
	// 3. DueTimers (the re-arm-on-load reader) names ONLY the taken timer.
	due, err := bypassDurableWorkflow(t, store, clk).DueTimers(start.Add(999 * time.Hour))
	require.NoError(t, err)
	assert.NotContains(t, due, "deadTimer", "a bypassed timer is never due (never armed)")
	assert.NotContains(t, due, "deadSignal", "a bypassed signal node is not a timer at all")

	// 4. delivering the bypassed node's signal + re-driving does NOT flip it out of
	//    Bypassed — the mailbox delivery finds no consumer for a bypassed node.
	w := bypassDurableWorkflow(t, store, clk)
	require.NoError(t, w.DeliverSignal(Signal{ID: "sig-1", Name: "wakeup"}))
	// Re-drive (a plain resume). The taken timer is still not due, so the run
	// re-parks (ErrSuspended); the bypassed signal node stays Bypassed regardless.
	_ = bypassDurableWorkflow(t, store, clk).Execute(context.Background()) //nolint:errcheck // re-drive after delivery; still-not-due re-parks (ErrSuspended), intentionally ignored
	after, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, after, "deadSignal", Bypassed) // never woke
	assert.NotContains(t, armedNodes(t, store, id), "deadTimer",
		"still no bypassed-branch node armed after signal delivery")
}
