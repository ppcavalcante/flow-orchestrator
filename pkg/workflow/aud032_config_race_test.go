package workflow_test

import (
	"context"
	"sync"
	"testing"

	workflow "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// AUD-032 / C-... : DAG.WithExecutionConfig / WithTracerProvider wrote d.config
// with no lock, while DAG.Execute reads d.config (MaxConcurrency, TracerProvider).
// Both are exported, so a consumer racing a config setter against a drive is a data
// race on the config field. Run under -race: RED before the fix (setter + Execute
// both touch d.config unsynchronized), GREEN after Execute snapshots config under
// the lock at entry and the setters take the write lock.
func TestAUD032_ConfigSetterDoesNotRaceExecute(t *testing.T) {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("aud032")
	b.AddStartNode("n").WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error { return nil })
	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	const iters = 500
	var wg sync.WaitGroup

	// Writer: hammer the config setter.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			dag.WithExecutionConfig(workflow.ExecutionConfig{MaxConcurrency: 1 + i%8})
		}
	}()

	// Reader: drive Execute repeatedly, each on its own fresh WorkflowData so the
	// only shared mutable state under test is the DAG's config field.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			data := workflow.NewWorkflowData("aud032")
			//nolint:errcheck // the drive's error is irrelevant here; this test asserts the absence of a data race on d.config, not the run outcome
			_ = dag.Execute(context.Background(), data)
		}
	}()

	wg.Wait()
}
