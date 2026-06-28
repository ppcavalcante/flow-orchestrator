package workflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// IdempotencyKey returns a stable, deterministic key for one node of one workflow
// run. It is the durable-execution dedupe handle: a side-effecting action can read
// this key and present it to a downstream system (the standard idempotency-key
// pattern) so the downstream deduplicates a re-execution.
//
// # The stability contract (the whole point)
//
// On crash-resume a node that had not reached Completed re-runs (the
// at-least-once-on-in-flight contract — see STABILITY.md / the persistence guide).
// IdempotencyKey is byte-identical across that re-run: it is derived ONLY from
// (WorkflowID, nodeName), so the original attempt and every resume re-run present
// the SAME key, and the downstream dedupes them as one logical operation.
//
// It deliberately does NOT fold in any retry attempt counter, timestamp, or live
// run state. A resume re-run is the SAME logical attempt, not a new one — folding
// in an attempt counter would hand the downstream a different key on resume and
// defeat the dedupe. (In-run retries are RetryableAction's separate concern.)
//
// # Stable format (a compatibility contract)
//
// The key is the lowercase hex encoding of a SHA-256 digest over a length-framed
// encoding of the inputs:
//
//	digest = SHA-256( uint64-LE(len(workflowID)) || workflowID || nodeName )
//	key    = hex(digest)   // 64 lowercase hex characters
//
// The 8-byte little-endian length prefix on workflowID frames the boundary between
// the two fields so the split point is unambiguous: ("ab","c") and ("a","bc") yield
// distinct keys (a naive concatenation would collide). This construction is a
// STABLE CONTRACT — downstream systems may recompute it, so it must not change
// across versions without a deliberate, documented break.
//
// The WorkflowID is read from data via GetWorkflowID; data must be non-nil.
func IdempotencyKey(data *WorkflowData, nodeName string) string {
	workflowID := data.GetWorkflowID()

	h := sha256.New()
	var lenPrefix [8]byte
	binary.LittleEndian.PutUint64(lenPrefix[:], uint64(len(workflowID)))
	h.Write(lenPrefix[:])
	h.Write([]byte(workflowID))
	h.Write([]byte(nodeName))

	return hex.EncodeToString(h.Sum(nil))
}
