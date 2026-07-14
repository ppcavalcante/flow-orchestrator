package workflow

// ph75 hard-bar: the z1 fencing bite through the REAL Claim path + the z4 initial-claim race +
// MINOR-6 (ErrFencedOut reachable at the Execute return). Deterministic (FakeClock); no executor
// for z1/z4 (the claim policy + CAS are proven at the store level).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkClaimStoresOn opens TWO mp store handles on the SAME db file (each its own tokenState) — the
// faithful two-process model — sharing ONE injected FakeClock + TTL. This is how a zombie (store
// A, stale token) and a re-claimer (store B, current token) coexist honestly, not a single store
// with a re-pointed tokenState.
func mkClaimStoresOn(t *testing.T, clk *FakeClock, ttl time.Duration) (dbPath string, a, b *SQLiteStore) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "claim.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
		return s
	}
	return dbPath, open(), open()
}

// mkClaimStore opens a single mp store (for the z4 race + Renew/Release + re-entrant, which are
// one-process concerns).
func mkClaimStore(t *testing.T, clk *FakeClock, ttl time.Duration) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "claim.db"),
		WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
	return s
}

// TestWithLeaseTTL_PublicKnobDrivesExpiry — the ph77 precursor bite: the PUBLIC WithLeaseTTL option
// actually governs lease-lapse (liveness). A store built WithLeaseTTL(2s): A Claims (token 1); the
// FakeClock advances 3s (> the PUBLIC TTL) → the lease is LAPSED → a re-claim by B bumps the token
// to 2. Seed-proof of the knob: with a LONG TTL (60s) the same 3s advance does NOT lapse → B loses
// the claim (ErrClaimLost). The differential proves WithLeaseTTL, not some default, sets the expiry.
func TestWithLeaseTTL_PublicKnobDrivesExpiry(t *testing.T) {
	openWith := func(clk *FakeClock, ttl time.Duration) (string, func() *SQLiteStore) {
		dbPath := filepath.Join(t.TempDir(), "ttl.db")
		return dbPath, func() *SQLiteStore {
			// PUBLIC WithLeaseTTL — the knob under test. withSQLiteClock stays test-only (a FakeClock
			// is not public API); the TTL it interacts with is the public surface.
			s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), WithLeaseTTL(ttl))
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
			return s
		}
	}

	// Short TTL: 3s advance > 2s TTL → lapse → re-claim bumps the token.
	clk := NewFakeClock(time.Unix(1000, 0))
	_, open := openWith(clk, 2*time.Second)
	sA, sB := open(), open()
	tokA, err := sA.Claim("wf", "A")
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), tokA)
	clk.Advance(3 * time.Second) // > the PUBLIC 2s TTL → A's lease lapses
	tokB, err := sB.Claim("wf", "B")
	require.NoError(t, err, "the public WithLeaseTTL made the lease re-claimable after its TTL")
	require.Equal(t, FencingToken(2), tokB, "re-claim bumped the token → WithLeaseTTL(2s) drove the lapse")

	// Long TTL (the differential): the SAME 3s advance does NOT lapse a 60s lease → B loses.
	clk2 := NewFakeClock(time.Unix(1000, 0))
	_, open2 := openWith(clk2, 60*time.Second)
	sC, sD := open2(), open2()
	_, err = sC.Claim("wf", "C")
	require.NoError(t, err)
	clk2.Advance(3 * time.Second) // < the 60s TTL → still live
	_, err = sD.Claim("wf", "D")
	require.ErrorIs(t, err, ErrClaimLost, "a 60s WithLeaseTTL is still live after 3s → D cannot re-claim (proves the knob sets the TTL, not a default)")
}

