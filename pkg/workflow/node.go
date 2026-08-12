package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// NodeStatus represents the execution status of a node
type NodeStatus string

const (
	// Pending is the initial state: the node has not started execution. Every
	// node in a DAG is set to Pending when Execute begins, so a node that is
	// never reached (e.g. a run that halted before it, with no failed/skipped
	// dependency of its own) remains observably Pending rather than absent.
	Pending NodeStatus = "pending"
	// Running indicates the node is currently executing
	Running NodeStatus = "running"
	// Completed indicates the node has completed successfully
	Completed NodeStatus = "completed"
	// Failed indicates the node's action returned an error
	Failed NodeStatus = "failed"
	// Skipped indicates the node did not run because at least one dependency was
	// in a terminal non-resolving state — a non-continue-on-error dependency
	// that Failed, or a dependency that was itself Skipped. Skipped is
	// transitive and is NOT a failure (it never appears in ExecutionError).
	Skipped NodeStatus = "skipped"
	// Waiting indicates the node has parked: it is blocked on an external event
	// (a clock or a signal) rather than on an upstream node, and the run has
	// suspended. Waiting is NON-TERMINAL and NON-FAILING — a Waiting node never
	// causes its dependents to be Skipped, never trips fail-fast, and is never
	// counted as terminal/skipped. It drives Execute to return ErrSuspended at
	// the level barrier (the run is not done); re-entering Execute on resume
	// re-runs the node, which re-parks (or wakes) — so Waiting is treated as
	// runnable, like Pending, not done. "Suspend is a crash you chose." (M10.)
	Waiting NodeStatus = "waiting"
	// Bypassed indicates a not-taken branch of a ChoiceNode: the node did not run
	// because the routing decision activated a different branch, NOT because an
	// upstream it needed failed. Bypassed is TERMINAL (never runs, never
	// re-armed) and is distinct from Skipped on purpose — Skipped preserves the
	// failure-diagnostics meaning "an upstream you needed failed/was-skipped"
	// (DEC-CHUNK3), so overloading it would corrupt that signal. A node blocked
	// solely by Bypassed dep(s) is Bypassed; a node blocked by a Bypassed dep
	// that ALSO has a surviving taken (resolved) ancestor is Skipped, not
	// Bypassed (the taken path wins — DEC-M11-P41-DIAMOND). (M11.)
	Bypassed NodeStatus = "bypassed"
	// Compensated indicates a node that Completed successfully and whose effect has
	// since been durably UNDONE by its compensating action during a saga rollback
	// (M12). It is TERMINAL: a compensated node never re-runs. It is reached ONLY
	// from Completed — the rollback drive runs the compensation of each Completed
	// compensable node in reverse-topological order and marks it Compensated on
	// success. Distinct from Failed (the node's own action never failed) and from
	// Skipped/Bypassed (those never ran at all); a Bypassed/Skipped/Waiting/never-run
	// node is never compensated. (M12.)
	Compensated NodeStatus = "compensated"
	// CompensationFailed indicates a Completed node whose compensating action was
	// attempted during a saga rollback and FAILED (after honoring RetryCount). It is
	// TERMINAL. It is the honest counterpart of Compensated: best-effort rollback
	// records each attempted node as Compensated (undo succeeded) or CompensationFailed
	// (undo failed), and the aggregate *SagaError enumerates both. A saga that
	// half-rolls-back must SAY so — a CompensationFailed node means its effect is NOT
	// undone and needs operator attention. Durable (FB wire 8) so a crash after a
	// failed compensation survives into ph48's resume without a silent re-attempt-clean.
	// (M12 ph47.)
	CompensationFailed NodeStatus = "compensation_failed"
)

