package workflow

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-025 / P-16: signals carried no delivery timestamp, so a stale buffered signal could
// satisfy a new wait/approval and a consumer had no way to check freshness. Signal now
// carries EnqueuedAt (unix-nanos), stamped by the store on delivery and returned by
// TakeSignals. Every durable store populates it (not just SQLite) so a freshness check is
// not a per-backend footgun.
func TestAUD025_SignalCarriesEnqueuedAt(t *testing.T) {
	newStores := map[string]func(t *testing.T) SignalStore{
		"SQLite": func(t *testing.T) SignalStore {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"), WithMultiProcess())
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
			return s
		},
		"InMemory": func(t *testing.T) SignalStore { return NewInMemoryStore() },
		"JSONFile": func(t *testing.T) SignalStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
		// CUR-001: FlatBuffers was ABSENT from this table, which is exactly why the backend
		// silently dropping EnqueuedAt stayed green. It is now a first-class row.
		"FlatBuffers": func(t *testing.T) SignalStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
	}

	for name, mk := range newStores {
		t.Run(name, func(t *testing.T) {
			store := mk(t)
			before := time.Now().UnixNano()
			require.NoError(t, store.DeliverSignal("wf", Signal{ID: "s1", Name: "approve", Payload: "yes"}))
			after := time.Now().UnixNano()

			got, err := store.TakeSignals("wf")
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.GreaterOrEqual(t, got[0].EnqueuedAt, before,
				"AUD-025: EnqueuedAt must be stamped at delivery, not left 0/unknown")
			require.LessOrEqual(t, got[0].EnqueuedAt, after)

			// A re-delivery of the same id refreshes the timestamp (last-writer-wins, uniform).
			time.Sleep(2 * time.Millisecond)
			reDeliver := time.Now().UnixNano()
			require.NoError(t, store.DeliverSignal("wf", Signal{ID: "s1", Name: "approve", Payload: "yes2"}))
			got2, err := store.TakeSignals("wf")
			require.NoError(t, err)
			require.Len(t, got2, 1)
			require.GreaterOrEqual(t, got2[0].EnqueuedAt, reDeliver,
				"AUD-025: a re-delivery must refresh EnqueuedAt")
		})
	}
}

// TestCUR001_FlatBuffersCodecPreservesEnqueuedAt pins the codec directly (below the store):
// forward fidelity, and the backward-compat property that a pre-CUR-001 buffer decodes the
// missing field as 0 = "unknown" rather than garbage.
func TestCUR001_FlatBuffersCodecPreservesEnqueuedAt(t *testing.T) {
	// Forward: a stamped delivery time survives the FlatBuffers round-trip.
	const stamp int64 = 1_700_000_000_123_456_789
	buf, err := encodeSignalFB(Signal{ID: "s1", Name: "approve", Payload: "yes", EnqueuedAt: stamp})
	require.NoError(t, err)
	got, err := decodeSignalFB(buf)
	require.NoError(t, err)
	require.Equal(t, stamp, got.EnqueuedAt, "CUR-001: the FB codec must preserve EnqueuedAt")

	// Backward-compat: FlatBuffers omits a scalar equal to its default (0) from the buffer,
	// so encoding EnqueuedAt=0 produces a buffer byte-identical to a pre-CUR-001 entry that
	// never carried the field. Decoding it must yield 0 (unknown), which a freshness-enforcing
	// consumer treats as reject — the documented contract.
	oldBuf, err := encodeSignalFB(Signal{ID: "s2", Name: "approve", Payload: "x"}) // EnqueuedAt zero
	require.NoError(t, err)
	oldGot, err := decodeSignalFB(oldBuf)
	require.NoError(t, err)
	require.Equal(t, int64(0), oldGot.EnqueuedAt, "CUR-001: a pre-field buffer decodes EnqueuedAt as 0/unknown")
}
