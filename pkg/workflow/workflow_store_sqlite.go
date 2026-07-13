package workflow

// SQLiteStore — a decomposed, row-based WorkflowStore backed by pure-Go
// modernc.org/sqlite (M15 ph66). Unlike the FB/JSON stores (whole-workflow blob per
// file), the durable unit here is a ROW: one row per workflow + one row per node +
// typed data/waits rows. This structurally removes the M14 deep-durable O(N²)
// re-serialization tail (per-node incremental UPSERT lands in ph67) and unlocks
// indexed visibility (ph69). ph66 scope: the FULL decomposed Save (all rows) + Load
// (reassemble a byte-identical WorkflowData) implementing the frozen base interface.
//
// FIDELITY CONTRACT: SQLiteStore.Load reconstructs a WorkflowData whose Snapshot() is
// byte-identical to the same WorkflowData through the FB/JSON path. To achieve that it
// mirrors the FB store's type-collapse EXACTLY: int/int32/int64 → int64; float32/
// float64 → float64; bool → bool; string + complex → the JSON/string form. int64 rides
// an INTEGER-affinity column (the spike-proven byte-safe path; never a float scan
// destination — the type-affinity landmine).
//
// Durability: ph66 uses a safe default (synchronous=FULL + WAL). The Strict/Batched
// mode knob + Syncer floor are ph68. Single-writer connection + busy_timeout (SQLite
// is single-writer even in WAL). NOT multi-process-safe (the Locker is in-process).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)
)

