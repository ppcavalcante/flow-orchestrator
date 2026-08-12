package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// boundaryChain builds v -> d -> s, the smallest graph a boundary (d, v, s) is valid
// on: v is the sole root and is the verifier, d is an ancestor of s, s is not a root.
// Built through the PUBLIC API so nothing here depends on a shape build() would refuse.
func boundaryChain(t *testing.T, id string) *DAG {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID(id)
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })
	b.AddStartNode("v").WithActionFunc(noop)
	b.AddNode("d").WithActionFunc(noop).DependsOn("v")
	b.AddNode("s").WithActionFunc(noop).DependsOn("d")
	b.WithBoundary("d", "v", "s")
	dag, err := b.Build()
	require.NoError(t, err)
	return dag
}

// TestBoundaryEnvelope_RoundTripsOnEveryBackend is the projection's acceptance arm. It
// drives a real run and then reads the projection back OUT OF THE STORE, per backend --
// not out of the live WorkflowData, which would prove only that Set works.
//
// Four backends because the value axis is the one SEAM-05 found unstable: a slice does
// not round-trip uniformly, which is why the projection is one JSON STRING.
func TestBoundaryEnvelope_RoundTripsOnEveryBackend(t *testing.T) {
	stores := []struct {
		make func(t *testing.T) WorkflowStore
		name string
	}{
		{func(*testing.T) WorkflowStore { return NewInMemoryStore() }, "InMemoryStore"},
		{func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}, "JSONFileStore"},
		{func(t *testing.T) WorkflowStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		}, "FlatBuffersStore"},
		{func(t *testing.T) WorkflowStore {
			s, err := NewSQLiteStore(t.TempDir() + "/wf.db")
			require.NoError(t, err)
			return s
		}, "SQLiteStore"},
	}

	for _, sc := range stores {
		t.Run(sc.name, func(t *testing.T) {
			store := sc.make(t)
			dag := boundaryChain(t, "wf-"+sc.name)
			data := NewWorkflowData("wf-" + sc.name)

			require.NoError(t, dag.Execute(context.Background(), data))
			require.NoError(t, store.Save(data))

			loaded, err := store.Load("wf-" + sc.name)
			require.NoError(t, err)

			raw, ok := loaded.Get(boundariesKey)
			require.True(t, ok, "the projection must survive the round-trip under its namespaced key")

			env, derr := decodeBoundaryEnvelope(raw)
			require.NoError(t, derr, "and must decode strictly after the round-trip")
			require.Equal(t, boundaryEnvelopeVersion, env.Version,
				"the version must survive as int64 — the axis SEAM-05 measured widening on")
			require.Equal(t,
				[]boundaryEnvelopeEntry{{Doer: "d", Verifier: "v", Sink: "s"}},
				env.Boundaries,
				"and the declaration must come back whole, in declaration order")
		})
	}
}

// TestBoundaryEnvelope_NoBoundaryWritesNothing pins the det-tax moat's OBSERVABLE half:
// a workflow declaring no boundary must leave no key behind. It does not by itself prove
// the allocation claim (that is the benchmark's job) — it proves the branch is gated at
// all, which is the part a refactor can silently lose.
func TestBoundaryEnvelope_NoBoundaryWritesNothing(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("plain")
	b.AddStartNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	dag, err := b.Build()
	require.NoError(t, err)
	require.False(t, dag.hasBoundaries, "a workflow with no WithBoundary must not be flagged")

	data := NewWorkflowData("plain")
	require.NoError(t, dag.Execute(context.Background(), data))

	_, ok := data.Get(boundariesKey)
	require.False(t, ok, "a boundary-free workflow must not pay for the projection")
}

