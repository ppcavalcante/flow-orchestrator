package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// --- Type-level tests (no executor) — locked under both Option A and B. ---

// TestExecutionError_ErrorString_DeterministicAndSummaryOnly verifies the
// aggregate Error() lists every failed node's name + its error, in NodeName
// order, regardless of the order failures were collected.
func TestExecutionError_ErrorString_DeterministicAndSummaryOnly(t *testing.T) {
	// Construct out of NodeName order; newExecutionError must sort.
	execErr := newExecutionError([]NodeError{
		{NodeName: "charlie", Err: errors.New("c-boom")},
		{NodeName: "alpha", Err: errors.New("a-boom")},
		{NodeName: "bravo", Err: errors.New("b-boom")},
	})
	if execErr == nil {
		t.Fatal("newExecutionError returned nil for 3 failures")
	}

	got := execErr.Error()

	// Deterministic ordering: alpha before bravo before charlie.
	ai := strings.Index(got, "alpha")
	bi := strings.Index(got, "bravo")
	ci := strings.Index(got, "charlie")
	ordered := ai >= 0 && bi >= 0 && ci >= 0 && ai < bi && bi < ci
	if !ordered {
		t.Fatalf("Error() not in deterministic NodeName order: %q (alpha=%d bravo=%d charlie=%d)", got, ai, bi, ci)
	}

	// Count is reported.
	if !strings.Contains(got, "3 nodes failed") {
		t.Fatalf("Error() missing failure count: %q", got)
	}

	// Each action error string is present.
	for _, want := range []string{"a-boom", "b-boom", "c-boom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() missing %q: %q", want, got)
		}
	}
}

// TestExecutionError_SingleFailureString checks the 1-node phrasing.
func TestExecutionError_SingleFailureString(t *testing.T) {
	execErr := newExecutionError([]NodeError{{NodeName: "solo", Err: errors.New("boom")}})
	got := execErr.Error()
	if !strings.Contains(got, "1 node failed") {
		t.Fatalf("Error() = %q, want it to say '1 node failed'", got)
	}
	if !strings.Contains(got, "solo") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q, want node name + error", got)
	}
}

// TestNewExecutionError_NilWhenEmpty: the constructor returns a typed-nil so
// callers can guard against it; an empty failure slice is not an error.
func TestNewExecutionError_NilWhenEmpty(t *testing.T) {
	if execErr := newExecutionError(nil); execErr != nil {
		t.Fatalf("newExecutionError(nil) = %v, want nil", execErr)
	}
	if execErr := newExecutionError([]NodeError{}); execErr != nil {
		t.Fatalf("newExecutionError([]) = %v, want nil", execErr)
	}
}

// TestExecutionError_UnwrapReachesSentinel: errors.Is must reach an action
// sentinel (ErrExecutionFailed) wrapped by a node's action, through the
// aggregate's multi-error Unwrap.
func TestExecutionError_UnwrapReachesSentinel(t *testing.T) {
	execErr := newExecutionError([]NodeError{
		{NodeName: "n1", Err: errors.New("plain")},
		{NodeName: "n2", Err: fmt.Errorf("wrapping: %w", ErrExecutionFailed)},
	})
	var err error = execErr

	if !errors.Is(err, ErrExecutionFailed) {
		t.Fatal("errors.Is(execErr, ErrExecutionFailed) = false, want true (must reach through Unwrap)")
	}
}

// TestExecutionError_AsExtracts: errors.As must extract *ExecutionError even
// when the aggregate is itself wrapped further up a chain.
func TestExecutionError_AsExtracts(t *testing.T) {
	inner := newExecutionError([]NodeError{{NodeName: "n", Err: errors.New("boom")}})
	wrapped := fmt.Errorf("outer: %w", inner)

	var execErr *ExecutionError
	if !errors.As(wrapped, &execErr) {
		t.Fatal("errors.As did not extract *ExecutionError from a wrapped chain")
	}
	if len(execErr.FailedNodes) != 1 || execErr.FailedNodes[0].NodeName != "n" {
		t.Fatalf("extracted ExecutionError = %+v, want 1 node 'n'", execErr.FailedNodes)
	}
}

// TestNodeError_UnwrapAndError: NodeError unwraps to its action error and its
// Error() carries name + error without extra state.
func TestNodeError_UnwrapAndError(t *testing.T) {
	sentinel := errors.New("the cause")
	ne := &NodeError{NodeName: "x", Err: fmt.Errorf("ctx: %w", sentinel)}
	if !errors.Is(ne, sentinel) {
		t.Fatal("errors.Is(NodeError, sentinel) = false, want true")
	}
	if !strings.Contains(ne.Error(), "x") {
		t.Fatalf("NodeError.Error() = %q, want node name", ne.Error())
	}
}

// --- Executor-level tests — A/B-independent (fail-fast multi-capture + no-leak). ---