// sqliteSchema is the decomposed schema. workflows: one row per run (the run-level
// scalars). nodes: one row per node (status + output). data_kv: typed data entries
// (kind discriminates the Go type for faithful reconstruction). waits: durable timer
// fireAt per parked node (M10). All FK-scoped by workflow_id.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS workflows (
    id            TEXT PRIMARY KEY,
    rolling_back  INTEGER NOT NULL DEFAULT 0,
    trigger_cause INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS nodes (
    workflow_id TEXT NOT NULL,
    node_name   TEXT NOT NULL,
    status      TEXT NOT NULL,     -- NodeStatus string ("pending"/…); '' = output-only node (no status entry), NOT persisted as a status
    output      TEXT,
    has_output  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workflow_id, node_name)
);
CREATE TABLE IF NOT EXISTS data_kv (
    workflow_id TEXT NOT NULL,
    key         TEXT NOT NULL,
    kind        INTEGER NOT NULL,  -- kvInt | kvBool | kvFloat | kvString
    i_val       INTEGER,           -- INTEGER-affinity: int64 byte-safe
    f_val       REAL,
    s_val       TEXT,
    PRIMARY KEY (workflow_id, key)
);
CREATE TABLE IF NOT EXISTS waits (
    workflow_id TEXT NOT NULL,
    node_name   TEXT NOT NULL,
    fire_at     INTEGER NOT NULL,  -- absolute unix-nanos int64, INTEGER-affinity
    PRIMARY KEY (workflow_id, node_name)
);
-- ph70 visibility indexes (WorkflowQuery, SQL-05). Additive (no data change). Only the two
-- indexes the interface's queries ACTUALLY use are created (EXPLAIN-verified — ph70-F1):
--   idx_nodes_status      → ListByNodeStatus (WHERE status=?, covering for DISTINCT workflow_id)
--   idx_workflows_rolling_back → ListRollingBack (WHERE rolling_back=1)
-- The since/updated_at filter rides these + the workflows PK (a residual on the fetched row),
-- so NO updated_at index is created — it would be pure write-amplification on the durable path
-- for zero read benefit (YAGNI; re-add if a bare WHERE-updated_at>=? query ever lands).
-- CREATE INDEX IF NOT EXISTS is idempotent, so an existing (ph66-69) DB gains these on the next
-- open — a ONE-TIME synchronous build proportional to the existing nodes rows (O(N log N) under
-- a write lock; safe under the single-process modernc driver), then free on every later open.
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status, workflow_id);
CREATE INDEX IF NOT EXISTS idx_workflows_rolling_back ON workflows(rolling_back);`

// data_kv.kind discriminators — mirror the FB store's typed vectors so Load
// reconstructs the SAME Go type the FB path yields (byte-identical Snapshot).
const (
	kvInt    = 0 // int/int32/int64 → int64
	kvBool   = 1 // bool
	kvFloat  = 2 // float32/float64 → float64
	kvString = 3 // string + complex(JSON) → string
)

// SQLiteStore is a decomposed row-based WorkflowStore (M15). It holds a single-writer
// *sql.DB. Construct with NewSQLiteStore.
//
// shadow (ph67) carries, per workflow, the last-COMMITTED encoded row-set so that
// SaveCheckpoint can diff the incoming snapshot and UPSERT/DELETE only what changed —
// O(Δ) per level, not O(nodes) (the M14 O(N²) fix). It is a durability-neutral CACHE:
// it is rebuilt from the durable journal on Load/reopen, and updated only AFTER a
// successful COMMIT, so a crash mid-checkpoint leaves both the DB and the shadow at the
// pre-crash committed frontier. Guarded by mu (the *sql.DB already serializes writes via
// SetMaxOpenConns(1); mu additionally guards the shadow map against a concurrent Load).
type SQLiteStore struct {
	db     *sql.DB
	path   string
	mu     sync.Mutex
	shadow map[string]*shadowState
	// ph68 durability: dur is the resolved mode; ckptCount tracks per-workflow checkpoint
	// count so Batched(K) forces a durable wal_checkpoint(TRUNCATE) every Kth SaveCheckpoint
	// (bounding a power-loss to ≤K un-checkpointed levels). Both guarded by mu.
	dur       sqliteDurability
	ckptCount map[string]uint
}

// NewSQLiteStore opens (creating if absent) a SQLite DB at path and ensures the
// decomposed schema. Single-writer connection (SetMaxOpenConns(1)) + busy_timeout.
// Durability defaults to Strict (synchronous=FULL + fullfsync=1, power-loss-durable per
// checkpoint); pass WithSQLiteDurability(SQLiteBatched(k)) to opt into group-commit (ph68).
func NewSQLiteStore(path string, opts ...SQLiteOption) (*SQLiteStore, error) {
	ctx := context.Background()
	dur := defaultDurability()
	for _, opt := range opts {
		opt(&dur)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("%w: open sqlite: %w", ErrIO, err)
	}
	// SQLite is single-writer even in WAL; one connection serializes writes and
	// avoids "database is locked" under the in-process lease.
	db.SetMaxOpenConns(1)
	// busy_timeout first, then the durability-mode pragmas (journal_mode/synchronous/
	// fullfsync). No FOREIGN KEY is declared — child-row cleanup is manual-by-design in
	// Save's clean-overwrite loop + Delete — so foreign_keys is intentionally NOT set.
	pragmas := append([]string{"PRAGMA busy_timeout=5000"}, dur.pragmas()...)
	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
			return nil, fmt.Errorf("%w: pragma %q: %w", ErrIO, pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
		return nil, fmt.Errorf("%w: schema: %w", ErrIO, err)
	}
	return &SQLiteStore{
		db:        db,
		path:      path,
		shadow:    make(map[string]*shadowState),
		dur:       dur,
		ckptCount: make(map[string]uint),
	}, nil
}

// Close releases the underlying DB. Not part of WorkflowStore; call at shutdown.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Save writes the FULL decomposed state of data in ONE transaction (crash-atomic
// level boundary — the spike-proven COMMIT atomicity). It replaces any prior state for
// this workflow (delete-then-insert within the txn) so a re-Save is a clean overwrite.
//
// ph67-F1: Save holds s.mu across its ENTIRE transaction (matching saveIncremental/Load),
// so the shadow-vs-DB frontier can never be read-then-mutated out from under a concurrent
// SaveCheckpoint. Uniform lock order (mu → DB conn) across every mutating path rules out
// both the coherence TOCTOU and any lock-inversion deadlock. The single DB conn already
// serializes the SQL, so mu adds ~zero contention.
func (s *SQLiteStore) Save(data *WorkflowData) error {
	ctx := context.Background()
	if data == nil {
		return fmt.Errorf("%w: cannot save nil workflow data", ErrValidation)
	}
	id := data.GetWorkflowID()
	if err := validateWorkflowID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin: %w", ErrIO, err)
	}
	// Roll back on any error path; a successful Commit makes this a no-op.
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	// Clean overwrite: drop the workflow's prior rows, then rewrite. The table + id
	// column names are HARDCODED literals (this loop + idCol), never user input — only
	// the `?`-bound workflowID carries external data — so this concatenation is not an
	// injection surface.
	for _, tbl := range []string{"workflows", "nodes", "data_kv", "waits"} {
		//nolint:gosec // G202: table/id-col are hardcoded literals, not user input; the only user value is the ?-bound id
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE "+idCol(tbl)+" = ?", id); err != nil {
			return fmt.Errorf("%w: clear %s: %w", ErrIO, tbl, err)
		}
	}

	// workflows row (run-level scalars).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflows (id, rolling_back, trigger_cause, updated_at) VALUES (?,?,?,?)`,
		id, boolToInt(data.IsRollingBack()), int64(data.TriggerCause()), unixNanoNow(),
	); err != nil {
		return fmt.Errorf("%w: insert workflow: %w", ErrIO, err)
	}

	// node rows (status + output). Output is encoded the SAME way the FB store does
	// (a string passes through; anything else is JSON-marshalled) so the reloaded
	// output is byte-identical.
	var saveErr error
	data.ForEachNodeStatus(func(node string, st NodeStatus) {
		if saveErr != nil {
			return
		}
		out, has := data.GetOutput(node)
		enc := ""
		if has {
			enc = encodeOutput(out)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)`,
			id, node, string(st), enc, boolToInt(has),
		); err != nil {
			saveErr = fmt.Errorf("%w: insert node %q: %w", ErrIO, node, err)
		}
	})
	if saveErr != nil {
		return saveErr
	}

	// An output MAY exist for a node that has no explicit status entry. The FB store
	// keeps outputs and nodeStatus as INDEPENDENT maps — an output-only node has NO
	// status. Persist such a node with an EMPTY status sentinel ('') so Load restores
	// only the output, never inventing a Pending status (which would redden fidelity vs
	// the FB round-trip). Nodes already written above carry a real status and are left
	// untouched by ON CONFLICT (their status is NOT downgraded).
	data.ForEachOutput(func(node string, out interface{}) {
		if saveErr != nil {
			return
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,'',?,1)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET output=excluded.output, has_output=1`,
			id, node, encodeOutput(out),
		); err != nil {
			saveErr = fmt.Errorf("%w: insert output %q: %w", ErrIO, node, err)
		}
	})
	if saveErr != nil {
		return saveErr
	}

	// typed data_kv rows — mirror the FB type dispatch so Load reconstructs the SAME
	// Go type (byte-identical Snapshot).
	data.ForEach(func(k string, value interface{}) {
		if saveErr != nil {
			return
		}
		kind, iv, fv, sv := encodeKV(value)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO data_kv (workflow_id, key, kind, i_val, f_val, s_val) VALUES (?,?,?,?,?,?)`,
			id, k, kind, iv, fv, sv,
		); err != nil {
			saveErr = fmt.Errorf("%w: insert data %q: %w", ErrIO, k, err)
		}
	})
	if saveErr != nil {
		return saveErr
	}

	// waits rows (M10 durable timer fireAt).
	data.ForEachWait(func(node string, fireAt int64) {
		if saveErr != nil {
			return
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO waits (workflow_id, node_name, fire_at) VALUES (?,?,?)`,
			id, node, fireAt,
		); err != nil {
			saveErr = fmt.Errorf("%w: insert wait %q: %w", ErrIO, node, err)
		}
	})
	if saveErr != nil {
		return saveErr
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit: %w", ErrIO, err)
	}
	committed = true
	// A full Save is a clean overwrite → the shadow now equals the full written state,
	// so a subsequent SaveCheckpoint diffs against it correctly. Advanced AFTER commit
	// (crash-safety ordering); s.mu is held for the whole method (see ph67-F1 note).
	s.shadow[id] = shadowFromData(data)
	return nil
}

