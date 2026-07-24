package workflow

// M21 coverage earn-back — biting tests over the genuinely-uncovered NEW fanout.go guard/error paths (the +547
// production lines that diluted pkg/workflow below the 90% floor). Each asserts a real refusal/behavior, not
// coverage padding. ⭐ TestFanOut_JournalTamper is LOW-M21-01: a tampered/corrupt expansion journal is REFUSED
// with ErrValidation (a security + resume-integrity guard AND the resolveExpansion coverage).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedJournalAndDrive seeds a fan-out parent with the given raw journal value under the reserved items key + a
// Pending fan node (the resume window), then drives the fan-out. Returns the run error. The journal is seeded as
// an arbitrary interface{} so a test can plant a NON-string / malformed / wrong-N value the guards must catch.
func seedJournalAndDrive(t *testing.T, journalValue interface{}) error {
	t.Helper()
	store := NewInMemoryStore()
	const id = "wf-tamper"
	seed := NewWorkflowData(id)
	seed.Set(fanOutItemsKey("fan"), journalValue) // the tampered/corrupt journal
	require.NoError(t, store.Save(seed))

	branch := ActionFunc(func(_ context.Context, d *WorkflowData) error { d.Set("out", int64(1)); return nil })
	b := NewWorkflowBuilder().WithWorkflowID(id)
	b.AddFanOut("fan", intItemsExpander(3), branch).WithResults("r", "out")
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = id
	w.DAG = dag
	return w.Execute(context.Background())
}

// TestFanOut_JournalTamper (LOW-M21-01) — a tampered/corrupt expansion journal is REFUSED with ErrValidation on
// resume, never fanned out at a wrong/undefined width. Covers the resolveExpansion corruption guards
// (fanout.go:451 non-string, :455 malformed-JSON, :460 count≠len). Resume-integrity + a tracked security LOW: a
// store an attacker/bit-rot can tamper must not let a corrupt journal drive an arbitrary branch count.
func TestFanOut_JournalTamper(t *testing.T) {
	// (a) non-string journal value (a slice snuck in where the string journal belongs).
	err := seedJournalAndDrive(t, []interface{}{1, 2, 3})
	require.ErrorIs(t, err, ErrValidation, "a non-string journal is refused")
	require.ErrorContains(t, err, "not a string")

	// (b) malformed JSON string (truncated).
	err = seedJournalAndDrive(t, `{"n":3,"items":[0,1`)
	require.ErrorIs(t, err, ErrValidation, "a malformed-JSON journal is refused")
	require.ErrorContains(t, err, "malformed")

	// (c) count ≠ items length (the torn-write / tamper guard — the load-bearing one): N says 5, items has 2.
	tampered, merr := json.Marshal(fanOutJournal{N: 5, Items: []json.RawMessage{json.RawMessage("0"), json.RawMessage("1")}})
	require.NoError(t, merr)
	err = seedJournalAndDrive(t, string(tampered))
	require.ErrorIs(t, err, ErrValidation, "a count≠items-length journal is refused (torn write / tamper)")
	require.ErrorContains(t, err, "≠ items length")
}

// TestFanOut_ExpanderError — an expander returning an error propagates (wrapped), the fan node fails, nothing is
// journaled. Covers resolveExpansion:469 (the expander-error branch).
func TestFanOut_ExpanderError(t *testing.T) {
	store := NewInMemoryStore()
	boom := ActionFunc(func(_ context.Context, d *WorkflowData) error { d.Set("out", int64(1)); return nil })
	b := NewWorkflowBuilder().WithWorkflowID("wf-experr")
	b.AddFanOut("fan", func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		return nil, context.DeadlineExceeded // an expander that fails
	}, boom).WithResults("r", "out")
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = "wf-experr"
	w.DAG = dag
	err = w.Execute(context.Background())
	require.Error(t, err, "an expander error fails the fan-out node")
	require.ErrorContains(t, err, "expander")
	// Nothing journaled (the failure was before the journal write).
	final, lerr := store.Load("wf-experr")
	require.NoError(t, lerr)
	_, has := final.Get(fanOutItemsKey("fan"))
	require.False(t, has, "an expander error journals nothing")
}

