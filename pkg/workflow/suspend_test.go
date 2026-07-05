package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSuspendAction returns ErrSuspended every time and records how many
// times it was invoked. It is a raw Action (no suspension marker) on purpose:
// the retry wrappers operate on bare Actions, and the contract under test is
// "ErrSuspended is never retried" independent of node.Execute's declared-node
// gating.
type countingSuspendAction struct{ calls int }

func (a *countingSuspendAction) Execute(_ context.Context, _ *WorkflowData) error {
	a.calls++
	return ErrSuspended
}

// TestRetryWrappersDoNotRetrySuspend is the T2 suspend-arm short-circuit guard:
// a park (ErrSuspended) is a SUCCESS arm, not a retryable failure. Each retry
// wrapper must return the sentinel on the FIRST attempt — never re-running the
// action and never wrapping the sentinel in a "max retries exceeded" error that
// would mis-describe the park (errors.Is must still see ErrSuspended).
func TestRetryWrappersDoNotRetrySuspend(t *testing.T) {
	const maxRetries = 5

	t.Run("RetryableAction", func(t *testing.T) {
		act := &countingSuspendAction{}
		r := NewRetryableAction(act, maxRetries, 0)

		err := r.Execute(context.Background(), NewWorkflowData("wf"))

		require.ErrorIs(t, err, ErrSuspended)
		assert.Equal(t, 1, act.calls, "a parked action must run exactly once, not be retried")
	})

	t.Run("RetryMiddleware", func(t *testing.T) {
		act := &countingSuspendAction{}
		wrapped := RetryMiddleware(maxRetries, 0)(act)

		err := wrapped.Execute(context.Background(), NewWorkflowData("wf"))

		require.ErrorIs(t, err, ErrSuspended)
		assert.Equal(t, 1, act.calls, "a parked action must run exactly once, not be retried")
	})

	t.Run("NoDelayRetryMiddleware", func(t *testing.T) {
		act := &countingSuspendAction{}
		wrapped := NoDelayRetryMiddleware(maxRetries)(act)

		err := wrapped.Execute(context.Background(), NewWorkflowData("wf"))

		require.ErrorIs(t, err, ErrSuspended)
		assert.Equal(t, 1, act.calls, "a parked action must run exactly once, not be retried")
	})

	t.Run("ConditionalRetryMiddleware", func(t *testing.T) {
		act := &countingSuspendAction{}
		// An always-retry predicate: only the ErrSuspended short-circuit (not the
		// predicate) can stop the loop, so this proves the short-circuit fires.
		wrapped := ConditionalRetryMiddleware(maxRetries, 0, func(error) bool { return true })(act)

		err := wrapped.Execute(context.Background(), NewWorkflowData("wf"))

		require.ErrorIs(t, err, ErrSuspended)
		assert.Equal(t, 1, act.calls, "a parked action must run exactly once, not be retried")
	})
}

// TestRetryWrappersStillRetryRealErrors is the companion bite-guard: the
// suspend-arm short-circuit must not accidentally suppress retries of genuine
// failures. A plain error is still retried the full maxRetries+1 times.
func TestRetryWrappersStillRetryRealErrors(t *testing.T) {
	const maxRetries = 3
	realErr := errors.New("boom")

	count := 0
	act := ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		count++
		return realErr
	})

	r := NewRetryableAction(act, maxRetries, 0)
	err := r.Execute(context.Background(), NewWorkflowData("wf"))

	require.ErrorIs(t, err, realErr)
	assert.False(t, errors.Is(err, ErrSuspended), "a real error must not be mistaken for a park")
	assert.Equal(t, maxRetries+1, count, "a real failure is retried the full budget")
}