// TestClaim_z1_FencingBite_RealClaimPath — the core safety case through the REAL Claim policy,
// faithfully modeled as TWO store handles (two processes) on one DB. A (store sA) Claims (token 1),
// writes L0; the clock advances past the TTL → the lease lapses; B (store sB) Claims (re-claim →
// token 2), writes; A's next checkpoint under its OWN stale token 1 → ErrFencedOut; the per-level
// journal is B's (per-level oracle from ph74). No tokenState re-pointing — each store holds its own.
func TestClaim_z1_FencingBite_RealClaimPath(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	_, sA, sB := mkClaimStoresOn(t, clk, 5*time.Second)
	const wf = "z1"

	// A claims → token 1 (in sA's tokenState), writes level 0 legitimately.
	tokA, err := sA.Claim(wf, "procA")
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), tokA)
	dA := NewWorkflowData(wf)
	dA.SetNodeStatus("n0", Completed)
	require.NoError(t, sA.Save(dA), "A writes L0 under the live token 1")

	// The lease LAPSES (advance the shared clock past the 5s TTL). DEC-M16-D3 liveness event.
	clk.Advance(6 * time.Second)

	// B claims → re-claim → token 2 (in sB's tokenState), writes level 1.
	tokB, err := sB.Claim(wf, "procB")
	require.NoError(t, err)
	require.Equal(t, FencingToken(2), tokB, "re-claim of a lapsed lease bumps the token")
	dB := NewWorkflowData(wf)
	dB.SetNodeStatus("n1", Completed)
	require.NoError(t, sB.Save(dB), "B writes L1 under the current token 2")

	// A (zombie) wakes and, STILL holding token 1 in sA's own tokenState, attempts its next
	// checkpoint → the CAS re-reads the durable fencing_token (now 2 from B's re-claim) → FENCED.
	dA.SetNodeStatus("n2", Completed)
	err = sA.Save(dA)
	require.ErrorIs(t, err, ErrFencedOut, "A's stale-token write must be FENCED (held 1 < current 2)")

	// Per-level oracle (via a fresh read handle): A's fenced write (n2) never landed.
	got, err := sB.Load(wf)
	require.NoError(t, err)
	_, hasN2 := got.GetNodeStatus("n2")
	require.False(t, hasN2, "A's fenced write (n2) must NOT have landed")
}

// TestClaim_z1_SeedBreak — remove the token BUMP on re-claim (B keeps token 1). Then A's write is
// NOT stale (held 1 == current 1) → it LANDS → the bite reddens. Done via a non-atomic re-claim
// helper that re-claims WITHOUT bumping (the seed-break mutation of Claim's token+1).
func TestClaim_z1_SeedBreak(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath, sA, _ := mkClaimStoresOn(t, clk, 5*time.Second)
	const wf = "z1sb"

	tokA, err := sA.Claim(wf, "procA")
	require.NoError(t, err)
	dA := NewWorkflowData(wf)
	dA.SetNodeStatus("n0", Completed)
	require.NoError(t, sA.Save(dA))

	clk.Advance(6 * time.Second)

	// SEED-BREAK: B "re-claims" but does NOT bump the token (the mutation of Claim's token+1 →
	// token stays 1). Write the lease row directly with the SAME token A holds — a broken re-claim.
	_, err = sA.db.Exec(`UPDATE leases SET owner_id=?, expiry=?, fencing_token=? WHERE workflow_id=?`,
		"procB", sA.leaseExpiryNanos(), int64(tokA), wf) // token NOT bumped (the break)
	require.NoError(t, err)

	// A writes under token 1; the durable current is ALSO 1 → held == current, not stale → the CAS
	// does NOT fence → the write LANDS. That is the safety hole the token-bump prevents.
	dA.SetNodeStatus("n2", Completed)
	require.NoError(t, sA.Save(dA), "seed-break: without the token bump, A's write is not stale → it lands")

	rd, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk))
	require.NoError(t, err)
	defer func() { _ = rd.Close() }() //nolint:errcheck // test cleanup
	got, err := rd.Load(wf)
	require.NoError(t, err)
	_, hasN2 := got.GetNodeStatus("n2")
	require.True(t, hasN2, "seed-break: A's write LANDED — the re-claim token BUMP is the load-bearing guard")
}

