package workflow

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CUR-003 / AUD-013: an external probe importing the public package proved Clone was NOT a
// deep clone — the exact shape []any{[]any{"original"}} had its INNER []interface{} aliased
// (the map-descent covered maps and top-level slices, but a nested slice fell through to a
// by-reference copy), and non-canonical values (typed maps, pointers, custom structs)
// aliased via the default branch. The original AUD-013 tests passed while the bug was live
// because none exercised a slice-in-slice nor asserted the non-canonical contract. These do.
//
// The contract is now HONEST in two directions: the CANONICAL value algebra (scalars,
// map[string]interface{}, []interface{} including nesting/cycles) is deep-copied; NON-canonical
// values are retained by reference by documented design.

// sameSlice reports whether two slices share a backing array (are the SAME slice), not merely
// equal ones — the aliasing the probe measured.
func sameSlice(a, b []interface{}) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// TestCUR003_CloneIsolatesNestedSliceInSlice reproduces the probe's exact aliasing shape:
// a []interface{} inside a []interface{} inside a map[string]interface{}. Mutating the
// original's INNERMOST slice element must not be observed by the clone.
func TestCUR003_CloneIsolatesNestedSliceInSlice(t *testing.T) {
	original := NewWorkflowData("wf")
	inner := []interface{}{"original"}
	original.Set("s", []interface{}{inner})

	clone := original.Clone()

	// Mutate the original's innermost slice element AFTER cloning.
	inner[0] = "mutated"

	outer := getSlice(t, clone, "s")
	cloneInner, ok := outer[0].([]interface{})
	require.True(t, ok, "clone must preserve the nested slice shape")
	assert.Equal(t, "original", cloneInner[0],
		"clone's inner slice must NOT observe a mutation to the original's inner slice (was aliased pre-fix)")
	assert.False(t, sameSlice(inner, cloneInner),
		"clone's inner slice must not share a backing array with the original's")
}

// TestCUR003_CloneMapDeepCopiesNestedSlice is the same property at the cloneMap level, plus
// a self-referential SLICE cycle and a map->slice->map cycle, all of which must terminate and
// close on the CLONE.
func TestCUR003_CloneMapNestedSliceAndCycles(t *testing.T) {
	t.Run("nested slice-in-slice is deep-copied", func(t *testing.T) {
		inner := []interface{}{"original"}
		m := map[string]interface{}{"s": []interface{}{inner}}

		c := cloneMap(m)

		couter := c["s"].([]interface{})    //nolint:errcheck // fixture: type known
		cinner := couter[0].([]interface{}) //nolint:errcheck // fixture: type known
		require.False(t, sameSlice(inner, cinner), "nested slice must not alias the original")
		inner[0] = "mutated"
		require.Equal(t, "original", cinner[0], "clone's nested slice must be isolated")
	})

	t.Run("self-referential slice terminates and closes on the CLONE", func(t *testing.T) {
		s := []interface{}{"tag", nil}
		s[1] = s // slice -> slice cycle
		m := map[string]interface{}{"s": s}

		c := cloneMap(m) // must not stack-overflow / hang

		cs, ok := c["s"].([]interface{})
		require.True(t, ok)
		require.Equal(t, "tag", cs[0])
		self, ok := cs[1].([]interface{})
		require.True(t, ok, "the slice cycle must be preserved, not dropped")
		require.True(t, sameSlice(self, cs), "the clone's slice cycle must point at the CLONE")
		require.False(t, sameSlice(self, s), "and must not alias the original")
	})

	t.Run("map -> slice -> map cycle closes on the CLONE", func(t *testing.T) {
		m := map[string]interface{}{"tag": "root"}
		m["kids"] = []interface{}{m} // map -> slice -> map (element IS the map, not a wrapper)

		c := cloneMap(m)

		kids, ok := c["kids"].([]interface{})
		require.True(t, ok)
		back, ok := kids[0].(map[string]interface{})
		require.True(t, ok, "map -> slice -> map cycle must be preserved")
		require.True(t, sameMap(back, c), "and must close on the clone, not the original")
		require.False(t, sameMap(back, m), "must not alias the original")
	})

	t.Run("a shared container slice stays shared in the clone", func(t *testing.T) {
		shared := []interface{}{map[string]interface{}{"v": int64(1)}}
		m := map[string]interface{}{"a": shared, "b": shared}

		c := cloneMap(m)

		ca := c["a"].([]interface{}) //nolint:errcheck // fixture: type known
		cb := c["b"].([]interface{}) //nolint:errcheck // fixture: type known
		require.True(t, sameSlice(ca, cb), "a shared container slice must remain shared in the clone")
		require.False(t, sameSlice(ca, shared), "but must not alias the original")
	})
}