// TestWaitingIsLiveNotTerminal_Predicates is the direct regression guard for the
// WaitingIsLiveNotTerminal binding sites (D-07): the three executor status
// predicates must all classify Waiting as live-but-parked, never terminal,
// skip-causing, or dependency-resolving. A future edit that folds Waiting into
// any of them (e.g. treating it as terminal) would silently break suspend — this
// catches it at the predicate level, independent of the integration tests.
func TestWaitingIsLiveNotTerminal_Predicates(t *testing.T) {
	assert.False(t, isTerminalStatus(Waiting), "Waiting is NOT terminal — the run is not done")
	assert.False(t, isSkipCause(Waiting), "a Waiting dep must NOT cause dependents to be Skipped")

	// A dependency that is Waiting does not resolve (dependents must wait, not
	// proceed) — for both an ordinary and a continue-on-error dependency.
	plain := NewNode("dep", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	coe := NewNode("dep", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	coe.ContinueOnError = true
	assert.False(t, depResolved(plain, Waiting, andDependent), "a Waiting dep does not resolve its dependents")
	assert.False(t, depResolved(coe, Waiting, andDependent), "a Waiting continue-on-error dep does not resolve either")
}

// TestNodeExecute_DeclaredSuspensionNodeParks is the T3 (DEC-M10-mechanism)
// guard: a declared suspension node whose action returns ErrSuspended parks —
// node.Execute sets the non-terminal Waiting status and propagates the sentinel,
// never a Failed-stamp — and the park bypasses the timeout/retry wrappers (it is
// not retried even with RetryCount set, and not timed out).
func TestNodeExecute_DeclaredSuspensionNodeParks(t *testing.T) {
	gate := newParkGate()
	node := newSuspendingNode("parker", gate)
	// Set retry + timeout to prove a park bypasses BOTH wrappers: a parked node
	// must run exactly once and surface the sentinel, not be retried or timed out.
	node.RetryCount = 5
	node.Timeout = time.Hour
	data := NewWorkflowData("wf")

	err := node.Execute(context.Background(), data)

	require.ErrorIs(t, err, ErrSuspended, "a declared suspension node returns the park sentinel")
	status, ok := data.GetNodeStatus("parker")
	require.True(t, ok)
	assert.Equal(t, Waiting, status, "a parked node is Waiting, never Failed")
	parkRuns, _ := gate.stats()
	assert.Equal(t, 1, parkRuns, "a park is not retried — the action runs exactly once")
}

// TestNodeExecute_SuspensionNodeWakesAndCompletes proves the re-enter arm: once
// the gate is woken (the external event arrived), re-running the same node
// completes it (Completed status, nil error) — the "park is a crash you chose,
// wake is the resume path re-entered" convergence at the node level.
func TestNodeExecute_SuspensionNodeWakesAndCompletes(t *testing.T) {
	gate := newParkGate()
	node := newSuspendingNode("parker", gate)
	data := NewWorkflowData("wf")

	// First run parks.
	require.ErrorIs(t, node.Execute(context.Background(), data), ErrSuspended)
	st, _ := data.GetNodeStatus("parker")
	require.Equal(t, Waiting, st)

	// The external event arrives; re-entering Execute now completes.
	gate.wake()
	require.NoError(t, node.Execute(context.Background(), data))
	st, _ = data.GetNodeStatus("parker")
	assert.Equal(t, Completed, st, "a woken node completes on re-entry")
}

// TestNodeExecute_OrdinaryActionCannotPark is the static-topology guard: an
// ORDINARY action (no suspendableAction marker) returning ErrSuspended is a
// misuse — node.Execute fails it loudly and the sentinel MUST NOT escape, or the
// executor would falsely park the run on a node the model never treats as
// waiting-capable. (errors.Is(returned, ErrSuspended) must be false.)
func TestNodeExecute_OrdinaryActionCannotPark(t *testing.T) {
	node := NewNode("rogue", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return ErrSuspended
	}))
	data := NewWorkflowData("wf")

	err := node.Execute(context.Background(), data)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrSuspended),
		"an ordinary action's ErrSuspended must not escape as a park")
	status, ok := data.GetNodeStatus("rogue")
	require.True(t, ok)
	assert.Equal(t, Failed, status, "a non-declared node misusing the sentinel is Failed")
}
