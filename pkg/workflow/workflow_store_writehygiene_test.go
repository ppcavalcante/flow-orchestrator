package workflow

// Two write/read hygiene defects found by review of the HYG-00 work. Both predate
// this phase but became visible because the new code does the same things correctly
// next door — the strongest argument for fixing them rather than leaving the
// inconsistency to be copied.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSaveToJSON_IsAtomic pins that SaveToJSON writes through writeFileAtomic like the
// four other snapshot writers. It was the ONE path still on a plain os.WriteFile, so a
// crash mid-write could leave a truncated file that LoadFromJSON then rejects — the
// same "durable state you cannot read back" family HYG-00 exists to close, by a
// different route.
//
// The bite is behavioural, not structural: with a failing temp-file seam, an ATOMIC
// writer leaves the previous file intact, while os.WriteFile would have truncated it.
func TestSaveToJSON_IsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wd.json")

	first := NewWorkflowData("wf")
	first.Set("generation", "ORIGINAL")
	require.NoError(t, first.SaveToJSON(path))

	original, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	require.NoError(t, err)
	require.Contains(t, string(original), "ORIGINAL")

	// Force the write to fail partway: the atomic writer must leave the previous
	// file untouched. A non-atomic os.WriteFile truncates in place before failing.
	prev := createTempFile
	createTempFile = func(d, pattern string) (atomicTempFile, error) {
		f, ferr := os.CreateTemp(d, pattern)
		if ferr != nil {
			return nil, ferr
		}
		return &failingTempFile{File: f, failOn: "write"}, nil
	}
	t.Cleanup(func() { createTempFile = prev })

	second := NewWorkflowData("wf")
	second.Set("generation", "REPLACEMENT")
	require.Error(t, second.SaveToJSON(path), "the seeded write failure must surface")

	after, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	require.NoError(t, err, "the previous file must still exist — an atomic write leaves it intact")
	assert.Equal(t, string(original), string(after),
		"a failed SaveToJSON must not damage the file already on disk")

	// And it must still be loadable, which is the property that actually matters.
	back := NewWorkflowData("wf")
	require.NoError(t, back.LoadFromJSON(path))
	got, ok := back.Get("generation")
	require.True(t, ok)
	assert.Equal(t, "ORIGINAL", got)
}

// closeErrReader is a ReadCloser whose Close fails after a successful read.
type closeErrReader struct {
	io.Reader
	closeErr error
}

func (c *closeErrReader) Close() error { return c.closeErr }

// TestJSONFileStoreLoad_SurfacesCloseError pins that JSONFileStore.Load actually
// returns a Close error. Its returns were UNNAMED, so the deferred `err = ...`
// assigned a local the already-determined return value ignored — the Close error was
// silently dropped despite the comment claiming it was surfaced. FlatBuffersStore.Load
// has always had named returns; this makes the two agree.
func TestJSONFileStoreLoad_SurfacesCloseError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	data := NewWorkflowData("wf")
	data.Set("k", "v")
	require.NoError(t, store.Save(data))

	boom := errors.New("close failed")
	prev := openForRead
	openForRead = func(path string) (io.ReadCloser, error) {
		rc, oerr := prev(path)
		if oerr != nil {
			return nil, oerr
		}
		return &closeErrReader{Reader: rc, closeErr: boom}, nil
	}
	t.Cleanup(func() { openForRead = prev })

	got, err := store.Load("wf")
	require.Error(t, err,
		"a Close failure on an otherwise-successful Load must reach the caller, not be dropped")
	assert.ErrorIs(t, err, boom, "the underlying Close error must stay reachable")
	assert.True(t, strings.Contains(err.Error(), "wf"), "the error should name the workflow")
	_ = got
}

// TestJSONFileStoreLoad_ReadErrorTakesPrecedence is the other half: a real read/parse
// failure must still win over a Close error, so naming the returns did not reorder the
// error precedence the comment promises.
func TestJSONFileStoreLoad_ReadErrorTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	// A corrupt file on disk: the parse failure is the real error.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0600))

	boom := errors.New("close failed")
	prev := openForRead
	openForRead = func(path string) (io.ReadCloser, error) {
		rc, oerr := prev(path)
		if oerr != nil {
			return nil, oerr
		}
		return &closeErrReader{Reader: rc, closeErr: boom}, nil
	}
	t.Cleanup(func() { openForRead = prev })

	_, err = store.Load("bad")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptData, "the parse failure must take precedence over the Close error")
	assert.NotErrorIs(t, err, boom, "the Close error must not mask the real failure")
}

// TestSaveToJSON_PreservesExistingFileMode pins the mode-preservation half of the
// atomic-write move. writeFileAtomic creates a fresh temp file and renames over the
// target, so without an explicit Stat the consumer's chmod is silently reset to
// 0600 — and this format has a known out-of-tree consumer who would get no signal.
func TestSaveToJSON_PreservesExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wd.json")
	d := NewWorkflowData("wf")
	d.Set("k", "v")

	// Create it, then set a distinctive mode the consumer chose deliberately.
	require.NoError(t, d.SaveToJSON(path))
	require.NoError(t, os.Chmod(path, 0640))

	d.Set("k", "v2")
	require.NoError(t, d.SaveToJSON(path))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), fi.Mode().Perm(),
		"an existing file's mode must survive the atomic write, not be reset to 0600")
}

// TestSaveToJSON_NewFileIsPrivate pins the other half: absent file → 0600. A guard
// that preserved modes but also loosened the create case would be a regression.
func TestSaveToJSON_NewFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.json")
	d := NewWorkflowData("wf")
	d.Set("k", "v")
	require.NoError(t, d.SaveToJSON(path))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm(),
		"a newly created snapshot must default to 0600")
}

// deepPayload builds a value nested `levels` deep.
func deepPayload(levels int) any {
	var v any = "leaf"
	for i := 0; i < levels; i++ {
		v = map[string]any{"n": v}
	}
	return v
}

// TestSaveToJSON_RefusesOverDepth is the SaveToJSON half of the third axis (review
// F2). encoding/json marshals unbounded but its DECODER caps nesting, so a document
// deeper than that limit wrote fine and then failed LoadFromJSON forever — the same
// write-succeeds/read-wedges shape as the byte and element axes.
//
// Unlike those two this ceiling has NO knob: it belongs to the stdlib decoder, not to
// us, so a deeper document is unreadable under any configuration. The guard converts
// a permanent silent wedge into a loud write-time failure, which is the whole of what
// is available here.
func TestSaveToJSON_RefusesOverDepth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep.json")
	d := NewWorkflowData("deepwf")
	d.Set("payload", deepPayload(maxJSONNestingDepth+16))

	err := d.SaveToJSON(path)
	require.Error(t, err, "an over-depth document must be refused at the write, not wedged at the read")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), "max nesting depth")
	assert.Contains(t, err.Error(), "deepwf", "the error must name the subject")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")
}

// TestSaveToJSON_AtDepthStillRoundTrips is the non-over-broad arm: a document under
// the ceiling must still write AND read back. A guard that rejected legitimate depth
// would be a regression, and this is the pair that would catch an off-by-one in
// whichever direction it fell.
func TestSaveToJSON_AtDepthStillRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.json")
	d := NewWorkflowData("okwf")
	d.Set("payload", deepPayload(64))

	require.NoError(t, d.SaveToJSON(path), "a modest nesting depth must still be accepted")

	back := NewWorkflowData("okwf")
	require.NoError(t, back.LoadFromJSON(path), "and must read back — the guard tracks the decoder, not a guess")
}
