package workflow

// M14 ph62 REM-03 — universal ErrRolledBack (DEC-M13-V1 Option C). A CLEAN saga
// rollback now wraps its cause so errors.Is(err, ErrRolledBack) fires for ANY
// rollback, while errors.As still reaches the underlying *ExecutionError. These are
// the load-bearing bites; each must be able to REDDEN the pre-fix behavior (which
// returned the bare cause).

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildFailingSaga builds a diamond where `fail` fails, triggering a rollback of the
// completed compensable nodes. `compFail` (if non-empty) names a node whose
// compensation itself fails → a PARTIAL rollback (*SagaError).
func buildFailingSaga(t *testing.T, id string, compFail string) *Workflow {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID(id)
	comp := func(_ context.Context, _ *WorkflowData) error { return nil }
	failingComp := func(_ context.Context, _ *WorkflowData) error { return errors.New("comp boom") }
	pick := func(n string) func(context.Context, *WorkflowData) error {
		if n == compFail {
			return failingComp
		}
		return comp
	}
	b.AddStartNode("a").WithAction(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("a", "ok")
		return nil
	}).WithCompensation(pick("a"))
	b.AddNode("b").WithAction(func(_ context.Context, d *WorkflowData) error {
		d.SetOutput("b", "ok")
		return nil
	}).WithCompensation(pick("b")).DependsOn("a")
	b.AddNode("fail").WithAction(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("hard failure")
	}).DependsOn("a")
	w, err := FromBuilder(b)
	require.NoError(t, err)
	return w
}

// Bite 1 — Is(ErrRolledBack) fires on a CLEAN rollback (pre-fix: bare cause → RED).
func TestREM03_CleanRollback_IsErrRolledBack(t *testing.T) {
	w := buildFailingSaga(t, "clean", "") // no comp fails → clean rollback
	err := w.Execute(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrRolledBack),
		"a clean rollback is detectable via errors.Is(ErrRolledBack) — the REM-03 contract")
}

// Bite 2 — As(&ExecutionError) STILL reaches the underlying cause (no regression).
func TestREM03_CleanRollback_AsReachesCause(t *testing.T) {
	w := buildFailingSaga(t, "clean2", "")
	err := w.Execute(context.Background())
	var ee *ExecutionError
	require.True(t, errors.As(err, &ee),
		"errors.As still reaches the *ExecutionError cause through the ErrRolledBack wrap")
	// And BOTH hold on the same error — the multi-%w contract.
	require.True(t, errors.Is(err, ErrRolledBack), "Is + As both reach on the same wrapped error")
}

// Bite 3 — a PARTIAL rollback STILL yields *SagaError (unchanged), and it too is
// recognizably a rollback via its own machinery (As reaches the cause).
func TestREM03_PartialRollback_StillSagaError(t *testing.T) {
	w := buildFailingSaga(t, "partial", "b") // b's compensation fails → partial
	err := w.Execute(context.Background())
	var se *SagaError
	require.True(t, errors.As(err, &se), "a partial rollback still surfaces as *SagaError")
	require.NotEmpty(t, se.FailedToCompensate, "the partition names the failed compensation")
	var ee *ExecutionError
	require.True(t, errors.As(err, &ee), "the *SagaError still unwraps to the *ExecutionError cause")
}

// Bite 4 — NO false-clean: a plain (non-rollback) failure does NOT Is(ErrRolledBack).
func TestREM03_PlainFailure_NotErrRolledBack(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("plain")
	// A single failing node with NO compensation anywhere → a plain failure, no saga
	// rollback (nothing compensable completed).
	b.AddStartNode("x").WithAction(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("plain boom")
	})
	w, err := FromBuilder(b)
	require.NoError(t, err)
	execErr := w.Execute(context.Background())
	require.Error(t, execErr)
	require.False(t, errors.Is(execErr, ErrRolledBack),
		"a plain failure (no rollback) must NOT match ErrRolledBack — no false-clean")
	var ee *ExecutionError
	require.True(t, errors.As(execErr, &ee), "it is a plain *ExecutionError")
}

// M14 ph62 REM-04 — Build() guards the WithStore footgun. A bare *DAG cannot carry a
// store; WithStore(s).Build().Execute would silently run non-durable. The guard turns
// that into a loud error. FromBuilder (which carries the store onto the *Workflow) is
// unaffected.

// Bite — WithStore(s).Build() ERRORS (pre-guard: silently returned a store-less DAG).
func TestREM04_WithStoreBuild_Errors(t *testing.T) {
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("g").WithStore(store)
	b.AddStartNode("n").WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
	dag, err := b.Build()
	require.Error(t, err, "WithStore(s).Build() must error (a DAG can't carry a store) — no silent non-durable run")
	require.Nil(t, dag, "no DAG is returned on the guard error")
	require.ErrorIs(t, err, ErrValidation, "the guard is an ErrValidation")
}

// Bite — the store-less Build() path is UNCHANGED (no WithStore → builds fine).
func TestREM04_StorelessBuild_Unchanged(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("plain")
	b.AddStartNode("n").WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
	dag, err := b.Build()
	require.NoError(t, err, "Build() with no store is unchanged")
	require.NotNil(t, dag)
}

// Bite — FromBuilder(WithStore(s)) STILL works (store propagates onto the Workflow +
// is observed used by Execute) — the guard did not break the durable-build path.
func TestREM04_FromBuilderWithStore_StillDurable(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	b := NewWorkflowBuilder().WithWorkflowID("dur").WithStore(store)
	b.AddStartNode("n").WithAction(func(_ context.Context, d *WorkflowData) error { d.SetOutput("n", "ok"); return nil })
	w, err := FromBuilder(b)
	require.NoError(t, err, "FromBuilder still builds with a store (uses the guard-free build())")
	require.Same(t, WorkflowStore(store), w.Store, "the store propagated onto the Workflow")
	require.NoError(t, w.Execute(context.Background()))
	loaded, err := store.Load("dur")
	require.NoError(t, err, "state persisted — the durable path is intact")
	st, _ := loaded.GetNodeStatus("n")
	require.Equal(t, Completed, st)
}
