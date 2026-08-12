// Adversarial suite for phase 116's two input-validation guards — the mailbox
// ENTRY-COUNT axis (F1) and the JSON NESTING-DEPTH axis (F2).
//
// Written by an independent pass, not by the author of the guards. The hard bar under
// test is TOTALITY: no input may make a guarded write path panic, hang, or leave state
// that cannot be read back. Every test here either reproduces a defect or closes an
// input class the author's own 22 bite-proofs did not cover.
//
// The oracle is stated per test. Where there is no oracle for "the right answer" the
// bar is the minimum one: a value or a typed error, never a crash and never a mailbox
// that a successful write left unreadable.
package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withMailboxCap lowers the package-wide mailbox ceiling for one test and restores it.
// The cap is a package var precisely so a test need not materialize 2^20 entries.
func withMailboxCap(t *testing.T, cap int) {
	t.Helper()
	prev := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = prev })
}

// nestValue builds a Go value nested d container levels deep: d==3 -> [[[1]]].
func nestValue(d int) any {
	var v any = 1
	for i := 0; i < d; i++ {
		v = []any{v}
	}
	return v
}

// ---------------------------------------------------------------------------
// F1 — the entry-count axis. Boundary values, and the ONE property that matters.
// ---------------------------------------------------------------------------

// TestAdv116_F1_TheOnlyPropertyThatMatters is the universally-quantified form of
// C116-F1-1, swept across every ceiling in the boundary neighbourhood {0,1,2,3} and
// every store.
//
// PROPERTY: for any sequence of deliveries, if every DeliverSignal returned nil then
// TakeSignals MUST succeed. The contrapositive is the wedge the phase exists to close —
// a write that reports success and a read that then refuses the whole mailbox.
//
// ORACLE: the property itself. No reference implementation is needed; the write's own
// return value is the oracle for what the read must accept.
func TestAdv116_F1_TheOnlyPropertyThatMatters(t *testing.T) {
	for _, ceiling := range []int{0, 1, 2, 3} {
		for name, store := range signalStores(t) {
			t.Run(fmt.Sprintf("cap=%d/%s", ceiling, name), func(t *testing.T) {
				withMailboxCap(t, ceiling)
				const wf = "wf-f1-property"

				accepted := 0
				// Offer well past the ceiling on BOTH sides of it.
				for i := 0; i < ceiling+4; i++ {
					err := store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n"})
					if err == nil {
						accepted++
						continue
					}
					require.ErrorIs(t, err, ErrValidation,
						"a refused delivery must be ErrValidation (the host over-delivered), never another domain")
				}

				got, err := store.TakeSignals(wf)
				require.NoError(t, err,
					"THE PROPERTY: %d deliveries returned nil, so the read must accept the mailbox. "+
						"A successful write followed by a refusing read IS the HYG-00 wedge.", accepted)
				assert.Len(t, got, accepted,
					"every accepted delivery must be readable back, and nothing else")
				assert.LessOrEqual(t, accepted, ceiling,
					"the write side must never admit more entries than the ceiling")
			})
		}
	}
}

// TestAdv116_F1_ExactlyAtTheCeilingIsLegal pins the boundary itself: the ceiling is
// INCLUSIVE on both sides. A guard that is > where it should be >= (or the reverse)
// passes every test written by whoever chose the threshold, so it is pinned from the
// outside here.
//
// ORACLE: the read guard's own predicate (len > cap rejects), which fixes cap as the
// largest legal count; the write must therefore accept exactly cap and refuse cap+1.
func TestAdv116_F1_ExactlyAtTheCeilingIsLegal(t *testing.T) {
	const ceiling = 3
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			withMailboxCap(t, ceiling)
			const wf = "wf-f1-boundary"

			for i := 0; i < ceiling; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%d", i), Name: "n"}),
					"delivery %d of %d must be accepted: the ceiling is the largest LEGAL count, "+
						"and a guard one too tight refuses documents that round-trip fine", i+1, ceiling)
			}
			got, err := store.TakeSignals(wf)
			require.NoError(t, err, "a mailbox holding exactly the ceiling must READ")
			require.Len(t, got, ceiling)

			err = store.DeliverSignal(wf, Signal{ID: "one-over", Name: "n"})
			require.Error(t, err, "the delivery that would make it ceiling+1 must be refused at the WRITE")
			require.ErrorIs(t, err, ErrValidation)

			got, err = store.TakeSignals(wf)
			require.NoError(t, err, "the refused delivery must not have wedged the read")
			require.Len(t, got, ceiling, "a refused delivery must not have landed")
		})
	}
}

