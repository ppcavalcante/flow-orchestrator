package workflow

// HYG-00 — the Save/Load size-cap asymmetry.
//
// Before this guard, every Load path capped its read at defaultMaxFileSize while
// NO write path capped anything. An over-ceiling Save SUCCEEDED and the workflow
// then failed to Load forever — and because Load is what resume calls, the failure
// surfaced maximally far from the write that caused it. `defaultMaxFileSize` was
// also unexported with no setter, so a consumer holding an over-limit file had no
// supported way to read it back.
//
// These tests pin four things:
//  1. every write path refuses over-ceiling state, loudly and typed;
//  2. the ceiling is settable through the public API on both stores;
//  3. Save and Load enforce the SAME ceiling — the boundary tests below go RED if
//     a future edit moves one side without the other;
//  4. nothing changes for any workflow whose serialized state fits under the ceiling.

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sizeCapData builds a WorkflowData with enough payload to exceed a shrunk ceiling.
func sizeCapData(id string, entries int) *WorkflowData {
	d := NewWorkflowData(id)
	blob := strings.Repeat("x", 512)
	for i := 0; i < entries; i++ {
		d.Set("key"+strings.Repeat("z", i%16)+string(rune('a'+i%26)), blob)
	}
	return d
}

// measuredSize saves data through a deliberately huge ceiling and returns the exact
// number of bytes that lands on disk. Measuring rather than guessing is what lets
// the boundary tests below sit EXACTLY on the ceiling.
func measuredSize(t *testing.T, data *WorkflowData, kind string) int64 {
	t.Helper()
	dir := t.TempDir()
	var path string
	switch kind {
	case "json":
		s, err := NewJSONFileStore(dir, WithJSONMaxFileSize(1<<40))
		require.NoError(t, err)
		require.NoError(t, s.Save(data))
		path = filepath.Join(dir, data.GetWorkflowID()+".json")
	case "fb":
		s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(1<<40))
		require.NoError(t, err)
		require.NoError(t, s.Save(data))
		path = filepath.Join(dir, data.GetWorkflowID()+".fb")
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Size()
}

// TestSizeCap_JSONSave_RefusesOverCeiling is the primary guard: the write that used
// to succeed-then-wedge now fails at the write.
func TestSizeCap_JSONSave_RefusesOverCeiling(t *testing.T) {
	data := sizeCapData("wedged", 64)
	size := measuredSize(t, data, "json")

	dir := t.TempDir()
	store, err := NewJSONFileStore(dir, WithJSONMaxFileSize(size-1))
	require.NoError(t, err)

	err = store.Save(data)
	require.Error(t, err, "over-ceiling Save must fail, not succeed-then-wedge")
	assert.ErrorIs(t, err, ErrValidation,
		"over-ceiling Save is a validation failure — nothing is corrupt, the caller sent too much")

	// The error must name BOTH numbers, or an operator cannot size the override.
	assert.Contains(t, err.Error(), "wedged", "error must name the workflow")
	assert.Contains(t, err.Error(), strconv.FormatInt(size, 10), "error must name the ACTUAL size")
	assert.Contains(t, err.Error(), strconv.FormatInt(size-1, 10), "error must name the CEILING")

	// And nothing may have landed on disk.
	_, statErr := os.Stat(filepath.Join(dir, "wedged.json"))
	assert.True(t, os.IsNotExist(statErr),
		"a refused Save must not leave a file behind")
}

func TestSizeCap_FlatBuffersSave_RefusesOverCeiling(t *testing.T) {
	data := sizeCapData("wedgedfb", 64)
	size := measuredSize(t, data, "fb")

	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(size-1))
	require.NoError(t, err)

	err = store.Save(data)
	require.Error(t, err, "over-ceiling FB Save must fail")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), strconv.FormatInt(size, 10))
	assert.Contains(t, err.Error(), strconv.FormatInt(size-1, 10))

	_, statErr := os.Stat(filepath.Join(dir, "wedgedfb.fb"))
	assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")
}

// TestSizeCap_GroupCommitPath_RefusesOverCeiling covers the write path that does
// NOT go through Save: writeFullSnapshotLocked, reached via SaveCheckpoint +
// Sync() under Batched(K). Guarding only Save would leave THIS path able to wedge
// a workflow, so if the guard is ever removed from the group-commit writer this
// test goes red on its own.
func TestSizeCap_GroupCommitPath_RefusesOverCeiling(t *testing.T) {
	data := sizeCapData("wedgedgc", 64)
	size := measuredSize(t, data, "fb")

	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir,
		WithDurabilityMode(Batched(4)),
		WithFlatBuffersMaxFileSize(size-1),
	)
	require.NoError(t, err)

	// Batched(4) defers the write; Sync forces it through writeFullSnapshotLocked.
	require.NoError(t, store.SaveCheckpoint(data), "the deferred checkpoint itself does not write")
	err = store.Sync("wedgedgc")
	require.Error(t, err, "the group-commit flush must refuse over-ceiling state too")
	assert.ErrorIs(t, err, ErrValidation)

	_, statErr := os.Stat(filepath.Join(dir, "wedgedgc.fb"))
	assert.True(t, os.IsNotExist(statErr), "a refused group-commit flush must not leave a file")
}

