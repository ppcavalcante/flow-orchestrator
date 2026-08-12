package workflow

import (
	"encoding/json"
	"fmt"
)

// unencodableFallback renders a value that json.Marshal REFUSED, without recursing
// into it.
//
// 116-AF4. Four store sites shared this shape:
//
//	if b, err := json.Marshal(v); err == nil { s = string(b) } else { s = fmt.Sprintf("%v", v) }
//
// and the else-branch is a crash. `fmt`'s %v walks a value recursively and has NO
// cycle detection, so an ordinary self-referential map:
//
//	m := map[string]any{}; m["self"] = m
//	data.SetOutput("n", m); store.Save(data)
//
// dies with `fatal error: stack overflow` — through the PUBLIC API, in two lines. It
// is unrecoverable: a Go stack overflow is a fatal error, not a panic, so no deferred
// recover() in any host can catch it. Measured, including that the deferred recover
// does not fire.
//
// THE MECHANISM IS THE UNCOMFORTABLE PART, AND IT IS WORTH STATING PLAINLY:
// json.Marshal is WELL-BEHAVED on a cycle — it detects one and returns a clean error
// ("encountered a cycle"). That clean refusal is precisely what routes the value into
// the %v fallback. The encoder's safety property is what selects for the crash.
//
// %T is safe on the same value: it renders the type without walking the value. So the
// fallback keeps the type and the reason — strictly more diagnostic than a bare
// sentinel, and non-recursive by construction.
//
// WHAT IS LOST, stated rather than glossed. This only runs where json.Marshal already
// failed, so the value was never durably representable and the old %v output was
// already non-round-trippable (decodeOutput hands the raw string straight back). But
// it is not true that NOTHING is lost: for the non-cyclic marshal failures — a chan, a
// func, a NaN — %v did render something, and that rendering is gone. It was of
// marginal worth (a chan's %v is a non-deterministic address) and it cost an
// unrecoverable process kill, which is the trade being made here.
//
// This deliberately does NOT depend on the AF2 pre-marshal walk. That walk would also
// refuse a cyclic value, but making AF4's closure contingent on every save path
// routing through a hoisted guard is the same "closes on the fourth writer" shape this
// phase keeps finding in the wild. A crash site that cannot crash is not redundant
// with a policy guard upstream.
func unencodableFallback(v any, err error) string {
	return fmt.Sprintf("<unencodable %T: %s>", v, err)
}

// encodeHostValue is the ONE encoder for a host-supplied value that is stored as a JSON
// string — a node output or a complex data value — with both depth axes closed around
// the marshal it wraps:
//
//	checkValueDepth  BEFORE   the crash axis (AF2): json.Marshal is recursive and dies
//	                          fatally on a deep value, so this must run first or not at all.
//	json.Marshal
//	checkJSONDepth   AFTER    the wedge axis (F2): refuses a document our own reader,
//	                          which HAS a scanner depth cap, could never decode back.
//
// Both, never one. They are not redundant and neither implies the other: the pre-walk
// cannot see a custom MarshalJSON's output or the levels an envelope adds, and the
// post-check cannot run at all if the encoder already killed the process.
//
// A marshal FAILURE is not an error here — it returns the AF4 fallback string, preserving
// the four sites' existing behaviour exactly. That is deliberate: json.Marshal refuses a
// chan, a func, a NaN and a cyclic value, and those refusals were already rendered rather
// than propagated. Turning them into errors would be a second behaviour change riding
// along on this one. Only the two DEPTH refusals are errors, because only they are
// refusing something that would otherwise crash or wedge.
//
// subject names the value in both refusals. A host saving twenty node outputs that is
// told only "a document is too deep" cannot act on it.
func encodeHostValue(v any, subject string) (string, error) {
	if err := checkValueDepth(v, subject); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		// AF4: NOT fmt.Sprintf("%v", v) — %v recurses with no cycle detection, so a
		// self-referential value here is an unrecoverable stack overflow.
		return unencodableFallback(v, err), nil
	}
	if err := checkJSONDepth(b, subject); err != nil {
		return "", err
	}
	return string(b), nil
}
