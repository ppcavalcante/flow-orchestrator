package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Blocker 117-F1: a saga node was stamped Compensated with its compensation NEVER
// INVOKED, reachable through the public builder API.
//
// WithRetries does no validation. Before M23 SEAL-01, build() called the mutator behind
// `if builder.retryCount > 0`, so a negative count was silently absorbed and never
// reached the node. The seal deleted the mutators and made the assignment
// unconditional, which let -1 through — and saga_rollback.go's compensation loop is
// `for attempt := 0; attempt <= n.retryCount` with no guard. With -1 the body never
// runs, lastErr stays nil, nil reads as a successful undo, and the node is recorded
// Compensated in the durable journal.
//
// WHY THIS ASSERTS THE INVOCATION COUNT AND NOT THE STATUS — the trap in testing this,
// and the reason the defect shipped over a green suite. MEASURED both sides (clamp
// present vs `node.retryCount = builder.retryCount`), retries=-1:
//
//	broken: compInvocations=0  status="compensated"  err=`workflow rolled back: …: boom`
//	fixed:  compInvocations=1  status="compensated"  err=`workflow rolled back: …: boom`
//
// So a test asserting status == Compensated passes over the live defect, and the
// returned error is IDENTICAL either side — no assertion on it discriminates. The
// invocation count is the only signal that does.
//
// An earlier version of this comment also said a test "asserting Execute returns a
// *SagaError" would pass over the defect. THAT WAS WRONG and unmeasured: errors.As
// for *SagaError is FALSE on BOTH sides, so such a test fails either way rather than
// passing either way. saga_error.go:35 says why — a rollback whose compensations all
// SUCCEEDED returns the trigger cause, not a *SagaError, and the broken build's
// skipped loop leaves lastErr nil, which is indistinguishable from "all succeeded".
// That is the same false-premise-under-a-true-conclusion shape as the blocker. (D-23.)
//
// BITE-PROVEN: removing the max(0, …) clamp at builder.go reds the -1 and -7 arms with
// "should have 1 item(s), but has 0", while the 0-retries control arm stays GREEN — so
// the zero count is the clamp, not a fixture that never rolled back.
//
// The pre-existing coverage (builder_test.go) pins only POSITIVE retry values —
// precisely the half of the boundary the migration did not change, which is why nothing
// went red.
func TestSagaCompensation_NegativeRetryCountStillCompensates(t *testing.T) {
	errBoom := errors.New("boom")

	// build wires a -> b. "a" succeeds and is compensable; "b" fails, which triggers
	// the saga rollback that must undo "a". retries is applied to "a", the node whose
	// compensation loop is under test.
	build := func(t *testing.T, rec *compRecorder, retries int) *Workflow {
		t.Helper()
		b := NewWorkflowBuilder()
		b.AddNode("a").
			WithActionFunc(func(context.Context, *WorkflowData) error { return nil }).
			WithRetries(retries).
			WithCompensationFunc(func(context.Context, *WorkflowData) error {
				rec.record("a")
				return nil
			})
		b.AddNode("b").
			WithActionFunc(func(context.Context, *WorkflowData) error { return errBoom }).
			DependsOn("a")
		dag, err := b.Build()
		require.NoError(t, err)
		return &Workflow{dag: dag, WorkflowID: "saga-neg-retry", Store: NewInMemoryStore()}
	}

	for _, tc := range []struct {
		name    string
		retries int
	}{
		// The control. If this arm ever fails the fixture is broken, not the clamp —
		// without it a zero count in the -1 arm would be indistinguishable from a
		// rollback that never ran at all.
		{"zero retries (control)", 0},
		// The defect's input. -1 is what the public API accepts and what build() clamps.
		{"negative retries", -1},
		// A negative value that is not -1, so the clamp cannot be a special case on -1.
		{"very negative retries", -7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec compRecorder
			wf := build(t, &rec, tc.retries)

			err := wf.Execute(context.Background())
			require.Error(t, err, "the run must fail — node b returns an error, which is what triggers the rollback")

			// THE ASSERTION THAT DISCRIMINATES. Status is Compensated either side of
			// the defect; only the invocation count differs.
			require.Len(t, rec.snapshot(), 1,
				"node a's compensation must be invoked exactly ONCE (got %v). A count of 0 with retryCount=%d "+
					"means the compensation loop skipped its body and the run still recorded the node as "+
					"compensated — an effect the journal claims was undone and never was (117-F1).",
				rec.snapshot(), tc.retries)
		})
	}
}
