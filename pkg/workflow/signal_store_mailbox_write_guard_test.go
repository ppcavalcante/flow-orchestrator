package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// The mailbox entry-count axis had a READ guard and no WRITE guard (F1). Every
// DeliverSignal returned nil past the bound; TakeSignals then rejected the WHOLE
// mailbox with ErrCorruptData, permanently — and the mailbox read is all-or-nothing,
// so one over-cap backlog fails a WaitForSignal run's take on every re-drive until the
// mailbox is drained out of band, with no knob able to rescue it. These tests pin the
// write side to the same bound the read side uses.
//
// Reproduced at HEAD before the fix, JSONFileStore at cap 3: four DeliverSignal calls
// returned nil, then TakeSignals returned
//   "corrupt workflow data: signal mailbox entry count exceeds max".

// TestMailboxWriteGuard_RefusesTheDeliveryThatWouldWedgeTheRead is the core arm: the
// delivery that would push the mailbox past the cap fails LOUD at the write, and the
// mailbox it refused to grow still reads clean afterwards. That second assertion is
// what makes this a fix rather than a relocation of the failure.
func TestMailboxWriteGuard_RefusesTheDeliveryThatWouldWedgeTheRead(t *testing.T) {
	const cap = 3
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-write-guard"
			for i := 0; i < cap; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "approve"}),
					"a delivery UNDER the cap must still succeed — no behavior change below the bound")
			}

			err := store.DeliverSignal(wf, Signal{ID: "one-too-many", Name: "approve"})
			require.Error(t, err, "the delivery that would exceed the cap must be refused at the WRITE")
			require.ErrorIs(t, err, ErrValidation,
				"ErrValidation, not ErrCorruptData: nothing is corrupt, the host over-delivered")
			// The message must name the resulting count AND the ceiling, so an operator
			// can see how far over the bound the mailbox is without guessing.
			require.Contains(t, err.Error(), fmt.Sprintf("%d entries", cap+1))
			require.Contains(t, err.Error(), fmt.Sprintf("%d-entry", cap))

			// The refusal left the mailbox readable — the whole point.
			got, terr := store.TakeSignals(wf)
			require.NoError(t, terr, "a refused delivery must not have wedged the read")
			require.Len(t, got, cap)
		})
	}
}

// TestMailboxWriteGuard_RedeliveryAtCapStillSucceeds pins the arm most likely to be
// broken by a naive count-only guard: DeliverSignal is idempotent by sig.ID, so
// re-delivering an ID already in the mailbox REPLACES its entry and does not grow the
// mailbox. Refusing it at the cap would break the documented idempotency contract for
// a workflow that is entirely within its bound.
func TestMailboxWriteGuard_RedeliveryAtCapStillSucceeds(t *testing.T) {
	const cap = 3
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-redeliver"
			for i := 0; i < cap; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "approve"}))
			}
			// Mailbox is exactly AT the cap. Re-delivering an existing ID with a new
			// payload must succeed and must last-writer-win, uniformly across stores.
			require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s001", Name: "approve", Payload: "updated"}),
				"re-delivering an EXISTING id does not grow the mailbox and must succeed at the cap")

			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, cap, "a re-delivery must not have added an entry")
			var found bool
			for _, s := range got {
				if s.ID == "s001" {
					found = true
					require.Equal(t, "updated", s.Payload, "last-writer-wins survived the guard")
				}
			}
			require.True(t, found)
		})
	}
}

// TestMailboxWriteGuard_ZeroCeilingRefusesTheFirstDelivery pins the boundary the
// file-store guard could most easily get wrong. deliverSignalToDir reads an absent
// mailbox dir as zero entries through the nil slice rather than short-circuiting on
// os.IsNotExist; a short-circuit would let the first delivery through at a ceiling of
// 0, which the read (len > cap) would then reject — re-arming the asymmetry at
// exactly one point.
func TestMailboxWriteGuard_ZeroCeilingRefusesTheFirstDelivery(t *testing.T) {
	orig := signalMailboxCap
	signalMailboxCap = 0
	t.Cleanup(func() { signalMailboxCap = orig })

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			err := store.DeliverSignal("wf-zero", Signal{ID: "s1", Name: "n"})
			require.ErrorIs(t, err, ErrValidation,
				"at a ceiling of 0 the read rejects any entry, so the write must refuse the first one")
		})
	}
}

