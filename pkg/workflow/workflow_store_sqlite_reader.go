package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// SQLiteReader observes an existing workflow database without exposing a writer.
// Construct it with OpenSQLiteReader. Each Load reconstructs one committed snapshot;
// separate method calls need not observe the same instant. WorkflowStatus retains
// the granular store method's documented two-query consistency semantics.
//
// SQLite may create, update, remove, or leave its own -wal/-shm coordination files
// beside the database, including on failed opens and Close. The database itself,
// its schema, and application records are never created, migrated, or written.
// This is not an immutable snapshot: subsequent calls see subsequent commits.
// Use a trusted local database path on a filesystem supported by SQLite WAL.
// Concurrent path replacement or deletion by the caller is not supported.
// Close the reader when finished; it owns its database connection.
//
// Schema compatibility checks cover the tables and columns used by these reads;
// they are not a whole-database integrity check. Corrupt row values are reported
// by the existing engine decoders when those rows are read.
type SQLiteReader struct {
	store *SQLiteStore
}

// OpenSQLiteReader opens a regular, nonempty existing database read-only. It
// refuses absent paths, empty/non-SQLite files, incompatible reader schemas, and
// database recovery that requires a write. It does not initialize or repair them.
// SQLite-only WAL/SHM coordination is permitted as described on SQLiteReader.
func OpenSQLiteReader(path string) (*SQLiteReader, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: stat reader database: %w", ErrIO, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("%w: reader requires a nonempty regular database", ErrCorruptData)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reader database path: %w", ErrIO, err)
	}
	uriPath := filepath.ToSlash(abs)
	if filepath.VolumeName(abs) != "" {
		uriPath = "/" + uriPath
	}
	params := url.Values{"mode": {"ro"}, "_txlock": {"deferred"}, "_pragma": {
		"query_only(ON)", "temp_store(MEMORY)", "busy_timeout(5000)",
	}}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: open reader database: %w", ErrIO, err)
	}
	db.SetMaxOpenConns(1)
	if err := validateSQLiteReaderSchema(db); err != nil {
		db.Close() //nolint:errcheck // preserve the schema/open failure
		return nil, err
	}
	return &SQLiteReader{store: &SQLiteStore{db: db, dur: sqliteDurability{mp: true}}}, nil
}

func validateSQLiteReaderSchema(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("%w: begin reader schema check: %w", ErrIO, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema
		WHERE type='table' AND name IN ('workflows','data_kv','nodes','waits','work_queue','leases','schedules')`).Scan(&tables); err != nil {
		return fmt.Errorf("%w: read workflow schema: %w", ErrCorruptData, err)
	}
	if tables != 7 {
		return fmt.Errorf("%w: incompatible workflow reader tables", ErrCorruptData)
	}
	// Prepare all required columns without scanning rows or demanding writer-only
	// schema additions. Keeping this producer-side avoids application-private SQL.
	rows, err := tx.QueryContext(ctx, `SELECT
		w.id,w.rolling_back,w.trigger_cause,
		d.workflow_id,d.key,d.kind,d.i_val,d.f_val,d.s_val,
		n.workflow_id,n.node_name,n.status,n.output,n.has_output,
		t.workflow_id,t.node_name,t.fire_at,
		q.workflow_id,q.type,q.state,q.enqueued_at,q.attempts,q.updated_at,
		l.workflow_id,l.owner_id,
		s.id,s.kind,s.spec,s.target_type,s.next_fire_time,s.missed_policy,s.paused,s.input,s.created_at,s.updated_at
		FROM workflows w,data_kv d,nodes n,waits t,work_queue q,leases l,schedules s LIMIT 0`)
	if err != nil {
		return fmt.Errorf("%w: incompatible workflow reader columns: %w", ErrCorruptData, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("%w: close reader schema check: %w", ErrIO, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: finish reader schema check: %w", ErrIO, err)
	}
	return nil
}

// ListWorkflows lists existing workflow IDs in lexical order.
func (r *SQLiteReader) ListWorkflows() ([]string, error) {
	return r.store.ListWorkflows()
}

// ListPending lists pending work using the store's inclusive age filter.
func (r *SQLiteReader) ListPending(olderThan int64) ([]PendingItem, error) {
	return r.store.ListPending(olderThan)
}

// WorkflowStatus returns granular dispatch and node observations, as on SQLiteStore.
func (r *SQLiteReader) WorkflowStatus(id string) (*WorkflowStatus, error) {
	return r.store.WorkflowStatus(id)
}

// Load reconstructs a workflow from one committed read transaction, without a
// writer shadow cache. ErrNotFound and typed codec errors retain store semantics.
func (r *SQLiteReader) Load(id string) (*WorkflowData, error) {
	if err := validateWorkflowID(id); err != nil {
		return nil, err
	}
	ctx := context.Background()
	tx, err := r.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("%w: begin workflow read: %w", ErrIO, err)
	}
	defer tx.Rollback() //nolint:errcheck // close the snapshot on every failure
	data, err := loadSQLiteWorkflow(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: finish workflow read: %w", ErrIO, err)
	}
	return data, nil
}

// ListSchedules lists existing schedules ordered by next-fire time.
func (r *SQLiteReader) ListSchedules() ([]ScheduleInfo, error) {
	return r.store.ListSchedules()
}

// Close releases this reader's connection. It never invokes writer checkpointing.
// SQLite may clean up or retain its own coordination sidecars.
func (r *SQLiteReader) Close() error { return r.store.Close() }
