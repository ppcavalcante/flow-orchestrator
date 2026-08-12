package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-011 / C-05 / F118-ENG-01: (*DAG).Execute satisfies Action, so a compiled
// child DAG can be passed to WithAction and run as an ordinary node action,
// bypassing AddSubWorkflow's depth/cycle/suspendable-child guards. Build must
// reject it (directly or wrapped) and point at the sanctioned entry.
func TestAUD011_BuildRejectsCompiledDAGAsAction(t *testing.T) {
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })

	buildChild := func() *DAG {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("c").WithActionFunc(noop)
		child, err := cb.Build()
		require.NoError(t, err)
		require.NotNil(t, child)
		return child
	}

	t.Run("direct", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddStartNode("n").WithAction(buildChild())
		dag, err := b.Build()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.Nil(t, dag)
	})

	t.Run("wrapped in composite", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddStartNode("n").WithAction(NewCompositeAction(buildChild()))
		dag, err := b.Build()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.Nil(t, dag)
	})

	t.Run("wrapped in retryable", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddStartNode("n").WithAction(NewRetryableAction(buildChild(), 1, 0))
		dag, err := b.Build()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.Nil(t, dag)
	})

	// Sanity: the sanctioned nesting entry (AddSubWorkflow) must still build.
	t.Run("AddSubWorkflow still builds", func(t *testing.T) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("c").WithActionFunc(noop)
		child, err := cb.Build()
		require.NoError(t, err)

		b := NewWorkflowBuilder()
		b.AddStartNode("root").WithActionFunc(noop)
		b.AddSubWorkflow("sub", child).DependsOn("root")
		_, err = b.Build()
		require.NoError(t, err, "AddSubWorkflow is the sanctioned nesting entry and must still build")
	})
}
