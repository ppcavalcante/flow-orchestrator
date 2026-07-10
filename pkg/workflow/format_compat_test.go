package workflow

// Phase 55 (M13) — on-disk format compatibility (freeze axis b). The PERMANENT compat suite.
// Golden fixtures at the frozen 0.12 format live in testdata/v012_format_full.{fb,json} —
// COMMITTED BYTES, never regenerated (regenerating would defeat the guard). Fence: fixtures +
// tests only, 0 production/schema change. A non-forward-safe reader is a finding routed UP.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	fb "github.com/ppcavalcante/flow-orchestrator/internal/workflow/fb/workflow"
	"github.com/stretchr/testify/require"
)

const goldenID = "v012_format_full"

// assertV012Fidelity checks the full M10/M11/M12 additive surface decoded from a golden.
func assertV012Fidelity(t *testing.T, d *WorkflowData, tag string) {
	t.Helper()
	status := func(node string, want NodeStatus) {
		st, ok := d.GetNodeStatus(node)
		require.True(t, ok, tag+": "+node+" present")
		require.Equal(t, want, st, tag+": "+node+" status")
	}
	status("bypassed_node", Bypassed)             // M11 wire 6
	status("compensated_node", Compensated)       // M12 wire 7
	status("compfailed_node", CompensationFailed) // M12 wire 8
	status("waiting_node", Waiting)               // M10 wire 5
	status("done_node", Completed)

	fireAt, ok := d.GetWait("waiting_node") // M10 waits
	require.True(t, ok, tag+": waits present")
	require.Equal(t, int64(1893456000000), fireAt, tag+": waits fireAt")

	require.True(t, d.IsRollingBack(), tag+": rolling_back (M12)")
	require.Equal(t, TriggerCanceled, d.TriggerCause(), tag+": trigger_cause (M12 ph49)")

	v, ok := Get(d, NewKey[int64]("balance_cents"))
	require.True(t, ok, tag+": int64 present")
	require.Equal(t, int64(9007199254740993), v, tag+": int64 2^53+1 byte/value-exact")

	// The full data map is load-bearing (review ph55-F2): the string/bool/double decode
	// paths must also round-trip from the golden, not just the int64.
	s, ok := Get(d, NewKey[string]("region"))
	require.True(t, ok, tag+": string present")
	require.Equal(t, "eu-west", s, tag+": string exact")
	bv, ok := Get(d, NewKey[bool]("vip"))
	require.True(t, ok, tag+": bool present")
	require.Equal(t, true, bv, tag+": bool exact")
	dv, ok := Get(d, NewKey[float64]("rate"))
	require.True(t, ok, tag+": double present")
	require.InDelta(t, 3.14159, dv, 1e-12, tag+": double exact")
}

// TestFormatCompat_Backward_v012 — §2: the CURRENT reader reads the FROZEN 0.12 goldens with
// full round-trip fidelity across the additive surface. Axis-(b) BACKWARD-compat evidence.
func TestFormatCompat_Backward_v012(t *testing.T) {
	t.Run("FB", func(t *testing.T) {
		fb, err := NewFlatBuffersStore("testdata")
		require.NoError(t, err)
		d, err := fb.Load(goldenID)
		require.NoError(t, err, "current reader must read the 0.12 FB golden")
		assertV012Fidelity(t, d, "FB")
	})
	t.Run("JSON", func(t *testing.T) {
		js, err := NewJSONFileStore("testdata")
		require.NoError(t, err)
		d, err := js.Load(goldenID)
		require.NoError(t, err, "current reader must read the 0.12 JSON golden")
		assertV012Fidelity(t, d, "JSON")
	})

	// Bite: corrupting a VALUE in the golden reddens the fidelity assertion (proving it is
	// non-vacuous — it would catch a real decode regression). Flip the compensated_node status
	// in the JSON golden; the fidelity check requires Compensated, so it must now fail.
	t.Run("bite_corrupt_value", func(t *testing.T) {
		dir := t.TempDir()
		raw, err := os.ReadFile(filepath.Join("testdata", goldenID+".json"))
		require.NoError(t, err)
		// Corrupt a decoded VALUE: compensated_node "compensated" -> "completed".
		corrupt := strings.Replace(string(raw), `"compensated_node":"compensated"`, `"compensated_node":"completed"`, 1)
		require.NotEqual(t, string(raw), corrupt, "the corruption actually changed the bytes")
		require.NoError(t, os.WriteFile(filepath.Join(dir, goldenID+".json"), []byte(corrupt), 0o600))
		js, err := NewJSONFileStore(dir)
		require.NoError(t, err)
		d, err := js.Load(goldenID)
		require.NoError(t, err, "the corrupted JSON still loads (structurally valid)")
		st, _ := d.GetNodeStatus("compensated_node")
		require.NotEqual(t, Compensated, st, "the corrupted value decodes differently → the fidelity assertion would redden")
	})
}

