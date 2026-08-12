package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// F117-T6-01: NewRetryableAction accepted a negative maxRetries and returned SUCCESS
// having never invoked the wrapped action.
//
// Execute's loop is `for attempt := 0; attempt <= r.maxRetries` and the function ends
// `return lastErr`. With -1 the body never runs, lastErr stays nil, and nil reads as a
// successful execution. In a durable engine the result is then journaled — the lie is
// persisted. Blocker 117-F1's exact shape one constructor over.
//
// THIS TEST IS DELIBERATELY IN package workflow_test — OUTSIDE the package — because
// externally-reachable is the whole finding. Both in-package callers already guarded
// (node.go behind `retryCount > 0`, fanout.go's policy behind `count <= 0`), so no
// production path ever reached this; only a consumer could. An in-package test would
// prove the clamp works but not that it was ever needed.
//
// WHY THE INVOCATION COUNT AND NOT THE ERROR. Measured both sides of the clamp:
//
//	maxRetries    broken (no clamp)          fixed (clamped)
//	 0            1 invocation, err=<nil>    1 invocation, err=<nil>
//	-1            0 invocations, err=<nil>   1 invocation, err=<nil>
//	-7            0 invocations, err=<nil>   1 invocation, err=<nil>
//
// The returned error is nil on BOTH sides, so require.NoError passes over the live
// defect and is vacuous — precisely the trap in 117-F1, where the node was stamped
// Compensated either side. The count is the only signal that discriminates.
func TestRetryableAction_NegativeMaxRetriesStillInvokesAction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRetries int
	}{
		// The control. A count of 1 here is what makes a count of 0 in the negative
		// arms attributable to the clamp rather than to a broken fixture.
		{"zero retries (control)", 0},
		// The defect's input: what the exported API accepts and now clamps.
		{"negative retries", -1},
		// Not -1, so the clamp cannot be a special case on the single value.
		{"very negative retries", -7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invocations := 0
			inner := workflow.ActionFunc(func(context.Context, *workflow.WorkflowData) error {
				invocations++
				return nil
			})

			ra := workflow.NewRetryableAction(inner, tc.maxRetries, 0)
			err := ra.Execute(context.Background(), workflow.NewWorkflowData("retry-clamp"))

			// THE ASSERTION THAT DISCRIMINATES. err is nil either side of the defect.
			require.Equal(t, 1, invocations,
				"the wrapped action must be invoked exactly ONCE with maxRetries=%d (got %d). "+
					"A count of 0 means Execute's `for attempt := 0; attempt <= maxRetries` loop "+
					"skipped its body, left lastErr nil, and RETURNED SUCCESS WITHOUT EVER RUNNING "+
					"THE ACTION — a result a durable run would then journal (F117-T6-01).",
				tc.maxRetries, invocations)
			require.NoError(t, err, "a succeeding action must not surface an error")
		})
	}
}

// The clamp must not have bought the arm above by disabling retry: a negative count
// collapses to "no retry", which still means exactly one attempt on success and exactly
// one on failure — never zero, and never a silent extra attempt.
func TestRetryableAction_ClampedCountRetriesZeroTimesOnFailure(t *testing.T) {
	errBoom := errors.New("boom")
	invocations := 0
	inner := workflow.ActionFunc(func(context.Context, *workflow.WorkflowData) error {
		invocations++
		return errBoom
	})

	ra := workflow.NewRetryableAction(inner, -1, 0)
	err := ra.Execute(context.Background(), workflow.NewWorkflowData("retry-clamp-fail"))

	require.ErrorIs(t, err, errBoom, "a failing action's error must surface, not be swallowed")
	require.Equal(t, 1, invocations,
		"a clamped (negative) count means NO retry: exactly one attempt, not zero and not two (got %d)",
		invocations)
}
