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
	"strings"
	"sync"
	"time"

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
CREATE INDEX IF NOT EXISTS idx_workflows_rolling_back ON workflows(rolling_back);
-- M16 (ph74) durable lease row for cross-process competing consumers (DEC-M16-D2). One row per
-- CLAIMED workflow: owner_id = the claiming process's opaque identity; expiry = wall-clock lease
-- deadline (a LIVENESS heuristic — decides WHEN re-claim is offered, NEVER safety, DEC-M16-D3);
-- fencing_token = the monotonic, DB-issued SAFETY arbiter (decides WHOSE write wins). Additive
-- (new table, no migration); empty + unused in single-process mode. The fencing CAS re-reads
-- fencing_token inside the checkpoint IMMEDIATE txn and rejects a stale-token write.
CREATE TABLE IF NOT EXISTS leases (
    workflow_id   TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL,
    expiry        INTEGER NOT NULL,  -- unix-nanos lease deadline; liveness heuristic, NOT safety
    fencing_token INTEGER NOT NULL   -- monotonic DB-issued token; the SOLE safety arbiter (DEC-M16-D3)
);
-- M17 (ph80) durable WORK QUEUE for Tier-1 dispatch (DF-1). One row per submitted workflow; a pool
-- worker ClaimNext()s the oldest pending row of a matching type, rides the M16 fencing CAS on the
-- leases table (orthogonal — this table never touches the fencing arbiter), and drives it. Additive/
-- opt-in; empty + unused unless Enqueue/ClaimNext are called. The state lifecycle is pending ->
-- claimed -> (done | failed | cancelled); terminal rows are NEVER re-claimed (the C2 reclaim guard).
CREATE TABLE IF NOT EXISTS work_queue (
    workflow_id     TEXT PRIMARY KEY,       -- shares the workflows/leases id space (one queue entry per run)
    type            TEXT NOT NULL,          -- caller's dispatch type (the ClaimNext typeFilter key)
    input           BLOB,                   -- opaque caller payload, nullable
    enqueued_at     INTEGER NOT NULL,       -- unix-nanos submit time; the FIFO ordering key
    state           TEXT NOT NULL,          -- pending | claimed | done | failed | cancelled
    attempts        INTEGER NOT NULL DEFAULT 0, -- bumped on each ClaimNext claim
    updated_at      INTEGER NOT NULL,       -- unix-nanos of the last state transition
    cancel_requested INTEGER,               -- M18 ph87: unix-nanos of an operator cancel request; NULL = not requested.
                                            -- The durable INTENT that splits operator-cancel (terminalize cancelled)
                                            -- from AF1 drain/crash (leave claimed). Additive/nullable — no FB change.
    parent_id       TEXT,                   -- M19 ph94: the PARENT workflow's mailbox address for a sub-workflow child
    parent_signal   TEXT,                   -- (parent_id = parent WorkflowID, parent_signal = completion-signal name).
                                            -- NULL = a plain M17 dispatch (no parent) → RunNext emits no completion signal.
                                            -- CONTROL-plane metadata (alongside cancel_requested/state) — the child's input
                                            -- BLOB structurally cannot reach it, so attacker input can never forge the
                                            -- completion target (DEC-P94-PARENT-ADDRESS-COLUMN, defense-by-construction).
                                            -- Additive/nullable, engine-set at EnqueueSubWorkflow. Orthogonal to the M16
                                            -- fencing arbiter (leases table) — never touches the fencing CAS.
    depth           INTEGER NOT NULL DEFAULT 0 -- M19 ph95: sub-workflow NESTING depth (the COMP-CLOSE DoS ceiling).
                                            -- ENGINE-SET at EnqueueSubWorkflow (parent_depth+1), NEVER user-supplied —
                                            -- same defense-by-construction as parent_id: a user cannot forge a low depth
                                            -- to bypass the ceiling. A plain M17 Enqueue is depth 0 (the DEFAULT). RunNext
                                            -- projects it back into the child drive-stack so a type-ref chain is bounded
                                            -- across the dispatch (F-P94-04). NOT NULL DEFAULT 0 → a legit row is never NULL;
                                            -- the read path treats a NULL as corruption (fail-safe refuse, DEC-P95-DEPTH-DEFENSIVE-READ).
);
-- PARTIAL index over ONLY claimable rows: the ClaimNext scan (WHERE state='pending' [AND type IN …]
-- ORDER BY enqueued_at) is fully index-covered, and terminal rows never bloat it.
CREATE INDEX IF NOT EXISTS idx_wq_claimable ON work_queue(type, enqueued_at) WHERE state='pending';
-- M19 (ph93) durable SIGNAL MAILBOX for the SignalStore interface (M10 signals: approvals,
-- wait-for-signal, ph92 sub-workflow completion). One row per un-acked signal per workflow.
-- SEPARATE from the WorkflowData snapshot tables (nodes/data_kv) — mailbox-outside-snapshot
-- (MH37-1): a Save/checkpoint rewrites the snapshot rows, never this table, so a delivered
-- signal can NEVER be clobbered by a concurrent checkpoint. NO FK to workflows: a signal may
-- be delivered before the instance exists (early-signal buffering, topology-independent
-- delivery). PRIMARY KEY (workflow_id, sig_id) gives BOTH the idempotent-by-id UNIQUE and the
-- ON CONFLICT target (a re-delivered sig_id is a no-op). Additive CREATE TABLE IF NOT EXISTS
-- (ph80/ph87 pattern) — an existing DB gains it on next open.
CREATE TABLE IF NOT EXISTS signals (
    workflow_id TEXT NOT NULL,
    sig_id      TEXT NOT NULL,
    name        TEXT NOT NULL,
    payload     TEXT,                  -- JSON-encoded (marshalSignalPayload); '' = nil payload. DeliverSignal
                                        -- always writes a non-NULL string, but the READ path (TakeSignals)
                                        -- scans via sql.NullString so a NULL from a corrupt DB / foreign
                                        -- writer decodes to nil, never bricking the read (F-P93-ADV-1). No
                                        -- NOT NULL constraint: it would give false confidence (constraints
                                        -- don't protect a corrupt DB — the read-path robustness is the defense).
    enqueued_at INTEGER NOT NULL,      -- unix-nanos delivery time (a tiebreak; sig_id is the primary sort)
    PRIMARY KEY (workflow_id, sig_id)  -- idempotent-by-id UNIQUE + the ON CONFLICT target; NO FK to workflows
);`

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
	// M16 (ph74): tokenState[workflowID] = the fencing token this PROCESS's claim holds for a
	// workflow (0 = no claim). Set by Claim, cleared by Release; read inside the checkpoint
	// IMMEDIATE txn to CAS against leases.fencing_token. Race-free WITHOUT a per-token lock
	// because the executor's Locker (workflow.go:215) already serializes all in-process drives
	// of one WorkflowID across the whole load→run→checkpoint span — so there is never an
	// in-process concurrent writer on the same workflow's token (the M15 shadow TOCTOU class,
	// ph67, does not apply). Still guarded by mu (uniform with shadow/ckptCount). Empty +
	// unused when mp=false (the CAS is mp-gated), so the single-process path is untouched.
	tokenState map[string]FencingToken
	// M16 (ph75): clock + leaseTTL govern the LEASE liveness only (Claim/Renew expiry +
	// renew-on-checkpoint). Default clock = SystemClock; tests inject a FakeClock to make
	// lease-lapse deterministic. DEC-M16-D3: expiry is a LIVENESS heuristic read through the
	// clock; the fencing TOKEN is the sole SAFETY arbiter and NEVER reads the clock, so a clock
	// skew/fake can never cause a safety violation. The store's updated_at writes stay on
	// unixNanoNow() (hot/fidelity path — det-tax + FB-fidelity untouched).
	clock    Clock
	leaseTTL time.Duration
	// M18 (ph86): the optional dispatch-metrics hook (OBS-MET). nil = zero-cost — every increment site is
	// a single `if s.metrics != nil` nil check; no counter/alloc/bridge when unset. Opt-in via
	// WithDispatchMetrics. Distinct from the M14 per-workflow metrics (Workflow.MetricsConfig), untouched.
	metrics *DispatchMetrics
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
	//nolint:gosec // G703: `path` is the caller's own store-construction DB path (trusted
	// construction input, not request-derived); the interprocedural taint tracker anchors here
	// via openSQLiteDB(path,...), but no external data reaches the filesystem/DSN.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}
	// M16 C1: multi-process mode opens with _txlock=immediate so every BeginTx acquires the
	// write lock UP FRONT. M15's default DEFERRED txns let two OS-process writers both BEGIN
	// then deadlock on lock upgrade (proven in the ph73 spike); IMMEDIATE serializes them.
	// The single-process path keeps the bare `path` DSN byte-for-byte unchanged (opt-in) — the
	// mp branch appends only a HARDCODED DSN suffix, no external data.
	db, err := openSQLiteDB(path, dur.mp, dur.driverName)
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
	// M18 ph87: additive migration for the cancel_requested column. CREATE TABLE IF NOT EXISTS above
	// gives NEW DBs the column, but an existing v0.16 work_queue predates it — ADD it idempotently. SQLite
	// has no ADD COLUMN IF NOT EXISTS, so we run the ALTER and TOLERATE the "duplicate column name" error
	// (the column already exists = the desired state). Any OTHER error is a real schema fault, surfaced.
	if _, err := db.ExecContext(ctx, `ALTER TABLE work_queue ADD COLUMN cancel_requested INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
		return nil, fmt.Errorf("%w: cancel_requested migration: %w", ErrIO, err)
	}
	// M19 ph94: additive parent-address columns for sub-workflow queue children (same ADD COLUMN
	// pattern — an existing DB gains them on next open; "duplicate column name" = already migrated).
	for _, col := range []string{"parent_id TEXT", "parent_signal TEXT"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE work_queue ADD COLUMN "+col); err != nil && //nolint:gosec // G202: col is a hardcoded literal, not user input
			!strings.Contains(err.Error(), "duplicate column name") {
			db.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
			return nil, fmt.Errorf("%w: %s migration: %w", ErrIO, col, err)
		}
	}
	// M19 ph95: additive sub-workflow nesting-depth column (same idempotent ADD COLUMN pattern). NOT NULL
	// DEFAULT 0 is a valid SQLite ADD COLUMN (a constant default backfills existing rows to 0 — a pre-ph95
	// row is a top-level dispatch = depth 0, correct). Engine-set at EnqueueSubWorkflow; NEVER user input.
	if _, err := db.ExecContext(ctx, `ALTER TABLE work_queue ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
		return nil, fmt.Errorf("%w: depth migration: %w", ErrIO, err)
	}
	// M16 (ph75): resolve the lease-liveness clock + TTL (defaults when unset). Lease liveness only.
	clock := dur.clock
	if clock == nil {
		clock = SystemClock()
	}
	leaseTTL := dur.leaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}
	return &SQLiteStore{
		db:         db,
		path:       path,
		shadow:     make(map[string]*shadowState),
		dur:        dur,
		ckptCount:  make(map[string]uint),
		tokenState: make(map[string]FencingToken),
		clock:      clock,
		leaseTTL:   leaseTTL,
		metrics:    dur.dispatchMetrics,
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
		// BEGIN IMMEDIATE (mp mode) acquires the write lock HERE — a contended writer that waits
		// past busy_timeout surfaces SQLITE_BUSY at this boundary. Classify it as ErrBusy (transient,
		// retryable) so a competing consumer can distinguish it from ErrFencedOut. Clean abort: no
		// txn was opened, so nothing was written. (MAJOR-5)
		return classifyTxErr("begin", err)
	}
	// Roll back on any error path; a successful Commit makes this a no-op.
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	// M16 (ph74): the FENCING CAS rides this IMMEDIATE txn (no-op single-process / non-claimed).
	if err := s.checkFencingLocked(ctx, tx, id); err != nil {
		return err
	}

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
		return classifyTxErr("commit", err) // SQLITE_BUSY at commit → ErrBusy (transient); else ErrIO.
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

	// Reclaim the durable mailbox FIRST + UNCONDITIONALLY — the SQLite analog of the file stores'
	// removeSignalDir, which reclaims the mailbox even for a never-Saved workflow (early-buffered
	// signals with NO workflows row; ph37 F2 — no background GC). Doing it before the workflows-row
	// check + committing it below (even on the ErrNotFound path) means an early-buffered mailbox is
	// never orphaned. (F-P93-R1.)
	if _, err := tx.ExecContext(ctx, `DELETE FROM signals WHERE workflow_id = ?`, workflowID); err != nil {
		return fmt.Errorf("%w: delete signals: %w", ErrIO, err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, workflowID)
	if err != nil {
		return fmt.Errorf("%w: delete workflow: %w", ErrIO, err)
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("%w: rows affected: %w", ErrIO, raErr)
	}
	if n == 0 {
		// The snapshot did not exist — but a mailbox MAY have (early buffering). Commit the signals
		// reclaim so it is not leaked, THEN report ErrNotFound (the snapshot-absence contract is
		// unchanged for callers).
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("%w: commit: %w", ErrIO, cerr)
		}
		committed = true
		return fmt.Errorf("%w: %s", ErrNotFound, workflowID)
	}
	// The snapshot existed — reclaim its remaining rows in the same txn.
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
