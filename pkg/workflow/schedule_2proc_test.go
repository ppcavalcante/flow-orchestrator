package workflow

// M20 ph100 T5 — ⭐ the headline safety property: NO-DOUBLE-FIRE across two REAL OS processes. Two processes each
// run a poller against the same DB with the same due schedule; EXACTLY ONE run is enqueued for the slot. Reuses
// the M16 re-exec-the-test-binary harness (the M16_MP_WORKER pattern). It runs in the BLOCKING CI Test job because
// CI invokes `go test ./... -race` with NO `-short` (ci.yml) → testing.Short() is false → this test is NOT
// skipped in the gate (the short-skip guard only fires for a local `go test -short`).
//
// THREE LAYERED DEFENSES give exactly-once-enqueue (verified empirically at build time — see the OBS note routed
// to the architect):
//   1. The in-txn `next_fire_time > now` RE-CHECK inside the BEGIN IMMEDIATE txn — THE true no-double-fire
//      arbiter. The two processes serialize on the SQLite write lock; the second's txn sees the advanced
//      next_fire → not due → no second enqueue. **Discriminator: drop this re-check → double-fire** (proven: with
//      owner-tagged run-ids defeating layer 3, dropping the re-check enqueues 2).
//   2. The per-schedule FENCE (claimLocked, DEC-P100-FENCE-PER-SCHEDULE) — one-writer-per-schedule +
//      crash-reclaim liveness (a poller that dies mid-fire; its lease lapses; a sibling reclaims). NOT the
//      primary no-double-fire arbiter (the in-txn re-check already is) — it is the liveness/crash layer.
//   3. The DETERMINISTIC per-slot run-id (scheduledRunID = f(scheduleID, slot)) + work_queue ON CONFLICT — a
//      DBOS-style idempotency belt: even a hypothetical double-fire of the SAME slot enqueues ONE row.
// This committed test asserts the user-visible property (exactly 1 enqueue) which holds by all three; the
// build-time discriminator (layer-1 drop, layer-3 defeated) is documented for qa to re-run at GATE-P100-VERIFY.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestSchedulePollerWorkerEntry is the subprocess worker: when SCHED_2PROC_WORKER is set it opens the DB in mp
// mode and repeatedly runs the fire for the one due schedule for a bounded window, then exits. The parent spawns
// two of these against the same DB + due slot.
func TestSchedulePollerWorkerEntry(t *testing.T) {
	if os.Getenv("SCHED_2PROC_WORKER") == "" {
		t.Skip("not a worker invocation (set SCHED_2PROC_WORKER to run as the subprocess poller)")
	}
	dbPath := os.Getenv("SCHED_2PROC_DB")
	schedID := os.Getenv("SCHED_2PROC_ID")
	owner := os.Getenv("SCHED_2PROC_OWNER")
	iters, _ := strconv.Atoi(os.Getenv("SCHED_2PROC_ITERS")) //nolint:errcheck // test env; 0 on parse-fail is fine

	s, err := NewSQLiteStore(dbPath, WithMultiProcess())
	if err != nil {
		t.Fatalf("worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	// NON-VACUITY MARKER (guard [[gate-vacuity-pin-the-input-axis]]): record that THIS worker actually launched +
	// reached the fire loop, in a durable per-owner row. The parent asserts BOTH owners' markers exist — so a
	// silently-failed exec (env unset, binary missing) can NEVER pass the "≤1 enqueue" assertion vacuously (0
	// real contenders). The count of attempts also proves the worker did real work, not a no-op skip.
	if _, merr := s.db.Exec(`CREATE TABLE IF NOT EXISTS sched2p_markers(owner TEXT PRIMARY KEY, attempts INTEGER NOT NULL)`); merr != nil {
		t.Fatalf("worker marker table: %v", merr)
	}

	// Hammer the fire for the due schedule: both processes race the same slot; the in-txn re-check (the arbiter)
	// + the fence let exactly one enqueue. A per-iter error (ErrBusy under contention) is retried — never fatal.
	attempts := 0
	for i := 0; i < iters; i++ {
		_, _ = s.fireDueLocked(context.Background(), schedID, owner) //nolint:errcheck // contention retries
		attempts++
	}
	// Record the attempt count (proves this contender really ran its fire loop).
	if _, merr := s.db.Exec(
		`INSERT INTO sched2p_markers(owner, attempts) VALUES(?, ?)
		 ON CONFLICT(owner) DO UPDATE SET attempts=excluded.attempts`, owner, attempts); merr != nil {
		t.Fatalf("worker marker write: %v", merr)
	}
}

// TestSchedule_TwoProc_NoDoubleFire — ⭐ two OS processes race the same due slot → EXACTLY ONE run enqueued.
func TestSchedule_TwoProc_NoDoubleFire(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses; skipped under -short (runs in the non-short CI Test job)")
	}
	db := filepath.Join(t.TempDir(), "sched2p.db")

	// Parent pre-creates the schema + a DUE schedule ONCE (real wall-clock: the workers use the default
	// SystemClock, so make the schedule already-past by anchoring its first fire in the past).
	pre, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	// A one-shot due 1h ago → exactly one fire expected, ever. (One-shot makes "exactly once" unambiguous:
	// after the single fire the row is deleted, so even many poller iterations across 2 procs → 1 enqueue.)
	spec, err := NewOneshotSchedule("due1", "T", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.CreateSchedule(spec); err != nil {
		t.Fatal(err)
	}
	_ = pre.Close() //nolint:errcheck // pre-create only

	spawn := func(owner string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestSchedulePollerWorkerEntry$", "-test.v") //nolint:gosec // test binary re-exec
		cmd.Env = append(os.Environ(),
			"SCHED_2PROC_WORKER=1",
			"SCHED_2PROC_DB="+db,
			"SCHED_2PROC_ID=due1",
			"SCHED_2PROC_OWNER="+owner,
			"SCHED_2PROC_ITERS=200",
		)
		return cmd
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	outs := make([][]byte, 2)
	for i, owner := range []string{"procA", "procB"} {
		wg.Add(1)
		go func(i int, owner string) {
			defer wg.Done()
			out, err := spawn(owner).CombinedOutput()
			outs[i], errs[i] = out, err
		}(i, owner)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v\n%s", i, errs[i], outs[i])
		}
	}

	verify, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close() //nolint:errcheck // cleanup

	// NON-VACUITY GATE FIRST (guard [[gate-vacuity-pin-the-input-axis]]): assert BOTH re-exec children actually
	// LAUNCHED + ran their fire loop (their durable markers exist with real attempt counts). Without this, a
	// silently-failed exec (0 real contenders) would pass the "==1 enqueue" check vacuously — the re-exec
	// pattern's classic trap. Two markers with attempts>0 = two genuine racing contenders.
	markerRows, merr := verify.db.Query(`SELECT owner, attempts FROM sched2p_markers ORDER BY owner`)
	if merr != nil {
		t.Fatalf("non-vacuity: markers table missing — did the workers actually run? %v", merr)
	}
	markers := map[string]int{}
	for markerRows.Next() {
		var o string
		var a int
		if serr := markerRows.Scan(&o, &a); serr != nil {
			t.Fatal(serr)
		}
		markers[o] = a
	}
	_ = markerRows.Close() //nolint:errcheck // read-only
	if markers["procA"] <= 0 || markers["procB"] <= 0 {
		t.Fatalf("VACUOUS: both processes must have launched + attempted fires; markers=%v (want procA>0 AND procB>0)", markers)
	}

	// EXACTLY ONE run was enqueued for the slot (the no-double-fire property) — now non-vacuous (2 real contenders).
	var n int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE type='T'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("no-double-fire VIOLATED: 2 processes racing one due slot enqueued %d runs (want exactly 1)", n)
	}
	// The one-shot auto-deleted after its single fire.
	var sched int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM schedules WHERE id='due1'`).Scan(&sched); err != nil {
		t.Fatal(err)
	}
	if sched != 0 {
		t.Fatalf("one-shot should auto-delete after firing; %d rows remain", sched)
	}
	t.Logf("2 real OS processes raced one due slot (200 fire-iters each) → EXACTLY 1 run enqueued, schedule auto-deleted.")
}

// TestSchedule_InTxnRecheck_IsTheArbiter — the COMMITTED biting discriminator (in-process, two separate stores on
// one DB = the faithful 2-proc model without re-exec cost). It DEFEATS the layer-3 id-dedup by asserting on the
// number of `fired=true` DECISIONS (the store's own return, independent of the work_queue ON CONFLICT), so it
// witnesses the in-txn re-check directly: exactly ONE of the racing fires reports fired=true for a single due
// slot. Seed-break (drop the `next_fire_time > now` re-check in fireDueLocked) → BOTH report fired=true → RED.
func TestSchedule_InTxnRecheck_IsTheArbiter(t *testing.T) {
	db := filepath.Join(t.TempDir(), "arb.db")
	pre, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	// An interval schedule due 1h ago (so it fires immediately), long period so only ONE slot is due in the window.
	spec, err := NewIntervalSchedule("arb", "T", time.Hour, time.Now().Add(-time.Hour-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.CreateSchedule(spec); err != nil {
		t.Fatal(err)
	}
	_ = pre.Close() //nolint:errcheck // pre-create only

	// Two SEPARATE store instances on the same DB — the faithful two-process model (distinct s.mu, distinct
	// tokenState, shared DB file + write lock), exactly as mkClaimStoresOn models it.
	a, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck // cleanup
	b, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close() //nolint:errcheck // cleanup

	var mu sync.Mutex
	firedTrue := 0
	var wg sync.WaitGroup
	for _, pr := range []struct {
		s     *SQLiteStore
		owner string
	}{{a, "arbA"}, {b, "arbB"}} {
		wg.Add(1)
		go func(st *SQLiteStore, owner string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				fired, _ := st.fireDueLocked(context.Background(), "arb", owner) //nolint:errcheck // contention retries
				if fired {
					mu.Lock()
					firedTrue++
					mu.Unlock()
				}
			}
		}(pr.s, pr.owner)
	}
	wg.Wait()

	// EXACTLY ONE fire decision for the single due slot — the in-txn re-check arbitrates (dedup-independent).
	if firedTrue != 1 {
		t.Fatalf("in-txn re-check VIOLATED: %d fires reported fired=true for one due slot (want exactly 1) — the "+
			"next_fire>now re-check inside the IMMEDIATE txn is the no-double-fire arbiter", firedTrue)
	}
}
