package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-015 / P-02: JSONFileStore.Load("A") decoded a snapshot whose payload ID was
// "B" and returned WorkflowData.ID == "B". A later Save(data) then writes under
// "B", redirecting a misplaced/copied/forged "A" file's writes into another valid
// workflow. The lookup KEY is authoritative — a payload-ID mismatch must be
// rejected as ErrCorruptData, not silently honored.
func TestAUD015_JSONLoadRejectsIDMismatch(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	// Persist a workflow with ID "B" (writes B.json with payload ID "B").
	b := NewWorkflowData("B")
	b.Set("k", "from-B")
	require.NoError(t, store.Save(b))

	// A misplaced/copied file: the "B" payload now sits at key "A".
	raw, err := os.ReadFile(filepath.Join(dir, "B.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "A.json"), raw, 0o600))

	loaded, err := store.Load("A")
	require.Error(t, err, "Load must reject a payload whose ID disagrees with the lookup key")
	require.ErrorIs(t, err, ErrCorruptData)
	require.Nil(t, loaded)
}

// The normal path — a payload whose ID matches its key — still loads.
func TestAUD015_JSONLoadAcceptsMatchingID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	a := NewWorkflowData("A")
	a.Set("k", "from-A")
	require.NoError(t, store.Save(a))

	loaded, err := store.Load("A")
	require.NoError(t, err)
	require.Equal(t, "A", loaded.GetWorkflowID())
	v, ok := loaded.Get("k")
	require.True(t, ok)
	require.Equal(t, "from-A", v)
}