// TestAdv116_F1_ReadSideIsUnprotectedByTheWriteGuard is the write-side/read-side
// asymmetry the phase's own framing names, attacked from the direction the phase did
// NOT: a store that ALREADY holds oversized data. The M9 threat model declares the
// store an input TCB, and 116 added a WRITE guard — which by construction does nothing
// about state that is already there.
//
// ORACLE: the minimum bar. There is no correct number of signals to return from an
// over-cap mailbox, so the only assertion is TOTALITY — a typed error, no panic, no
// partial/corrupt result, and no crash on the recovery paths a host would reach for.
func TestAdv116_F1_ReadSideIsUnprotectedByTheWriteGuard(t *testing.T) {
	const ceiling = 3
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)
	const wf = "wf-f1-external"

	// Seed ABOVE the cap the way an external writer would (M9): straight at the
	// backing store, bypassing DeliverSignal entirely.
	withMailboxCap(t, ceiling)
	mailbox := filepath.Join(dir, wf+signalDirSuffix)
	require.NoError(t, os.MkdirAll(mailbox, 0o750))
	for i := 0; i < ceiling+5; i++ {
		b, merr := encodeSignalJSON(Signal{ID: fmt.Sprintf("x%d", i), Name: "n"})
		require.NoError(t, merr)
		require.NoError(t, os.WriteFile(filepath.Join(mailbox, fmt.Sprintf("x%d%s", i, signalFileSuffix)), b, 0o600))
	}

	_, terr := store.TakeSignals(wf)
	require.Error(t, terr, "an over-cap mailbox must be refused by the read (the DoS guard)")
	require.ErrorIs(t, terr, ErrCorruptData)

	// The write guard also refuses, which means the mailbox cannot be drained by
	// delivering into it -- and TakeSignals, the only enumeration path, is the failing
	// call. This is the documented out-of-band-only residual; assert it holds rather
	// than trusting the prose.
	derr := store.DeliverSignal(wf, Signal{ID: "new", Name: "n"})
	require.Error(t, derr)
	require.ErrorIs(t, derr, ErrValidation)

	// AckSignals is the one path a host holding ids could use. It must not panic and
	// must not be capped -- if it were, the state would be unrecoverable even with ids.
	require.NoError(t, store.AckSignals(wf, []string{"x0", "x1", "x2", "x3", "x4"}),
		"AckSignals must stay uncapped: it is the only in-band drain for an over-cap mailbox")
	got, err := store.TakeSignals(wf)
	require.NoError(t, err, "after acking back under the cap the mailbox must read again")
	require.Len(t, got, 3)
}

// TestAdv116_F1_ReDeliveryAtTheCeilingCannotGrowTheMailbox attacks the Stat-first fast
// path from the input side rather than the concurrency side (which round 2 covered).
// The write guard SKIPS the count entirely when os.Stat finds the entry, so anything
// that makes Stat succeed for a path that is not a live entry is a hole.
//
// ORACLE: the property from TestAdv116_F1_TheOnlyPropertyThatMatters -- accepted
// deliveries must leave a readable mailbox.
func TestAdv116_F1_ReDeliveryAtTheCeilingCannotGrowTheMailbox(t *testing.T) {
	const ceiling = 2
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			withMailboxCap(t, ceiling)
			const wf = "wf-f1-redeliver"
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "a", Name: "n", Payload: "v1"}))
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "b", Name: "n", Payload: "v1"}))

			// At the ceiling. Re-delivering an existing id 50 times must stay legal and
			// must never grow the mailbox.
			for i := 0; i < 50; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: "a", Name: "n", Payload: fmt.Sprintf("v%d", i)}),
					"re-delivery at the cap is the documented idempotency contract")
			}
			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, 2, "50 re-deliveries must not have grown a 2-entry mailbox")
		})
	}
}

