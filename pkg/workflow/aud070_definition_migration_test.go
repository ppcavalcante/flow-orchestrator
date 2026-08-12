package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-070 / O-03: a definition-digest mismatch is a TYPED error a host can classify,
// and WithDefinitionMigration lets a host reject / accept / transform instead of only
// restarting from scratch.

// buildAUD070 runs graph G1 (a->b) once to persist + stamp its digest, then returns a
// resume-ready Workflow for G2 (a->b->c) over the same store, optionally with a handler.
func buildAUD070(t *testing.T, store *InMemoryStore, wf string, mig DefinitionMigration) *Workflow {
	t.Helper()
	build := func(withExtra bool) *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID(wf)
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
	require.NoError(t, build(false).Execute(context.Background()), "first run under G1 stamps its digest")
	g2 := build(true)
	if mig != nil {
		g2.WithDefinitionMigration(mig)
	}
	return g2
}

func TestAUD070_DefaultRejectIsTyped(t *testing.T) {
	err := buildAUD070(t, NewInMemoryStore(), "wf-typed", nil).Execute(context.Background())
	require.Error(t, err)

	// Typed: errors.As reveals the digests; both sentinels match.
	var mm *DefinitionMismatchError
	require.True(t, errors.As(err, &mm), "the mismatch is a *DefinitionMismatchError")
	require.Equal(t, "wf-typed", mm.WorkflowID)
	require.NotEmpty(t, mm.PersistedDigest)
	require.NotEmpty(t, mm.CurrentDigest)
	require.NotEqual(t, mm.PersistedDigest, mm.CurrentDigest, "the two digests genuinely differ")
	require.ErrorIs(t, err, ErrDefinitionChanged, "new specific sentinel matches")
	require.ErrorIs(t, err, ErrValidation, "the pre-AUD-070 generic classifier still matches")
}

func TestAUD070_MigrationAcceptsChange(t *testing.T) {
	var sawMismatch DefinitionMismatch
	accept := func(mm DefinitionMismatch, _ *WorkflowData) error {
		sawMismatch = mm
		return nil // accept the changed graph as-is
	}
	w := buildAUD070(t, NewInMemoryStore(), "wf-accept", accept)
	require.NoError(t, w.Execute(context.Background()), "an accepting handler lets the resume proceed")
	require.Equal(t, "wf-accept", sawMismatch.WorkflowID, "the handler saw the mismatch")
	require.NotEqual(t, sawMismatch.PersistedDigest, sawMismatch.CurrentDigest)
}

func TestAUD070_MigrationTransformsState(t *testing.T) {
	transform := func(_ DefinitionMismatch, data *WorkflowData) error {
		data.Set("migrated", "yes") // rehydrate state to match the new graph
		return nil
	}
	store := NewInMemoryStore()
	w := buildAUD070(t, store, "wf-transform", transform)
	require.NoError(t, w.Execute(context.Background()))

	final, err := store.Load("wf-transform")
	require.NoError(t, err)
	got, ok := final.Get("migrated")
	require.True(t, ok)
	require.Equal(t, "yes", got, "the handler's in-place transform persisted with the resumed run")
}

func TestAUD070_MigrationRejectsWithOwnError(t *testing.T) {
	sentinel := errors.New("host says no")
	reject := func(_ DefinitionMismatch, _ *WorkflowData) error { return sentinel }
	err := buildAUD070(t, NewInMemoryStore(), "wf-reject", reject).Execute(context.Background())
	require.ErrorIs(t, err, sentinel, "a rejecting handler's error propagates verbatim")
}
