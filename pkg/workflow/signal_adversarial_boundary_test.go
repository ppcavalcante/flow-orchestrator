package workflow

// M10 ph37 signal-store ADVERSARIAL boundary / fuzz suite (independent of the
// author's tests). The author's signal_store_test.go covers the happy mailbox
// contract (round-trip / idempotent-by-id / empty-id reject / traversal guard /
// int64 fidelity / mailbox-outside-snapshot). This file attacks the gaps those
// structurally miss, straight at the hard bar (no input panics, hangs, corrupts,
// loses, or double-applies a signal):
//
//   - many signals of the SAME Name: which one a WaitForSignalNode consumes, and
//     that the choice is DETERMINISTIC (lowest ID, since TakeSignals sorts by ID)
//   - many signals of DIFFERENT Names: only the awaited one is consumed; the rest
//     stay buffered (no collateral drain)
//   - adversarial sig.Name (empty / unicode / very long / path-shaped): Name is
//     NOT a filename so it must round-trip verbatim and never escape the mailbox
//   - adversarial sig.ID at the path boundary (unicode that is a legal single
//     segment vs. traversal-shaped that must be rejected)
//   - huge payloads (multi-MB) round-trip without truncation/corruption
//   - CORRUPT / TRUNCATED on-disk mailbox entries: TakeSignals must error cleanly
//     (ErrCorruptData / ErrIO), NEVER panic — the FB decoder reads raw offsets, so
//     a torn .sig file is the canonical totality landmine
//   - ack-nonexistent / take-empty totality
//   - a fuzz target over arbitrary (id, name, payload) round-trips through all
//     three stores: no panic, byte-identical Name, fidelity-preserving payload
//
// Oracle for each is stated inline: a round-trip property, the determinism of the
// consume order, or the minimum bar (a clean typed error, never a panic).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileSignalStores returns the two FILE-backed SignalStores together with the
// baseDir each was built over, so a test can reach into the on-disk mailbox to
// corrupt an entry. InMemory has no on-disk surface, so it is excluded here.
func fileSignalStores(t *testing.T) map[string]struct {
	store   SignalStore
	baseDir string
} {
	t.Helper()
	jsDir := t.TempDir()
	js, err := NewJSONFileStore(jsDir)
	require.NoError(t, err)
	fbDir := t.TempDir()
	fbs, err := NewFlatBuffersStore(fbDir)
	require.NoError(t, err)
	return map[string]struct {
		store   SignalStore
		baseDir string
	}{
		"JSONFileStore":    {js, jsDir},
		"FlatBuffersStore": {fbs, fbDir},
	}
}

// TestAdvSig_ManySameName_LowestIDWinsDeterministically: deliver several signals
// with the SAME Name but different IDs, then drive a WaitForSignalNode. Exactly ONE
// is consumed, and it is the lowest-ID one (TakeSignals sorts by ID, Execute takes
// the first match) — the same on every store, so the consume order is a documented,
// deterministic contract, not an accident of map iteration.
//
// Oracle (determinism + no-over-consume): among N same-Name signals the applied
// payload is the lowest-ID one, and N-1 remain buffered after convergence.
func TestAdvSig_ManySameName_LowestIDWinsDeterministically(t *testing.T) {
	for name, ss := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			store := ss.(WorkflowStore) //nolint:errcheck // test-local assertion on a known concrete store type
			const wf = "wf-same-name"
			w := buildSignalWorkflow(t, store, wf, "go")

			// Deliver out of ID order to prove the sort, not insertion order, decides.
			require.NoError(t, w.DeliverSignal(Signal{ID: "s3", Name: "go", Payload: "third"}))
			require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "go", Payload: "first"}))
			require.NoError(t, w.DeliverSignal(Signal{ID: "s2", Name: "go", Payload: "second"}))

			require.NoError(t, w.Execute(nil2ctx()))

			payload, ok := appliedSignalPayload(t, store, wf, "wait")
			require.True(t, ok)
			assert.Equal(t, "first", payload, "lowest-ID same-Name signal wins (TakeSignals sorts by ID)")

			// Exactly one consumed: the consuming node's signal (s1) is acked; the two
			// unrelated-but-same-Name entries remain (no over-drain).
			remaining, err := ss.TakeSignals(wf)
			require.NoError(t, err)
			ids := signalIDs(remaining)
			assert.ElementsMatch(t, []string{"s2", "s3"}, ids,
				"only the consumed signal is acked; the other same-Name signals stay buffered")
		})
	}
}