// ---------------------------------------------------------------------------
// F2 — the nesting-depth axis. Boundary, encoding tricks, and the totality bar.
// ---------------------------------------------------------------------------

// TestAdv116_F2_PerStoreDepthBoundaryIsMechanicallyDerived does not trust ANY number
// in the phase's prose. It binary-searches, per store, the deepest payload that
// DeliverSignal accepts, then asserts the two things that must hold at that boundary:
// the accepted depth ROUND-TRIPS, and one deeper is refused.
//
// This is the partition/boundary attack the author's own bites cannot be: the author
// chose the threshold, so a test written against the chosen number is circular. The
// number here is DISCOVERED and then checked against the read.
//
// ORACLE: round-trip. parse(print(x)) == x is the property; the boundary is wherever
// the implementation puts it, and it is only correct if the read agrees.
func TestAdv116_F2_PerStoreDepthBoundaryIsMechanicallyDerived(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-f2-boundary"
			accepts := func(d int) bool {
				return store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("d%d", d), Name: "n", Payload: nestValue(d)}) == nil
			}
			// The InMemory store serializes nothing, so no encoding constraint applies
			// to it (FID-01, a known and documented divergence).
			if name == "InMemoryStore" {
				require.True(t, accepts(maxJSONNestingDepth+10),
					"InMemoryStore holds Go values and must stay unconstrained (FID-01)")
				return
			}

			lo, hi := 1, maxJSONNestingDepth+8 // hi is expected to be refused
			require.True(t, accepts(lo), "a depth-1 payload must be accepted")
			require.False(t, accepts(hi), "a payload %d deep must be refused by every durable store", hi)
			for hi-lo > 1 {
				mid := (lo + hi) / 2
				if accepts(mid) {
					lo = mid
				} else {
					hi = mid
				}
			}
			t.Logf("%s: deepest accepted payload = %d, first refused = %d", name, lo, hi)

			// THE BOUNDARY MUST BE HONEST IN BOTH DIRECTIONS.
			got, err := store.TakeSignals(wf)
			require.NoError(t, err,
				"every accepted delivery must read back: the deepest accepted payload (%d) is the "+
					"whole point of the guard, and if the read refuses it the guard is one too loose", lo)
			require.NotEmpty(t, got)

			// One deeper must fail at the WRITE, in the validation domain -- not at the
			// read, and not in the corrupt-data domain.
			err = store.DeliverSignal(wf, Signal{ID: "over", Name: "n", Payload: nestValue(hi)})
			require.Error(t, err)
			require.ErrorIs(t, err, ErrValidation,
				"an over-depth payload is a host contract violation, not corrupt state")
		})
	}
}

