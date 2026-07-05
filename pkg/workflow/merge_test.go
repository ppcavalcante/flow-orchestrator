package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T1a: MergeNode representation + AddMerge().From() builder wiring ---------
// NOTE: OR-join FIRING semantics (a Bypassed tail satisfied; all-bypassed ->
// Bypassed; taken-tail fail-fast) need the mergeDependent classifier fill (T1b),
// which is HELD pending the architect's confirm of the routed-up seam shape.
// These tests cover only the builder/representation, which is role-independent.

// TestMerge_Representation: newMergeNode carries a *mergeAction with its tails;
// the default action is a pass-through join (Completes, no user action).
func TestMerge_Representation(t *testing.T) {
	n := newMergeNode("done", []string{"a", "b"})
	ma, ok := n.Action.(*mergeAction)
	require.True(t, ok, "a MergeNode's action is a *mergeAction (the OR-join marker)")
	assert.Equal(t, []string{"a", "b"}, ma.tails)
	assert.Nil(t, ma.userAction, "default is a pass-through join")
	// Pass-through Execute Completes cleanly.
	require.NoError(t, n.Action.Execute(context.Background(), NewWorkflowData("wf")))
}

// minimalChoiceMerge builds the smallest valid structured choice-merge:
// seed -> route.When(pickX,"x").Otherwise("y"); done = merge.From("x","y"). The
// merge optionally carries a user join action.
func minimalChoiceMerge(t *testing.T, pickX bool, mergeAct interface{}) *DAG {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID("mcm")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return pickX }, "x").
		Otherwise("y")
	wb.AddNode("x").WithAction(choiceNoop())
	wb.AddNode("y").WithAction(choiceNoop())
	mb := wb.AddMerge("done").From("x", "y")
	if mergeAct != nil {
		mb.WithAction(mergeAct)
	}
	dag, err := wb.Build()
	require.NoError(t, err)
	return dag
}

// TestMerge_BuilderWiresTails: AddMerge("done").From("x","y") wires the merge to
// depend on both branch tails.
func TestMerge_BuilderWiresTails(t *testing.T) {
	dag := minimalChoiceMerge(t, true, nil)
	node, ok := dag.GetNode("done")
	require.True(t, ok)
	depNames := map[string]bool{}
	for _, d := range node.DependsOn {
		depNames[d.Name] = true
	}
	assert.True(t, depNames["x"] && depNames["y"], "merge depends on both From tails")
}

// TestMerge_UnknownTail: From referencing an undeclared tail is a Build error.
func TestMerge_UnknownTail(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("merge-unknown")
	wb.AddStartNode("a").WithAction(choiceNoop())
	wb.AddMerge("done").From("a", "ghost")
	_, err := wb.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost", "Build names the unknown tail")
}

// TestMerge_WithAction: a supplied join action replaces the pass-through and runs
// when the merge fires; an unsupported action type is a Build error.
func TestMerge_WithAction(t *testing.T) {
	ran := false
	dag := minimalChoiceMerge(t, true, func(context.Context, *WorkflowData) error {
		ran = true
		return nil
	})
	require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("mcm")))
	assert.True(t, ran, "the supplied merge join action runs on fire")

	// An unsupported action type is a Build error (caught at node creation, before
	// reconvergence validation).
	wbBad := NewWorkflowBuilder().WithWorkflowID("merge-bad")
	wbBad.AddStartNode("a").WithAction(choiceNoop())
	wbBad.AddMerge("done").From("a").WithAction(42) // unsupported type
	_, err := wbBad.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported merge action")
}

// --- T2: strict reconvergence validator (D-P42-STRICT) ------------------------

// TestValidate_AcceptsWellFormed: the canonical structured choice-merge builds
// with no error.
func TestValidate_AcceptsWellFormed(t *testing.T) {
	_ = minimalChoiceMerge(t, true, nil) // Build inside asserts NoError
}

// TestValidate_RejectsNonMergeReconvergence: a plain AddNode depending on two
// branch tails of one Choice is an implicit OR-join -> typed error (must be a
// MergeNode). D-P42-STRICT(a).
func TestValidate_RejectsNonMergeReconvergence(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("v-nonmerge")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "x").
		Otherwise("y")
	wb.AddNode("x").WithAction(choiceNoop())
	wb.AddNode("y").WithAction(choiceNoop())
	wb.AddNode("join").DependsOn("x", "y").WithAction(choiceNoop()) // plain reconvergence
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge)
}

// TestValidate_RejectsCrossChoiceMerge: a merge whose From tails come from two
// different Choices -> typed error. D-P42-STRICT(b).
func TestValidate_RejectsCrossChoiceMerge(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("v-crosschoice")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("c1").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a1").
		Otherwise("a2")
	wb.AddChoice("c2").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "b1").
		Otherwise("b2")
	for _, n := range []string{"a1", "a2", "b1", "b2"} {
		wb.AddNode(n).WithAction(choiceNoop())
	}
	wb.AddMerge("done").From("a1", "b1") // tails from two different Choices
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge)
}

// TestValidate_RejectsSharedBranch: a branch entry used as a target by two
// AddChoice calls -> typed error. D-P42-STRICT(c) (resolves 41-F2).
func TestValidate_RejectsSharedBranch(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("v-shared")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("c1").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "shared").
		Otherwise("o1")
	wb.AddChoice("c2").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "shared"). // same target as c1
		Otherwise("o2")
	for _, n := range []string{"shared", "o1", "o2"} {
		wb.AddNode(n).WithAction(choiceNoop())
	}
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSharedBranch)
}

