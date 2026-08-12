package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMailboxWriteGuard_DeleteRacingADeliveryCannotBreachTheCap is the third writer's
// regression arm, and the reason the mailbox lock lives on a SIBLING file rather than on the
// mailbox directory.
//
// flock(2) binds to an INODE, not a path. removeSignalDir (Delete) called os.RemoveAll on the
// mailbox directory under NO lock, destroying the very object a parked deliverer's flock was
// bound to. The next deliverer MkdirAll'd a fresh inode and locked THAT, so two writers held
// "the" lock on two different objects while writing through one path — lock sets disjoint by
// construction, exclusion vacuous.
//
// MEASURED at bc6cb3b: sameInode=false, 5 entries against a cap of 4, TakeSignals returned
// "corrupt workflow data: signal mailbox entry count exceeds max", and the parked delivery
// reported SUCCESS. Silent, like the two blockers before it.
//
// This was the THIRD writer. The entry-count invariant named two — the delivery and the ack —
// and an ack is safe because removal cannot push a mailbox over its cap. The one that was
// missing is the one that broke it, which is exactly round 2's shape, so the writers are now
// enumerated mechanically (`grep -n signalDirSuffix` over non-test sources) in
// deliverSignalToDir rather than recalled.
//
// ASSERTS THE INVARIANT, NOT THE SCHEDULE. Post-fix Delete blocks on the lock instead of
// destroying it, so an inline Delete deadlocks — the first draft of this very test did, which
// is the fix working. Delete and the refill therefore run in a goroutine with a bounded window;
// whether they complete or block, only the over-cap mailbox is a failure.
func TestMailboxWriteGuard_DeleteRacingADeliveryCannotBreachTheCap(t *testing.T) {
	const capN = 4
	orig := signalMailboxCap
	signalMailboxCap = capN
	t.Cleanup(func() { signalMailboxCap = orig })

	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)
	const wf = "wf-delete-inode"
	require.NoError(t, js.Save(NewWorkflowData(wf)))
	for i := 0; i < capN; i++ {
		require.NoError(t, js.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n"}))
	}
	mbox := filepath.Join(base, wf+signalDirSuffix)
	before, serr := os.Stat(mbox)
	require.NoError(t, serr)

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
		return prev(d, pattern)
	}
	armed.Store(true)

	var wg sync.WaitGroup
	var reErr, delErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		reErr = js.DeliverSignal(wf, Signal{ID: "s000", Name: "n", Payload: "updated"})
	}()
	// Bounded, not a bare receive: an unreached seam is a FIXTURE failure, not a breach, and a
	// -timeout panic reports both as FAIL. See awaitSeam and fixture property 6.
	awaitSeam(t, reached, "the re-delivery never reached writeFileAtomic")

	// Delete, then refill to the cap through whatever inode now backs the path.
	racerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(racerDone)
		if delErr = js.Delete(wf); delErr != nil {
			return
		}
		for i := 0; i < capN; i++ {
			if e := js.DeliverSignal(wf, Signal{ID: fmt.Sprintf("n%03d", i), Name: "n"}); e != nil {
				return
			}
		}
	}()
	select {
	case <-racerDone:
	case <-time.After(2 * time.Second):
	}
	close(release)
	wg.Wait()

	des, rerr := os.ReadDir(mbox)
	if rerr != nil && !os.IsNotExist(rerr) {
		require.NoError(t, rerr)
	}
	_, takeErr := js.TakeSignals(wf)
	after, aerr := os.Stat(mbox)
	sameInode := aerr == nil && os.SameFile(before, after)
	t.Logf("sameInode=%v entries=%d takeErr=%v reErr=%v delErr=%v", sameInode, len(des), takeErr, reErr, delErr)

	require.Falsef(t, len(des) > capN || takeErr != nil,
		"mailbox cap BREACHED by a Delete racing a delivery: entries=%d takeErr=%v reErr=%v "+
			"delErr=%v sameInode=%v. os.RemoveAll destroyed the INODE the parked deliverer's flock "+
			"was bound to; the refill then locked a brand-new inode, so two writers held 'the' lock "+
			"on two different objects and both wrote through one path. The lock must live on an "+
			"object no writer can unlink — see signalLockSuffix.",
		len(des), takeErr, reErr, delErr, sameInode)
}

