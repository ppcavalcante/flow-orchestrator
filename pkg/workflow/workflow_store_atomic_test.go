package workflow

// M9 chunk 1 — atomic file writes (torn-write guard).
//
// Both file stores previously wrote via a bare os.WriteFile, which a crash
// mid-write can leave torn/partial. writeFileAtomic writes to a temp file in the
// same directory, fsyncs, and renames over the target — so a crash or error
// leaves either the prior file fully intact or the new file fully written, never
// a torn mix. These tests pin that contract directly and through both stores.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leftoverTempCount counts files in dir whose name matches the atomic-write temp
// pattern (".<base>.tmp-*"). A successful or failed write must leave zero.
func leftoverTempCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			n++
		}
	}
	return n
}

// TestWriteFileAtomic_WritesCompleteFile: the happy path writes exactly the bytes
// with the requested permission and leaves no temp file behind.
func TestWriteFileAtomic_WritesCompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	content := []byte(`{"id":"wf","data":{"k":1}}`)

	require.NoError(t, writeFileAtomic(path, content, 0600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, got, "atomic write must persist the exact bytes")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "permission must match")

	assert.Equal(t, 0, leftoverTempCount(t, dir), "no temp file may be left after a successful write")
}

// TestWriteFileAtomic_OverwriteIsAtomic: overwriting an existing file replaces it
// fully (the rename-over path), no leftover temp.
func TestWriteFileAtomic_OverwriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")

	require.NoError(t, writeFileAtomic(path, []byte("VERSION-1"), 0600))
	require.NoError(t, writeFileAtomic(path, []byte("VERSION-2-longer"), 0600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "VERSION-2-longer", string(got))
	assert.Equal(t, 0, leftoverTempCount(t, dir))
}

// TestWriteFileAtomic_ErrorLeavesOriginalIntact: when the atomic write fails
// (here: the rename target is a directory, so os.Rename fails), the ORIGINAL file
// is unchanged — no torn/partial output — and no temp file is left behind. This
// is the core torn-write guarantee.
func TestWriteFileAtomic_ErrorLeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()

	// "target" is a non-empty directory: renaming a file onto it fails on POSIX,
	// which drives writeFileAtomic's error path AFTER the temp file is written.
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0750))
	require.NoError(t, os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0600))

	err := writeFileAtomic(target, []byte("NEW-CONTENT"), 0600)
	require.Error(t, err, "renaming over a non-empty directory must fail")

	// The original directory (the "prior state") is intact: still a directory,
	// child still present — the failed write corrupted nothing.
	info, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir(), "the original target must be untouched on write failure")
	child, readErr := os.ReadFile(filepath.Join(target, "child"))
	require.NoError(t, readErr)
	assert.Equal(t, "x", string(child))

	// No leftover temp file from the aborted write.
	assert.Equal(t, 0, leftoverTempCount(t, dir), "a failed write must not leave a temp file")
}

// TestWriteFileAtomic_PreservesExistingFileOnFailure: a stronger version using a
// real prior file. We write v1, then attempt a failing atomic write to a path we
// force to fail, and confirm v1 is byte-for-byte intact. We induce failure by
// pointing at a path whose directory does not exist (CreateTemp fails up front),
// so the original file is never touched.
func TestWriteFileAtomic_PreservesExistingFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	require.NoError(t, writeFileAtomic(path, []byte("ORIGINAL"), 0600))

	// A path inside a non-existent subdirectory: CreateTemp(dir,...) fails because
	// the dir doesn't exist, so the write aborts before touching anything.
	badPath := filepath.Join(dir, "nope", "wf.json")
	err := writeFileAtomic(badPath, []byte("SHOULD-NOT-LAND"), 0600)
	require.Error(t, err)

	// The real prior file is untouched.
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "ORIGINAL", string(got))
	assert.Equal(t, 0, leftoverTempCount(t, dir))
}

