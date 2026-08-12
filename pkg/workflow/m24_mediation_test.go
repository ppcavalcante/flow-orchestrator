package workflow_test

import (
	"context"
	"testing"

	workflow "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// M24 AUD-019: an external consumer action receives a SEALED per-node view. A forge of
// engine journal state (another node's status/output, run-level saga state) is refused
// and fails the node; the forge never takes effect.

func TestBoundary_SealedViewFailsStatusForge(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("seal-forge")
	b.AddStartNode("forger").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.SetNodeStatus("forger", workflow.Completed) // forge -- must be refused
		return nil                                       // the action "succeeds", but the forge must still fail the node
	})
	dag, err := b.Build()
	require.NoError(t, err)

	data := workflow.NewWorkflowData("seal-forge")
	execErr := dag.Execute(context.Background(), data)
	require.Error(t, execErr, "a forging action must fail the run")

	st, _ := data.GetNodeStatus("forger")
	require.Equal(t, workflow.Failed, st,
		"the forged Completed must NOT take effect; the executor marks the node Failed")
}

func TestBoundary_SealedViewFailsOtherNodeOutputForge(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("seal-out-forge")
	b.AddStartNode("w").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.SetOutput("some-other-node", "forged") // another node's output -- refused
		return nil
	})
	dag, err := b.Build()
	require.NoError(t, err)

	data := workflow.NewWorkflowData("seal-out-forge")
	require.Error(t, dag.Execute(context.Background(), data))

	_, ok := data.GetOutput("some-other-node")
	require.False(t, ok, "the forged output for another node must not be recorded")
}

func TestBoundary_SealedViewFailsRollbackForge(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("seal-rb-forge")
	b.AddStartNode("w").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.SetRollingBack(true) // engine-only -- refused
		return nil
	})
	dag, err := b.Build()
	require.NoError(t, err)

	data := workflow.NewWorkflowData("seal-rb-forge")
	require.Error(t, dag.Execute(context.Background(), data))
	require.False(t, data.IsRollingBack(), "an action must not be able to force rollback")
}

// AUD-018: a consumer action cannot write an engine-reserved (__-prefixed) key through
// the sealed view — that would clobber engine metadata (boundary envelope, digest, etc.).
func TestBoundary_SealedViewRefusesReservedKeyWrite(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("seal-reserved")
	b.AddStartNode("w").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set("__boundaries__", "evil") // clobber engine metadata -- must be refused
		return nil
	})
	dag, err := b.Build()
	require.NoError(t, err)

	data := workflow.NewWorkflowData("seal-reserved")
	require.Error(t, dag.Execute(context.Background(), data))

	got, _ := data.Get("__boundaries__")
	require.Nil(t, got, "a consumer write to a reserved key must not take effect")
}

// The legitimate consumer surface still works through the sealed view: consumer data
// writes and the action's OWN output.
func TestBoundary_SealedViewAllowsConsumerWrites(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("seal-ok")
	b.AddStartNode("worker").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set("k", "v")              // consumer data -- allowed
		data.SetOutput("worker", "out") // OWN-node output -- allowed
		return nil
	})
	dag, err := b.Build()
	require.NoError(t, err)

	data := workflow.NewWorkflowData("seal-ok")
	require.NoError(t, dag.Execute(context.Background(), data))

	got, ok := data.Get("k")
	require.True(t, ok)
	require.Equal(t, "v", got)

	out, ok := data.GetOutput("worker")
	require.True(t, ok)
	require.Equal(t, "out", out)

	st, _ := data.GetNodeStatus("worker")
	require.Equal(t, workflow.Completed, st)
}
