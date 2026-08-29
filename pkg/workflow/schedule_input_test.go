package workflow

// SCHED-INPUT (consumer change request, 2026-08-13) — a schedule carries the JSON-object input its fire
// enqueues. WithInput attaches it, CreateSchedule validates it is an object, and the fire copies it verbatim
// into work_queue.input so a node reads it exactly as for a manually enqueued run. These are the 7 tests the
// downstream change request specified; each is a real state effect, none touches production code.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// firedInput reads the input of the single pending run of a given type (the schedule's enqueue target).
func firedInput(t *testing.T, s *SQLiteStore, typ string) ([]byte, bool) {
	t.Helper()
	var in []byte
	err := s.db.QueryRow(`SELECT input FROM work_queue WHERE type=? AND state='pending' ORDER BY workflow_id LIMIT 1`, typ).Scan(&in)
	if err == sql.ErrNoRows {
		return nil, false
	}
	require.NoError(t, err)
	return in, true
}

// 1. A schedule created WITH input fires a run whose work_queue.input is byte-identical.
func TestScheduleInput_FiresByteIdentical(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	in := []byte(`{"engagement":44,"note":"hi"}`)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec.WithInput(in))
	require.NoError(t, err)
	require.True(t, created)

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired)

	got, ok := firedInput(t, s, "T")
	require.True(t, ok, "a run was enqueued")
	require.Equal(t, in, got, "the fired run carries the schedule's input byte-identical")
}

// 2. A schedule created WITHOUT input fires a run with NULL input, which is the pre-change behaviour.
func TestScheduleInput_NoInputFiresNilRun(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec) // no WithInput
	require.NoError(t, err)
	require.True(t, created)

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired)

	got, ok := firedInput(t, s, "T")
	require.True(t, ok, "a run was enqueued")
	require.Nil(t, got, "no schedule input → NULL/nil run input (the prior behaviour, unchanged)")
}

// 3. A pre-SCHED-INPUT DB (schedules table without the column) opens, gains the column via the idempotent
// ALTER, backfills existing rows to NULL, and those schedules still fire.
func TestScheduleInput_PreMigrationDBGainsColumn(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "mig-input.db")

	// Stand up a store, then recreate `schedules` WITHOUT `input` to simulate a pre-SCHED-INPUT DB, and insert
	// a due interval row directly (all the pre-change columns).
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk))
	require.NoError(t, err)
	_, err = s1.db.Exec(`DROP TABLE schedules`)
	require.NoError(t, err)
	_, err = s1.db.Exec(`CREATE TABLE schedules (
		id TEXT PRIMARY KEY, kind TEXT NOT NULL, spec TEXT NOT NULL, target_type TEXT NOT NULL,
		next_fire_time INTEGER NOT NULL, missed_policy TEXT NOT NULL, paused INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`)
	require.NoError(t, err)
	fireAt := clk.Now().Add(time.Minute).UnixNano()
	period := strconv.FormatInt(int64(time.Minute), 10)
	_, err = s1.db.Exec(
		`INSERT INTO schedules(id,kind,spec,target_type,next_fire_time,missed_policy,paused,created_at,updated_at)
		 VALUES('iv','interval',?,'T',?,'skip',0,?,?)`,
		period, fireAt, clk.Now().UnixNano(), clk.Now().UnixNano())
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Reopen → the idempotent ALTER adds `input` (existing row backfills to NULL).
	s2, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup

	var backfilled []byte
	require.NoError(t, s2.db.QueryRow(`SELECT input FROM schedules WHERE id='iv'`).Scan(&backfilled))
	require.Nil(t, backfilled, "the existing row backfilled to NULL input")

	// And the pre-migration schedule still fires (with nil input).
	clk.Advance(90 * time.Second)
	fired, err := s2.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired, "a pre-migration schedule still fires after gaining the column")
}

