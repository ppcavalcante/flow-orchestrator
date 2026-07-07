package workflow

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DefaultMaxConcurrency is the default per-level concurrency limit used when
// ExecutionConfig.MaxConcurrency is unset or non-positive. Concurrency is
// always bounded — there is no "unbounded" mode — because an unbounded level
// would spawn one goroutine per node, a goroutine-explosion / DoS hazard on
// large levels.
const DefaultMaxConcurrency = 16

// ExecutionConfig holds configuration options for DAG execution.
type ExecutionConfig struct {
	MaxConcurrency int // Maximum number of nodes executed concurrently per level (<=0 -> DefaultMaxConcurrency)

	// TracerProvider is the OpenTelemetry trace provider used to emit a span
	// per executed node (and a parent span per workflow run). The zero value
	// (nil) disables tracing: the executor resolves a noop tracer and the hot
	// path stays byte-identical to an untraced run. This is API-only — the host
	// owns the SDK/exporter; the library never imports go.opentelemetry.io/otel/sdk
	// (DEC-M6-otel-api-only parity, DEC-CHUNK5).
	TracerProvider trace.TracerProvider
}

// DefaultConfig returns the default execution configuration.
func DefaultConfig() ExecutionConfig {
	return ExecutionConfig{
		MaxConcurrency: DefaultMaxConcurrency,
	}
}

