package workflow

// M18 ph85 — the operator READ-MODEL (OBS-RM): a NEW type-asserted Observability interface over the
// M17 work_queue + leases + M15 node model. Purely additive read-side — the write path (Save/ClaimNext/
// Enqueue/terminal transitions) + WorkflowQuery are BYTE-UNCHANGED. Answers the four operator questions:
// what's in the queue (per-state counts), what's in-flight (claimed + lease owner/freshness), what's
// wedged (stuck-work), and what is each workflow doing (dispatch + node status).
//
// CONCURRENCY (grounded, workflow_store_sqlite.go): the store is WAL with SetMaxOpenConns(1) and every
// write holds s.mu for its whole txn. So a single SELECT is atomic (a WAL statement-start read snapshot),
// and each granular method below holds s.mu (uniform with the rest of the store; a read never observes a
// half-applied write on the same instance). The aggregate Snapshot() (a separate concern) wraps its
// component SELECTs in a BEGIN DEFERRED read-txn for MUTUAL consistency — see workflow_store_sqlite_snapshot.go.
//
// Lease FRESHNESS (DEC-M16-D3): a claimed row's lease is LIVE iff leases.expiry >= now, else LAPSED
// (reclaim-eligible). `now` is the injected LEASE clock (s.nowNanos()), the same clock leaseExpiryNanos()
// writes expiry through — so a FakeClock test sees a deterministic freshness boundary. expiry is a
// liveness heuristic, NEVER a safety input; the read-model only REPORTS it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InFlightItem is one claimed (in-flight) work item: the workflow, its dispatch metadata, and the lease
// owner + freshness (OBS-RM-02). LeaseLive is true iff the lease has not lapsed (expiry >= now).
type InFlightItem struct {
	WorkflowID string
	Type       string
	OwnerID    string
	Attempts   int
	EnqueuedAt int64 // unix-nanos
	Expiry     int64 // unix-nanos lease deadline (liveness)
	LeaseLive  bool  // expiry >= now (the injected lease clock)
}

// StuckReason classifies why a work item is wedged (OBS-RM-03).
type StuckReason string

const (
	// StuckUnregisteredType — a pending item whose type no worker in the given registered set can build.
	StuckUnregisteredType StuckReason = "unregistered_type"
	// StuckTooOldPending — a pending item enqueued at/before the age cutoff (nobody has claimed it).
	StuckTooOldPending StuckReason = "too_old_pending"
	// StuckLapsedClaimed — a claimed item whose lease has lapsed (a dead worker's abandoned item awaiting reclaim).
	StuckLapsedClaimed StuckReason = "lapsed_claimed"
)

// StuckItem is one wedged work item + why it's stuck (OBS-RM-03).
type StuckItem struct {
	WorkflowID string
	Type       string
	State      string
	Attempts   int
	EnqueuedAt int64
	Reason     StuckReason
}

// NodeStatusCount is a per-status node tally for one workflow's journal (OBS-RM-04).
type NodeStatusCount struct {
	Status string
	Count  int
}

// WorkflowStatus is one workflow's combined dispatch + run status (OBS-RM-04). Dispatch fields come from
// work_queue (+ leases for the owner); NodeCounts is the per-status node tally from the journal (a direct
// nodes read — NOT a WorkflowQuery call). Queued is false when the id has no work_queue row.
type WorkflowStatus struct {
	WorkflowID string
	Queued     bool
	State      string // work_queue state ("" when !Queued)
	Attempts   int
	OwnerID    string // "" when not claimed / no lease
	UpdatedAt  int64  // last work_queue transition (unix-nanos)
	NodeCounts []NodeStatusCount
}

// WorkerInfo is one owner's health: how many leases it holds and how many are still live (OBS-RM-05).
type WorkerInfo struct {
	OwnerID   string
	TotalHeld int
	LiveHeld  int // leases with expiry >= now
}