// SaveCheckpoint implements Checkpointer (ph67 — the incremental per-node write). It
// diffs data against the store's in-memory shadow of the last-committed row-set and, in
// ONE transaction (one crash-atomic level boundary), UPSERTs only the changed/new rows,
// DELETEs rows that vanished, and updates the workflow-row scalars if they changed —
// O(Δ) per level, not O(nodes). The shadow is updated only AFTER a successful COMMIT, so
// a crash mid-checkpoint leaves the DB AND the shadow at the pre-crash committed frontier.
//
// Byte-identity to a full Save (the ph66 hold) is preserved: the UNION of all committed
// UPSERT/DELETEs reconstructs exactly the row-set Save would write, and Load reads rows
// order-independently. A workflow with no shadow yet (first checkpoint, or the first
// after a reopen before any Load) writes every row (the diff baseline is empty) —
// degenerating to a correct full write.
func (s *SQLiteStore) SaveCheckpoint(data *WorkflowData) error {
	return s.saveIncremental(data)
}

// Load reassembles the WorkflowData from its decomposed rows. A missing workflow →
// ErrNotFound. A corrupt/garbage DB → a clean typed ErrCorruptData/ErrIO, never a panic.
//
// p67-AF2: Load holds s.mu across its DB reads AND the shadow rebuild, so it is atomic
// w.r.t. a concurrent SaveCheckpoint — the rebuilt shadow always reflects exactly the DB
// state Load just read, never a frontier a checkpoint advanced past in between (which
// could otherwise clobber the shadow with a staler baseline and skip a later diff write).
func (s *SQLiteStore) Load(workflowID string) (*WorkflowData, error) {
	ctx := context.Background()
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Existence: the workflows row is the anchor (a workflow with no data/nodes still
	// has its row). Absent → ErrNotFound.
	var rollingBack, triggerCause int64
	err := s.db.QueryRowContext(ctx,
		`SELECT rolling_back, trigger_cause FROM workflows WHERE id = ?`, workflowID,
	).Scan(&rollingBack, &triggerCause)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load workflow: %w", ErrCorruptData, err)
	}

	data := NewWorkflowData(workflowID)

	// data_kv → typed values (mirror FB reconstruction).
	rows, err := s.db.QueryContext(ctx, `SELECT key, kind, i_val, f_val, s_val FROM data_kv WHERE workflow_id = ?`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("%w: query data: %w", ErrCorruptData, err)
	}
	if err := scanDataKV(rows, data); err != nil {
		return nil, err
	}

	// nodes → status (+ output if has_output).
	nrows, err := s.db.QueryContext(ctx, `SELECT node_name, status, output, has_output FROM nodes WHERE workflow_id = ?`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("%w: query nodes: %w", ErrCorruptData, err)
	}
	if err := scanNodes(nrows, data); err != nil {
		return nil, err
	}

	// waits → fireAt.
	wrows, err := s.db.QueryContext(ctx, `SELECT node_name, fire_at FROM waits WHERE workflow_id = ?`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("%w: query waits: %w", ErrCorruptData, err)
	}
	if err := scanWaits(wrows, data); err != nil {
		return nil, err
	}

	// run-level scalars.
	data.SetRollingBack(rollingBack != 0)
	if triggerCause >= 0 && triggerCause <= int64(TriggerDeadlineExceeded) {
		data.SetTriggerCause(TriggerCause(triggerCause))
	}

	// ph67 resume-into: rebuild the incremental diff baseline from the just-loaded
	// (durable, committed) state, so a crash → reopen → Load → continue run's first
	// post-resume SaveCheckpoint diffs against the real committed frontier — not an empty
	// baseline that would needlessly re-write every row (correct, but O(N)) NOR a stale
	// one that would miss/over-write a row. The shadow mirrors what Load reconstructed.
	// (s.mu is already held for the whole Load — see the AF2 note above; no re-lock.)
	s.shadow[workflowID] = shadowFromData(data)

	return data, nil
}

