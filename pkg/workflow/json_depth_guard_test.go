package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The third asymmetric axis (F2): json.Marshal has no nesting limit, json.Decoder
// does. A deeper-than-decodable document serialized fine, was written fine, and then
// failed to decode PERMANENTLY.
//
// Reproduced at HEAD, JSONFileStore, payload nested 10001 deep: DeliverSignal
// returned nil, TakeSignals returned
//   "corrupt workflow data: corrupt signal: invalid character '[' exceeded max depth"
// and a mailbox holding one good signal alongside it returned ZERO signals — one
// over-depth entry poisons the whole mailbox.

// nestJSON returns a value whose JSON encoding nests depth levels of arrays.
func nestJSON(depth int) any {
	var v any = 1
	for i := 0; i < depth; i++ {
		v = []any{v}
	}
	return v
}

// TestJSONDepth_MirrorsStdlibDecoderLimit is the reason maxJSONNestingDepth is not a
// bare literal. The ceiling is a property of encoding/json, not of our format, and it
// can move across Go releases — this project raised its floor to 1.25 this year. A
// hardcoded constant becomes wrong the day it changes, and wrong in the UNSAFE
// direction if the limit ever DROPS: a stale-high constant would let us write
// documents the decoder rejects, silently re-arming exactly the wedge this guard
// closes.
//
// So the limit is MEASURED, not asserted: binary-search the live decoder for the
// deepest document it accepts, and require our write ceiling to be no higher.
// Self-verifying on every Go upgrade — the same discipline as the child-ID goldens.
//
// Both stdlib entry points are checked. json.Decoder is what our readers use;
// json.Valid runs the same scanner and is checked too so a divergence between them in
// some future release cannot slip past on the side we happened not to test.
func TestJSONDepth_MirrorsStdlibDecoderLimit(t *testing.T) {
	doc := func(d int) string { return strings.Repeat("[", d) + "1" + strings.Repeat("]", d) }

	decoderAccepts := func(d int) bool {
		var v any
		return json.NewDecoder(strings.NewReader(doc(d))).Decode(&v) == nil
	}
	validAccepts := func(d int) bool { return json.Valid([]byte(doc(d))) }

	// Binary-search the deepest accepted depth. The upper bound is far above any
	// plausible limit; the search is O(log n) decodes.
	search := func(accepts func(int) bool) int {
		lo, hi := 1, 1<<20
		require.True(t, accepts(lo), "sanity: a depth-1 document must be accepted")
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			if accepts(mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return lo
	}

	decoderLimit := search(decoderAccepts)
	validLimit := search(validAccepts)
	t.Logf("measured stdlib limits: json.Decoder=%d json.Valid=%d; our ceiling=%d",
		decoderLimit, validLimit, maxJSONNestingDepth)

	require.LessOrEqual(t, maxJSONNestingDepth, decoderLimit,
		"maxJSONNestingDepth is above what json.Decoder actually accepts — the guard would pass writes "+
			"that can never be read back. Lower the constant to the measured limit.")
	require.LessOrEqual(t, maxJSONNestingDepth, validLimit,
		"maxJSONNestingDepth is above what the encoding/json scanner accepts")

	// Not merely <=: a ceiling far BELOW the real limit would reject documents that
	// round-trip perfectly well, which is its own defect. Pinning equality is what
	// makes a stdlib move visible instead of silently absorbed.
	require.Equal(t, decoderLimit, maxJSONNestingDepth,
		"the stdlib nesting limit moved to %d; update maxJSONNestingDepth to match", decoderLimit)
}

// TestJSONDepth_Scanner checks jsonNestingDepth against hand-computed shapes,
// including the case a naive brace-counter gets wrong: brackets inside strings, and
// escaped quotes inside those strings.
func TestJSONDepth_Scanner(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`1`, 0},
		{`[]`, 1},
		{`[[1]]`, 2},
		{`{"a":{"b":[1,2]}}`, 3},
		{`[1,2,3]`, 1},
		{`{"a":[],"b":[[]]}`, 3},
		// Brackets inside a string are DATA, not nesting.
		{`{"a":"[[[[[["}`, 1},
		// ...including past an escaped quote, where a naive string-tracker resyncs wrong.
		{`{"a":"x\"[[[[","b":"]]]]"}`, 1},
		{`["\\"]`, 1},
	}
	for _, c := range cases {
		require.Equal(t, c.want, jsonNestingDepth([]byte(c.in)), "depth of %s", c.in)
	}
}

// TestJSONDepth_ScannerAgreesWithTheDecoder is the anti-vacuity leg for the scanner:
// hand-computed shapes only prove the scanner matches MY arithmetic. This proves it
// matches the DECODER's, at the exact boundary that matters — a document our scanner
// measures at the ceiling must decode, and one it measures at ceiling+1 must not.
func TestJSONDepth_ScannerAgreesWithTheDecoder(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		d := maxJSONNestingDepth + delta
		doc := []byte(strings.Repeat("[", d) + "1" + strings.Repeat("]", d))
		require.Equal(t, d, jsonNestingDepth(doc), "scanner must measure the document it was handed")

		var v any
		derr := json.NewDecoder(strings.NewReader(string(doc))).Decode(&v)
		if delta <= 0 {
			require.NoError(t, derr, "at or under our ceiling the decoder must accept")
			require.NoError(t, checkJSONDepth(doc, "doc"), "and our guard must allow it")
		} else {
			require.Error(t, derr, "over our ceiling the decoder must reject")
			require.ErrorIs(t, checkJSONDepth(doc, "doc"), ErrValidation, "and our guard must refuse it")
		}
	}
}

