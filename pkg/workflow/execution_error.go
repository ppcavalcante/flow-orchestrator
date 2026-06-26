package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// NodeError pairs a failed node's name with the error its action returned.
//
// The Err field is the error produced by the node's own action (wrapped by the
// node/executor with the node name for context). It is NOT augmented with any
// WorkflowData values, file paths, or internal engine state by the executor —
// only what the action itself returned flows through.
type NodeError struct {
	// NodeName is the name of the node that failed.
	NodeName string

	// Err is the error the node's action returned.
	Err error
}

// Error renders a per-node summary: the node name and its error string. Like the
// aggregate ExecutionError, this adds nothing beyond the node name and the
// action's own error — it does not read from WorkflowData or engine state.
func (e *NodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("node %q failed", e.NodeName)
	}
	return fmt.Sprintf("node %q: %s", e.NodeName, e.Err.Error())
}

// Unwrap exposes the underlying action error so errors.Is / errors.As reach the
// action's own error (and any sentinel it wraps, e.g. ErrExecutionFailed).
func (e *NodeError) Unwrap() error {
	return e.Err
}

// ExecutionError aggregates the failures from a single DAG execution. When more
// than one node fails (e.g. several nodes in a level fail concurrently before
// fail-fast cancellation takes effect), every failure is captured here rather
// than only the first.
//
// ExecutionError is the error type returned by DAG.Execute (and surfaced by
// Workflow.Execute) when execution fails; extract it with errors.As:
//
//	var execErr *workflow.ExecutionError
//	if errors.As(err, &execErr) {
//	    for _, ne := range execErr.FailedNodes {
//	        // ne.NodeName, ne.Err
//	    }
//	}
//
// Because Unwrap returns the per-node errors, errors.Is also reaches through to
// any sentinel an action wrapped (e.g. errors.Is(err, ErrExecutionFailed)).
type ExecutionError struct {
	// FailedNodes holds one entry per failed node, sorted by NodeName for a
	// deterministic Error() string and stable iteration.
	FailedNodes []NodeError
}

// Error renders a deterministic summary: the failure count followed by each
// failed node's name and its error string. It deliberately contains ONLY node
// names and the errors the actions themselves returned — never WorkflowData
// values, inputs, file paths, or internal engine state.
func (e *ExecutionError) Error() string {
	switch len(e.FailedNodes) {
	case 0:
		// Defensive: an ExecutionError should never be constructed empty, but
		// never render a misleading "0 nodes failed" as a success-looking string.
		return "workflow execution failed"
	case 1:
		return fmt.Sprintf("workflow execution failed: 1 node failed: %s",
			e.FailedNodes[0].Error())
	default:
		parts := make([]string, len(e.FailedNodes))
		for i, ne := range e.FailedNodes {
			parts[i] = ne.Error()
		}
		return fmt.Sprintf("workflow execution failed: %d nodes failed: %s",
			len(e.FailedNodes), strings.Join(parts, "; "))
	}
}

// Unwrap returns the underlying per-node errors (Go 1.20+ multi-error unwrap),
// so errors.Is and errors.As reach through the aggregate to each node's error
// and any sentinel it wraps.
func (e *ExecutionError) Unwrap() []error {
	errs := make([]error, len(e.FailedNodes))
	for i := range e.FailedNodes {
		errs[i] = &e.FailedNodes[i]
	}
	return errs
}

// newExecutionError builds an *ExecutionError from collected node failures,
// sorting them by NodeName for determinism. It returns nil when there are no
// failures, so callers can `return newExecutionError(failures)` and naturally
// yield a nil error on the happy path.
func newExecutionError(failures []NodeError) *ExecutionError {
	if len(failures) == 0 {
		return nil
	}
	sorted := make([]NodeError, len(failures))
	copy(sorted, failures)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].NodeName < sorted[j].NodeName
	})
	return &ExecutionError{FailedNodes: sorted}
}