// ListWorkflows returns all workflow IDs (the workflows-table PKs).
func (s *SQLiteStore) ListWorkflows() ([]string, error) {
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM workflows ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("%w: list: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: scan id: %w", ErrCorruptData, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows: %w", ErrCorruptData, err)
	}
	return ids, nil
}

// Delete removes a workflow and all its decomposed rows in one transaction.
// A missing workflow → ErrNotFound.
func (s *SQLiteStore) Delete(workflowID string) error {
	ctx := context.Background()
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	s.mu.Lock() // ph67-F1: hold mu across the whole txn (uniform mu → conn order).
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin: %w", ErrIO, err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort
		}
	}()

	res, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, workflowID)
	if err != nil {
		return fmt.Errorf("%w: delete workflow: %w", ErrIO, err)
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("%w: rows affected: %w", ErrIO, raErr)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, workflowID)
	}
	for _, tbl := range []string{"nodes", "data_kv", "waits"} {
		//nolint:gosec // G202: table names are hardcoded literals, not user input; only the ?-bound id is external
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE workflow_id = ?", workflowID); err != nil {
			return fmt.Errorf("%w: delete %s: %w", ErrIO, tbl, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit: %w", ErrIO, err)
	}
	committed = true
	// The workflow is gone → drop its incremental diff baseline AND its checkpoint-cadence
	// counter (ph68-F3: leaving ckptCount grows the map unboundedly over deleted ids and
	// carries a stale cadence into a reused id). Both pruned; s.mu held for the whole method.
	delete(s.shadow, workflowID)
	delete(s.ckptCount, workflowID)
	return nil
}