// TestMailboxLockFile_IsNotReclaimedByDelete pins the one thing Delete deliberately does NOT
// reclaim, because reclaiming it would re-arm the inode-destruction class above: unlinking the
// lock file while another process holds it hands the next deliverer a fresh inode.
//
// Written as a test rather than a comment because it is a claim about behaviour, and this phase
// has now produced seven plausible-but-wrong claims that lived only in comments.
//
// IT IS ALSO HALF OF A PAIR, and the halves bite different things — MEASURED, not assumed:
//
//	the race test above  bites the fix's ABSENCE       (Delete takes no lock at all)
//	                     -> RED, 5 entries at cap 4
//	this test            bites the fix's WRONG PRESENCE (the lock moved back onto the mailbox
//	                     dir, with Delete taking that dir lock — the plausible tidy, "why do we
//	                     need a separate lock file?")
//	                     -> RED, "a delivery must materialize the mailbox lock file"
//
// The race test PASSES that wrong fix. Verified by performing it: locking the directory and
// having Delete take the directory lock leaves the race arm green, because that schedule needs
// the victim BLOCKED on the lock while the inode is destroyed, which this seam cannot force.
// So the mechanism is asserted directly rather than only through a schedule. Same shape as B09
// — most guards defend against a guard's absence; this pair also defends against a wrong one.
func TestMailboxLockFile_IsNotReclaimedByDelete(t *testing.T) {
	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)
	const wf = "wf-lockfile"
	require.NoError(t, js.Save(NewWorkflowData(wf)))
	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s1", Name: "n"}))

	lock := filepath.Join(base, wf+signalLockSuffix)
	_, serr := os.Stat(lock)
	require.NoError(t, serr, "a delivery must materialize the mailbox lock file")

	require.NoError(t, js.Delete(wf))

	// The data IS reclaimed — that is Delete's contract.
	got, terr := js.TakeSignals(wf)
	require.NoError(t, terr)
	require.Empty(t, got, "Delete reclaims the durable mailbox")
	_, derr := os.Stat(filepath.Join(base, wf+signalDirSuffix))
	require.True(t, os.IsNotExist(derr), "Delete removes the mailbox directory itself")

	// The lock file is NOT, and must not be.
	_, lerr := os.Stat(lock)
	require.NoError(t, lerr,
		"the lock file must SURVIVE Delete: unlinking it while another holder has it open "+
			"restores the inode-destruction race the sibling lock file exists to close")

	// It is invisible to the store's own listing, so it cannot appear as a phantom workflow.
	ids, lerr2 := js.ListWorkflows()
	require.NoError(t, lerr2)
	require.NotContains(t, ids, wf)
	require.NotContains(t, ids, wf+signalLockSuffix)
}