// TestSizeCap_SaveLoadBoundarySymmetry is the anti-drift property and the reason
// this file exists. For a store with ceiling C, a payload of EXACTLY C bytes must
// be accepted by Save AND by Load; C+1 must be refused by Save. Move either side's
// ceiling without the other and one of these two halves goes red:
//   - lower Load's ceiling alone  -> the at-cap Load fails;
//   - raise Save's ceiling alone  -> the over-cap Save succeeds.
func TestSizeCap_SaveLoadBoundarySymmetry(t *testing.T) {
	for _, kind := range []string{"json", "fb"} {
		t.Run(kind, func(t *testing.T) {
			data := sizeCapData("boundary", 48)
			size := measuredSize(t, data, kind)

			newStore := func(ceiling int64) WorkflowStore {
				dir := t.TempDir()
				if kind == "json" {
					s, err := NewJSONFileStore(dir, WithJSONMaxFileSize(ceiling))
					require.NoError(t, err)
					return s
				}
				s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(ceiling))
				require.NoError(t, err)
				return s
			}

			// Exactly AT the ceiling: both sides must accept. A Save that lands is a
			// Save that loads back — that is the whole invariant.
			atCap := newStore(size)
			require.NoError(t, atCap.Save(data), "at-cap Save must be accepted")
			got, err := atCap.Load("boundary")
			require.NoError(t, err, "at-cap Load must be accepted — Save and Load must agree on the boundary")
			require.NotNil(t, got)

			// One byte over: Save must refuse.
			overCap := newStore(size - 1)
			require.Error(t, overCap.Save(data), "over-cap Save must be refused")

			// Anti-drift, the case the two legs above CANNOT catch. The most likely
			// regression is a Load edit that reverts to reading the package default
			// instead of the store's field. Every ceiling above is far below that
			// default, so such a revert would leave both legs green — Load would just
			// be more permissive. Shrinking the package default BELOW the store's
			// ceiling inverts that: if Load consults the default, this at-cap Load
			// fails. (Same var seam as workflow_store_sec01_test.go; sequential, so
			// no race with other tests.)
			origDefault := defaultMaxFileSize
			defaultMaxFileSize = 512
			t.Cleanup(func() { defaultMaxFileSize = origDefault })
			require.Less(t, defaultMaxFileSize, size,
				"sanity: the package default must be BELOW the store ceiling for this leg to bite")

			decoupled := newStore(size)
			require.NoError(t, decoupled.Save(data),
				"Save must honour the STORE ceiling, not the package default")
			back, err := decoupled.Load("boundary")
			require.NoError(t, err,
				"Load must honour the STORE ceiling, not the package default — "+
					"a Load that reads defaultMaxFileSize re-arms the asymmetry")
			require.NotNil(t, back)
		})
	}
}

// TestSizeCap_RaisedCeilingRecoversExistingFile proves the knob is a real recovery
// path, not decoration: a file already on disk that exceeds the default ceiling is
// unreadable at the default and readable once the ceiling is raised. This is the
// only remedy available to anyone already wedged by the shipped bug, since a
// write-side cap does nothing for state that is already written.
func TestSizeCap_RaisedCeilingRecoversExistingFile(t *testing.T) {
	for _, kind := range []string{"json", "fb"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			data := sizeCapData("recover", 64)
			size := measuredSize(t, data, kind)

			mk := func(ceiling int64) WorkflowStore {
				if kind == "json" {
					s, err := NewJSONFileStore(dir, WithJSONMaxFileSize(ceiling))
					require.NoError(t, err)
					return s
				}
				s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(ceiling))
				require.NoError(t, err)
				return s
			}

			// Simulate the wedged file: written under a generous ceiling.
			require.NoError(t, mk(size*4).Save(data))

			// A store at a ceiling below it cannot read it — the wedge.
			_, err := mk(size - 1).Load("recover")
			require.Error(t, err, "the low-ceiling store must reject the oversized file")
			assert.ErrorIs(t, err, ErrCorruptData)

			// Raising the ceiling recovers it. THIS is the supported escape hatch.
			got, err := mk(size * 4).Load("recover")
			require.NoError(t, err, "raising the ceiling must make the existing file readable again")
			require.NotNil(t, got)
			assert.Equal(t, "recover", got.GetWorkflowID())
		})
	}
}

