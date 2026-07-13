package workflow

// M9 chunk 3 — crash-resume serialization-fidelity property (DECISION 6).
//
// Durable crash-resume MULTIPLIES the blast radius of the int64-via-float64
// persistence bug (the v0.7.1 first-CI-run saga): every completed node's output
// and every data value is rehydrated from the journal on EVERY resume, so a codec
// that silently turns an int64 into a float64 corrupts state on each resume cycle.
// This property pins the checkpoint Save -> reload round-trip (the real
// JSONFileStore.SaveCheckpoint -> Load path that resume uses) and asserts EXACT
// equality of every value's dynamic TYPE and magnitude — an integer must come back
// an int64 at full magnitude, never a float64.
//
// Mutation-bite (captured in the CHUNK3 summary): reverting the UseNumber() codec
// in loadSnapshotInternal makes integers decode as float64, so the int64 values
// (the killers below) come back float64 and the property FALSIFIES.

import (
	"fmt"
	"math"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fidelityKillers are the integer magnitudes that a float64 codec corrupts: the
// int64 extremes and the 2^53 mantissa boundary (the largest integer a float64
// can represent exactly — 2^53+1 is the first that cannot). They ride along EVERY
// property iteration so the bite is guaranteed, not left to a lucky random draw.
var fidelityKillers = []int64{
	math.MaxInt64, math.MinInt64,
	1 << 53, (1 << 53) + 1, (1 << 53) - 1,
	-(1 << 53), -((1 << 53) + 1),
	math.MaxInt32, math.MaxInt32 + 1,
	math.MinInt32, math.MinInt32 - 1,
}

// fidKind tags the dynamic type of a generated value so the round-trip assertion
// knows which exact type+magnitude check to apply.
type fidKind int

const (
	kInt fidKind = iota
	kFloat
	kString
	kBool
)

type fidEntry struct {
	kind fidKind
	i    int64
	f    float64
	s    string
	b    bool
}

// value materialises the entry as the interface{} a caller would Set.
func (e fidEntry) value() interface{} {
	switch e.kind {
	case kInt:
		return e.i
	case kFloat:
		return e.f
	case kString:
		return e.s
	default:
		return e.b
	}
}

// fidEntryGen generates one mixed-type scalar value. Floats are constrained to be
// finite and NON-integral: NaN/Inf cannot be JSON-encoded at all, and an integral
// float (e.g. 5.0) round-trips through JSON as the integer "5" and is legitimately
// rehydrated as int64 — so asserting "float stays float" only carries meaning for
// genuinely fractional values.
func fidEntryGen() gopter.Gen {
	genInt := gen.Int64().Map(func(v int64) fidEntry { return fidEntry{kind: kInt, i: v} })
	genFloat := gen.Float64().
		SuchThat(func(f float64) bool {
			return !math.IsNaN(f) && !math.IsInf(f, 0) && f != math.Trunc(f)
		}).
		Map(func(v float64) fidEntry { return fidEntry{kind: kFloat, f: v} })
	genString := gen.AnyString().Map(func(v string) fidEntry { return fidEntry{kind: kString, s: v} })
	genBool := gen.Bool().Map(func(v bool) fidEntry { return fidEntry{kind: kBool, b: v} })
	return gen.OneGenOf(genInt, genFloat, genString, genBool)
}

// assertRoundTrip checks that got holds, for every key in want, a value of the
// exact same dynamic type and magnitude. Returns false (no t.Fatal) so it composes
// inside a gopter property; the named-edge test below uses testify directly.
func roundTripExact(got *WorkflowData, want map[string]interface{}) bool {
	for k, wv := range want {
		raw, ok := got.Get(k)
		if !ok {
			return false
		}
		switch w := wv.(type) {
		case int64:
			iv, isInt := raw.(int64) // MUST be int64, never float64
			if !isInt || iv != w {
				return false
			}
		case float64:
			fv, isFloat := raw.(float64)
			if !isFloat || fv != w {
				return false
			}
		case string:
			sv, isStr := raw.(string)
			if !isStr || sv != w {
				return false
			}
		case bool:
			bv, isBool := raw.(bool)
			if !isBool || bv != w {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// TestCheckpointFidelity_Property is the headline DECISION-6 property: random mixed
// scalar state plus the int64 killers, round-tripped through the real checkpoint
// Save -> reload path, returns every value with its exact type and magnitude.
func TestCheckpointFidelity_Property(t *testing.T) {
	// ph71: run the checkpoint fidelity property over the durable factories (InMemory/FB/SQLite),
	// so the SQLite type-collapse round-trips every value byte-identically to the serializing
	// stores (indistinguishable-under-test — the whole-suite SQL-01 bar, not just the ph66
	// fixture). FB + SQLite carry the real type-collapse TEETH (encode/decode); InMemory is a
	// non-serializing control (Clone(), cannot collapse int64->float64 — always passes). The
	// JSON UseNumber int64 path is pinned separately by TestCheckpointFidelity_Int64Edges and
	// TestCrossBackendParity_AllTypes (it is NOT in this factory set — reviewer ph71-F2).
	for storeName, newStore := range durableStoreFactories(t) {
		t.Run(storeName, func(t *testing.T) {
			runCheckpointFidelityProperty(t, newStore())
		})
	}
}

func runCheckpointFidelityProperty(t *testing.T, store WorkflowStore) {
	const id = "fid"
	cp, ok := store.(Checkpointer)
	require.True(t, ok, "durable store must implement Checkpointer")

	params := gopter.DefaultTestParametersWithSeed(0xF1DE1117)
	params.MinSuccessfulTests = 300
	properties := gopter.NewProperties(params)

	properties.Property("checkpoint Save->reload preserves every value's type and magnitude", prop.ForAll(
		func(entries []fidEntry) bool {
			d := NewWorkflowData(id)
			want := make(map[string]interface{}, len(entries)+len(fidelityKillers))

			// The killers ride along every iteration (guaranteed bite surface).
			for i, k := range fidelityKillers {
				key := fmt.Sprintf("kill-%d", i)
				d.Set(key, k)
				want[key] = k
			}
			// Plus the random mixed-type draw.
			for i, e := range entries {
				key := fmt.Sprintf("k-%d", i)
				v := e.value()
				d.Set(key, v)
				want[key] = v
			}

			// The REAL resume path: checkpoint (atomic JSON Save) then reload.
			if err := cp.SaveCheckpoint(d); err != nil {
				return false
			}
			got, err := store.Load(id)
			if err != nil {
				return false
			}
			return roundTripExact(got, want)
		},
		gen.SliceOf(fidEntryGen()),
	))

	properties.TestingRun(t)
}

// TestCheckpointFidelity_Int64Edges pins the int64 killer magnitudes by name, so a
// regression on a specific boundary (e.g. 2^53+1 silently becoming a float64)
// names itself rather than hiding behind a random gopter draw.
func TestCheckpointFidelity_Int64Edges(t *testing.T) {
	store, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)

	for _, want := range fidelityKillers {
		want := want
		t.Run(fmt.Sprintf("%d", want), func(t *testing.T) {
			id := "edge"
			d := NewWorkflowData(id)
			d.Set("v", want)
			require.NoError(t, store.SaveCheckpoint(d))

			got, err := store.Load(id)
			require.NoError(t, err)

			raw, ok := got.Get("v")
			require.True(t, ok)
			iv, isInt := raw.(int64)
			require.True(t, isInt, "value %d must rehydrate as int64, not %T (the float64 corruption)", want, raw)
			assert.Equal(t, want, iv, "int64 %d must survive checkpoint round-trip at full magnitude", want)
		})
	}
}
