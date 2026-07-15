package workflow

// M17 ph80 — the work-queue substrate proven at the STORE level: Enqueue idempotency, the atomic
// ClaimNext (FIFO, type filter, ErrNoWork, no-reclaim-of-terminal), CAS-guarded terminal transitions.
// The cross-process no-double-claim hard bar is the real-2-OS-proc test at the bottom (reusing the
// M16 re-exec harness). Every CONTEXT seed-break is bite-proven here (neuter → RED → restore is run
// in the fix-tail; these tests assert the GUARDED behavior + the detectable no-ops the guards yield).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func mkWQStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "wq.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// wqState reads a row's state (test helper).
func wqState(t *testing.T, s *SQLiteStore, wf string) string {
	t.Helper()
	var st string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM work_queue WHERE workflow_id=?`, wf).Scan(&st))
	return st
}

// TestEnqueue_IdempotentDetectable — a first submit queues (true); a re-submit of a live OR terminal
// id is a VISIBLE no-op (false), one row, the existing row unchanged (DEC-M17-REENQUEUE).
func TestEnqueue_IdempotentDetectable(t *testing.T) {
	s := mkWQStore(t)

	q, err := s.Enqueue("wf1", "typeA", []byte("first"))
	require.NoError(t, err)
	require.True(t, q, "first submit queues")

	// re-submit a PENDING id → no-op, row unchanged (input stays "first").
	q, err = s.Enqueue("wf1", "typeA", []byte("SECOND-should-not-land"))
	require.NoError(t, err)
	require.False(t, q, "re-submit of a pending id is a detectable no-op")
	var input string
	require.NoError(t, s.db.QueryRow(`SELECT input FROM work_queue WHERE workflow_id='wf1'`).Scan(&input))
	require.Equal(t, "first", input, "the existing row is unchanged — not a silent overwrite")

	// one row total.
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE workflow_id='wf1'`).Scan(&n))
	require.Equal(t, 1, n)

	// drive it to a terminal state, re-submit → STILL a no-op, stays terminal (a failed id stays failed).
	_, err = s.ClaimNext("owner")
	require.NoError(t, err)
	flipped, err := s.MarkFailed("wf1")
	require.NoError(t, err)
	require.True(t, flipped)
	q, err = s.Enqueue("wf1", "typeA", []byte("resurrect?"))
	require.NoError(t, err)
	require.False(t, q, "re-submit of a FAILED id is a no-op — never resurrects it")
	require.Equal(t, wqFailed, wqState(t, s, "wf1"), "a failed id STAYS failed")
}

// TestClaimNext_FIFO_TypeFilter_ErrNoWork — claims are oldest-first (FIFO by enqueued_at) within the
// type filter; an empty/no-match scan returns ErrNoWork.
func TestClaimNext_FIFO_TypeFilter(t *testing.T) {
	s := mkWQStore(t)
	// enqueue in a known order (enqueued_at is unix-nanos; space them so ordering is deterministic).
	for _, e := range []struct{ id, typ string }{
		{"a", "T1"}, {"b", "T2"}, {"c", "T1"}, {"d", "T2"},
	} {
		q, err := s.Enqueue(e.id, e.typ, nil)
		require.NoError(t, err)
		require.True(t, q)
		time.Sleep(time.Millisecond) // ensure distinct enqueued_at
	}

	// type-filtered FIFO: T1 → a then c.
	it, err := s.ClaimNext("owner", "T1")
	require.NoError(t, err)
	require.Equal(t, "a", it.WorkflowID, "oldest T1 first")
	require.Equal(t, "T1", it.Type)
	it, err = s.ClaimNext("owner", "T1")
	require.NoError(t, err)
	require.Equal(t, "c", it.WorkflowID, "next-oldest T1")

	// T1 exhausted → ErrNoWork (b, d are T2, filtered out).
	_, err = s.ClaimNext("owner", "T1")
	require.ErrorIs(t, err, ErrNoWork, "no more T1 work → ErrNoWork")

	// no filter → the oldest remaining across types (b, then d).
	it, err = s.ClaimNext("owner")
	require.NoError(t, err)
	require.Equal(t, "b", it.WorkflowID)
}

