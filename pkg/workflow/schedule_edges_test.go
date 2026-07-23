package workflow

// M20 ph103 earn-back (round 2) — biting tests over the uncovered ERROR / EDGE branches of the M20 schedule
// machinery: advanceSchedule's corrupt-spec / overflow / unknown-kind failure modes, the poller constructor
// guards, the store-lifecycle mp-mode refusals, and a few cron parse-error edges. Every assertion checks a real
// refusal or a real computed value; none touches production code.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAdvanceSchedule_CorruptSpecsAndUnknownKind — advanceSchedule refuses (typed ErrSchedule, no enqueue) on a
// spec that no longer parses or an unknown kind. These are the fail-safe branches the fire path takes on a
// DB-tampered / bit-rotted row; a validating constructor makes them unreachable via the public API, so they are
// only reachable by calling the internal advance directly (white-box).
func TestAdvanceSchedule_CorruptSpecsAndUnknownKind(t *testing.T) {
	const now = int64(1_700_000_000_000_000_000)
	cases := []struct {
		name          string
		kind, spec    string
		nextFire, now int64
	}{
		{"interval/non-numeric", schedInterval, "not-a-number", now, now},
		{"interval/zero-period", schedInterval, "0", now, now},
		{"cron/unparseable", schedCron, "not five fields", now, now},
		{"cron/no-fire", schedCron, "0 0 30 2 *", now, now}, // Feb 30 → cron advance surfaces no-fire
		{"unknown-kind", "bogus", "", now, now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newNext, doEnqueue, err := advanceSchedule(tc.kind, tc.spec, missedSkip, tc.nextFire, tc.now)
			require.ErrorIs(t, err, ErrSchedule, "a corrupt/unknown schedule must fail-safe with ErrSchedule")
			require.False(t, doEnqueue, "a failed advance must not signal an enqueue")
			require.Zero(t, newNext, "a failed advance returns the zero next-fire")
		})
	}
}

// TestAdvanceSchedule_IntervalOverflowRefused — a corrupt row with a huge period whose next-slot computation would
// wrap int64 is refused rather than returning a wrapped (negative/past) next-fire that would re-fire every tick.
// Bites the overflow guard (schedule.go:447).
func TestAdvanceSchedule_IntervalOverflowRefused(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	// nextFire=0, period=maxInt64/2+... so that k*period+nextFire overflows. now well past nextFire → k≥2.
	newNext, doEnqueue, err := advanceSchedule(schedInterval, strconv.FormatInt(maxInt64-10, 10), missedSkip, 0, maxInt64-5)
	require.ErrorIs(t, err, ErrSchedule, "an interval advance that overflows int64 must be refused")
	require.False(t, doEnqueue)
	require.Zero(t, newNext)
}

// TestAdvanceSchedule_IntervalClosedFormJumpsPastNow — the happy closed-form path: an interval far behind advances
// in ONE step to the first future slot (not a loop), and doEnqueue is true (a slot was due). Guards the ph100-F3
// closed-form advance against a regression to the unbounded loop.
func TestAdvanceSchedule_IntervalClosedFormJumpsPastNow(t *testing.T) {
	period := int64(time.Minute)
	nextFire := int64(0)
	now := 10*period + 30 // 10.5 periods elapsed
	newNext, doEnqueue, err := advanceSchedule(schedInterval, strconv.FormatInt(period, 10), missedSkip, nextFire, now)
	require.NoError(t, err)
	require.True(t, doEnqueue, "at least one slot was due")
	require.Greater(t, newNext, now, "the advance must land strictly in the future")
	require.Equal(t, int64(11)*period, newNext, "closed-form: first slot > now is the 11th period")
}

// TestNewSchedulePoller_Guards — the poller constructor refuses a nil store, a non-mp store, and an empty ownerID
// (each would make the fenced fire meaningless), and clamps a non-positive poll interval to the default. Bites the
// three guard branches + the interval clamp.
func TestNewSchedulePoller_Guards(t *testing.T) {
	t.Run("nil-store", func(t *testing.T) {
		_, err := NewSchedulePoller(nil, "owner")
		require.ErrorIs(t, err, ErrValidation)
	})
	t.Run("non-mp-store", func(t *testing.T) {
		s, err := NewSQLiteStore(t.TempDir() + "/np.db") // single-process (no WithMultiProcess)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		_, err = NewSchedulePoller(s, "owner")
		require.ErrorIs(t, err, ErrValidation, "the poller requires an mp store")
	})
	t.Run("empty-owner", func(t *testing.T) {
		clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
		s := mkSchedStore(t, clk)
		_, err := NewSchedulePoller(s, "")
		require.ErrorIs(t, err, ErrValidation, "the fence needs a non-empty owner id")
	})
	t.Run("non-positive-interval-clamps-to-default", func(t *testing.T) {
		clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
		s := mkSchedStore(t, clk)
		p, err := NewSchedulePoller(s, "owner", WithSchedulePollInterval(-1))
		require.NoError(t, err)
		require.Equal(t, defaultSchedulePollInterval, p.pollInterval, "a non-positive interval clamps to the default")
	})
}

