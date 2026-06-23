package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFlatBuffersStore_AllTypes_SaveLoad closes the highest-leverage blind spot
// from the deep review (Gap-A / Gap-H, R-COV-1): the FlatBuffers Save type-dispatch
// arms for bool / int32 / float32 / float64 (and the JSON-fallback `default` arm
// for complex types) and the matching Load typed-vector loops (BoolData/DoubleData)
// were hit-count 0 — every existing store test drove only string and int64. A
// regression that garbled a bool or truncated a float on FB Save would have shipped
// green. This Save->Load round-trips one value of every supported scalar kind plus
// a struct (default JSON-fallback arm) through the real FlatBuffersStore and asserts
// faithful recovery, exercising:
//   - Save:  case int / int32 / int64 / bool / float64 / float32 / string / default
//   - Load:  IntData, BoolData, DoubleData, StringData loops
//
// Type-normalization contract under test (by design of the FB schema):
//   - int, int32, int64  -> stored in value_long, read back as int64
//   - float32, float64    -> stored in DoubleData, read back as float64
//   - bool                -> bool
//   - string              -> string
//   - complex (struct)    -> JSON-marshaled string (default arm)
//
// We assert through the typed getters (GetInt64/GetFloat64/GetBool/GetString),
// which encode that contract, so the assertion is about value fidelity, not the
// Go dynamic type of the stored interface{}.
func TestFlatBuffersStore_AllTypes_SaveLoad(t *testing.T) {
	store, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)

	data := NewWorkflowData("alltypes")
	// One key per Save dispatch arm.
	data.Set("k_int", int(123456789))               // case int
	data.Set("k_int32", int32(-2000000000))         // case int32
	data.Set("k_int64", int64(9223372036854775807)) // case int64 (MaxInt64 — the >2^53 fidelity edge)
	data.Set("k_bool_t", true)                      // case bool (true)
	data.Set("k_bool_f", false)                     // case bool (false — see fidelity note below)
	data.Set("k_f64", float64(3.141592653589793))   // case float64
	data.Set("k_f32", float32(2.5))                 // case float32 (exact in both f32 and f64)
	data.Set("k_str", "hello-world")                // case string
	data.Set("k_struct", structValue{A: 7, B: "x"}) // default arm -> JSON-fallback string

	require.NoError(t, store.Save(data))

	loaded, err := store.Load("alltypes")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// int family -> int64 (Load IntData loop, value_long path).
	gotInt, ok := loaded.GetInt64("k_int")
	require.True(t, ok, "k_int must load")
	assert.Equal(t, int64(123456789), gotInt)

	gotI32, ok := loaded.GetInt64("k_int32")
	require.True(t, ok, "k_int32 must load")
	assert.Equal(t, int64(-2000000000), gotI32, "int32 must widen to int64 faithfully")

	gotI64, ok := loaded.GetInt64("k_int64")
	require.True(t, ok, "k_int64 must load")
	assert.Equal(t, int64(9223372036854775807), gotI64, "MaxInt64 must round-trip exactly (>2^53)")

	// bool -> bool (Load BoolData loop). The KeyValueBool *entry* (table with its
	// key) is created for every bool key regardless of value, so BoolData(&kv,i)
	// returns the entry on Load. The boolean payload round-trips faithfully for both
	// true and false: even though the FlatBuffers runtime elides a false *scalar
	// field* (it equals the default), the accessor returns that default false — so a
	// stored false reads back as (false, true), i.e. PRESENT and false. This is the
	// fidelity contract we want; we pin both directions.
	gotBoolT, ok := loaded.GetBool("k_bool_t")
	require.True(t, ok, "k_bool_t must load")
	assert.True(t, gotBoolT)

	gotBoolF, okF := loaded.GetBool("k_bool_f")
	require.True(t, okF, "stored false must load PRESENT (the entry is kept; only the scalar field elides)")
	assert.False(t, gotBoolF, "stored false must round-trip as false")

	// float family -> float64 (Load DoubleData loop).
	gotF64, ok := loaded.GetFloat64("k_f64")
	require.True(t, ok, "k_f64 must load")
	assert.Equal(t, float64(3.141592653589793), gotF64)

	gotF32, ok := loaded.GetFloat64("k_f32")
	require.True(t, ok, "k_f32 must load")
	assert.Equal(t, float64(2.5), gotF32, "float32 widens to float64; 2.5 is exact in both")

	// string -> string (Load StringData loop).
	gotStr, ok := loaded.GetString("k_str")
	require.True(t, ok, "k_str must load")
	assert.Equal(t, "hello-world", gotStr)

	// complex struct -> JSON-fallback string (Save default arm). The struct is
	// serialized to JSON on Save and recovered as that JSON string on Load.
	gotStruct, ok := loaded.GetString("k_struct")
	require.True(t, ok, "k_struct must load as a JSON string (default fallback arm)")
	assert.JSONEq(t, `{"A":7,"B":"x"}`, gotStruct,
		"complex value must round-trip as its JSON encoding")
}