// TestFormatCompat_Forward_JSON — §3 (JSON half): the CURRENT reader tolerates a genuinely-
// unknown JSON key (a simulated future 1.x field), ignoring it, WITHOUT error and WITHOUT
// losing fidelity. This is the evidence that commits 1.x JSON to additive-only evolution.
// The key is injected at the STRING level (no re-marshal), so it is genuinely on-disk and the
// int64 fidelity is not disturbed by the injection itself.
func TestFormatCompat_Forward_JSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", goldenID+".json"))
	require.NoError(t, err)

	// Inject two unknown keys right after the opening brace (string-level, non-corrupting).
	txt := string(raw)
	i := strings.Index(txt, "{")
	require.GreaterOrEqual(t, i, 0)
	injected := txt[:i+1] + `"future_field_1x":123,"nested_future":{"x":[1,2,3]},` + txt[i+1:]
	require.Contains(t, injected, "future_field_1x", "the unknown key is genuinely present in the bytes")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, goldenID+".json"), []byte(injected), 0o600))

	js, err := NewJSONFileStore(dir)
	require.NoError(t, err)
	d, err := js.Load(goldenID)
	require.NoError(t, err, "current reader must tolerate an unknown JSON key (forward-compat), got error")
	assertV012Fidelity(t, d, "JSON+unknown-key") // ignored the unknown key, fidelity intact
}

// TestFormatCompat_Forward_FB — §3 (FB half, the load-bearing one): the CURRENT 0.12 reader
// tolerates a genuinely-UNKNOWN FlatBuffers field (a simulated future "1.1" field at a slot
// HIGHER than any 0.12 field), reading the buffer WITHOUT error and ignoring the unknown
// field. Built with the production fb builder + a low-level PrependUOffsetTSlot at slot 11
// (trigger_cause is the highest 0.12 field, slot 10) — a genuine unknown-id field, not a no-op.
// This is EXERCISED firsthand (per the provisional-until-falsified discipline): a buffer with
// the extra field is put in front of the real reader. If it did NOT tolerate it → a finding.
func TestFormatCompat_Forward_FB(t *testing.T) {
	builder := flatbuffers.NewBuilder(0)
	wfID := builder.CreateString("fwd-tolerant")
	// The "1.1" field payload — a string the 0.12 schema does not define.
	futureField := builder.CreateString("a-future-1.1-field-the-0.12-reader-must-ignore")

	// StartObject(12) not WorkflowStateStart (which sizes the vtable for the 0.12's 11 slots
	// 0-10) — we need room for slot 11. The Add helpers still write their own slots 0-10.
	builder.StartObject(12)
	fb.WorkflowStateAddWorkflowId(builder, wfID)
	fb.WorkflowStateAddRollingBack(builder, true)   // slot 9 (known)
	fb.WorkflowStateAddTriggerCause(builder, 2)     // slot 10 (known, = canceled)
	builder.PrependUOffsetTSlot(11, futureField, 0) // slot 11 — UNKNOWN to the 0.12 schema
	ws := builder.EndObject()
	builder.Finish(ws)
	buf := builder.FinishedBytes()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fwdfb.fb"), buf, 0o600))

	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	d, err := store.Load("fwdfb")
	// THE load-bearing assertion — exercised, not inferred:
	require.NoError(t, err, "the 0.12 FB reader must tolerate an unknown higher-id field (forward-compat)")
	require.True(t, d.IsRollingBack(), "the known slot-9 field decoded past the unknown field")
	require.Equal(t, TriggerCanceled, d.TriggerCause(), "the known slot-10 field decoded past the unknown field")

	// Non-vacuity: the unknown field is GENUINELY in the buffer (its string bytes are present),
	// so a strict reader WOULD see it — this is not a no-op injection.
	require.Contains(t, string(buf), "a-future-1.1-field", "the unknown field is genuinely present in the buffer bytes")

	// (review ph55-F1) The unknown-field TYPE is irrelevant to FlatBuffers vtable tolerance: an
	// old reader never dereferences an unknown slot, so a string at slot 11 is representative of
	// any higher-slot addition (scalar/vector/table). One shape suffices for the property.
}
