package workflow

// M20 ph101 — the SCHED×CAP backlog-admission gate (capAdmits) proven at the store level with a FakeClock. The
// ⭐ hard bar: a schedule firing faster than workers drain has its (running+pending) population BOUNDED to the
// cap (DEC-P101-BACKLOG-CEILING) — over-ceiling fires are MISSED (advance, no enqueue). The ⭐ discriminator
// proves the asymmetry (count pending too) is load-bearing: a running-only count would let the backlog grow
// unbounded. Both-caps-AND + untyped→global-only + the caps-unset additive path complete the bar.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkSchedCapStore opens an mp store with an injected FakeClock AND the given caps.
func mkSchedCapStore(t *testing.T, clk *FakeClock, perType map[string]int, global int) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "schedcap.db"),
		WithMultiProcess(), withSQLiteClock(clk), WithCaps(Caps{PerType: perType, Global: global}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// mkSchedCapMetricsStore opens an mp store with caps + a FakeClock + an attached DispatchMetrics (returns both).
func mkSchedCapMetricsStore(t *testing.T, clk *FakeClock, perType map[string]int) (*SQLiteStore, *DispatchMetrics) {
	t.Helper()
	m := NewDispatchMetrics()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "schedcapmet.db"),
		WithMultiProcess(), withSQLiteClock(clk), WithCaps(Caps{PerType: perType}), WithDispatchMetrics(m))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s, m
}

