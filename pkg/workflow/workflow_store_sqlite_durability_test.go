package workflow

// M15 ph68 — durability-mode contract tests. These prove the mode MAPPING is correct
// (pragmas applied; Batched Loads byte-identical to Strict; the Sync floor forces durable
// in both modes) — the durability BITES. The deep-durable perf benchmarks (the ph68
// fallback O(N²) curve + the ph69 delta O(N) curve) live in workflow_store_sqlite_bench_test.go.
// Run non-race (single-writer conn serializes).
//
// NOTE ON POWER-LOSS DURABILITY: a Go test cannot yank power, so these assert the
// OBSERVABLE mode contract (pragmas, byte-fidelity, WAL-flush-on-Sync), not literal
// platter durability. The real power-loss cost is a benchmark number (mode+platform
// stated); the crash-loses-≤K semantic is the WAL model (asserted via wal frame state).

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSQLiteDur_PragmasApplied — the mode mapping is real: Strict = synchronous=FULL +
// fullfsync=1; Batched = synchronous=NORMAL. Read the live pragmas back off the conn.
func TestSQLiteDur_PragmasApplied(t *testing.T) {
	ctx := context.Background()
	read := func(s *SQLiteStore) (jm string, sync, ff int64) {
		require.NoError(t, s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm))
		require.NoError(t, s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync))
		require.NoError(t, s.db.QueryRowContext(ctx, "PRAGMA fullfsync").Scan(&ff))
		return
	}
	dir := t.TempDir()

	strict, err := NewSQLiteStore(filepath.Join(dir, "s.db")) // default Strict
	require.NoError(t, err)
	defer strict.Close() //nolint:errcheck // test cleanup
	jm, sync, ff := read(strict)
	require.Equal(t, "wal", jm)
	// synchronous=FULL(2) is the PORTABLE power-loss-durability primitive: on linux it's
	// the native fdatasync (linux's power-loss floor); on darwin it's plain fsync (which
	// Apple deliberately does NOT flush the drive cache — hence fullfsync=1 below). This
	// assertion is the durability-correctness gate that MUST hold in CI on every platform.
	require.Equal(t, int64(2), sync, "Strict → synchronous=FULL(2) — the portable power-loss primitive")
	// fullfsync is the DARWIN-specific F_FULLFSYNC opt-in (drive-cache flush). We set it so
	// a darwin run is power-loss-durable; on linux it is a no-op and its readback value is
	// platform-dependent, so we only assert it darwin-side. NOT a CI durability gate.
	if runtime.GOOS == "darwin" {
		require.Equal(t, int64(1), ff, "darwin Strict → fullfsync=1 (F_FULLFSYNC drive flush)")
	}

	batched, err := NewSQLiteStore(filepath.Join(dir, "b.db"), SQLiteBatched(4))
	require.NoError(t, err)
	defer batched.Close() //nolint:errcheck // test cleanup
	jm, sync, ff = read(batched)
	require.Equal(t, "wal", jm)
	require.Equal(t, int64(1), sync, "Batched → synchronous=NORMAL(1)")
	// ph68-F1: Batched ALSO sets fullfsync=1 so its every-Kth wal_checkpoint(TRUNCATE)
	// boundary (and the Sync floor) is a real F_FULLFSYNC drive flush on darwin — otherwise
	// the ≤K power-loss bound is hollow (OS-cache only). synchronous=NORMAL still amortizes
	// the per-commit fsync; fullfsync governs whether the fsyncs that DO fire flush the drive.
	if runtime.GOOS == "darwin" {
		require.Equal(t, int64(1), ff, "darwin Batched → fullfsync=1 (checkpoint boundary power-loss-durable)")
	}
}

// TestSQLiteDur_BatchedByteIdenticalToStrict — a durability mode must NOT change the
// persisted state's bytes. Batched(K) over the full M10/M11/M12 fixture Loads
// byte-identical to Strict (fidelity is orthogonal to fsync cadence).
func TestSQLiteDur_BatchedByteIdenticalToStrict(t *testing.T) {
	dir := t.TempDir()
	strict, err := NewSQLiteStore(filepath.Join(dir, "s.db"))
	require.NoError(t, err)
	defer strict.Close() //nolint:errcheck // test cleanup
	batched, err := NewSQLiteStore(filepath.Join(dir, "b.db"), SQLiteBatched(3))
	require.NoError(t, err)
	defer batched.Close() //nolint:errcheck // test cleanup

	ds, db := NewWorkflowData("wf"), NewWorkflowData("wf")
	for _, step := range evolveFixture() {
		step(ds)
		require.NoError(t, strict.SaveCheckpoint(ds))
		step(db)
		require.NoError(t, batched.SaveCheckpoint(db))
	}
	require.NoError(t, batched.Sync("wf")) // floor — flush any un-checkpointed tail

	ls, err := strict.Load("wf")
	require.NoError(t, err)
	lb, err := batched.Load("wf")
	require.NoError(t, err)
	ss, err := ls.Snapshot()
	require.NoError(t, err)
	bs, err := lb.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(ss, bs), "Batched must Load byte-identical to Strict\nstrict:  %s\nbatched: %s", ss, bs)
}

