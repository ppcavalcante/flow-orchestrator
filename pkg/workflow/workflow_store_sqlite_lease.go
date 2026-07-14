package workflow

// M16 ph74 — the multi-process foundation: the ClaimStore/Leaser interface CONTRACT (defined
// here, implemented by ph75, wired by ph76), the FencingToken type, the WithMultiProcess opt-in,
// and the fencing-token CAS that rides the checkpoint IMMEDIATE txn.
//
// SAFETY MODEL (DEC-M16-D3, the inversion — load-bearing): the wall-clock lease `expiry` is a
// pure LIVENESS heuristic (it decides WHEN a lapsed workflow is offered for re-claim). It is
// NEVER a safety mechanism — a paused-but-alive worker is indistinguishable from a dead one by
// clock alone. The monotonic, DB-issued `fencing_token` is the SOLE safety arbiter: it decides
// WHOSE write wins. The CAS below re-reads the durable fencing_token inside the same
// BEGIN IMMEDIATE txn as the checkpoint write and rejects a stale-token write — so a superseded
// (re-claimed-past) zombie can never land a write, no matter what the clock says.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"
)

// FencingToken is a monotonic, DB-issued claim token — the sole cross-process safety arbiter
// (DEC-M16-D3). It is NEVER a timestamp. Zero means "no claim held".
type FencingToken int64

// M16 competing-consumers sentinels.
var (
	// ErrFencedOut — a write (or renew) attempted under a fencing token that a re-claim has
	// superseded. The zombie's write is rejected; its drive must abort. (DEC-M16-D7 z1)
	ErrFencedOut = errors.New("fenced out: stale claim token")
	// ErrClaimLost — a claim the caller did not win (a live lease is held by another owner).
	ErrClaimLost = errors.New("claim lost: workflow owned by another worker")
	// ErrBusy — a write txn could not acquire the SQLite write lock within busy_timeout (SQLITE_BUSY;
	// writer starvation under cross-process contention). TRANSIENT: a caller MAY retry/back off — it is
	// errors.Is-DISTINCT from ErrFencedOut (superseded, do NOT retry) so a competing consumer knows
	// retry-vs-abort. Only reachable in multi-process mode (the single-process single-conn lease never
	// contends). (MP-05, MAJOR-5.)
	ErrBusy = errors.New("sqlite busy: write lock contended past busy_timeout")
)

// isBusy reports whether a driver error is a SQLITE_BUSY (or a BUSY-family code) — the
// write-lock-contended-past-busy_timeout signal. modernc/sqlite DOES expose a typed error
// (*sqlite.Error with Code()), so the SOUND check is errors.As + a low-byte code compare
// (SQLITE_BUSY plus the extended BUSY_* codes all share the low byte 5). We mask the low byte to
// catch the family (e.g. SQLITE_BUSY_SNAPSHOT == SQLITE_BUSY | (n<<8)). A string fallback covers a
// busy error that lost its type across a wrap boundary — but keyed ONLY on the unambiguous
// "SQLITE_BUSY" token (NOT a bare "(5)" substring, which false-positives on any error text
// containing "(5)", e.g. a real IO fault "disk I/O error (5)").
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xFF == sqlite3lib.SQLITE_BUSY
	}
	// Typed error lost across a wrap — fall back to the unambiguous token only.
	return strings.Contains(err.Error(), "SQLITE_BUSY")
}