// TestExecute_MultipleConcurrentFailFastFailures_AllCaptured is the CORE DEFECT
// FIX: when several NORMAL (non-coe) nodes in one level fail concurrently, the
// returned ExecutionError must capture ALL of them, not just the first. True
// under both Option A and Option B.
func TestExecute_MultipleConcurrentFailFastFailures_AllCaptured(t *testing.T) {
	const n = 8
	b := NewWorkflowBuilder().WithWorkflowID("multi-fail")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%d", i)
		b.AddStartNode(name).WithAction(failAction(fmt.Sprintf("boom-%d", i)))
	}

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("multi-fail")

	err = dag.Execute(context.Background(), data)
	if err == nil {
		t.Fatal("Execute = nil, want error (all normal nodes failed)")
	}

	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("Execute error is not *ExecutionError: %T (%v)", err, err)
	}

	// All n concurrent failures must be captured (the defect fix).
	if len(execErr.FailedNodes) != n {
		names := make([]string, len(execErr.FailedNodes))
		for i, ne := range execErr.FailedNodes {
			names[i] = ne.NodeName
		}
		t.Fatalf("captured %d failures %v, want all %d", len(execErr.FailedNodes), names, n)
	}

	// Deterministic ordering by NodeName.
	for i := 1; i < len(execErr.FailedNodes); i++ {
		if execErr.FailedNodes[i-1].NodeName >= execErr.FailedNodes[i].NodeName {
			t.Fatalf("FailedNodes not sorted by NodeName: %s before %s",
				execErr.FailedNodes[i-1].NodeName, execErr.FailedNodes[i].NodeName)
		}
	}
}

// TestExecute_SingleFailure_StillReported is the regression guard: a single
// normal failure still produces an ExecutionError naming that node.
func TestExecute_SingleFailure_StillReported(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("single")
	b.AddStartNode("only").WithAction(failAction("boom"))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("single")

	err = dag.Execute(context.Background(), data)
	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("Execute error is not *ExecutionError: %T (%v)", err, err)
	}
	if len(execErr.FailedNodes) != 1 || execErr.FailedNodes[0].NodeName != "only" {
		t.Fatalf("FailedNodes = %+v, want exactly [only]", execErr.FailedNodes)
	}
}

// TestExecute_Success_NilError: the happy path returns nil (no ExecutionError).
func TestExecute_Success_NilError(t *testing.T) {
	var ran atomic.Bool
	b := NewWorkflowBuilder().WithWorkflowID("ok")
	b.AddStartNode("a").WithAction(markAction(&ran))

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("ok")

	if err := dag.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute = %v, want nil on success", err)
	}
	if !ran.Load() {
		t.Fatal("action did not run")
	}
}

// TestExecute_ErrorIsReachesSentinelThroughExecutor: an action that wraps
// ErrExecutionFailed must remain reachable via errors.Is on the top-level error
// returned by Execute (through Node.Execute + the aggregate Unwrap).
func TestExecute_ErrorIsReachesSentinelThroughExecutor(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("sentinel")
	b.AddStartNode("boom").WithAction(func(_ context.Context, _ *WorkflowData) error {
		return fmt.Errorf("action failed: %w", ErrExecutionFailed)
	})

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("sentinel")

	err = dag.Execute(context.Background(), data)
	if err == nil {
		t.Fatal("Execute = nil, want error")
	}
	if !errors.Is(err, ErrExecutionFailed) {
		t.Fatalf("errors.Is(Execute err, ErrExecutionFailed) = false, want true; err = %v", err)
	}
}

// TestExecute_NoDataLeakInError is the discriminating no-leak test (security
// pass-2 boundary): a sentinel secret is planted in WorkflowData, the failing
// action returns a CLEAN error that does not itself contain the secret, and we
// assert the secret never appears in the aggregate Error() or any
// NodeError.Error(). This proves OUR aggregation pulls nothing from the data
// map / engine state into the error string.
func TestExecute_NoDataLeakInError(t *testing.T) {
	const secret = "SUPERSECRET-XYZ"

	b := NewWorkflowBuilder().WithWorkflowID("noleak")
	b.AddStartNode("leaky").WithAction(func(_ context.Context, d *WorkflowData) error {
		// Plant the secret in the data map; return a CLEAN error (no secret).
		d.Set("secret", secret)
		return errors.New("boom")
	})

	dag, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data := NewWorkflowData("noleak")

	err = dag.Execute(context.Background(), data)
	if err == nil {
		t.Fatal("Execute = nil, want error")
	}

	// The secret must not appear anywhere in our error rendering.
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked into Execute error string: %q", err.Error())
	}

	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("Execute error is not *ExecutionError: %T", err)
	}
	if strings.Contains(execErr.Error(), secret) {
		t.Fatalf("secret leaked into ExecutionError.Error(): %q", execErr.Error())
	}
	for _, ne := range execErr.FailedNodes {
		if strings.Contains(ne.Error(), secret) {
			t.Fatalf("secret leaked into NodeError.Error(): %q", ne.Error())
		}
	}
	// Sanity: the secret really was in the data map (so the test is meaningful).
	if got, _ := data.GetString("secret"); got != secret {
		t.Fatalf("precondition: secret not in data map, got %q", got)
	}
}
