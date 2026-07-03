package workflow

import "context"

// checkpointCtxKey is the private context key carrying the per-invocation durable
// checkpoint callback. The callback is injected at the top of Workflow.Execute
// (when the Store implements Checkpointer) and flows down through the same ctx the
// executor already threads into DAG.Execute, exactly like the timer Clock
// (clock.go). Carrying it on the ctx — rather than on a shared ExecutionConfig
// field on the *DAG — is what makes concurrent Execute on one *Workflow
// memory-safe: each Execute call has its OWN ctx-scoped callback, so two drivers
// of the same *Workflow no longer race a shared field (and there is no
// `defer …=nil` that one run could use to nil out another run's callback). This
// is the M10 phase-37 T1 structural fix for the checkpoint-field data race
// (DEC-M10-P37-LEASE(a) / MH37-5a); the in-process lease (T6) is the logical
// double-drive guard layered on top of this memory-safety fix.
type checkpointCtxKey struct{}

// withCheckpoint returns a child context carrying cp as the durable mid-run
// checkpoint callback. Workflow.Execute injects it when the Store implements
// Checkpointer; a non-checkpointing Store injects nothing, so checkpointFrom
// resolves to nil and DAG.Execute stays on its zero-overhead, save-at-boundaries
// path.
func withCheckpoint(ctx context.Context, cp func(data *WorkflowData) error) context.Context {
	return context.WithValue(ctx, checkpointCtxKey{}, cp)
}

// checkpointFrom extracts the injected checkpoint callback from ctx, returning nil
// when none was injected. Unlike clockFrom (which falls back to the system clock),
// the nil return is SEMANTICALLY LOAD-BEARING: DAG.Execute distinguishes
// "checkpoint wired" from "not wired" (a park with no checkpoint wired returns
// ErrSuspendRequiresCheckpointer — D-11; a completed level with no checkpoint
// wired simply skips the flush — DEC-M9). A non-checkpointing run carries no
// callback and reads back nil, preserving that exact behavior.
func checkpointFrom(ctx context.Context) func(data *WorkflowData) error {
	if cp, ok := ctx.Value(checkpointCtxKey{}).(func(data *WorkflowData) error); ok {
		return cp
	}
	return nil
}
