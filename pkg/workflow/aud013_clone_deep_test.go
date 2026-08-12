package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AUD-013 / AUD-014 / P-01 / C-13: Clone claims a deep copy but shallow-copies
// interface{} values, so nested maps/slices are ALIASED between original and
// clone. That breaks InMemoryStore.Save/Load isolation (both call Clone). The
// existing TestWorkflowDataClone asserts equality but never MUTATES a nested
// value to check isolation, which is exactly the blind spot the audit named.

func getMap(t *testing.T, wd *WorkflowData, key string) map[string]interface{} {
	t.Helper()
	v, ok := wd.Get(key)
	require.True(t, ok, "key %q should be present", key)
	m, isMap := v.(map[string]interface{})
	require.True(t, isMap, "value at %q should be a map[string]interface{}", key)
	return m
}

func getSlice(t *testing.T, wd *WorkflowData, key string) []interface{} {
	t.Helper()
	v, ok := wd.Get(key)
	require.True(t, ok, "key %q should be present", key)
	s, isSlice := v.([]interface{})
	require.True(t, isSlice, "value at %q should be a []interface{}", key)
	return s
}

func getOutputMap(t *testing.T, wd *WorkflowData, node string) map[string]interface{} {
	t.Helper()
	v, ok := wd.GetOutput(node)
	require.True(t, ok, "output for %q should be present", node)
	m, isMap := v.(map[string]interface{})
	require.True(t, isMap, "output for %q should be a map[string]interface{}", node)
	return m
}

func TestAUD013_CloneDeepCopiesNestedMap(t *testing.T) {
	original := NewWorkflowData("wf")
	original.Set("m", map[string]interface{}{"k": "orig"})

	clone := original.Clone()

	// Mutate the nested map through the ORIGINAL's handle.
	getMap(t, original, "m")["k"] = "mutated"

	assert.Equal(t, "orig", getMap(t, clone, "m")["k"],
		"clone's nested map must NOT observe a mutation to the original's nested map")
}

func TestAUD013_CloneDeepCopiesNestedSlice(t *testing.T) {
	original := NewWorkflowData("wf")
	original.Set("s", []interface{}{"orig"})

	clone := original.Clone()

	getSlice(t, original, "s")[0] = "mutated"

	assert.Equal(t, "orig", getSlice(t, clone, "s")[0],
		"clone's nested slice must NOT observe a mutation to the original's nested slice")
}

func TestAUD013_CloneDeepCopiesNestedOutput(t *testing.T) {
	original := NewWorkflowData("wf")
	original.SetOutput("node1", map[string]interface{}{"k": "orig"})

	clone := original.Clone()

	getOutputMap(t, original, "node1")["k"] = "mutated"

	assert.Equal(t, "orig", getOutputMap(t, clone, "node1")["k"],
		"clone's nested output must NOT observe a mutation to the original's output")
}

// AUD-026 update: InMemoryStore now canonicalizes a stored value (a nested map
// collapses to its canonical JSON STRING — the same value FB/SQLite yield), so a
// reloaded complex value is an immutable string, not an aliasable map. Isolation from
// post-save / post-load caller mutation is therefore stronger than the deep-clone the
// original AUD-014 test asserted: there is nothing left to alias. The Clone deep-copy
// (TestAUD013_Clone*) is unchanged and still covers the in-run Clone contract.
func TestAUD014_InMemoryStoreSaveIsolatesNested(t *testing.T) {
	store := NewInMemoryStore()

	data := NewWorkflowData("wf")
	nested := map[string]interface{}{"k": "orig"}
	data.Set("m", nested)
	require.NoError(t, store.Save(data))

	// Mutate the caller's nested map AFTER Save. The store's snapshot must not observe
	// it — and under the canonical contract it cannot, the stored value is a string.
	nested["k"] = "mutated-after-save"

	loaded, err := store.Load("wf")
	require.NoError(t, err)
	got, ok := loaded.Get("m")
	require.True(t, ok)
	assert.Equal(t, `{"k":"orig"}`, got,
		"Save must isolate nested values; the canonical form is the pre-mutation JSON string")
}

func TestAUD014_InMemoryStoreLoadIsolatesNested(t *testing.T) {
	store := NewInMemoryStore()

	data := NewWorkflowData("wf")
	data.Set("m", map[string]interface{}{"k": "orig"})
	require.NoError(t, store.Save(data))

	// A reloaded complex value is a canonical string, so two Loads are independent by
	// construction — a caller cannot mutate a returned map to affect a later Load.
	first, err := store.Load("wf")
	require.NoError(t, err)
	firstVal, ok := first.Get("m")
	require.True(t, ok)
	assert.Equal(t, `{"k":"orig"}`, firstVal)

	second, err := store.Load("wf")
	require.NoError(t, err)
	secondVal, ok := second.Get("m")
	require.True(t, ok)
	assert.Equal(t, `{"k":"orig"}`, secondVal,
		"Load must return a snapshot isolated from mutation of a prior Load result")
}
