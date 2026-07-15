package workflow

// M17 ph80 — the durable work-queue substrate on SQLiteStore (DF-1): Enqueue + the atomic ClaimNext
// + CAS-guarded terminal transitions. This is the Tier-1 dispatch primitive — a pool of worker
// processes shares one .db file, each ClaimNext()ing the oldest pending row of a matching type and
// driving it. It rides the M16 fencing CAS orthogonally: ClaimNext claims the durable `leases` row
// (the sole safety arbiter, unchanged) inside the SAME BEGIN IMMEDIATE txn as the work_queue scan +
// state flip (constraint C1 — scan → claim → flip is ONE atomic txn, so N contenders never
// double-claim). The work_queue table is additive; shipped tables + the FB format are untouched.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNoWork — ClaimNext found no claimable row (empty queue, or none matched the type filter, or the
// whole pending set was contended away). A poller backs off on this (ph82); it is NOT an error
// condition, so it is errors.Is-distinguishable from ErrIO/ErrBusy (a transient fault a poller
// retries) — a poller sleeps on ErrNoWork but retries on ErrBusy.
var ErrNoWork = errors.New("no claimable work in the queue")

// WorkItem is a claimed queue entry returned by ClaimNext: the workflow to drive, its dispatch type,
// the opaque caller payload, and the M16 fencing token the claim was granted under (the executor
// checkpoints through the same store instance's tokenState — see WithMultiProcessLocker).
type WorkItem struct {
	WorkflowID string
	Type       string
	Input      []byte
	Token      FencingToken
}

