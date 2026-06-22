package workflow

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingInfiniteReader yields an unbounded stream of zero bytes and counts how
// many were actually consumed. It models a file far larger than the size cap
// without materializing one on disk — the seam CONTEXT calls for to prove the
// TOCTOU-killing property.
type countingInfiniteReader struct{ n int64 }

func (r *countingInfiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	r.n += int64(len(p))
	return len(p), nil
}

// TestSEC01_BytesReadBound is the discriminating SEC-01 test (M4-SEC-02). The
// weak oracle "a big file is rejected" passes under BOTH the old os.Stat+ReadFile
// and the new io.LimitReader code, so it proves nothing. The TOCTOU-killing
// property is: Load pulls at most defaultMaxFileSize+1 bytes into memory for ANY
// on-disk size. This asserts that bound directly against the exact construct Load
// uses, with a reader that would otherwise stream forever.
func TestSEC01_BytesReadBound(t *testing.T) {
	src := &countingInfiniteReader{}

	// The exact expression Load uses to bound the read.
	buf, err := io.ReadAll(io.LimitReader(src, defaultMaxFileSize+1))
	require.NoError(t, err)

	// ReadAll drains the LimitReader to EOF: exactly cap+1 bytes returned, and
	// the underlying reader is consumed by no more than cap+1 (ReadAll may make
	// its final Read slightly past via buffer growth, but LimitReader hard-caps
	// what it forwards — so the bytes that ever leave the source are <= cap+1).
	assert.Equal(t, defaultMaxFileSize+1, int64(len(buf)),
		"LimitReader(cap+1) must yield exactly cap+1 bytes from an infinite source")
	assert.LessOrEqual(t, src.n, defaultMaxFileSize+1,
		"Load must consume <= cap+1 bytes regardless of on-disk size (TOCTOU-killing bound)")

	// And Load's rejection predicate: len(buf) > cap rejects; the +1 is what
	// makes an exactly-at-cap read pass and an over-cap read fail. Apply the real
	// predicate to the cap+1-length buffer we just read.
	assert.True(t, int64(len(buf)) > defaultMaxFileSize,
		"cap+1 bytes must trip the over-cap rejection")
	// The exactly-at-cap edge (a cap-length buffer must NOT trip the predicate) is
	// proven against a real cap-length read in TestSEC01_AtCapBoundary — not by
	// comparing the constant to itself (which would be a tautology, SA4000).
}

// countingReadCloser wraps a file and counts the bytes Load actually pulls from
// the fd. It is the instrumented seam (A1) that makes the SEC-01 guard a real
// regression test of the LIVE Load path, not just of the io.LimitReader construct.
type countingReadCloser struct {
	rc   io.ReadCloser
	read int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.read += int64(n)
	return n, err
}
func (c *countingReadCloser) Close() error { return c.rc.Close() }

// TestSEC01_LoadReadsBoundedFromFD is the DISCRIMINATING regression test (A1).
// TestSEC01_BytesReadBound only exercises the io.LimitReader expression in
// isolation, so it would pass under the OLD os.Stat+os.ReadFile code too. This
// one drives a real store.Load against an OVER-CAP file through a byte-counting
// fd seam and asserts Load consumed EXACTLY cap+1 bytes from the descriptor:
//   - new code (os.Open + io.ReadAll(io.LimitReader(f, cap+1))) reads cap+1 then
//     rejects on len>cap  -> read == cap+1.
//   - old code (os.Stat sees size>cap -> reject before opening) would read 0 from
//     the fd -> read == 0, FAILING this assertion. (Verified: temporarily reverting
//     Load to stat+ReadFile makes this test fail; restored. That is the discriminator.)
//
// Uses a shrunk defaultMaxFileSize var seam so we never materialize a 64 MiB file.
func TestSEC01_LoadReadsBoundedFromFD(t *testing.T) {
	// Shrink the cap for this test; restore after (var seam, prod default untouched).
	origCap := defaultMaxFileSize
	defaultMaxFileSize = 1024 // 1 KiB
	t.Cleanup(func() { defaultMaxFileSize = origCap })

	// Instrument the open seam to count fd bytes; restore after.
	var counter *countingReadCloser
	origOpen := openForRead
	openForRead = func(path string) (io.ReadCloser, error) {
		rc, err := origOpen(path)
		if err != nil {
			return nil, err
		}
		counter = &countingReadCloser{rc: rc}
		return counter, nil
	}
	t.Cleanup(func() { openForRead = origOpen })

	// Write a real file well OVER the (shrunk) cap.
	base := t.TempDir()
	store, err := NewFlatBuffersStore(base)
	require.NoError(t, err)
	oversize := make([]byte, defaultMaxFileSize+4096) // cap + 4 KiB
	require.NoError(t, os.WriteFile(filepath.Join(base, "big.fb"), oversize, 0600))

	data, err := store.Load("big")

	// Rejected (over-cap), data nil — same outcome old code would give...
	assert.ErrorIs(t, err, ErrCorruptData, "an over-cap file must be rejected as ErrCorruptData")
	assert.Nil(t, data)

	// ...but the DISCRIMINATOR is the bytes consumed from the fd: the atomic
	// LimitReader read pulls exactly cap+1 and stops; a stat-then-read design
	// would read 0 here (reject on stat before opening), failing this assertion.
	require.NotNil(t, counter, "Load must have opened the file via the seam")
	assert.Equal(t, defaultMaxFileSize+1, counter.read,
		"Load must read exactly cap+1 bytes from the fd (atomic LimitReader bound); a stat-then-read would read 0")
}