// executeNodesInLevel executes all nodes in a level in parallel, bounded by
// maxConcurrency (a non-positive value coerces to DefaultMaxConcurrency).
//
// It returns the fail-fast (non-continue-on-error) failures observed in this
// level as a slice of NodeError. When more than one node fails concurrently
// before cancellation takes effect, EVERY such failure is captured (not just
// the first). On the first fail-fast failure the level context is cancelled to
// stop sibling goroutines; siblings already in flight may still complete and
// contribute their own NodeError, which is why the result is a slice.
//
// Continue-on-error failures are NOT returned here: the node is marked Failed
// (by n.Execute) and the workflow proceeds; dependents observe the status.
// (DEC-M7-failure / DEC-P21-depguard.)
//
// tracer is the resolved (never-nil) OpenTelemetry tracer: each executed node
// is wrapped in a span named after the node, a child of the run's parent span
// carried in ctx. When tracing is off the tracer is the noop tracer, so the
// span machinery is zero-cost (DEC-CHUNK5).
//
// It additionally returns the names of nodes that PARKED (returned ErrSuspended)
// this level. A park is NOT a failure: the node is left Waiting (by n.Execute),
// its siblings are NOT cancelled — the level drains to the barrier (Model A,
// whole-run suspend) — and the park is reported separately so the level loop can
// flush the durable checkpoint and return ErrSuspended. (M10 / DEC-M10.)
func executeNodesInLevel(ctx context.Context, level []*Node, data *WorkflowData, maxConcurrency int, tracer trace.Tracer) (failures []NodeError, parkedNodes []string) {
	// Create a cancellable context so we can stop siblings on first failure
	levelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered failure channel (one slot per node is the upper bound) and wait
	// group. Each goroutine sends at most one NodeError; the channel is drained
	// after wg.Wait so a full buffer never blocks a sender. A parallel parkChan
	// carries the names of nodes that parked — a park drains to the barrier with
	// no cancel, so its slot is independent of the failure path.
	failChan := make(chan NodeError, len(level))
	parkChan := make(chan string, len(level))
	var wg sync.WaitGroup

	// Semaphore to limit concurrency. A non-positive limit coerces to the
	// bounded default; concurrency is never unbounded.
	if maxConcurrency <= 0 {
		maxConcurrency = DefaultMaxConcurrency
	}
	semaphore := make(chan struct{}, maxConcurrency)

	// Execute each node in this level in parallel
	for _, node := range level {
		// Skip nodes that already reached a terminal state (Completed by a prior
		// pass, or Failed/Skipped — e.g. marked Skipped by the cross-level sweep).
		if status, _ := data.GetNodeStatus(node.Name); isTerminalStatus(status) {
			continue
		}

		// Two-part dependency check (DEC-P21-depguard + DEC-M11-STATUS-CAUSE):
		//
		//  (1) LAUNCH decision — a cheap boolean: do ALL deps resolve? A dep
		//      resolves iff it Completed OR it is a continue-on-error node that
		//      Failed. This keeps the early break on the first unresolved dep, so
		//      the launch hot path costs exactly what it did pre-M11.
		//  (2) BLOCKED classification — only when NOT all deps resolve, assign a
		//      cause-aware terminal status computed from the FULL dep set (a
		//      second scan, off the launch hot path). The early break in (1)
		//      cannot see a taken sibling that appears AFTER the first unresolved
		//      dep, so the diamond rule (D-03) needs the full scan here — see
		//      classifyBlockedStatus. Both parts consult the SAME predicates
		//      (depResolved / isSkipCause), so launch and skip/bypass cannot drift.
		role := dependentRole(node)
		dependenciesComplete := true
		for _, dep := range node.DependsOn {
			depStatus, _ := data.GetNodeStatus(dep.Name)
			if depResolved(dep, depStatus, role) {
				continue
			}
			dependenciesComplete = false
			break
		}

		if !dependenciesComplete {
			// Blocked this pass: assign the terminal status implied by the cause
			// across the whole dep set — Skipped (a failure/skip cascade, or a
			// bypassed dep with a surviving taken ancestor) or Bypassed (a purely
			// not-taken branch interior). assign=false means "not decidable yet"
			// (a Pending/Running/Waiting dep) — leave the node for a later pass.
			if status, assign := classifyBlockedStatus(node, data, role); assign {
				data.SetNodeStatus(node.Name, status)
			}
			continue
		}

		// OR-join fire-vs-bypass (M11 ph42, DEC-M11-P42-SEAM). A MergeNode is
		// launch-eligible once ALL its predecessors resolved — and a Bypassed tail
		// resolves for a merge (depResolved). So "all deps resolved" alone does NOT
		// mean fire: it could be all-bypassed. Decide here, on the launch path (the
		// one place the OR-join must touch beyond classifyBlockedStatus): fire iff
		// >=1 TAKEN branch-tail (Completed or coe-Failed). The count ranges over the
		// merge's RECORDED From tail-set (mergeAction.tails), NOT its full DependsOn —
		// so neither the structural always-Completed Choice-dep (DEPMODEL edge) nor
		// any extra DependsOn can inflate the count (anti-vacuity, red-team MAJOR-1;
		// code-review 42-F1/adversarial 42-AF2). Zero taken (every join tail bypassed)
		// -> the merge is itself Bypassed and never runs (composes downward, MH-2).
		// Role-gated: AND nodes pay nothing.
		if ma, isMerge := node.Action.(*mergeAction); isMerge { // == role==mergeDependent
			tailSet := make(map[string]bool, len(ma.tails))
			for _, t := range ma.tails {
				tailSet[t] = true
			}
			takenTails := 0
			for _, dep := range node.DependsOn {
				if !tailSet[dep.Name] {
					continue // not a From join tail (Choice-dep or an extra DependsOn)
				}
				depStatus, _ := data.GetNodeStatus(dep.Name)
				// "taken" = the AND resolution (Completed || coe-Failed); a Bypassed
				// tail is satisfied, not taken.
				if depResolved(dep, depStatus, andDependent) {
					takenTails++
				}
			}
			if takenTails == 0 {
				data.SetNodeStatus(node.Name, Bypassed)
				continue
			}
		}

		// Launch goroutine for node execution
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Open a span for this node, named after the node for a readable
			// trace waterfall. The span is a child of the workflow parent span
			// carried in levelCtx; spanCtx flows into n.Execute so any host
			// instrumentation inside the action nests correctly. When tracing
			// is off the tracer is noop and this is zero-cost. The span is a
			// DAG-run construct (Node.Execute stays untraced when called
			// standalone). (DEC-CHUNK5.)
			spanCtx, span := tracer.Start(levelCtx, n.Name)
			defer span.End()

			// Execute the node with the (cancellable, span-carrying) context
			err := n.Execute(spanCtx, data)

			// Annotate the span from engine-controlled state only: the final
			// node status and the configured retry count. On failure, record
			// the action's own error (its contract) and set the span status to
			// Error. No WorkflowData values/keys/paths are read into the span —
			// the chunk-2 no-leak discipline extends to traces.
			status, _ := data.GetNodeStatus(n.Name)
			span.SetAttributes(nodeSpanAttributes(n, status)...)

			if err != nil {
				// A park is NOT a failure (Model A, whole-run suspend). The node
				// is already Waiting (set by n.Execute); record it as parked, do
				// NOT cancel siblings (the level drains to the barrier), and do
				// NOT record a NodeError. The level loop turns a parked level into
				// the checkpoint flush + ErrSuspended return. Checked before the
				// span is marked Error — a park is a clean outcome, not an error.
				if errors.Is(err, ErrSuspended) {
					parkChan <- n.Name
					return
				}
				span.RecordError(err)
				span.SetStatus(codes.Error, "node execution failed")
				if n.ContinueOnError {
					// Continue-on-error: the node is already marked Failed by
					// n.Execute. Do NOT cancel siblings and do NOT record a
					// fail-fast failure — the workflow proceeds and dependents
					// observe the Failed status. (DEC-M7-failure)
					return
				}
				// Fail-fast (default): cancel sibling goroutines and record the
				// failure. The action's own error (wrapped by n.Execute with the
				// node name for context) is carried as-is — no WorkflowData
				// values or engine state are added here.
				cancel()
				failChan <- NodeError{NodeName: n.Name, Err: err}
			}
		}(node)
	}

	// Wait for all nodes to complete, then drain every recorded failure and
	// every parked node. Both channels are closed after the barrier so a full
	// buffer never blocks a sender.
	wg.Wait()
	close(failChan)
	close(parkChan)

	for ne := range failChan {
		failures = append(failures, ne)
	}
	for name := range parkChan {
		parkedNodes = append(parkedNodes, name)
	}
	return failures, parkedNodes
}