// classifyTxErr maps a raw write-txn error to a typed sentinel: ErrBusy for a SQLITE_BUSY (transient,
// retryable), else ErrIO (opaque). The %w chain preserves the raw driver error for diagnosis while
// making the class errors.Is-reachable at the Execute return.
func classifyTxErr(op string, err error) error {
	if isBusy(err) {
		return fmt.Errorf("%w: %s: %w", ErrBusy, op, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrIO, op, err)
}

// ClaimStore is the additive, optional cross-process competing-consumers interface (M16). A
// SQLiteStore opened WithMultiProcess implements it; type-assert like Checkpointer/WorkflowQuery.
// The frozen 4-method WorkflowStore base is UNTOUCHED. ph74 defines this contract + wires the
// token→CAS plumbing; ph75 implements the claim POLICY; ph76 wires it into the executor — so the
// interface does NOT churn across phases.
//
// Ownership model: N worker processes share one DB file; each Claims a distinct workflow and
// drives it to a checkpoint. No two live workers ever execute one workflow; a dead/slow worker's
// claim becomes re-claimable via lease expiry (liveness) + fencing (safety).
type ClaimStore interface {
	// Claim acquires — or re-claims a lapsed — durable lease for workflowID on behalf of ownerID
	// (an opaque process identity), returning a fresh monotonic FencingToken. INSERT-or-CAS: the
	// lease row may not exist yet (a never-run workflow — the z4 initial-claim case). Returns
	// ErrClaimLost if a LIVE lease is held by a different owner.
	Claim(workflowID, ownerID string) (FencingToken, error)

	// Renew extends the lease's expiry under the held token (DEC-M16-D4 explicit lifecycle;
	// ph76 calls it). Returns ErrFencedOut if the token was superseded by a re-claim — the drive
	// has been taken over and MUST abort, never continue uninsured. (Checkpoints ALSO bump expiry
	// automatically inside their CAS txn — renew-on-checkpoint — so Renew is for the explicit
	// out-of-band lifecycle, not the only renewal path.)
	Renew(workflowID string, token FencingToken) error

	// Release drops the lease iff the caller still holds token (best-effort clean handoff so a
	// successor need not wait for expiry). A no-op if the token was already superseded.
	Release(workflowID string, token FencingToken) error
}

// openSQLiteDB opens the modernc SQLite DB, adding the M16 `_txlock=immediate` DSN suffix in
// multi-process mode (C1) and leaving the bare `path` DSN unchanged single-process. `path` is
// the caller's own store-construction DB path (the same trust level as every M15 sql.Open in
// this package); the mp branch appends only a hardcoded literal.
func openSQLiteDB(path string, mp bool, driverName string) (*sql.DB, error) {
	dsn := path
	if mp {
		dsn = path + "?_txlock=immediate"
	}
	if driverName == "" {
		driverName = "sqlite" // production default: the real modernc driver (unchanged).
	}
	return sql.Open(driverName, dsn) //nolint:gosec // G703: `path` is trusted store-construction input, not request-derived; only a hardcoded DSN suffix is added. driverName is "" (real driver) in production; a test-only fault driver otherwise.
}

// WithMultiProcess opts a SQLiteStore into M16 cross-process mode: the DSN opens with
// _txlock=immediate (C1 — DEFERRED txns deadlock on lock-upgrade with 2 OS-process writers) and
// the fencing-token CAS is enabled on every checkpoint write path. Default (without this option)
// is the M15 single-process store, byte-for-byte unchanged. Compose with WithSQLiteDurability.
func WithMultiProcess() SQLiteOption {
	return func(d *sqliteDurability) { d.mp = true }
}

// WithLeaseTTL sets the multi-process lease duration — the LIVENESS heuristic that decides when a
// lapsed workflow becomes re-claimable (DEC-M16-D3; it is NEVER a safety mechanism, the fencing
// token is). Size it ABOVE your longest expected level compute: a single level whose compute
// exceeds leaseTTL may be re-claimed by another worker and its work fenced + redone — never
// double-committed (the ph76 D4 operator contract). No effect on a single-process store (the lease
// path only engages under WithMultiProcess). A leaseTTL <= 0 falls back to defaultLeaseTTL (30s).
// Off the hot path (updated_at stays on unixNanoNow) — det-tax is unaffected.
func WithLeaseTTL(d time.Duration) SQLiteOption {
	return func(sd *sqliteDurability) { sd.leaseTTL = d }
}

// withSQLiteClock injects the lease-liveness clock (test-only; production uses SystemClock).
// Unexported: the injectable clock exists so lease-lapse is deterministically testable
// (FakeClock), NOT as a public knob — DEC-M16-D3 keeps the clock a liveness heuristic only.
func withSQLiteClock(c Clock) SQLiteOption {
	return func(d *sqliteDurability) { d.clock = c }
}

// withSQLiteDriverName overrides the database/sql driver the store opens (TEST-ONLY, unexported;
// default "" → the real "sqlite" modernc driver, production byte-unchanged). A fault-injecting
// test driver registers under a distinct name so the store's error branches (Exec/Query/Commit
// failure, fsync failure) can be exercised — task #126 coverage sweep. Never a production knob.
func withSQLiteDriverName(name string) SQLiteOption {
	return func(d *sqliteDurability) { d.driverName = name }
}

// withSQLiteLeaseTTL sets the lease duration (test-only; ph76 owns the production TTL/cadence).
func withSQLiteLeaseTTL(ttl time.Duration) SQLiteOption {
	return func(d *sqliteDurability) { d.leaseTTL = ttl }
}

// setToken records the fencing token this process holds for a workflow (called by ph75's Claim).
// Guarded by mu (uniform with shadow/ckptCount).
func (s *SQLiteStore) setToken(workflowID string, token FencingToken) {
	s.mu.Lock()
	s.tokenState[workflowID] = token
	s.mu.Unlock()
}

// clearToken drops this process's held token for a workflow (called by ph75's Release).
func (s *SQLiteStore) clearToken(workflowID string) {
	s.mu.Lock()
	delete(s.tokenState, workflowID)
	s.mu.Unlock()
}

// heldToken returns the token this process holds for a workflow (0 = none). Caller holds mu.
func (s *SQLiteStore) heldTokenLocked(workflowID string) FencingToken {
	return s.tokenState[workflowID]
}

// checkFencingLocked is the FENCING CAS — the load-bearing safety check. Called INSIDE a
// checkpoint's BEGIN IMMEDIATE txn (so the check and the subsequent write are atomic under the
// write lock). It re-reads the lease's current fencing_token and rejects the write if this
// process's held token is STALE (strictly less than the durable current token — a re-claim
// bumped it). On success in mp mode it ALSO renews the lease expiry (renew-on-checkpoint,
// DEC-M16-D4) under the held token.
//
// No-op (returns nil) when:
//   - the store is single-process (dur.mp == false) — the M15 path is untouched; OR
//   - this process holds no token for the workflow (held == 0) — a non-claimed drive (e.g. the
//     single-process default even on an mp store, or a Save outside any claim). The lease table
//     is simply absent of a row → nothing to fence against.
//
// SEED-THE-BREAK anchor: the `held < current` predicate is THE guard. Removing it lets a stale
// zombie write land → the store-level bite reddens (guard-must-bite; the ph74 hard bar).
func (s *SQLiteStore) checkFencingLocked(ctx context.Context, tx *sql.Tx, workflowID string) error {
	if !s.dur.mp {
		return nil // single-process: no fencing.
	}
	held := s.heldTokenLocked(workflowID)
	if held == 0 {
		return nil // this process holds no claim on this workflow → nothing to fence.
	}
	var current FencingToken
	err := tx.QueryRowContext(ctx, `SELECT fencing_token FROM leases WHERE workflow_id = ?`, workflowID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// We believe we hold a token but the lease row is gone (Released/deleted). Treat as
		// fenced — our claim is no longer authoritative.
		return ErrFencedOut
	case err != nil:
		return err
	case held < current:
		return ErrFencedOut // a re-claim superseded us; this write is a zombie write — reject it.
	}
	// Still current: renew the lease expiry under our token (renew-on-checkpoint, DEC-M16-D4).
	// Expiry is read through the injected clock (liveness). A failure here is surfaced; the write
	// txn will roll back.
	if _, err := tx.ExecContext(ctx,
		`UPDATE leases SET expiry = ? WHERE workflow_id = ? AND fencing_token = ?`,
		s.leaseExpiryNanos(), workflowID, held,
	); err != nil {
		return err
	}
	return nil
}

// defaultLeaseTTL is the lease duration when none is configured. A placeholder for ph74/ph75
// (the store foundation + claim core); ph76 owns the real production TTL/renewal-cadence policy.
// Modest so a stalled worker's lease lapses in bounded time for the liveness heuristic.
const defaultLeaseTTL = 30 * time.Second

// nowNanos reads "now" through the injected clock (LIVENESS only, DEC-M16-D3).
func (s *SQLiteStore) nowNanos() int64 { return s.clock.Now().UnixNano() }

// leaseExpiryNanos is now + the lease TTL, in unix-nanos. The lease deadline (liveness).
func (s *SQLiteStore) leaseExpiryNanos() int64 { return s.nowNanos() + s.leaseTTL.Nanoseconds() }

// Compile-time assertion that *SQLiteStore satisfies the M16 ClaimStore contract (ph74 defined
// the interface; ph75 implements the policy, so the assertion is now real).
var _ ClaimStore = (*SQLiteStore)(nil)

// Claim implements ClaimStore: acquire — or re-claim a lapsed — durable lease for workflowID on
// behalf of ownerID, returning a fresh monotonic FencingToken. The whole read-branch-write is ONE
// BEGIN IMMEDIATE txn (mp DSN), so N contenders SERIALIZE on the write lock and exactly one wins
// (the z4 arbiter — the atomic INSERT-or-CAS, not a read-then-write race). Requires mp mode.
func (s *SQLiteStore) Claim(workflowID, ownerID string) (FencingToken, error) {
	if !s.dur.mp {
		return 0, fmt.Errorf("%w: Claim requires a multi-process store (WithMultiProcess)", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return 0, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE (mp DSN) → the write lock is held across this txn
	if err != nil {
		// Claim is the BUSIEST cross-process contention point (every worker claims; all serialize on
		// the one write lock). A loser that waits out busy_timeout surfaces SQLITE_BUSY here → classify
		// as ErrBusy (transient, retryable) so a host can retry the claim rather than treat it as fatal
		// (review ph77-F1). No token was set → nothing to unwind.
		return 0, classifyTxErr("claim begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	var (
		curOwner  string
		curExpiry int64
		curToken  FencingToken
	)
	scanErr := tx.QueryRowContext(ctx,
		`SELECT owner_id, expiry, fencing_token FROM leases WHERE workflow_id = ?`, workflowID,
	).Scan(&curOwner, &curExpiry, &curToken)

	now := s.nowNanos()
	var token FencingToken
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		// z4: never-claimed workflow → INSERT with token 1.
		token = 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES (?,?,?,?)`,
			workflowID, ownerID, s.leaseExpiryNanos(), int64(token)); err != nil {
			return 0, fmt.Errorf("%w: claim insert: %w", ErrIO, err)
		}
	case scanErr != nil:
		return 0, fmt.Errorf("%w: claim read: %w", ErrIO, scanErr)
	case now >= curExpiry:
		// Lease LAPSED → re-claim: bump the token (this is what FENCES the old owner) + take it.
		token = curToken + 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET owner_id=?, expiry=?, fencing_token=? WHERE workflow_id=?`,
			ownerID, s.leaseExpiryNanos(), int64(token), workflowID); err != nil {
			return 0, fmt.Errorf("%w: claim reclaim: %w", ErrIO, err)
		}
	case curOwner == ownerID:
		// LIVE lease held by US (re-entrant) → idempotent: keep the token, refresh expiry.
		token = curToken
		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET expiry=? WHERE workflow_id=?`, s.leaseExpiryNanos(), workflowID); err != nil {
			return 0, fmt.Errorf("%w: claim refresh: %w", ErrIO, err)
		}
	default:
		// LIVE lease held by ANOTHER owner → do NOT steal it.
		return 0, ErrClaimLost
	}

	if err := tx.Commit(); err != nil {
		return 0, classifyTxErr("claim commit", err) // SQLITE_BUSY at commit → ErrBusy (transient); else ErrIO.
	}
	committed = true
	s.tokenState[workflowID] = token // in-process token for the checkpoint CAS (mu held)
	return token, nil
}

// Renew implements ClaimStore: bump the lease expiry under the held token. ErrFencedOut if a
// re-claim superseded it (0 rows matched the token). Liveness; expiry read through the clock.
func (s *SQLiteStore) Renew(workflowID string, token FencingToken) error {
	if !s.dur.mp {
		return fmt.Errorf("%w: Renew requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE leases SET expiry=? WHERE workflow_id=? AND fencing_token=?`,
		s.leaseExpiryNanos(), workflowID, int64(token))
	if err != nil {
		// This auto-commit UPDATE acquires the write lock itself; a loser past busy_timeout surfaces
		// SQLITE_BUSY → ErrBusy (transient). A host that keys abort on ErrFencedOut must be able to
		// distinguish a merely-contended renew (retry, keep the live lease) from a hard fault (review
		// ph77-F1). ErrBusy stays errors.Is-distinct from ErrFencedOut.
		return classifyTxErr("renew", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: renew rows: %w", ErrIO, err)
	}
	if n == 0 {
		return ErrFencedOut // the token was superseded (re-claim bumped it) → drive must abort.
	}
	return nil
}

// Release implements ClaimStore: drop the lease iff the caller still holds token (best-effort
// clean handoff so a successor need not wait for expiry) + clear this process's tokenState. A
// no-op DELETE (already superseded) is fine — the successor already owns a higher token.
func (s *SQLiteStore) Release(workflowID string, token FencingToken) error {
	if !s.dur.mp {
		return fmt.Errorf("%w: Release requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM leases WHERE workflow_id=? AND fencing_token=?`, workflowID, int64(token)); err != nil {
		return classifyTxErr("release", err) // SQLITE_BUSY → ErrBusy (transient, retryable); else ErrIO.
	}
	delete(s.tokenState, workflowID)
	return nil
}
