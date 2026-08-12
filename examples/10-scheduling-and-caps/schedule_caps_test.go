package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestDurableSchedule_FiresExpectedTimes proves the schedule fires exactly the expected
// number of times over N ticks, in due-time order — and does so DETERMINISTICALLY via an
// injected FakeClock (no wall-clock sleeping, so the "3-hour schedule" is an instant test).
func TestDurableSchedule_FiresExpectedTimes(t *testing.T) {
	store, err := workflow.NewSQLiteStore(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	clk := workflow.NewFakeClock(testEpoch)
	log := newFireLog()

	// 8 ticks of 30 min across a 4-hour horizon: the +1h/+2h/+3h schedules fire at ticks 2/4/6.
	fires, err := pollSchedules(context.Background(), store, clk, log, 30*time.Minute, 8)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	if fires != len(scheduledJobs) {
		t.Errorf("fires = %d, want %d (each schedule fires exactly once)", fires, len(scheduledJobs))
	}
	got := log.snapshot()
	want := []string{"job-1", "job-2", "job-3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fire order = %v, want %v (independent schedules fire at their own due times)", got, want)
	}
}

// TestDurableSchedule_NoEarlyFire proves a durable timer does NOT fire before its fireAt:
// polling only up to +30m (below the earliest +1h schedule) fires nothing. A spurious
// early poll is a cheap no-op — deterministic, because the clock is injected.
func TestDurableSchedule_NoEarlyFire(t *testing.T) {
	store, err := workflow.NewSQLiteStore(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	clk := workflow.NewFakeClock(testEpoch)
	log := newFireLog()

	// 3 ticks of 10 min → clock reaches only +30m, before the earliest (+1h) fireAt.
	fires, err := pollSchedules(context.Background(), store, clk, log, 10*time.Minute, 3)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if fires != 0 {
		t.Errorf("fires = %d, want 0 (no timer is due before +1h)", fires)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Errorf("fire log = %v, want empty (nothing fires early)", got)
	}
}

// TestDurableSchedule_SurvivesStoreReopen is the durability proof: the schedules are armed
// on one store handle, that handle is CLOSED, and a FRESH handle to the same DB file drives
// them to completion. The parked timers were persisted to disk, so the fresh handle (a
// stand-in for a whole new process) resumes and fires them — the schedule survives a restart.
func TestDurableSchedule_SurvivesStoreReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "schedule.db")
	clk := workflow.NewFakeClock(testEpoch)
	log := newFireLog()
	ctx := context.Background()

	// Handle #1: arm the schedules (persist the absolute fireAts), then close it entirely.
	arm, err := workflow.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open arm store: %v", err)
	}
	if err := armSchedules(ctx, arm, clk, log); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := arm.Close(); err != nil {
		t.Fatalf("close arm store: %v", err)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("armed schedules must not have fired yet, log = %v", got)
	}

	// Handle #2: a fresh handle to the same file drives the armed schedules to completion.
	drive, err := workflow.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open drive store: %v", err)
	}
	defer drive.Close()
	fires, err := tickSchedules(ctx, drive, clk, log, 30*time.Minute, 8)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	if fires != len(scheduledJobs) {
		t.Errorf("fires after reopen = %d, want %d — the durable schedule survived the handle swap", fires, len(scheduledJobs))
	}
	if got, want := log.snapshot(), []string{"job-1", "job-2", "job-3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fire order after reopen = %v, want %v", got, want)
	}
}

// TestConcurrencyCap_NeverExceeded proves the cross-process cap holds under contention: a
// fleet of capWorkers (> K) workers drains capJobs transcodes, yet the observed peak
// concurrency never exceeds K — and, because the work window guarantees overlap, the peak
// actually REACHES K (so the assertion is not vacuously true). Every job runs exactly once.
func TestConcurrencyCap_NeverExceeded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	meter := newConcurrencyMeter()

	if err := runCapFleet(context.Background(), dbPath, meter, capJobs, capWorkers, transcodeCap); err != nil {
		t.Fatalf("cap fleet: %v", err)
	}

	peak := meter.peakConcurrency()
	if peak > transcodeCap {
		t.Errorf("peak concurrency = %d, exceeds cap K=%d", peak, transcodeCap)
	}
	// Non-vacuity: the fleet would run all capJobs at once absent the cap; the 40ms work
	// window makes concurrent transcodes overlap, so under a > K fleet the cap is actually
	// reached. If this ever fails without a cap violation, the demo lost its contention.
	if peak != transcodeCap {
		t.Errorf("peak concurrency = %d, want it to reach the cap K=%d (else the test is vacuous)", peak, transcodeCap)
	}

	runs := meter.runCounts()
	if len(runs) != capJobs {
		t.Errorf("distinct jobs run = %d, want %d (none lost)", len(runs), capJobs)
	}
	for id, n := range runs {
		if n != 1 {
			t.Errorf("job %s ran %d times, want exactly 1", id, n)
		}
	}
}