// Node is a unit of work in a workflow: an action, the nodes that must resolve before
// it may run, and the execution policy the executor applies around it (retries,
// timeout, error handling, compensation).
//
// OUTSIDE THIS PACKAGE A *Node IS AN OPAQUE HANDLE (M23 SEAL-01/02). Every field is
// unexported and the six post-Build mutators are deleted, so a consumer holding a
// *Node from GetNode or GetLevels can read its name (Name) and its edges
// (GetDependencies) and can change nothing THROUGH THAT HANDLE.
//
// THE EDGE SET IS SEALED TOO, as of SEAL-06 (T6): (*DAG).AddDependency and
// (*Workflow).AddDependency are gone from the public surface — measured with `go doc
// DAG` and `go doc Workflow`, which report neither — so no out-of-package caller can
// add an edge to an already-validated graph.
//
// 🔴 THIS PARAGRAPH HAS NOW BEEN WRONG IN BOTH DIRECTIONS. It first implied the edge
// set was sealed when it was not; it was corrected to assert the opposite, with a
// clause deferring to work that had not landed yet; and that correction then went
// stale the moment SEAL-06 did land, leaving the canonical doc block UNDERSTATING a
// shipped guarantee — which is worse than a stale cross-reference, because a
// contributor may re-derive a defence that already exists or decline to rely on one
// that does. A deferral clause outliving the work it defers to is 118-QA-01's shape.
// A correction is not exempt from the failure it corrects.
//
// The prior wording is DESCRIBED above rather than QUOTED, deliberately. Quoting it
// verbatim left the retired phrases greppable in a file whose stale-claim sweep is a
// phrase sweep — and it produced a false hit for a reader checking this exact block
// within a day of the fix. A history note should not be indistinguishable from the
// claim it retires.
//
// WHAT REMAINS TRUE, and it is about Validate rather than about the seal: the
// reconvergence check runs ONLY in build(). Validate() does not call
// validateReconvergence (measured: zero references in its body), so an IN-PACKAGE
// caller adding an edge after build() can still create a violation build() would have
// refused. The seal closes that path from outside; it does not make Validate()
// equivalent to build().
//
// WHICH FIELDS ARE WRITTEN AFTER THE MINT — from an assignment sweep over non-test
// pkg/ and internal/ with each site's receiver type resolved, because grep alone
// cannot tell a *Node write from a *NodeBuilder one. The sweep sees direct
// assignments only: it would not see a write through reflection or a whole-struct
// copy, and neither exists for Node (checked separately). What it found:
//   - name, action, suspendable: written ONLY by NewNode/NewNodeWithCapacity. There is
//     no assignment to any of them anywhere else, which is what lets suspendable be
//     derived once and trusted forever (see its own comment).
//   - retryCount, timeout, continueOnError, compensation: written once by build(),
//     immediately after the mint, from the NodeBuilder's values.
//   - dependsOn: the one field mutated later. build() wires the declared edges,
//     validateReconvergence appends the DEC-M11-DEPMODEL merge<-choice edges, and
//     (*DAG).addDependency appends by name — the last WAS an exported writer on an
//     already-validated graph, and SEAL-06 closed it: it is unexported now, so that
//     path exists only in-package.
type Node struct {
	// name identifies the node uniquely within its DAG. It keys the node's status in
	// WorkflowData and participates in graph identity across a resume
	// (checkGraphIdentity). Read through Name.
	name string

	// action is the work this node performs. suspendable is derived from it exactly
	// once, at the mint.
	action Action

	// dependsOn are the nodes that must reach a resolved status before this node may
	// run. The executor reads this slice directly, in-package, on the hot path;
	// GetDependencies hands external callers a copy so the edge set cannot be edited
	// through a read accessor (M23 BYPASS-05).
	dependsOn []*Node

	// retryCount is how many times the executor re-invokes a failed action before
	// giving up. NEVER NEGATIVE: build() clamps it with max(0, …), which is what makes
	// the unconditional assignment there safe.
	//
	// The clamp is the guarantee, and it replaced a FALSE one. This comment previously
	// said the value was safe because "the consumption site guards with > 0" — singular.
	// There are FOUR readers: node.go's execute and tracing.go guard; saga_rollback.go's
	// compensation loop is `for attempt := 0; attempt <= n.retryCount` and does NOT.
	// A -1 made that loop skip its body, which read as a successful rollback and stamped
	// the node Compensated without ever invoking its compensation (blocker 117-F1).
	// Guarded by TestSagaCompensation_NegativeRetryCountStillCompensates, which asserts
	// the INVOCATION COUNT — the status is identical either side of the defect.
	retryCount int

	// timeout bounds a single execution of the action; zero means unbounded. A park
	// bypasses it entirely — see execute.
	timeout time.Duration

	// continueOnError, when true, changes how a failure of this node is
	// handled by the executor: instead of failing the workflow (the default
	// fail-fast behavior), the node is marked Failed, its siblings and the
	// rest of the DAG continue, and dependents may inspect this node's Failed
	// status and branch on it. Default false preserves fail-fast.
	continueOnError bool

	// compensation is the optional compensating action for a saga (M12). When set,
	// a run that fails with a hard error rolls back: after this node has Completed,
	// its compensation is invoked (reverse-topological order, fresh context) to
	// durably undo the node's effect, and the node is then marked Compensated. Nil
	// means nothing to undo — the node is a rollback no-op. A compensation MUST be
	// idempotent: it may be re-invoked after a crash mid-rollback (at-least-once,
	// M12 ph48), and the executor passes it a stable IdempotencyKey handle.
	compensation Action

	// suspendable is the node-indexed park capability (M23 SEAL-09,
	// DEC-M23-PARK-CAPABILITY as AMENDED 2026-07-27). True iff this node's action
	// is one of the declared suspension primitives.
	//
	// WHY A FIELD AND NOT AN EXECUTE-TIME TYPE ASSERTION. The TLA+ model indexes the
	// park capability BY NODE: M10DurableExecutor.tla:52 declares Suspendable as a
	// CONSTANT that is a subset of Nodes, the Suspend(n) guard is n \in Suspendable,
	// and WaitingSound:986-988 is status[n]="waiting" => n \in Suspendable. No spec
	// anywhere references an action or a type. So a node-indexed field makes the Go
	// representation literally the model's, which is what makes the Go<->model
	// correspondence defensible (VER-04).
	//
	// HONESTY CLAUSE, and it must not be softened: after SEAL-01 unexported
	// Node.action, the execute-time assertion would ALSO have been safe - the action
	// can no longer be swapped from outside. This field is REPRESENTATION FIDELITY to
	// the model, NOT a repair of live breakage. (D-08.)
	//
	// WHY DERIVING IT AT MINT IS NOT THE DEFECT CLASS THIS PHASE REMOVES. The class
	// is behaviour decided from an action's dynamic type AT EXECUTE TIME, when the
	// action could have been swapped. This is decided ONCE at construction, in-package,
	// from an action the package itself just built, then frozen. That is the hasFanOut
	// precedent verbatim: (*DAG).addNode sets hasFanOut for the same reason, at the same
	// moment, from the same just-built action.
	//
	// RUN-CONSTANCY (D-07), the load-bearing obligation: NEVER persisted. It is
	// deterministically re-derived from the rebuilt DAG on every resume, so the set is
	// identical across Crash/Recover exactly as the model's CONSTANT requires. Any
	// future mechanism that resolves it from RUNTIME state breaks this silently.
	// Guarded by TestSuspendable_RunConstantAcrossRebuild.
	suspendable bool
}

