package workflow

// OBS-RM-06 (consumer change request, 2026-08-13) — ListSchedules on the Observability read-model. An
// operator can enumerate every schedule (soonest next-fire first), see paused ones, and read each one's
// input/next-fire — closing the read-model's only asymmetry (create/pause/resume/delete existed, no read).
// These are the 6 tests the downstream change request specified.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func scheduleIDs(list []ScheduleInfo) []string {
	out := make([]string, len(list))
	for i, si := range list {
		out[i] = si.ID
	}
	return out
}

// 1. Empty store → empty slice, not nil-with-error.
func TestListSchedules_EmptyIsEmptySlice(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	list, err := s.ListSchedules()
	require.NoError(t, err)
	require.NotNil(t, list, "empty store returns an empty non-nil slice")
	require.Empty(t, list)
}

// 2. Ordering is by next_fire_time ascending; Input round-trips.
func TestListSchedules_OrderedByNextFireAndCarriesInput(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	// Create out of order; expect ascending next_fire_time (a<b<c).
	in := []byte(`{"engagement":1}`)
	_, err := s.CreateSchedule(mustOneshot(t, "c", "T", clk.Now().Add(30*time.Minute)))
	require.NoError(t, err)
	_, err = s.CreateSchedule(mustOneshot(t, "a", "T", clk.Now().Add(10*time.Minute)).WithInput(in))
	require.NoError(t, err)
	_, err = s.CreateSchedule(mustOneshot(t, "b", "T", clk.Now().Add(20*time.Minute)))
	require.NoError(t, err)

	list, err := s.ListSchedules()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, scheduleIDs(list), "soonest next-fire first")
	// Ascending, strictly.
	require.Less(t, list[0].NextFireTime, list[1].NextFireTime)
	require.Less(t, list[1].NextFireTime, list[2].NextFireTime)
	// Input round-trips (Request 1 field on the read-model).
	require.Equal(t, in, list[0].Input, "the schedule's input is visible on the read-model")
	require.Nil(t, list[1].Input, "a schedule with no input reads nil")
	require.Equal(t, "oneshot", list[0].Kind)
	require.Equal(t, "T", list[0].TargetType)
}

// 3. A paused schedule appears with Paused: true (invisible to the poller, visible to an operator).
func TestListSchedules_PausedVisible(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("p", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	paused, err := s.PauseSchedule("p")
	require.NoError(t, err)
	require.True(t, paused)

	list, err := s.ListSchedules()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, list[0].Paused, "a paused schedule is listed with Paused=true")
}

// 4. A fired one-shot is absent afterwards (it deletes its row on fire).
func TestListSchedules_FiredOneshotAbsent(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	_, err := s.CreateSchedule(mustOneshot(t, "os", "T", clk.Now().Add(time.Minute)))
	require.NoError(t, err)
	require.Len(t, mustList(t, s), 1)

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "os", "owner")
	require.NoError(t, err)
	require.True(t, fired)

	require.Empty(t, mustList(t, s), "a fired one-shot deletes its row and is no longer listed")
}

// 5. NextFireTime reflects the post-fire advance.
func TestListSchedules_NextFireReflectsAdvance(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	before := mustList(t, s)
	require.Len(t, before, 1)
	firstFire := before[0].NextFireTime

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired)

	after := mustList(t, s)
	require.Len(t, after, 1)
	require.Greater(t, after[0].NextFireTime, firstFire, "next_fire advanced past the fired slot")
	require.Equal(t, firstFire+int64(time.Minute), after[0].NextFireTime, "advanced by exactly one period")
}

// 6. Requires mp mode, consistent with the rest of Observability.
func TestListSchedules_RequiresMultiProcess(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "single.db")) // NOT mp
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	_, err = s.ListSchedules()
	require.ErrorIs(t, err, ErrValidation, "ListSchedules requires a multi-process store")
}

// helpers
func mustOneshot(t *testing.T, id, typ string, at time.Time) ScheduleSpec {
	t.Helper()
	spec, err := NewOneshotSchedule(id, typ, at)
	require.NoError(t, err)
	return spec
}

func mustList(t *testing.T, s *SQLiteStore) []ScheduleInfo {
	t.Helper()
	list, err := s.ListSchedules()
	require.NoError(t, err)
	return list
}
