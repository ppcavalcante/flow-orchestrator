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

		// Check dependencies via the shared resolution predicate (DEC-P21-depguard):
		// a dep resolves iff it Completed OR it is a continue-on-error node that
		// Failed. If any dep is NOT resolved AND is a terminal skip-cause (a
		// non-coe Failed dep, or an already-Skipped dep), this node did not run
		// because an upstream it needed failed/was-skipped — mark it Skipped
		// (DEC-CHUNK3-status, S1). Skipped is transitive: a node skipped here
		// makes its own dependents skip on a later level.
		dependenciesComplete := true
		for _, dep := range node.DependsOn {
			depStatus, _ := data.GetNodeStatus(dep.Name)
			if depResolved(dep, depStatus) {
				continue
			}
			dependenciesComplete = false
			if isSkipCause(depStatus) {
				data.SetNodeStatus(node.Name, Skipped)
			}
			break
		}

		if !dependenciesComplete {
			continue
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

// depResolved reports whether a dependency in the given status no longer blocks
// its dependents (DEC-P21-depguard). A dep resolves iff it Completed, OR it is a
// continue-on-error node that Failed (its failure is tolerated and observable
// via status). This is the single source of truth for dependency resolution —
// both the launch decision and the Skipped rule consult it, so the two cannot
// drift.
func depResolved(dep *Node, depStatus NodeStatus) bool {
	if depStatus == Completed {
		return true
	}
	if dep.ContinueOnError && depStatus == Failed {
		return true
	}
	return false
}

// isSkipCause reports whether a non-resolving dependency status is a terminal
// reason to Skip a dependent (DEC-CHUNK3-status, S1): a Failed dependency (the
// continue-on-error case is already resolved by depResolved, so a Failed status
// reaching here is a non-coe failure) or an already-Skipped dependency. A
// Pending/Running dep is non-resolving but NOT terminal — it means "not reached
// yet", which is not a skip cause.
func isSkipCause(depStatus NodeStatus) bool {
	return depStatus == Failed || depStatus == Skipped
}

// isTerminalStatus reports whether a node has reached a terminal state and will
// not transition further within a run.
func isTerminalStatus(status NodeStatus) bool {
	return status == Completed || status == Failed || status == Skipped
}