// TestValidate_RejectsDanglingMerge: a merge joining a tail under no Choice ->
// typed error. D-P42-STRICT(b, dangling).
func TestValidate_RejectsDanglingMerge(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("v-dangling")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "x").
		Otherwise("y")
	wb.AddNode("x").WithAction(choiceNoop())
	wb.AddNode("y").WithAction(choiceNoop())
	wb.AddStartNode("loose").WithAction(choiceNoop()) // under no Choice
	wb.AddMerge("done").From("x", "y", "loose")
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDanglingMerge)
}

// --- T1b: OR-join firing semantics (DEC-M11-P42-SEAM) -------------------------

// choiceMergeDAG builds the canonical structured choice-merge:
//
//	seed -> route(choice) --When(pickBig)--> big -> bigEnd -\
//	                       --Otherwise-----> small -> smallEnd -> done(merge) -> after
func choiceMergeDAG(t *testing.T, pickBig bool, bigEndFails bool, probe *choiceProbe) *DAG {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID("cm")
	wb.AddStartNode("seed").WithAction(probe.action("seed"))
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return pickBig }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(probe.action("big"))
	bigEnd := wb.AddNode("bigEnd").DependsOn("big")
	if bigEndFails {
		bigEnd.WithAction(ActionFunc(func(context.Context, *WorkflowData) error {
			return assert.AnError
		}))
	} else {
		bigEnd.WithAction(probe.action("bigEnd"))
	}
	wb.AddNode("small").WithAction(probe.action("small"))
	wb.AddNode("smallEnd").DependsOn("small").WithAction(probe.action("smallEnd"))
	wb.AddMerge("done").From("bigEnd", "smallEnd")
	wb.AddNode("after").DependsOn("done").WithAction(probe.action("after"))
	dag, err := wb.Build()
	require.NoError(t, err)
	return dag
}

// TestMerge_FiresOnTakenTail is the headline JOIN-01 regression: a merge below a
// 2-branch Choice, whose not-taken sibling tail is Bypassed, now RUNS (in ph41
// the same merge stayed Skipped — strict-AND treated the Bypassed tail as a
// skip-cause). The merge fires and its downstream runs.
func TestMerge_FiresOnTakenTail(t *testing.T) {
	probe := newChoiceProbe()
	dag := choiceMergeDAG(t, true /*pickBig*/, false, probe)
	data := NewWorkflowData("cm")
	require.NoError(t, dag.Execute(context.Background(), data))

	assertNodeStatus(t, data, "bigEnd", Completed)  // taken branch ran
	assertNodeStatus(t, data, "smallEnd", Bypassed) // not-taken branch bypassed
	assertNodeStatus(t, data, "done", Completed)    // the merge FIRED (the ph41->ph42 flip)
	assertNodeStatus(t, data, "after", Completed)   // downstream of the merge runs
	assert.Equal(t, int32(1), probe.fired("after"))
}

// TestMerge_TakenTailFails_FailFast is MH-3/INV-01: the TAKEN branch-tail fails
// (non-coe) -> fail-fast; the merge does not fire and is Skipped (NOT Bypassed —
// a failure cascade, not a clean not-taken), downstream Skipped.
func TestMerge_TakenTailFails_FailFast(t *testing.T) {
	probe := newChoiceProbe()
	dag := choiceMergeDAG(t, true /*pickBig*/, true /*bigEndFails*/, probe)
	data := NewWorkflowData("cm")
	err := dag.Execute(context.Background(), data)
	require.Error(t, err) // fail-fast surfaces an *ExecutionError

	assertNodeStatus(t, data, "bigEnd", Failed)
	assertNodeStatus(t, data, "done", Skipped)  // merge Skipped, NOT Bypassed, NOT Completed
	assertNodeStatus(t, data, "after", Skipped) // transitive
	assert.Equal(t, int32(0), probe.fired("after"))
}

// TestMerge_AllBypassed_IsBypassed is MH-2: when an OUTER choice bypasses a branch
// that contains an inner choice+merge, the inner merge sees all tails Bypassed ->
// it is itself Bypassed (composes downward), never fires. A structured, single-
// Choice-per-merge construction (valid under the strict validator).
func TestMerge_AllBypassed_IsBypassed(t *testing.T) {
	probe := newChoiceProbe()
	wb := NewWorkflowBuilder().WithWorkflowID("cm-nested")
	wb.AddStartNode("seed").WithAction(probe.action("seed"))
	// Outer choice takes "live"; the "ic" (inner-choice) branch is bypassed.
	wb.AddChoice("oc").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "live").
		Otherwise("ic")
	wb.AddNode("live").WithAction(probe.action("live"))
	// Inner choice + its two branch tails + the inner merge — all under the
	// bypassed outer branch.
	wb.AddChoice("ic").
		When(func(*WorkflowData) bool { return true }, "ia").
		Otherwise("ib")
	wb.AddNode("ia").WithAction(probe.action("ia"))
	wb.AddNode("ib").WithAction(probe.action("ib"))
	wb.AddMerge("im").From("ia", "ib")
	wb.AddNode("afterInner").DependsOn("im").WithAction(probe.action("afterInner"))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("cm-nested")
	require.NoError(t, dag.Execute(context.Background(), data))

	assertNodeStatus(t, data, "live", Completed)
	assertNodeStatus(t, data, "ic", Bypassed) // the inner choice never decided
	assertNodeStatus(t, data, "ia", Bypassed)
	assertNodeStatus(t, data, "ib", Bypassed)
	assertNodeStatus(t, data, "im", Bypassed) // all tails bypassed -> merge Bypassed (MH-2)
	assertNodeStatus(t, data, "afterInner", Bypassed)
}