// --- site 4: the store-less SaveToJSON / LoadFromJSON pair -------------------

// TestSizeCap_SaveToJSON_RefusesOverCeiling is the fourth write site. It is
// store-less public API, so the per-store option cannot reach it; it carries its
// own DataFileOption instead.
func TestSizeCap_SaveToJSON_RefusesOverCeiling(t *testing.T) {
	data := sizeCapData("wedgedwd", 64)
	path := filepath.Join(t.TempDir(), "wd.json")

	// Measure through a deliberately huge ceiling, then sit one byte under it.
	require.NoError(t, data.SaveToJSON(path, WithDataFileMaxSize(1<<40)))
	size := fileSizeAt(t, path)

	err := data.SaveToJSON(filepath.Join(t.TempDir(), "refused.json"), WithDataFileMaxSize(size-1))
	require.Error(t, err, "over-ceiling SaveToJSON must fail, not succeed-then-wedge")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), strconv.FormatInt(size, 10), "error must name the ACTUAL size")
	assert.Contains(t, err.Error(), strconv.FormatInt(size-1, 10), "error must name the CEILING")
}

// TestSizeCap_DataFileSymmetry_BothHalvesHonourOption is the anti-drift guarantee
// for the store-less pair. The stores get symmetry structurally (one field, read
// by both sides); these are two independent calls with no field to share, so the
// equivalent is that BOTH halves resolve through resolveDataFileMaxSize.
//
// Shrinking the package default BELOW the payload is what makes this bite: if
// either half stops consulting the option and falls back to the default, that
// half fails and this test goes red. Without the inversion, a half that ignored
// the option would simply use the (larger) default and stay green.
func TestSizeCap_DataFileSymmetry_BothHalvesHonourOption(t *testing.T) {
	data := sizeCapData("symmetric", 64)
	dir := t.TempDir()
	path := filepath.Join(dir, "symmetric.json")

	require.NoError(t, data.SaveToJSON(path, WithDataFileMaxSize(1<<40)))
	size := fileSizeAt(t, path)
	require.NoError(t, os.Remove(path))

	origDefault := defaultMaxFileSize
	defaultMaxFileSize = 512
	t.Cleanup(func() { defaultMaxFileSize = origDefault })
	require.Less(t, defaultMaxFileSize, size,
		"sanity: the package default must be BELOW the payload for either half to bite")

	raised := WithDataFileMaxSize(size * 2)

	// If SaveToJSON stops resolving the option, it falls back to 512 and refuses.
	require.NoError(t, data.SaveToJSON(path, raised),
		"SaveToJSON must honour the option, not the package default")

	// If LoadFromJSON stops resolving the option, it falls back to 512 and rejects.
	back := NewWorkflowData("symmetric")
	require.NoError(t, back.LoadFromJSON(path, raised),
		"LoadFromJSON must honour the option, not the package default")

	// And the pair must still agree at the default: no option => both use it.
	require.Error(t, data.SaveToJSON(filepath.Join(dir, "nodefault.json")),
		"with no option both halves fall back to the package default, which this payload exceeds")
}

// TestSizeCap_MigrateToFlatBuffers_InheritsStoreCeiling covers the gap the store
// option alone left open: MigrateToFlatBuffers read through readBoundedFileCapped at the
// PACKAGE DEFAULT and built its destination store with the DEFAULT ceiling, so a
// store opened with a raised ceiling to rescue an oversized .json could Load it
// and then fail to migrate it — a recovery path that works for Load and breaks for
// migration. Both halves now inherit s.maxFileSize.
func TestSizeCap_MigrateToFlatBuffers_InheritsStoreCeiling(t *testing.T) {
	dir := t.TempDir()
	data := sizeCapData("migrated", 64)

	// An oversized .json already on disk, as a wedged consumer would have.
	big, err := NewJSONFileStore(dir, WithJSONMaxFileSize(1<<40))
	require.NoError(t, err)
	require.NoError(t, big.Save(data))
	size := fileSizeAt(t, filepath.Join(dir, "migrated.json"))

	// Shrink the package default below it. If either half of the migration
	// consulted the default instead of the store field, this fails.
	origDefault := defaultMaxFileSize
	defaultMaxFileSize = 512
	t.Cleanup(func() { defaultMaxFileSize = origDefault })
	require.Less(t, defaultMaxFileSize, size, "sanity: default must be below the file for this to bite")

	raised, err := NewJSONFileStore(dir, WithJSONMaxFileSize(size*4))
	require.NoError(t, err)

	fbStore, err := raised.MigrateToFlatBuffers(false)
	require.NoError(t, err, "migration must read at the STORE ceiling, not the package default")
	require.NotNil(t, fbStore)

	// The destination store must have inherited the ceiling too, or the converted
	// .fb could not have been written.
	got, err := fbStore.Load("migrated")
	require.NoError(t, err, "the migrated .fb must be readable at the inherited ceiling")
	require.NotNil(t, got)
	assert.Equal(t, "migrated", got.GetWorkflowID())
}