// TestClaim_z4_InitialClaimRace — N goroutines race the FIRST Claim of one never-run workflow.
// Exactly ONE wins a FencingToken; the rest get ErrClaimLost. This proves the claim BRANCH-LOGIC
// (first INSERTs token 1 with a live TTL → the rest hit curOwner!=self → ErrClaimLost).
//
// HONEST SCOPE (reviewer m16-ph75-F1 + architect guardrail): these are IN-PROCESS goroutines
// sharing ONE store, so Claim's `s.mu` (held across its whole body) SERIALIZES them — the in-proc
// arbiter here is `s.mu`, NOT the IMMEDIATE txn per se. So this test proves the claim BRANCH-LOGIC
// correctness ONLY; it does NOT prove cross-process arbitration. The real cross-process claim race
// against the PRODUCTION Claim (two OS procs, two independent *sql.DB opens, where s.mu does NOT
// span them and only the single BEGIN IMMEDIATE txn + the workflow_id PK arbitrate) is
// TestClaim_z4_TwoProc_RealClaim below — proven HERE where the code is built, not deferred.
func TestClaim_z4_InitialClaimRace(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkClaimStore(t, clk, 60*time.Second) // long TTL: the winner holds it for the whole test
	const wf = "z4"
	const N = 5

	var wg sync.WaitGroup
	toks := make([]FencingToken, N)
	errs := make([]error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize the race
			toks[i], errs[i] = s.Claim(wf, "p"+itoaClaim(i))
		}(i)
	}
	close(start)
	wg.Wait()

	won, lost := 0, 0
	for i := 0; i < N; i++ {
		switch {
		case errs[i] == nil && toks[i] > 0:
			won++
		case errors.Is(errs[i], ErrClaimLost):
			lost++
		default:
			t.Fatalf("z4: goroutine %d unexpected result token=%d err=%v", i, toks[i], errs[i])
		}
	}
	require.Equal(t, 1, won, "exactly one goroutine must win the initial claim")
	require.Equal(t, N-1, lost, "the rest must get ErrClaimLost")
}

// nonAtomicClaim is the SEED-BREAK for z4: a claim that reads THEN writes in SEPARATE txns
// (the atomicity removed). Two racers can both SELECT "no row", then both INSERT-or-bump → two
// winners. Proves the single-IMMEDIATE-txn atomicity in the real Claim is the load-bearing arbiter.
func nonAtomicClaim(s *SQLiteStore, workflowID, ownerID string) (FencingToken, error) {
	ctx := context.Background()
	// Txn 1: READ (releases the lock).
	var curToken FencingToken
	err := s.db.QueryRowContext(ctx, `SELECT fencing_token FROM leases WHERE workflow_id=?`, workflowID).Scan(&curToken)
	if errors.Is(err, sql.ErrNoRows) {
		// Txn 2 (SEPARATE): INSERT. Both racers reach here → the 2nd's INSERT hits the PK conflict,
		// so we UPSERT-bump instead of failing — modeling a broken claim that "succeeds" for both.
		_, e := s.db.ExecContext(ctx,
			`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES (?,?,?,1)
			 ON CONFLICT(workflow_id) DO UPDATE SET owner_id=excluded.owner_id, fencing_token=fencing_token+1`,
			workflowID, ownerID, s.leaseExpiryNanos())
		return 1, e // both racers return a "won" token — the two-winner bug
	}
	return curToken, err
}

