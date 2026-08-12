package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// VB-07 J1, discharged THROUGH THE PUBLIC API FROM AN EXTERNAL PACKAGE.
//
// 🔴 `package workflow_test` IS MANDATORY HERE AND IS NOT A STYLE CHOICE. Nearly every
// test file in pkg/workflow is `package workflow`, and an in-package test structurally
// cannot exhibit a hole an external consumer would fall into: it can reach unexported
// fields, call unexported constructors, and assert on shapes no caller can build. The 118
// close discharged VB-02 on the wire, from an external module, through the exported
// surface only. J1 discharges the same way or it is not the same idiom.
//
// Everything below is reachable from a consumer's import: NewWorkflowBuilder, AddNode,
// WithAction, DependsOn, WithContinueOnError, AddTimer, WithBoundary, Build, ErrValidation.

func vb07Act() workflow.ActionFunc {
	return workflow.ActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
}

// TestVB07_J1_VerifierWithContinueOnError_IsRefusedAtBuild is the J1 fixture.
func TestVB07_J1_VerifierWithContinueOnError_IsRefusedAtBuild(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1")
	b.AddNode("D").WithAction(vb07Act())
	b.AddNode("V").WithAction(vb07Act()).DependsOn("D").WithContinueOnError()
	b.AddNode("S").WithAction(vb07Act()).DependsOn("V")
	b.WithBoundary("D", "V", "S")

	dag, err := b.Build()

	require.Nil(t, dag, "a refused declaration must not yield a DAG")
	require.True(t, errors.Is(err, workflow.ErrValidation),
		"the refusal must be a typed ErrValidation, matching VB-02/VB-08's shape; got: %v", err)
	require.Contains(t, err.Error(), `"V"`,
		"the refusal must NAME the offending node -- a consumer needs the node to look at; got: %v", err)
	require.Contains(t, err.Error(), "ContinueOnError",
		"the refusal must carry the criterion's reason, not merely reject; got: %v", err)
}

// TestVB07_J1_Controls is the half a bite cannot show. A clause that refuses everything
// bites just as convincingly as one that discriminates, so each of these must stay
// ACCEPTED and each is a separate arm with its own name.
//
// 🔴 EVERY ARM'S FAILURE IS READ FOR ITS REASON, NOT JUST ITS VERDICT. At the 118 close a
// five-arm probe red on all five INCLUDING the no-boundary control, every one of them with
// an unrelated upstream builder error -- a different guard substituting its message for the
// one under test. A red for the wrong reason is not evidence about this clause, so each arm
// asserts NoError with the message quoted.
func TestVB07_J1_Controls(t *testing.T) {
	t.Run("continue-on-error on the DOER is accepted", func(t *testing.T) {
		b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1-c1")
		b.AddNode("D").WithAction(vb07Act()).WithContinueOnError()
		b.AddNode("V").WithAction(vb07Act()).DependsOn("D")
		b.AddNode("S").WithAction(vb07Act()).DependsOn("V")
		b.WithBoundary("D", "V", "S")
		dag, err := b.Build()
		require.NoError(t, err, "J1 is scoped to the VERIFIER; a doer carrying the flag is not its business")
		require.NotNil(t, dag)
	})

	t.Run("continue-on-error on the SINK is accepted", func(t *testing.T) {
		b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1-c2")
		b.AddNode("D").WithAction(vb07Act())
		b.AddNode("V").WithAction(vb07Act()).DependsOn("D")
		b.AddNode("S").WithAction(vb07Act()).DependsOn("V").WithContinueOnError()
		b.WithBoundary("D", "V", "S")
		dag, err := b.Build()
		require.NoError(t, err, "J1 is scoped to the VERIFIER; a sink carrying the flag is not its business")
		require.NotNil(t, dag)
	})

	t.Run("continue-on-error on an UNRELATED node is accepted", func(t *testing.T) {
		b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1-c3")
		b.AddNode("D").WithAction(vb07Act())
		b.AddNode("V").WithAction(vb07Act()).DependsOn("D")
		b.AddNode("S").WithAction(vb07Act()).DependsOn("V")
		b.AddNode("X").WithAction(vb07Act()).DependsOn("D").WithContinueOnError()
		b.WithBoundary("D", "V", "S")
		dag, err := b.Build()
		require.NoError(t, err, "a node outside the declared triple carrying the flag is not J1's business")
		require.NotNil(t, dag)
	})

	t.Run("continue-on-error with NO boundary declared is accepted", func(t *testing.T) {
		b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1-c4")
		b.AddNode("D").WithAction(vb07Act())
		b.AddNode("V").WithAction(vb07Act()).DependsOn("D").WithContinueOnError()
		b.AddNode("S").WithAction(vb07Act()).DependsOn("V")
		dag, err := b.Build()
		require.NoError(t, err,
			"WITHOUT a declaration there is nothing for J1 to refuse. If this arm ever reds, the "+
				"clause has escaped the boundary path entirely and become a blanket ban on the flag")
		require.NotNil(t, dag)
	})
}

// TestVB07_J1_OrderIsPinnedOnTheWire is the external half of the placement decision. The
// in-package arm of TestBoundary_CheckOrderIsCheapestFirst pins the same thing against
// validateBoundary directly; this one pins that the ordering survives to a consumer, which
// is where a substituted message actually costs someone time.
func TestVB07_J1_OrderIsPinnedOnTheWire(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("vb07-j1-ord")
	b.AddNode("D").WithAction(vb07Act())
	// A timer is an opaque kind the action clause refuses, AND it carries the flag J1
	// refuses. Both clauses would reject; the CHEAPER one must answer.
	b.AddTimer("V", time.Millisecond).DependsOn("D").WithContinueOnError()
	b.AddNode("S").WithAction(vb07Act()).DependsOn("V")
	b.WithBoundary("D", "V", "S")

	_, err := b.Build()

	require.True(t, errors.Is(err, workflow.ErrValidation), "got: %v", err)
	require.Contains(t, err.Error(), "ContinueOnError",
		"J1 is ordered before the action clause and must answer; got: %v", err)
	require.NotContains(t, err.Error(), "timerAction",
		"J1 is the cheaper, more specific refusal and must answer on the wire too -- a general "+
			"clause substituting its message is what sends a consumer looking in the wrong "+
			"place; got: %v", err)
}
