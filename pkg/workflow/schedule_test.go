package workflow

// M20 ph100 T2/T3/T4 — durable schedules + the fenced single-txn fire + lifecycle, proven at the store level
// with a FakeClock (the injected clock drives due-ness, DEC-P100-CLOCK-SEAM). The ⭐ cross-process no-double-fire
// is the 2-OS-proc re-exec test in schedule_2proc_test.go. Here: migration, exactly-once-enqueue per due slot,
// missed-run advance, paused/delete/one-shot/idempotent-re-register lifecycle.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkSchedStore opens an mp store with an injected FakeClock so schedule due-ness is deterministic.
func mkSchedStore(t *testing.T, clk *FakeClock) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sched.db"), WithMultiProcess(), withSQLiteClock(clk))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// pendingWorkIDs returns the pending work_queue ids (the fire's enqueue target).
func pendingWorkIDs(t *testing.T, s *SQLiteStore) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT workflow_id FROM work_queue WHERE state='pending' ORDER BY workflow_id`)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // test
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	return out
}

// schedNextFire reads a schedule's stored next_fire_time (0 if absent).
func schedNextFire(t *testing.T, s *SQLiteStore, id string) int64 {
	t.Helper()
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT next_fire_time FROM schedules WHERE id=?`, id).Scan(&n)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return n.Int64
}

// TestSchedule_Migration_ExistingDBGainsTable — an existing M19-era DB (no schedules table) gains it on next
// open (additive CREATE TABLE IF NOT EXISTS, DEC-P100-MIGRATION), no data loss.
func TestSchedule_Migration_ExistingDBGainsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mig.db")
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	_, err = s1.Enqueue("keep", "X", []byte("data")) // a pre-existing work_queue row
	require.NoError(t, err)
	// Simulate a pre-ph100 DB: drop the schedules table.
	_, err = s1.db.Exec(`DROP TABLE schedules`)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup
	// The table is back (a create doesn't error) + the pre-existing work row survived.
	_, err = s2.db.Exec(`SELECT id FROM schedules LIMIT 0`)
	require.NoError(t, err, "schedules table re-created on next open")
	var typ string
	require.NoError(t, s2.db.QueryRow(`SELECT type FROM work_queue WHERE workflow_id='keep'`).Scan(&typ))
	require.Equal(t, "X", typ, "existing data survives the additive migration")
}

// TestSchedule_Interval_ExactlyOnceFire — an interval schedule fires exactly one run per due slot; the fire
// advances next_fire by one period; a second fire at the same instant is a no-op (not yet due).
func TestSchedule_Interval_ExactlyOnceFire(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec)
	require.NoError(t, err)
	require.True(t, created)

	// Not due yet (first fire = now+1min).
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.False(t, fired, "not due before the first period elapses")
	require.Empty(t, pendingWorkIDs(t, s))

	// Advance past the first slot → fires exactly one run.
	clk.Advance(90 * time.Second)
	fired, err = s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired, "due → fires")
	require.Len(t, pendingWorkIDs(t, s), 1, "exactly one run enqueued")

	// A second fire at the same instant is a no-op (next_fire advanced past now).
	fired, err = s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.False(t, fired, "already fired this slot → no double-fire")
	require.Len(t, pendingWorkIDs(t, s), 1, "still exactly one run")
}

// TestSchedule_Cron_FiresAndAdvances — a cron schedule (every minute) fires and advances to the next cron slot.
func TestSchedule_Cron_FiresAndAdvances(t *testing.T) {
	clk := NewFakeClock(time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC))
	s := mkSchedStore(t, clk)
	spec, err := NewCronSchedule("cr", "T", "* * * * *", clk.Now()) // every minute
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	before := schedNextFire(t, s, "cr")
	clk.Advance(2 * time.Minute) // past the first slot
	fired, err := s.fireDueLocked(context.Background(), "cr", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, pendingWorkIDs(t, s), 1)
	after := schedNextFire(t, s, "cr")
	require.Greater(t, after, before, "next_fire advanced to a future cron slot")
	require.Greater(t, after, clk.Now().UnixNano(), "next_fire is strictly in the future")
}