// TestBoundaryEnvelope_StrictDecodeRefuses is the compatibility contract. The version
// arm is the load-bearing one: it is what makes "M24 extends rather than rewrites"
// checkable instead of hoped.
func TestBoundaryEnvelope_StrictDecodeRefuses(t *testing.T) {
	t.Run("unknown version", func(t *testing.T) {
		future, err := json.Marshal(boundaryEnvelope{Version: boundaryEnvelopeVersion + 1})
		require.NoError(t, err)

		_, derr := decodeBoundaryEnvelope(string(future))
		require.ErrorIs(t, derr, errBoundaryEnvelopeVersion, "an unknown version is refused by TYPE, not by string")
		require.ErrorIs(t, derr, ErrValidation, "and it stays in the validation domain")
		require.Contains(t, derr.Error(), "this build reads version 1",
			"the refusal must say which version this build speaks, or an operator cannot act on it")
	})

	t.Run("version zero is not a free pass", func(t *testing.T) {
		// The zero value of an int64 field is the shape an envelope written by a build
		// that had no version at all would decode to. It must be refused, not defaulted.
		_, derr := decodeBoundaryEnvelope(`{"boundaries":[]}`)
		require.ErrorIs(t, derr, errBoundaryEnvelopeVersion,
			"a missing version decodes to 0 and must be refused rather than treated as version 1")
	})

	t.Run("not a string", func(t *testing.T) {
		_, derr := decodeBoundaryEnvelope(map[string]any{"version": 1})
		require.ErrorIs(t, derr, ErrValidation)
		require.Contains(t, derr.Error(), "not a string")
	})

	t.Run("malformed", func(t *testing.T) {
		_, derr := decodeBoundaryEnvelope(`{"version":`)
		require.ErrorIs(t, derr, ErrValidation)
		require.Contains(t, derr.Error(), "malformed")
	})
}

// TestBoundaryEnvelope_EncodedDepthIsConstant IS THE DEPTH ARM, and it asserts the
// structural fact rather than pretending to exercise the guard.
//
// checkJSONDepth runs on the encoded bytes before Set (encodeBoundaryEnvelope), because
// WorkflowData.Set is an unguarded bare map write and every writer owes its own check.
// But AT THIS ENVELOPE'S SHAPE THE GUARD CANNOT FIRE: boundaryDecl is three strings and
// jsonNestingDepth deliberately skips string contents, so no consumer input reaches the
// depth axis at all. Asserting "deep input is refused" here would be theatre — there is
// no deep input to hand it.
//
// So this test pins the PREMISE that makes the guard vacuous today: the encoded depth is
// a constant, whatever a consumer names its nodes. The day someone adds a nested or
// consumer-supplied field to the envelope, this reds, and the reader is sent to the
// depth guard that then has a live axis. That is the only durable thing available here:
// a check that reds when the honest note above stops being true.
//
// The guard's own bite is a MUTATION bite, run in a throwaway worktree and reported with
// its refusal message, for the same reason: no legal input can red it.
func TestBoundaryEnvelope_EncodedDepthIsConstant(t *testing.T) {
	// Node names chosen to break a naive depth scan if one were ever substituted: JSON
	// punctuation, unbalanced brackets, escapes and a long bracket run, all inside
	// strings where the scanner must not count them.
	hostile := []boundaryDecl{
		{doer: `{"a":[[[`, verifier: strings.Repeat("[", 20000), sink: "\\\"}]}"},
		{doer: "d", verifier: "v", sink: "s"},
	}

	enc, err := encodeBoundaryEnvelope(hostile)
	require.NoError(t, err, "string contents are not nesting, so no name can trip the depth guard")

	require.Equal(t, 3, jsonNestingDepth([]byte(enc)),
		"the envelope is {object, array, object} = 3 levels and nothing a consumer supplies can deepen it; "+
			"if this changed, the depth guard in encodeBoundaryEnvelope now has a live axis and needs a real bite")

	// And the names really did survive — otherwise the constant depth would be evidence
	// of truncation rather than of string-skipping.
	back, derr := decodeBoundaryEnvelope(enc)
	require.NoError(t, derr)
	require.Equal(t, strings.Repeat("[", 20000), back.Boundaries[0].Verifier)
}
