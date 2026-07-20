package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// payloadAsInt64 normalizes a decoded signal payload to int64. The durable stores
// decode JSON numbers via UseNumber, so an integer payload arrives as json.Number;
// the in-memory store would keep a raw int64.
func payloadAsInt64(p any) (int64, error) {
	switch v := p.(type) {
	case json.Number:
		return v.Int64()
	case int64:
		return v, nil
	default:
		return 0, fmt.Errorf("payload is not int64-ish: %T", p)
	}
}

// signalStores returns the SignalStore implementations over fresh backing, each
// labelled, for table-driven coverage. File/DB stores get a per-call t.TempDir. SQLite
// (M19 ph93) joins the set — the conformance suite IS its semantics spec: SQLite must be
// behaviorally indistinguishable from the other stores.
func signalStores(t *testing.T) map[string]SignalStore {
	t.Helper()
	js, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	fbs, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	sq, err := NewSQLiteStore(filepath.Join(t.TempDir(), "signals.db"))
	require.NoError(t, err)
	return map[string]SignalStore{
		"InMemoryStore":    NewInMemoryStore(),
		"JSONFileStore":    js,
		"FlatBuffersStore": fbs,
		"SQLiteStore":      sq,
	}
}

// TestSignalStore_DeliverTakeAck_RoundTrip: the core mailbox contract across all
// three stores — deliver, non-destructive take (twice), ack, take is empty.
func TestSignalStore_DeliverTakeAck_RoundTrip(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-roundtrip"
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "approve", Payload: "yes"}))
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s2", Name: "cancel", Payload: nil}))

			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, 2)
			// Sorted by ID: s1, s2.
			assert.Equal(t, "s1", got[0].ID)
			assert.Equal(t, "approve", got[0].Name)
			assert.Equal(t, "yes", got[0].Payload)
			assert.Equal(t, "s2", got[1].ID)
			assert.Nil(t, got[1].Payload)

			// Take is non-destructive: a second take sees the same set.
			again, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, again, 2)

			// Ack one → the other remains.
			require.NoError(t, store.AckSignals(wf, []string{"s1"}))
			rem, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, rem, 1)
			assert.Equal(t, "s2", rem[0].ID)

			// Ack is idempotent: acking an absent + the remaining ID is fine.
			require.NoError(t, store.AckSignals(wf, []string{"s1", "s2"}))
			empty, err := store.TakeSignals(wf)
			require.NoError(t, err)
			assert.Empty(t, empty)
		})
	}
}

// TestSignalStore_TakeEmptyMailbox: an untouched workflow yields an empty,
// non-error result (early-take before any delivery).
func TestSignalStore_TakeEmptyMailbox(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			got, err := store.TakeSignals("never-delivered")
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

// TestSignalStore_IdempotentByID: re-delivering the same sig.ID leaves exactly one
// mailbox entry (D37-06 dedupe; the no-double-apply foundation).
func TestSignalStore_IdempotentByID(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-idem"
			sig := Signal{ID: "dup", Name: "go", Payload: "v"}
			require.NoError(t, store.DeliverSignal(wf, sig))
			require.NoError(t, store.DeliverSignal(wf, sig))
			require.NoError(t, store.DeliverSignal(wf, sig))
			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			assert.Len(t, got, 1, "re-delivering the same sig.ID must leave one entry")

			// The WINNER on a re-deliver with DIFFERENT content is LAST-writer-wins, uniformly
			// across every store — InMemory does box[id]=sig, the file stores overwrite the
			// sig_id file, and SQLite uses ON CONFLICT DO UPDATE. A store that diverged here
			// (e.g. SQLite ON CONFLICT DO NOTHING = first-writer) would fail THIS check even
			// though the count check above still passes — so this pins behavioral uniformity.
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "dup", Name: "updated", Payload: "v2"}))
			after, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, after, 1, "still one entry after a re-deliver with new content")
			assert.Equal(t, "updated", after[0].Name, "re-deliver of the same id is LAST-writer-wins (uniform across stores)")
			assert.Equal(t, "v2", after[0].Payload)
		})
	}
}

// TestSignalStore_RejectsEmptySignalID: an empty sig.ID is a validation error
// (the host must supply a stable unique ID — D37-06).
func TestSignalStore_RejectsEmptySignalID(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			err := store.DeliverSignal("wf", Signal{ID: "", Name: "x"})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

// TestSignalStore_TraversalGuard: a forged/path-traversing workflowID OR sig.ID is
// rejected (D37-09 + the sig.ID-as-filename hardening). sig.ID is the second
// caller-supplied component joined onto a path, so it must be guarded too.
func TestSignalStore_TraversalGuard(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			// Forged workflowID.
			err := store.DeliverSignal("../escape", Signal{ID: "s", Name: "n"})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
			_, err = store.TakeSignals("../escape")
			assert.ErrorIs(t, err, ErrValidation)
			err = store.AckSignals("../escape", []string{"s"})
			assert.ErrorIs(t, err, ErrValidation)

			// Forged sig.ID (would become a traversing filename).
			err = store.DeliverSignal("wf", Signal{ID: "../../etc/passwd", Name: "n"})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

// TestSignalStore_Int64PayloadFidelity: a max-int64 payload round-trips through
// the durable stores at full magnitude (re-asserts the chunk-3 int64 fidelity for
// the signal channel — MH37-6). The file stores serialize via JSON; UseNumber on
// decode keeps the value exact (a float64 detour would corrupt it).
func TestSignalStore_Int64PayloadFidelity(t *testing.T) {
	const want = int64(math.MaxInt64)
	// File stores only (InMemoryStore keeps the raw any without a serialization
	// round-trip, like its checkpoint — fidelity there is trivial).
	js, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	fbs, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	for name, store := range map[string]SignalStore{"JSONFileStore": js, "FlatBuffersStore": fbs} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.DeliverSignal("wf-i64", Signal{ID: "n", Name: "num", Payload: want}))
			got, err := store.TakeSignals("wf-i64")
			require.NoError(t, err)
			require.Len(t, got, 1)
			// Payload decodes as json.Number (UseNumber); assert exact int64.
			gotI64, perr := payloadAsInt64(got[0].Payload)
			require.NoError(t, perr)
			assert.Equal(t, want, gotI64, "max-int64 signal payload must round-trip exactly")
		})
	}
}

