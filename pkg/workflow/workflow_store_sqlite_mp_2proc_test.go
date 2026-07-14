package workflow

// ph74 hard-bar: TWO REAL OS PROCESSES issuing contended BEGIN IMMEDIATE writes against one DB
// file must SERIALIZE (no deadlock, no instant `database is locked (5)`). This is what C1
// (_txlock=immediate) + C2 (busy_timeout PRAGMA) buy. We use the re-exec-the-test-binary pattern
// (a subprocess of `go test` itself) to get genuine separate *sql.DB opens in separate OS
// processes — SetMaxOpenConns(1) is in-process-only and would prove nothing here.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMain-less subprocess entry: a test that, when M16_MP_WORKER is set, acts as a worker
// (opens the DB in mp mode and hammers Save on its own workflow), then exits. The parent
// TestMPTwoProc_NoDeadlock spawns two of these.
func TestMPWorkerEntry(t *testing.T) {
	if os.Getenv("M16_MP_WORKER") == "" {
		t.Skip("not a worker invocation (set M16_MP_WORKER to run as the subprocess worker)")
	}
	dbPath := os.Getenv("M16_MP_DB")
	wf := os.Getenv("M16_MP_WF")
	iters, _ := strconv.Atoi(os.Getenv("M16_MP_ITERS")) //nolint:errcheck // test env; 0 on parse-fail is fine

	s, err := NewSQLiteStore(dbPath, WithMultiProcess())
	if err != nil {
		t.Fatalf("worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	// Each worker drives its OWN distinct workflow — legitimate competing consumers, maximizing
	// cross-process WRITE-LOCK contention (both hit the same DB file's single writer) without any
	// fencing noise (distinct workflows never fence each other).
	for i := 0; i < iters; i++ {
		d := NewWorkflowData(wf)
		d.SetNodeStatus("n"+strconv.Itoa(i), Completed)
		if err := s.Save(d); err != nil {
			t.Fatalf("worker %s Save iter %d: %v", wf, i, err) // ErrBusy/deadlock/instant-lock all fail here
		}
	}
}

// TestMPTwoProc_NoDeadlock — two OS-process workers hammer Save on one DB concurrently and both
// complete (writes serialize under the write lock + busy_timeout). This exercises the WRITE-FIRST
// workload: each worker holds NO token, so checkFencingLocked returns at held==0 BEFORE its SELECT,
// making Save a pure write-first txn. HONEST SCOPE (F-M16-P74-QA-1): this workload does NOT bite C1
// — qa forced DEFERRED and it still passed, because write-first has no read-then-upgrade to deadlock
// on. It proves 2 procs don't deadlock on the write-first path, NOT that BEGIN IMMEDIATE is active.
// The biting C1 witness — the SELECT-then-write (read fencing_token, then UPSERT) path the fencing
// CAS actually performs — is TestMPTwoProc_C1SelectThenWrite_Bites, which DOES redden under DEFERRED.
func TestMPTwoProc_NoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses; skipped under -short")
	}
	db := filepath.Join(t.TempDir(), "mp2.db")
	// Pre-create the schema ONCE (C3): the parent opens mp once so both workers open a DB that
	// already has the schema (concurrent DDL-on-open would itself contend — a ph73 finding).
	pre, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	_ = pre.Close() //nolint:errcheck // pre-create schema only; close error is not material here

	spawn := func(wf string) *exec.Cmd {
		// Re-exec THIS test binary, running only the worker entry test, with the worker env set.
		cmd := exec.Command(os.Args[0], "-test.run", "^TestMPWorkerEntry$", "-test.v")
		cmd.Env = append(os.Environ(),
			"M16_MP_WORKER=1",
			"M16_MP_DB="+db,
			"M16_MP_WF="+wf,
			"M16_MP_ITERS=50",
		)
		return cmd
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	outs := make([][]byte, 2)
	for i, wf := range []string{"wfP1", "wfP2"} {
		wg.Add(1)
		go func(i int, wf string) {
			defer wg.Done()
			out, err := spawn(wf).CombinedOutput()
			outs[i], errs[i] = out, err
		}(i, wf)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d FAILED (deadlock/instant-lock/busy under 2-proc write contention): %v\n%s", i, errs[i], outs[i])
		}
	}
	t.Log("2 real OS processes issued contended write-first Saves → both completed (no deadlock). " +
		"NOTE: this write-first workload does not bite C1 (see TestMPTwoProc_C1SelectThenWrite_Bites for the SELECT-then-write witness).")
}

