package workflow

import (
	"fmt"
	"reflect"
)

// actionRunSafetyReason reports whether executing this action would PANIC the
// executor worker goroutine — killing the host process, because the panic occurs
// where no caller's recover() can reach it — and why. (AUD-001 / F118-ENG-06.)
//
// This is the ordinary-node counterpart to boundaryOpaqueReason's second,
// separately-named ground ("a nil operand or nil transform is refused because THE
// NODE'S ACTION CANNOT RUN"). It deliberately covers ONLY the nil / typed-nil
// forms that dereference at Execute — NOT the completes-on-structure/clock kinds
// (fan-out, merge, timer, sub-workflow, *DAG). Those are opaque for a BOUNDARY's
// verifier/sink but are perfectly valid ORDINARY nodes, so this check must not
// touch them (that is why it is not boundaryOpaqueReason).
//
// The exported, user-constructible action types whose zero/nil operands panic:
//
//	WithAction(nil-typed *CompositeAction/…)            -> nil ptr deref
//	NewCompositeAction(nil) / .Add(nil)                 -> calls nil.Execute
//	NewRetryableAction(nil, …)                          -> calls nil.Execute
//	NewMapAction(…, nil)                                -> calls nil transform
//	NewValidationAction(…, nil, …)                      -> calls nil validator
//	WithAction(ActionFunc(nil))                         -> calls nil func
//
// A top-level typed-nil of ANY nillable action kind is caught by reflect, so a
// nil pointer/func smuggled in through WithAction is rejected regardless of type.
func actionRunSafetyReason(a Action) (string, bool) {
	return actionRunSafetyReasonAt(a, 0)
}

func actionRunSafetyReasonAt(a Action, depth int) (string, bool) {
	// Reuse the boundary walk's cap: the same publicly-composable wrappers
	// (CompositeAction/RetryableAction) nest and can cycle (c.Add(c)); an unbounded
	// walk would exhaust the stack inside build() — a fatal error, not a refusal.
	// Fails closed: past the cap we cannot certify the action is run-safe.
	if depth > maxBoundaryActionNestingDepth {
		return fmt.Sprintf("its wrappers nest more than %d deep, or are cyclic — this check cannot "+
			"distinguish the two, and neither can be certified safe to execute", maxBoundaryActionNestingDepth), true
	}

	// A nil interface, or a typed-nil (the interface holds a type but a nil
	// pointer/func/map/slice/chan value): both panic when Execute dereferences.
	if isNilAction(a) {
		return "a nil (or typed-nil) action cannot run and would panic the executor goroutine, killing the host", true
	}

	switch v := a.(type) {
	case *CompositeAction:
		for i, inner := range v.actions {
			if why, bad := actionRunSafetyReasonAt(inner, depth+1); bad {
				return fmt.Sprintf("its composed action %d %s", i, why), true
			}
		}
	case *RetryableAction:
		// Transparent wrapper: it calls the action it retries.
		if why, bad := actionRunSafetyReasonAt(v.action, depth+1); bad {
			return fmt.Sprintf("the action it retries %s", why), true
		}
	case *MapAction:
		if v.mapFn == nil {
			return "a map action with a nil transform would panic the executor goroutine, killing the host", true
		}
	case *ValidationAction:
		if v.validationFn == nil {
			return "a validation action with a nil validator would panic the executor goroutine, killing the host", true
		}
	case *waitForConditionAction:
		if v.predicate == nil {
			return "a wait-for-condition with a nil predicate would panic the executor goroutine, killing the host", true
		}
	}
	return "", false
}

// interfaceHoldsNil reports whether v is a nil interface OR a typed-nil wrapping a nil
// pointer/func/map/slice/chan/interface value — a call THROUGH which would nil-panic even
// though `v == nil` is false. Used to reject typed-nil exported inputs (a WorkflowStore, an
// Action) with a typed error instead of crashing the host goroutine. (AUD-001/AUD-031/CUR-002.)
func interfaceHoldsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

// isNilAction reports whether a is a nil interface or a typed-nil wrapping a nil
// pointer/func/map/slice/chan/interface value — both of which panic at Execute.
func isNilAction(a Action) bool { return interfaceHoldsNil(a) }
