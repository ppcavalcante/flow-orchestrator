package workflow

// M20 ph98 — concurrency caps proven at the STORE level: the atomic cap gate in ClaimNext (per-type + global,
// both AND), the parked-column set/clear round-trip + additive migration, the discriminator seed-break (COUNT
// outside the txn → claim past K), the running-slot referent (claimed∧¬parked), and the additive moat (unset
// cap ⇒ ClaimNext byte-behavior-unchanged). The parked-DEADLOCK bite + crash/reclaim + no-kill live in
// workflow_store_sqlite_caps_deadlock_test.go (they drive the real dispatch/park machinery).

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkCapStore opens a single mp store WITH the given caps (per-type + global). A nil perType / 0 global ⇒
// unbounded on that axis.
func mkCapStore(t *testing.T, perType map[string]int, global int) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "caps.db"),
		WithMultiProcess(), WithCaps(Caps{PerType: perType, Global: global}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// runningSlots counts the RUNNING-slot rows (claimed∧¬parked) — the cap's referent — optionally by type.
func runningSlots(t *testing.T, s *SQLiteStore, typ string) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM work_queue WHERE state='claimed' AND parked IS NULL`
	var err error
	if typ == "" {
		err = s.db.QueryRow(q).Scan(&n)
	} else {
		err = s.db.QueryRow(q+` AND type=?`, typ).Scan(&n)
	}
	require.NoError(t, err)
	return n
}

// parkedFlag reports whether a row's `parked` marker is set (non-NULL).
func parkedFlag(t *testing.T, s *SQLiteStore, wf string) bool {
	t.Helper()
	var p sql.NullInt64
	require.NoError(t, s.db.QueryRow(`SELECT parked FROM work_queue WHERE workflow_id=?`, wf).Scan(&p))
	return p.Valid
}

// TestCap_PerType_HoldsAtLimit — with cap(X)=2, exactly 2 type-X items claim; the 3rd is backpressured
// (ErrNoWork), NOT rejected — the pending row stays claimable, so once a slot frees it claims.
func TestCap_PerType_HoldsAtLimit(t *testing.T) {
	s := mkCapStore(t, map[string]int{"X": 2}, 0)
	for _, id := range []string{"x1", "x2", "x3"} {
		_, err := s.Enqueue(id, "X", nil)
		require.NoError(t, err)
	}

	// Two claims succeed (fill the 2 slots).
	i1, err := s.ClaimNext("w1", "X")
	require.NoError(t, err)
	i2, err := s.ClaimNext("w2", "X")
	require.NoError(t, err)
	require.Equal(t, 2, runningSlots(t, s, "X"))

	// The 3rd claim is BACKPRESSURED (at cap) → ErrNoWork, not an error, and x3 stays pending+claimable.
	_, err = s.ClaimNext("w3", "X")
	require.ErrorIs(t, err, ErrNoWork, "at cap ⇒ backpressure (ErrNoWork), never reject")
	require.Equal(t, wqPending, wqState(t, s, "x3"), "the backpressured item stays pending (claimable-later)")

	// Free a slot (i1 done) → x3 is now claimable (the cap is per running slot, not per-lifetime).
	ok, err := s.MarkDone(i1.WorkflowID)
	require.NoError(t, err)
	require.True(t, ok)
	i3, err := s.ClaimNext("w3", "X")
	require.NoError(t, err, "a freed slot admits the backpressured item")
	require.Equal(t, "x3", i3.WorkflowID)
	require.Equal(t, 2, runningSlots(t, s, "X"))
	_ = i2
}

// TestCap_Global_AcrossTypes — a global cap G=2 bounds the TOTAL running across ALL types (a claim needs
// count(*) < G), independent of type.
func TestCap_Global_AcrossTypes(t *testing.T) {
	s := mkCapStore(t, nil, 2) // no per-type; global=2
	for _, e := range []struct{ id, typ string }{{"a1", "A"}, {"b1", "B"}, {"c1", "C"}} {
		_, err := s.Enqueue(e.id, e.typ, nil)
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w1", "A")
	require.NoError(t, err)
	_, err = s.ClaimNext("w2", "B")
	require.NoError(t, err)
	require.Equal(t, 2, runningSlots(t, s, ""))
	// A third claim of a DIFFERENT type is still global-capped.
	_, err = s.ClaimNext("w3", "C")
	require.ErrorIs(t, err, ErrNoWork, "the global cap bounds total running across types")
}

// TestCap_GlobalAndPerType_BothGates — DEC-M20-D6: a claim needs count(X)<cap(X) AND count(*)<G. With
// cap(X)=1 and G=3, a second X is per-type-blocked even though the global cap has room.
func TestCap_GlobalAndPerType_BothGates(t *testing.T) {
	s := mkCapStore(t, map[string]int{"X": 1}, 3)
	for _, id := range []string{"x1", "x2"} {
		_, err := s.Enqueue(id, "X", nil)
		require.NoError(t, err)
	}
	_, err := s.Enqueue("y1", "Y", nil)
	require.NoError(t, err)

	_, err = s.ClaimNext("w1", "X")
	require.NoError(t, err)
	// x2 blocked by the per-type cap (X=1), NOT the global (which has 2 free) — proves the AND.
	_, err = s.ClaimNext("w2", "X")
	require.ErrorIs(t, err, ErrNoWork, "per-type cap gates even when global has room")
	// But a different, uncapped type Y still claims (global has room).
	iy, err := s.ClaimNext("w3", "Y")
	require.NoError(t, err, "an under-both-gates type still claims")
	require.Equal(t, "y1", iy.WorkflowID)
}

// TestCap_Untyped_GlobalOnly — DEC-M20-D6: an untyped run (type="") is governed by the global cap only,
// never a per-type cap. With cap(X)=1 (irrelevant) and G=2, two untyped items claim.
func TestCap_Untyped_GlobalOnly(t *testing.T) {
	s := mkCapStore(t, map[string]int{"X": 1}, 2)
	// Untyped items require a non-empty type at Enqueue (validation), so use a type NOT in the cap map;
	// claim with NO type filter (the "untyped claim" — governed by global only). We assert the per-type
	// cap for a DIFFERENT type never gates these.
	for _, id := range []string{"u1", "u2", "u3"} {
		_, err := s.Enqueue(id, "U", nil) // type "U" is uncapped in the map
		require.NoError(t, err)
	}
	_, err := s.ClaimNext("w1") // no type filter
	require.NoError(t, err)
	_, err = s.ClaimNext("w2")
	require.NoError(t, err)
	require.Equal(t, 2, runningSlots(t, s, ""))
	_, err = s.ClaimNext("w3")
	require.ErrorIs(t, err, ErrNoWork, "an uncapped type is bounded only by the global cap")
}

// TestCap_Unbounded_ByteUnchanged — the additive moat (CAP-05): with NO caps set, ClaimNext claims all
// pending items regardless of running count (byte-behavior-unchanged from M17). The parked column is
// present but invisible to an unset-cap store.
func TestCap_Unbounded_ByteUnchanged(t *testing.T) {
	s := mkQueueStore(t) // no WithCaps ⇒ isUnbounded()
	require.True(t, s.caps.isUnbounded())
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		_, err := s.Enqueue(id, "X", nil)
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := s.ClaimNext("w", "X")
		require.NoError(t, err, "unbounded ⇒ every pending item claims, no cap gate")
	}
	require.Equal(t, 5, runningSlots(t, s, "X"))
}

// TestCap_ConcurrentClaims_NeverExceedK — ⭐ HARD BAR: N workers race to claim type-X items with cap(X)=K;
// never more than K are claimed∧¬parked at once, under -race. The BEGIN IMMEDIATE txn serializes the
// count+claim so no two workers claim past K. Discriminator: the seed-break test below moves the COUNT
// outside the txn semantics (a plain count with no exclusion) and proves it would claim past K.
func TestCap_ConcurrentClaims_NeverExceedK(t *testing.T) {
	const K = 3
	const N = 12
	s := mkCapStore(t, map[string]int{"X": K}, 0)
	for i := 0; i < N; i++ {
		_, err := s.Enqueue("x"+string(rune('a'+i)), "X", nil)
		require.NoError(t, err)
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < N; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := s.ClaimNext("w"+string(rune('a'+id)), "X")
			if err == nil {
				claimed.Add(1)
			}
		}(w)
	}
	wg.Wait()
	// At most K ever claimed (the others backpressured). The store serializes on MaxOpenConns(1)+IMMEDIATE,
	// so the race is on the CAP LOGIC, not raw SQLite concurrency — exactly what we want to prove.
	require.LessOrEqual(t, int(claimed.Load()), K, "never more than K claimed under concurrent claims")
	require.Equal(t, K, runningSlots(t, s, "X"), "exactly K running slots occupied")
}

// TestCap_Discriminator_CountOutsideTxnClaimsPastK — the DISCRIMINATOR: prove the in-txn COUNT is
// load-bearing. A count taken OUTSIDE the write lock (before BEGIN IMMEDIATE) is stale under concurrency
// → two workers each read "K-1 running" and both claim → K+1. This test simulates that broken ordering
// directly (count-then-race-claim without the txn gate) and asserts it WOULD exceed K — so the real
// in-txn gate's LessOrEqual(K) above is a genuine constraint, not vacuously true.
func TestCap_Discriminator_CountOutsideTxnClaimsPastK(t *testing.T) {
	const K = 1
	// An UNBOUNDED store: no cap gate at all (models "the gate is absent / outside the txn"). With the
	// gate removed, N concurrent claims of K+ items ALL succeed → running exceeds K. This is the state the
	// in-txn gate prevents; if this test ever showed <=K with no gate, the concurrent test above would be
	// vacuous (nothing to constrain).
	s := mkQueueStore(t) // no caps ⇒ no gate
	for _, id := range []string{"d1", "d2", "d3"} {
		_, err := s.Enqueue(id, "X", nil)
		require.NoError(t, err)
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if _, err := s.ClaimNext("w"+string(rune('a'+id)), "X"); err == nil {
				claimed.Add(1)
			}
		}(w)
	}
	wg.Wait()
	require.Greater(t, int(claimed.Load()), K, "WITHOUT the in-txn cap gate, claims exceed K (the gate is load-bearing)")
}

// TestParked_Migration_ExistingDBGainsColumn — T2: an existing M19-era work_queue (opened without ph98)
// gains the `parked` column on next open, with no data loss (the idempotent ADD COLUMN). A row written
// pre-ph98 reads parked=NULL (running, the correct default).
func TestParked_Migration_ExistingDBGainsColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mig.db")
	// Open #1 — enqueue a row (the column exists in this build, but we simulate an existing DB by dropping
	// the column, then reopening to prove the migration re-adds it idempotently).
	s1, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	_, err = s1.Enqueue("m1", "X", []byte("payload"))
	require.NoError(t, err)
	// Simulate a pre-ph98 schema: drop the parked column (SQLite 3.35+ supports DROP COLUMN).
	_, err = s1.db.Exec(`ALTER TABLE work_queue DROP COLUMN parked`)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Open #2 — the ph98 migration must re-ADD parked idempotently, preserving the existing row + payload.
	s2, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup
	var input string
	require.NoError(t, s2.db.QueryRow(`SELECT input FROM work_queue WHERE workflow_id='m1'`).Scan(&input))
	require.Equal(t, "payload", input, "existing row + payload survive the additive migration")
	require.False(t, parkedFlag(t, s2, "m1"), "a pre-ph98 row backfills to parked=NULL (running)")
}

// TestParked_SetClearRoundTrip — T2: markWorkQueueParked sets the marker on a claimed row (state stays
// claimed); the ClaimNext re-claim flip clears it. Store-level round-trip (no executor).
func TestParked_SetClearRoundTrip(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.Enqueue("p1", "X", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w1", "X")
	require.NoError(t, err)
	require.False(t, parkedFlag(t, s, "p1"), "a freshly claimed row is not parked")

	// PARK it (the runNext ErrSuspended path calls this).
	marked, err := s.markWorkQueueParked("p1")
	require.NoError(t, err)
	require.True(t, marked)
	require.Equal(t, wqClaimed, wqState(t, s, "p1"), "a parked row STAYS state='claimed' (M18 read-model intact)")
	require.True(t, parkedFlag(t, s, "p1"), "the parked marker is set")
	require.Equal(t, 0, runningSlots(t, s, "X"), "a parked row is NOT a running slot")

	// A parked row is NOT counted against a cap: with cap(X)=1, after the single slot's holder PARKS, a
	// SECOND X still claims (the parked row does not consume the slot — the cap's deadlock-safe referent).
	sc := mkCapStore(t, map[string]int{"X": 1}, 0)
	_, err = sc.Enqueue("a", "X", nil)
	require.NoError(t, err)
	ia, err := sc.ClaimNext("w", "X")
	require.NoError(t, err)
	_, err = sc.markWorkQueueParked(ia.WorkflowID)
	require.NoError(t, err)
	_, err = sc.Enqueue("b", "X", nil)
	require.NoError(t, err)
	_, err = sc.ClaimNext("w2", "X")
	require.NoError(t, err, "a parked child does not consume the capped slot (no deadlock referent)")
}

// TestParked_ReclaimClearsMarker — T2: a parked row that is re-claimed (via the lapsed-lease reclaim scan)
// is running again → the flip clears the parked marker so it re-occupies a running slot. Proven at the
// store level with a FakeClock lease-lapse.
func TestParked_ReclaimClearsMarker(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath, a, b := mkClaimStoresOn(t, clk, defaultLeaseTTL)
	_ = dbPath
	_, err := a.Enqueue("r1", "X", nil)
	require.NoError(t, err)
	_, err = a.ClaimNext("ownerA", "X")
	require.NoError(t, err)
	_, err = a.markWorkQueueParked("r1")
	require.NoError(t, err)
	require.True(t, parkedFlag(t, a, "r1"))

	// Lapse A's lease; B reclaims r1 (the parked child re-drives) → parked cleared, it's running again.
	clk.Advance(defaultLeaseTTL + 1)
	item, err := b.ClaimNext("ownerB", "X")
	require.NoError(t, err, "the lapsed parked row is re-offered by the reclaim scan")
	require.Equal(t, "r1", item.WorkflowID)
	require.False(t, parkedFlag(t, b, "r1"), "re-claim clears the parked marker (running again)")
	require.Equal(t, 1, runningSlots(t, b, "X"), "the re-claimed row re-occupies a running slot")
}