// depRole classifies a DEPENDENT node's join semantics — how its own dependency
// set gates its launch and terminal status. Every node today is andDependent
// (strict AND: needs all deps resolved). Phase 42 adds MergeNode (OR-join),
// which is mergeDependent; until then dependentRole is a total function to
// andDependent and the merge arms below are present-but-unreached.
type depRole int

const (
	andDependent depRole = iota
	// mergeDependent is the OR-join role activated by MergeNode in phase 42.
	mergeDependent
)

// dependentRole reports the join role of a node as a dependent: a MergeNode
// OR-joins (mergeDependent), every other node is strict-AND (andDependent). The
// role is carried by the node's action type — a *mergeAction marks a MergeNode
// (the marker survives even a user join action, see mergeAction). (M11 ph42.)
func dependentRole(node *Node) depRole {
	if _, isMerge := node.Action.(*mergeAction); isMerge {
		return mergeDependent
	}
	return andDependent
}

// depResolved reports whether a dependency in the given status no longer blocks
// its dependents (DEC-P21-depguard). A dep resolves iff it Completed, OR it is a
// continue-on-error node that Failed (its failure is tolerated and observable
// via status). This is the single source of truth for dependency resolution —
// both the launch decision and the skip/bypass rule consult it, so the two
// cannot drift.
func depResolved(dep *Node, depStatus NodeStatus, role depRole) bool {
	if depStatus == Completed {
		return true
	}
	if dep.ContinueOnError && depStatus == Failed {
		return true
	}
	if role == mergeDependent {
		// OR-join (M11 ph42, DEC-M11-P42-SEAM): a Bypassed predecessor is SATISFIED
		// for a merge — a not-taken branch does not block the join. (Completed /
		// coe-Failed are the "taken" cases, already resolved above.) The fire-vs-
		// bypass decision over the taken count is made on the launch-eligibility
		// path in executeNodesInLevel, not here.
		return depStatus == Bypassed
	}
	return false
}