// TestCUR003_CloneMapDeepAcyclicDoesNotOverflow: a deeply-nested ACYCLIC chain of slices and
// maps must clone iteratively without a stack overflow. The recursive predecessor died at ~4M;
// 200k is far past any recursion frame budget while staying fast (~ms), and exercises both the
// map worklist and the slice worklist alternating.
func TestCUR003_CloneMapDeepAcyclicDoesNotOverflow(t *testing.T) {
	const depth = 200_000

	// Build map -> slice -> map -> slice -> ... alternating, so BOTH worklists are driven.
	leaf := map[string]interface{}{"leaf": "bottom"}
	var cur interface{} = leaf
	for i := 0; i < depth; i++ {
		if i%2 == 0 {
			cur = []interface{}{cur} // wrap in a slice
		} else {
			cur = map[string]interface{}{"next": cur} // wrap in a map
		}
	}
	root := map[string]interface{}{"chain": cur}

	c := cloneMap(root) // must return, not overflow

	require.NotNil(t, c)
	// Walk the clone to the leaf and confirm depth was preserved and isolated.
	walk := c["chain"]
	steps := 0
	for {
		if s, ok := walk.([]interface{}); ok {
			require.Len(t, s, 1)
			walk = s[0]
			steps++
			continue
		}
		if mm, ok := walk.(map[string]interface{}); ok {
			if v, ok := mm["leaf"]; ok {
				require.Equal(t, "bottom", v)
				break
			}
			walk = mm["next"]
			steps++
			continue
		}
		t.Fatalf("unexpected node type at step %d: %T", steps, walk)
	}
	require.Equal(t, depth, steps, "the clone must preserve the full acyclic depth")

	// Isolation: mutating the ORIGINAL leaf must not touch the clone's leaf.
	leaf["leaf"] = "mutated"
	require.Equal(t, "bottom", walkToLeaf(t, c["chain"]), "deep clone must be isolated from the original")
}

func walkToLeaf(t *testing.T, node interface{}) string {
	t.Helper()
	for {
		switch v := node.(type) {
		case []interface{}:
			node = v[0]
		case map[string]interface{}:
			if leaf, ok := v["leaf"]; ok {
				return leaf.(string) //nolint:errcheck // fixture: type known
			}
			node = v["next"]
		default:
			t.Fatalf("unexpected node type: %T", node)
			return ""
		}
	}
}

// customStruct and a typed map stand in for the NON-canonical shapes the narrowed contract
// documents as retained-by-reference.
type cur003Custom struct{ V string }

// TestCUR003_NonCanonicalRetainedByReference pins the HONEST narrowed contract: a typed map
// (map[string]string), a pointer, and a custom struct pointer are RETAINED BY REFERENCE, so a
// mutation through the original IS observed by the clone. This is the contract the docs and
// CHANGELOG now state — the test asserts code and contract agree (not that aliasing is
// desirable, but that it is the documented, bounded behavior for values that cannot cross the
// canonical store boundary).
func TestCUR003_NonCanonicalRetainedByReference(t *testing.T) {
	t.Run("typed map[string]string is shared", func(t *testing.T) {
		typed := map[string]string{"k": "orig"}
		original := NewWorkflowData("wf")
		original.Set("typed", typed)

		clone := original.Clone()
		typed["k"] = "mutated"

		v, ok := clone.Get("typed")
		require.True(t, ok)
		got, ok := v.(map[string]string)
		require.True(t, ok, "a non-canonical typed map is retained as-is, not stringified")
		assert.Equal(t, "mutated", got["k"],
			"documented contract: a non-canonical typed map is retained BY REFERENCE and stays shared")
	})

	t.Run("pointer to a custom struct is shared", func(t *testing.T) {
		p := &cur003Custom{V: "orig"}
		original := NewWorkflowData("wf")
		original.Set("ptr", p)

		clone := original.Clone()
		p.V = "mutated"

		v, ok := clone.Get("ptr")
		require.True(t, ok)
		got, ok := v.(*cur003Custom)
		require.True(t, ok, "a non-canonical pointer is retained as-is")
		assert.Same(t, p, got, "documented contract: a pointer value is retained BY REFERENCE")
		assert.Equal(t, "mutated", got.V, "and observes mutation through the original")
	})
}

