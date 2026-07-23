package workflow

// M20 ph103 earn-back — biting tests over the schedule/cron constructor VALIDATION branches and the two
// reserved-but-durable surfaces (Schedule.String, ScheduleSpec.WithCatchupOnce) that the happy-path tests in
// schedule_test.go / cron_test.go leave uncovered. Every assertion checks a real refusal or a real state effect;
// none touches production code.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestScheduleCtor_RejectsEmptyIDAndType — every constructor refuses an empty id or an empty target type with a
// typed ErrSchedule (a schedule with no id can't be re-registered idempotently; one with no target type mints a
// run onto no dispatch type). These are the id==""||typ=="" guard branches the happy paths never hit.
func TestScheduleCtor_RejectsEmptyIDAndType(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name string
		mk   func() (ScheduleSpec, error)
	}{
		{"cron/empty-id", func() (ScheduleSpec, error) { return NewCronSchedule("", "T", "* * * * *", now) }},
		{"cron/empty-type", func() (ScheduleSpec, error) { return NewCronSchedule("c", "", "* * * * *", now) }},
		{"interval/empty-id", func() (ScheduleSpec, error) { return NewIntervalSchedule("", "T", time.Minute, now) }},
		{"interval/empty-type", func() (ScheduleSpec, error) { return NewIntervalSchedule("i", "", time.Minute, now) }},
		{"oneshot/empty-id", func() (ScheduleSpec, error) { return NewOneshotSchedule("", "T", now) }},
		{"oneshot/empty-type", func() (ScheduleSpec, error) { return NewOneshotSchedule("o", "", now) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := tc.mk()
			require.ErrorIs(t, err, ErrSchedule, "empty id/type must be a typed ErrSchedule refusal")
			require.Equal(t, ScheduleSpec{}, spec, "a refused constructor must return the zero spec")
		})
	}
}

// TestNewCronSchedule_RejectsBadSpec — a malformed cron spec propagates a typed ErrSchedule (wrapping the cron
// ErrCronSpec), not a silent zero-spec. This bites the ParseCron error branch in NewCronSchedule.
func TestNewCronSchedule_RejectsBadSpec(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	spec, err := NewCronSchedule("c", "T", "not five fields", now)
	require.ErrorIs(t, err, ErrSchedule)
	require.ErrorIs(t, err, ErrCronSpec, "the underlying cron parse error must be chained through")
	require.Equal(t, ScheduleSpec{}, spec)
}

// TestNewCronSchedule_RejectsNoFireSpec — an impossible-but-parseable spec (Feb 30) yields ErrCronNoFire chained
// under ErrSchedule from the sched.Next(anchor) branch, rather than hanging or returning a fireless spec.
func TestNewCronSchedule_RejectsNoFireSpec(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	spec, err := NewCronSchedule("c", "T", "0 0 30 2 *", now) // Feb 30 never exists
	require.ErrorIs(t, err, ErrSchedule)
	require.ErrorIs(t, err, ErrCronNoFire, "an unsatisfiable spec must surface ErrCronNoFire, not hang")
	require.Equal(t, ScheduleSpec{}, spec)
}

// TestNewIntervalSchedule_RejectsNonPositivePeriod — period<=0 is refused (a zero/negative interval would fire
// every poll or never advance). Bites the period<=0 guard.
func TestNewIntervalSchedule_RejectsNonPositivePeriod(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, p := range []time.Duration{0, -time.Second} {
		spec, err := NewIntervalSchedule("i", "T", p, now)
		require.ErrorIs(t, err, ErrSchedule, "period %v must be refused", p)
		require.Equal(t, ScheduleSpec{}, spec)
	}
}

// TestCronSchedule_StringRoundTrips — Schedule.String() returns the original spec verbatim (it feeds errors and
// the persisted `spec` column). Bites the 0%-covered String method.
func TestCronSchedule_StringRoundTrips(t *testing.T) {
	const spec = "*/5 9-17 * * 1-5"
	sched, err := ParseCron(spec)
	require.NoError(t, err)
	require.Equal(t, spec, sched.String(), "String() must round-trip the parsed spec")
}

// TestWithCatchupOnce_PersistsCatchupPolicy — WithCatchupOnce() flips the spec to the catch-up-once policy, and a
// CreateSchedule of that spec durably records missed_policy='catchup' (the RESERVED-but-round-trips contract,
// DEC-P103-CATCHUP-RESERVED). Bites both the 0%-covered WithCatchupOnce and the catchup branch of CreateSchedule.
func TestWithCatchupOnce_PersistsCatchupPolicy(t *testing.T) {
	clk := NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := mkSchedStore(t, clk)

	base, err := NewIntervalSchedule("cu", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	spec := base.WithCatchupOnce()

	created, err := s.CreateSchedule(spec)
	require.NoError(t, err)
	require.True(t, created)

	var policy string
	require.NoError(t, s.db.QueryRow(`SELECT missed_policy FROM schedules WHERE id=?`, "cu").Scan(&policy))
	require.Equal(t, "catchup", policy, "WithCatchupOnce must durably persist the catchup missed policy")

	// Contrast: the default (no WithCatchupOnce) persists skip — proves the flag is what changed the column.
	def, err := NewIntervalSchedule("sk", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(def)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT missed_policy FROM schedules WHERE id=?`, "sk").Scan(&policy))
	require.Equal(t, "skip", policy, "the default missed policy is skip-to-next")
}