// isSkipCause reports whether a non-resolving dependency status is a FAILURE
// skip-cause for a dependent (DEC-CHUNK3-status, S1): a Failed dependency (the
// continue-on-error case is already resolved by depResolved, so a Failed status
// reaching here is a non-coe failure) or an already-Skipped dependency. A
// Bypassed dep is NON-resolving but NOT a failure skip-cause — it is a distinct
// bypass cause handled by classifyBlockedStatus. A Pending/Running/Waiting dep
// is non-resolving but NOT terminal — "not reached yet", not a skip cause.
//
// Role-independent (M11 ph42, DEC-M11-P42-SEAM / Finding B): a merge's Failed/
// Skipped predecessor is ALSO a skip-cause. Because a Choice takes exactly one
// branch and the strict validator forbids cross-Choice merges, a Failed merge
// predecessor is always the SOLE taken branch failing → fail-fast (INV-01); the
// OR "tolerance" is Bypassed-only, already handled by depResolved. (A merge arm
// returning false here would strand the merge Pending forever.)
func isSkipCause(depStatus NodeStatus) bool {
	return depStatus == Failed || depStatus == Skipped
}

// classifyBlockedStatus computes the terminal status of a node that could not
// launch this pass, from the cause across its FULL dependency set — a single
// scan with NO early break, because the diamond rule (D-03 / DEC-M11-P41-DIAMOND)
// must see a taken sibling even when a Bypassed dep appears first. It returns
// (status, true) to assign, or ("", false) to leave the node for a later pass
// (a dep is not-terminal yet, so the outcome is not decidable). It consults the
// SAME predicates the launch decision uses (depResolved / isSkipCause), so the
// run and skip/bypass rules cannot drift.
//
// Rule order (DEC-M11-STATUS-CAUSE + DEC-M11-P41-DIAMOND + D-03a):
//  1. any failure skip-cause dep (non-coe Failed / Skipped) -> Skipped   (a failure cascade dominates)
//  2. any non-terminal dep (Pending / Running / Waiting)    -> revisit   (outcome not yet decidable — D-03a)
//  3. any Bypassed dep AND any resolved (taken) dep         -> Skipped   (surviving taken ancestor wins)
//  4. any Bypassed dep                                      -> Bypassed  (pure not-taken branch interior)
func classifyBlockedStatus(node *Node, data *WorkflowData, role depRole) (NodeStatus, bool) {
	var sawFailCause, sawNonTerminal, sawBypassed, sawResolved bool
	for _, dep := range node.DependsOn {
		depStatus, _ := data.GetNodeStatus(dep.Name)
		switch {
		case depResolved(dep, depStatus, role):
			sawResolved = true // a taken path that ran (Completed or coe-Failed)
		case isSkipCause(depStatus):
			sawFailCause = true
		case depStatus == Bypassed:
			sawBypassed = true
		default:
			// Pending / Running / Waiting: non-resolving but not a terminal cause.
			// A Waiting (suspended) sibling must NOT be read as a terminal cause
			// (D-03a) — the node waits for a later pass, staying Pending.
			sawNonTerminal = true
		}
	}
	switch {
	case sawFailCause:
		return Skipped, true
	case sawNonTerminal:
		return "", false
	case sawBypassed && sawResolved:
		return Skipped, true
	case sawBypassed:
		return Bypassed, true
	default:
		return "", false
	}
}

// isTerminalStatus reports whether a node has reached a terminal state and will
// not transition further within a run.
func isTerminalStatus(status NodeStatus) bool {
	return status == Completed || status == Failed || status == Skipped || status == Bypassed || status == Compensated || status == CompensationFailed
}
