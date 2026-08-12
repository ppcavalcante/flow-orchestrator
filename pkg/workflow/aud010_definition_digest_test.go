package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-010 / C-07: the resume graph-identity guard was a node-NAME subset check —
// it rejected a removed node but silently mis-resumed onto an added node, a
// changed edge/policy/compensation/boundary/action-kind, or a suspendability
// change. DefinitionDigest is the structural identity that closes that gap.

func digestNoop() ActionFunc {
	return ActionFunc(func(context.Context, *WorkflowData) error { return nil })
}

// baseDigestDAG builds a small canonical graph; mutate returns a builder variant.
func buildDigest(t *testing.T, mutate func(b *WorkflowBuilder)) *DAG {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID("wf")
	b.AddStartNode("a").WithAction(digestNoop())
	b.AddNode("b").WithAction(digestNoop()).DependsOn("a")
	if mutate != nil {
		mutate(b)
	}
	dag, err := b.Build()
	require.NoError(t, err)
	return dag
}

func TestAUD010_DigestIsDeterministic(t *testing.T) {
	d1 := buildDigest(t, nil).DefinitionDigest()
	d2 := buildDigest(t, nil).DefinitionDigest()
	require.Equal(t, d1, d2, "two builds of the same definition must produce the same digest")
	require.NotEmpty(t, d1)
}

func TestAUD010_DigestChangesOnEveryDefinitionChange(t *testing.T) {
	base := buildDigest(t, nil).DefinitionDigest()

	cases := []struct {
		name   string
		mutate func(b *WorkflowBuilder)
	}{
		{"added node", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b")
		}},
		{"changed edge", func(b *WorkflowBuilder) {
			// a third node whose edge wiring differs from base's shape
			b.AddNode("c").WithAction(digestNoop()).DependsOn("a")
		}},
		{"retry policy", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b").WithRetries(3)
		}},
		{"timeout policy", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b").WithTimeout(5 * time.Second)
		}},
		{"continue-on-error", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b").WithContinueOnError()
		}},
		{"compensation", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b").WithCompensation(digestNoop())
		}},
		{"boundary", func(b *WorkflowBuilder) {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b")
			b.WithBoundary("a", "b", "c")
		}},
		{"action kind", func(b *WorkflowBuilder) {
			// b's action becomes a composite of the func rather than the bare func
			b.AddNode("c").WithAction(NewCompositeAction(digestNoop())).DependsOn("b")
		}},
	}

	seen := map[string]string{base: "base"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := buildDigest(t, c.mutate).DefinitionDigest()
			require.NotEqual(t, base, d, "%s must change the definition digest", c.name)
			if prev, dup := seen[d]; dup {
				t.Fatalf("%s produced the same digest as %q — the change is invisible to the digest", c.name, prev)
			}
			seen[d] = c.name
		})
	}
}

// End-to-end: a workflow persisted under one graph must refuse to resume under a
// changed graph, via the digest stamped into the checkpoint.
func TestAUD010_ResumeRejectsChangedGraph(t *testing.T) {
	store := NewInMemoryStore()

	build := func(withExtra bool) *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID("wf")
		b.AddStartNode("a").WithAction(digestNoop())
		b.AddNode("b").WithAction(digestNoop()).DependsOn("a")
		if withExtra {
			b.AddNode("c").WithAction(digestNoop()).DependsOn("b")
		}
		w, err := FromBuilder(b)
		require.NoError(t, err)
		w.Store = store
		return w
	}

	// First run persists under graph G1 (a->b), stamping its digest.
	require.NoError(t, build(false).Execute(context.Background()))

	// Resuming the SAME graph is fine.
	require.NoError(t, build(false).Execute(context.Background()))

	// Resuming a CHANGED graph (adds c, no node removed) must be refused by the
	// digest — the node-name check alone would pass since {a,b} still exist.
	err := build(true).Execute(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
}