// fileSizeAt returns the on-disk size of path.
func fileSizeAt(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Size()
}

// TestSizeCap_DefaultsUnchanged pins requirement 4: normal-sized state is entirely
// unaffected. A store built with no options keeps the 64 MiB default and round-trips.
func TestSizeCap_DefaultsUnchanged(t *testing.T) {
	data := sizeCapData("normal", 8)

	t.Run("json", func(t *testing.T) {
		s, err := NewJSONFileStore(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, s.maxFileSize, "default ceiling must be the package default")
		require.NoError(t, s.Save(data))
		got, err := s.Load("normal")
		require.NoError(t, err)
		require.NotNil(t, got)
	})

	t.Run("flatbuffers", func(t *testing.T) {
		s, err := NewFlatBuffersStore(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, s.maxFileSize, "default ceiling must be the package default")
		require.NoError(t, s.Save(data))
		got, err := s.Load("normal")
		require.NoError(t, err)
		require.NotNil(t, got)
	})
}

// TestSizeCap_NonPositiveCeilingIgnored pins the option's guard: a zero or negative
// ceiling would otherwise make every Save fail, turning a misconfiguration into a
// total outage.
func TestSizeCap_NonPositiveCeilingIgnored(t *testing.T) {
	for _, n := range []int64{0, -1} {
		js, err := NewJSONFileStore(t.TempDir(), WithJSONMaxFileSize(n))
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, js.maxFileSize, "non-positive ceiling must be ignored")

		fs, err := NewFlatBuffersStore(t.TempDir(), WithFlatBuffersMaxFileSize(n))
		require.NoError(t, err)
		assert.Equal(t, defaultMaxFileSize, fs.maxFileSize, "non-positive ceiling must be ignored")
	}
}

// TestSizeCap_MaxInt64CeilingDoesNotWedgeLoad covers the overflow the option itself
// made reachable (review F2). The bounded-read idiom is LimitReader(ceiling+1), so a
// ceiling of math.MaxInt64 wraps that +1 to MinInt64, LimitReader returns EOF
// immediately, and the zero-byte result passes the `len > ceiling` check — turning
// every Load of a perfectly good file into ErrCorruptData. math.MaxInt64 is the
// natural way to write "no limit", so this must not be a footgun.
func TestSizeCap_MaxInt64CeilingDoesNotWedgeLoad(t *testing.T) {
	data := sizeCapData("nolimit", 16)

	t.Run("json", func(t *testing.T) {
		s, err := NewJSONFileStore(t.TempDir(), WithJSONMaxFileSize(math.MaxInt64))
		require.NoError(t, err)
		assert.Equal(t, maxAllowedCeiling, s.maxFileSize, "MaxInt64 must be clamped so ceiling+1 stays representable")
		require.NoError(t, s.Save(data))
		got, err := s.Load("nolimit")
		require.NoError(t, err, `"no limit" must not turn every Load into a corruption report`)
		require.NotNil(t, got)
	})

	t.Run("flatbuffers", func(t *testing.T) {
		s, err := NewFlatBuffersStore(t.TempDir(), WithFlatBuffersMaxFileSize(math.MaxInt64))
		require.NoError(t, err)
		assert.Equal(t, maxAllowedCeiling, s.maxFileSize)
		require.NoError(t, s.Save(data))
		got, err := s.Load("nolimit")
		require.NoError(t, err)
		require.NotNil(t, got)
	})

	t.Run("datafile pair", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nolimit.json")
		require.NoError(t, data.SaveToJSON(path, WithDataFileMaxSize(math.MaxInt64)))
		back := NewWorkflowData("nolimit")
		require.NoError(t, back.LoadFromJSON(path, WithDataFileMaxSize(math.MaxInt64)))
	})

	// readLimit is the point-of-use guard behind the option clamp: even if an
	// internal caller passes a raw MaxInt64, the bound must saturate, not wrap.
	t.Run("readLimit saturates", func(t *testing.T) {
		assert.Equal(t, int64(math.MaxInt64), readLimit(math.MaxInt64), "must saturate, not wrap to MinInt64")
		assert.Equal(t, int64(11), readLimit(10), "normal case is still ceiling+1")
	})
}