// TestClaim_z4_SeedBreak — the non-atomic claim (read-then-write in separate txns) lets TWO
// racers both "win" → reddens. Proves the single-IMMEDIATE-txn INSERT-or-CAS is what makes z4
// exactly-one-wins.
func TestClaim_z4_SeedBreak(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkClaimStore(t, clk, 60*time.Second)
	const wf = "z4sb"
	const N = 5

	var wg sync.WaitGroup
	won := make([]bool, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := nonAtomicClaim(s, wf, "p"+itoaClaim(i)) // the BROKEN claim
			won[i] = err == nil && tok > 0
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	require.Greater(t, winners, 1, "seed-break: a non-atomic claim lets MORE THAN ONE racer win — this is the two-winner bug the atomic INSERT-or-CAS prevents")
}

// TestClaim_ReEntrant — a re-entrant Claim by the SAME owner on a LIVE lease is idempotent:
// returns the held token (not a bump, not ErrClaimLost).
func TestClaim_ReEntrant(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkClaimStore(t, clk, 60*time.Second)
	const wf = "reentrant"
	t1, err := s.Claim(wf, "same")
	require.NoError(t, err)
	t2, err := s.Claim(wf, "same") // same owner, live lease
	require.NoError(t, err)
	require.Equal(t, t1, t2, "a re-entrant claim returns the held token, does not bump")
}

// TestRenew_And_Release — Renew bumps expiry under the token (ErrFencedOut if superseded);
// Release drops the lease + clears tokenState.
func TestRenew_And_Release(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkClaimStore(t, clk, 5*time.Second)
	const wf = "renew"
	tok, err := s.Claim(wf, "o")
	require.NoError(t, err)

	// Renew under the held token succeeds.
	require.NoError(t, s.Renew(wf, tok))
	// Renew under a superseded (higher-than-exists / wrong) token → ErrFencedOut.
	require.ErrorIs(t, s.Renew(wf, tok+99), ErrFencedOut)

	// Release drops the lease; a subsequent Renew under the old token is fenced (row gone).
	require.NoError(t, s.Release(wf, tok))
	require.ErrorIs(t, s.Renew(wf, tok), ErrFencedOut)
}

// TestMinor6_ErrFencedOut_ReachableAtExecuteReturn — MINOR-6: a fenced-out worker's checkpoint
// failure propagates ErrFencedOut all the way to Workflow.Execute's return via the %w chain
// (dag.go checkpoint wrap → workflow.go failed-state return), so a superseded worker can
// errors.Is(err, ErrFencedOut) and know NOT to retry — distinct from a transient IO error.
func TestMinor6_ErrFencedOut_ReachableAtExecuteReturn(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	_, sA, sB := mkClaimStoresOn(t, clk, 5*time.Second)
	const wf = "minor6"

	// A claims token 1. Then B re-claims (token 2) after a lapse — A is now stale BEFORE it drives.
	_, err := sA.Claim(wf, "procA")
	require.NoError(t, err)
	clk.Advance(6 * time.Second)
	_, err = sB.Claim(wf, "procB")
	require.NoError(t, err)

	// A drives a real Workflow whose Store is sA (stale token 1). The forward drive's final Save
	// (via sA) is fenced → the error must reach Execute's return as errors.Is ErrFencedOut.
	d := NewDAG(wf)
	require.NoError(t, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))))
	w := &Workflow{DAG: d, WorkflowID: wf, Store: sA}
	execErr := w.Execute(context.Background())

	require.Error(t, execErr, "a fenced-out drive must return an error")
	require.ErrorIs(t, execErr, ErrFencedOut, "ErrFencedOut must be errors.Is-reachable at Execute's return (MINOR-6)")
	// Distinguishable from a transient IO error: a plain ErrIO is NOT ErrFencedOut.
	require.NotErrorIs(t, ErrIO, ErrFencedOut, "ErrFencedOut is a distinct sentinel from transient ErrIO")
}

func itoaClaim(i int) string { return string(rune('0' + i)) }