// TestScheduleOps_RequireMultiProcess — CreateSchedule / Pause / Resume / DeleteSchedule all refuse a
// single-process store (schedules fire onto the mp work_queue via the fenced txn, meaningless without mp). Bites
// the !s.dur.mp guard on each lifecycle op.
func TestScheduleOps_RequireMultiProcess(t *testing.T) {
	s, err := NewSQLiteStore(t.TempDir() + "/sp.db") // single-process
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup

	spec, err := NewIntervalSchedule("x", "T", time.Minute, time.Unix(1_700_000_000, 0).UTC())
	require.NoError(t, err)

	_, err = s.CreateSchedule(spec)
	require.ErrorIs(t, err, ErrValidation, "CreateSchedule requires mp")
	_, err = s.PauseSchedule("x")
	require.ErrorIs(t, err, ErrValidation, "PauseSchedule requires mp")
	_, err = s.ResumeSchedule("x")
	require.ErrorIs(t, err, ErrValidation, "ResumeSchedule requires mp")
	_, err = s.DeleteSchedule("x")
	require.ErrorIs(t, err, ErrValidation, "DeleteSchedule requires mp")
}

// TestScheduleLifecycle_MissingIDIsZeroRowNoOp — pausing/resuming/deleting an absent id is a benign 0-row no-op
// (returns ok=false, no error), not a failure. Bites the n==1 false branch of setSchedulePaused/DeleteSchedule.
func TestScheduleLifecycle_MissingIDIsZeroRowNoOp(t *testing.T) {
	clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := mkSchedStore(t, clk)

	ok, err := s.PauseSchedule("ghost")
	require.NoError(t, err)
	require.False(t, ok, "pausing a missing schedule is a 0-row no-op")

	ok, err = s.ResumeSchedule("ghost")
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = s.DeleteSchedule("ghost")
	require.NoError(t, err)
	require.False(t, ok, "deleting a missing schedule is a 0-row no-op")
}

// TestCreateSchedule_RejectsIncompleteSpec — a zero-valued ScheduleSpec (empty id/type/kind — e.g. one built
// outside the validating constructors) is refused with ErrSchedule before any DB write. Bites the incomplete-spec
// guard in CreateSchedule.
func TestCreateSchedule_RejectsIncompleteSpec(t *testing.T) {
	clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := mkSchedStore(t, clk)
	_, err := s.CreateSchedule(ScheduleSpec{}) // no id/type/kind
	require.ErrorIs(t, err, ErrSchedule, "an incomplete spec is refused before the DB write")
}

// TestParseCron_BadStepAndRangeErrors — the parseTerm/parseField error edges: a non-positive or non-numeric step,
// an empty comma-term, and an out-of-range value each surface a typed ErrCronSpec. Bites the bad-step branch and
// the empty-term branch.
func TestParseCron_BadStepAndRangeErrors(t *testing.T) {
	bad := []string{
		"*/0 * * * *",   // step 0
		"*/abc * * * *", // non-numeric step
		"1,,2 * * * *",  // empty comma-term
		"0 0 * * 9",     // day-of-week 9 out of [0,6]
	}
	for _, spec := range bad {
		_, err := ParseCron(spec)
		require.ErrorIs(t, err, ErrCronSpec, "spec %q must be a typed cron parse error", spec)
	}
}

// TestFireDueLocked_CorruptSpecPausesRow — a DB-tampered / bit-rotted schedule row whose spec no longer parses
// (impossible via the validating constructors) is FAIL-SAFE QUARANTINED: fireDueLocked pauses the row + commits
// the pause (so a corrupt schedule stops firing and is operator-visible), then surfaces the ErrSchedule — rather
// than spinning the poller forever on a row that stays due. Bites the corrupt-spec pause+commit branch
// (schedule.go:350-364) with real behavior, not just the pure advanceSchedule error path.
func TestFireDueLocked_CorruptSpecPausesRow(t *testing.T) {
	clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := mkSchedStore(t, clk)

	// Insert a corrupt interval row directly (bypassing the validating constructor): a non-numeric spec that
	// advanceSchedule will reject. Make it DUE (next_fire in the past relative to the clock).
	due := clk.Now().Add(-time.Minute).UnixNano()
	_, err := s.db.Exec(
		`INSERT INTO schedules(id, kind, spec, target_type, next_fire_time, missed_policy, paused, created_at, updated_at)
		 VALUES ('rot','interval','not-a-number','T',?,'skip',0,?,?)`,
		due, due, due)
	require.NoError(t, err)

	fired, ferr := s.fireDueLocked(context.Background(), "rot", "owner-1")
	require.False(t, fired, "a corrupt schedule never fires a run")
	require.ErrorIs(t, ferr, ErrSchedule, "the corrupt spec surfaces as ErrSchedule")

	// The fail-safe pause is DURABLE (committed): the row is now paused=1 so the poller stops re-attempting it.
	var paused int
	require.NoError(t, s.db.QueryRow(`SELECT paused FROM schedules WHERE id='rot'`).Scan(&paused))
	require.Equal(t, 1, paused, "a corrupt-spec row is quarantined by pausing it, durably")
}

// TestFieldSpec_MatchesOutOfRange — the matches() guard rejects values outside [0,63] without indexing past the
// 64-bit set. Bites the v<0||v>63 guard (cron.go:34).
func TestFieldSpec_MatchesOutOfRange(t *testing.T) {
	sched, err := ParseCron("* * * * *") // every field is `*` → full set
	require.NoError(t, err)
	require.False(t, sched.minute.matches(-1), "a negative value never matches")
	require.False(t, sched.minute.matches(64), "a value past the 64-bit set never matches")
	require.True(t, sched.minute.matches(0), "an in-range value in a `*` field matches")
}
