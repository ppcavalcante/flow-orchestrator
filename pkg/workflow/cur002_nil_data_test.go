package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// CUR-002 / AUD-001: DAG.Execute must reject a nil *WorkflowData with a typed ErrValidation
// rather than nil-dereferencing inside an executor goroutine (which has no recover(), so the
// panic would crash the host with SIGSEGV). An invalid input is a returned error, not a crash.
func TestCUR002_DAGExecuteRejectsNilWorkflowData(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("cur002")
	b.AddStartNode("n").WithActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })
	dag, err := b.Build()
	require.NoError(t, err)

	// The call must RETURN a typed error, not panic. require.NotPanics makes the crash-vs-error
	// distinction the assertion, not just the error value.
	var execErr error
	require.NotPanics(t, func() { execErr = dag.Execute(context.Background(), nil) })
	require.ErrorIs(t, execErr, ErrValidation,
		"CUR-002: a nil WorkflowData must be a typed ErrValidation")

	// Positive control: a real WorkflowData still drives cleanly (the guard does not over-reject).
	require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("cur002")))
}

// TestCUR002_TypedNilStoreIsRejectedNotPanicked covers AUD-031's second half: a TYPED-nil
// WorkflowStore (a non-nil interface wrapping a nil concrete pointer) passes a plain `== nil`
// check but panics on the first method call. Both ApprovalNonceFromStore and Workflow.Execute
// must reject it with ErrValidation instead of crashing the host.
func TestCUR002_TypedNilStoreIsRejectedNotPanicked(t *testing.T) {
	// A nil concrete pointer boxed into the interface: the interface is non-nil (that is the whole
	// point of AUD-031 — `store == nil` is provably false, so it misses this), but store.Load panics.
	var typedNil *JSONFileStore
	var store WorkflowStore = typedNil

	// (1) ApprovalNonceFromStore
	var err error
	require.NotPanics(t, func() { _, err = ApprovalNonceFromStore(store, "wf", "gate") })
	require.ErrorIs(t, err, ErrValidation, "CUR-002/AUD-031: typed-nil store must be a typed error")

	// (2) Workflow.Execute with a typed-nil Store injected.
	b := NewWorkflowBuilder().WithWorkflowID("cur002-typednil")
	b.AddStartNode("n").WithActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })
	w, ferr := FromBuilder(b)
	require.NoError(t, ferr)
	w.Store = store // inject the typed-nil store
	var execErr error
	require.NotPanics(t, func() { execErr = w.Execute(context.Background()) })
	require.ErrorIs(t, execErr, ErrValidation, "CUR-002/AUD-031: a typed-nil Workflow.Store must be a typed error")
}
