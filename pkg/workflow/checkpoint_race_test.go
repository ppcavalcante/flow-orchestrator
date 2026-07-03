package workflow

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentExecute_CheckpointFieldRaceClean is the M10-P37 T1 bite
// (MH37-5a): two concurrent Workflow.Execute on ONE *Workflow backed by a
// Checkpointer store must be MEMORY-safe under -race.
//
// Pre-fix, Workflow.Execute wrote the durable checkpoint callback into the shared
// w.DAG.config.checkpoint field on entry and `defer …=nil`'d it on exit, so two
// concurrent drivers of the same *Workflow raced that field (one's defer nilling
// the other's callback). The fix threads the callback on the per-Execute ctx
// (withCheckpoint/checkpointFrom) — nothing shared is mutated.
//
// Scope: this asserts MEMORY-safety ONLY. Logical single-writer serialization of
// two drivers of one WorkflowID is the T6 in-process lease's job; two concurrent
// drives both running is a logical double-drive (idempotent-apply softened),
// NOT a data race. The test therefore makes NO assertion on the logical outcome —
// `go test -race` is the oracle: a clean run proves the shared-field race is gone.
//
// Bite-proof (anti-vacuity): re-introducing any shared mutable write on the
// Execute path (e.g. restoring the w.DAG.config.checkpoint field assignment +
// defer) makes `-race` report a DATA RACE here and this test FAILS. Verified by
// the engineer before commit.
func TestConcurrentExecute_CheckpointFieldRaceClean(t *testing.T) {
	store := NewInMemoryStore() // implements Checkpointer → exercises the callback path
	wf := NewWorkflow(store)
	wf.WorkflowID = "concurrent-exec-race"
	mustAddNode(t, wf.DAG, NewNode("a", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	mustAddNode(t, wf.DAG, NewNode("b", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	mustAddDep(t, wf.DAG, "a", "b")

	const drivers = 8
	var wg sync.WaitGroup
	wg.Add(drivers)
	for i := 0; i < drivers; i++ {
		go func() {
			defer wg.Done()
			// Outcome intentionally ignored: a logical double-drive is allowed
			// (T6 lease serializes it); we are proving memory-safety, not ordering.
			_ = wf.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		}()
	}
	wg.Wait()
}