// TestFanOut_BuilderGuards — the builder-method guard branches (AddFanOut nil args; WithResults/WithMaxWidth/
// WithCollectPartial on a non-fan-out node → a deferred actionErr surfaced by Build). Covers fanout.go:114/134/
// 148/170.
func TestFanOut_BuilderGuards(t *testing.T) {
	t.Run("AddFanOut-nil-expander", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("x")
		b.AddFanOut("fan", nil, ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
		_, err := b.Build()
		require.ErrorIs(t, err, ErrValidation, "a nil expander is refused at Build")
	})
	t.Run("AddFanOut-nil-branch", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("x")
		b.AddFanOut("fan", intItemsExpander(1), nil)
		_, err := b.Build()
		require.ErrorIs(t, err, ErrValidation, "a nil branchAction is refused at Build")
	})
	// WithResults/WithMaxWidth/WithCollectPartial on a NON-fan-out node → a deferred actionErr.
	for _, tc := range []struct {
		name  string
		apply func(*NodeBuilder)
	}{
		{"WithResults", func(n *NodeBuilder) { n.WithResults("a", "b") }},
		{"WithMaxWidth", func(n *NodeBuilder) { n.WithMaxWidth(5) }},
		{"WithCollectPartial", func(n *NodeBuilder) { n.WithCollectPartial() }},
	} {
		t.Run(tc.name+"-on-non-fanout-node", func(t *testing.T) {
			b := NewWorkflowBuilder().WithWorkflowID("x")
			nb := b.AddStartNode("plain").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
			tc.apply(nb)
			_, err := b.Build()
			require.ErrorIs(t, err, ErrValidation, "%s on a non-AddFanOut node is a build error", tc.name)
		})
	}
}

// TestFanOut_CountKeyForeignCollision — a FOREIGN pre-existing count key (a value ≠ N, or a non-integer) is a loud
// collision; an equal count (== N) is an idempotent re-apply (allowed). Covers checkBaseKeyCollisions:421 (the
// count-value-aware branch) + coerceCountInt (fanout.go:429, the int/int64/json.Number/foreign-sentinel coercion).
func TestFanOut_CountKeyForeignCollision(t *testing.T) {
	// (a) a foreign NON-integer count value → collision (coerceCountInt returns the -1 sentinel ≠ N).
	t.Run("foreign-non-int-count", func(t *testing.T) {
		store := NewInMemoryStore()
		b := NewWorkflowBuilder().WithWorkflowID("wf-cc")
		b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set(fanOutResultCountKey("r"), "not-an-int") // foreign value under the count key
			return nil
		}))
		b.AddFanOut("fan", intItemsExpander(3), ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("out", int64(1))
			return nil
		})).WithResults("r", "out").DependsOn("seed")
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(store)
		w.WorkflowID = "wf-cc"
		w.DAG = dag
		require.ErrorIs(t, w.Execute(context.Background()), ErrFanOutResultKeyCollision, "a foreign count value is a loud collision")
	})
	// (b) an EQUAL count (== N) is the idempotent re-apply → allowed (the resume case).
	t.Run("equal-count-allowed", func(t *testing.T) {
		store := NewInMemoryStore()
		b := NewWorkflowBuilder().WithWorkflowID("wf-cc2")
		b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set(fanOutResultCountKey("r"), 3) // == N (the fan-out has 3 branches) → idempotent
			return nil
		}))
		b.AddFanOut("fan", intItemsExpander(3), ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("out", int64(1))
			return nil
		})).WithResults("r", "out").DependsOn("seed")
		dag, err := b.Build()
		require.NoError(t, err)
		w := NewWorkflow(store)
		w.WorkflowID = "wf-cc2"
		w.DAG = dag
		require.NoError(t, w.Execute(context.Background()), "an equal (==N) pre-existing count is an idempotent re-apply, allowed")
	})
}
