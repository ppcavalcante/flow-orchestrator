package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M22 ph114 — operability error-papercut bites (HARD-11). Each proves the wrong
// input now yields a CLEAR, actionable message that names what to fix (not just
// that something is wrong). Message-only changes; control flow is unchanged, so
// the existing Err* sentinel assertions stay green — these only add the message.

// TestPapercut_ReconvergenceNamesOffendingEdges (F-PG-06): a plain node that
// reconverges two branches of one ChoiceNode is rejected with ErrUnstructuredMerge —
// and the message now NAMES the offending DependsOn edges, so the operator knows
// exactly which dependencies to fix. Bite: without the fix the message names only
// the branch COUNT ("2 branches"), leaving the operator to hunt.
func TestPapercut_ReconvergenceNamesOffendingEdges(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("papercut-reconverge")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(choiceNoop())
	wb.AddNode("small").WithAction(choiceNoop())
	// "join" plainly reconverges BOTH branches of "route" — an implicit OR-join.
	wb.AddNode("join").DependsOn("big", "small").WithAction(choiceNoop())

	_, err := wb.Build()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnstructuredMerge, "still the same typed error (control flow unchanged)")

	msg := err.Error()
	assert.Contains(t, msg, "join", "the message names the offending node")
	assert.Contains(t, msg, "route", "the message names the ChoiceNode")
	// The load-bearing improvement: the specific offending DependsOn edges are named.
	assert.Contains(t, msg, "big", "the message names the first colliding branch entry")
	assert.Contains(t, msg, "small", "the message names the second colliding branch entry")
	assert.True(t, strings.Contains(msg, "AddMerge"), "the message tells the operator the fix (use AddMerge)")
}

// TestPapercut_AddApprovalEmptyNameIsLoud (F-PG-01): AddApproval("") used to build
// SILENTLY (no error) into a node whose decision signal name is "" — a node that can
// never be satisfied. Now it fails loud at Build with an actionable message naming
// the fix. Bite: without the guard Build returns nil (the silent footgun).
func TestPapercut_AddApprovalEmptyNameIsLoud(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("papercut-approval")
	wb.AddApproval("") // bare / empty — the footgun

	_, err := wb.Build()
	require.Error(t, err, "a bare AddApproval(\"\") is loud at Build, not a silent unsatisfiable node")
	require.ErrorIs(t, err, ErrValidation, "it is a typed validation error")
	msg := err.Error()
	assert.Contains(t, msg, "AddApproval", "the message names the offending builder call")
	assert.Contains(t, msg, "non-empty", "the message says what is wrong")
	assert.Contains(t, msg, "ApproveSignal", "the message names how to satisfy it (the decision signal helper)")

	// A valid name still builds fine (the guard only fires on empty).
	wbOK := NewWorkflowBuilder().WithWorkflowID("papercut-approval-ok")
	wbOK.AddApproval("gate")
	_, errOK := wbOK.Build()
	require.NoError(t, errOK, "a non-empty AddApproval name builds cleanly")
}