// TestSizeCap_RefusedSyncRetainsPendingCheckpoint covers review F4. Sync's
// postcondition is "nil implies fsync-durable". The guard makes an over-ceiling
// flush fail deterministically and repeatably, so if Sync TOOK the pending entry
// before writing, the retained state would be destroyed and the SECOND Sync would
// find an empty map and return nil with nothing on disk — telling a host retrying
// the durability floor that the park is durable when nothing was ever written.
func TestSizeCap_RefusedSyncRetainsPendingCheckpoint(t *testing.T) {
	data := sizeCapData("retained", 64)
	size := measuredSize(t, data, "fb")

	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir,
		WithDurabilityMode(Batched(4)),
		WithFlatBuffersMaxFileSize(size-1),
	)
	require.NoError(t, err)
	require.NoError(t, store.SaveCheckpoint(data))

	// First Sync: refused by the guard.
	err = store.Sync("retained")
	require.Error(t, err, "the over-ceiling flush must be refused")
	assert.ErrorIs(t, err, ErrValidation)

	// The pending state must SURVIVE the refusal, or the retry below is silently
	// a no-op that reports success.
	require.NotNil(t, store.peekPendingCheckpoint("retained"),
		"a refused flush must retain the pending checkpoint, not destroy it")

	// Second Sync must NOT return nil-with-nothing-written. It must fail the same
	// way, because nothing has changed.
	err = store.Sync("retained")
	require.Error(t, err,
		"a repeat Sync must not return nil while nothing is on disk — nil means fsync-durable")
	assert.ErrorIs(t, err, ErrValidation)

	_, statErr := os.Stat(filepath.Join(dir, "retained.fb"))
	assert.True(t, os.IsNotExist(statErr), "nothing may have landed on disk")

	// And once the obstruction is gone, the retained state still flushes — proving
	// retention is useful, not merely present.
	roomy, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(size*4))
	require.NoError(t, err)
	require.NoError(t, roomy.SaveCheckpoint(data))
	require.NoError(t, roomy.Sync("retained"))
	got, err := roomy.Load("retained")
	require.NoError(t, err)
	require.NotNil(t, got)
}

// TestSizeCap_SignalMailbox_DeliverRefusesOverCeiling covers review F1, the FIFTH
// write site. deliverSignalToDir wrote via writeFileAtomic with no guard while
// takeSignalsFromDir read every entry through a byte cap — and because ONE
// over-ceiling entry fails the read of the WHOLE mailbox, an unguarded delivery
// could permanently strand a WaitForSignal run. Signal.Payload is host-supplied
// `any`, which is what makes the encoded size unbounded.
func TestSizeCap_SignalMailbox_DeliverRefusesOverCeiling(t *testing.T) {
	for _, kind := range []string{"json", "fb"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			mk := func(ceiling int64) SignalStore {
				if kind == "json" {
					s, err := NewJSONFileStore(dir, WithJSONMaxFileSize(ceiling))
					require.NoError(t, err)
					return s
				}
				s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxFileSize(ceiling))
				require.NoError(t, err)
				return s
			}

			big := Signal{ID: "s1", Name: "go", Payload: strings.Repeat("x", 4096)}

			// Over the ceiling: the delivery must be refused at the write.
			err := mk(512).DeliverSignal("wf", big)
			require.Error(t, err, "an over-ceiling signal must be refused, not written")
			assert.ErrorIs(t, err, ErrValidation)

			// Nothing was written, so the mailbox still reads cleanly. This is the
			// property that matters: one bad entry must not poison the whole read.
			sigs, err := mk(512).TakeSignals("wf")
			require.NoError(t, err, "a refused delivery must leave the mailbox readable")
			assert.Empty(t, sigs)

			// Under a ceiling that accommodates it, the same signal round-trips, and
			// the reader honours the store's ceiling rather than the package default.
			roomy := mk(1 << 20)
			require.NoError(t, roomy.DeliverSignal("wf", big))
			sigs, err = roomy.TakeSignals("wf")
			require.NoError(t, err, "TakeSignals must read at the STORE ceiling")
			require.Len(t, sigs, 1)
			assert.Equal(t, "s1", sigs[0].ID)
		})
	}
}