// TestMailboxLock_NoStaleLockClassAcrossProcessDeath confirms by EXECUTION the property that
// motivated choosing flock(2) over an O_CREATE|O_EXCL lockfile in the first place: the kernel
// releases a flock when the holder dies, so a deliverer killed mid-write cannot wedge every
// future delivery to that mailbox.
//
// It needs confirming now rather than inheriting, because the lock file changed from a
// transient object to a PERMANENT one. The reasonable worry is that a lock file which is never
// deleted reintroduces the stale-lock class the original choice avoided. It does not — the
// stale-lock hazard belongs to lockfiles whose EXISTENCE is the lock, and here existence means
// nothing; only the kernel-held flock does. But that is an argument, and this phase has been
// wrong about arguments, so the child below acquires the lock and exits WITHOUT releasing it.
//
// A real OS process, not a goroutine: closing an fd in-process also releases the flock, so an
// in-process version would pass without testing death at all. os.Exit(1) in the child skips
// every deferred release, so the release observed here is the KERNEL's, not the code's.
//
// WHAT THIS DOES NOT ESTABLISH, stated because the test's name is broader than its reach: it
// says nothing about whether the lock EXCLUDES. A no-op lockMailboxDir passes it trivially.
// Exclusion is tested at the level it is claimed at by TestMailboxCap_2Proc (16 real processes
// on one mailbox) and by the in-process concurrent arms. This test covers exactly one axis —
// that a dead holder leaves nothing behind — because making the lock file PERMANENT is what
// put that axis in doubt.
//
// The parent cannot contend with a LIVE child here: cmd.CombinedOutput waits for exit, so the
// acquire below always happens after death. That is correct for the property under test, and
// it is also why an "is the child still holding it?" bite does not fire — verified by
// performing it. The bite that DOES fire is replacing flock with an existence-based lockfile
// (O_EXCL, release-by-unlink), the shape flock was chosen over:
//
//	cannot lock signal mailbox: .../wf-stale-lock.signals.lock: file exists
//
// i.e. a genuine stale lock, with the child having acquired successfully first.
func TestMailboxLock_NoStaleLockClassAcrossProcessDeath(t *testing.T) {
	if os.Getenv(mboxCapWorkerEnv) != "" {
		t.Skip("worker invocation; the parent drives this scenario")
	}
	if testing.Short() {
		t.Skip("spawns a subprocess; skipped under -short")
	}

	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)
	const wf = "wf-stale-lock"
	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s1", Name: "n"}))
	lockPath := filepath.Join(base, wf+signalLockSuffix)

	// The child takes the lock and dies holding it.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestMailboxLockHolderEntry$") //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), mboxCapWorkerEnv+"=1", mboxCapDirEnv+"="+lockPath)
	out, cerr := cmd.CombinedOutput()
	require.Error(t, cerr, "the child must die holding the lock (non-zero exit); output: %s", out)
	require.Contains(t, string(out), "HELD", "the child must have actually acquired the lock; output: %s", out)

	// The dead holder's lock must be gone. Bounded, so a genuine stale lock is a FAILURE with a
	// diagnosis rather than a 30-minute hang — the same discipline as awaitSeam.
	done := make(chan error, 1)
	go func() {
		unlock, lerr := lockMailboxDir(lockPath, true)
		if lerr == nil {
			unlock()
		}
		done <- lerr
	}()
	select {
	case lerr := <-done:
		require.NoError(t, lerr)
	case <-time.After(30 * time.Second):
		t.Fatal("STALE LOCK: the mailbox lock was still held 30s after its holder died. flock is " +
			"supposed to be released by the kernel on process death — if this fires, the persistent " +
			"lock file HAS reintroduced the stale-lock class that flock was chosen to avoid, and " +
			"every future delivery to this mailbox is wedged until someone removes the file by hand.")
	}

	// And the store is actually usable again, not merely lockable.
	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s2", Name: "n"}))
	got, terr := js.TakeSignals(wf)
	require.NoError(t, terr)
	require.Len(t, got, 2)
}

// TestMailboxLockHolderEntry is the subprocess: acquire the mailbox lock, announce it, then die
// without releasing. os.Exit(1) skips every deferred release, which is the point.
func TestMailboxLockHolderEntry(t *testing.T) {
	if os.Getenv(mboxCapWorkerEnv) == "" {
		t.Skip("not a worker invocation")
	}
	if _, lerr := lockMailboxDir(os.Getenv(mboxCapDirEnv), true); lerr != nil {
		t.Fatalf("worker could not lock: %v", lerr)
	}
	fmt.Println("HELD")
	os.Stdout.Sync() //nolint:errcheck // best-effort flush before the deliberate death
	os.Exit(1)       // die holding it; no unlock, no deferred cleanup
}