// Enqueue durably submits workflowID for dispatch as a `pending` work_queue row of the given type.
// IDEMPOTENT + DETECTABLE (DEC-M17-REENQUEUE): INSERT … ON CONFLICT(workflow_id) DO NOTHING, and it
// returns queued=true iff a new row was inserted. A re-submit of ANY existing id (pending, claimed,
// or a terminal done/failed/cancelled) is a VISIBLE no-op — queued=false, the existing row byte-
// unchanged (a `failed` id stays `failed`) — never a silent black hole. input may be nil.
func (s *SQLiteStore) Enqueue(workflowID, typ string, input []byte) (queued bool, err error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: Enqueue requires a multi-process store (WithMultiProcess)", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return false, err
	}
	if typ == "" {
		return false, fmt.Errorf("%w: Enqueue requires a non-empty type", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := unixNanoNow() // NOT the lease clock — enqueue is not a lease op (keeps clock injection scoped to leases)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO work_queue(workflow_id, type, input, enqueued_at, state, attempts, updated_at)
		 VALUES (?,?,?,?, 'pending', 0, ?)
		 ON CONFLICT(workflow_id) DO NOTHING`,
		workflowID, typ, input, now, now)
	if err != nil {
		return false, classifyTxErr("enqueue", err) // SQLITE_BUSY → ErrBusy (retry); else ErrIO.
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: enqueue rows: %w", ErrIO, err)
	}
	return n == 1, nil // 1 = a new pending row landed; 0 = the id already existed (visible no-op).
}

// wqStates — the terminal set (never re-claimed; the C2 infinite-reclaim guard lives in ClaimNext's
// `WHERE state='pending'` scan, but these name the lifecycle for the transition methods).
const (
	wqPending   = "pending"
	wqClaimed   = "claimed"
	wqDone      = "done"
	wqFailed    = "failed"
	wqCancelled = "cancelled"
)

// buildTypeFilter renders the optional `AND type IN (?,?,…)` clause + its args for a ClaimNext scan.
// Empty filter → no clause (claim any type). The values are ?-bound (never concatenated).
func buildTypeFilter(typeFilter []string) (clause string, args []interface{}) {
	if len(typeFilter) == 0 {
		return "", nil
	}
	ph := make([]string, len(typeFilter))
	args = make([]interface{}, len(typeFilter))
	for i, t := range typeFilter {
		ph[i] = "?"
		args[i] = t
	}
	return " AND type IN (" + strings.Join(ph, ",") + ")", args
}

// ClaimNext atomically claims the oldest claimable work item for ownerID (optionally filtered to the
// given types) and returns it, or ErrNoWork if none is claimable. C1 — the WHOLE operation is ONE
// BEGIN IMMEDIATE txn: scan the oldest pending row → claimLocked the M16 fencing lease for it →
// flip work_queue.state pending→claimed (CAS-guarded) → commit. N worker processes contend on the
// one write lock, so exactly one claims any given row; a terminal row is NEVER returned (the
// `state='pending'` scan filter — C2). Requires mp mode.
func (s *SQLiteStore) ClaimNext(ownerID string, typeFilter ...string) (WorkItem, error) {
	if !s.dur.mp {
		return WorkItem{}, fmt.Errorf("%w: ClaimNext requires a multi-process store (WithMultiProcess)", ErrValidation)
	}
	if ownerID == "" {
		return WorkItem{}, fmt.Errorf("%w: ClaimNext requires a non-empty ownerID", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE (mp DSN) → the write lock spans scan+claim+flip.
	if err != nil {
		return WorkItem{}, classifyTxErr("claimnext begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	typeClause, typeArgs := buildTypeFilter(typeFilter)

	// A claimed workflow_id is excluded from the RE-SCAN on a rare ErrClaimLost (a scanned pending row
	// whose lease is momentarily LIVE under another owner — a lapsed-then-relive edge). The re-scan
	// stays INSIDE this same IMMEDIATE txn (the write lock is held → a stable snapshot → no interleave),
	// so C1 is preserved. Bounded by the pending-set size; exhausted → ErrNoWork.
	// M17 ph82 (DISP-05): the scan is BROADENED to also discover lapsed-`claimed` rows (re-claim-after-
	// death). A `claimed` row is a reclaim candidate iff its lease has LAPSED — `expiry < now` in the
	// `leases` table (the LIVENESS heuristic, DEC-M16-D3). This is a CANDIDATE filter only; `claimLocked`
	// below is the SAFETY arbiter (a row whose lease is LIVE under another owner → ErrClaimLost → skip).
	// A LIVE-lease `claimed` row fails the EXISTS predicate → never offered → never stolen.
	//
	// The expiry comparison reads s.nowNanos() — the injected LEASE clock (DEC-M16-D3), the SAME clock
	// leaseExpiryNanos() writes expiry through. It is NOT unixNanoNow() (the enqueue/flip wall clock): a
	// FakeClock test advances THIS clock to lapse a lease deterministically, and the scan must see it.
	reclaimNow := s.nowNanos()
	var tried []string
	for {
		excludeClause, excludeArgs := buildNotInFilter(tried)
		scanSQL := `SELECT workflow_id, type, input, state FROM work_queue
		            WHERE (state='pending'
		                   OR (state='claimed'
		                       AND EXISTS (SELECT 1 FROM leases l
		                                   WHERE l.workflow_id = work_queue.workflow_id AND l.expiry < ?)))` +
			typeClause + excludeClause + `
		            ORDER BY enqueued_at LIMIT 1`
		// The reclaim-expiry bind is FIRST — its `?` precedes the type/exclude clauses in the SQL text.
		scanArgs := append(append(append([]interface{}{}, reclaimNow), typeArgs...), excludeArgs...)

		var (
			wf, typ, state string
			input          []byte
		)
		scanErr := tx.QueryRowContext(ctx, scanSQL, scanArgs...).Scan(&wf, &typ, &input, &state)
		switch {
		case errors.Is(scanErr, sql.ErrNoRows):
			return WorkItem{}, ErrNoWork // empty / no-match / whole claimable set contended away.
		case scanErr != nil:
			return WorkItem{}, classifyTxErr("claimnext scan", scanErr)
		}

		// Claim the M16 fencing lease for this id INSIDE this txn (the sole safety arbiter, untouched).
		// A lapsed-`claimed` reclaim → claimLocked bumps the token (fencing the dead/stalled owner); a
		// row whose lease re-lived under another owner since the scan → ErrClaimLost → skip.
		token, claimErr := s.claimLocked(ctx, tx, wf, ownerID)
		if errors.Is(claimErr, ErrClaimLost) {
			tried = append(tried, wf) // a live foreign lease → skip this id, re-scan the rest in-txn.
			continue
		}
		if claimErr != nil {
			return WorkItem{}, claimErr // ErrIO/ErrBusy from the lease write.
		}

		// The flip is SPLIT by the scanned state — both branches bump attempts + updated_at, both keep the
		// C2 "scanned row the flip can't match = corruption" invariant per-case:
		//   - pending → flip pending→claimed, guarded `WHERE state='pending'` (the ph80 C2 guard, unchanged).
		//   - claimed → a RECLAIM: the row STAYS `claimed` (a new owner under a bumped token); bump attempts
		//               + updated_at only, guarded `WHERE state='claimed'`. RunNext's F3 gate then resumes
		//               from the committed frontier (no re-seed, no lost work).
		// In BOTH branches the BEGIN IMMEDIATE write lock serializes every ClaimNext, so nothing mutates this
		// row between the scan and the flip — a 0-row flip is an INVARIANT VIOLATION (a broken scan filter
		// surfacing a row whose state disagrees). Surfacing it (not silently recovering) is what lets the C2
		// and reclaim seed-breaks REDDEN instead of the guard masking them.
		var (
			res   sql.Result
			upErr error
		)
		if state == wqPending {
			res, upErr = tx.ExecContext(ctx,
				`UPDATE work_queue SET state='claimed', attempts=attempts+1, updated_at=?
				 WHERE workflow_id=? AND state='pending'`,
				unixNanoNow(), wf)
		} else { // wqClaimed reclaim — keep state, bump attempts under the new token.
			res, upErr = tx.ExecContext(ctx,
				`UPDATE work_queue SET attempts=attempts+1, updated_at=?
				 WHERE workflow_id=? AND state='claimed'`,
				unixNanoNow(), wf)
		}
		if upErr != nil {
			return WorkItem{}, classifyTxErr("claimnext flip", upErr)
		}
		n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
		if n == 0 {
			return WorkItem{}, fmt.Errorf(
				"%w: claimnext invariant: scanned %s row %q flipped 0 rows (scan/flip state disagree — a broken state filter)",
				ErrCorruptData, state, wf)
		}

		if err := tx.Commit(); err != nil {
			return WorkItem{}, classifyTxErr("claimnext commit", err)
		}
		committed = true
		s.tokenState[wf] = token // in-process fencing token for the checkpoint CAS (mu held).
		return WorkItem{WorkflowID: wf, Type: typ, Input: input, Token: token}, nil
	}
}

// buildNotInFilter renders `AND workflow_id NOT IN (?,?,…)` for the re-scan exclusion set.
func buildNotInFilter(ids []string) (clause string, args []interface{}) {
	if len(ids) == 0 {
		return "", nil
	}
	ph := make([]string, len(ids))
	args = make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return " AND workflow_id NOT IN (" + strings.Join(ph, ",") + ")", args
}

// flipState is the CAS-guarded terminal transition primitive: UPDATE the row to `to` ONLY if it is
// currently in `from`. Returns flipped=true iff exactly one row changed. A SECOND flip of an already-
// terminal row is a 0-row UPDATE → flipped=false (detectable, never a silent double-apply — the ph83
// oracle-(a) mechanism, pulled forward). Caller holds nothing; this is its own auto-commit statement.
func (s *SQLiteStore) flipState(workflowID, from, to string) (flipped bool, err error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: work-queue transitions require a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return false, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE work_queue SET state=?, updated_at=? WHERE workflow_id=? AND state=?`,
		to, unixNanoNow(), workflowID, from)
	if err != nil {
		return false, classifyTxErr("workqueue transition", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: transition rows: %w", ErrIO, err)
	}
	return n == 1, nil
}

// MarkDone flips a claimed work item to `done` (CAS-guarded: only from `claimed`). flipped=false if the
// row was not claimed (already terminal, or never claimed) — a detectable no-op, never a double-apply.
// M17 ph82 (ph80-F1): on a successful flip it ALSO Releases the M16 lease under the held token, so a
// successor need not wait out the TTL (also serves graceful drain).
func (s *SQLiteStore) MarkDone(workflowID string) (bool, error) {
	return s.flipTerminalAndRelease(workflowID, wqDone)
}

// MarkFailed flips a claimed work item to `failed` (CAS-guarded: only from `claimed`). Also Releases the
// lease on a successful flip (ph80-F1) — same rationale as MarkDone.
func (s *SQLiteStore) MarkFailed(workflowID string) (bool, error) {
	return s.flipTerminalAndRelease(workflowID, wqFailed)
}

// flipTerminalAndRelease flips a claimed row to a terminal state (CAS-guarded from `claimed`) and, on a
// successful flip, Releases the M16 lease this process holds for it (ph80-F1). Release is UNDER the held
// token — a superseded token (a re-claim already took the workflow) makes it a no-op DELETE (the successor
// owns a higher token; the fencing token, not the lease-row presence, is the arbiter). The lease-release is
// BEST-EFFORT: a Release error does NOT unflip the terminal transition (the durable queue state is already
// correct; the lease is a liveness convenience, and it lapses on its own at TTL). We flip FIRST, release
// SECOND: if the flip is a no-op (row wasn't claimed — already terminal or never claimed) there is no lease
// of OURS to release, so we skip it.
func (s *SQLiteStore) flipTerminalAndRelease(workflowID, to string) (bool, error) {
	flipped, err := s.flipTerminalFenced(workflowID, to)
	if err != nil {
		return false, err
	}
	if !flipped {
		return false, nil // not a claimed→terminal transition WE made → no lease of ours to release.
	}
	if s.dur.mp {
		token := s.heldToken(workflowID)
		if token != 0 {
			_ = s.Release(workflowID, token) //nolint:errcheck // best-effort; TTL lapse is the backstop.
		}
	}
	return true, nil
}

// flipTerminalFenced is the TOKEN-GUARDED claimed→terminal flip (review ph82-F1, structural defense). In
// multi-process mode it flips ONLY IF this process still holds the CURRENT fencing token for the workflow —
// the `AND fencing_token <= <held>` correlated guard makes a SUPERSEDED worker's terminal flip a 0-row
// no-op, so it can NEVER clobber a live higher-token reclaimer's queue row. The token check rides the SAME
// auto-commit UPDATE as the state CAS (one statement → atomic, no TOCTOU). A held==0 (non-claimed drive,
// e.g. single-process default) means "no claim to fence" → the guard degenerates to the plain state CAS
// (the leases subquery is skipped). Single-process (non-mp) uses the plain flipState (no leases table).
func (s *SQLiteStore) flipTerminalFenced(workflowID, to string) (bool, error) {
	if !s.dur.mp {
		return s.flipState(workflowID, wqClaimed, to) // single-process: no fencing, plain state CAS.
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return false, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.tokenState[workflowID]
	if held == 0 {
		// No claim of ours to fence (a non-claimed mp drive) → plain state CAS, inline under the held lock.
		res, err := s.db.ExecContext(ctx,
			`UPDATE work_queue SET state=?, updated_at=? WHERE workflow_id=? AND state=?`,
			to, unixNanoNow(), workflowID, wqClaimed)
		if err != nil {
			return false, classifyTxErr("workqueue terminal", err)
		}
		n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
		return n == 1, nil
	}
	// TOKEN-GUARDED: flip claimed→terminal ONLY IF we still hold the current token (no re-claim bumped it
	// past us). A superseded worker (held < durable current) matches 0 rows → a no-op, never clobbers B.
	res, err := s.db.ExecContext(ctx,
		`UPDATE work_queue SET state=?, updated_at=?
		 WHERE workflow_id=? AND state=?
		   AND EXISTS (SELECT 1 FROM leases l WHERE l.workflow_id=work_queue.workflow_id AND l.fencing_token <= ?)`,
		to, unixNanoNow(), workflowID, wqClaimed, int64(held))
	if err != nil {
		return false, classifyTxErr("workqueue terminal fenced", err)
	}
	n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
	return n == 1, nil
}

// MarkForRetry attempts to requeue a claimed work item for another attempt (M17 ph82, DF-4 retry policy).
// It is ONE atomic auto-commit UPDATE: flip `claimed`→`pending` ONLY IF the row is still `claimed` AND its
// `attempts` count is strictly below maxAttempts. Returns requeued=true iff the row was requeued (a new
// worker can now re-claim it — the `attempts` bumped on that next claim is what bounds the retry loop).
// requeued=false means either the attempt budget is exhausted (attempts>=maxAttempts) or the row was not
// claimed — in BOTH cases the caller dead-letters via MarkFailed (the terminal `failed` state). The CAS on
// `state='claimed'` keeps this consistent with the rest of the lifecycle: a row a re-claim already took is
// not requeued twice. On requeue we ALSO Release the lease under the held token so a sibling re-claims
// immediately rather than waiting out the TTL (same rationale as the terminal transitions, ph80-F1).
func (s *SQLiteStore) MarkForRetry(workflowID string, maxAttempts int) (requeued bool, err error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: MarkForRetry requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return false, err
	}
	ctx := context.Background()
	s.mu.Lock()
	held := s.tokenState[workflowID]
	// TOKEN-GUARDED requeue (review ph82-F1, structural): requeue claimed→pending ONLY IF this process
	// still holds the current fencing token — a superseded worker (held < durable current, or held==0 =
	// no claim) matches 0 rows → a no-op, never requeues a live reclaimer's row out from under it. The
	// guard rides the SAME auto-commit UPDATE as the state+attempts CAS (atomic, no TOCTOU). A superseded A
	// is already caught in disposeExecErr (ErrFencedOut → abort before MarkForRetry); this is the belt.
	res, err := s.db.ExecContext(ctx,
		`UPDATE work_queue SET state='pending', updated_at=?
		 WHERE workflow_id=? AND state='claimed' AND attempts < ?
		   AND EXISTS (SELECT 1 FROM leases l WHERE l.workflow_id=work_queue.workflow_id AND l.fencing_token <= ?)`,
		unixNanoNow(), workflowID, maxAttempts, int64(held))
	if err != nil {
		s.mu.Unlock()
		return false, classifyTxErr("markforretry", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("%w: markforretry rows: %w", ErrIO, err)
	}
	s.mu.Unlock()
	if n == 0 {
		return false, nil // budget exhausted, not claimed, OR superseded → caller dead-letters via MarkFailed.
	}
	if held != 0 {
		_ = s.Release(workflowID, held) //nolint:errcheck // best-effort; TTL lapse is the backstop.
	}
	return true, nil
}

// CancelPending cancels a PENDING work item (CAS-guarded: only from `pending` → `cancelled`).
// DEC-M17-CANCEL: a cancel of a CLAIMED (already-running) item is REJECTED — it returns flipped=false
// (the `WHERE state='pending'` guard matches 0 rows), a detectable "not cancellable", never a
// mid-flight interruption. Cancelling an already-terminal or already-cancelled row is likewise a
// detectable 0-row no-op.
func (s *SQLiteStore) CancelPending(workflowID string) (bool, error) {
	return s.flipState(workflowID, wqPending, wqCancelled)
}

// PendingItem is a stuck-work-visibility row (DEC-M17-STUCKVIS): a pending work_queue entry an operator
// can inspect. Type + EnqueuedAt let a host spot stuck work — an UNREGISTERED type (no worker claims it)
// or a too-OLD item (enqueued long ago, still unclaimed = the pool can't keep up or nothing handles it).
type PendingItem struct {
	WorkflowID string
	Type       string
	EnqueuedAt int64 // unix-nanos (the enqueue wall clock, unixNanoNow)
	Attempts   int   // requeues so far (a high count = a poison item cycling toward its dead-letter)
}

// ListPending returns the PENDING work items, oldest-first (FIFO — the claim order), optionally filtered
// to those enqueued at or before `olderThan` (unix-nanos; <=0 = no age filter → all pending). The
// stuck-work-visibility query (DEC-M17-STUCKVIS): an operator lists pending items to find work no worker
// is draining (an unregistered type stays pending forever; a too-old item signals the pool is behind or
// the type is unhandled). Read-only, indexed by idx_wq_claimable (the partial index on pending rows).
// Requires mp mode (the work_queue only exists on an mp store).
func (s *SQLiteStore) ListPending(olderThan int64) ([]PendingItem, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: ListPending requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	query := `SELECT workflow_id, type, enqueued_at, attempts FROM work_queue WHERE state='pending'`
	var args []interface{}
	if olderThan > 0 {
		query += ` AND enqueued_at <= ?`
		args = append(args, olderThan)
	}
	query += ` ORDER BY enqueued_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: list pending: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var out []PendingItem
	for rows.Next() {
		var it PendingItem
		if err := rows.Scan(&it.WorkflowID, &it.Type, &it.EnqueuedAt, &it.Attempts); err != nil {
			return nil, fmt.Errorf("%w: scan pending row: %w", ErrCorruptData, err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: pending rows: %w", ErrCorruptData, err)
	}
	return out, nil
}