// TestSizeCap_SignalMailbox_ReaderHonoursStoreCeiling is the anti-drift leg for the
// mailbox pair: with the package default shrunk BELOW the entry, a reader that
// consulted the default instead of the store field would reject a legitimately
// delivered signal. Before this fix the reader was hard-wired to the default and
// ignored the store's ceiling even when raised.
func TestSizeCap_SignalMailbox_ReaderHonoursStoreCeiling(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir, WithJSONMaxFileSize(1<<20))
	require.NoError(t, err)

	sig := Signal{ID: "s1", Name: "go", Payload: strings.Repeat("x", 4096)}
	require.NoError(t, store.DeliverSignal("wf", sig))

	origDefault := defaultMaxFileSize
	defaultMaxFileSize = 512
	t.Cleanup(func() { defaultMaxFileSize = origDefault })

	sigs, err := store.TakeSignals("wf")
	require.NoError(t, err,
		"TakeSignals must read at the store ceiling, not the package default")
	require.Len(t, sigs, 1)
}

// --- the ELEMENT-COUNT axis ------------------------------------------------
//
// Every Load path enforces TWO caps: bytes AND per-section/per-vector element count
// (defaultMaxElements). Before this, every write path enforced only the first, so
// state of defaultMaxElements+1 short keys — ~22 MB of JSON, ~42 MB of FlatBuffers,
// both comfortably UNDER the 64 MiB byte ceiling — Saved with err=nil and then failed
// Load forever with "element count exceeds max".
//
// Strictly worse than the byte wedge it hid behind: defaultMaxElements was a const
// with no option, so the documented recovery path was inert against it. These tests
// pin both halves — the guard AND the knob.

// elementData builds a WorkflowData whose `data` section has exactly n entries.
func elementData(id string, n int) *WorkflowData {
	d := NewWorkflowData(id)
	for i := 0; i < n; i++ {
		d.Set("k"+strconv.Itoa(i), "v")
	}
	return d
}

func TestElementCap_JSONSave_RefusesOverCeiling(t *testing.T) {
	data := elementData("manykeys", 40)

	dir := t.TempDir()
	store, err := NewJSONFileStore(dir, WithJSONMaxElements(39))
	require.NoError(t, err)

	err = store.Save(data)
	require.Error(t, err, "over-count Save must fail, not succeed-then-wedge")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), "40", "error must name the ACTUAL count")
	assert.Contains(t, err.Error(), "39", "error must name the CEILING")

	_, statErr := os.Stat(filepath.Join(dir, "manykeys.json"))
	assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")
}

func TestElementCap_FlatBuffersSave_RefusesOverCeiling(t *testing.T) {
	data := elementData("manykeysfb", 40)

	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxElements(39))
	require.NoError(t, err)

	err = store.Save(data)
	require.Error(t, err, "over-count FB Save must fail")
	assert.ErrorIs(t, err, ErrValidation)

	_, statErr := os.Stat(filepath.Join(dir, "manykeysfb.fb"))
	assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")
}

func TestElementCap_GroupCommitPath_RefusesOverCeiling(t *testing.T) {
	data := elementData("manykeysgc", 40)

	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir,
		WithDurabilityMode(Batched(4)), WithFlatBuffersMaxElements(39))
	require.NoError(t, err)

	require.NoError(t, store.SaveCheckpoint(data))
	err = store.Sync("manykeysgc")
	require.Error(t, err, "the group-commit flush must enforce the element cap too")
	assert.ErrorIs(t, err, ErrValidation)
}

func TestElementCap_SaveToJSON_RefusesOverCeiling(t *testing.T) {
	data := elementData("manykeyswd", 40)
	path := filepath.Join(t.TempDir(), "wd.json")

	err := data.SaveToJSON(path, WithDataFileMaxElements(39))
	require.Error(t, err, "over-count SaveToJSON must fail")
	assert.ErrorIs(t, err, ErrValidation)
}

// TestElementCap_BoundarySymmetry is the anti-drift property for this axis: at-cap
// must be accepted by BOTH sides, cap+1 refused by the writer.
func TestElementCap_BoundarySymmetry(t *testing.T) {
	for _, kind := range []string{"json", "fb"} {
		t.Run(kind, func(t *testing.T) {
			const n = 32
			data := elementData("boundary", n)

			mk := func(ceiling int) WorkflowStore {
				dir := t.TempDir()
				if kind == "json" {
					s, err := NewJSONFileStore(dir, WithJSONMaxElements(ceiling))
					require.NoError(t, err)
					return s
				}
				s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxElements(ceiling))
				require.NoError(t, err)
				return s
			}

			atCap := mk(n)
			require.NoError(t, atCap.Save(data), "at-cap Save must be accepted")
			got, err := atCap.Load("boundary")
			require.NoError(t, err, "at-cap Load must be accepted — both sides must agree on the boundary")
			require.NotNil(t, got)

			require.Error(t, mk(n-1).Save(data), "cap+1 Save must be refused")
		})
	}
}