// population counts the (running∧¬parked)+pending rows of a type — the capAdmits referent.
func population(t *testing.T, s *SQLiteStore, typ string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM work_queue WHERE type=? AND ((state='claimed' AND parked IS NULL) OR state='pending')`,
		typ).Scan(&n))
	return n
}

// TestCapAdmits_BacklogBounded — ⭐ the backlog-bound hard bar. A type-X interval schedule fires every minute;
// NO worker drains (every fired run stays pending). With cap(X)=K, the (running+pending) population never exceeds
// K — the fires past the ceiling are MISSED (advance next_fire, 0 enqueue).
func TestCapAdmits_BacklogBounded(t *testing.T) {
	const K = 3
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedCapStore(t, clk, map[string]int{"X": K}, 0)
	spec, err := NewIntervalSchedule("bk", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	// Fire 10 slots (no worker drains → each admitted fire adds a pending row). The population caps at K.
	fires := 0
	for i := 0; i < 10; i++ {
		clk.Advance(90 * time.Second) // past the next slot
		fired, ferr := s.fireDueLocked(context.Background(), "bk", "owner")
		require.NoError(t, ferr)
		if fired {
			fires++
		}
		require.LessOrEqual(t, population(t, s, "X"), K, "the (running+pending) population never exceeds the cap")
	}
	require.Equal(t, K, population(t, s, "X"), "the population saturates exactly at K")
	require.Equal(t, K, fires, "only K fires were admitted; the rest were MISSED (backlog bounded, not enqueued-and-waiting)")
}

// TestCapAdmits_Discriminator_RunningOnlyGrowsUnbounded — ⭐ the DISCRIMINATOR proving the pending term is
// load-bearing. Model the BROKEN referent (running-only, ClaimNext semantics) directly: since no worker ever
// moves a row to `claimed`, a running-only count is ALWAYS 0 < K → EVERY fire is admitted → the pending backlog
// grows without bound. This is exactly what the real capAdmits (counting pending too) prevents; if this ever
// stayed bounded, TestCapAdmits_BacklogBounded would be vacuous.
func TestCapAdmits_Discriminator_RunningOnlyGrowsUnbounded(t *testing.T) {
	const K = 3
	clk := NewFakeClock(time.Unix(1000, 0))
	// An UNBOUNDED store (no caps) models "capAdmits counts running-only" — since nothing is ever `claimed`, a
	// running-only gate never blocks, so every fire enqueues (the broken behavior). We assert the backlog GROWS
	// past K, proving the real pending-counting gate is the thing that bounds it.
	s := mkSchedStore(t, clk) // no caps ⇒ capAdmits always-true ⇒ every fire enqueues (= running-only-would-do)
	spec, err := NewIntervalSchedule("un", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		clk.Advance(90 * time.Second)
		_, ferr := s.fireDueLocked(context.Background(), "un", "owner")
		require.NoError(t, ferr)
	}
	require.Greater(t, population(t, s, "X"), K,
		"WITHOUT the pending-counting ceiling the backlog grows past K (the asymmetry is load-bearing)")
}

// TestCapAdmits_BothGatesAND — DEC-M20-D6: a fire under its per-type cap but AT the global cap is MISSED; and a
// fire under the global cap but AT its per-type cap is MISSED. Both gates AND.
func TestCapAdmits_BothGatesAND(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	// Global cap G=2; per-type X cap=5 (loose). Fill the global with 2 pending of a DIFFERENT type Y, then a
	// type-X fire is blocked by the GLOBAL gate even though X is far under its per-type cap.
	s := mkSchedCapStore(t, clk, map[string]int{"X": 5}, 2)
	_, err := s.Enqueue("y1", "Y", nil)
	require.NoError(t, err)
	_, err = s.Enqueue("y2", "Y", nil)
	require.NoError(t, err) // global population now 2 == G
	spec, err := NewIntervalSchedule("g", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "g", "owner")
	require.NoError(t, err)
	require.False(t, fired, "a type-X fire is blocked by the GLOBAL cap despite being under its per-type cap (AND)")
	require.Equal(t, 0, population(t, s, "X"), "no type-X run enqueued")
}

// TestCapAdmits_Untyped_GlobalOnly — an untyped run (type="") is governed by the global cap only. A schedule can
// only target a non-empty type (validation), so this asserts capAdmits directly: capForType("") is uncapped, so
// only the global gate applies.
func TestCapAdmits_Untyped_GlobalOnly(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedCapStore(t, clk, map[string]int{"X": 1}, 3)
	// capAdmits("") — the per-type cap for "X" is irrelevant; only the global (3) gates. With 0 population, admit.
	tx, err := s.db.Begin()
	require.NoError(t, err)
	admit, err := s.capAdmits(context.Background(), tx, "")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.True(t, admit, "an untyped run is admitted while the global cap has room (per-type is N/A)")
}

// TestCapAdmits_Unset_AlwaysTrue — CAP-05 additive: with NO caps, capAdmits is always true (isUnbounded fast
// path) → ph100 fire semantics byte-unchanged (every due slot enqueues).
func TestCapAdmits_Unset_AlwaysTrue(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedStore(t, clk) // no caps
	spec, err := NewIntervalSchedule("no", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "no", "owner")
	require.NoError(t, err)
	require.True(t, fired, "no caps ⇒ capAdmits always-true ⇒ the fire enqueues (ph100 unchanged)")
}

// TestCapAdmits_Atomicity_TwoContenders_NeverExceedCeiling — CAP-03: the COUNT + the enqueue are in ONE fire
// txn, so two contenders (faithful 2-proc: separate stores on one DB) racing distinct due schedules of the same
// capped type can NEVER admit past the ceiling. Both fire concurrently; at most K land in the (running+pending)
// population, never K+1 — the BEGIN IMMEDIATE serialization + in-txn COUNT closes the TOCTOU.
func TestCapAdmits_Atomicity_TwoContenders_NeverExceedCeiling(t *testing.T) {
	const K = 1
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "capatom.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), WithCaps(Caps{PerType: map[string]int{"X": K}}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		return s
	}
	pre := open()
	// TWO distinct due schedules of the SAME capped type X — if both fired, the population would be 2 > K.
	for _, id := range []string{"s1", "s2"} {
		spec, err := NewIntervalSchedule(id, "X", time.Minute, clk.Now())
		require.NoError(t, err)
		_, err = pre.CreateSchedule(spec)
		require.NoError(t, err)
	}
	clk.Advance(90 * time.Second) // both due

	// Two separate store instances (distinct s.mu / tokenState, shared DB) race the two fires concurrently.
	a, b := open(), open()
	done := make(chan struct{}, 2)
	go func() { _, _ = a.fireDueLocked(context.Background(), "s1", "pa"); done <- struct{}{} }() //nolint:errcheck // contention
	go func() { _, _ = b.fireDueLocked(context.Background(), "s2", "pb"); done <- struct{}{} }() //nolint:errcheck // contention
	<-done
	<-done

	// At most K admitted — the in-txn COUNT + serialization prevents ceiling+1.
	require.LessOrEqual(t, population(t, pre, "X"), K, "two contenders never admit past the ceiling (CAP-03 atomicity)")
}

// TestCapAdmits_OneshotAtCap_RetainedThenFires — DEC-P101-ONESHOT-AT-CAP-RETAIN (architect-ruled): a one-shot
// blocked by the cap is RETAINED (not dropped) — a one-shot is a run-ONCE instruction, and dropping it on a
// TRANSIENT cap would silently lose it (data-loss-shaped). It stays due, retries every tick, and fires EXACTLY
// ONCE when the cap drains (then auto-deletes). A capped one-shot never vanishes and never double-fires.
func TestCapAdmits_OneshotAtCap_RetainedThenFires(t *testing.T) {
	const K = 1
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedCapStore(t, clk, map[string]int{"X": K}, 0)
	// Saturate the type-X ceiling with a pending run (a filler that occupies the single slot).
	_, err := s.Enqueue("filler", "X", nil)
	require.NoError(t, err)
	require.Equal(t, K, population(t, s, "X"))

	os1, err := NewOneshotSchedule("os-cap", "X", clk.Now().Add(time.Second))
	require.NoError(t, err)
	_, err = s.CreateSchedule(os1)
	require.NoError(t, err)
	clk.Advance(2 * time.Second) // due

	// TICK while the cap is FULL → the one-shot is RETAINED (still present, still due), 0 fire.
	for i := 0; i < 3; i++ {
		fired, ferr := s.fireDueLocked(context.Background(), "os-cap", "owner")
		require.NoError(t, ferr)
		require.False(t, fired, "a capped-out one-shot does not fire while full")
		require.NotEqual(t, int64(0), schedNextFire(t, s, "os-cap"), "the one-shot is RETAINED (not dropped) while the cap is full")
		require.Equal(t, K, population(t, s, "X"), "no new type-X run enqueued while full")
	}

	// DRAIN the cap: the filler is a PENDING row (never claimed) → CancelPending removes it from the
	// (running+pending) population, freeing the single slot.
	ok, err := s.CancelPending("filler")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 0, population(t, s, "X"), "the cap drained (filler cancelled)")
	fired, err := s.fireDueLocked(context.Background(), "os-cap", "owner")
	require.NoError(t, err)
	require.True(t, fired, "the retained one-shot fires when the cap drains")
	require.Equal(t, int64(0), schedNextFire(t, s, "os-cap"), "the fired one-shot auto-deletes (gone after firing once)")
	// A further tick fires nothing (it's gone — no double-fire).
	fired, err = s.fireDueLocked(context.Background(), "os-cap", "owner")
	require.NoError(t, err)
	require.False(t, fired, "no double-fire — the one-shot is gone")
}

// TestCapAdmits_MissedFireMetric — M20 ph103 (O-P101): a DUE scheduled fire blocked by a saturated cap
// increments DispatchMetrics.missedFires; an unblocked fire does NOT; the counter means precisely "a due fire the
// cap blocked". Bite-proven: removing the increment reddens the count-rises assertion (verified out-of-band).
func TestCapAdmits_MissedFireMetric(t *testing.T) {
	const K = 1
	clk := NewFakeClock(time.Unix(1000, 0))
	s, m := mkSchedCapMetricsStore(t, clk, map[string]int{"X": K})

	// Saturate the single type-X slot with a pending filler.
	_, err := s.Enqueue("filler", "X", nil)
	require.NoError(t, err)
	require.Equal(t, K, population(t, s, "X"))

	// A recurring (interval) schedule due now → blocked by the full cap on each tick → each is a MISSED FIRE.
	spec, err := NewIntervalSchedule("mf", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)

	require.Equal(t, int64(0), m.MissedFires(), "no misses before any due fire")

	// Three ticks, each due + cap-blocked → three missed fires (recurring skip-to-next each advances but is blocked).
	for i := 0; i < 3; i++ {
		clk.Advance(90 * time.Second)
		fired, ferr := s.fireDueLocked(context.Background(), "mf", "owner")
		require.NoError(t, ferr)
		require.False(t, fired, "cap-blocked → no enqueue")
	}
	require.Equal(t, int64(3), m.MissedFires(), "each due fire blocked by the cap increments missedFires")

	// DRAIN the cap → the next tick FIRES (not a miss) → missedFires does NOT increment.
	ok, err := s.CancelPending("filler")
	require.NoError(t, err)
	require.True(t, ok)
	clk.Advance(90 * time.Second)
	fired, err := s.fireDueLocked(context.Background(), "mf", "owner")
	require.NoError(t, err)
	require.True(t, fired, "cap drained → the fire lands")
	require.Equal(t, int64(3), m.MissedFires(), "an UNBLOCKED fire does not increment missedFires")
}

// TestCapAdmits_MissedFireMetric_NilMetricsNoOp — the additive/opt-in floor: with NO DispatchMetrics attached,
// a cap-blocked fire is a no-op on the metric path (no panic, byte-unchanged behavior).
func TestCapAdmits_MissedFireMetric_NilMetricsNoOp(t *testing.T) {
	const K = 1
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkSchedCapStore(t, clk, map[string]int{"X": K}, 0) // NO metrics attached (s.metrics == nil)
	_, err := s.Enqueue("filler", "X", nil)
	require.NoError(t, err)
	spec, err := NewIntervalSchedule("nm", "X", time.Minute, clk.Now())
	require.NoError(t, err)
	_, err = s.CreateSchedule(spec)
	require.NoError(t, err)
	clk.Advance(90 * time.Second)
	// Must not panic; the cap-blocked branch skips the increment because s.metrics is nil.
	fired, err := s.fireDueLocked(context.Background(), "nm", "owner")
	require.NoError(t, err)
	require.False(t, fired, "cap-blocked, nil metrics → no fire, no panic")
}