// TestAdvSig_DifferentNames_OnlyAwaitedConsumed: a mailbox holding many DIFFERENT
// names; the node waits on exactly one. Only that one is applied + acked; every
// other name stays buffered untouched (a later node waiting on them could still
// consume them).
//
// Oracle (no collateral drain): the awaited name is consumed; all other names
// remain, byte-intact.
func TestAdvSig_DifferentNames_OnlyAwaitedConsumed(t *testing.T) {
	for name, ss := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			store := ss.(WorkflowStore) //nolint:errcheck // test-local assertion on a known concrete store type
			const wf = "wf-diff-names"
			w := buildSignalWorkflow(t, store, wf, "approve")

			require.NoError(t, w.DeliverSignal(Signal{ID: "a", Name: "approve", Payload: "yes"}))
			require.NoError(t, w.DeliverSignal(Signal{ID: "b", Name: "reject", Payload: "no"}))
			require.NoError(t, w.DeliverSignal(Signal{ID: "c", Name: "cancel", Payload: "x"}))

			require.NoError(t, w.Execute(nil2ctx()))

			payload, ok := appliedSignalPayload(t, store, wf, "wait")
			require.True(t, ok)
			assert.Equal(t, "yes", payload)

			remaining, err := ss.TakeSignals(wf)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"b", "c"}, signalIDs(remaining),
				"only the awaited name is drained; other names are left for their own waiters")
		})
	}
}

// TestAdvSig_AdversarialNames_RoundTripVerbatim: sig.Name is NOT a path component
// (only sig.ID and workflowID are), so any Name — empty, unicode, very long,
// path-shaped, newline-laden — must round-trip byte-identical through every store
// and be matchable by a WaitForSignalNode.
//
// Oracle (round-trip identity): the Name a node waits on is compared by exact
// string equality, so whatever was delivered must come back unchanged.
func TestAdvSig_AdversarialNames_RoundTripVerbatim(t *testing.T) {
	names := map[string]string{
		"empty":     "",
		"unicode":   "承認-🚀-Ωμέγα",
		"path-ish":  "../../etc/passwd",
		"newlines":  "line1\nline2\ttab",
		"very-long": strings.Repeat("n", 8192),
		"spaces":    "  leading and trailing  ",
		"json-ish":  `{"name":"x","arr":[1,2,3]}`,
	}
	for storeName, ss := range signalStores(t) {
		for caseName, sigName := range names {
			t.Run(storeName+"/"+caseName, func(t *testing.T) {
				store := ss.(WorkflowStore) //nolint:errcheck // test-local assertion on a known concrete store type
				wf := "wf-advname-" + caseName
				w := buildSignalWorkflow(t, store, wf, sigName)

				require.NoError(t, w.DeliverSignal(Signal{ID: "id1", Name: sigName, Payload: "P"}))

				// The raw mailbox entry must carry the Name verbatim.
				sigs, err := ss.TakeSignals(wf)
				require.NoError(t, err)
				require.Len(t, sigs, 1)
				assert.Equal(t, sigName, sigs[0].Name, "Name round-trips verbatim")

				// And a node waiting on that exact Name consumes it.
				require.NoError(t, w.Execute(nil2ctx()), "a node waiting on the adversarial Name still matches it")
				p, _ := store.Load(wf) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
				st, _ := p.GetNodeStatus("wait")
				assert.Equal(t, Completed, st)
			})
		}
	}
}

