package workflow_test

import (
	"context"
	"testing"

	workflow "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// AUD-040 / A-03: the fluent builder + expander types are EXPORTED, so an external
// consumer can name them for a variable, a wrapper, a struct field, or documentation —
// not just chain off them anonymously. This test is package workflow_test (an external
// consumer): if any of these types were unexported, it would not COMPILE.

// consumerWrapper is the exact use the audit named: an external consumer holding the
// builder/expander types in STRUCT FIELDS (a wrapper). It only compiles if all three
// types are exported.
type consumerWrapper struct {
	choice   *workflow.ChoiceBuilder
	merge    *workflow.MergeBuilder
	expander workflow.FanOutExpander
}

func TestAUD040_BuilderTypesAreNameable(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("aud040")

	w := consumerWrapper{
		choice: b.AddChoice("route"),
		merge:  b.AddMerge("done"),
		expander: func(_ context.Context, _ *workflow.WorkflowData) ([]interface{}, error) {
			return []interface{}{1, 2}, nil
		},
	}
	w.choice.When(func(*workflow.WorkflowData) bool { return true }, "x").Otherwise("y")
	w.merge.From("x", "y")
	require.NotNil(t, w.expander)

	b.AddStartNode("x").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	b.AddNode("y").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	b.AddFanOut("fan", w.expander, workflow.ActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })).
		DependsOn("done")

	_, err := b.Build()
	require.NoError(t, err)
}
