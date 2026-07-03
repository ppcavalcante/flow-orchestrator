package workflow

import "errors"

// ErrSuspended is the sentinel returned by Workflow.Execute / DAG.Execute when a
// run has PARKED rather than completed or failed. It makes Execute a
// three-outcome operation:
//
//   - nil          — the run completed (every node reached a terminal status).
//   - ErrSuspended — the run parked: a declared suspension node is Waiting on an
//     external event (a clock or a signal), and the run's progress has been
//     durably checkpointed. This is a SUCCESS arm, not a failure — the process
//     may exit entirely and resume later by re-running Execute with the same
//     WorkflowID + Store ("suspend is a crash you chose").
//   - any other error — a real failure (an *ExecutionError, a cancellation, a
//     validation or store error).
//
// Callers distinguish the park arm with errors.Is(err, ErrSuspended). Because a
// parked run has durably flushed its checkpoint before this is returned (see the
// executor's barrier-drain → flush → ErrSuspended path), the suspend arm must be
// short-circuited everywhere a non-nil Execute/action error is interpreted as a
// failure: it is never retried, never aggregated into an *ExecutionError, and
// never recorded as a NodeError. The complete set of short-circuit sites is the
// retry wrappers (RetryableAction, RetryMiddleware, NoDelayRetryMiddleware), the
// per-node executor goroutine, the level loop, and the Workflow.Execute wrapper.
//
// Only a DECLARED suspension node may park: node.Execute honors an ErrSuspended
// return from an action ONLY when that action is a declared suspension node (it
// satisfies the package-internal suspension marker). An ErrSuspended returned by
// an ordinary action is treated as a real failure — this keeps the suspension
// topology statically known. (M10 / DEC-M10-mechanism.)
var ErrSuspended = errors.New("workflow suspended: a node is waiting on an external event")

// ErrSuspendRequiresCheckpointer is returned (as a real FAILURE, not the park
// success arm) when a declared suspension node parks but no durable checkpoint is
// wired — i.e. the workflow's Store does not implement Checkpointer, or
// DAG.Execute was driven directly without Workflow.Execute's checkpoint wiring.
// A park is only durable ("suspend is a crash you chose") if its bytes are
// flushed before Execute returns (durable-flush-before-suspend); with no
// checkpoint there would be a parked-but-unpersisted run that no resume could
// recover, so this is surfaced as a configuration error rather than a silently
// non-durable ErrSuspended. To suspend, run via Workflow.Execute with a Store
// that implements Checkpointer (the built-in InMemory / JSONFile / FlatBuffers
// stores all do). (M10 / DEC-M10; durable-flush-before-suspend.)
var ErrSuspendRequiresCheckpointer = errors.New("workflow cannot suspend: no durable checkpoint configured (use Workflow.Execute with a Checkpointer Store)")

// suspendableAction is the package-internal marker an Action must satisfy for
// its ErrSuspended return to be honored as an intentional PARK rather than a
// failure. The capability is a STATIC code property of the action's Go type,
// re-derived from the rebuilt DAG on every run/resume and never persisted in the
// checkpoint (the checkpoint stores the Waiting status + wake metadata, never
// "this node is suspension-capable") — so resume needs no extra graph state and
// the graph-identity guard is untouched.
//
// Because the marker method is UNEXPORTED, only types declared inside package
// workflow can satisfy it: the real TimerNode / WaitForSignalNode actions in
// later chunks, and the test-only park vehicle. A caller's own Action cannot
// park — an ErrSuspended from a non-declared action is a misuse that node.Execute
// fails loudly rather than honoring. This makes the suspension topology
// statically known (TLA can enumerate the waiting-capable nodes).
//
// NOTE (forward constraint): the marker is matched on the node's DIRECT Action.
// Wrapping a suspension action in middleware (which re-wraps it in an ActionFunc)
// hides the marker and disables suspension — the declared-node builders in
// phases 36/37 must set the suspending action directly, not route it through the
// general middleware stack. (DEC-M10-mechanism.)
//
// NOTE (forward constraint, review F3): a declared suspension node bypasses the
// timeout and retry wrappers (a park is never timed-out or retried), so it has no
// engine-side backstop against an action that BLOCKS instead of promptly
// returning its park-or-complete decision. The real TimerNode / WaitForSignalNode
// actions in phases 36/37 MUST compute their park decision without blocking I/O
// and MUST remain context-aware (so a sibling's fail-fast cancel still drops
// them) — the action's job is to RETURN ErrSuspended, not to wait in-process.
type suspendableAction interface {
	Action
	suspendable()
}
