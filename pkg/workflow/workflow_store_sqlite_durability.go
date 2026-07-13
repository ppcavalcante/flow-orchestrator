package workflow

import (
	"context"
	"fmt"
)

// SQLite durability modes (M15 ph68). WithDurabilityMode / DurabilityOption are FB-typed
// (func(*FlatBuffersStore)) and CANNOT be reused, so SQLiteStore gets its own option type.
// The mapping to SQLite's actual durability model:
//
//   Strict     → PRAGMA synchronous=FULL + fullfsync=1 (darwin: real F_FULLFSYNC drive
//                flush, the honest power-loss-durable cost). Every checkpoint COMMIT is
//                power-loss-durable-per-level. fsync-bound (measured ≈3.97ms/level on darwin
//                @ WAL+fullfsync=1, deep-1000; hardware-dependent) — the per-level fsync
//                dominates and the O(Δ) write win is HIDDEN behind it.
//   Batched(K) → PRAGMA synchronous=NORMAL + WAL (+ fullfsync=1): commits append to the WAL
//                without a per-commit fsync; an every-Kth wal_checkpoint(TRUNCATE) flushes
//                durably (F_FULLFSYNC on darwin). A crash loses only the WAL frames since the
//                last checkpoint — bounded to ≤K levels. Re-run is idempotent (the M9/M12
//                IdempotencyKey no-double-apply contract). This mode AMORTIZES the fsync cost
//                over K (~K× fewer full fsyncs). NOTE: the deep-durable WALL-CLOCK stays
//                O(N²) in BOTH modes — shadowFromData re-encodes all N nodes/level (the
//                delta-free Checkpointer interface); the O(Δ) win is the DB-WRITE layer, NOT
//                the wall-clock shape. See finding F-M15-P68-1. Do NOT label this O(N).
//
// The suspend/completion durability FLOOR (Sync) is durable in BOTH modes: Strict's Sync is a
// cheap no-op (every commit already F_FULLFSYNC'd); Batched's Sync forces a
// wal_checkpoint(TRUNCATE) F_FULLFSYNC so a parked/completed run is power-loss-durable
// regardless of the batch cadence (both modes set fullfsync=1 — ph68-F1).
//
// PLATFORM NUANCE (honest framing — do NOT overstate): synchronous=FULL is the PORTABLE
// power-loss-durability primitive. On LINUX it uses the platform's fdatasync, which IS
// linux's native power-loss floor — a linux Strict number is honest (just a DIFFERENT,
// hardware-dependent number than darwin's). The macOS quirk is the EXCEPTION: Apple's
// fsync deliberately does NOT flush the drive cache, which is WHY darwin additionally
// needs fullfsync=1 (F_FULLFSYNC) to be power-loss-durable. So: Strict per-level durable
// cost is fsync-bound and platform-dependent — darwin needs F_FULLFSYNC (≈3.97ms/level
// measured, WAL+fullfsync=1, deep-1000); linux's synchronous=FULL fdatasync is its native
// primitive, a different hardware-dependent number. The ONLY actual
// lie to avoid is presenting a darwin fullfsync=0 (OS-cache 0.4-0.9ms) figure as durable
// (the ph65 trap). The Strict/Batched crash-CORRECTNESS is platform-portable (CI-gated on
// linux); only the fullfsync TIMING number is darwin-specific + SUMMARY-reported.

// sqliteDurability is the resolved durability configuration for a SQLiteStore.
type sqliteDurability struct {
	strict bool // true = synchronous=FULL + fullfsync=1, commit-per-level power-loss-durable
	batchK uint // Batched cadence: force a durable wal_checkpoint(TRUNCATE) every K checkpoints (Strict ⇒ 1)
}

// defaultDurability is Strict (the safe default — every checkpoint power-loss-durable),
// matching the ph66/ph67 synchronous=FULL default.
func defaultDurability() sqliteDurability { return sqliteDurability{strict: true, batchK: 1} }