// TestMailboxDeleteLock_DeleteWaitsForAnInFlightDelivery is the THIRD-ROW bite, and it exists
// because an independent reviewer proved nothing covered it.
//
// The coverage matrix for the mailbox lock, all three rows MEASURED:
//
//	mutation                                     race test  lockfile test  this test
//	true absence (the bc6cb3b shape)             RED        RED            RED
//	wrong presence (lock the DIR in Delete)      green      RED            green
//	sibling kept, Delete-side acquisition GONE   green      green          RED
//
// The reviewer deleted removeSignalDir's five lock lines, left the sibling lock file in place,
// and the ENTIRE signal suite passed under -race including both other regression tests. An
// untested guard is a guard the next tidy removes — exactly as it was removed to demonstrate
// the point.
//
// WHAT THE DELETE-SIDE LOCK BUYS is not the inode fix (the sibling file already provides that;
// crediting it here was a comment that did not match the mechanism, which is the same class of
// error as the blockers this phase spent itself on). It buys this: without it, a Delete lands
// os.RemoveAll between an in-flight delivery's MkdirAll and its rename, and that delivery fails
// with a SPURIOUS ErrIO — it did nothing wrong and the mailbox it was writing to was legally
// reclaimed underneath it. With it, Delete waits for the delivery to finish.
//
// So the assertion is on the DELIVERY's error, not on the mailbox contents: a delivery that
// races a Delete must either succeed or be reclaimed, never fail with an IO error.
func TestMailboxDeleteLock_DeleteWaitsForAnInFlightDelivery(t *testing.T) {
	base := t.TempDir()
	js, err := NewJSONFileStore(base)
	require.NoError(t, err)
	const wf = "wf-delete-waits"
	require.NoError(t, js.Save(NewWorkflowData(wf)))
	// One delivery first, so the mailbox and its lock file both exist and Delete has something
	// to serialize against. Without this the delete-side acquire correctly skips (create=false).
	require.NoError(t, js.DeliverSignal(wf, Signal{ID: "seed", Name: "n"}))

	reached := make(chan struct{})
	release := make(chan struct{})
	var armed atomic.Bool
	prev := createTempFile
	t.Cleanup(func() { createTempFile = prev })
	createTempFile = func(d, pattern string) (atomicTempFile, error) {
		if strings.Contains(pattern, "inflight") && armed.CompareAndSwap(true, false) {
			close(reached)
			<-release
		}
		return prev(d, pattern)
	}
	armed.Store(true)

	var wg sync.WaitGroup
	var deliverErr, delErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		deliverErr = js.DeliverSignal(wf, Signal{ID: "inflight", Name: "n"})
	}()
	awaitSeam(t, reached, "the in-flight delivery never reached writeFileAtomic")

	// Delete races it. WITH the delete-side lock this blocks; WITHOUT it, os.RemoveAll lands
	// between the delivery's MkdirAll and its rename.
	delDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(delDone)
		delErr = js.Delete(wf)
	}()
	select {
	case <-delDone:
	case <-time.After(2 * time.Second):
	}
	close(release)
	wg.Wait()

	t.Logf("deliverErr=%v delErr=%v", deliverErr, delErr)
	require.NoErrorf(t, deliverErr,
		"a delivery racing a Delete failed with a SPURIOUS IO error (delErr=%v). removeSignalDir "+
			"ran os.RemoveAll between this delivery's MkdirAll and its rename, so writeFileAtomic "+
			"could not create its temp file in a directory that no longer existed. The delivery did "+
			"nothing wrong. removeSignalDir must take the mailbox lock so a Delete waits for an "+
			"in-flight delivery instead of pulling the directory out from under it.", delErr)
}

// TestMailboxLockFile_DeleteOfAnUntouchedWorkflowLeavesNothing pins the leak that taking the
// lock with O_CREATE on the Delete path introduced: Delete minted a permanent lock file for
// workflows that never existed.
//
// MEASURED before the fix: five Deletes of nonexistent ids, each correctly returning
// ErrNotFound, left five .signals.lock files; and Delete of a signal-less workflow turned
// [wf-nosignals.json] into [wf-nosignals.signals.lock] — the reclamation API replacing a
// workflow with a permanent artifact. Unbounded in the number of distinct ids ever passed to
// Delete, so a cleanup sweep over stale ids accumulates one file per id forever.
func TestMailboxLockFile_DeleteOfAnUntouchedWorkflowLeavesNothing(t *testing.T) {
	t.Run("nonexistent workflow", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		for i := 0; i < 5; i++ {
			_ = js.Delete(fmt.Sprintf("never-existed-%d", i)) //nolint:errcheck // ErrNotFound is the expected outcome; the assertion is on the directory
		}
		names := dirNames(t, base)
		require.Emptyf(t, names, "Delete of a nonexistent workflow must leave NOTHING behind; left %v", names)
	})

	t.Run("workflow with a snapshot but no signals", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		const wf = "wf-nosignals"
		require.NoError(t, js.Save(NewWorkflowData(wf)))
		require.NoError(t, js.Delete(wf))
		names := dirNames(t, base)
		require.Emptyf(t, names,
			"Delete replaced the workflow with a permanent artifact: %v. Reclamation must not mint state.", names)
	})

	// The arms above run against an EMPTY store, where "left nothing" and "unchanged" happen to
	// coincide. The scenario that motivated the finding does not: a cleanup sweep over stale ids
	// runs against a POPULATED store, and the property there is that a failed Delete leaves the
	// directory BYTE-IDENTICAL — it must not mint state, and it must not disturb the workflows
	// that are legitimately present either. Asserted as a before/after comparison rather than an
	// emptiness check, because emptiness cannot express it.
	t.Run("failed deletes leave a populated store byte-identical", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		for _, wf := range []string{"live-a", "live-b"} {
			require.NoError(t, js.Save(NewWorkflowData(wf)))
			require.NoError(t, js.DeliverSignal(wf, Signal{ID: "s1", Name: "n"}))
		}
		before := dirNames(t, base)

		for i := 0; i < 5; i++ {
			derr := js.Delete(fmt.Sprintf("stale-sweep-%d", i))
			require.ErrorIs(t, derr, ErrNotFound, "sanity: these ids do not exist, so Delete must refuse")
		}

		require.Equalf(t, before, dirNames(t, base),
			"five FAILED Deletes changed the store directory. A Delete that returns ErrNotFound "+
				"must be a no-op on disk; minting one lock file per id makes a stale-id sweep "+
				"accumulate state forever on the one API whose job is reclamation.")
	})
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(des))
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

