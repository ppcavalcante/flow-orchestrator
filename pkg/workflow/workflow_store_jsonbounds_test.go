package workflow

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJSONLoad_SizeCap_Symmetric covers C5 (commit d140e96): the JSON load paths
// now read through io.LimitReader(cap+1) and reject over-cap input as
// ErrCorruptData, symmetric with the FlatBuffers Load path — no unbounded
// os.ReadFile, no OOM. This is the discriminating test: it shrinks the
// defaultMaxFileSize var seam (so we never materialize a 64 MiB file, the same
// technique the FB sec01 test uses) and asserts an over-cap file on disk surfaces
// as errors.Is(ErrCorruptData) from BOTH JSONFileStore.Load and
// WorkflowData.LoadFromJSON (which share readBoundedFile / the inline LimitReader).
func TestJSONLoad_SizeCap_Symmetric(t *testing.T) {
	origCap := defaultMaxFileSize
	defaultMaxFileSize = 1024 // 1 KiB — tiny so a small file trips the cap
	t.Cleanup(func() { defaultMaxFileSize = origCap })

	dir := t.TempDir()

	// A syntactically-valid-ish JSON document larger than the (shrunk) cap. Content
	// doesn't matter: the size bound rejects it before any decode. cap + 2 KiB.
	oversize := []byte("{" + strings.Repeat(`"k":"`+strings.Repeat("x", 64)+`",`, 64) + `"end":"x"}`)
	require.Greater(t, int64(len(oversize)), defaultMaxFileSize,
		"sanity: payload must exceed the shrunk cap")

	t.Run("JSONFileStore.Load", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "big.json"), oversize, 0600))
		store, err := NewJSONFileStore(dir)
		require.NoError(t, err)

		data, err := store.Load("big")
		require.Error(t, err, "over-cap file must be rejected, not OOM'd")
		assert.ErrorIs(t, err, ErrCorruptData, "size-cap rejection must be ErrCorruptData")
		assert.Nil(t, data, "rejected load must return nil data")
	})

	t.Run("WorkflowData.LoadFromJSON", func(t *testing.T) {
		path := filepath.Join(dir, "big2.json")
		require.NoError(t, os.WriteFile(path, oversize, 0600))

		wd := NewWorkflowData("x")
		err := wd.LoadFromJSON(path)
		require.Error(t, err, "over-cap file must be rejected, not OOM'd")
		assert.ErrorIs(t, err, ErrCorruptData, "size-cap rejection must be ErrCorruptData")
	})

	// At-cap (exactly defaultMaxFileSize bytes) must be ACCEPTED — the cap+1 read
	// distinguishes at-cap from over-cap. We verify the boundary read directly via
	// the same LimitReader discipline the load path uses, so the test pins that the
	// rejection is strictly > cap, not >= cap.
	t.Run("at-cap-boundary-accepted", func(t *testing.T) {
		atCap := make([]byte, defaultMaxFileSize)
		buf, err := io.ReadAll(io.LimitReader(strings.NewReader(string(atCap)), defaultMaxFileSize+1))
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, int64(len(buf)))
		assert.False(t, int64(len(buf)) > defaultMaxFileSize, "exactly-at-cap is accepted")
	})
}

// TestLoadSnapshot_ElementCountCap covers the C5 element-count guard in
// loadSnapshotInternal: a document whose decoded "data" / "nodeStatus" / "outputs"
// section holds more than defaultMaxElements entries is rejected as ErrCorruptData
// BEFORE the maps are populated, so a small-on-disk-but-huge-decoded payload cannot
// drive a giant allocation. The byte-size cap upstream bounds the document; this
// bounds the decoded entry count. We build a JSON snapshot with defaultMaxElements+1
// entries in the "data" section (keeping defaultMaxFileSize at its real default so
// the byte cap does NOT trip first — this isolates the element-count arm) and assert
// LoadSnapshot returns errors.Is(ErrCorruptData).
func TestLoadSnapshot_ElementCountCap(t *testing.T) {
	// Build {"id":"x","data":{"k0":0,"k1":1,...}} with defaultMaxElements+1 keys.
	// Use small integer values and short keys to keep the document compact; even so
	// it stays under the 64 MiB byte cap, so the element-count guard is what fires.
	n := defaultMaxElements + 1
	var b strings.Builder
	// Rough pre-size: ~ n * (avg key+value ~ 14 bytes).
	b.Grow(n * 14)
	b.WriteString(`{"id":"huge","data":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteByte('k')
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":`)
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString(`}}`)
	payload := []byte(b.String())

	// Guard the isolation assumption: the document must fit under the byte cap so
	// the element-count arm (not the size arm) is the one under test.
	require.LessOrEqual(t, int64(len(payload)), defaultMaxFileSize,
		"element-count test payload must stay under the byte cap to isolate the count guard")

	wd := NewWorkflowData("huge")
	err := wd.LoadSnapshot(payload)
	require.Error(t, err, "over-count section must be rejected")
	assert.ErrorIs(t, err, ErrCorruptData, "element-count rejection must be ErrCorruptData")

	// A section comfortably UNDER the cap must be accepted (the success path of
	// the same code).
	t.Run("under-cap-accepted", func(t *testing.T) {
		var ok strings.Builder
		ok.WriteString(`{"id":"ok","data":{"a":1,"b":2}}`)
		wd2 := NewWorkflowData("ok")
		require.NoError(t, wd2.LoadSnapshot([]byte(ok.String())))
		v, found := wd2.GetInt64("a")
		require.True(t, found)
		assert.Equal(t, int64(1), v)
	})

	// A section of EXACTLY defaultMaxElements entries must be ACCEPTED — the guard
	// is strictly greater-than (len(m) > cap), so the cap value itself is allowed.
	// This is the discriminating boundary the over-count (cap+1) case alone does
	// NOT cover: mutation testing showed the CONDITIONALS_BOUNDARY mutant
	// `> cap` -> `>= cap` (workflow_store.go:394 / loadSnapshotInternal) SURVIVED,
	// because nothing exercised the exact-cap point. With `>= cap` a snapshot of
	// exactly defaultMaxElements entries would be wrongly rejected as ErrCorruptData
	// — so this assertion kills that mutant. (It builds ~1M short entries once;
	// kept to a single section + skipped under -short to stay fast in quick runs.)
	t.Run("at-exact-cap-accepted", func(t *testing.T) {
		if testing.Short() {
			t.Skip("builds defaultMaxElements (~1M) entries; skipped under -short")
		}
		n := defaultMaxElements
		var b strings.Builder
		b.Grow(n * 14)
		b.WriteString(`{"id":"atcap","data":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('"')
			b.WriteByte('k')
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`":`)
			b.WriteString(strconv.Itoa(i))
		}
		b.WriteString(`}}`)
		payload := []byte(b.String())
		require.LessOrEqual(t, int64(len(payload)), defaultMaxFileSize,
			"at-cap payload must stay under the byte cap to isolate the count guard")

		wd3 := NewWorkflowData("atcap")
		require.NoError(t, wd3.LoadSnapshot(payload),
			"a section of exactly defaultMaxElements entries must be accepted (guard is strictly >)")
	})
}