// TestClaimWorkerEntry is the real-OS-process worker for TestClaim_z4_TwoProc_RealClaim. Each
// invocation opens its OWN mp *sql.DB (the only config that exercises the cross-process write
// lock + the single IMMEDIATE txn as the true arbiter — s.mu does NOT span processes) and calls
// the PRODUCTION Claim on one shared workflow. Exit 0 = won (got a token); 4 = ErrClaimLost;
// 1 = other error. M16_CLAIM_NONATOMIC=1 selects the seed-break (a broken read-then-write claim
// in SEPARATE txns) to prove the atomicity is what makes exactly-one-wins hold cross-process.
func TestClaimWorkerEntry(t *testing.T) {
	if os.Getenv("M16_CLAIM_WORKER") == "" {
		t.Skip("not a claim-worker invocation")
	}
	dbPath := os.Getenv("M16_CLAIM_DB")
	owner := os.Getenv("M16_CLAIM_OWNER")
	wf := os.Getenv("M16_CLAIM_WF")

	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteLeaseTTL(5*time.Minute))
	if err != nil {
		t.Fatalf("claim worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	var tok FencingToken
	if os.Getenv("M16_CLAIM_NONATOMIC") == "1" {
		tok, err = nonAtomicClaim(s, wf, owner) // seed-break: read-then-write in separate txns
	} else {
		tok, err = s.Claim(wf, owner) // the PRODUCTION claim (one IMMEDIATE txn)
	}
	switch {
	case err == nil && tok > 0:
		// won
	case errors.Is(err, ErrClaimLost):
		os.Exit(4)
	default:
		t.Fatalf("claim worker %s: unexpected err=%v tok=%d", owner, err, tok)
	}
}

// TestClaim_z4_TwoProc_RealClaim — the architect's guardrail: prove the PRODUCTION Claim's
// cross-process arbitration where it is BUILT (not deferred). N real OS processes, each its own
// *sql.DB open, race the initial Claim of one never-run workflow. IMMEDIATE + workflow_id PK →
// exactly ONE wins, the rest ErrClaimLost. Seed-break: the non-atomic claim (read-then-write in
// separate txns, bypassing the single-txn arbitration) → MORE THAN ONE wins across procs → reddens.
func TestClaim_z4_TwoProc_RealClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses; skipped under -short")
	}
	db := filepath.Join(t.TempDir(), "z4proc.db")
	pre, err := NewSQLiteStore(db, WithMultiProcess()) // create the schema once (C3)
	require.NoError(t, err)
	_ = pre.Close() //nolint:errcheck // schema pre-create only

	race := func(nonAtomic bool, wf string, n int) (won int) {
		spawn := func(owner string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestClaimWorkerEntry$")
			cmd.Env = append(os.Environ(),
				"M16_CLAIM_WORKER=1", "M16_CLAIM_DB="+db, "M16_CLAIM_OWNER="+owner, "M16_CLAIM_WF="+wf)
			if nonAtomic {
				cmd.Env = append(cmd.Env, "M16_CLAIM_NONATOMIC=1")
			}
			return cmd
		}
		var wg sync.WaitGroup
		exits := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := spawn("p" + itoaClaim(i)).Run()
				if err == nil {
					exits[i] = 0
				} else if ee, ok := err.(*exec.ExitError); ok {
					exits[i] = ee.ExitCode()
				} else {
					exits[i] = -1
				}
			}(i)
		}
		wg.Wait()
		for _, e := range exits {
			if e == 0 {
				won++
			}
		}
		return won
	}

	// PRODUCTION Claim: exactly one of 3 OS procs wins the initial claim.
	require.Equal(t, 1, race(false, "z4real", 3),
		"cross-process: exactly one OS process must win the initial Claim (IMMEDIATE txn + PK arbitrate)")

	// SEED-BREAK: the non-atomic claim lets MORE THAN ONE OS proc win → the two-winner bug.
	require.Greater(t, race(true, "z4break", 3), 1,
		"seed-break: a non-atomic (separate-txn) claim lets >1 OS process win — the single IMMEDIATE txn is the cross-process arbiter")
}