// 4. CreateSchedule refuses a non-object input, and the schedule is NOT created.
func TestScheduleInput_NonObjectRefusedAtCreate(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	spec, err := NewIntervalSchedule("bad", "T", time.Minute, clk.Now())
	require.NoError(t, err)

	for _, bad := range [][]byte{[]byte(`[1,2,3]`), []byte(`42`), []byte(`"a string"`), []byte(`{not json`)} {
		created, cerr := s.CreateSchedule(spec.WithInput(bad))
		require.Error(t, cerr, "non-object input %q must be refused", bad)
		require.ErrorIs(t, cerr, ErrSchedule)
		require.False(t, created)
	}
	require.Equal(t, int64(0), schedNextFire(t, s, "bad"), "a refused schedule is not persisted")
}

// 5. Input survives fire → claim → seedInput and is readable by a node.
func TestScheduleInput_SurvivesToSeedInput(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	in := []byte(`{"engagement":"eng-9","batch":"b1"}`)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec.WithInput(in))
	require.NoError(t, err)
	require.True(t, created)

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired)

	item, err := s.ClaimNext("worker", "T")
	require.NoError(t, err)
	require.Equal(t, in, item.Input, "the claimed run carries the schedule's input")

	data := NewWorkflowData("wf")
	require.NoError(t, seedInput(data, item.Input))
	got, ok := data.Get("engagement")
	require.True(t, ok)
	require.Equal(t, "eng-9", got, "a node Gets the seeded input value")
}

// 6. A one-shot's input is delivered exactly once (it deletes its row on fire).
func TestScheduleInput_OneshotDeliveredOnce(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk)
	in := []byte(`{"engagement":"once"}`)
	spec, err := NewOneshotSchedule("os", "T", clk.Now().Add(time.Minute))
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec.WithInput(in))
	require.NoError(t, err)
	require.True(t, created)

	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "os", "owner")
	require.NoError(t, err)
	require.True(t, fired)
	got, ok := firedInput(t, s, "T")
	require.True(t, ok)
	require.Equal(t, in, got, "the one-shot's fire carries its input")

	// The row is gone → a second fire is a no-op; the input was delivered exactly once.
	fired, err = s.fireDueLocked(context.Background(), "os", "owner")
	require.NoError(t, err)
	require.False(t, fired, "a one-shot fires exactly once")
	require.Len(t, pendingWorkIDs(t, s), 1, "exactly one run, delivered once")
}

// 7. A cap-blocked fire does NOT consume or drop the input — the next admitted fire still carries it.
func TestScheduleInput_CapBlockedFirePreservesInput(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedCapStore(t, clk, map[string]int{"T": 1}, 0) // cap type T at 1
	in := []byte(`{"engagement":7}`)
	spec, err := NewIntervalSchedule("iv", "T", time.Minute, clk.Now())
	require.NoError(t, err)
	created, err := s.CreateSchedule(spec.WithInput(in))
	require.NoError(t, err)
	require.True(t, created)

	// Saturate the cap: one pending T run → population 1 == cap.
	_, err = s.Enqueue("filler", "T", nil)
	require.NoError(t, err)

	// First due slot: at cap → missed fire, no enqueue; the input is NOT consumed.
	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.False(t, fired, "at-cap → missed fire, no enqueue")
	var held []byte
	require.NoError(t, s.db.QueryRow(`SELECT input FROM schedules WHERE id='iv'`).Scan(&held))
	require.Equal(t, in, held, "a cap-blocked fire does not consume or drop the schedule's input")

	// Drain the cap, advance to the next slot → the admitted fire still carries the original input.
	_, err = s.db.Exec(`DELETE FROM work_queue WHERE workflow_id='filler'`)
	require.NoError(t, err)
	clk.Advance(60 * time.Second)
	fired, err = s.fireDueLocked(context.Background(), "iv", "owner")
	require.NoError(t, err)
	require.True(t, fired, "cap drained → the next slot fires")
	got, ok := firedInput(t, s, "T")
	require.True(t, ok)
	require.Equal(t, in, got, "the next admitted fire still carries the original input")
}