// TestAdv116_F2_EncodingTricks is the encoding-axis attack: payloads whose ENCODED
// depth is not their apparent value depth. The guard measures the encoded document,
// which is the right choice -- these are the inputs that prove it, and the inputs that
// would defeat a guard measuring the value instead.
//
// ORACLE: round-trip through the store's own read path.
func TestAdv116_F2_EncodingTricks(t *testing.T) {
	deepMarshaler := deepJSONMarshaler{depth: maxJSONNestingDepth + 5}

	cases := []struct {
		name     string
		payload  any
		wantRefu bool
		why      string
	}{
		{
			name:     "string full of brackets is depth 1 when encoded",
			payload:  strings.Repeat("[", 40000),
			wantRefu: false,
			why:      "brackets inside a JSON string are NOT nesting; refusing this would be a false positive",
		},
		{
			name:     "string with escaped quotes and backslashes around brackets",
			payload:  `a\"[[[` + strings.Repeat(`\\[`, 20000) + `"`,
			wantRefu: false,
			why:      "the scanner's escape handling must not fall out of string state and start counting",
		},
		{
			name:     "custom MarshalJSON returning a deep document",
			payload:  deepMarshaler,
			wantRefu: true,
			why:      "the VALUE is one level deep; only measuring the ENCODED document catches this",
		},
		{
			name:     "json.RawMessage carrying a deep document",
			payload:  json.RawMessage(strings.Repeat("[", maxJSONNestingDepth+5) + "1" + strings.Repeat("]", maxJSONNestingDepth+5)),
			wantRefu: true,
			why:      "RawMessage is embedded verbatim, so its nesting is the document's nesting",
		},
		{
			name:     "map keys containing brace characters",
			payload:  map[string]any{"{{{{[[[[": map[string]any{"}}}}]]]]": 1}},
			wantRefu: false,
			why:      "braces in KEYS are string content; the encoded depth here is 3",
		},
	}

	for name, store := range signalStores(t) {
		if name == "InMemoryStore" {
			continue // serializes nothing (FID-01)
		}
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				const wf = "wf-f2-encoding"
				err := store.DeliverSignal(wf, Signal{ID: "e1", Name: "n", Payload: tc.payload})
				if tc.wantRefu {
					require.Error(t, err, tc.why)
					require.ErrorIs(t, err, ErrValidation, tc.why)
					return
				}
				require.NoError(t, err, tc.why)
				// Accepted => must read back. This is the half that catches a guard
				// which is too LOOSE on an encoding trick.
				got, terr := store.TakeSignals(wf)
				require.NoError(t, terr,
					"accepted payload (%s) must not have wedged the read -- %s", tc.name, tc.why)
				require.NotEmpty(t, got)
			})
		}
	}
}

// deepJSONMarshaler encodes to a document far deeper than its own value shape. It is
// the adversarial case for "count depth before or after encoding": before encoding it
// looks like a single scalar.
type deepJSONMarshaler struct{ depth int }

func (d deepJSONMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(strings.Repeat("[", d.depth) + "1" + strings.Repeat("]", d.depth)), nil
}

// TestAdv116_F2_ScannerAgreesWithTheStdlibOnAdversarialBytes is a metamorphic /
// differential test against the real oracle: encoding/json's own scanner. Our
// jsonNestingDepth is a hand-written byte scan, and the guard is only sound if it
// counts what the decoder counts.
//
// RELATION: for any well-formed document b, checkJSONDepth(b) == nil  <=>  a
// json.Decoder can decode b. A divergence in either direction is a defect -- too tight
// refuses documents that round-trip, too loose re-arms the wedge.
func TestAdv116_F2_ScannerAgreesWithTheStdlibOnAdversarialBytes(t *testing.T) {
	mk := func(open, close string, d int, inner string) []byte {
		return []byte(strings.Repeat(open, d) + inner + strings.Repeat(close, d))
	}
	cases := [][]byte{
		mk("[", "]", maxJSONNestingDepth-1, "1"),
		mk("[", "]", maxJSONNestingDepth, "1"),
		mk("[", "]", maxJSONNestingDepth+1, "1"),
		mk(`{"a":`, "}", maxJSONNestingDepth, "1"),
		mk(`{"a":`, "}", maxJSONNestingDepth+1, "1"),
		// A string full of brackets at the bottom of a legal nest: the scanner must
		// stay in string state for all of it.
		mk("[", "]", maxJSONNestingDepth, `"`+strings.Repeat("[", 5000)+`"`),
		// Sibling nests: max depth is the deepest branch, not the total count of
		// brackets. A guard that summed rather than tracked a running maximum would
		// refuse this document, which round-trips perfectly.
		[]byte("[" + strings.TrimSuffix(strings.Repeat("[1],", 4000), ",") + "]"),
		[]byte(`{"a":{"b":[1,2,3]}}`),
		[]byte(`"` + strings.Repeat(`\\`, 5000) + `"`),
		[]byte(`"` + strings.Repeat(`\"[`, 5000) + `"`),
	}
	for i, b := range cases {
		t.Run(fmt.Sprintf("case%02d", i), func(t *testing.T) {
			derr := decodeLikeTheRealReader(b)
			// The fixture must be STRUCTURALLY well-formed: the only decoder rejection
			// admissible here is the depth one, otherwise the comparison is unfair and
			// the bug is in the fixture (this caught three of my own on the first run).
			if derr != nil {
				require.Contains(t, derr.Error(), "exceeded max depth",
					"fixture must be structurally well-formed; the only fair rejection is the depth one, got: %v", derr)
			}
			decoderAccepts := derr == nil
			guardAccepts := checkJSONDepth(b, "probe") == nil
			require.Equal(t, decoderAccepts, guardAccepts,
				"THE RELATION: the guard must accept exactly what the decoder accepts. "+
					"decoder=%v guard=%v measured_depth=%d ceiling=%d",
				decoderAccepts, guardAccepts, jsonNestingDepth(b), maxJSONNestingDepth)
		})
	}
}