// structValue is a small exported-field struct used to drive the FB Save `default`
// arm (json.Marshal fallback for complex types).
type structValue struct {
	A int
	B string
}

// TestCrossBackendParity_AllTypes hardens cross-backend parity beyond int64
// (Gap-B): it asserts FlatBuffers, JSON, and InMemory backends agree on the
// loaded value for bool(true) / bool(false) / float64 / float32 / string / int64,
// not just int64. The backends could otherwise diverge on, e.g., float precision
// or bool handling and no test would notice.
//
// Normalization across backends: numeric int comes back as int64 from FB and
// JSON (UseNumber->Int64), and floats as float64. We compare via the typed
// getters so the comparison is value-level, robust to per-backend interface{}
// dynamic-type differences.
func TestCrossBackendParity_AllTypes(t *testing.T) {
	build := func() *WorkflowData {
		d := NewWorkflowData("parity")
		d.Set("bt", true)
		d.Set("bf", false)
		d.Set("f64", float64(1.25))
		d.Set("f32", float32(0.5))
		d.Set("s", "parity-string")
		d.Set("i", int64(4242424242))
		return d
	}

	fbStore, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	jsonStore, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	memStore := NewInMemoryStore()

	require.NoError(t, fbStore.Save(build()))
	require.NoError(t, jsonStore.Save(build()))
	require.NoError(t, memStore.Save(build()))

	fbLoaded, err := fbStore.Load("parity")
	require.NoError(t, err)
	jsonLoaded, err := jsonStore.Load("parity")
	require.NoError(t, err)
	memLoaded, err := memStore.Load("parity")
	require.NoError(t, err)

	backends := map[string]*WorkflowData{
		"flatbuffers": fbLoaded,
		"json":        jsonLoaded,
		"inmemory":    memLoaded,
	}

	for name, b := range backends {
		bt, ok := b.GetBool("bt")
		require.True(t, ok, "[%s] bool(true) must load", name)
		assert.True(t, bt, "[%s] bool(true) parity", name)

		bf, ok := b.GetBool("bf")
		require.True(t, ok, "[%s] bool(false) must load PRESENT", name)
		assert.False(t, bf, "[%s] bool(false) parity", name)

		f64, ok := b.GetFloat64("f64")
		require.True(t, ok, "[%s] f64 must load", name)
		assert.Equal(t, float64(1.25), f64, "[%s] float64 parity", name)

		f32, ok := b.GetFloat64("f32")
		require.True(t, ok, "[%s] f32 must load", name)
		assert.Equal(t, float64(0.5), f32, "[%s] float32->float64 parity", name)

		s, ok := b.GetString("s")
		require.True(t, ok, "[%s] string must load", name)
		assert.Equal(t, "parity-string", s, "[%s] string parity", name)

		i, ok := b.GetInt64("i")
		require.True(t, ok, "[%s] int64 must load", name)
		assert.Equal(t, int64(4242424242), i, "[%s] int64 parity", name)
	}
}