// TestElementCap_RaisedCeilingRecoversExistingFile is why the knob is not optional.
// Guarding the write without a raisable ceiling would convert a silent wedge into a
// HARD PRODUCT LIMIT: a consumer with legitimate large state, or an over-count file
// already on disk, would have no escape at all. The byte option cannot help here —
// over-count state sits far under the byte ceiling.
func TestElementCap_RaisedCeilingRecoversExistingFile(t *testing.T) {
	for _, kind := range []string{"json", "fb"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			const n = 40
			data := elementData("recover", n)

			mk := func(ceiling int) WorkflowStore {
				if kind == "json" {
					s, err := NewJSONFileStore(dir, WithJSONMaxElements(ceiling))
					require.NoError(t, err)
					return s
				}
				s, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxElements(ceiling))
				require.NoError(t, err)
				return s
			}

			// The wedged file: written under a generous ceiling.
			require.NoError(t, mk(n*4).Save(data))

			// A low-ceiling store cannot read it — the wedge.
			_, err := mk(n - 1).Load("recover")
			require.Error(t, err, "the low-ceiling store must reject the over-count file")
			assert.ErrorIs(t, err, ErrCorruptData)

			// Raising the ceiling recovers it. The escape hatch the const never had.
			got, err := mk(n * 4).Load("recover")
			require.NoError(t, err, "raising the element ceiling must make the file readable again")
			require.NotNil(t, got)

			// Anti-drift, and the leg that actually bites. Every ceiling above sits far
			// BELOW the package default, so a Load reverting to the default would merely
			// be more permissive and leave this test green. Shrinking the default below
			// the payload inverts it: a Load consulting the default now REJECTS state the
			// store's own ceiling allows.
			origDefault := defaultMaxElements
			defaultMaxElements = n / 2
			t.Cleanup(func() { defaultMaxElements = origDefault })
			require.Less(t, defaultMaxElements, n,
				"sanity: the package default must be BELOW the payload for this leg to bite")

			back, err := mk(n * 4).Load("recover")
			require.NoError(t, err,
				"Load must honour the STORE element ceiling, not the package default — "+
					"a Load that reads defaultMaxElements re-arms the asymmetry")
			require.NotNil(t, back)
		})
	}
}

// TestElementCap_DataFilePairSymmetry is the store-less pair's anti-drift leg: both
// halves must resolve the element ceiling through the shared config, so shrinking the
// package default below the payload reddens whichever half stopped consulting it.
func TestElementCap_DataFilePairSymmetry(t *testing.T) {
	const n = 40
	data := elementData("pair", n)
	path := filepath.Join(t.TempDir(), "pair.json")

	// Shrink the package default BELOW the payload, or a half that ignores the option
	// simply falls back to the (much larger) default and this test stays green.
	origDefault := defaultMaxElements
	defaultMaxElements = n / 2
	t.Cleanup(func() { defaultMaxElements = origDefault })
	require.Less(t, defaultMaxElements, n, "sanity: default must be below the payload to bite")

	raised := WithDataFileMaxElements(n * 4)
	require.NoError(t, data.SaveToJSON(path, raised),
		"SaveToJSON must honour the element option, not the package default")

	back := NewWorkflowData("pair")
	require.NoError(t, back.LoadFromJSON(path, raised),
		"LoadFromJSON must honour the element option — otherwise the pair re-arms the asymmetry")
	_, ok := back.Get("k0")
	assert.True(t, ok, "the recovered data must actually be present")
}

// TestElementCap_DefaultsUnchanged pins that ordinary state is untouched: the default
// ceiling is the same const every Load has always enforced.
func TestElementCap_DefaultsUnchanged(t *testing.T) {
	data := elementData("normal", 8)

	js, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, defaultMaxElements, js.maxElements)
	require.NoError(t, js.Save(data))

	fs, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, defaultMaxElements, fs.maxElements)
	require.NoError(t, fs.Save(data))
}

// TestZeroCeiling_StructLiteralStoreStillWorks covers the latent struct-literal case.
// A store built as a literal bypasses both constructors, leaving maxFileSize and
// maxElements at zero — which without a floor makes checkWriteSize refuse EVERY write
// and readLimit(0) report every non-empty file corrupt. batchK already defends this
// shape; these ceilings now do too.
func TestZeroCeiling_StructLiteralStoreStillWorks(t *testing.T) {
	data := sizeCapData("literal", 4)

	t.Run("json", func(t *testing.T) {
		s := &JSONFileStore{baseDir: t.TempDir()} // zero ceilings, no constructor
		require.NoError(t, s.Save(data), "a zero ceiling must floor to the default, not refuse every write")
		got, err := s.Load("literal")
		require.NoError(t, err, "a zero ceiling must not make every Load report corruption")
		require.NotNil(t, got)
	})

	t.Run("flatbuffers", func(t *testing.T) {
		s := &FlatBuffersStore{baseDir: t.TempDir(), ckptCount: make(map[string]uint)}
		require.NoError(t, s.Save(data))
		got, err := s.Load("literal")
		require.NoError(t, err)
		require.NotNil(t, got)
	})
}