// SQLiteOption configures a SQLiteStore at construction (variadic on NewSQLiteStore).
type SQLiteOption func(*sqliteDurability)

// SQLiteStrict is the default durability mode: synchronous=FULL + fullfsync=1, every
// checkpoint COMMIT power-loss-durable-per-level (darwin: real F_FULLFSYNC). Pass nothing
// for this default, or WithSQLiteDurability(SQLiteStrict()) explicitly.
func SQLiteStrict() SQLiteOption {
	return func(d *sqliteDurability) { d.strict = true; d.batchK = 1 }
}

// SQLiteBatched sets group-commit durability: synchronous=NORMAL + WAL, forcing a durable
// wal_checkpoint(TRUNCATE) every k checkpoints. A power-loss loses ≤k un-checkpointed levels,
// which resume re-runs idempotently (IdempotencyKey). k must be ≥1 (k=1 ≡ Strict-ish, but
// still synchronous=NORMAL). Weakens per-level power-loss durability for ~k× fewer full
// fsyncs on deep durable runs — you must type Batched to opt in.
func SQLiteBatched(k uint) SQLiteOption {
	return func(d *sqliteDurability) {
		if k < 1 {
			k = 1
		}
		d.strict = false
		d.batchK = k
	}
}

// WithSQLiteDurability applies a durability option to NewSQLiteStore. The SQLite analogue
// of the FB WithDurabilityMode (which is FB-typed and cannot be reused).
func WithSQLiteDurability(opt SQLiteOption) SQLiteOption { return opt }

// pragmas returns the PRAGMAs that realize the mode. WAL is used in BOTH modes (ph66/ph67
// already use WAL); the mode differs in `synchronous`. fullfsync=1 in BOTH modes: it makes
// EVERY fsync on the connection a real F_FULLFSYNC drive flush on darwin, so both Strict's
// per-commit fsync AND Batched's every-Kth wal_checkpoint(TRUNCATE) boundary are actually
// power-loss-durable (ph68-F1: fullfsync=0 in Batched left the checkpoint boundary + the
// Sync floor OS-cache-only on darwin — the ph65 trap re-walked). synchronous alone gives
// Batched its amortization (skip the per-commit fsync); fullfsync governs WHETHER the fsyncs
// that DO happen flush the drive.
func (d sqliteDurability) pragmas() []string {
	sync := "PRAGMA synchronous=NORMAL" // Batched: fsync amortized to the checkpoint boundary
	if d.strict {
		sync = "PRAGMA synchronous=FULL" // Strict: fsync every commit
	}
	return []string{
		"PRAGMA journal_mode=WAL",
		sync,
		"PRAGMA fullfsync=1", // darwin: real F_FULLFSYNC on every fsync; linux: no-op (see header)
	}
}

// walCheckpointFullLocked forces a durable WAL checkpoint (TRUNCATE = the strongest
// wal_checkpoint variant: checkpoints all frames AND truncates the WAL, blocking until the
// data is fsync'd into the main DB). This is the Batched(K) durability boundary and the
// Sync() floor. Caller holds s.mu.
func (s *SQLiteStore) walCheckpointFullLocked(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("%w: wal_checkpoint: %w", ErrIO, err)
	}
	return nil
}

// Sync implements Syncer — the suspend/completion durability FLOOR. It forces the
// workflow's committed-but-un-checkpointed WAL frames durable via wal_checkpoint(TRUNCATE),
// so a parked/completed run is power-loss-durable regardless of the Batched(K) cadence. In
// Strict every commit is already fullfsync-durable, so this is a cheap (idempotent) no-op
// on an empty WAL. workflowID is validated for API symmetry; the checkpoint is DB-wide
// (SQLite's WAL is per-DB, not per-workflow) — flushing it makes THIS workflow durable too.
func (s *SQLiteStore) Sync(workflowID string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.walCheckpointFullLocked(ctx)
}
