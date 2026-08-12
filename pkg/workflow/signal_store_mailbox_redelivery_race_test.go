package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The regression pair for the SECOND blocker on the mailbox entry-count guard, and the reason
// there is no lock-free fast path for re-delivery.
//
// The deleted fast path was `if os.Stat(path) == nil { writeFileAtomic; return }`, on the
// premise that a re-delivery of an existing id "cannot grow the mailbox". That premise holds
// only if the observed entry STILL EXISTS when the rename lands, and ackSignalsInDir takes no
// lock and just os.Removes. Interleaving, mailbox exactly at cap:
//
//  1. G1 re-delivers s000, Stat hits, enters writeFileAtomic holding NO lock
//  2. a consumer acks s000                                          -> cap-1 entries
//  3. G2 delivers a NEW id: under the flock it counts cap-1, passes -> cap entries
//  4. G1's rename lands                                             -> cap+1, over the bound
//
// MEASURED at 6855050, cap 4: 5 entries and TakeSignals returned
// "corrupt workflow data: signal mailbox entry count exceeds max" — the permanent wedge.
//
// Reproduced INDEPENDENTLY twice: by the engineer who built the fix, and by the round's
// independent code reviewer, which authored neither the code nor the fix. Both landed on
// cap=4 -> 5 entries with the same TakeSignals text. The reviewer's probe is archived at
// .planning/phases/116-spine-and-hygiene/reviewer-probe-fastpath-ack-race.go.txt; this file is
// the permanent version and merges the stronger half of each.
//
// THE FIXTURE CARRIES THE EVIDENCE. Each property below is a way a later tidy makes this pass
// VACUOUSLY, which this phase has already been bitten by twice:
//
//  1. The pause is a one-shot atomic CAS, NOT sync.Once. Once.Do BLOCKS every other caller
//     until the first returns, and the first is parked inside on purpose — so a second caller
//     reaching it deadlocks rather than proceeding. The id filter is kept alongside it because
//     the two guard different things: the CAS makes the pause one-shot, the filter targets it
//     by IDENTITY so an unrelated createTempFile call can never be the one parked.
//     TO BE PRECISE ABOUT WHAT THEY DO TODAY: NEITHER is load-bearing under the current
//     schedule — nothing else calls createTempFile between arming and the race, and
//     writeFileAtomic calls it exactly once. They guard FUTURE edits, which is the reason to
//     keep them, and saying they are presently required would be the same overstatement this
//     phase keeps producing.
//  2. The pause lands BEFORE the real createTempFile. A temp file is itself a directory entry
//     and the locked deliverer's os.ReadDir counts ALL entries, so creating it first perturbs
//     the count and hides the breach behind a spurious refusal.
//  3. The re-delivered id must ALREADY be in the mailbox. A new id never took the fast path,
//     so the test would exercise the locked path and prove nothing.
//  4. The ack must be of the SAME id being re-delivered. Acking a different id frees a slot
//     without invalidating the fast path's premise — no breach.
//  5. The mailbox must be exactly AT cap when the race starts, and seeding must happen BEFORE
//     the seam is armed, or the seeding deliveries trip the pause and hang.
//  6. THE WAIT FOR THE SEAM MUST BE BOUNDED — see awaitSeam. A bare `<-reached` blocks forever
//     if the delivery never reaches writeFileAtomic, so the suite dies on go test's -timeout
//     and prints FAIL: a hang that is INDISTINGUISHABLE from a real breach, with no diagnostic
//     and 30 minutes of the gate spent. Reproduced by a plausible tidy ("skip the redundant
//     write when the payload is unchanged"), which turned this test into a 300-second hang
//     rather than a failure. A fixture that cannot reach its own precondition is a FIXTURE
//     failure and must say so.
//
// Neither arm may take t.Parallel(): both mutate package globals (signalMailboxCap,
// createTempFile).

// awaitSeam waits for a test seam to be reached, bounded, and fails as a FIXTURE error rather
// than hanging. The distinction is the whole point: a breach means the guard is broken, a hang
// means the test never got far enough to find out, and a -timeout panic reports both as FAIL.
//
// 30s, not 5s: this package carries load-sensitive tests and the callers already use a 2s
// window for the racing delivery. A tight bound here would manufacture exactly the false FAIL
// this exists to remove. The correct schedule never comes near it — the seam is reached in
// milliseconds — so a fire means the path really is gone, not that the host was busy.
//
// ON THE TIMEOUT PATH t.Fatalf calls runtime.Goexit, so the caller's wg.Wait() is NEVER
// reached and the delivery goroutine outlives the test. Checked, and benign: that goroutine
// touches no *testing.T, and by hypothesis nothing is parked on the release channel — if
// nothing reached the seam, nothing is waiting to be released. Stated rather than left for
// someone to rediscover in a goroutine dump.
func awaitSeam(t *testing.T, reached <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatalf("FIXTURE FAILURE, not a cap breach: %s within 30s. This test proves nothing "+
			"about the entry-count guard — the code path it instruments was never entered, so "+
			"do not read this as a passing or failing guard. Check that DeliverSignal still "+
			"routes a re-delivery through writeFileAtomic and that the createTempFile seam still "+
			"matches the seeded id.", what)
	}
}

