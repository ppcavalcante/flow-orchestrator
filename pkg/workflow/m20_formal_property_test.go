package workflow

// M20 ph102 T1 — the EXECUTION ANCHOR for the formal capstone: a gopter property arm over the REAL SQLiteStore
// that mirrors the two machine-checked SAFETY arms (specs/M20Scheduling.tla NoDoubleFire + specs/M20Caps.tla
// CapNeverExceeded). The TLA proves the abstract protocol holds over ALL interleavings of a bounded model; this
// proves the Go IMPLEMENTATION refines it — random schedules × caps × concurrent poller/worker interleavings on
// the actual store. Each property is BITE-proven (a discriminator run Falsifies it) so it is non-vacuous
// ([[gate-vacuity-pin-the-input-axis]]).

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestM20Formal_NoDoubleFire_Property — the execution twin of M20Scheduling.tla's NoDoubleFire (arbiter =
// in-txn re-check, DEC-P100-RECHECK-IS-ARBITER). Over random poller counts + fire-iteration counts, N faithful
// pollers (separate stores on one DB) race a SINGLE due slot → EXACTLY ONE reports fired=true (the
// dedup-independent decision count, mirroring TestSchedule_InTxnRecheck_IsTheArbiter — NOT the queue-level dedup).
func TestM20Formal_NoDoubleFire_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 60 // each case spins goroutines + a real DB; keep the count modest but adversarial
	properties := gopter.NewProperties(params)

	properties.Property("N pollers racing one due slot → exactly one fire decision", prop.ForAll(
		func(nPollers, iters int) bool {
			return oneDueSlotFiresOnce(t, nPollers, iters)
		},
		gen.IntRange(2, 5), // number of racing pollers (separate stores on one DB)
		gen.IntRange(10, 40),
	))

	properties.TestingRun(t)
}

// oneDueSlotFiresOnce sets up ONE due schedule, races nPollers each hammering the fire `iters` times, and
// returns true iff EXACTLY ONE fire decision landed (the NoDoubleFire property on the real store).
//
// It uses an INTERVAL schedule (long period, due 1h ago) — NOT a one-shot — precisely so the IN-TXN RE-CHECK is
// the sole no-double-fire arbiter (mirroring TestSchedule_InTxnRecheck_IsTheArbiter). A one-shot would AUTO-DELETE
// after the first fire, and that delete (not the re-check) would mask a broken re-check → a vacuous property.
// With an interval schedule the row persists (only next_fire advances), so exactly-one-fire holds ONLY because
// the second poller's txn re-reads the advanced next_fire → not due. This is the dedup-independent decision count.
//
// CLOCK NOTE (review ph102-MINOR): this arm uses the REAL clock (no injected FakeClock) — safe because the single
// due slot is anchored an hour in the past and the next slot is +59m out, so wall-clock drift over the test can't
// create a second due slot. (The CapNeverExceeded arm DOES inject a frozen FakeClock — see concurrentClaimsRespectCap.)
func oneDueSlotFiresOnce(t *testing.T, nPollers, iters int) bool {
	t.Helper()
	db := filepath.Join(t.TempDir(), fmt.Sprintf("ndf-%d-%d.db", nPollers, iters))
	pre, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		return false
	}
	// An interval schedule due 1h ago, long period → exactly ONE slot is due in the window; the re-check (not a
	// delete) is what makes it fire once.
	spec, err := NewIntervalSchedule("slot", "T", time.Hour, time.Now().Add(-time.Hour-time.Minute))
	if err != nil {
		_ = pre.Close() //nolint:errcheck
		return false
	}
	if _, err := pre.CreateSchedule(spec); err != nil {
		_ = pre.Close() //nolint:errcheck
		return false
	}
	_ = pre.Close() //nolint:errcheck // pre-create only

	stores := make([]*SQLiteStore, nPollers)
	for i := range stores {
		s, oerr := NewSQLiteStore(db, WithMultiProcess())
		if oerr != nil {
			return false
		}
		defer s.Close() //nolint:errcheck // cleanup
		stores[i] = s
	}

	var firedTrue atomic.Int32
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(st *SQLiteStore, owner string) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if fired, _ := st.fireDueLocked(context.Background(), "slot", owner); fired { //nolint:errcheck // contention retries
					firedTrue.Add(1)
				}
			}
		}(s, fmt.Sprintf("p%d", i))
	}
	wg.Wait()
	return firedTrue.Load() == 1 // exactly one fire decision — the in-txn re-check arbitrates.
}

