package workflow

import (
	"encoding/json"
	"fmt"
)

// AUD-026 — the canonical cross-store value contract ("honest common denominator").
//
// The four stores diverged on how a value round-trips: InMemory returned the host Go
// value verbatim (int, map), JSON returned map[string]interface{} with json.Number,
// and FB/SQLite returned typed scalars with COMPLEX values collapsed to a JSON string.
// A workflow could pass on InMemory (real maps) and behave differently in production on
// FB/SQLite (strings) — the test substitute was MORE faithful than production.
//
// The contract converges every store on what FB/SQLite already yield, because that is
// the honest floor of what the durable wire formats preserve without a format change:
//
//	data value   int/int32/int64 -> int64 ; float32/float64 -> float64 ; bool ; string
//	             everything else (maps, slices, unsupported scalar kinds) -> canonical
//	             JSON string  (mirrors SQLite encodeKV / the FB typed-vector dispatch)
//	node output  string stays a string; anything else -> canonical JSON string
//	             (mirrors encodeOutput: FB/SQLite outputs are string-on-wire)
//
// canonicalDataValue and canonicalOutputValue ARE "what a value becomes after an
// FB/SQLite round-trip", so applying them to InMemory (at Save) and JSON (at Load)
// makes all four stores byte-for-byte agree. FB/SQLite need no change — their Load is
// already canonical by construction.

// canonicalDataValue returns the canonical durable form of a DATA value. It mirrors
// SQLite's encodeKV dispatch EXACTLY: only int/int32/int64, bool, float32/float64 and
// string keep a typed form; every other kind (int8, uint*, map, slice, struct, …)
// collapses to the canonical JSON string, just as encodeKV routes them to its default
// arm. json.Number (a JSON-decode artifact) normalizes to int64-if-exact else float64,
// matching loadSnapshotInternal's own number handling.
//
// key names the value only so a depth refusal can report which value it rejected.
func canonicalDataValue(key string, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	// nil is intentionally NOT special-cased: it falls to the default arm, where
	// json.Marshal(nil) yields "null" — exactly what SQLite encodeKV / the FB store do
	// with an untyped nil, so the contract stays a mirror of them.
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case bool:
		return x, nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		return x, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		if f, err := x.Float64(); err == nil {
			return f, nil
		}
		return x.String(), nil
	default:
		// complex / unsupported scalar -> the SAME JSON string the FB/SQLite stores
		// write, produced by the SAME encoder (depth guards + fmt-free fallback).
		return encodeHostValue(v, fmt.Sprintf("data key %q", key))
	}
}

// canonicalOutputValue returns the canonical durable form of a node OUTPUT. FB and
// SQLite store outputs in a string-only column/vector, so the honest common
// denominator for an output is its string form: a string passes through; anything else
// (including a scalar int or bool) becomes the canonical JSON string, exactly as
// encodeOutput does. node names the value for a depth refusal.
func canonicalOutputValue(node string, v interface{}) (interface{}, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return encodeHostValue(v, fmt.Sprintf("output of node %q", node))
}

// canonicalizeForStore rewrites this WorkflowData's data and output values to their
// canonical durable form, in place, under the write lock. It is applied by
// InMemoryStore to the ISOLATED clone it stores (never a live, in-flight instance), so
// the in-memory store is a faithful substitute for the durable stores — which
// canonicalize implicitly via their wire encoding.
//
// SCOPE — this canonicalizes the SHAPE of a persistable value (a map/slice reloads as
// its JSON string). It deliberately does NOT change InMemory's behavior for a value the
// encoder REFUSES (an over-deep or cyclic value: encodeHostValue runs checkValueDepth
// before the marshal precisely so a deep value never reaches — and crashes — json.Marshal).
// Such a value is left as-is: canonicalizing it would mean either risking the AF2 crash
// or introducing a Save-time failure InMemory never had. The durable stores reject a
// cyclic/over-deep value at their own encode step; that divergence is pre-existing AF2
// territory, not the value-shape fidelity AUD-026 governs. So a keep-as-is here preserves
// InMemory's leniency for pathological values while making every PERSISTABLE value
// cross-store-uniform.
func (w *WorkflowData) canonicalizeForStore() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, v := range w.data {
		if cv, err := canonicalDataValue(k, v); err == nil {
			w.data[k] = cv
		}
	}
	for k, v := range w.outputs {
		if cv, err := canonicalOutputValue(k, v); err == nil {
			w.outputs[k] = cv
		}
	}
}