// newNode creates a new node with the given name and action
func newNode(name string, action Action) *Node {
	return &Node{
		name:        name,
		action:      action,
		dependsOn:   make([]*Node, 0, 4), // Pre-allocate with small capacity
		retryCount:  0,
		suspendable: isSuspendableAction(action),
	}
}

// newNodeWithCapacity creates a new node with capacity hints to reduce allocations
// This is useful when the approximate number of dependencies is known in advance.
func newNodeWithCapacity(name string, action Action, dependencyCapacity int) *Node {
	return &Node{
		name:        name,
		action:      action,
		dependsOn:   make([]*Node, 0, dependencyCapacity),
		retryCount:  0,
		suspendable: isSuspendableAction(action),
	}
}

// execute runs this node's action and records the resulting status in data.
//
// TWO POLICIES, not one. A declared suspension node (n.suspendable) runs BARE —
// neither the timeout nor the retry wrapper is applied; the arm below states why.
// Every other node runs under this node's configured timeout and retry policy.
//
// A returned error is not necessarily a failure: on a park this returns ErrSuspended
// with the node marked Waiting, which is a SUCCESS arm the executor turns into a
// checkpoint flush. Only the Failed arms below signal actual failure.
//
// Unexported by M23 SEAL-01 (BYPASS-03) — as (*Node).Execute it was an exported entry
// point that let a caller invoke one node's action outside the executor.
func (n *Node) execute(ctx context.Context, data *WorkflowData) error {
	// Mark node as running
	data.SetNodeStatus(n.name, Running)

	// M24 AUD-019 (DEC-M24-MEDIATION): hand the action a SEALED per-node view unless it
	// is engine machinery (choice/timer/signal). Through the sealed view the action can
	// read freely and write consumer data + its own output, but any attempt to forge the
	// engine journal (another node's status/output, run-level saga/wait state) is refused
	// and recorded; sealFault() below turns that into a node failure. The executor's own
	// SetNodeStatus calls stay on the unsealed `data`. Merge/fan-out wrappers are NOT
	// trusted, so the sealed view propagates down to the consumer user action they run.
	actionData := data
	if !isEngineTrusted(n.action) {
		sealed := acquireSealedView(data, n.name)
		defer releaseSealedView(sealed)
		actionData = sealed
	}

	// Declared suspension node: it may PARK (return ErrSuspended). A park
	// bypasses the timeout and retry wrappers entirely — a park is neither a
	// failure to time-out nor a transient error to retry — and the action sees
	// the raw caller context so the park decision is deterministic. The capability
	// is a NODE property (n.suspendable, set once at construction - M23 SEAL-09), so
	// an ordinary action returning ErrSuspended is NOT honored here (handled below).
	// (DEC-M10-mechanism; DEC-M23-PARK-CAPABILITY as amended.)
	if n.suspendable {
		err := n.action.Execute(ctx, actionData)
		if v := actionData.sealFault(); v != nil {
			err = v // a forge attempt fails the node regardless of what the action returned
		}
		switch {
		case err == nil:
			data.SetNodeStatus(n.name, Completed)
			return nil
		case errors.Is(err, ErrSuspended):
			// Park: set the non-terminal Waiting status and propagate the
			// sentinel unchanged (carrying any wake metadata) — a SUCCESS arm,
			// never a Failed-stamp. The executor turns this into the barrier
			// drain → checkpoint flush → ErrSuspended return.
			data.SetNodeStatus(n.name, Waiting)
			return err
		default:
			// A declared suspension node can still fail for a real reason.
			data.SetNodeStatus(n.name, Failed)
			return fmt.Errorf("node %s execution failed: %w", n.name, err)
		}
	}

	// Create timeout context if needed
	var execCtx context.Context
	var cancel context.CancelFunc

	if n.timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, n.timeout)
		defer cancel()
	} else {
		execCtx = ctx
	}

	// Execute with retries
	var err error
	if n.retryCount > 0 {
		retryAction := NewRetryableAction(n.action, n.retryCount, time.Second)
		err = retryAction.Execute(execCtx, actionData)
	} else {
		err = n.action.Execute(execCtx, actionData)
	}
	if v := actionData.sealFault(); v != nil {
		err = v // M24: a forge attempt fails the node regardless of the action's own return
	}

	// Update node status based on result
	if err != nil {
		// An ORDINARY action returning ErrSuspended is a misuse: suspension is
		// confined to declared suspension node types so the topology stays static.
		// Fail loudly AND do not let the sentinel escape (no %w of err here) — if
		// it did, errors.Is would falsely park the run on a node the executor and
		// TLA model never treat as waiting-capable.
		if errors.Is(err, ErrSuspended) {
			data.SetNodeStatus(n.name, Failed)
			return fmt.Errorf("node %s returned ErrSuspended but is not a declared suspension node", n.name)
		}
		data.SetNodeStatus(n.name, Failed)
		return fmt.Errorf("node %s execution failed: %w", n.name, err)
	}

	data.SetNodeStatus(n.name, Completed)
	return nil
}

