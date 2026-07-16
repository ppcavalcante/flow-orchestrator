package workflow

// M18 ph85 Slice 2 — the aggregate Snapshot(): one MUTUALLY-CONSISTENT read of the operator read-model.
// The granular Observability methods (OBS-RM-01..05) are each atomic on their own, but a caller composing
// several of them across separate calls could observe a torn view (a write lands between call 1 and call
// 2). Snapshot() issues its component queries inside ONE `BEGIN DEFERRED` read-txn so its parts agree at a
// single instant (DEC-M18-RM-CONSISTENCY).
//
// s.mu DECISION (DEC-M18-RM-CONSISTENCY, red-team-reviewed): Snapshot() TAKES s.mu — with
// SetMaxOpenConns(1) the single connection is the real serialization point, and taking s.mu makes it
// explicit + uniform with every other store method. COST (the disclosed operability footgun): a long
// Snapshot() read-txn blocks SAME-INSTANCE writers for its duration (they wait on s.mu + the one conn).
// MITIGATION: Snapshot issues only the fast component SELECTs inside the DEFERRED txn and commits promptly
// — bounded, not a scan. A caller wanting NON-BLOCKING reads opens a SEPARATE read-only *SQLiteStore on the
// same file (which contends only at the WAL layer, never on this instance's s.mu) — the same pattern the
// mutual-consistency test uses.

import (
	"context"
	"database/sql"
	"fmt"
)

// QueueSnapshot is a mutually-consistent operator read-model at one instant (all parts read inside one
// BEGIN DEFERRED txn): per-state counts + the in-flight list + stuck items + worker health.
type QueueSnapshot struct {
	Counts   map[string]int
	InFlight []InFlightItem
	Stuck    []StuckItem
	Workers  []WorkerInfo
}

// Snapshot returns a mutually-consistent read-model. It opens a BEGIN DEFERRED read-txn (a WAL read
// snapshot taken at the first read) and issues every component query inside it, so the counts, in-flight
// list, stuck items, and worker health all reflect ONE instant — a concurrent writer's flip is either
// fully visible or fully absent across all parts, never torn. Requires mp mode. olderThan/registeredTypes
// parameterize the stuck-work component exactly like StuckWork.
func (s *SQLiteStore) Snapshot(olderThan int64, registeredTypes []string) (*QueueSnapshot, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: Snapshot requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	// BEGIN DEFERRED: the read snapshot is taken at the FIRST read inside the txn and held stable for all
	// subsequent reads in it (WAL). Read-only → no write lock; commits promptly.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot begin: %w", ErrIO, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the real error is already returned
		}
	}()

	now := s.nowNanos()
	snap := &QueueSnapshot{Counts: make(map[string]int)}

	// (1) per-state counts.
	if err := scanCountsTx(ctx, tx, snap.Counts); err != nil {
		return nil, err
	}
	// (2) in-flight (claimed JOIN leases).
	inflight, err := scanInFlightTx(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	snap.InFlight = inflight
	// (3) stuck-work.
	stuck, err := scanStuckTx(ctx, tx, now, olderThan, registeredTypes)
	if err != nil {
		return nil, err
	}
	snap.Stuck = stuck
	// (4) worker health.
	workers, err := scanWorkersTx(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	snap.Workers = workers

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: snapshot commit: %w", ErrIO, err)
	}
	committed = true
	return snap, nil
}

// --- txn-scoped scanners (shared by Snapshot; the granular methods use their own auto-commit reads) ---

func scanCountsTx(ctx context.Context, tx *sql.Tx, out map[string]int) error {
	rows, err := tx.QueryContext(ctx, `SELECT state, count(*) FROM work_queue GROUP BY state`)
	if err != nil {
		return fmt.Errorf("%w: snapshot counts: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return fmt.Errorf("%w: scan snapshot count: %w", ErrCorruptData, err)
		}
		out[state] = n
	}
	return wrapRowsErr(rows.Err(), "snapshot count rows")
}

func scanInFlightTx(ctx context.Context, tx *sql.Tx, now int64) ([]InFlightItem, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT q.workflow_id, q.type, q.attempts, q.enqueued_at, l.owner_id, l.expiry
		 FROM work_queue q JOIN leases l ON l.workflow_id = q.workflow_id
		 WHERE q.state = 'claimed' ORDER BY q.enqueued_at`)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot in-flight: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []InFlightItem
	for rows.Next() {
		var it InFlightItem
		if err := rows.Scan(&it.WorkflowID, &it.Type, &it.Attempts, &it.EnqueuedAt, &it.OwnerID, &it.Expiry); err != nil {
			return nil, fmt.Errorf("%w: scan snapshot in-flight: %w", ErrCorruptData, err)
		}
		it.LeaseLive = it.Expiry >= now
		out = append(out, it)
	}
	return out, wrapRowsErr(rows.Err(), "snapshot in-flight rows")
}

func scanStuckTx(ctx context.Context, tx *sql.Tx, now, olderThan int64, registeredTypes []string) ([]StuckItem, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT q.workflow_id, q.type, q.state, q.attempts, q.enqueued_at
		 FROM work_queue q LEFT JOIN leases l ON l.workflow_id = q.workflow_id
		 WHERE q.state = 'pending' OR (q.state = 'claimed' AND l.expiry < ?)
		 ORDER BY q.enqueued_at`, now)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot stuck: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	regSet := make(map[string]struct{}, len(registeredTypes))
	for _, t := range registeredTypes {
		regSet[t] = struct{}{}
	}
	var out []StuckItem
	for rows.Next() {
		var it StuckItem
		if err := rows.Scan(&it.WorkflowID, &it.Type, &it.State, &it.Attempts, &it.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("%w: scan snapshot stuck: %w", ErrCorruptData, err)
		}
		switch {
		case it.State == wqClaimed:
			it.Reason = StuckLapsedClaimed
		case registeredTypes != nil && !containsType(regSet, it.Type):
			it.Reason = StuckUnregisteredType
		case olderThan > 0 && it.EnqueuedAt <= olderThan:
			it.Reason = StuckTooOldPending
		default:
			continue
		}
		out = append(out, it)
	}
	return out, wrapRowsErr(rows.Err(), "snapshot stuck rows")
}

func scanWorkersTx(ctx context.Context, tx *sql.Tx, now int64) ([]WorkerInfo, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT owner_id, count(*), sum(CASE WHEN expiry >= ? THEN 1 ELSE 0 END)
		 FROM leases GROUP BY owner_id ORDER BY owner_id`, now)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot workers: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []WorkerInfo
	for rows.Next() {
		var wi WorkerInfo
		if err := rows.Scan(&wi.OwnerID, &wi.TotalHeld, &wi.LiveHeld); err != nil {
			return nil, fmt.Errorf("%w: scan snapshot worker: %w", ErrCorruptData, err)
		}
		out = append(out, wi)
	}
	return out, wrapRowsErr(rows.Err(), "snapshot worker rows")
}

func wrapRowsErr(err error, what string) error {
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorruptData, what, err)
	}
	return nil
}
