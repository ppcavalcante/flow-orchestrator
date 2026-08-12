package workflow

import "fmt"

// maxJSONNestingDepth is the deepest container nesting (objects + arrays combined)
// this package will WRITE into any JSON document it must read back.
//
// It is NOT our number. It mirrors the nesting limit built into encoding/json's
// scanner, which every one of our readers goes through (json.Decoder in
// decodeSignalJSON / unmarshalSignalPayload / the snapshot load path). json.Marshal
// has NO depth limit, so before this guard a deeper-than-decodable document
// serialized fine, was written fine, and then failed to decode PERMANENTLY — the
// HYG-00 wedge on a third axis (F2). Unlike the byte and element axes this one gets
// no knob, because the ceiling belongs to the standard library: we cannot raise it,
// so it is a hard product limit and the only honest move is to refuse the write.
//
// A bare literal would be wrong the day the stdlib moves it — and wrong in the
// UNSAFE direction if it ever drops, since a stale-high constant would let us write
// documents the decoder rejects, silently re-arming exactly this wedge. So
// TestJSONDepth_MirrorsStdlibDecoderLimit binary-searches the live decoder's real
// limit on every run and fails if this constant is above it. Self-verifying across
// Go upgrades, the same discipline as the child-ID goldens in childid_contract_test.go.
const maxJSONNestingDepth = 10000

// jsonNestingDepth returns the maximum container nesting depth of WELL-FORMED JSON,
// which is all it is ever handed (the output of json.Marshal). It counts what the
// decoder's scanner counts: every '{' and '[' outside a string pushes a level.
//
// A byte scan rather than a trial decode: the guard runs on every write, including
// whole snapshots up to the 64 MiB byte ceiling, so it must be O(n) with no
// allocation. It also returns the ACTUAL depth, which a bool from json.Valid cannot,
// and the error message needs that number for an operator to act on it.
func jsonNestingDepth(b []byte) int {
	depth, maxDepth := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case '}', ']':
			depth--
		}
	}
	return maxDepth
}

// checkJSONDepth rejects an encoded document that nests deeper than the decoder
// which will read it back can accept — the nesting-depth twin of checkWriteSize and
// checkWriteElements, and the third asymmetric axis (F2).
//
// It measures the ENCODED DOCUMENT, not the value that produced it, because the
// decoder's budget is spent on the whole document and our own envelopes consume
// part of it: a payload nested 10000 deep round-trips through the FlatBuffers and
// SQLite stores (which encode the payload standalone) but wedges the JSON store,
// whose signalWire object wraps it in one more level. Measuring the value instead of
// the bytes would have been off by exactly that envelope.
//
// ErrValidation for the same reason as its two twins: nothing is corrupt, the caller
// handed over a structure deeper than can be read back.
func checkJSONDepth(encoded []byte, subject string) error {
	if d := jsonNestingDepth(encoded); d > maxJSONNestingDepth {
		return fmt.Errorf("%w: %s nests JSON %d levels, exceeds the %d-level max nesting depth (the encoding/json decoder's limit — a hard ceiling, not configurable)",
			ErrValidation, subject, d, maxJSONNestingDepth)
	}
	return nil
}