// Observability is the additive optional operator read-model interface (M18 ph85, OBS-RM). A store opened
// as an mp SQLiteStore implements it; callers type-assert like Checkpointer/ClaimStore/WorkflowQuery. Four
// of the five granular methods are ONE atomic SELECT each; WorkflowStatus composes TWO (its dispatch row +
// its node tally) and is NOT mutually-consistent across them — its two halves can reflect different instants
// under a concurrent cross-process writer. Use Snapshot() for a consistent cross-query view (it wraps its
// component queries in one read-txn). Requires mp mode (the work_queue/leases tables only exist on an mp store).
type Observability interface {
	// QueueCounts returns per-state work_queue row counts (OBS-RM-01). typ=="" counts the whole queue;
	// a non-empty typ filters to that dispatch type. States with 0 rows are absent from the map.
	QueueCounts(typ string) (map[string]int, error)
	// InFlight enumerates the claimed (in-flight) items with their lease owner + freshness (OBS-RM-02).
	InFlight() ([]InFlightItem, error)
	// StuckWork returns wedged items (OBS-RM-03): pending items enqueued at/before olderThan (unix-nanos;
	// <=0 = no age filter) OR whose type is not in registeredTypes, PLUS lapsed-claimed items (a dead
	// worker's abandoned claim). registeredTypes=nil skips the unregistered-type check.
	StuckWork(olderThan int64, registeredTypes []string) ([]StuckItem, error)
	// WorkflowStatus returns one workflow's dispatch + node status (OBS-RM-04).
	WorkflowStatus(workflowID string) (*WorkflowStatus, error)
	// WorkerHealth returns each lease owner's held/live counts (OBS-RM-05).
	WorkerHealth() ([]WorkerInfo, error)
}

// Compile-time assertion that *SQLiteStore satisfies Observability.
var _ Observability = (*SQLiteStore)(nil)