// Name reports the node's name. The name is fixed at the mint and never written
// afterwards (the struct comment enumerates the writers), so it is a stable identity
// a caller may hold across a run — and the one checkGraphIdentity compares on resume.
func (n *Node) Name() string { return n.name }

// GetDependencies returns a DEFENSIVE COPY of this node's dependencies.
//
// It used to return n.dependsOn directly — the live slice header — so a caller
// could reorder, truncate or overwrite an element and edit the graph's edge set
// THROUGH a read accessor (M23 BYPASS-05). The elements are *Node handles whose
// own fields are unexported, so copying the header is sufficient: the copy cannot
// reach the graph, and reaching a *Node through it grants no mutation.
//
// RETURNS NIL, NOT AN EMPTY SLICE, for a node with no dependencies. That is an
// undocumented behaviour change from the pre-seal accessor, which returned the empty
// live slice; len/range are unaffected, but a caller comparing against []*Node{} or
// checking != nil will see the difference.
//
// Cost: one allocation of len(dependsOn) pointers per call, and none at all in that
// nil case. It is off the executor's hot path —
// the executor reads n.dependsOn directly in-package, and this accessor has no
// non-test caller in the repository.
func (n *Node) GetDependencies() []*Node {
	if len(n.dependsOn) == 0 {
		return nil
	}
	out := make([]*Node, len(n.dependsOn))
	copy(out, n.dependsOn)
	return out
}

// HasDependency checks if this node depends on the given node
func (n *Node) HasDependency(nodeName string) bool {
	for _, dep := range n.dependsOn {
		if dep.name == nodeName {
			return true
		}
	}
	return false
}
