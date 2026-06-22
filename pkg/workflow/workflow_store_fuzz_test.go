package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzFlatBuffersStoreLoad fuzzes the layered bounds guard on the single FB
// decode (FlatBuffersStore.Load). The property under test is the availability
// guarantee DEC-M4-mechanism makes, NOT value correctness:
//
//	Load of ANY byte sequence → never panics, and if it returns err == nil the
//	returned *WorkflowData is non-nil and self-consistent (its accessors do not
//	panic when read back).
//
// A nil-error Load of a well-formed-but-arbitrary buffer is allowed — the guard
// is a bounds guard, not a structural verifier (the disclosed semantic-forgery
// residual). Crashes found by `-fuzz` are minimized into
// testdata/fuzz/FuzzFlatBuffersStoreLoad/ and then replayed deterministically by
// the normal `go test` run (no -fuzz) as Decision-grade regressions.
func FuzzFlatBuffersStoreLoad(f *testing.F) {
	// Seed with the golden fixture (a real valid buffer) ...
	if golden, err := os.ReadFile(filepath.Join("testdata", "m1_format_int.fb")); err == nil {
		f.Add(golden)
	}
	// ... and each malformed family from the strengthened oracle corpus.
	for _, seed := range [][]byte{
		{},
		{0x01},
		{0x01, 0x02, 0x03},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0xf0, 0xff, 0xff, 0x0f}, // oversized root
		{0xff, 0xff, 0xff, 0x7f}, // alloc-bomb root
		{0x10, 0x00, 0x00, 0x00}, // root past EOF
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		baseDir := t.TempDir()
		store, err := NewFlatBuffersStore(baseDir)
		require.NoError(t, err)

		id := "fuzz"
		require.NoError(t, os.WriteFile(filepath.Join(baseDir, id+".fb"), payload, 0600))

		var data *WorkflowData
		require.NotPanics(t, func() {
			data, err = store.Load(id)
		}, "Load must never panic on arbitrary input")

		if err == nil {
			// Property: a clean Load returns usable, self-consistent data —
			// reading it back must not panic either.
			require.NotNil(t, data, "Load with nil error must return non-nil data")
			require.NotPanics(t, func() {
				_ = data.GetWorkflowID()
				for _, k := range data.Keys() {
					_, _ = data.Get(k)
				}
				_ = data.GetAllNodeStatuses()
			}, "data returned by a clean Load must be safe to read back")
		} else {
			// Any rejection returns no data (no partially-built object escapes).
			require.Nil(t, data, "Load with error must return data == nil")
		}
	})
}
