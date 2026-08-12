package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// aud037NilNilStore is a WorkflowStore whose Load returns (nil, nil) -- the exact
// contract violation AUD-037 is about. A well-behaved store signals "no prior state"
// with ErrNotFound, never a nil payload and a nil error.
type aud037NilNilStore struct{}

func (aud037NilNilStore) Save(*WorkflowData) error           { return nil }
func (aud037NilNilStore) Load(string) (*WorkflowData, error) { return nil, nil } //nolint:nilnil // this IS the tested contract violation
func (aud037NilNilStore) ListWorkflows() ([]string, error)   { return nil, nil }
func (aud037NilNilStore) Delete(string) error                { return nil }

// AUD-037 / P-06: a Store returning (nil, nil) from Load was silently treated as fresh
// state. That would start the run over and overwrite real persisted state on the next
// Save. ErrNotFound is the ONLY legal "fresh" signal; a nil/nil return must surface as a
// typed store-contract violation, not masquerade as a clean first run.
func TestAUD037_NilNilLoadIsRejected(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("wf-037")
	b.AddStartNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	w, err := FromBuilder(b)
	require.NoError(t, err)
	w.Store = aud037NilNilStore{}

	err = w.Execute(context.Background())
	require.Error(t, err, "a (nil, nil) Load must not be treated as fresh state")
	require.ErrorIs(t, err, ErrCorruptData, "AUD-037: (nil, nil) is a store-contract violation")
}