// TestMailboxWriteGuard_RefusedDeliveryLeavesNothingBehind: the refusal must be
// complete, not partial. No entry file, and — for InMemoryStore, whose guard runs
// before the per-workflow map is created — no empty mailbox residue either.
//
// ONE DELIBERATE EXCEPTION, named so this test's own name is not a lie: on the file stores a
// refused delivery DOES leave <id>.signals.lock behind. The mailbox lock is acquired before
// the count — that ordering is what makes the count correct — so any delivery attempted past
// validation materializes it, including one refused a moment later. The lock file is never
// reclaimed by anything (see signalLockSuffix), so "nothing behind" means no entry file and no
// mailbox DIRECTORY. That is what the assertions below check, and it is the strongest form the
// property can take without giving up the lock ordering.
func TestMailboxWriteGuard_RefusedDeliveryLeavesNothingBehind(t *testing.T) {
	orig := signalMailboxCap
	signalMailboxCap = 0
	t.Cleanup(func() { signalMailboxCap = orig })

	t.Run("file store leaves no entry AND no mailbox", func(t *testing.T) {
		dir := t.TempDir()
		js, err := NewJSONFileStore(dir)
		require.NoError(t, err)
		require.ErrorIs(t, js.DeliverSignal("wf", Signal{ID: "s1", Name: "n"}), ErrValidation)

		mbox := filepath.Join(dir, "wf"+signalDirSuffix)
		_, serr := os.Stat(filepath.Join(mbox, "s1"+signalFileSuffix))
		require.True(t, os.IsNotExist(serr), "a refused delivery must not have written an entry file")

		// The stronger arm, and it was MISSING while its in-memory sibling below asserted
		// exactly this. b315997 moved MkdirAll ahead of the guard and a refused delivery began
		// leaving an empty <id>.signals/ behind; this test kept passing because it only checked
		// the entry file. A test weaker than its own name, and weaker than its own sibling —
		// the two stores disagreed about a property one of them advertised in a comment.
		_, derr := os.Stat(mbox)
		require.True(t, os.IsNotExist(derr),
			"a refused delivery must not have created the mailbox dir either — 'leaves nothing behind' "+
				"means nothing, and the in-memory arm below holds itself to exactly that")
	})

	t.Run("in-memory leaves no empty mailbox", func(t *testing.T) {
		s := NewInMemoryStore()
		require.ErrorIs(t, s.DeliverSignal("wf", Signal{ID: "s1", Name: "n"}), ErrValidation)
		s.mu.RLock()
		_, ok := s.signals["wf"]
		s.mu.RUnlock()
		require.False(t, ok, "a refused delivery must not have created the per-workflow mailbox")
	})
}