// TestAdvSig_UnicodeSigID_LegalSegmentAccepted: a unicode sig.ID that is still a
// single legal path segment (no separators, IsLocal, Base==itself) must be accepted
// and round-trip; a SEPARATOR/traversal-shaped ID must be rejected ErrValidation.
// Boundary on validateSignalID beyond the author's empty-id / "../" cases.
//
// NOTE on the contract (verified against validateSignalID / validateWorkflowID,
// which share identical logic): "." and ".." are NOT traversal once the ".sig"
// suffix is appended (they yield in-dir files "..sig" / "...sig"), so the guard
// correctly accepts them — they are not in the reject set. A NUL byte in an ID
// likewise passes BOTH guards and then fails at the filesystem with a raw ErrIO on
// the file stores while InMemory accepts it — a pre-existing, SHARED ID-validation
// gap (it affects workflowID identically), not a ph37 signal regression and not a
// traversal/corruption risk, so it is documented here, not asserted as a defect.
func TestAdvSig_UnicodeSigID_LegalSegmentAccepted(t *testing.T) {
	for storeName, ss := range signalStores(t) {
		t.Run(storeName, func(t *testing.T) {
			const wf = "wf-unicode-id"

			// Legal unicode single-segment id: accepted, round-trips.
			legal := "承認-2026-Ω"
			require.NoError(t, ss.DeliverSignal(wf, Signal{ID: legal, Name: "go", Payload: 1}))
			sigs, err := ss.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, sigs, 1)
			assert.Equal(t, legal, sigs[0].ID, "a legal unicode single-segment ID round-trips")

			// Genuinely separator/traversal-shaped ids: rejected ErrValidation, never written.
			for _, bad := range []string{"../escape", "a/b", "nested/../escape", "dir/sub"} {
				err := ss.DeliverSignal(wf, Signal{ID: bad, Name: "go", Payload: 1})
				assert.ErrorIs(t, err, ErrValidation, "traversal/separator sig.ID %q must be rejected", bad)
			}
		})
	}
}

// TestAdvSig_HugePayload_RoundTrips: a multi-megabyte payload (a large string and a
// large slice) must round-trip without truncation or corruption, and stay under the
// store's bounded-read cap. Boundary on payload size the author tests never probe.
//
// Oracle (round-trip): the decoded payload equals the delivered one, full length.
func TestAdvSig_HugePayload_RoundTrips(t *testing.T) {
	for storeName, ss := range signalStores(t) {
		t.Run(storeName, func(t *testing.T) {
			store := ss
			const wf = "wf-huge"
			big := strings.Repeat("A", 2*1024*1024) // 2 MiB string
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "big", Name: "go", Payload: big}))

			sigs, err := store.TakeSignals(wf)
			require.NoError(t, err, "a large-but-bounded payload must read back cleanly, not error")
			require.Len(t, sigs, 1)
			got, ok := sigs[0].Payload.(string)
			require.True(t, ok)
			assert.Equal(t, len(big), len(got), "huge payload round-trips at full length, no truncation")
			assert.Equal(t, big, got)
		})
	}
}

