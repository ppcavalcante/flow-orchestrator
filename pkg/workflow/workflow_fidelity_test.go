package workflow

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fidelityEdges are the boundary integer values the M2 fidelity contract must
// round-trip without loss. They exercise the former int32 clamp branch (which
// had zero test coverage at M1 — TEST-LANDSCAPE gap #1) and the int64 extremes.
var fidelityEdges = []int64{
	0, 1, -1,
	math.MaxInt32, math.MaxInt32 + 1,
	math.MinInt32, math.MinInt32 - 1,
	1 << 40, -(1 << 40),
	math.MaxInt64, math.MinInt64,
}

// newFBStore returns a FlatBuffersStore rooted at a per-test temp dir.
func newFBStore(t *testing.T) *FlatBuffersStore {
	t.Helper()
	s, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	return s
}

// TestFBInt64RoundTripProperty is the core FID-01/FID-03 property: any int64
// value Saved then Loaded through the FlatBuffers store returns unchanged.
// Before M2 this clamped to int32 and would fail for |v| > MaxInt32.
func TestFBInt64RoundTripProperty(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 200
	properties := gopter.NewProperties(params)

	properties.Property("FB int64 Save->Load is lossless", prop.ForAll(
		func(v int64) bool {
			store, err := NewFlatBuffersStore(t.TempDir())
			if err != nil {
				return false
			}
			d := NewWorkflowData("rt")
			d.Set("v", v)
			if err := store.Save(d); err != nil {
				return false
			}
			got, err := store.Load("rt")
			if err != nil {
				return false
			}
			out, ok := got.GetInt64("v")
			return ok && out == v
		},
		gen.Int64(),
	))

	properties.TestingRun(t)
}

// TestFBInt64RoundTripEdges pins the mandatory boundary values explicitly, so a
// regression on a specific edge (e.g. MaxInt32+1 re-clamping) names itself
// rather than hiding behind random gopter draws.
func TestFBInt64RoundTripEdges(t *testing.T) {
	for _, want := range fidelityEdges {
		want := want
		t.Run("", func(t *testing.T) {
			store := newFBStore(t)
			d := NewWorkflowData("edge")
			d.Set("v", want)
			require.NoError(t, store.Save(d))
			got, err := store.Load("edge")
			require.NoError(t, err)
			out, ok := got.GetInt64("v")
			require.True(t, ok)
			assert.Equal(t, want, out, "edge %d must round-trip losslessly", want)
		})
	}
}

// TestCrossBackendParity asserts FID-01's consistency invariant: the same value
// Saved/Loaded through FlatBuffers, JSON, and InMemory yields identical results.
// FB joins the faithful pair (JSON/InMemory) instead of diverging.
func TestCrossBackendParity(t *testing.T) {
	for _, want := range fidelityEdges {
		want := want
		t.Run("", func(t *testing.T) {
			dir := t.TempDir()

			fbStore, err := NewFlatBuffersStore(dir)
			require.NoError(t, err)
			jsonPath := filepath.Join(dir, "wf.json")
			memStore := NewInMemoryStore()

			d := NewWorkflowData("parity")
			d.Set("v", want)

			// FB
			require.NoError(t, fbStore.Save(d))
			fbData, err := fbStore.Load("parity")
			require.NoError(t, err)
			fbVal, ok := fbData.GetInt64("v")
			require.True(t, ok)

			// JSON (WorkflowData's own JSON round-trip)
			require.NoError(t, d.SaveToJSON(jsonPath))
			jsonData := NewWorkflowData("parity")
			require.NoError(t, jsonData.LoadFromJSON(jsonPath))
			jsonVal, ok := jsonData.GetInt64("v")
			require.True(t, ok)

			// InMemory
			require.NoError(t, memStore.Save(d))
			memData, err := memStore.Load("parity")
			require.NoError(t, err)
			memVal, ok := memData.GetInt64("v")
			require.True(t, ok)

			assert.Equal(t, fbVal, jsonVal, "FB and JSON must agree for %d", want)
			assert.Equal(t, fbVal, memVal, "FB and InMemory must agree for %d", want)
			assert.Equal(t, want, fbVal, "all backends must equal the stored value %d", want)
		})
	}
}

// TestBackwardCompatGoldenFixture loads the M1-format golden fixture captured at
// pre-schema-change HEAD (T1). It proves new code reads legacy value:int-only
// buffers via the value_long-absent fallback. clamped reads back as the OLD
// lossy MaxInt32 — that is correct: the bytes on disk encode the M1 behavior,
// and we are validating the read path, not retroactively healing old data.
func TestBackwardCompatGoldenFixture(t *testing.T) {
	store, err := NewFlatBuffersStore("testdata")
	require.NoError(t, err)
	d, err := store.Load("m1_format_int")
	require.NoError(t, err)

	small, ok := d.GetInt64("small")
	require.True(t, ok)
	assert.Equal(t, int64(42), small)

	edge, ok := d.GetInt64("edge")
	require.True(t, ok)
	assert.Equal(t, int64(math.MaxInt32), edge)

	clamped, ok := d.GetInt64("clamped")
	require.True(t, ok)
	assert.Equal(t, int64(math.MaxInt32), clamped,
		"legacy fixture stored 2^40 lossily as MaxInt32; the fallback must read that legacy value")
}