// TestMailboxWriteGuard_CountsTheSameQuantityTheReadCounts is the anti-drift leg, and
// it is the one that catches the subtle wrong fix. takeSignalsFromDir bounds
// len(os.ReadDir(dir)) — ALL directory entries, its own deliberate over-approximation
// (non-.sig files, crash-left temp files). A write guard that counted only *.sig
// entries instead would look correct and still let a delivery through that the read
// then rejects, which is precisely the asymmetry being closed.
//
// Seeded with a stray non-.sig file, so the two sides can only agree if the writer
// counts the way the reader does.
func TestMailboxWriteGuard_CountsTheSameQuantityTheReadCounts(t *testing.T) {
	const cap = 3
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	dir := t.TempDir()
	js, err := NewJSONFileStore(dir)
	require.NoError(t, err)
	const wf = "wf-stray"

	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s1", Name: "n"}))
	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s2", Name: "n"}))

	// A stray non-.sig file: invisible to a *.sig count, but counted by the read.
	mbox := filepath.Join(dir, wf+signalDirSuffix)
	require.NoError(t, os.WriteFile(filepath.Join(mbox, "leftover.tmp"), []byte("x"), 0600))

	// The mailbox now holds 3 dir entries — exactly the cap — so it reads clean and
	// one more delivery must be refused.
	_, terr := js.TakeSignals(wf)
	require.NoError(t, terr, "3 entries is AT the cap and must read clean")

	derr := js.DeliverSignal(wf, Signal{ID: "s3", Name: "n"})
	require.ErrorIs(t, derr, ErrValidation,
		"the writer must count what the reader counts (all dir entries), or it passes a write the read rejects")

	// And the read is still clean — proof the two sides agree rather than both being wrong.
	got, terr2 := js.TakeSignals(wf)
	require.NoError(t, terr2)
	require.Len(t, got, 2, "only the two real .sig entries decode; the stray file is skipped")
}

// TestMailboxWriteGuard_AckSignalsIsUncappedOnAnOverCapMailbox pins that the drain
// path stays usable when the read path is refusing: AckSignals has no cap check on any
// store, so a caller who HOLDS ids can still remove them from an over-cap mailbox. A
// future cap check added there would red this.
//
// What this does NOT establish, stated because an earlier version of this comment
// claimed it and the claim was false: **AckSignals is not a recovery path**, and it is
// not why this axis has no knob. Its signature is AckSignals(workflowID, ids) — the
// caller must already hold the IDs, and in the scenario that would matter (a consumer
// restarting on top of an over-cap mailbox) it holds none, because TakeSignals is the
// only enumeration path and TakeSignals is the call that is failing. This test knows
// the IDs only because it just delivered them. Recovery there is out-of-band; see the
// residual in the SignalStore contract block.
//
// The reasons the no-knob decision actually rests on are in checkMailboxEntries, and there
// are TWO of them, not three: 2^20 is an absurdity ceiling rather than a tuning parameter,
// and the bound is an interface-level contract rather than per-store format state. The third
// reason this comment used to list — "post-guard the over-cap state is API-unreachable" — was
// struck from the design record as DEAD/False and is deliberately not restored here. It has
// now been falsified three times: the non-unix no-op lock leaves it reachable, the unguarded
// count under concurrent delivery reached it, and an ack racing the lock-free re-delivery fast
// path reached it again. "Unreachable" was never a property of the guard's existence, only of
// its correctness, and it is not the kind of claim that should carry a design decision.
func TestMailboxWriteGuard_AckSignalsIsUncappedOnAnOverCapMailbox(t *testing.T) {
	const cap = 3
	orig := signalMailboxCap

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-recover"
			// Reach the over-cap state the only way that remains: deliver under a high
			// ceiling, then lower it — the stand-in for pre-guard state on disk.
			signalMailboxCap = orig
			for i := 0; i < cap+1; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n"}))
			}
			signalMailboxCap = cap
			t.Cleanup(func() { signalMailboxCap = orig })

			_, terr := store.TakeSignals(wf)
			require.ErrorIs(t, terr, ErrCorruptData, "sanity: the mailbox is over the bound and unreadable")

			require.NoError(t, store.AckSignals(wf, []string{fmt.Sprintf("s%03d", cap)}),
				"AckSignals must have no cap check — a caller holding ids can still drain")

			got, terr2 := store.TakeSignals(wf)
			require.NoError(t, terr2, "draining one entry restores the read")
			require.Len(t, got, cap)
		})
	}
}