// TestSchedule_Oneshot_FiresOnceThenDeletes — a one-shot fires once then auto-deletes on the fire-commit.
func TestSchedule_Oneshot_FiresOnceThenDeletes(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewOneshotSchedule("os", "T", clk.Now().Add(30*time.Second))
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	clk.Advance(time.Minute) // past the fire instant
	fired, err := s.fireDueLocked(context.Background(), "os", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, pendingWorkIDs(t, s), 1)
	// The row is gone (auto-delete).
	require.Equal(t, int64(0), schedNextFire(t, s, "os"), "one-shot auto-deletes after firing")
	// A re-fire finds nothing.
	fired, err = s.fireDueLocked(context.Background(), "os", "owner")
	require.NoError(t, err)
	require.False(t, fired)
	require.Len(t, pendingWorkIDs(t, s), 1, "no second fire")
}

// TestSchedule_Paused_NeverFires + resume fires again.
func TestSchedule_Paused_NeverFires(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("pz", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	_, err = s.PauseSchedule("pz")
	require.NoError(t, err)

	clk.Advance(2 * time.Minute) // due, but paused
	fired, err := s.fireDueLocked(context.Background(), "pz", "owner")
	require.NoError(t, err)
	require.False(t, fired, "a paused schedule never fires")
	require.Empty(t, pendingWorkIDs(t, s))

	// Resume → fires.
	_, err = s.ResumeSchedule("pz")
	require.NoError(t, err)
	fired, err = s.fireDueLocked(context.Background(), "pz", "owner")
	require.NoError(t, err)
	require.True(t, fired, "resume → fires on the due slot")
}

// TestSchedule_Delete_StopsFires + idempotent re-register.
func TestSchedule_Delete_And_IdempotentReRegister(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("dl", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec)
	require.NoError(t, err)
	require.True(t, created)

	// A re-register of the same id is a detectable no-op (does NOT reset next_fire → no double-fire).
	before := schedNextFire(t, s, "dl")
	clk.Advance(30 * time.Second)
	reReg := spec // same id, but a fresh anchor would move next_fire if not deduped
	reReg.firstFire = clk.Now().Add(time.Hour)
	created2, err := s.CreateSchedule(reReg)
	require.NoError(t, err)
	require.False(t, created2, "re-register of an existing id is a no-op")
	require.Equal(t, before, schedNextFire(t, s, "dl"), "next_fire unchanged by the re-register (no double-fire)")

	// Delete stops all fires.
	deleted, err := s.DeleteSchedule("dl")
	require.NoError(t, err)
	require.True(t, deleted)
	clk.Advance(2 * time.Minute)
	fired, err := s.fireDueLocked(context.Background(), "dl", "owner")
	require.NoError(t, err)
	require.False(t, fired, "a deleted schedule never fires")
}

// TestSchedule_MissedRun_SkipToNext — a schedule whose next_fire is far in the past coalesces the missed window
// into ONE fire and advances to a FUTURE slot (skip-to-next; DEC-P100-MISSED-RUN).
func TestSchedule_MissedRun_SkipToNext(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("ms", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	// Jump FAR forward (100 missed minutes) → one fire, next_fire advanced to a future slot (not 100 fires).
	clk.Advance(100 * time.Minute)
	fired, err := s.fireDueLocked(context.Background(), "ms", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, pendingWorkIDs(t, s), 1, "missed window coalesces into ONE fire (skip-to-next), not 100")
	require.Greater(t, schedNextFire(t, s, "ms"), clk.Now().UnixNano(), "next_fire jumped to a future slot")
}

// TestSchedule_CronTZConsistent — review ph100-F1: a cron schedule anchored in a NON-UTC zone must fire on ONE
// consistent (UTC) calendar — the first fire and every advance agree. Before the fix, the first fire was in the
// anchor's zone and every advance in UTC (a silent offset shift after one fire).
func TestSchedule_CronTZConsistent(t *testing.T) {
	ny := nyLoc(t)
	anchor := time.Date(2024, 6, 1, 0, 0, 0, 0, ny)
	spec, err := NewCronSchedule("tz", "T", "0 9 * * *", anchor)
	require.NoError(t, err)
	first := spec.firstFire
	require.Equal(t, time.UTC, first.Location(), "firstFire is anchored in UTC (F1)")
	require.Equal(t, 9, first.UTC().Hour(), "firstFire is 09:00 UTC")
	next, _, aerr := advanceSchedule(schedCron, "0 9 * * *", missedSkip, first.UnixNano(), first.UnixNano())
	require.NoError(t, aerr)
	require.InDelta(t, 24.0, time.Duration(next-first.UnixNano()).Hours(), 0.001, "consecutive fires are 24h apart (no TZ drift)")
}

// TestSchedule_IDValidated — review ph100-F2: a traversal-shaped schedule id is REJECTED (else the minted run id
// poisons the work_queue with an unpersistable id).
func TestSchedule_IDValidated(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	for _, badID := range []string{"../evil", "a/b", ".."} {
		spec, err := NewOneshotSchedule(badID, "T", clk.Now())
		require.NoError(t, err, "constructor allows any non-empty id; CreateSchedule is the gate")
		_, err = s.CreateSchedule(spec)
		require.ErrorIs(t, err, ErrSchedule, "a traversal-shaped schedule id %q must be rejected", badID)
	}
	ok, err := NewOneshotSchedule("clean-id", "T", clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(ok)
	require.NoError(t, err)
	require.True(t, created)
}

// TestSchedule_IntervalNoStall — review ph100-F3: a tiny period far behind advances in CLOSED FORM (no
// multi-billion-iteration loop inside the write txn). The fire returns promptly + next_fire jumps to the future.
func TestSchedule_IntervalNoStall(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("tiny", "T", time.Nanosecond, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	clk.Advance(time.Hour) // the OLD loop would run ~3.6e12 iterations here
	start := time.Now()
	fired, err := s.fireDueLocked(context.Background(), "tiny", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	require.Less(t, time.Since(start), 2*time.Second, "closed-form advance is O(1) — no multi-billion-iteration stall")
	require.Greater(t, schedNextFire(t, s, "tiny"), clk.Now().UnixNano(), "next_fire jumped past now")
}

// TestSchedule_LeaseReleasedOnDeleteAndOneshot — review ph100-F4: a fired one-shot and a DeleteSchedule both
// release the schedule's leases row (no unbounded leak; a same-id re-register can fire immediately).
func TestSchedule_LeaseReleasedOnDeleteAndOneshot(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	leaseExists := func(id string) bool {
		var n int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE workflow_id=?`, scheduleLeaseKey(id)).Scan(&n))
		return n > 0
	}

	os1, err := NewOneshotSchedule("os1", "T", clk.Now().Add(time.Second))
	require.NoError(t, err)
	_, err = s.CreateSchedule(os1)
	require.NoError(t, err)
	clk.Advance(2 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "os1", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	require.False(t, leaseExists("os1"), "a fired one-shot releases its lease (no leak)")

	iv, err := NewIntervalSchedule("iv1", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(iv)
	require.NoError(t, err)
	clk.Advance(2 * time.Minute)
	_, err = s.fireDueLocked(context.Background(), "iv1", "owner")
	require.NoError(t, err)
	require.True(t, leaseExists("iv1"), "a fired interval schedule holds a lease")
	deleted, err := s.DeleteSchedule("iv1")
	require.NoError(t, err)
	require.True(t, deleted)
	require.False(t, leaseExists("iv1"), "DeleteSchedule releases the lease (matches its contract)")
}

// TestSchedule_PollerDrivesFire — the poller ticks on the FakeClock and fires a due schedule end-to-end.
func TestSchedule_PollerDrivesFire(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("pl", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	clk.Advance(2 * time.Minute) // due

	p, err := NewSchedulePoller(s, "poller-1", WithSchedulePollInterval(10*time.Millisecond))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }() //nolint:errcheck // Run returns nil on ctx-cancel; the test asserts the fire, not the return

	require.Eventually(t, func() bool { return len(pendingWorkIDs(t, s)) == 1 }, 3*time.Second, 20*time.Millisecond,
		"the poller fires the due schedule")
	cancel()
	<-done
}
