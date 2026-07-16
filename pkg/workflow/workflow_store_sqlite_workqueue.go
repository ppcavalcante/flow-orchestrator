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
	cancelledAny := false // M18 ph87: true once a reclaim-terminalize-cancelled has run in this txn (BLOCKER-2)
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
			// No runnable item. BUT if this txn terminalized any cancel-set reclaim rows (BLOCKER-2), we
			// MUST COMMIT so those `cancelled` flips persist — otherwise the defer-rollback discards them and
			// the crashed-while-cancel-pending row strands in `claimed` limbo (the exact bug the resolution
			// closes). Commit-then-ErrNoWork on that path; plain ErrNoWork (rollback, nothing to persist) else.
			if cancelledAny {
				if cErr := tx.Commit(); cErr != nil {
					return WorkItem{}, classifyTxErr("claimnext cancel-commit", cErr)
				}
				committed = true
			}
			return WorkItem{}, ErrNoWork // empty / no-match / whole claimable set contended away (cancelled rows cleaned up).
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

		// RECLAIM-TERMINALIZES-CANCELLED (M18 ph87, BLOCKER-2 — the liveness resolution): a lapsed-`claimed`
		// row (state==wqClaimed here, its owner presumed dead/stalled) whose `cancel_requested` is SET must
		// NOT be re-offered for reclaim (it must never resume) — but also must NOT be excluded from the scan
		// (that would STRAND a crashed-while-cancel-pending row in `claimed` limbo forever = the AF1
		// crash-window class). So we TERMINALIZE it `cancelled` HERE, inside the BEGIN IMMEDIATE txn, under
		// the reclaimer's FRESH bumped token (fencing-clean — the superseded dead owner can't defeat it),
		// then skip it (tried + continue) and re-scan for the next ACTUALLY-runnable item. This closes the
		// limbo AND satisfies never-resume (the row is cancelled, never handed to a worker). A LIVE-lease
		// cancel-set row is NOT matched by the reclaim scan (it needs expiry<now); its owner's own
		// disposition gate (disposeExecErr, BLOCKER-1) handles that case — no overlap.
		if state == wqClaimed {
			var reqTS sql.NullInt64
			if serr := tx.QueryRowContext(ctx, `SELECT cancel_requested FROM work_queue WHERE workflow_id=?`, wf).Scan(&reqTS); serr != nil {
				return WorkItem{}, classifyTxErr("claimnext cancel-check", serr)
			}
			if reqTS.Valid {
				if _, terr := tx.ExecContext(ctx,
					`UPDATE work_queue SET state='cancelled', updated_at=? WHERE workflow_id=? AND state='claimed'`,
					unixNanoNow(), wf); terr != nil {
					return WorkItem{}, classifyTxErr("claimnext cancel-terminalize", terr)
				}
				cancelledAny = true       // a cancel-terminalize ran → the txn MUST commit even if no runnable item follows.
				tried = append(tried, wf) // cancelled + cleaned up → skip it, re-scan for the next runnable item in-txn.
				continue
			}
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
		if state == wqClaimed && s.metrics != nil {
			// RECLAIM-AFTER-DEATH (OBS-MET-01): the scanned row was `claimed` (not pending) → a lapsed-claimed
			// row was re-claimed by this live worker (a dead/stalled owner's item taken over under a bumped
			// token). Count the event (nil-guarded → zero-cost when metrics unset).
			s.metrics.incReclaimAfterDeath()
		}
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
	if n == 0 && s.metrics != nil {
		// FENCE-REJECTION (OBS-MET-02): a token-guarded flip matching 0 rows has TWO causes (review
		// ph86-F3): (a) a GENUINE fence-rejection — the row is STILL `claimed` but our token is stale
		// (a re-claim bumped past us, the EXISTS guard fails); OR (b) a BENIGN already-terminal no-op — a
		// live owner (held != 0) double-flips an already-terminal row (state != `claimed`). Only (a) is a
		// fence-rejection. Distinguish by re-reading the row's state (only on this rare 0-row+metrics path,
		// so no hot-path cost): count ONLY when the row is still `claimed`.
		var cur string
		if serr := s.db.QueryRowContext(ctx, `SELECT state FROM work_queue WHERE workflow_id=?`, workflowID).Scan(&cur); serr == nil && cur == wqClaimed {
			s.metrics.incFenceRejections()
		}
	}
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

// CancelRunning requests cancellation of a RUNNING (or still-pending) workflow (M18 ph87, OBS-CAN). It
// sets the durable `cancel_requested` INTENT flag — it does NOT itself terminalize the row. The intent is
// what distinguishes an operator-cancel (→ terminal `cancelled`, never resumed) from an AF1 graceful-drain
// or crash (→ leave `claimed`, resume-from-frontier later), which are byte-identical `context.Canceled` at
// the queue layer. The actual terminalization is done by the token-holding owner (its disposeExecErr
// disposition gate) or, if the owner crashed, by a reclaimer inside ClaimNext (both fencing-gated).
//
// AUTHORITY (DEC-M18-CANCEL-FENCING): requires NO fencing token — any store-handle caller (a CLI, an admin
// process, a pool worker) may request cancel. This is the operator's control surface; setting a flag on a
// separate column is fencing-safe by construction (it cannot lose to a live MarkDone nor clobber a
// reclaimer's completion — only the token-holder terminalizes). Idempotent: sets the flag only WHERE it is
// still NULL and the row is non-terminal (pending|claimed), so a double-cancel or a cancel-of-terminal is a
// detectable 0-row no-op. Returns requested=true iff this call set the flag.
func (s *SQLiteStore) CancelRunning(workflowID string) (requested bool, err error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: CancelRunning requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return false, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE work_queue SET cancel_requested=?, updated_at=?
		 WHERE workflow_id=? AND state IN ('pending','claimed') AND cancel_requested IS NULL`,
		unixNanoNow(), unixNanoNow(), workflowID)
	if err != nil {
		return false, classifyTxErr("cancelrunning", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: cancelrunning rows: %w", ErrIO, err)
	}
	return n == 1, nil // 1 = the flag was set now; 0 = already-cancelled | already-terminal | not present.
}

// isCancelRequested reports whether a workflow's cancel_requested flag is SET (non-NULL). Lock-free BY
// DESIGN — it does NOT hold s.mu (unlike the granular store methods): its callers (the cancelWatcher
// goroutine, the disposition gate in disposeExecErr, the post-reclaim re-read) run OUTSIDE the store lock,
// and a lone SELECT is atomic regardless under MaxOpenConns(1)+WAL (it serializes on the single conn
// behind any in-flight write txn). Safe because the flag is monotonic (NULL→set, never cleared) and every
// caller treats a read error as best-effort (not-cancelled, retry next poll). The reclaim path does NOT
// use this method — it inlines its own cancel_requested SELECT inside ClaimNext's IMMEDIATE txn (BLOCKER-2).
// Used by the disposition gate (BLOCKER-1) + the post-reclaim re-read. A missing row → false (nothing to cancel).
func (s *SQLiteStore) isCancelRequested(ctx context.Context, workflowID string) (bool, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT cancel_requested FROM work_queue WHERE workflow_id=?`, workflowID).Scan(&ts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, classifyTxErr("iscancelrequested", err)
	}
	return ts.Valid, nil
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