// TestJSONDepth_DeliverRefusesWhatTakeCouldNotRead is the end-to-end arm across every
// store, and it pins the detail the reproduction changed the design over: the
// decoder's budget is spent on the WHOLE document, and our envelopes consume part of
// it. JSONFileStore wraps the payload in a signalWire object, so its usable payload
// depth is one shallower than the FlatBuffers and SQLite stores', which encode the
// payload standalone. A guard that measured the payload value instead of the encoded
// bytes would be off by exactly that envelope and leave the JSON store wedged.
//
// Each store is driven at ITS boundary, both sides: the deepest payload that must
// round-trip, and the first one that must be refused.
func TestJSONDepth_DeliverRefusesWhatTakeCouldNotRead(t *testing.T) {
	// usable payload depth per store = maxJSONNestingDepth - (levels its envelope costs)
	cases := []struct {
		store        func(t *testing.T) SignalStore
		name         string
		envelopeCost int
	}{
		{func(t *testing.T) SignalStore { return NewInMemoryStore() }, "InMemoryStore", -1}, // no JSON codec
		{func(t *testing.T) SignalStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}, "JSONFileStore", 1},
		{func(t *testing.T) SignalStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		}, "FlatBuffersStore", 0},
		{func(t *testing.T) SignalStore {
			s, err := NewSQLiteStore(t.TempDir() + "/sig.db")
			require.NoError(t, err)
			return s
		}, "SQLiteStore", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.envelopeCost < 0 {
				// InMemoryStore holds the value, never encoding it — no depth limit
				// applies and none must be invented. Asserted rather than skipped so a
				// future change that starts encoding here is caught.
				s := c.store(t)
				require.NoError(t, s.DeliverSignal("wf", Signal{ID: "s1", Payload: nestJSON(maxJSONNestingDepth + 50)}),
					"the in-memory mailbox does not serialize, so no JSON depth limit applies to it")
				got, err := s.TakeSignals("wf")
				require.NoError(t, err)
				require.Len(t, got, 1)
				return
			}

			usable := maxJSONNestingDepth - c.envelopeCost

			t.Run("deepest round-trips", func(t *testing.T) {
				s := c.store(t)
				require.NoError(t, s.DeliverSignal("wf", Signal{ID: "s1", Name: "n", Payload: nestJSON(usable)}),
					"a payload at the store's usable depth must be accepted")
				got, err := s.TakeSignals("wf")
				require.NoError(t, err, "and must read back — this is the symmetry claim")
				require.Len(t, got, 1)
			})

			t.Run("one deeper is refused at the write", func(t *testing.T) {
				s := c.store(t)
				err := s.DeliverSignal("wf", Signal{ID: "s1", Name: "n", Payload: nestJSON(usable + 1)})
				require.Error(t, err, "one level deeper could never be read back, so the write must refuse it")
				require.ErrorIs(t, err, ErrValidation)
				require.Contains(t, err.Error(), fmt.Sprintf("%d-level", maxJSONNestingDepth),
					"the message must name the ceiling")
				require.Contains(t, err.Error(), "nests JSON", "and the actual depth")

				// The refusal left the mailbox usable — no poisoning.
				got, terr := s.TakeSignals("wf")
				require.NoError(t, terr)
				require.Empty(t, got)
			})
		})
	}
}

// TestJSONDepth_RefusedEntryDoesNotPoisonTheMailbox is the arm that names the actual
// harm. The mailbox read is all-or-nothing: at HEAD a mailbox holding one good signal
// and one over-depth signal returned ZERO signals and an error, so a WaitForSignal run
// could never be woken. With the write refused, the good signal is still there.
func TestJSONDepth_RefusedEntryDoesNotPoisonTheMailbox(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-poison"
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "good", Name: "approve", Payload: "ok"}))

			derr := store.DeliverSignal(wf, Signal{ID: "bad", Name: "approve", Payload: nestJSON(maxJSONNestingDepth + 10)})
			if name == "InMemoryStore" {
				// KNOWN, DELIBERATE store divergence: InMemoryStore holds the value and
				// never encodes it, so no JSON limit applies and it accepts this. It is
				// the same shape as a divergence that already exists and is accepted —
				// an unmarshalable payload (a chan) is likewise fine in memory and
				// ErrValidation on every durable store. Adding a serialization
				// constraint to a non-serializing store would be inconsistent (why
				// depth and not marshalability?) and is out of scope here.
				require.NoError(t, derr, "the in-memory mailbox does not serialize; nothing to poison")
			} else {
				require.ErrorIs(t, derr, ErrValidation)
			}

			got, terr := store.TakeSignals(wf)
			require.NoError(t, terr, "the good signal must still be readable — at HEAD this returned 0 and an error")
			byID := map[string]bool{}
			for _, s := range got {
				byID[s.ID] = true
			}
			require.True(t, byID["good"], "the good signal survived a neighbour the store refused")
		})
	}
}