// TestMailboxWriteGuard_HoldsUnderConcurrentDelivery is the arm whose ABSENCE let a blocker
// ship. Six guard tests above and not one of them delivered concurrently, so a guard that
// counted outside any lock passed all six — the structural gap, and it matters more than the
// fix. A guard is a claim about an invariant, and an invariant untested under concurrency is
// tested only against the author's imagination of the schedule.
//
// Measured at 264265d, BEFORE the fix, cap 8 seeded with 7:
//   - file stores, 16 goroutines: accepted 4-5, mailbox left with 11-12 entries, TakeSignals
//     then returned ErrCorruptData on every run.
//   - SQLite, 8 separate handles on ONE db file: accepted 8, refused ZERO, 15 rows. The
//     store's s.mu is process-local, so it defended nothing in the multi-process case this
//     store exists for (M16 competing consumers).
//
// The bound is the same one TakeSignals enforces, so the assertion is simply that the read
// still works afterwards: if the mailbox is over the bound the read rejects it, which is the
// wedge. Racing deliveries of DISTINCT new ids is the shape that breaks a count-then-write
// guard; re-deliveries of the same id cannot, since they overwrite in place.
func TestMailboxWriteGuard_HoldsUnderConcurrentDelivery(t *testing.T) {
	const cap = 8
	const seeded = cap - 1
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-concurrent"
			for i := 0; i < seeded; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("seed%03d", i), Name: "n"}))
			}

			var accepted int32
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if err := store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("new%03d", i), Name: "n"}); err == nil {
						atomic.AddInt32(&accepted, 1)
					}
				}(i)
			}
			wg.Wait()

			// ONE composite assertion rather than three sequential ones, so the failure text
			// carries the whole breach instead of only whichever check fires first. A
			// readable TakeSignals implies the mailbox is within the cap, since the read
			// rejects an over-cap mailbox — so these two facts together are the invariant.
			acc := int(atomic.LoadInt32(&accepted))
			free := cap - seeded
			got, terr := store.TakeSignals(wf)
			require.Falsef(t, acc > free || terr != nil,
				"mailbox cap BREACHED under concurrent delivery: cap=%d seeded=%d freeSlots=%d "+
					"accepted=%d TakeSignals_err=%v returned=%d. The count is being read OUTSIDE the "+
					"write lock, so every racer observed the same pre-count and all committed; the read "+
					"then rejects the whole mailbox and a WaitForSignal run's take fails on every re-drive.",
				cap, seeded, free, acc, terr, len(got))

			// LIVENESS, and it is a separate obligation from the safety assertion above. That
			// one is satisfied by a store that refuses EVERY delivery — a mutation doing exactly
			// that left this arm green, which made the 2-proc arm the only place liveness was
			// tested at all. A spuriously refused signal is not a visible error; it is a park
			// that never wakes, the same wedge from the other direction.
			//
			// Equality, not a floor: deliveries serialize (flock on the file stores, one atomic
			// statement on SQLite, the map mutex in memory), so the racer that wins fills the
			// last slot and every subsequent one is refused against a full mailbox. Exactly
			// freeSlots admitted is deterministic here, not a race-dependent range.
			require.Equalf(t, free, acc,
				"exactly the %d free slot(s) must be FILLED, not left empty: a guard that refuses "+
					"every delivery satisfies the cap and wedges the host instead", free)
			require.Len(t, got, cap, "the mailbox must end exactly AT the cap — %d seeded + %d admitted", seeded, free)
		})
	}
}