// TestAdvSig_CorruptMailboxEntry_ErrorsCleanlyNeverPanics is THE totality landmine.
// A torn / forged on-disk .sig entry (truncated FB buffer; non-JSON garbage; empty
// file) must make TakeSignals return a clean typed error — NEVER panic. The FB
// decoder (decodeSignalFB → GetRootAsSignal → s.Id()/s.Name()/s.Payload()) reads raw
// vtable offsets out of the buffer with no bounds validation, so a truncated buffer
// is the canonical crash candidate. We deliver a valid signal (to create the dir and
// learn the path), then overwrite the entry with adversarial bytes and assert
// take-or-clean-error under a panic recover.
//
// Oracle (totality / the hard bar): corrupt input ⇒ a defined typed error, never a
// panic, hang, or silent wrong-decode.
func TestAdvSig_CorruptMailboxEntry_ErrorsCleanlyNeverPanics(t *testing.T) {
	corruptions := map[string][]byte{
		"empty":           {},
		"truncated-2b":    {0x08, 0x00},
		"truncated-8b":    {0x08, 0x00, 0x00, 0x00, 0x0c, 0x00, 0x00, 0x00},
		"garbage":         []byte("this is not a valid signal entry at all"),
		"huge-offset-fb":  {0xff, 0xff, 0xff, 0x7f, 0x00, 0x00, 0x00, 0x00},
		"json-not-object": []byte("[1,2,3]"),
		"nul-bytes":       {0x00, 0x00, 0x00, 0x00},
	}
	for storeName, info := range fileSignalStores(t) {
		for cName, bad := range corruptions {
			t.Run(storeName+"/"+cName, func(t *testing.T) {
				const wf = "wf-corrupt"
				// Create a valid entry so the mailbox dir + path exist.
				require.NoError(t, info.store.DeliverSignal(wf, Signal{ID: "victim", Name: "go", Payload: "ok"}))
				path := filepath.Join(info.baseDir, wf+signalDirSuffix, "victim"+signalFileSuffix)
				require.NoError(t, os.WriteFile(path, bad, 0600), "overwrite the entry with corrupt bytes")

				// TakeSignals must not panic; a clean typed error is the acceptable arm.
				var sigs []Signal
				var err error
				didPanic := func() (p bool) {
					defer func() {
						if r := recover(); r != nil {
							p = true
							t.Logf("PANIC decoding %s/%s: %v", storeName, cName, r)
						}
					}()
					sigs, err = info.store.TakeSignals(wf)
					return false
				}()

				require.False(t, didPanic,
					"TakeSignals must NEVER panic on a corrupt %s mailbox entry (totality / hard bar)", storeName)
				if err == nil {
					// If it did not error, it must at least have produced a *defined*
					// result — but a corrupt entry decoding to a bogus Signal silently is
					// itself a defect; flag it.
					t.Logf("no error on corrupt entry; decoded %d signals: %+v", len(sigs), sigs)
					assert.Failf(t, "corrupt entry silently accepted",
						"%s/%s: corrupt bytes should yield ErrCorruptData/ErrIO, got a clean decode", storeName, cName)
					return
				}
				assert.True(t, isCorruptOrIO(err),
					"corrupt entry must surface as ErrCorruptData or ErrIO, got %v", err)
			})
		}
	}
}

// TestAdvSig_AckNonexistentAndTakeEmpty_Totality: acking IDs that were never
// delivered is a no-op (idempotent, best-effort) and taking an empty/absent mailbox
// returns an empty slice, not an error — on every store.
func TestAdvSig_AckNonexistentAndTakeEmpty_Totality(t *testing.T) {
	for storeName, ss := range signalStores(t) {
		t.Run(storeName, func(t *testing.T) {
			store := ss
			const wf = "wf-empty"

			sigs, err := store.TakeSignals(wf)
			require.NoError(t, err, "take on an absent mailbox is empty, not an error")
			assert.Empty(t, sigs)

			require.NoError(t, store.AckSignals(wf, []string{"never", "delivered"}),
				"ack of absent IDs is a best-effort no-op")
			require.NoError(t, store.AckSignals(wf, nil), "ack of nil id list is a no-op")

			// Deliver one, ack a DIFFERENT id: the real one survives.
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "real", Name: "go", Payload: 1}))
			require.NoError(t, store.AckSignals(wf, []string{"ghost"}))
			sigs, err = store.TakeSignals(wf)
			require.NoError(t, err)
			assert.Equal(t, []string{"real"}, signalIDs(sigs), "acking a ghost id leaves the real signal")
		})
	}
}

