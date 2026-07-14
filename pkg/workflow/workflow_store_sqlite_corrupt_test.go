package workflow

// M16 ph78 — CORRUPT-SCAN fault injection (task #126, class B). A malformed durable row (a forged
// or bit-rotted DB) must surface a TYPED ErrCorruptData on Load, NEVER a panic and never silent
// bad state. We inject the malformed row directly via the store's own *sql.DB (a forged DB is the
// exact threat: the store trusts its journal, so the scan-time guards are the last line). No driver
// wrapper needed — the corruption is in the DATA, not the driver.
//
// ANTI-VACUITY (ph78 bar): each test is paired with a seed-break note — the guard it exercises
// (isKnownStatus / the kind default) is what turns the bad row into ErrCorruptData; remove it and
// Load would either panic or restore forged state. The `isKnownStatus`/kind-default guards ARE the
// production seed-break anchors (removing `default: return ErrCorruptData` lets the bad row through).

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// mkCorruptStore builds an ordinary single-process store with one clean committed workflow, then
// returns it for a raw-row corruption + reload.
func mkCorruptStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "corrupt.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	require.NoError(t, s.Save(d), "seed one clean workflow")
	return s
}

// TestCorruptScan_UnknownNodeStatus — a nodes row with a bogus status string (a forged/bit-rotted
// DB) → Load returns ErrCorruptData, not a panic, not a silently-restored bad status. Covers
// scanNodes' isKnownStatus guard (codec.go:129-130).
func TestCorruptScan_UnknownNodeStatus(t *testing.T) {
	s := mkCorruptStore(t)
	// Forge a node row with a status that is not a known NodeStatus.
	_, err := s.db.Exec(
		`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)`,
		"wf", "nBad", "TOTALLY_BOGUS_STATUS", "", 0)
	require.NoError(t, err, "inject the forged row")

	_, loadErr := s.Load("wf")
	require.Error(t, loadErr, "a forged node status must NOT load silently")
	require.ErrorIs(t, loadErr, ErrCorruptData,
		"scanNodes must surface ErrCorruptData for an unknown status (the corrupt-DB guard) — SEED-BREAK: remove isKnownStatus → the bogus status restores silently, this reddens")
	require.NotErrorIs(t, loadErr, ErrIO, "a corrupt row is ErrCorruptData, not ErrIO (distinct domains)")
}

// TestCorruptScan_UnknownDataKind — a data_kv row with an out-of-range kind discriminator → Load
// returns ErrCorruptData. Covers scanDataKV's `default: unknown kind` (codec.go:100-101).
func TestCorruptScan_UnknownDataKind(t *testing.T) {
	s := mkCorruptStore(t)
	_, err := s.db.Exec(
		`INSERT INTO data_kv (workflow_id, key, kind, i_val, f_val, s_val) VALUES (?,?,?,?,?,?)`,
		"wf", "kBad", 99 /* not kvInt/kvBool/kvFloat/kvString */, 0, 0.0, "")
	require.NoError(t, err, "inject the forged kv row")

	_, loadErr := s.Load("wf")
	require.Error(t, loadErr, "a forged data kind must NOT load silently")
	require.ErrorIs(t, loadErr, ErrCorruptData,
		"scanDataKV must surface ErrCorruptData for an unknown kind — SEED-BREAK: remove the `default` arm → the bad kind is silently dropped, this reddens")
}

// TestCorruptScan_NoPanicOnForgedRows — the totality property: NO forged row shape panics Load. A
// store is an input TCB (M9 threat model); a scan-time guard that panics instead of returning a
// typed error is a DoS on the reader. Drive several forgeries; each returns a clean error.
func TestCorruptScan_NoPanicOnForgedRows(t *testing.T) {
	forgeries := []struct {
		name string
		stmt string
		args []interface{}
	}{
		{"bogus-status", `INSERT INTO nodes (workflow_id, node_name, status, has_output) VALUES (?,?,?,0)`,
			[]interface{}{"wf", "x", "NOPE"}},
		{"bogus-kind", `INSERT INTO data_kv (workflow_id, key, kind) VALUES (?,?,?)`,
			[]interface{}{"wf", "y", 77}},
	}
	for _, f := range forgeries {
		t.Run(f.name, func(t *testing.T) {
			s := mkCorruptStore(t)
			_, err := s.db.Exec(f.stmt, f.args...)
			require.NoError(t, err)
			// The load MUST return (an error), never panic. require.NotPanics captures the totality.
			require.NotPanics(t, func() {
				_, loadErr := s.Load("wf")
				require.ErrorIs(t, loadErr, ErrCorruptData, "forged %q → typed ErrCorruptData", f.name)
			}, "Load must not panic on a forged row — a store is an input TCB (M9)")
		})
	}
}

// TestEncodeKV_AllTypeArms — round-trips every encodeKV type arm (int32/float32/bool + the JSON
// fallback for a complex value) through Save→Load, asserting the Go type + value survive. Covers the
// int32/float32/bool/default arms of encodeKV (a plain coverage gap the debt left — the store's own
// tests only exercised int/int64/float64/string). Not a fault path; real round-trip behavior.
func TestEncodeKV_AllTypeArms(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kv.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup

	d := NewWorkflowData("wf")
	d.Set("i32", int32(42))    // → kvInt (int64 42)
	d.Set("f32", float32(1.5)) // → kvFloat (float64 1.5)
	d.Set("b", true)           // → kvBool
	d.Set("cplx", []int{1, 2}) // complex → JSON string "[1,2]" (the default arm)
	require.NoError(t, s.Save(d))

	got, err := s.Load("wf")
	require.NoError(t, err)
	// int32/float32 widen to int64/float64 (the documented FB-parity reconstruction).
	vi, _ := got.Get("i32")
	require.Equal(t, int64(42), vi, "int32 → int64 42 round-trips")
	vf, _ := got.Get("f32")
	require.Equal(t, float64(float32(1.5)), vf, "float32 → float64 round-trips")
	vb, _ := got.Get("b")
	require.Equal(t, true, vb, "bool round-trips")
	vc, _ := got.Get("cplx")
	require.Equal(t, "[1,2]", vc, "a complex value → JSON string (the encodeKV default arm)")
}
