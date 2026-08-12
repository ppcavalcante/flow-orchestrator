package workflow

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 116-AF4: a self-referential value reached the durable stores and killed the process.
//
// The chain is short and every link is well-behaved on its own, which is why it
// survived: json.Marshal DETECTS the cycle and returns a clean error; the four store
// sites treat a marshal error as "fall back to fmt.Sprintf("%v", v)"; and %v walks the
// value recursively with no cycle detection. The encoder's correct refusal is what
// selects the crash.
//
// It is `fatal error: stack overflow`, not a panic, so no recover() anywhere in the
// host can contain it. That is why these tests assert on the RESULT of a save rather
// than trying to catch anything: if the defect were present, the test binary would
// die and take every other test in the package with it.
//
// ── REVISED BY 116-AF2, AND THE REASON IS WHY THE NAME STAYED THE SAME ──
//
// This test's NAME states the property: a cyclic value must not kill the process. Its
// original ASSERTION was `Save() == nil` with the unencodableFallback placeholder
// stored — but that is one IMPLEMENTATION of the property, not the property. AF2's
// pre-marshal walk returns a typed ErrValidation instead, which ALSO does not kill the
// process, and so satisfies the thing this test is named for while violating the
// narrower thing it happened to assert.
//
// So this is not a contract collision. It is a test asserting wider than the property
// it names — the shape this phase has now filed repeatedly, here inside the adversarial
// suite itself. The name was right; the assertion was over-specified.
//
// WHY THE NEW VERDICT IS THE BETTER ONE, not merely the one that fell out: returning
// nil while storing `<unencodable map[string]interface {}: …>` in place of the host's
// value is SILENT DATA LOSS at the exact point durability is promised. The caller is
// told the checkpoint succeeded and the checkpoint does not contain their state — a
// checkpoint silently differing from the state it claims to preserve. A loud refusal
// is strictly better than a successful save of a placeholder.
//
// WHAT IS UNCHANGED, so this is not read as AF4 being undone: unencodableFallback and
// its four call sites are untouched and still reached for the NON-cyclic marshal
// failures (a chan, a func, a NaN) — see the arms below, which still pass. AF4's actual
// content was "a `fmt %v` fallback is an unrecoverable crash", and that stays fixed.
// Only the VERDICT for the cyclic case moved, from succeed-with-a-placeholder to refuse.
func TestAF4_CyclicValueDoesNotKillTheProcess(t *testing.T) {
	cyclic := func() map[string]any {
		m := map[string]any{}
		m["self"] = m
		return m
	}

	// The OUTPUT paths exercise AF4b (cloneMap self-recursion), which is a DIFFERENT
	// defect from AF4a (the %v fallback) on a different layer: they die before any
	// encoder is reached. Both are now fixed, so both paths run.
	//
	// assertRefused is the property, spelled out where it can be read: the process
	// survives (reaching any assertion at all proves that) AND the caller is told, in
	// the validation domain, rather than being handed a successful save of a placeholder.
	assertRefused := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err,
			"a cyclic value must be REFUSED. A nil return here means the placeholder was stored and "+
				"the host was told the checkpoint succeeded — silent data loss, which is the one "+
				"outcome worse than the crash this test was written for")
		require.ErrorIs(t, err, ErrValidation)
		// The refusal must not claim to know which of the two it saw: the walk is
		// non-transparent, so a cycle and an over-deep value are the same event to it.
		require.Contains(t, err.Error(), "or is cyclic",
			"the message must not diagnose this confidently as depth — it IS a cycle, and the walk "+
				"cannot tell. A confidently wrong diagnosis is worse than a vague correct one")
	}

	t.Run("FlatBuffersStore/output", func(t *testing.T) {
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		d := NewWorkflowData("wf-af4-fb-out")
		d.SetOutput("n", cyclic())
		assertRefused(t, store.Save(d))
	})

	t.Run("FlatBuffersStore/data", func(t *testing.T) {
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		d := NewWorkflowData("wf-af4-fb-kv")
		d.Set("k", cyclic())
		assertRefused(t, store.Save(d))
	})

	t.Run("SQLiteStore/output", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "af4.db"))
		require.NoError(t, err)
		d := NewWorkflowData("wf-af4-sq-out")
		d.SetOutput("n", cyclic())
		assertRefused(t, store.Save(d))
	})

	t.Run("SQLiteStore/data", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "af4b.db"))
		require.NoError(t, err)
		d := NewWorkflowData("wf-af4-sq-kv")
		d.Set("k", cyclic())
		assertRefused(t, store.Save(d))
	})

	// THE PROPERTY THE NAME STATES, asserted directly rather than only implied by the
	// four arms above surviving. Without this the file would assert the new verdict and
	// stop asserting the thing it is named for.
	t.Run("the process survives, which is the property", func(t *testing.T) {
		store, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		d := NewWorkflowData("wf-af4-alive")
		d.SetOutput("n", cyclic())
		_ = store.Save(d) //nolint:errcheck // the VERDICT is asserted by the four arms above; this arm is only about the process still being alive afterwards
		// Reaching this line at all is the assertion: a stack overflow is a fatal error,
		// so a regression would have taken the whole binary down before now.
		d2 := NewWorkflowData("wf-af4-alive-2")
		d2.SetOutput("ok", map[string]any{"a": 1})
		require.NoError(t, store.Save(d2), "the process is still alive and still able to write")
	})
}