// FuzzSignalStore_RoundTrip drives arbitrary (id, name, payload-string) through all
// three stores. The minimum bar: no panic on deliver/take/ack; for any id the
// validator accepts, the Name and payload round-trip byte-identical. Rejected ids
// (empty / traversal) must be a clean ErrValidation, never a panic or a silent write.
func FuzzSignalStore_RoundTrip(f *testing.F) {
	seeds := []struct {
		id, name, payload string
	}{
		{"s1", "go", "hello"},
		{"承認", "approve", "🚀"},
		{"../escape", "go", "x"},
		{"", "go", "x"},
		{"a.b.c", "", ""},
		{strings.Repeat("i", 300), "n", strings.Repeat("p", 4096)},
	}
	for _, s := range seeds {
		f.Add(s.id, s.name, s.payload)
	}
	f.Fuzz(func(t *testing.T, id, name, payloadStr string) {
		mk := map[string]func() SignalStore{
			"InMemory":    func() SignalStore { return NewInMemoryStore() },
			"JSONFile":    func() SignalStore { s, _ := NewJSONFileStore(t.TempDir()); return s },    //nolint:errcheck // constructor over t.TempDir() cannot fail in this test
			"FlatBuffers": func() SignalStore { s, _ := NewFlatBuffersStore(t.TempDir()); return s }, //nolint:errcheck // constructor over t.TempDir() cannot fail in this test
		}
		for storeName, mkStore := range mk {
			store := mkStore()
			if store == nil {
				t.Fatalf("%s: nil store", storeName)
			}
			const wf = "fuzz-wf"
			err := store.DeliverSignal(wf, Signal{ID: id, Name: name, Payload: payloadStr})
			if err != nil {
				// Acceptable rejection classes: ErrValidation (empty / separator /
				// traversal) or ErrIO (an ID that passes the path-segment guard but is
				// not a legal filename — over-long, NUL byte; a pre-existing SHARED
				// limitation of validateSignalID/validateWorkflowID, which bound
				// neither length nor charset). Anything else — or a panic, caught by
				// the fuzz engine — is a defect.
				if !isValidationErr(err) && !errors.Is(err, ErrIO) {
					t.Fatalf("%s: deliver(%q) unexpected error class: %v", storeName, id, err)
				}
				continue // rejected/unpersistable id: nothing should have been written
			}
			sigs, terr := store.TakeSignals(wf)
			if terr != nil {
				t.Fatalf("%s: take after a SUCCESSFUL deliver(%q) errored: %v", storeName, id, terr)
			}
			if len(sigs) != 1 {
				t.Fatalf("%s: deliver(%q) then take returned %d signals, want 1", storeName, id, len(sigs))
			}
			if sigs[0].ID != id {
				t.Fatalf("%s: id %q corrupted to %q", storeName, id, sigs[0].ID)
			}
			// Name and Payload are JSON-encoded in the durable stores, so (like the
			// payload below) their fidelity is asserted only for valid UTF-8 — invalid
			// UTF-8 is silently coerced to U+FFFD by encoding/json, a pre-existing,
			// system-wide JSON property, not a ph37 defect (see the payload note).
			if utf8.ValidString(name) && sigs[0].Name != name {
				t.Fatalf("%s: valid-UTF-8 name %q corrupted to %q", storeName, name, sigs[0].Name)
			}
			got, ok := sigs[0].Payload.(string)
			if !ok {
				t.Fatalf("%s: payload type changed to %T", storeName, sigs[0].Payload)
			}
			// Payload fidelity is asserted only for VALID UTF-8: the durable stores
			// JSON-encode the payload, and Go's encoding/json replaces invalid UTF-8
			// bytes with U+FFFD on marshal — so a string with non-UTF-8 bytes is
			// silently lossy on the JSON/FB stores (but byte-preserved on InMemory).
			// That lossiness is INHERENT to JSON encoding and pre-existing across the
			// whole system (node outputs and data values encode identically), not a
			// ph37 signal regression — so it is documented here, not asserted as a
			// defect. The totality bar (no panic, no error, single defined result) IS
			// asserted for every input above.
			if utf8.ValidString(payloadStr) && got != payloadStr {
				t.Fatalf("%s: valid-UTF-8 payload %q corrupted to %q", storeName, payloadStr, got)
			}
			if aerr := store.AckSignals(wf, []string{id}); aerr != nil {
				t.Fatalf("%s: ack(%q) errored: %v", storeName, id, aerr)
			}
		}
	})
}

// --- shared helpers for the adversarial signal suite ---

// nil2ctx returns a non-nil background context (a tiny readability helper so the
// adversarial cases read cleanly).
func nil2ctx() context.Context { return context.Background() }

func signalIDs(sigs []Signal) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.ID)
	}
	return out
}

func isCorruptOrIO(err error) bool {
	return errors.Is(err, ErrCorruptData) || errors.Is(err, ErrIO)
}

func isValidationErr(err error) bool { return errors.Is(err, ErrValidation) }