// TestWriteFileAtomic_FailedOverwriteDoesNotTruncate is the DISCRIMINATING test:
// it distinguishes the temp+rename helper from a bare os.WriteFile. A direct
// os.WriteFile opens the existing file with O_TRUNC, so a write that fails AFTER
// truncation leaves the file torn (empty/short). writeFileAtomic never touches
// the original until the final rename, so a failure that aborts the new write
// leaves the original's content byte-for-byte intact.
//
// We induce failure by making the directory read-only after the original exists.
// The atomic helper cannot create its temp file there, so it fails UP FRONT and
// never touches the original. A bare os.WriteFile, by contrast, opens the
// EXISTING file (which is itself writable — the read-only dir only blocks new
// entries, not opening an existing one) with O_TRUNC and succeeds, destroying the
// original. Mutation-verified: reverting writeFileAtomic to a direct os.WriteFile
// makes this test fail (the overwrite succeeds and the original is lost), so the
// test genuinely discriminates the atomic path from the torn-write one.
func TestWriteFileAtomic_FailedOverwriteDoesNotTruncate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	original := []byte("ORIGINAL-COMPLETE-CONTENT")
	require.NoError(t, writeFileAtomic(path, original, 0600))

	// Make the directory read-only so a new temp file cannot be created.
	require.NoError(t, os.Chmod(dir, 0500))
	// Restore writability at cleanup so t.TempDir's removal works.
	t.Cleanup(func() {
		if chmodErr := os.Chmod(dir, 0700); chmodErr != nil {
			t.Logf("cleanup chmod failed: %v", chmodErr)
		}
	})

	err := writeFileAtomic(path, []byte("NEW"), 0600)
	require.Error(t, err, "write into a read-only directory must fail")

	// Restore write so we can read back (read doesn't need it, but be safe).
	require.NoError(t, os.Chmod(dir, 0700))
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, string(original), string(got),
		"a failed atomic write must leave the original file's content fully intact (no truncation)")
}

// TestJSONFileStore_SaveLoadRoundTripAtomic: Save (now atomic) → Load is correct
// and leaves no temp file in the store dir.
func TestJSONFileStore_SaveLoadRoundTripAtomic(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	data := NewWorkflowData("round-trip")
	data.Set("count", 42)
	data.SetNodeStatus("n1", Completed)
	require.NoError(t, store.Save(data))

	loaded, err := store.Load("round-trip")
	require.NoError(t, err)
	v, ok := loaded.GetInt("count")
	require.True(t, ok)
	assert.Equal(t, 42, v)
	st, _ := loaded.GetNodeStatus("n1")
	assert.Equal(t, Completed, st)

	assert.Equal(t, 0, leftoverTempCount(t, dir), "Save must not leave a temp file")
}

// TestFlatBuffersStore_SaveLoadRoundTripAtomic: same for the FlatBuffers store.
func TestFlatBuffersStore_SaveLoadRoundTripAtomic(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)

	data := NewWorkflowData("fb-round-trip")
	data.Set("count", 7)
	data.SetNodeStatus("n1", Completed)
	require.NoError(t, store.Save(data))

	loaded, err := store.Load("fb-round-trip")
	require.NoError(t, err)
	v, ok := loaded.GetInt("count")
	require.True(t, ok)
	assert.Equal(t, 7, v)
	st, _ := loaded.GetNodeStatus("n1")
	assert.Equal(t, Completed, st)

	assert.Equal(t, 0, leftoverTempCount(t, dir), "Save must not leave a temp file")
}

// --- M9 chunk 3: writeFileAtomic Write/Sync/Chmod/Close error branches ------
//
// These exercise the torn-write-guard error returns that are impossible to drive
// with a real on-disk file (a write to a freshly created temp file does not fail
// on demand). Each test injects a failure on ONE step via the createTempFile seam,
// wrapping a REAL temp file so Name()/cleanup behave normally, and asserts: (1) the
// expected error surfaces, (2) a pre-existing target file is left byte-for-byte
// intact (the rename never happened), and (3) no temp file is left behind (the
// error-path defer cleaned it up).