// TestMailboxWriteGuard_ConcurrentRedeliveryIsNotRefused: N racing re-deliveries of the SAME
// id cannot grow the mailbox and must all succeed even with the mailbox exactly AT the cap.
// This is the liveness half of the re-delivery contract — the safety half, an ack racing an
// in-flight re-delivery, is TestMailboxWriteGuard_AckRacingAnInFlightRedeliveryCannotBreachTheCap.
//
// It is the arm that pins the cost of closing that safety hole. These re-deliveries used to
// run in parallel through a lock-free fast path; they now serialize on the mailbox flock, so
// this test got measurably slower and that is the trade being made deliberately, not a
// regression to optimize away. Do not reintroduce a lock-free re-delivery path to speed it up.
func TestMailboxWriteGuard_ConcurrentRedeliveryIsNotRefused(t *testing.T) {
	const cap = 4
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-conc-redeliver"
			for i := 0; i < cap; i++ {
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n"}))
			}

			errs := make([]error, 12)
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs[i] = store.DeliverSignal(wf, Signal{ID: "s000", Name: "n", Payload: "updated"})
				}(i)
			}
			wg.Wait()

			for i, e := range errs {
				require.NoError(t, e, "re-delivery %d of an existing id must never be refused (it cannot grow the mailbox)", i)
			}
			got, err := store.TakeSignals(wf)
			require.NoError(t, err)
			require.Len(t, got, cap, "re-deliveries must not have added entries")
		})
	}
}

// TestMailboxWriteGuard_SQLiteHoldsAcrossSeparateHandles exists because the
// cross-store concurrent arm above does NOT cover the case that was actually broken, and
// I only learned that by biting it: signalStores(t) hands every goroutine the SAME
// *SQLiteStore, so the store's process-local s.mu serializes them and the arm stays green
// even with the count and the insert split back into two statements.
//
// The defect was never in-process. It was two HANDLES on one database file — the
// multi-process shape SQLiteStore exists for (M16 competing consumers) — where two
// mutexes defend nothing. Measured before the fix: 8 handles, cap 8 seeded with 7, 8
// accepted, ZERO refused, 15 rows.
//
// Separate handles on one file is the closest in-process analogue of separate processes:
// distinct sql.DB pools, distinct s.mu, one shared database. If this arm is ever deleted
// as "redundant with the arm above", the multi-process guarantee loses its only test.
func TestMailboxWriteGuard_SQLiteHoldsAcrossSeparateHandles(t *testing.T) {
	const cap = 8
	const seeded = cap - 1
	orig := signalMailboxCap
	signalMailboxCap = cap
	t.Cleanup(func() { signalMailboxCap = orig })

	dbPath := filepath.Join(t.TempDir(), "signals.db")
	seed, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { seed.Close() }) //nolint:errcheck // test cleanup; a close error cannot affect the assertions above

	const wf = "wf-handles"
	for i := 0; i < seeded; i++ {
		require.NoError(t, seed.DeliverSignal(wf, Signal{ID: fmt.Sprintf("seed%03d", i), Name: "n"}))
	}

	var accepted int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, oerr := NewSQLiteStore(dbPath) // its OWN handle, its OWN s.mu
			if oerr != nil {
				t.Error(oerr)
				return
			}
			defer s.Close() //nolint:errcheck // test cleanup
			if e := s.DeliverSignal(wf, Signal{ID: fmt.Sprintf("new%03d", i), Name: "n"}); e == nil {
				atomic.AddInt32(&accepted, 1)
			}
		}(i)
	}
	wg.Wait()

	acc := int(atomic.LoadInt32(&accepted))
	free := cap - seeded
	got, terr := seed.TakeSignals(wf)
	require.Falsef(t, acc > free || terr != nil,
		"mailbox cap BREACHED across SEPARATE HANDLES: cap=%d seeded=%d freeSlots=%d accepted=%d "+
			"TakeSignals_err=%v returned=%d. The COUNT is being taken outside the write lock, so the "+
			"store's process-local s.mu is the only thing serializing deliveries — and it does not span "+
			"handles, which is the multi-process case this store exists for.",
		cap, seeded, free, acc, terr, len(got))

	// LIVENESS — see the same pair in TestMailboxWriteGuard_HoldsUnderConcurrentDelivery. The
	// safety assertion above is satisfied by an INSERT whose WHERE never matches; the free slot
	// must actually be filled.
	require.Equalf(t, free, acc,
		"exactly the %d free slot(s) must be FILLED across handles, not left empty", free)
	require.Len(t, got, cap, "the mailbox must end exactly AT the cap")
}