// TestGuardPassesValidFiles is the data-compat POSITIVE pin (OBL-M4-positive-pin):
// the Phase-12 layered bounds guard must NOT bounce valid files. An over-tight
// guard that "rejects malformed" by rejecting everything would be a false-pass on
// HARD-01 AND a real data-compat regression. This pins that both the M2 golden
// fixture (legacy value:int fallback) AND freshly round-tripped valid files
// (incl. boundary magnitudes via value_long) pass the new guard and Load clean.
func TestGuardPassesValidFiles(t *testing.T) {
	// (1) Legacy M2 golden fixture passes the guard and Loads byte-faithfully.
	t.Run("golden fixture passes guard", func(t *testing.T) {
		store, err := NewFlatBuffersStore("testdata")
		require.NoError(t, err)
		d, err := store.Load("m1_format_int")
		require.NoError(t, err, "the guard must not reject the valid golden fixture")
		require.NotNil(t, d)

		small, ok := d.GetInt64("small")
		require.True(t, ok)
		assert.Equal(t, int64(42), small)

		edge, ok := d.GetInt64("edge")
		require.True(t, ok)
		assert.Equal(t, int64(math.MaxInt32), edge, "legacy value:int fallback intact through the guard")
	})

	// (2) A fresh round-trip across the fidelity boundary magnitudes passes the
	// guard (value_long path) — small=42 and edge=MaxInt32 explicitly, plus the
	// full fidelity edge set.
	t.Run("fresh round-trip passes guard", func(t *testing.T) {
		for _, v := range fidelityEdges {
			store := newFBStore(t)
			d := NewWorkflowData("pin")
			d.Set("small", int64(42))
			d.Set("edge", int64(math.MaxInt32))
			d.Set("v", v)
			require.NoError(t, store.Save(d))

			got, err := store.Load("pin")
			require.NoError(t, err, "the guard must not reject a freshly-saved valid file (v=%d)", v)
			require.NotNil(t, got)

			gv, ok := got.GetInt64("v")
			require.True(t, ok)
			assert.Equal(t, v, gv, "fidelity preserved through the guard for v=%d", v)
		}
	})
}

// TestLoadFallbackBranches exercises the three states of the T4 read fallback:
// (a) value_long present and non-zero -> used; (b) value_long absent (legacy
// buffer) -> falls back to value:int; (c) genuine stored zero -> 0 either way.
func TestLoadFallbackBranches(t *testing.T) {
	// (a) present, non-zero: a fresh M2 Save writes value_long.
	t.Run("value_long present", func(t *testing.T) {
		store := newFBStore(t)
		d := NewWorkflowData("a")
		d.Set("v", int64(1<<40))
		require.NoError(t, store.Save(d))
		got, err := store.Load("a")
		require.NoError(t, err)
		v, ok := got.GetInt64("v")
		require.True(t, ok)
		assert.Equal(t, int64(1<<40), v)
	})

	// (b) absent: the legacy golden fixture has no value_long, so the fallback
	// to value:int is the only path that returns a non-zero result.
	t.Run("value_long absent falls back to legacy value", func(t *testing.T) {
		store, err := NewFlatBuffersStore("testdata")
		require.NoError(t, err)
		got, err := store.Load("m1_format_int")
		require.NoError(t, err)
		v, ok := got.GetInt64("small")
		require.True(t, ok)
		assert.Equal(t, int64(42), v)
	})

	// (c) genuine zero: round-trips as 0 (value_long elided at default, legacy
	// value also 0 -> fallback yields 0).
	t.Run("stored zero", func(t *testing.T) {
		store := newFBStore(t)
		d := NewWorkflowData("c")
		d.Set("v", int64(0))
		require.NoError(t, store.Save(d))
		got, err := store.Load("c")
		require.NoError(t, err)
		v, ok := got.GetInt64("v")
		require.True(t, ok)
		assert.Equal(t, int64(0), v)
	})
}

// TestGetInt64Faithful covers FID-02's accessor: GetInt64 returns the full
// int64 for values stored as int, int32, or int64 (64-bit runtime; on 32-bit
// it is the portable accessor by construction — return type is int64).
func TestGetInt64Faithful(t *testing.T) {
	d := NewWorkflowData("g")
	d.Set("as_int64", int64(1<<40))
	d.Set("as_int32", int32(123))
	d.Set("as_int", 456)

	v, ok := d.GetInt64("as_int64")
	require.True(t, ok)
	assert.Equal(t, int64(1<<40), v)

	v, ok = d.GetInt64("as_int32")
	require.True(t, ok)
	assert.Equal(t, int64(123), v)

	v, ok = d.GetInt64("as_int")
	require.True(t, ok)
	assert.Equal(t, int64(456), v)

	_, ok = d.GetInt64("missing")
	assert.False(t, ok)
}

// TestGetIntDocumentedLimit asserts GetInt's current behavior. On the 64-bit
// builds this suite runs on, int is 64 bits so large values are returned
// faithfully; the 32-bit narrowing is a documented property of GetInt (callers
// needing portability use GetInt64) and is not asserted as a silent bug here.
func TestGetIntDocumentedLimit(t *testing.T) {
	d := NewWorkflowData("g")
	d.Set("big", int64(1<<40))

	v, ok := d.GetInt("big")
	require.True(t, ok)
	if math.MaxInt >= math.MaxInt64 { // 64-bit build
		// Compare as int64 so the >int32 expected value is never assigned to an
		// int-typed argument (which would overflow the constant on a 32-bit build,
		// even though this branch is unreachable there).
		assert.Equal(t, int64(1<<40), int64(v), "on 64-bit, GetInt returns the value faithfully")
	}

	// GetInt64 is always faithful regardless of arch.
	v64, ok := d.GetInt64("big")
	require.True(t, ok)
	assert.Equal(t, int64(1<<40), v64)
}