// mailboxRaceCap is the lowered cap both arms and the helper share. One constant rather than
// three copies: a drifted copy would not have been VACUOUS (the composite assertion's takeErr
// limb reds regardless, because TakeSignals bounds against the real signalMailboxCap and not
// the test's copy) but it would have been confusing, and there is no reason to make a reader
// prove that for themselves.
const mailboxRaceCap = 4

// mailboxRedeliveryRace runs the interleaving once. withAck is the ONLY difference between the
// two arms — same fixture, same schedule, one line — which is what lets the control isolate the
// cause instead of merely agreeing with the breach case.
func mailboxRedeliveryRace(t *testing.T, wf string, withAck bool) (entries int, takeErr, reErr, newErr error) {
	t.Helper()
	const capN = mailboxRaceCap
	orig := signalMailboxCap
	signalMailboxCap = capN
	t.Cleanup(func() { signalMailboxCap = orig })

	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)

	// Seed to exactly the cap BEFORE arming the seam (property 5).
	for i := 0; i < capN; i++ {
		require.NoError(t, js.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n"}))
	}
	mbox := filepath.Join(base, wf+signalDirSuffix)

	reached := make(chan struct{})
	release := make(chan struct{})
	var armed atomic.Bool
	prev := createTempFile
	t.Cleanup(func() { createTempFile = prev })
	createTempFile = func(d, pattern string) (atomicTempFile, error) {
		if strings.Contains(pattern, "s000") && armed.CompareAndSwap(true, false) {
			close(reached)
			<-release
		}
		return prev(d, pattern) // the pause is BEFORE the real create (property 2)
	}
	armed.Store(true)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A RE-delivery of an id ALREADY PRESENT (property 3).
		reErr = js.DeliverSignal(wf, Signal{ID: "s000", Name: "n", Payload: "updated"})
	}()
	awaitSeam(t, reached, "the re-delivery never reached writeFileAtomic")

	if withAck {
		// The consumer acks the VERY id being re-delivered (property 4) — the
		// take -> apply -> checkpoint -> ack ordering racing an at-least-once re-delivery.
		require.NoError(t, js.AckSignals(wf, []string{"s000"}))
	}

	// A new delivery races for the slot. Pre-fix it commits immediately; post-fix it BLOCKS on
	// the flock, so it runs in its own goroutine with a bounded window rather than inline.
	// Waiting for it inline is what deadlocked the first draft of this test.
	newDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(newDone)
		newErr = js.DeliverSignal(wf, Signal{ID: "brand-new", Name: "n"})
	}()
	select {
	case <-newDone:
	case <-time.After(2 * time.Second):
	}
	close(release)
	wg.Wait()

	des, rerr := os.ReadDir(mbox)
	require.NoError(t, rerr)
	_, takeErr = js.TakeSignals(wf)

	// The re-delivered entry is ON DISK. Defence in depth, and honestly labelled as such: an
	// earlier revision asserted this and the helper's count-only return dropped it. The
	// reviewer tried to demonstrate that dropping it opened a silent-drop hole and FAILED to —
	// the mutation that silently drops the write is precisely the one that makes the fixture
	// hang rather than pass, so awaitSeam catches it first. Restored because it is one line and
	// pins the outcome directly rather than through a count, NOT because a hole was shown.
	_, sigErr := os.Stat(filepath.Join(mbox, "s000"+signalFileSuffix))
	require.NoError(t, sigErr, "the re-delivered entry must be present on disk after the race")

	t.Logf("withAck=%v entries=%d takeErr=%v reErr=%v newErr=%v", withAck, len(des), takeErr, reErr, newErr)
	return len(des), takeErr, reErr, newErr
}

