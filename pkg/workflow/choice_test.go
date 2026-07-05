package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func choiceNoop() Action {
	return ActionFunc(func(context.Context, *WorkflowData) error { return nil })
}

func countingAction(counter *atomic.Int32) Action {
	return ActionFunc(func(context.Context, *WorkflowData) error {
		counter.Add(1)
		return nil
	})
}

func assertNodeStatus(t *testing.T, data *WorkflowData, node string, want NodeStatus) {
	t.Helper()
	st, ok := data.GetNodeStatus(node)
	require.True(t, ok, "node %s has no status", node)
	assert.Equal(t, want, st, "node %s status", node)
}

// TestChoice_FirstMatch: two branches whose predicates BOTH match -> the FIRST
// (declared order) is taken, the second is Bypassed and never invoked
// (DEC-M11-FIRSTMATCH).
func TestChoice_FirstMatch(t *testing.T) {
	var bigN, smallN, bigChildN atomic.Int32
	truePred := func(*WorkflowData) bool { return true }

	wb := NewWorkflowBuilder().WithWorkflowID("choice-firstmatch")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(truePred, "big").
		When(truePred, "small")
	wb.AddNode("big").WithAction(countingAction(&bigN))
	wb.AddNode("small").WithAction(countingAction(&smallN))
	wb.AddNode("bigChild").DependsOn("big").WithAction(countingAction(&bigChildN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("choice-firstmatch")
	require.NoError(t, dag.Execute(context.Background(), data))

	assert.Equal(t, int32(1), bigN.Load(), "the first matching branch runs")
	assert.Equal(t, int32(0), smallN.Load(), "the second matching branch is NOT taken")
	assert.Equal(t, int32(1), bigChildN.Load(), "the taken branch interior runs")

	assertNodeStatus(t, data, "route", Completed) // a ChoiceNode always Completes (D38-01)
	assertNodeStatus(t, data, "big", Completed)
	assertNodeStatus(t, data, "small", Bypassed)
}

// TestChoice_Bypass: a not-taken branch's action is never invoked and its
// interior nodes are Bypassed (NOT Skipped) — the T2 cause-aware gate propagates
// Bypassed through the not-taken subgraph.
func TestChoice_Bypass(t *testing.T) {
	var takenN, notTakenN, notTakenChildN atomic.Int32

	wb := NewWorkflowBuilder().WithWorkflowID("choice-bypass")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("notTaken")
	wb.AddNode("taken").WithAction(countingAction(&takenN))
	wb.AddNode("notTaken").WithAction(countingAction(&notTakenN))
	wb.AddNode("notTakenChild").DependsOn("notTaken").WithAction(countingAction(&notTakenChildN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("choice-bypass")
	require.NoError(t, dag.Execute(context.Background(), data))

	assert.Equal(t, int32(1), takenN.Load(), "taken branch runs")
	assert.Equal(t, int32(0), notTakenN.Load(), "not-taken branch entry action never invoked")
	assert.Equal(t, int32(0), notTakenChildN.Load(), "not-taken branch interior action never invoked")

	assertNodeStatus(t, data, "taken", Completed)
	assertNodeStatus(t, data, "notTaken", Bypassed)
	// The load-bearing distinction: the interior is Bypassed, NOT Skipped.
	assertNodeStatus(t, data, "notTakenChild", Bypassed)
}

// TestChoice_NoMatch_Default: no When matches but an Otherwise is declared ->
// the default is taken, the When targets are Bypassed (CHOICE-04).
func TestChoice_NoMatch_Default(t *testing.T) {
	var aN, defN atomic.Int32
	falsePred := func(*WorkflowData) bool { return false }

	wb := NewWorkflowBuilder().WithWorkflowID("choice-default")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(falsePred, "a").
		Otherwise("def")
	wb.AddNode("a").WithAction(countingAction(&aN))
	wb.AddNode("def").WithAction(countingAction(&defN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("choice-default")
	require.NoError(t, dag.Execute(context.Background(), data))

	assert.Equal(t, int32(0), aN.Load(), "unmatched When branch is not taken")
	assert.Equal(t, int32(1), defN.Load(), "the Otherwise default runs")
	assertNodeStatus(t, data, "a", Bypassed)
	assertNodeStatus(t, data, "def", Completed)
}

// TestChoice_NoMatch_NoDefault: no When matches and no Otherwise -> Execute
// returns a typed error wrapping ErrNoBranchMatched, promptly (a timeout context
// proves it neither hangs nor panics). CHOICE-04.
func TestChoice_NoMatch_NoDefault(t *testing.T) {
	var aN atomic.Int32
	falsePred := func(*WorkflowData) bool { return false }

	wb := NewWorkflowBuilder().WithWorkflowID("choice-deadend")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(falsePred, "a")
	wb.AddNode("a").WithAction(countingAction(&aN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("choice-deadend")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = dag.Execute(ctx, data)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoBranchMatched, "an unmatched choice with no Otherwise is a typed error")
	assert.NoError(t, ctx.Err(), "the choice returned an error, it did not hang until the deadline")
	assert.Equal(t, int32(0), aN.Load(), "the unmatched branch never ran")
	// 41-F1: a routing FAILURE cascades to Skipped ("an upstream failed"), NOT
	// Bypassed — the choice Failed, so its downstream is Skipped, not cleanly
	// not-taken. route is Failed; the not-taken branch entry is Skipped.
	assertNodeStatus(t, data, "route", Failed)
	assertNodeStatus(t, data, "a", Skipped)
}

// TestChoice_MissingKey: a predicate reading an absent key returns false (the
// caller's data-precondition, D-09) and routes to the default — never a panic.
func TestChoice_MissingKey(t *testing.T) {
	var defN atomic.Int32
	// Reads "amt" which is never set: GetInt returns (0,false) -> 0 > 0 is false.
	pred := func(d *WorkflowData) bool { v, _ := d.GetInt("amt"); return v > 0 }

	wb := NewWorkflowBuilder().WithWorkflowID("choice-missingkey")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(pred, "a").
		Otherwise("def")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("def").WithAction(countingAction(&defN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("choice-missingkey")
	require.NotPanics(t, func() {
		require.NoError(t, dag.Execute(context.Background(), data))
	})
	assert.Equal(t, int32(1), defN.Load(), "absent-key predicate routes to the default")
	assertNodeStatus(t, data, "a", Bypassed)
}

// TestChoice_UnknownTarget: a When/Otherwise routing to a node that was never
// declared is a Build-time error (a routing typo cannot silently dangle).
func TestChoice_UnknownTarget(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("choice-unknown")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "ghost")
	_, err := wb.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost", "Build names the unknown branch target")
}

// TestChoice_Representation exercises the direct newChoiceNode representation
// (symmetric with NewTimerNode): the node's action is a *choiceAction, and
// running it with a matching sole branch Completes without marking the taken
// target.
func TestChoice_Representation(t *testing.T) {
	choice := newChoiceNode(
		"route",
		[]choiceBranch{{predicate: func(*WorkflowData) bool { return true }, target: "x"}},
		"", false,
	)
	_, ok := choice.Action.(*choiceAction)
	require.True(t, ok, "a ChoiceNode's action is a *choiceAction")

	data := NewWorkflowData("wf")
	require.NoError(t, choice.Action.Execute(context.Background(), data))
	_, ok = data.GetNodeStatus("x")
	assert.False(t, ok, "the taken target is left for the gate, not marked by the choice")
}