// TestClaimNext_EmptyQueue_ErrNoWork — an empty queue → ErrNoWork (distinct from a fault).
func TestClaimNext_EmptyQueue_ErrNoWork(t *testing.T) {
	s := mkWQStore(t)
	_, err := s.ClaimNext("owner")
	require.ErrorIs(t, err, ErrNoWork)
	require.NotErrorIs(t, err, ErrIO, "ErrNoWork is not a fault — a poller sleeps, doesn't retry-as-error")
}

// TestClaimNext_NoReclaimOfTerminal (C2 — the infinite-reclaim guard) — a done/failed/cancelled row is
// NEVER returned by ClaimNext. THE load-bearing guard is the `WHERE state='pending'` scan filter.
func TestClaimNext_NoReclaimOfTerminal(t *testing.T) {
	s := mkWQStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	it, err := s.ClaimNext("owner")
	require.NoError(t, err)
	require.Equal(t, "wf", it.WorkflowID)
	flipped, err := s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped)

	// the row is `done` — ClaimNext must NOT return it (C2). Seed-break: dropping `WHERE state='pending'`
	// from the scan would re-claim this terminal row → this ErrNoWork assertion reddens.
	_, err = s.ClaimNext("owner")
	require.ErrorIs(t, err, ErrNoWork, "a terminal (done) row is NEVER re-claimed — the C2 guard")
}

// TestTerminalTransitions_CASGuarded — a claimed→terminal flip succeeds once; a SECOND flip of an
// already-terminal row is a detectable 0-row no-op (the CAS `WHERE state=<expected>` guard). Also:
// MarkDone on a PENDING (not-yet-claimed) row is a no-op (only claimed→done is valid).
func TestTerminalTransitions_CASGuarded(t *testing.T) {
	s := mkWQStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)

	// MarkDone before claim → no-op (state is pending, not claimed).
	flipped, err := s.MarkDone("wf")
	require.NoError(t, err)
	require.False(t, flipped, "MarkDone on a pending (unclaimed) row is a 0-row no-op")

	_, err = s.ClaimNext("owner")
	require.NoError(t, err)

	// first claimed→done: flips.
	flipped, err = s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "claimed→done flips once")

	// SECOND done flip: 0-row (already done) → the CAS guard bites. Seed-break: an unconditional flip
	// (drop `WHERE state='claimed'`) would allow this double-flip → this False assertion reddens.
	flipped, err = s.MarkDone("wf")
	require.NoError(t, err)
	require.False(t, flipped, "a second claimed→done flip is a detectable 0-row no-op (CAS-guarded)")

	// MarkFailed on the now-done row: also 0-row (not claimed anymore).
	flipped, err = s.MarkFailed("wf")
	require.NoError(t, err)
	require.False(t, flipped, "a done row cannot be flipped to failed")
}

// TestCancelPending_RejectsClaimed (DEC-M17-CANCEL) — cancel accepts ONLY a pending row; a cancel of a
// CLAIMED (running) row is REJECTED as a detectable 0-row no-op.
func TestCancelPending_RejectsClaimed(t *testing.T) {
	s := mkWQStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)

	// cancel a PENDING row: succeeds.
	flipped, err := s.CancelPending("wf")
	require.NoError(t, err)
	require.True(t, flipped, "a pending row cancels")
	require.Equal(t, wqCancelled, wqState(t, s, "wf"))
	// and a cancelled row is not claimable (C2).
	_, err = s.ClaimNext("owner")
	require.ErrorIs(t, err, ErrNoWork)

	// now the claimed-rejection: a fresh claimed row cannot be cancelled.
	_, err = s.Enqueue("wf2", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("owner")
	require.NoError(t, err)
	flipped, err = s.CancelPending("wf2")
	require.NoError(t, err)
	require.False(t, flipped, "cancel of a CLAIMED (running) row is REJECTED — a detectable no-op (DEC-M17-CANCEL)")
	require.Equal(t, wqClaimed, wqState(t, s, "wf2"), "the claimed row keeps running — not interrupted")
}