// TestM20Formal_CapNeverExceeded_Property — the execution twin of M20Caps.tla's CapNeverExceeded
// (DEC-P98-COUNT-IN-TXN). Over random caps + enqueued-item counts + concurrent claimers, the running
// (claimed∧¬parked) count of the type NEVER exceeds cap — the in-txn COUNT serializes the admission.
func TestM20Formal_CapNeverExceeded_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 60
	properties := gopter.NewProperties(params)

	properties.Property("N concurrent claimers under cap(X)=K → running never exceeds K", prop.ForAll(
		func(k, extra int) bool {
			return concurrentClaimsRespectCap(t, k, extra)
		},
		gen.IntRange(1, 4), // cap K
		gen.IntRange(1, 8), // extra items enqueued beyond K (the contended surplus)
	))

	properties.TestingRun(t)
}

// concurrentClaimsRespectCap enqueues K+extra type-X items, races (K+extra) claimers each on its OWN store
// sharing one DB file, and returns true iff the running (claimed∧¬parked) count is EXACTLY K.
//
// SEPARATE STORE PER CLAIMER is load-bearing (review ph102-F1): if all claimers shared ONE store, its s.mu would
// serialize every ClaimNext end-to-end, so ANY count placement (in-txn OR a stale outside-txn count) would yield
// an exact count — the DEC-P98-COUNT-IN-TXN TOCTOU the TLA CapNeverExceeded guards would be UNOBSERVABLE. With a
// store per claimer the SQLite BEGIN IMMEDIATE write lock (MaxOpenConns(1)) is the real cross-process serializer,
// so a stale count genuinely over-admits → the property can go RED on the guarded mechanism (mirrors the
// NoDoubleFire harness). Assert == k (not <= k): a <=k check stays green even if the gate wrongly claimed NOTHING
// ([[gate-vacuity-pin-the-input-axis]]).
//
// Each store gets its OWN frozen FakeClock (never advanced) so leases never lapse → no reclaim/completion path →
// the running count is monotone nondecreasing, so the post-race count == the during-race peak (no transient K+1
// could hide). If a future edit advances a clock or adds a release path here, that equivalence breaks — keep frozen.
func concurrentClaimsRespectCap(t *testing.T, k, extra int) bool {
	t.Helper()
	n := k + extra
	db := filepath.Join(t.TempDir(), fmt.Sprintf("cap-%d-%d.db", k, extra))
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(db, WithMultiProcess(), withSQLiteClock(NewFakeClock(time.Unix(1000, 0))),
			WithCaps(Caps{PerType: map[string]int{"X": k}}))
		if err != nil {
			t.Fatalf("open cap store: %v", err)
		}
		return s
	}
	seed := open()
	for i := 0; i < n; i++ {
		if _, err := seed.Enqueue(fmt.Sprintf("x%d", i), "X", nil); err != nil {
			_ = seed.Close() //nolint:errcheck // cleanup
			return false
		}
	}
	_ = seed.Close() //nolint:errcheck // seed only; each claimer opens its own store below

	stores := make([]*SQLiteStore, n)
	for i := range stores {
		stores[i] = open()
		defer stores[i].Close() //nolint:errcheck // cleanup
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(id int, st *SQLiteStore) {
			defer wg.Done()
			_, _ = st.ClaimNext(fmt.Sprintf("w%d", id), "X") //nolint:errcheck // backpressured claims return ErrNoWork
		}(w, stores[w])
	}
	wg.Wait()
	// EXACTLY K running slots — n>k items + a working in-txn gate ⇒ exactly K admitted, never K+1, never 0.
	return runningSlots(t, stores[0], "X") == k
}

// TestM20Formal_HelpersPositive — a fast POSITIVE guard on the property helpers (review ph102-MINOR: this is NOT
// the discriminating mutation itself — the real bites are the out-of-band mutation runs recorded in the seal /
// the SUMMARY: drop the in-txn re-check → NoDoubleFire Falsifies (verified 1.4s); neuter the ClaimNext gate OR
// claim-nothing → CapNeverExceeded Falsifies (verified); parked cap-consuming → the composition HANGS 18s). This
// test just asserts the helpers return the expected verdict on a KNOWN-GOOD store, so a broken property HELPER
// reddens here rather than silently passing. The non-vacuity of the properties rests on those out-of-band bites.
func TestM20Formal_HelpersPositive(t *testing.T) {
	// Positive check: both helpers report the expected verdict on a known-good store (a broken helper reddens here).
	if !oneDueSlotFiresOnce(t, 3, 20) {
		t.Fatal("NoDoubleFire property helper must report exactly-one-fire on the real store")
	}
	if !concurrentClaimsRespectCap(t, 2, 4) {
		t.Fatal("CapNeverExceeded property helper must report running ≤ cap on the real store")
	}
}
