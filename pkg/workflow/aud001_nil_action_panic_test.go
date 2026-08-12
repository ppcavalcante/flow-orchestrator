package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-001 / C-03 / F118-ENG-06: nil, typed-nil, and nil-operand built-in actions
// panic the executor worker goroutine at Execute, where no caller recover() can
// reach — killing the host. Build must reject them (ErrValidation) so the invalid
// state is unrepresentable rather than a run-time process kill.
func TestAUD001_BuildRejectsPanicProneActions(t *testing.T) {
	cases := []struct {
		name   string
		action Action
	}{
		{"typed-nil *CompositeAction", (*CompositeAction)(nil)},
		{"typed-nil *RetryableAction", (*RetryableAction)(nil)},
		{"typed-nil *MapAction", (*MapAction)(nil)},
		{"typed-nil *ValidationAction", (*ValidationAction)(nil)},
		{"nil ActionFunc", ActionFunc(nil)},
		{"composite with nil member", NewCompositeAction(nil)},
		{"retryable wrapping nil", NewRetryableAction(nil, 1, 0)},
		{"map with nil transform", NewMapAction("in", "out", nil)},
		{"validation with nil validator", NewValidationAction("in", nil, "out", "err")},
		{"nested composite -> retryable(nil)", NewCompositeAction(NewRetryableAction(nil, 1, 0))},
		{"nested composite -> map(nil)", NewCompositeAction(NewMapAction("in", "out", nil))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewWorkflowBuilder()
			b.AddStartNode("n").WithAction(tc.action)

			// Must not panic here, and must return a validation error.
			dag, err := b.Build()
			require.Error(t, err, "Build must reject a panic-prone action, not accept it")
			require.ErrorIs(t, err, ErrValidation)
			require.Nil(t, dag)
		})
	}
}

// Run-safe actions — including an EMPTY composite (a valid no-op) and the
// completes-on-structure kinds' ordinary use — must still build. This guards
// against the check over-reaching into valid ordinary nodes.
func TestAUD001_BuildAcceptsRunSafeActions(t *testing.T) {
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })
	cases := []struct {
		name   string
		action Action
	}{
		{"plain func", noop},
		{"empty composite (no-op)", NewCompositeAction()},
		{"composite of valid", NewCompositeAction(noop)},
		{"retryable of valid", NewRetryableAction(noop, 1, 0)},
		{"map with transform", NewMapAction("in", "out", func(x interface{}) (interface{}, error) { return x, nil })},
		{"validation with validator", NewValidationAction("in", func(interface{}) error { return nil }, "out", "err")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewWorkflowBuilder()
			b.AddStartNode("n").WithAction(tc.action)
			_, err := b.Build()
			require.NoError(t, err, "a run-safe action must still build")
		})
	}
}
