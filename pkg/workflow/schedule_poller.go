package workflow

// M20 ph100 — the opt-in in-process schedule POLLER (SCHED-02). An embedder starts it (like the M17 Pool); it
// ticks on the store's injected Clock, scans due schedules, and runs the fenced single-txn fire for each. NO
// network / IPC surface (M20-INV-ZERO-ENTRY): it is a goroutine that calls store methods, nothing more. Many
// pollers across many processes race the same due slot safely — the per-schedule fence (fireDueLocked) lets
// exactly one win, so the poller CADENCE is pure liveness (miss a tick → the slot fires on the next tick),
// while the stored next_fire_time is the safety arbiter.

import (
	"context"
	"fmt"
	"time"
)

// defaultSchedulePollInterval bounds the schedule scan rate. Off the safety path (the fence + stored next_fire
// are the arbiters); a longer interval only delays a fire, never drops or doubles it.
const defaultSchedulePollInterval = 1 * time.Second

// SchedulePoller fires due schedules off a store. Constructed with NewSchedulePoller; started with Run (blocks
// until ctx is cancelled). Immutable after construction.
type SchedulePoller struct {
	store        *SQLiteStore
	ownerID      string        // this poller's fencing owner id (distinct per process/poller)
	pollInterval time.Duration // scan cadence (liveness only)
}

// SchedulePollerOption configures a SchedulePoller.
type SchedulePollerOption func(*SchedulePoller)

// WithSchedulePollInterval sets the scan cadence (liveness only; <= 0 → the default).
func WithSchedulePollInterval(d time.Duration) SchedulePollerOption {
	return func(p *SchedulePoller) { p.pollInterval = d }
}

// NewSchedulePoller builds a poller firing `store`'s due schedules under `ownerID` (the fencing owner — distinct
// per OS process so the per-schedule fence arbitrates cross-process races). Requires an mp store.
func NewSchedulePoller(store *SQLiteStore, ownerID string, opts ...SchedulePollerOption) (*SchedulePoller, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: NewSchedulePoller requires a non-nil store", ErrValidation)
	}
	if !store.dur.mp {
		return nil, fmt.Errorf("%w: NewSchedulePoller requires a multi-process store", ErrValidation)
	}
	if ownerID == "" {
		return nil, fmt.Errorf("%w: NewSchedulePoller requires a non-empty ownerID", ErrValidation)
	}
	p := &SchedulePoller{store: store, ownerID: ownerID, pollInterval: defaultSchedulePollInterval}
	for _, opt := range opts {
		opt(p)
	}
	if p.pollInterval <= 0 {
		p.pollInterval = defaultSchedulePollInterval
	}
	return p, nil
}

// Run scans + fires due schedules every pollInterval until ctx is cancelled, then returns nil. Each tick: scan
// the due set (next_fire ≤ now, not paused), fire each via the fenced single-txn fire. A per-schedule error is
// swallowed + retried next tick (a transient ErrBusy / a lost claim is normal under contention) — the poller
// never dies on one bad schedule. The FIRST scan runs immediately (no initial pollInterval delay) so a due
// schedule fires promptly on start.
func (p *SchedulePoller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	p.tick(ctx) // fire due schedules immediately on start
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick scans the due schedules and fires each. Bounded: it reads the due-id set into memory first (a short
// snapshot), then fires each id in its own txn — so a long fire never holds the scan's read open, and a
// schedule deleted/advanced between scan and fire is handled inside fireDueLocked (it re-reads under the txn).
func (p *SchedulePoller) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ids, err := p.store.dueScheduleIDs(ctx)
	if err != nil {
		return // transient scan error → retry next tick.
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_, _ = p.store.fireDueLocked(ctx, id, p.ownerID) //nolint:errcheck // per-schedule faults retry next tick
	}
}

// dueScheduleIDs returns the ids of active (non-paused) schedules whose next_fire_time has passed, per the
// INJECTED clock (DEC-P100-CLOCK-SEAM — a FakeClock drives due-ness). Read-only; the fire re-validates each id
// under its own IMMEDIATE txn, so this scan is just a candidate list (a stale row is caught at fire time).
func (s *SQLiteStore) dueScheduleIDs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowNanos()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM schedules WHERE paused=0 AND next_fire_time <= ? ORDER BY next_fire_time`, now)
	if err != nil {
		return nil, classifyTxErr("scan due schedules", err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var ids []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			return nil, classifyTxErr("scan due schedule id", serr)
		}
		ids = append(ids, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, classifyTxErr("due schedule rows", rerr)
	}
	return ids, nil
}