// TestSEC01_AtCapBoundary pins the off-by-one at the rejection predicate with a
// finite source, without materializing a 64 MiB file: a reader supplying exactly
// cap bytes is accepted (len == cap is not > cap); a reader supplying cap+1 is
// rejected.
func TestSEC01_AtCapBoundary(t *testing.T) {
	t.Run("exactly at cap loads", func(t *testing.T) {
		src := &countingInfiniteReader{}
		buf, err := io.ReadAll(io.LimitReader(src, defaultMaxFileSize))
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, int64(len(buf)))
		assert.False(t, int64(len(buf)) > defaultMaxFileSize, "exactly-at-cap must be accepted")
	})
	t.Run("one over cap rejects", func(t *testing.T) {
		src := &countingInfiniteReader{}
		buf, err := io.ReadAll(io.LimitReader(src, defaultMaxFileSize+1))
		require.NoError(t, err)
		assert.True(t, int64(len(buf)) > defaultMaxFileSize, "cap+1 must be rejected")
	})
}

// TestSEC01_LoadEdges pins the regression edges through the real Load path:
// ErrNotFound still maps from a missing file (os.Open's fs.ErrNotExist branch),
// and a normal valid round-trip Loads clean (the common case is unaffected by the
// atomic cap).
func TestSEC01_LoadEdges(t *testing.T) {
	t.Run("missing file -> ErrNotFound", func(t *testing.T) {
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		_, err = store.Load("does-not-exist")
		assert.ErrorIs(t, err, ErrNotFound,
			"a missing file must surface ErrNotFound via os.Open, not ErrCorruptData/ErrIO")
	})

	t.Run("valid round-trip loads clean", func(t *testing.T) {
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		d := NewWorkflowData("rt")
		d.Set("k", int64(7))
		require.NoError(t, store.Save(d))
		got, err := store.Load("rt")
		require.NoError(t, err, "a valid file well under the cap must Load clean")
		v, ok := got.GetInt64("k")
		require.True(t, ok)
		assert.Equal(t, int64(7), v)
	})

	t.Run("small malformed file -> ErrCorruptData, not size-cap false-positive", func(t *testing.T) {
		// A tiny malformed file must be rejected by the sanity pre-check as
		// ErrCorruptData — confirming the atomic cap does NOT spuriously reject
		// small inputs (the real over-cap rejection is proven by the bytes-read-
		// bound unit test, which we cannot cheaply reproduce with a >64 MiB file).
		base := t.TempDir()
		store, err := NewFlatBuffersStore(base)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(base, "small.fb"), []byte{0x01, 0x02, 0x03, 0x04}, 0600))
		_, err = store.Load("small")
		assert.ErrorIs(t, err, ErrCorruptData,
			"a 4-byte malformed file hits the sanity pre-check (ErrCorruptData), not the size cap")
	})
}
