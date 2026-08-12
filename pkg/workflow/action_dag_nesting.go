package workflow

import "fmt"

// actionEmbedsCompiledDAG reports whether an action IS a compiled *DAG, or wraps
// one through a transparent Composite/Retryable wrapper. (AUD-011 / C-05 /
// F118-ENG-01.)
//
// (*DAG).Execute has the Action signature, so a whole compiled graph structurally
// satisfies Action and `WithAction(childDAG)` compiles. Executed that way, the
// child runs as an ordinary node action and BYPASSES every guard the sanctioned
// nesting entry (AddSubWorkflow) enforces: the depth ceiling, the ancestor-cycle
// check, and the suspendable-child refusal. A depth-5,000 witness builds and runs.
// The boundary clause already marks *DAG opaque for a verifier/sink; this rejects
// it for ORDINARY nodes at Build so the unsanctioned nesting channel is closed.
//
// Nested composites/retries cannot recurse unboundedly here in practice because
// the run-safety walk that runs just before this at build() already fails closed
// past maxBoundaryActionNestingDepth; the same cap is applied defensively.
func actionEmbedsCompiledDAG(a Action) (bool, int) {
	return actionEmbedsCompiledDAGAt(a, 0)
}

func actionEmbedsCompiledDAGAt(a Action, depth int) (bool, int) {
	if depth > maxBoundaryActionNestingDepth {
		return false, depth
	}
	switch v := a.(type) {
	case *DAG:
		return true, depth
	case *CompositeAction:
		if v == nil {
			return false, depth
		}
		for _, inner := range v.actions {
			if embeds, at := actionEmbedsCompiledDAGAt(inner, depth+1); embeds {
				return true, at
			}
		}
	case *RetryableAction:
		if v == nil {
			return false, depth
		}
		return actionEmbedsCompiledDAGAt(v.action, depth+1)
	case *mergeAction:
		// A MergeNode's user action (merge.WithAction(dag)) runs under the same
		// executor path, so a *DAG smuggled there nests exactly as a plain action would.
		if v == nil || v.userAction == nil {
			return false, depth
		}
		return actionEmbedsCompiledDAGAt(v.userAction, depth+1)
	}
	return false, depth
}

// rejectCompiledDAGAction returns a validation error if the action smuggles a
// compiled *DAG in through WithAction, pointing the caller at the sanctioned entry.
func rejectCompiledDAGAction(nodeName string, a Action) error {
	if embeds, at := actionEmbedsCompiledDAG(a); embeds {
		return fmt.Errorf("%w: node %s uses a compiled *DAG as its action (%s) — a DAG used as an action "+
			"bypasses AddSubWorkflow's depth/cycle/suspendable-child guards; use AddSubWorkflow "+
			"(or AddSubWorkflowParked/AddSubWorkflowQueued) to nest a child graph", ErrValidation, nodeName, embedWhere(at))
	}
	return nil
}

// rejectCompiledDAGCompensation is the compensation-slot counterpart. A *DAG passed
// to WithCompensation runs under (*DAG).Execute during rollback exactly as an action
// would, so it bypasses the identical AddSubWorkflow guards and must be refused too.
func rejectCompiledDAGCompensation(nodeName string, a Action) error {
	if embeds, at := actionEmbedsCompiledDAG(a); embeds {
		return fmt.Errorf("%w: node %s uses a compiled *DAG as its compensation (%s) — a DAG used as a "+
			"compensation bypasses AddSubWorkflow's depth/cycle/suspendable-child guards; a compensation "+
			"must be a plain Action or func(ctx, *WorkflowData) error", ErrValidation, nodeName, embedWhere(at))
	}
	return nil
}

// embedWhere renders where in an action's wrapper chain a compiled *DAG was found.
func embedWhere(at int) string {
	if at > 0 {
		return fmt.Sprintf("nested %d wrapper(s) deep", at)
	}
	return "directly"
}