// TestCUR003_EmptyAndNilSlicesAreIsolated guards the empty-slice case the identity scheme must
// special-case: every empty non-nil slice shares one backing pointer (runtime.zerobase), so
// keying empties by identity would collapse unrelated empties into one clone. cloneMap sidesteps
// this by never REGISTERING an empty slice — it has no elements to alias. Pointer identity is
// therefore a useless instrument on empties (they all read the same zerobase pointer); the
// meaningful guarantees are that empties are preserved-as-empty, nested empties survive, a
// non-empty sibling is still isolated, and the whole structure round-trips by value.
func TestCUR003_EmptyAndNilSlicesAreIsolated(t *testing.T) {
	e1 := []interface{}{}
	e2 := []interface{}{}
	nonEmpty := []interface{}{"x"}
	m := map[string]interface{}{
		"a":    e1,
		"b":    e2,
		"ne":   nonEmpty,
		"wrap": []interface{}{[]interface{}{}},
	}

	c := cloneMap(m)

	ca := c["a"].([]interface{}) //nolint:errcheck // fixture: type known
	cb := c["b"].([]interface{}) //nolint:errcheck // fixture: type known
	require.Empty(t, ca)
	require.Empty(t, cb)
	require.NotNil(t, ca, "an empty (non-nil) slice clones to an empty non-nil slice")

	// A non-empty sibling is still deep-isolated (the empty path did not break real cloning).
	cne := c["ne"].([]interface{}) //nolint:errcheck // fixture: type known
	require.False(t, sameSlice(nonEmpty, cne), "a non-empty slice must not alias the original")
	nonEmpty[0] = "mutated"
	require.Equal(t, "x", cne[0], "the non-empty sibling clone stays isolated")

	wrap := c["wrap"].([]interface{}) //nolint:errcheck // fixture: type known
	inner := wrap[0].([]interface{})  //nolint:errcheck // fixture: type known
	require.Empty(t, inner, "an empty slice nested in a container slice is still cloned empty")
}

// TestCUR003_OverlappingSubSlicesDoNotCollapse guards the (ptr,len) slice-identity fix.
// An independent review found that keying the slice-dedup on the backing-array POINTER alone
// collapsed two overlapping same-start sub-slices of different lengths (full and full[0:1])
// onto ONE clone — the clone's short view came back wrong-length AND aliased to the long one,
// a corruption InMemoryStore.Save could persist. The dedup key must be (pointer, len).
func TestCUR003_OverlappingSubSlicesDoNotCollapse(t *testing.T) {
	wd := NewWorkflowData("overlap")
	mapA := map[string]interface{}{"x": int64(1)}
	mapB := map[string]interface{}{"y": int64(2)}
	full := []interface{}{mapA, mapB} // len 2, container elements (so cloneSlice registers it)
	head := full[0:1]                 // len 1, SAME backing-array start pointer as full
	wd.Set("full", full)
	wd.Set("head", head)

	clone := wd.Clone()
	cf := clone.data["full"].([]interface{}) //nolint:errcheck // fixture: type known
	ch := clone.data["head"].([]interface{}) //nolint:errcheck // fixture: type known

	// The bug: ch collapsed to full's clone (len 2). Fixed: each keeps its own length.
	require.Len(t, ch, 1, "the head sub-slice must keep len 1, not collapse onto full")
	require.Len(t, cf, 2, "the full slice keeps len 2")
	require.False(t, sameSlice(cf, ch), "head and full must be distinct clones, not aliased")

	// Deep isolation still holds: mutating the original's inner map does not reach the clone.
	mapA["x"] = int64(999)
	require.EqualValues(t, 1, ch[0].(map[string]interface{})["x"], //nolint:errcheck // fixture: type known
		"clone head[0] must be a deep copy, not aliased to the original map")
}
