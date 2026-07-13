package workflow

// ph124 (v0.14.0-alpha release fix-tail) — targeted coverage for the M15 SQLite
// surface the ph66–71 gates left thin: the two trivial durability-option constructors
// (SQLiteStrict / WithSQLiteDurability — a one-liner at 0% is inexcusable, no baseline
// covers that) plus the cheap, no-fault-injection error/return branches on Delete,
// ListWorkflows, and Load-not-found. The DEEP error paths (fsync/DB-error/corrupt-scan
// fault injection) are tracked to M16's fault-injection harness (#126); this file grabs
// only what a plain test can hit directly.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// mkSQLiteCovStore opens a fresh SQLiteStore under t.TempDir with the given options and
// registers Close.
func mkSQLiteCovStore(t *testing.T, opts ...SQLiteOption) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "wf.db"), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
	return s
}

// TestSQLiteDurability_OptionConstructors — the two 0%-covered option constructors are
// exercised via their OBSERVABLE effect (the applied PRAGMAs), not just called: SQLiteStrict
// → synchronous=FULL; WithSQLiteDurability(SQLiteBatched(k)) is a passthrough that still lands
// synchronous=NORMAL. This pins that the default IS Strict and the explicit opts resolve.
func TestSQLiteDurability_OptionConstructors(t *testing.T) {
	// SQLiteStrict() → strict mode → synchronous=FULL in the realized pragmas.
	strict := mkSQLiteCovStore(t, SQLiteStrict())
	require.True(t, strict.dur.strict, "SQLiteStrict must set strict")
	require.Equal(t, uint(1), strict.dur.batchK, "SQLiteStrict must set batchK=1")
	require.Contains(t, strict.dur.pragmas(), "PRAGMA synchronous=FULL")

	// The bare default (no opts) is Strict too — the documented default.
	def := mkSQLiteCovStore(t)
	require.True(t, def.dur.strict, "default durability must be Strict")

	// WithSQLiteDurability(SQLiteBatched(4)) — the passthrough wrapper applies its inner
	// option; Batched → NOT strict, batchK=4, synchronous=NORMAL.
	batched := mkSQLiteCovStore(t, WithSQLiteDurability(SQLiteBatched(4)))
	require.False(t, batched.dur.strict, "Batched must clear strict")
	require.Equal(t, uint(4), batched.dur.batchK, "Batched(4) must set batchK=4")
	require.Contains(t, batched.dur.pragmas(), "PRAGMA synchronous=NORMAL")

	// SQLiteBatched(0) clamps to 1 (k must be ≥1).
	clamped := mkSQLiteCovStore(t, SQLiteBatched(0))
	require.Equal(t, uint(1), clamped.dur.batchK, "Batched(0) must clamp to 1")
}

// TestSQLiteDelete_Branches — the cheap Delete return branches: missing workflow → ErrNotFound,
// invalid id → ErrValidation, and the happy path (present workflow removed, then a second
// Delete is ErrNotFound).
func TestSQLiteDelete_Branches(t *testing.T) {
	s := mkSQLiteCovStore(t)

	// Missing workflow → ErrNotFound.
	err := s.Delete("nope")
	require.ErrorIs(t, err, ErrNotFound)

	// Invalid id (empty) → ErrValidation (validateWorkflowID rejects before the txn).
	require.ErrorIs(t, s.Delete(""), ErrValidation)

	// Happy path: save then delete removes it; a re-Delete is ErrNotFound.
	d := NewWorkflowData("del-me")
	d.SetNodeStatus("n1", Completed)
	require.NoError(t, s.Save(d))
	require.NoError(t, s.Delete("del-me"))
	require.ErrorIs(t, s.Delete("del-me"), ErrNotFound, "second delete of a gone workflow is ErrNotFound")

	// And it is really gone.
	_, err = s.Load("del-me")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSQLiteListWorkflows — the happy-path list return (empty, then populated + sorted).
func TestSQLiteListWorkflows(t *testing.T) {
	s := mkSQLiteCovStore(t)

	ids, err := s.ListWorkflows()
	require.NoError(t, err)
	require.Empty(t, ids, "a fresh store lists no workflows")

	for _, id := range []string{"b", "a", "c"} {
		require.NoError(t, s.Save(NewWorkflowData(id)))
	}
	ids, err = s.ListWorkflows()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, ids, "ListWorkflows returns all ids ORDER BY id")
}

// TestSQLiteLoad_NotFound — Load of an absent workflow → ErrNotFound (the sql.ErrNoRows branch).
func TestSQLiteLoad_NotFound(t *testing.T) {
	s := mkSQLiteCovStore(t)
	_, err := s.Load("ghost")
	require.True(t, errors.Is(err, ErrNotFound), "Load of an absent workflow is ErrNotFound, got %v", err)
}