// The fallback must be REACHED and must carry its diagnosis. A guard that never fires
// and a guard that fires silently are the same thing to a reader at 3am.
//
// json.Marshal genuinely refuses this value, so the fallback is the live path — and
// the stored string must name the type and the reason rather than being an opaque
// sentinel. This also pins that the fallback is not accidentally round-trippable: it
// is a diagnostic, and a later reader must not mistake it for data.
func TestAF4_FallbackNamesTypeAndReason(t *testing.T) {
	m := map[string]any{}
	m["self"] = m

	_, err := json.Marshal(m)
	require.Error(t, err, "premise: json.Marshal must refuse a cycle — if this ever stops being true the fallback is dead code")

	got := unencodableFallback(m, err)
	require.Contains(t, got, "map[string]interface {}", "the fallback must name the TYPE (%T is non-recursive; %v is the crash)")
	require.Contains(t, got, "cycle", "the fallback must carry the reason json.Marshal gave")
	require.True(t, strings.HasPrefix(got, "<unencodable "), "must be visibly a diagnostic, not plausible data")
}

// A NON-cyclic marshal failure still reaches the same fallback, and this test records
// what the fix COSTS rather than only what it buys.
//
// A channel cannot be JSON-encoded. Before AF4 the store wrote %v's rendering of it (a
// non-deterministic address); now it writes the type and reason. That rendering is
// genuinely lost — the trade is a marginal, non-deterministic diagnostic for the
// removal of an unrecoverable process kill. Stating it here so the loss is on the
// record and not discovered later.
func TestAF4_NonCyclicMarshalFailureAlsoUsesTheFallback(t *testing.T) {
	ch := make(chan int)

	_, err := json.Marshal(ch) //nolint:staticcheck // deliberate: marshal an unsupported type (chan) to exercise the fallback
	require.Error(t, err)

	got := unencodableFallback(ch, err)
	require.Contains(t, got, "chan int", "type is preserved")
	require.NotContains(t, got, "0x", "the old %v address rendering is deliberately NOT reproduced")
}

// AF4b — cloneMap's cycle handling, and the obligations that make it a CORRECT deep
// copy rather than merely a terminating one.
//
// ForEachOutput deep-copies every map-valued output through cloneMap, which used to
// self-recurse with no cycle handling: SetOutput of a self-referential map was an
// unrecoverable stack overflow, measured with every frame in cloneMap and json.Marshal
// never reached. That is why AF4a (the %v fallback) did not close it.
//
// The fix HANDLES the cycle rather than refusing it, because a faithful deep copy of a
// cyclic structure is itself cyclic. So termination is the weakest thing to assert;
// these pin the semantics.
func TestAF4b_CloneMapHandlesCyclesCorrectly(t *testing.T) {
	t.Run("self-cycle terminates and points at the CLONE", func(t *testing.T) {
		m := map[string]interface{}{"tag": "root"}
		m["self"] = m

		c := cloneMap(m)

		require.NotNil(t, c)
		require.Equal(t, "root", c["tag"])
		self, ok := c["self"].(map[string]interface{})
		require.True(t, ok, "the cycle must be preserved, not dropped")
		require.True(t, sameMap(self, c),
			"the clone's cycle must point at the CLONE, not the original — otherwise the "+
				"copy shares structure with its source and is not a deep copy")
		require.False(t, sameMap(self, m), "must not alias the original")
	})

	t.Run("cycle routed through a slice", func(t *testing.T) {
		// The seen check is required on the slice branch too, not only the direct-map
		// branch: cloneMap descends into maps nested inside []interface{}.
		m := map[string]interface{}{"tag": "root"}
		m["kids"] = []interface{}{map[string]interface{}{"back": m}}

		c := cloneMap(m)

		kids, ok := c["kids"].([]interface{})
		require.True(t, ok)
		kid, ok := kids[0].(map[string]interface{})
		require.True(t, ok)
		back, ok := kid["back"].(map[string]interface{})
		require.True(t, ok, "map -> slice -> map cycle must be preserved")
		require.True(t, sameMap(back, c), "and must close on the clone")
	})

	t.Run("a SHARED subtree stays shared, not duplicated", func(t *testing.T) {
		// Identity, not structural equality: two references to the same map must clone
		// to the same clone. Structural equality would collapse legitimately distinct
		// subtrees; duplicating would silently change the shape of the data.
		shared := map[string]interface{}{"v": 1}
		m := map[string]interface{}{"a": shared, "b": shared}

		c := cloneMap(m)

		ca := c["a"].(map[string]interface{}) //nolint:errcheck // fixture: subtree type is known
		cb := c["b"].(map[string]interface{}) //nolint:errcheck // fixture: subtree type is known
		require.True(t, sameMap(ca, cb), "a shared subtree must remain shared in the clone")
		require.False(t, sameMap(ca, shared), "but must not alias the original")
	})

	t.Run("distinct but structurally identical subtrees stay distinct", func(t *testing.T) {
		m := map[string]interface{}{
			"a": map[string]interface{}{"v": 1},
			"b": map[string]interface{}{"v": 1},
		}

		c := cloneMap(m)

		da := c["a"].(map[string]interface{}) //nolint:errcheck // fixture: subtree type is known
		db := c["b"].(map[string]interface{}) //nolint:errcheck // fixture: subtree type is known
		require.False(t, sameMap(da, db),
			"identity-keyed, so equal-looking distinct maps must NOT be collapsed")
	})

	t.Run("the ordinary flat map is unchanged", func(t *testing.T) {
		m := map[string]interface{}{"s": "x", "n": 1, "l": []interface{}{1, 2}}
		c := cloneMap(m)
		require.Equal(t, m, c)
		require.False(t, sameMap(m, c))
	})
}

// sameMap reports whether two maps are the SAME map, not merely equal ones.
func sameMap(a, b map[string]interface{}) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