// TestMPC1WorkerEntry is the BITING worker subprocess (F-M16-P74-QA-1): it performs the SELECT-then-write
// transaction the fencing CAS actually does (read leases.fencing_token, THEN UPSERT a row) on the
// SAME workflow both procs contend on. Its txlock is env-controlled so the parent can run it under
// IMMEDIATE (both serialize + succeed) AND forced DEFERRED (two read locks both upgrading → the
// classic SQLite write-write deadlock → `database is locked (5)`, busy_timeout does not save it).
func TestMPC1WorkerEntry(t *testing.T) {
	if os.Getenv("M16_C1_WORKER") == "" {
		t.Skip("not a C1 worker invocation")
	}
	dbPath := os.Getenv("M16_C1_DB")
	txlock := os.Getenv("M16_C1_TXLOCK")                // "immediate" | "deferred"
	iters, _ := strconv.Atoi(os.Getenv("M16_C1_ITERS")) //nolint:errcheck // test env

	// Open a RAW handle with the requested txlock (the test controls it to force DEFERRED; the
	// production store always uses immediate in mp mode — this is a store-substrate witness).
	db, err := sql.Open("sqlite", dbPath+"?_txlock="+txlock) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("c1 worker open: %v", err)
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // worker cleanup
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=3000"); err != nil { // C2: give the loser a bounded wait
		t.Fatalf("busy_timeout: %v", err)
	}

	ctx := context.Background()
	const wf = "c1shared" // BOTH procs contend on the SAME workflow's lease row (max read→write contention)
	for i := 0; i < iters; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("c1 begin iter %d: %v", i, err)
		}
		// SELECT first (a read) — the fencing CAS's fencing_token read.
		var tok int64
		row := tx.QueryRowContext(ctx, `SELECT fencing_token FROM leases WHERE workflow_id=?`, wf)
		_ = row.Scan(&tok) //nolint:errcheck // no-row is fine (first iter); the READ is what matters for the lock
		// THEN write (an UPSERT) — the read→write upgrade DEFERRED can deadlock on.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES (?,?,?,?)
			 ON CONFLICT(workflow_id) DO UPDATE SET fencing_token=fencing_token+1`,
			wf, os.Getenv("M16_C1_OWNER"), int64(0), tok+1); err != nil {
			_ = tx.Rollback()                                            //nolint:errcheck // best-effort rollback on the error path
			t.Fatalf("c1 write iter %d (txlock=%s): %v", i, txlock, err) // SQLITE_BUSY/deadlock fails here
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("c1 commit iter %d (txlock=%s): %v", i, txlock, err)
		}
	}
}

// TestMPTwoProc_C1SelectThenWrite_Bites — the biting C1 witness. Two OS processes each run the
// SELECT-then-write txn on the SAME lease row. Under IMMEDIATE they SERIALIZE + both succeed; under
// forced DEFERRED they DEADLOCK on the read→write upgrade (`database is locked (5)`), so C1 is now
// self-witnessing: the seed-break (DEFERRED) reddens, proving _txlock=immediate is load-bearing.
func TestMPTwoProc_C1SelectThenWrite_Bites(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses; skipped under -short")
	}
	db := filepath.Join(t.TempDir(), "c1.db")
	pre, err := NewSQLiteStore(db, WithMultiProcess()) // create the schema once (C3)
	if err != nil {
		t.Fatal(err)
	}
	_ = pre.Close() //nolint:errcheck // schema pre-create only

	run := func(txlock string) (ok bool, detail string) {
		spawn := func(owner string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestMPC1WorkerEntry$")
			cmd.Env = append(os.Environ(),
				"M16_C1_WORKER=1", "M16_C1_DB="+db, "M16_C1_TXLOCK="+txlock,
				"M16_C1_OWNER="+owner, "M16_C1_ITERS=40")
			return cmd
		}
		var wg sync.WaitGroup
		res := make([]error, 2)
		out := make([][]byte, 2)
		for i, owner := range []string{"A", "B"} {
			wg.Add(1)
			go func(i int, owner string) {
				defer wg.Done()
				o, e := spawn(owner).CombinedOutput()
				out[i], res[i] = o, e
			}(i, owner)
		}
		wg.Wait()
		for i := range res {
			if res[i] != nil {
				return false, string(out[i])
			}
		}
		return true, ""
	}

	// IMMEDIATE (the production mp path): both SELECT-then-write workers must SERIALIZE + succeed.
	okImm, detImm := run("immediate")
	if !okImm {
		t.Fatalf("C1 WITNESS FAIL: under _txlock=immediate the SELECT-then-write workers should serialize, but one failed:\n%s", detImm)
	}
	t.Log("immediate: 2-proc SELECT-then-write serialized + both succeeded (C1 correct)")

	// DEFERRED (the seed-break): the read→write upgrade MUST deadlock/SQLITE_BUSY — proving C1 bites.
	okDef, _ := run("deferred")
	if okDef {
		t.Fatal("C1 SEED-BREAK FAIL: forced DEFERRED did NOT redden the SELECT-then-write path — C1 (_txlock=immediate) is not self-witnessing here")
	}
	t.Log("deferred (seed-break): 2-proc SELECT-then-write DEADLOCKED (database is locked) — C1 bites; _txlock=immediate is load-bearing + necessary")
}