// FuzzAdv116_JSONDepthGuardMatchesTheDecoder searches for a counterexample to the same
// relation over arbitrary bytes. Run with:
//
//	go test -run=NONE -fuzz=FuzzAdv116_JSONDepthGuardMatchesTheDecoder ./pkg/workflow
//
// Seeded from the boundaries above, since a coverage-guided fuzzer will not stumble
// onto a 10000-deep document on its own.
func FuzzAdv116_JSONDepthGuardMatchesTheDecoder(f *testing.F) {
	f.Add([]byte(`{"a":[1,2,{"b":"c"}]}`))
	f.Add([]byte(`"` + strings.Repeat(`\"[`, 100) + `"`))
	f.Add([]byte(strings.Repeat("[", maxJSONNestingDepth) + "1" + strings.Repeat("]", maxJSONNestingDepth)))
	f.Add([]byte(strings.Repeat("[", maxJSONNestingDepth+1) + "1" + strings.Repeat("]", maxJSONNestingDepth+1)))
	f.Add([]byte(`{"{[":"]}"}`))

	f.Fuzz(func(t *testing.T, b []byte) {
		if !json.Valid(b) {
			return // the guard is only ever handed json.Marshal output
		}
		derr := decodeLikeTheRealReader(b)
		guardAccepts := checkJSONDepth(b, "fuzz") == nil

		// The relation is about DEPTH, and only depth. A decoder rejection for any
		// other reason says nothing about this guard -- the first version of this
		// property was stated too broadly and the fuzzer duly produced "1E1000",
		// which json.Valid accepts and a plain Decode rejects for RANGE. (The real
		// readers use UseNumber and never parse the number at all, which is why
		// decodeLikeTheRealReader exists rather than a bare json.Decoder.)
		if derr != nil && !strings.Contains(derr.Error(), "exceeded max depth") {
			return
		}
		decoderAccepts := derr == nil
		if decoderAccepts != guardAccepts {
			t.Fatalf("guard/decoder divergence on DEPTH: decoder=%v guard=%v depth=%d ceiling=%d on %q",
				decoderAccepts, guardAccepts, jsonNestingDepth(b), maxJSONNestingDepth, truncate(b))
		}
	})
}

// decodeLikeTheRealReader decodes b exactly the way the package's own signal readers
// do -- json.Decoder WITH UseNumber -- so the comparison is against the decoder that
// actually reads these documents back, not a differently-configured one.
// decodeSignalJSON and unmarshalSignalPayload both set UseNumber for int64 fidelity,
// and it also means a number literal is never parsed, so range errors do not arise on
// the real path.
func decodeLikeTheRealReader(b []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	return dec.Decode(&v)
}

func truncate(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}