// ---- the real-2-OS-process no-double-claim hard bar (reuses the M16 re-exec harness pattern) ----

// TestClaimNextWorkerEntry — the subprocess worker: opens its own mp *sql.DB, loops ClaimNext until
// ErrNoWork, and prints each claimed workflow_id (one per line, prefixed) so the parent can collect
// the union across processes and assert DISTINCT (no double-claim) + complete (no lost work).
func TestClaimNextWorkerEntry(t *testing.T) {
	if os.Getenv("M17_WQ_WORKER") == "" {
		t.Skip("not a work-queue worker invocation")
	}
	dbPath := os.Getenv("M17_WQ_DB")
	owner := os.Getenv("M17_WQ_OWNER")
	s, err := NewSQLiteStore(dbPath, WithMultiProcess())
	if err != nil {
		t.Fatalf("wq worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup
	nonAtomic := os.Getenv("M17_WQ_NONATOMIC") == "1"
	for {
		var it WorkItem
		var err error
		if nonAtomic {
			it, err = nonAtomicClaimNext(s, owner) // SEED-BREAK: scan + claim/flip in SEPARATE txns.
		} else {
			it, err = s.ClaimNext(owner) // production: ONE IMMEDIATE txn.
		}
		if errors.Is(err, ErrNoWork) {
			return
		}
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrClaimLost) {
			continue // transient contention — retry the scan.
		}
		if err != nil {
			t.Fatalf("wq worker %s ClaimNext: %v", owner, err)
		}
		// CLAIMED= marker the parent greps; then mark it done so it leaves the pending set. Under the
		// non-atomic seed a DOUBLE-claimed id is marked done twice — the parent's distinct-union check
		// catches the double-claim (>K total CLAIMED lines / a repeated id).
		t.Logf("CLAIMED=%s", it.WorkflowID)
		_, _ = s.MarkDone(it.WorkflowID) //nolint:errcheck // seed-break tolerates a 0-row done on a doubly-claimed id
	}
}

// nonAtomicClaimNext is the SEED-BREAK for the no-double-claim hard bar: it does the scan in ONE txn,
// then the claim+flip in a SEPARATE txn — so two OS procs can both scan the SAME pending row before
// either flips it, and BOTH win it (a double-claim). The production ClaimNext does all three in ONE
// BEGIN IMMEDIATE txn precisely to preclude this. Test-only (a `_test.go` helper, never shippable).
func nonAtomicClaimNext(s *SQLiteStore, owner string) (WorkItem, error) {
	ctx := context.Background()
	// txn 1: scan only (read a pending id, then COMMIT — releasing the write lock, opening the race).
	s.mu.Lock()
	var wf, typ string
	scanErr := s.db.QueryRowContext(ctx,
		`SELECT workflow_id, type FROM work_queue WHERE state='pending' ORDER BY enqueued_at LIMIT 1`).Scan(&wf, &typ)
	s.mu.Unlock()
	if errors.Is(scanErr, sql.ErrNoRows) {
		return WorkItem{}, ErrNoWork
	}
	if scanErr != nil {
		return WorkItem{}, scanErr
	}
	// Widen the race window deterministically: both procs scan the SAME oldest pending row, then pause
	// here (write lock released) — so the peer scans the same row before EITHER claims it. This makes
	// the double-claim near-certain, which is the whole point of the seed-break (a narrow real race is
	// flaky to catch; the sleep makes the atomicity's load-bearing role unambiguous).
	time.Sleep(15 * time.Millisecond)
	// Claim via the BROKEN non-atomic claim (M16's nonAtomicClaim: read-then-write in SEPARATE txns with
	// an ON CONFLICT DO UPDATE bump, so BOTH racers "win" a token) INSTEAD of the atomic claimLocked.
	// This is the honest "make the claim non-atomic" seed: it bypasses BOTH the queue's single-txn AND
	// the M16 lease arbiter (which, being atomic on its own, would otherwise catch the double-claim via
	// ErrClaimLost — the deep reason a merely-non-atomic-QUEUE path can't double-claim). Then flip
	// unconditionally. Both procs return the SAME wf as "claimed" → the cross-process double-claim.
	token, cerr := nonAtomicClaim(s, wf, owner)
	if cerr != nil {
		return WorkItem{}, cerr
	}
	s.mu.Lock()
	_, _ = s.db.ExecContext(ctx, `UPDATE work_queue SET state='claimed' WHERE workflow_id=?`, wf) //nolint:errcheck
	s.tokenState[wf] = token
	s.mu.Unlock()
	return WorkItem{WorkflowID: wf, Type: typ, Token: token}, nil
}

// TestClaimNext_TwoProc_NoDoubleClaim — the hard bar: pre-enqueue K pending rows, spawn 2 REAL OS
// processes each looping ClaimNext to exhaustion, collect every claimed id → assert the union is
// DISTINCT (no id claimed by both procs — no double-claim, the IMMEDIATE txn + PK arbitrate) AND
// covers all K (no lost work). This is the cross-process arbitration s.mu cannot provide.
func TestClaimNext_TwoProc_NoDoubleClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses; skipped under -short")
	}
	const k = 30

	// race spawns 2 OS procs each looping ClaimNext to exhaustion over a fresh K-row queue, and returns
	// how many DISTINCT ids were claimed + the TOTAL claim count. Atomic: distinct==total==K. The
	// non-atomic seed-break: scan+claim in separate txns → the same row double-claimed → total > distinct.
	race := func(nonAtomic bool) (distinct, total int, dump string) {
		db := filepath.Join(t.TempDir(), "wq2proc.db")
		pre, err := NewSQLiteStore(db, WithMultiProcess())
		require.NoError(t, err)
		for i := 0; i < k; i++ {
			q, err := pre.Enqueue("wf"+strconv.Itoa(i), "T", nil)
			require.NoError(t, err)
			require.True(t, q)
		}
		require.NoError(t, pre.Close())

		spawn := func(owner string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestClaimNextWorkerEntry$", "-test.v")
			cmd.Env = append(os.Environ(), "M17_WQ_WORKER=1", "M17_WQ_DB="+db, "M17_WQ_OWNER="+owner)
			if nonAtomic {
				cmd.Env = append(cmd.Env, "M17_WQ_NONATOMIC=1")
			}
			return cmd
		}
		var wg sync.WaitGroup
		outs := make([][]byte, 2)
		for i, owner := range []string{"P0", "P1"} {
			wg.Add(1)
			go func(i int, owner string) {
				defer wg.Done()
				out, _ := spawn(owner).CombinedOutput() //nolint:errcheck // the worker exits 0 on ErrNoWork; a real failure surfaces as a t.Fatalf in `out`, which the parent's CLAIMED-count assertions catch
				outs[i] = out
			}(i, owner)
		}
		wg.Wait()

		seen := map[string]int{}
		for _, out := range outs {
			for _, line := range strings.Split(string(out), "\n") {
				if idx := strings.Index(line, "CLAIMED="); idx >= 0 {
					seen[strings.TrimSpace(line[idx+len("CLAIMED="):])]++
					total++
				}
			}
		}
		return len(seen), total, string(outs[0]) + string(outs[1])
	}

	// ATOMIC (production): each id claimed EXACTLY once — no double-claim, no lost work.
	distinct, total, dump := race(false)
	require.Equal(t, k, distinct, "all %d rows claimed (no lost work):\n%s", k, dump)
	require.Equal(t, k, total, "exactly K claims total — no id claimed twice across the 2 procs:\n%s", dump)

	// SEED-BREAK (non-atomic scan+claim in separate txns): a double-claim MUST appear cross-process →
	// total > distinct (the same id claimed by both procs). This proves the single IMMEDIATE txn is the
	// load-bearing thing preventing double-claim — remove the atomicity and the hard bar reddens.
	sbDistinct, sbTotal, sbDump := race(true)
	require.Greater(t, sbTotal, sbDistinct,
		"SEED-BREAK: non-atomic scan+claim MUST double-claim at least one row cross-process (total %d > distinct %d):\n%s",
		sbTotal, sbDistinct, sbDump)
}
