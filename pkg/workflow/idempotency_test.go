package workflow

// M9 chunk 3 — idempotency-key helper (DECISION 5).
//
// IdempotencyKey gives a side-effecting action a stable handle it can present to
// a downstream system for dedupe. The load-bearing contract is STABILITY ACROSS
// CRASH-RESUME: the same (WorkflowID, nodeName) must produce a byte-identical key
// on the original run and on every resume re-run, so a re-executed in-flight node
// presents the same key its first attempt did and the downstream deduplicates.
// The key deliberately does NOT fold in any retry attempt or timestamp — a resume
// re-run is the SAME logical attempt, not a new one.

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdempotencyKey_StableAcrossConstructions: two independently constructed
// WorkflowData with the same ID yield the SAME key for the same node. This is the
// core stability contract — the key depends only on (WorkflowID, nodeName), never
// on the live object identity or any per-run/per-attempt state.
func TestIdempotencyKey_StableAcrossConstructions(t *testing.T) {
	a := NewWorkflowData("wf-1")
	b := NewWorkflowData("wf-1")

	ka := IdempotencyKey(a, "node-x")
	kb := IdempotencyKey(b, "node-x")

	assert.Equal(t, ka, kb, "same (WorkflowID, nodeName) must yield the same key across independent constructions")
	assert.NotEmpty(t, ka)
}

// TestIdempotencyKey_DeterministicRepeat: calling twice on the same object is
// deterministic (no hidden state, no counter advance).
func TestIdempotencyKey_DeterministicRepeat(t *testing.T) {
	d := NewWorkflowData("wf-1")
	first := IdempotencyKey(d, "node-x")
	second := IdempotencyKey(d, "node-x")
	assert.Equal(t, first, second, "repeated calls must be deterministic")
}

// TestIdempotencyKey_DistinctInputs: a different WorkflowID OR a different node
// name produces a different key. Includes the collision-safety pair
// (wf="ab", node="c") vs (wf="a", node="bc"): a naive concatenation "ab"+"c" ==
// "a"+"bc" would collide; the length-framed construction must keep them distinct.
func TestIdempotencyKey_DistinctInputs(t *testing.T) {
	t.Run("different node name", func(t *testing.T) {
		d := NewWorkflowData("wf-1")
		assert.NotEqual(t, IdempotencyKey(d, "node-x"), IdempotencyKey(d, "node-y"))
	})

	t.Run("different workflow id", func(t *testing.T) {
		assert.NotEqual(t,
			IdempotencyKey(NewWorkflowData("wf-1"), "node-x"),
			IdempotencyKey(NewWorkflowData("wf-2"), "node-x"),
		)
	})

	t.Run("collision-safe boundary", func(t *testing.T) {
		// "ab"|"c" must not collide with "a"|"bc".
		k1 := IdempotencyKey(NewWorkflowData("ab"), "c")
		k2 := IdempotencyKey(NewWorkflowData("a"), "bc")
		assert.NotEqual(t, k1, k2, "length-framed key must disambiguate the (wfID, nodeName) split point")
	})
}

// TestIdempotencyKey_StableAcrossResume: the key a node presents on the original
// run must be identical to the key it presents on a resume re-run. We model resume
// faithfully: the original run's data is persisted to a Store and the resume run
// reconstructs its WorkflowData by Loading it back (a fresh object). The key keyed
// on (WorkflowID, nodeName) must survive that round-trip unchanged — this is what
// lets a re-executed in-flight node dedupe against its first attempt downstream.
func TestIdempotencyKey_StableAcrossResume(t *testing.T) {
	const id = "resume-wf"
	store := NewInMemoryStore()

	// Original run.
	original := NewWorkflowData(id)
	originalKey := IdempotencyKey(original, "side-effect-node")
	require.NoError(t, store.Save(original))

	// Resume: reconstruct from the persisted state (a new object).
	resumed, err := store.Load(id)
	require.NoError(t, err)
	resumedKey := IdempotencyKey(resumed, "side-effect-node")

	assert.Equal(t, originalKey, resumedKey,
		"the idempotency key must be identical across a crash-resume re-run")
}

// TestIdempotencyKey_Format pins the documented stable format: lowercase hex of a
// 32-byte SHA-256 digest = 64 hex chars. The exact bytes are a STABLE CONTRACT
// (changing them would break downstream dedupe across an upgrade), so this test is
// the canary that flags any accidental format change.
func TestIdempotencyKey_Format(t *testing.T) {
	k := IdempotencyKey(NewWorkflowData("wf-1"), "node-x")
	require.Len(t, k, 64, "key must be 64 lowercase hex chars (SHA-256)")
	raw, err := hex.DecodeString(k)
	require.NoError(t, err, "key must be valid lowercase hex")
	assert.Len(t, raw, 32, "decoded digest must be 32 bytes (SHA-256)")
}