// TestSignalStore_MailboxOutsideSnapshot: delivering a signal does NOT touch the
// workflow's snapshot file (MH37-1) — the mailbox is a sibling directory. Proven
// by saving a snapshot, recording its bytes, delivering a signal, and asserting
// the snapshot bytes are unchanged + the mailbox is a separate sibling dir.
func TestSignalStore_MailboxOutsideSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	const wf = "wf-sep"

	data := NewWorkflowData(wf)
	data.SetNodeStatus("a", Completed)
	require.NoError(t, store.Save(data))

	snapPath := filepath.Join(dir, wf+".fb")
	before, err := os.ReadFile(snapPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "p"}))

	after, err := os.ReadFile(snapPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, before, after, "DeliverSignal must not touch the snapshot file (mailbox-outside-snapshot)")

	// The mailbox is a distinct sibling directory.
	info, err := os.Stat(filepath.Join(dir, wf+signalDirSuffix))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "mailbox is a sibling directory, separate from the snapshot")
}

// TestSignalStore_DeleteReclaimsMailbox (ph37 review F2): Store.Delete reclaims the
// durable mailbox, not just the snapshot — there is no background GC, so deleting a
// workflow must remove its <id>.signals/ entries (else they leak forever).
func TestSignalStore_DeleteReclaimsMailbox(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			ws, ok := store.(WorkflowStore)
			require.True(t, ok)
			const wf = "wf-del-reclaim"
			// Save a snapshot too, so Delete succeeds cleanly and we prove BOTH the
			// snapshot and the mailbox are reclaimed.
			require.NoError(t, ws.Save(NewWorkflowData(wf)))
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "p"}))
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s2", Name: "go", Payload: "q"}))
			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, 2)

			require.NoError(t, ws.Delete(wf))

			after, err := store.TakeSignals(wf)
			require.NoError(t, err)
			assert.Empty(t, after, "Delete reclaims the durable mailbox (no leak; ph37 F2)")
		})
	}
}

// TestSignalStore_UnderlyingErrorRemainsReachable guards the store-domain error
// contract (STABILITY.md:165/176) for the M10 signal-store decode paths: the
// error must carry BOTH the store-domain sentinel (via errors.Is) AND the
// underlying cause (via errors.As). The canon guards the workflow_store paths
// (errors_test.go:157); this locks the signal_store %w wrapping so a future
// silent %w->%v regression fails here instead of quietly dropping the cause.
func TestSignalStore_UnderlyingErrorRemainsReachable(t *testing.T) {
	_, err := decodeSignalJSON([]byte("{not json"))
	require.Error(t, err)
	// Store-domain label holds.
	assert.ErrorIs(t, err, ErrCorruptData, "malformed signal JSON must categorize as ErrCorruptData")
	// Underlying stdlib cause stays reachable (the %w contract; a bare "{not json"
	// yields a *json.SyntaxError).
	var syn *json.SyntaxError
	assert.ErrorAs(t, err, &syn, "underlying *json.SyntaxError must remain reachable via errors.As")
}

// TestSignalStore_DeleteReclaimsEarlyBufferedMailbox — F-P93-R1 regression: Delete reclaims the
// durable mailbox EVEN for a workflow that was NEVER Saved (an early-buffered signal with no
// snapshot). The file stores' removeSignalDir reclaims unconditionally; SQLite must too, else the
// buffered rows leak on the DoS-sensitive channel. Delete still reports ErrNotFound (no snapshot),
// but the mailbox is gone. Cross-store so the contract is uniform.
func TestSignalStore_DeleteReclaimsEarlyBufferedMailbox(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			ws, ok := store.(WorkflowStore)
			require.True(t, ok)
			const wf = "wf-early-del"
			// Deliver a signal WITHOUT ever Saving a snapshot (early buffering).
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "buffered"}))
			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, 1)

			// Delete a never-Saved workflow. The snapshot-absence return DIFFERS by store (InMemory
			// returns nil, the durable stores ErrNotFound) — that pre-existing difference is NOT what
			// this test pins. What MUST be uniform is the MAILBOX RECLAIM: the buffered signal is gone
			// (no leak), regardless of the snapshot-absence return. (F-P93-R1.)
			_ = ws.Delete(wf) //nolint:errcheck // the snapshot-absence return varies by store; the reclaim is what's asserted

			after, terr := store.TakeSignals(wf)
			require.NoError(t, terr)
			assert.Empty(t, after, "Delete reclaims the early-buffered mailbox even with no snapshot (no leak; F-P93-R1)")
		})
	}
}