// TestZeroCeiling_ReadBoundedFileCappedPath covers review F3. The zero-floor test
// above passes through Save/Load, NEITHER of which routes through
// readBoundedFileCapped — so that function's own missing floor stayed green. These
// are the two readers that DO route through it.
func TestZeroCeiling_ReadBoundedFileCappedPath(t *testing.T) {
	t.Run("TakeSignals", func(t *testing.T) {
		dir := t.TempDir()
		seed, err := NewJSONFileStore(dir)
		require.NoError(t, err)
		require.NoError(t, seed.DeliverSignal("wf", Signal{ID: "s1", Name: "go", Payload: "p"}))

		lit := &JSONFileStore{baseDir: dir} // zero ceilings, no constructor
		sigs, err := lit.TakeSignals("wf")
		require.NoError(t, err, "a zero ceiling must floor to the default, not report every entry corrupt")
		require.Len(t, sigs, 1)
	})

	t.Run("MigrateToFlatBuffers", func(t *testing.T) {
		dir := t.TempDir()
		seed, err := NewJSONFileStore(dir)
		require.NoError(t, err)
		require.NoError(t, seed.Save(sizeCapData("mig", 4)))

		lit := &JSONFileStore{baseDir: dir}
		fbStore, err := lit.MigrateToFlatBuffers(false)
		require.NoError(t, err, "migration must not report a valid file corrupt on a zero ceiling")
		require.NotNil(t, fbStore)
	})
}

// waitsData builds a WorkflowData whose WAITS section has n entries and whose other
// sections are small — so `waits` is the section that decides the element count.
func waitsData(id string, n int) *WorkflowData {
	d := NewWorkflowData(id)
	for i := 0; i < n; i++ {
		d.SetWait("timer"+strconv.Itoa(i), int64(1_700_000_000_000_000_000+i))
	}
	return d
}

// TestElementCap_WaitsSectionIsCounted covers review F4. Both Load paths cap the
// waits section, and the write-side counting of waits is the only thing between a
// waits-dominant snapshot (M10 durable timers — a wide fan-out of timer nodes) and
// the HYG-00 wedge. Every other element-cap test stages data/nodeStatus/outputs, so
// deleting the waits arm from maxSectionCountLocked (JSON) or len(waitOffsets) from
// maxVec (FB) left the ENTIRE package suite green. Correct today, and completely
// unbitten — the state this phase argues is indistinguishable from broken.
func TestElementCap_WaitsSectionIsCounted(t *testing.T) {
	const n = 40

	t.Run("json", func(t *testing.T) {
		data := waitsData("waitsjson", n)
		dir := t.TempDir()
		store, err := NewJSONFileStore(dir, WithJSONMaxElements(n-1))
		require.NoError(t, err)

		err = store.Save(data)
		require.Error(t, err, "a waits-dominant snapshot over the ceiling must be refused")
		assert.ErrorIs(t, err, ErrValidation)
		assert.Contains(t, err.Error(), strconv.Itoa(n), "the error must name the waits count")

		_, statErr := os.Stat(filepath.Join(dir, "waitsjson.json"))
		assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")

		// Under the ceiling it still round-trips, so the guard is not over-broad.
		roomy, err := NewJSONFileStore(t.TempDir(), WithJSONMaxElements(n*2))
		require.NoError(t, err)
		require.NoError(t, roomy.Save(data))
		got, err := roomy.Load("waitsjson")
		require.NoError(t, err)
		require.NotNil(t, got)
	})

	t.Run("flatbuffers", func(t *testing.T) {
		data := waitsData("waitsfb", n)
		dir := t.TempDir()
		store, err := NewFlatBuffersStore(dir, WithFlatBuffersMaxElements(n-1))
		require.NoError(t, err)

		err = store.Save(data)
		require.Error(t, err, "the FB waits vector must be counted too")
		assert.ErrorIs(t, err, ErrValidation)

		_, statErr := os.Stat(filepath.Join(dir, "waitsfb.fb"))
		assert.True(t, os.IsNotExist(statErr), "a refused Save must not leave a file behind")

		roomy, err := NewFlatBuffersStore(t.TempDir(), WithFlatBuffersMaxElements(n*2))
		require.NoError(t, err)
		require.NoError(t, roomy.Save(data))
		got, err := roomy.Load("waitsfb")
		require.NoError(t, err)
		require.NotNil(t, got)
	})
}