// QueueCounts implements OBS-RM-01.
func (s *SQLiteStore) QueueCounts(typ string) (map[string]int, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: QueueCounts requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT state, count(*) FROM work_queue`
	var args []interface{}
	if typ != "" {
		query += ` WHERE type = ?`
		args = append(args, typ)
	}
	query += ` GROUP BY state`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: queue counts: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	out := make(map[string]int)
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("%w: scan count row: %w", ErrCorruptData, err)
		}
		out[state] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: count rows: %w", ErrCorruptData, err)
	}
	return out, nil
}

// InFlight implements OBS-RM-02.
func (s *SQLiteStore) InFlight() ([]InFlightItem, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: InFlight requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowNanos()
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.workflow_id, q.type, q.attempts, q.enqueued_at, l.owner_id, l.expiry
		 FROM work_queue q JOIN leases l ON l.workflow_id = q.workflow_id
		 WHERE q.state = 'claimed'
		 ORDER BY q.enqueued_at`)
	if err != nil {
		return nil, fmt.Errorf("%w: in-flight: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []InFlightItem
	for rows.Next() {
		var it InFlightItem
		if err := rows.Scan(&it.WorkflowID, &it.Type, &it.Attempts, &it.EnqueuedAt, &it.OwnerID, &it.Expiry); err != nil {
			return nil, fmt.Errorf("%w: scan in-flight row: %w", ErrCorruptData, err)
		}
		it.LeaseLive = it.Expiry >= now
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: in-flight rows: %w", ErrCorruptData, err)
	}
	return out, nil
}

// StuckWork implements OBS-RM-03.
func (s *SQLiteStore) StuckWork(olderThan int64, registeredTypes []string) ([]StuckItem, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: StuckWork requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowNanos()

	// One SELECT over work_queue + leases; the stuck classification is derived per-row in Go so the query
	// stays a single atomic read. We fetch every pending row + every lapsed-claimed row, then label each.
	// lapsed-claimed = state='claimed' AND its lease has expired (a dead worker's abandoned item).
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.workflow_id, q.type, q.state, q.attempts, q.enqueued_at
		 FROM work_queue q LEFT JOIN leases l ON l.workflow_id = q.workflow_id
		 WHERE q.state = 'pending'
		    OR (q.state = 'claimed' AND l.expiry < ?)
		 ORDER BY q.enqueued_at`, now)
	if err != nil {
		return nil, fmt.Errorf("%w: stuck work: %w", ErrIO, err)
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
			return nil, fmt.Errorf("%w: scan stuck row: %w", ErrCorruptData, err)
		}
		switch {
		case it.State == wqClaimed:
			// The query already filtered to lapsed (expiry < now).
			it.Reason = StuckLapsedClaimed
		case registeredTypes != nil && !containsType(regSet, it.Type):
			it.Reason = StuckUnregisteredType
		case olderThan > 0 && it.EnqueuedAt <= olderThan:
			it.Reason = StuckTooOldPending
		default:
			continue // a pending row that is neither unregistered nor too-old is NOT stuck — skip it.
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: stuck rows: %w", ErrCorruptData, err)
	}
	return out, nil
}

func containsType(set map[string]struct{}, t string) bool {
	_, ok := set[t]
	return ok
}

// WorkflowStatus implements OBS-RM-04. It composes TWO SELECTs — the dispatch row (work_queue + lease) and
// the node tally (nodes GROUP BY status) — so its two halves are NOT mutually-consistent: under a concurrent
// cross-process writer they can reflect different instants. This is acceptable for a per-workflow status view;
// use Snapshot() when you need a cross-query-consistent read.
func (s *SQLiteStore) WorkflowStatus(workflowID string) (*WorkflowStatus, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: WorkflowStatus requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	st := &WorkflowStatus{WorkflowID: workflowID}
	// Dispatch status: the work_queue row + the lease owner (LEFT JOIN — a pending row has no lease).
	var (
		state, owner sql.NullString
		attempts     sql.NullInt64
		updatedAt    sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT q.state, q.attempts, q.updated_at, l.owner_id
		 FROM work_queue q LEFT JOIN leases l ON l.workflow_id = q.workflow_id
		 WHERE q.workflow_id = ?`, workflowID).Scan(&state, &attempts, &updatedAt, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		st.Queued = false // no work_queue row — never dispatched (may still have a journal below)
	case err != nil:
		return nil, fmt.Errorf("%w: workflow status dispatch: %w", ErrIO, err)
	default:
		st.Queued = true
		st.State = state.String
		st.Attempts = int(attempts.Int64)
		st.UpdatedAt = updatedAt.Int64
		st.OwnerID = owner.String
	}

	// Node/journal status: the per-status node tally (a direct nodes read — NOT a WorkflowQuery call).
	nrows, err := s.db.QueryContext(ctx,
		`SELECT status, count(*) FROM nodes WHERE workflow_id = ? AND status != '' GROUP BY status ORDER BY status`,
		workflowID)
	if err != nil {
		return nil, fmt.Errorf("%w: workflow status nodes: %w", ErrIO, err)
	}
	defer nrows.Close() //nolint:errcheck // read-only
	for nrows.Next() {
		var nc NodeStatusCount
		if err := nrows.Scan(&nc.Status, &nc.Count); err != nil {
			return nil, fmt.Errorf("%w: scan node status: %w", ErrCorruptData, err)
		}
		st.NodeCounts = append(st.NodeCounts, nc)
	}
	if err := nrows.Err(); err != nil {
		return nil, fmt.Errorf("%w: node status rows: %w", ErrCorruptData, err)
	}
	return st, nil
}

// WorkerHealth implements OBS-RM-05.
func (s *SQLiteStore) WorkerHealth() ([]WorkerInfo, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: WorkerHealth requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowNanos()
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner_id, count(*), sum(CASE WHEN expiry >= ? THEN 1 ELSE 0 END)
		 FROM leases GROUP BY owner_id ORDER BY owner_id`, now)
	if err != nil {
		return nil, fmt.Errorf("%w: worker health: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []WorkerInfo
	for rows.Next() {
		var wi WorkerInfo
		if err := rows.Scan(&wi.OwnerID, &wi.TotalHeld, &wi.LiveHeld); err != nil {
			return nil, fmt.Errorf("%w: scan worker row: %w", ErrCorruptData, err)
		}
		out = append(out, wi)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: worker rows: %w", ErrCorruptData, err)
	}
	return out, nil
}