// TestMailboxLockFile_Population makes the ONE authoritative statement about the lock file's
// population executable, because that statement has already been wrong once and now lives in
// exactly one place with three comments pointing at it.
//
// signalLockSuffix says: one file per workflow for which a delivery was ever ATTEMPTED PAST
// VALIDATION, including a delivery later refused for exceeding the cap — and nothing else in
// the package creates one. Each row below is one clause of that sentence, and the last row is
// the clause that falsifies the phrasing this replaced ("per workflow that ever received a
// signal"), because a refused delivery receives nothing and still leaves the file.
//
// The two Delete rows overlap TestMailboxLockFile_DeleteOfAnUntouchedWorkflowLeavesNothing on
// purpose: that test is about Delete not minting state, this one is about the population being
// exactly what the comment says. Same facts, different obligations — deleting either because it
// duplicates the other loses a claim.
func TestMailboxLockFile_Population(t *testing.T) {
	lockOf := func(wf string) string { return wf + signalLockSuffix }

	t.Run("failed Delete of a nonexistent id creates nothing", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		require.ErrorIs(t, js.Delete("never-existed"), ErrNotFound)
		require.Empty(t, dirNames(t, base))
	})

	t.Run("Delete of a signal-less workflow creates nothing", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		require.NoError(t, js.Save(NewWorkflowData("wf")))
		require.NoError(t, js.Delete("wf"))
		require.Empty(t, dirNames(t, base))
	})

	t.Run("delivery rejected at VALIDATION creates nothing", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		// Empty sig ID fails validateSignalID, which runs before the acquire.
		require.ErrorIs(t, js.DeliverSignal("wf", Signal{ID: "", Name: "n"}), ErrValidation)
		require.Empty(t, dirNames(t, base),
			"validation runs before the acquire, so a delivery refused there must create nothing")
	})

	t.Run("delivery rejected on SIZE creates nothing", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base, WithJSONMaxFileSize(64))
		require.NoError(t, err)
		big := strings.Repeat("x", 4096)
		require.Error(t, js.DeliverSignal("wf", Signal{ID: "s1", Name: "n", Payload: big}))
		require.Empty(t, dirNames(t, base),
			"the size ceiling is checked before the acquire, so it must create nothing either")
	})

	t.Run("delivery refused for exceeding the CAP leaves the lock file", func(t *testing.T) {
		const capN = 2
		orig := signalMailboxCap
		signalMailboxCap = capN
		t.Cleanup(func() { signalMailboxCap = orig })

		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		const wf = "wf-capped"
		for i := 0; i < capN; i++ {
			require.NoError(t, js.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%d", i), Name: "n"}))
		}
		require.ErrorIs(t, js.DeliverSignal(wf, Signal{ID: "over", Name: "n"}), ErrValidation)

		// THE CLAUSE THAT FALSIFIED THE OLD PHRASING. This workflow's mailbox is at the cap and
		// the refused delivery received nothing, yet the lock file is present — because the cap
		// check happens UNDER the lock, and it has to, or the count is stale by the time the
		// entry lands. "Per workflow that ever received a signal" cannot describe this row.
		require.Contains(t, dirNames(t, base), lockOf(wf),
			"a delivery refused for the cap is still a delivery attempted past validation, so the "+
				"lock file must be present — the acquire precedes the count by necessity")
	})

	t.Run("nothing else in the package creates one", func(t *testing.T) {
		base := t.TempDir()
		js, err := NewJSONFileStore(base)
		require.NoError(t, err)
		const wf = "wf-reads"
		require.NoError(t, js.Save(NewWorkflowData(wf)))
		_, terr := js.TakeSignals(wf)
		require.NoError(t, terr)
		require.NoError(t, js.AckSignals(wf, []string{"nope"}))
		_, lerr := js.ListWorkflows()
		require.NoError(t, lerr)
		_, loadErr := js.Load(wf)
		require.NoError(t, loadErr)

		require.NotContains(t, dirNames(t, base), lockOf(wf),
			"only DeliverSignal may create a lock file; a read, an ack, a list or a load must not")
	})
}