// TestSQLiteDur_SyncFloorDurableBothModes — the suspend/completion floor: after a Sync,
// the workflow's state is flushed out of the WAL into the main DB (wal_checkpoint TRUNCATE
// → 0 remaining frames), in BOTH modes. Reading a fresh store off the same file (WAL
// truncated) sees the full state — the floor holds regardless of batch cadence.
func TestSQLiteDur_SyncFloorDurableBothModes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts []SQLiteOption
	}{
		{"strict", nil},
		{"batched", []SQLiteOption{SQLiteBatched(100)}}, // K large so nothing auto-checkpoints
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wf.db")
			s, err := NewSQLiteStore(path, tc.opts...)
			require.NoError(t, err)

			d := NewWorkflowData("wf")
			for _, step := range evolveFixture() {
				step(d)
				require.NoError(t, s.SaveCheckpoint(d))
			}
			require.NoError(t, s.Sync("wf")) // the floor

			// After Sync, the WAL is TRUNCATE-checkpointed → 0 frames remaining (state is in
			// the main DB file, power-loss-durable at the platform's floor).
			var busy, logFrames, checkpointed int64
			require.NoError(t, s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
				Scan(&busy, &logFrames, &checkpointed))
			require.Equal(t, int64(0), logFrames, "%s: Sync must leave 0 un-checkpointed WAL frames", tc.name)
			require.NoError(t, s.Close())

			// A fresh store on the same file Loads the full state (the floor persisted it).
			s2, err := NewSQLiteStore(path, tc.opts...)
			require.NoError(t, err)
			defer s2.Close() //nolint:errcheck // test cleanup
			loaded, err := s2.Load("wf")
			require.NoError(t, err)
			require.True(t, loaded.IsRollingBack(), "%s: floor persisted rolling_back", tc.name)
			st, _ := loaded.GetNodeStatus("a")
			require.Equal(t, Compensated, st, "%s: floor persisted the final node state", tc.name)
		})
	}
}

// TestSQLiteDur_BatchedResumeIdempotent — a Batched(K) store that "crashes" (close) mid-run
// and reopens re-Loads the last DURABLE frontier; continuing re-applies levels
// idempotently to a byte-identical final state (the M14 loses-≤K + re-run discipline, the
// PERSISTENCE half — effect-idempotency is the executor's IdempotencyKey, out of store scope).
func TestSQLiteDur_BatchedResumeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.db")
	steps := evolveFixture()

	s1, err := NewSQLiteStore(path, SQLiteBatched(2))
	require.NoError(t, err)
	d := NewWorkflowData("wf")
	for _, step := range steps[:3] {
		step(d)
		require.NoError(t, s1.SaveCheckpoint(d))
	}
	require.NoError(t, s1.Sync("wf")) // floor before the "crash" → the frontier is durable
	require.NoError(t, s1.Close())

	// Reopen, Load the durable frontier, continue — idempotent re-application.
	s2, err := NewSQLiteStore(path, SQLiteBatched(2))
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup
	resumed, err := s2.Load("wf")
	require.NoError(t, err)
	for _, step := range steps[3:] {
		step(resumed)
		require.NoError(t, s2.SaveCheckpoint(resumed))
	}
	require.NoError(t, s2.Sync("wf"))

	// Byte-identical to a full Save of the final state.
	full, err := NewSQLiteStore(filepath.Join(dir, "full.db"))
	require.NoError(t, err)
	defer full.Close() //nolint:errcheck // test cleanup
	require.NoError(t, full.Save(resumed))
	got, err := s2.Load("wf")
	require.NoError(t, err)
	want, err := full.Load("wf")
	require.NoError(t, err)
	gs, err := got.Snapshot()
	require.NoError(t, err)
	ws, err := want.Snapshot()
	require.NoError(t, err)
	require.True(t, bytes.Equal(gs, ws), "Batched resume-continue must Load byte-identical to a full Save\ngot:  %s\nwant: %s", gs, ws)
}
