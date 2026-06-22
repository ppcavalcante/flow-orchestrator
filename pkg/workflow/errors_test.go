package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrorTaxonomy is the API-04 acceptance test for DEC-M3-error-taxonomy:
// each named boundary site returns an error that errors.Is-matches its sentinel
// category, and the corrupt-data boundary leaks neither the file path nor raw
// internals.
func TestErrorTaxonomy(t *testing.T) {
	t.Run("ErrValidation_emptyID", func(t *testing.T) {
		// Validation is rejected before any I/O, uniformly across stores.
		stores := map[string]WorkflowStore{
			"inmemory": NewInMemoryStore(),
		}
		js, err := NewJSONFileStore(t.TempDir())
		require.NoError(t, err)
		stores["json"] = js
		fbs, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		stores["fb"] = fbs

		for name, store := range stores {
			t.Run(name, func(t *testing.T) {
				_, loadErr := store.Load("")
				assert.ErrorIs(t, loadErr, ErrValidation, "Load(\"\") must be ErrValidation")

				delErr := store.Delete("")
				assert.ErrorIs(t, delErr, ErrValidation, "Delete(\"\") must be ErrValidation")

				saveErr := store.Save(nil)
				assert.ErrorIs(t, saveErr, ErrValidation, "Save(nil) must be ErrValidation")
			})
		}
	})

	t.Run("ErrValidation_traversalID", func(t *testing.T) {
		store, err := NewJSONFileStore(t.TempDir())
		require.NoError(t, err)
		_, loadErr := store.Load("../escape")
		assert.ErrorIs(t, loadErr, ErrValidation, "a path-traversal ID must be ErrValidation")
	})

	t.Run("ErrNotFound_missing", func(t *testing.T) {
		stores := map[string]WorkflowStore{
			"inmemory": NewInMemoryStore(),
		}
		js, err := NewJSONFileStore(t.TempDir())
		require.NoError(t, err)
		stores["json"] = js
		fbs, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		stores["fb"] = fbs

		for name, store := range stores {
			t.Run(name, func(t *testing.T) {
				_, loadErr := store.Load("does-not-exist")
				assert.ErrorIs(t, loadErr, ErrNotFound, "Load of a missing workflow must be ErrNotFound")
				assert.NotErrorIs(t, loadErr, ErrIO, "a missing workflow is not an I/O error")
			})
		}
	})

	t.Run("ErrCorruptData_malformedFB", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir)
		require.NoError(t, err)

		// Write garbage bytes under the expected .fb name so the FB accessors
		// panic and the recover path converts it to ErrCorruptData.
		const id = "garbage"
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".fb"), []byte("not a flatbuffer at all, just bytes"), 0600))

		_, loadErr := store.Load(id)
		require.Error(t, loadErr)
		assert.ErrorIs(t, loadErr, ErrCorruptData, "a malformed .fb must be ErrCorruptData")

		// Exposure rule: the boundary message must not leak the file path or the
		// base directory.
		assert.NotContains(t, loadErr.Error(), dir, "corrupt-data error must not leak the base dir / file path")
	})

	t.Run("ErrCorruptData_malformedJSON", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewJSONFileStore(dir)
		require.NoError(t, err)

		const id = "badjson"
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), []byte("{ this is not valid json "), 0600))

		_, loadErr := store.Load(id)
		require.Error(t, loadErr)
		assert.ErrorIs(t, loadErr, ErrCorruptData, "a malformed .json must be ErrCorruptData")
		assert.NotContains(t, loadErr.Error(), dir, "corrupt-data error must not leak the base dir / file path")
	})

	t.Run("ErrIO_writeFailure", func(t *testing.T) {
		// Make the store directory read-only so a Save write fails with a
		// non-not-found I/O error. Skipped if the platform/uid can still write
		// (e.g. running as root).
		dir := t.TempDir()
		store, err := NewJSONFileStore(dir)
		require.NoError(t, err)

		if err := os.Chmod(dir, 0500); err != nil {
			t.Skipf("cannot chmod temp dir: %v", err)
		}
		// Best-effort perm restore in cleanup; error genuinely ignorable.
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) }) //nolint:errcheck // best-effort cleanup

		data := NewWorkflowData("io-fail")
		saveErr := store.Save(data)
		if saveErr == nil {
			t.Skip("write unexpectedly succeeded (likely running as root); cannot exercise ErrIO")
		}
		assert.ErrorIs(t, saveErr, ErrIO, "a write failure must be ErrIO")
		assert.NotErrorIs(t, saveErr, ErrNotFound)
		// M3-SEC-01: the ErrIO boundary message must not leak the path either.
		assert.NotContains(t, saveErr.Error(), dir, "ErrIO error must not leak the base dir / file path")
	})

	// ErrIO_noPathLeak is the M3-SEC-01 regression guard. It deterministically
	// triggers an ErrIO (EISDIR — a directory where the workflow file is
	// expected, so os.ReadFile fails with a path-bearing *os.PathError that is
	// NOT fs.ErrNotExist) and asserts the two-part contract: (1) the boundary
	// Error() string carries no absolute path, yet (2) the underlying os error
	// stays reachable via errors.As for debugging. This is the same discipline
	// already applied to ErrCorruptData, now extended to ErrIO.
	t.Run("ErrIO_noPathLeak", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir)
		require.NoError(t, err)

		// Create a DIRECTORY at the path where the ".fb" file is expected, so
		// os.ReadFile returns an EISDIR *os.PathError (path-bearing, not
		// fs.ErrNotExist).
		const id = "eisdir"
		require.NoError(t, os.Mkdir(filepath.Join(dir, id+".fb"), 0700))

		_, loadErr := store.Load(id)
		require.Error(t, loadErr)
		// Category: ErrIO, and explicitly NOT not-found (EISDIR is not ENOENT).
		assert.ErrorIs(t, loadErr, ErrIO, "EISDIR read must be ErrIO")
		assert.NotErrorIs(t, loadErr, ErrNotFound)
		// (1) No path leak in the boundary string.
		assert.NotContains(t, loadErr.Error(), dir, "ErrIO error must not leak the base dir / file path")
		// (2) The underlying os error stays reachable for debugging.
		var pathErr *os.PathError
		assert.ErrorAs(t, loadErr, &pathErr, "underlying *os.PathError must remain reachable via errors.As")
	})

	t.Run("StoreSentinelsDistinctFromActionSentinels", func(t *testing.T) {
		// The two taxonomies are intentionally not aliased.
		assert.NotErrorIs(t, ErrNotFound, ErrInputNotFound)
		assert.NotErrorIs(t, ErrValidation, ErrInvalidInput)
		assert.False(t, errors.Is(ErrInputNotFound, ErrNotFound))
	})

	t.Run("RoundTripDoesNotErrorIs", func(t *testing.T) {
		// A successful save+load must not surface any taxonomy sentinel.
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		data := NewWorkflowData("ok")
		data.Set("k", int64(7))
		require.NoError(t, store.Save(data))

		got, loadErr := store.Load("ok")
		require.NoError(t, loadErr)
		require.NotNil(t, got)
		assert.False(t, errors.Is(loadErr, ErrNotFound) || errors.Is(loadErr, ErrCorruptData))
	})

	// Guard against accidental message leak: build a representative corrupt
	// error and confirm the generic phrasing.
	t.Run("CorruptMessageIsGeneric", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "g.fb"), []byte("xxxx"), 0600))
		_, loadErr := store.Load("g")
		require.Error(t, loadErr)
		assert.True(t, strings.Contains(loadErr.Error(), "malformed"),
			"corrupt message should be the generic 'malformed ...' phrasing, got: %s", loadErr.Error())
	})
}