// failingTempFile wraps a real *os.File but forces the named step to fail. The
// underlying real file backs Name() (so the defer's os.Remove targets a real
// path) and any steps that are NOT the injected one (so data/sync behave normally
// up to the failure point).
type failingTempFile struct {
	*os.File
	failOn string // "write" | "sync" | "chmod" | "close"
}

func (f *failingTempFile) Write(p []byte) (int, error) {
	if f.failOn == "write" {
		return 0, fmt.Errorf("injected write failure")
	}
	return f.File.Write(p)
}

func (f *failingTempFile) Sync() error {
	if f.failOn == "sync" {
		return fmt.Errorf("injected sync failure")
	}
	return f.File.Sync()
}

func (f *failingTempFile) Chmod(mode os.FileMode) error {
	if f.failOn == "chmod" {
		return fmt.Errorf("injected chmod failure")
	}
	return f.File.Chmod(mode)
}

func (f *failingTempFile) Close() error {
	if f.failOn == "close" {
		// Close the real file (release the fd) but report failure once, so the
		// helper's error branch is taken; the defer's second Close is a no-op.
		f.File.Close() //nolint:errcheck,gosec // releasing the fd; the injected error is what we report
		return fmt.Errorf("injected close failure")
	}
	return f.File.Close()
}

func TestWriteFileAtomic_InjectedStepFailures(t *testing.T) {
	cases := []struct {
		failOn  string
		wantMsg string
	}{
		{"write", "write temp file"},
		{"sync", "sync temp file"},
		{"chmod", "chmod temp file"},
		{"close", "close temp file"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.failOn, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wf.json")
			original := []byte("ORIGINAL-COMPLETE-CONTENT")
			require.NoError(t, writeFileAtomic(path, original, 0600))

			// Inject the failure for the duration of this subtest only.
			prev := createTempFile
			createTempFile = func(d, pattern string) (atomicTempFile, error) {
				f, err := os.CreateTemp(d, pattern)
				if err != nil {
					return nil, err
				}
				return &failingTempFile{File: f, failOn: tc.failOn}, nil
			}
			t.Cleanup(func() { createTempFile = prev })

			err := writeFileAtomic(path, []byte("NEW-CONTENT-THAT-MUST-NOT-LAND"), 0600)
			require.Error(t, err, "an injected %s failure must surface as an error", tc.failOn)
			assert.Contains(t, err.Error(), tc.wantMsg,
				"the error must name the failing step (%s)", tc.failOn)

			// The original file is byte-for-byte intact — the rename never happened.
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, string(original), string(got),
				"a failed atomic write (%s) must leave the original fully intact (no torn write)", tc.failOn)

			// The error-path defer removed the temp file.
			assert.Equal(t, 0, leftoverTempCount(t, dir),
				"a failed atomic write (%s) must leave no temp file behind", tc.failOn)
		})
	}
}

// TestFlatBuffersStore_SaveCheckpoint covers the FB Checkpointer path (M9): a
// checkpoint is an atomic whole-snapshot Save, so SaveCheckpoint must round-trip
// through Load identically to Save.
func TestFlatBuffersStore_SaveCheckpoint(t *testing.T) {
	store := newFBStore(t)
	var _ Checkpointer = store // compile-time: FB store implements Checkpointer

	d := NewWorkflowData("fb-cp")
	d.Set("n", int64(1<<40))
	d.SetNodeStatus("a", Completed)
	d.SetOutput("a", "out-a")
	require.NoError(t, store.SaveCheckpoint(d))

	got, err := store.Load("fb-cp")
	require.NoError(t, err)
	v, ok := got.GetInt64("n")
	require.True(t, ok)
	assert.Equal(t, int64(1<<40), v)
	st, _ := got.GetNodeStatus("a")
	assert.Equal(t, Completed, st)
	out, ok := got.GetOutput("a")
	require.True(t, ok)
	assert.Equal(t, "out-a", out)
}