// TestMailboxWriteGuard_AckRacingAnInFlightRedeliveryCannotBreachTheCap is the breach case.
//
// THE ASSERTION IS THE INVARIANT, NOT THE SCHEDULE, and that is deliberate. Post-fix the
// re-delivery holds the flock across writeFileAtomic, so the new delivery BLOCKS instead of
// committing — an earlier draft waited for it inline before releasing the pause and deadlocked,
// which is the fix working. A test that encodes a schedule breaks on every correct change to
// that schedule. So both outcomes (the new delivery committed, or it blocked and was then
// refused) are accepted; only the over-cap mailbox is not.
func TestMailboxWriteGuard_AckRacingAnInFlightRedeliveryCannotBreachTheCap(t *testing.T) {
	const capN = mailboxRaceCap
	entries, takeErr, reErr, newErr := mailboxRedeliveryRace(t, "wf-ack-races-redelivery", true)

	require.Falsef(t, entries > capN || takeErr != nil,
		"mailbox cap BREACHED by an ack racing an in-flight re-delivery: cap=%d entries=%d "+
			"TakeSignals_err=%v reErr=%v newErr=%v. The re-delivery evaluated \"this id already "+
			"exists, so I cannot grow the mailbox\" OUTSIDE the lock; the ack then removed that "+
			"entry and a new delivery took the freed slot before the re-delivery's rename landed. "+
			"TakeSignals now rejects the whole mailbox and a WaitForSignal run's take fails on "+
			"every re-drive, with no API able to recover it.",
		capN, entries, takeErr, reErr, newErr)

	// LOAD-BEARING, not a sanity check: the breach is SILENT. The fast path reported SUCCESS
	// while resurrecting an entry into a slot that had already been reallocated — that silence
	// is the severity. A breach that returned an error would be a different, far milder bug.
	// This also fails any "fix" that closes the hole by REFUSING re-deliveries, which would
	// break the idempotency contract pinned by RedeliveryAtCapStillSucceeds.
	require.NoError(t, reErr, "the re-delivery must report success — it does not grow the mailbox")
}

// TestMailboxWriteGuard_AckRacingRedeliveryControl_NoAck is the CONTROL, and it is what makes
// the arm above evidence rather than an assertion that agrees with itself. Identical fixture,
// identical interleaving, the ack REMOVED and nothing else changed. The new delivery is then
// correctly refused and the mailbox stays at the bound — isolating the cause to THE ACK RACING
// THE FAST PATH, and not to an off-by-one in checkMailboxEntries nor to the createTempFile seam
// itself perturbing the count.
//
// IF THIS CONTROL EVER REDS, THE BREACH ARM STOPS BEING EVIDENCE: it would mean the fixture
// breaks the guard on its own. It is GREEN both at 6855050 (pre-fix, where the breach arm REDs)
// and at the fix — invariant across the change, which is exactly what a control must be.
func TestMailboxWriteGuard_AckRacingRedeliveryControl_NoAck(t *testing.T) {
	const capN = mailboxRaceCap
	entries, takeErr, reErr, newErr := mailboxRedeliveryRace(t, "wf-ack-races-control", false)

	require.Falsef(t, entries > capN || takeErr != nil,
		"CONTROL BREACHED without an ack: cap=%d entries=%d TakeSignals_err=%v reErr=%v newErr=%v. "+
			"The fixture itself is breaking the guard, so the breach arm proves nothing.",
		capN, entries, takeErr, reErr, newErr)
	require.ErrorIs(t, newErr, ErrValidation,
		"without the ack the mailbox is at the bound, so the NEW delivery must be refused")
	require.NoError(t, reErr, "the re-delivery must report success even at the cap")
}

// TestMailbox_EmptyMailboxDirResidueIsBenign discharges a claim deliverSignalToDir makes in
// prose and would otherwise leave unchecked: its IO-failure paths (lock acquisition, ReadDir,
// writeFileAtomic) all return AFTER MkdirAll and can leave an empty <id>.signals/ behind, and
// the comment says that residue is deliberately not cleaned up because it is harmless.
//
// "Harmless" is a property of the READ and of the NEXT delivery, so both are exercised
// directly against a hand-made empty mailbox rather than argued from the code. Written because
// the comment it backs replaced one that read as if the residue class were closed — a comment
// asserting a property is a verification obligation, and this is the discharge.
func TestMailbox_EmptyMailboxDirResidueIsBenign(t *testing.T) {
	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)
	const wf = "wf-residue"

	// Exactly what an IO-failed delivery leaves: the dir, no entries.
	require.NoError(t, os.MkdirAll(filepath.Join(base, wf+signalDirSuffix), 0750))

	got, terr := js.TakeSignals(wf)
	require.NoError(t, terr, "an empty mailbox dir must read as an empty mailbox, not an error")
	require.Empty(t, got)

	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s1", Name: "n"}),
		"a later delivery must reuse the residual dir, not trip over it")
	got, terr = js.TakeSignals(wf)
	require.NoError(t, terr)
	require.Len(t, got, 1)
}
