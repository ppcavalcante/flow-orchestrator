package workflow

// M23 ph116 F1 — the GENUINE 2-OS-process mailbox entry-count test. The guard's whole claim is a
// CROSS-PROCESS one: the file-store mailbox is guarded by flock(2), which exists to serialize
// separate processes, and SQLiteStore is the multi-process store (M16 competing consumers). Every
// other arm in signal_store_mailbox_write_guard_test.go races goroutines or sql.DB handles inside
// ONE process, which is a strictly weaker setting.
//
// WHAT THIS ARM COVERS, stated from measurement rather than from the argument that motivated it —
// because that argument turned out to be wrong, and the correction is the useful part.
//
// The motivating worry was: flock is per OPEN FILE DESCRIPTION, and lockMailboxDir re-opens the
// mailbox directory on every call, which is exactly WHY it serializes. So the obvious future tidy
// ("avoid a syscall per delivery — hoist the os.Open to a store field") would make LOCK_EX a no-op
// between goroutines sharing that descriptor. The prediction attached to it was that every
// in-process arm would still pass while the guarantee evaporated, and that THIS arm would be what
// catches it.
//
// MEASURED, by performing exactly that hoist: the prediction is backwards.
//   - the in-process arms RED immediately (accepted 3 and 6 where 1 slot was free) — they DO catch it
//   - this 2-process arm PASSES under the hoist, six runs, and must: separate processes hold separate
//     descriptors, so a per-process fd cache cannot weaken cross-process locking at all
//
// So the two levels cover DIFFERENT failure modes and neither subsumes the other:
//   - the in-process arms detect a lock that stops excluding WITHIN a process (the hoist)
//   - this arm detects the absence of cross-process exclusion ENTIRELY — RED at 264265d, the real
//     pre-fix code, at 10/14/15 entries against a cap of 8 on every run
//
// Keep both. Deleting this one as "redundant with the goroutine arm" would drop the only test of the
// guarantee at the level it is actually claimed at; deleting those as "the 2-proc arm covers it"
// would drop the only detector of the hoist.
//
// Mirrors the re-exec pattern of fanout_kill_2proc_test.go: a child process invocation is gated on an
// env var, and the parent spawns N of them against one shared mailbox.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	mboxCapWorkerEnv = "MBOX_CAP_WORKER"  // set => this invocation is a delivering child
	mboxCapDirEnv    = "MBOX_CAP_BASEDIR" // the shared store baseDir
	mboxCapIDEnv     = "MBOX_CAP_SIGID"   // the sig.ID this child delivers
	mboxCapCapEnv    = "MBOX_CAP_CAP"     // the lowered signalMailboxCap
	mboxCapWID       = "wf-2proc"
)

// The children must reach DeliverSignal AT THE SAME TIME or this test is vacuous. Process
// startup jitter is tens of milliseconds; the guard's unlocked window is microseconds, so
// simply exec'ing 16 children never overlaps them and the arm passes against code that has
// no lock at all — verified: without this rendezvous it went GREEN at 264265d, the live
// defect. So the children rendezvous on the filesystem: each announces readiness, then spins
// until the parent releases them.
func mboxCapReadyPath(base, id string) string { return filepath.Join(base, "ready-"+id) }
func mboxCapGoPath(base string) string        { return filepath.Join(base, "GO") }

// The two barrier deadlines, declared together because their ORDER is the requirement and
// keeping them apart is how they got it backwards: the child waited 30s while the parent
// waited 60s for the last child to arrive, so on a contended host every child could time out
// inside the window the parent was still legitimately using. The child must outlast the
// parent, so its failure means "the parent never released" rather than "the parent was slow".
const (
	mboxCapParentBarrier = 60 * time.Second
	mboxCapChildBarrier  = 90 * time.Second
)

// TestMailboxCap2ProcEntry is the subprocess worker: it opens the SAME baseDir as a fresh
// JSONFileStore — its own process, its own file descriptors, its own flock — and attempts exactly
// one delivery of a distinct new sig.ID. It exits 0 whether the delivery is accepted or refused;
// the parent's oracle is the resulting mailbox, not this process's status.
func TestMailboxCap2ProcEntry(t *testing.T) {
	if os.Getenv(mboxCapWorkerEnv) == "" {
		t.Skip("not a worker invocation (set MBOX_CAP_WORKER to run as the delivering subprocess)")
	}
	capN, err := strconv.Atoi(os.Getenv(mboxCapCapEnv))
	if err != nil {
		t.Fatalf("worker cap: %v", err)
	}
	// The cap is a package var, so each child must lower it for itself — it is per-process state,
	// which is precisely why it cannot be the thing serializing anyone.
	signalMailboxCap = capN

	base := os.Getenv(mboxCapDirEnv)
	sigID := os.Getenv(mboxCapIDEnv)
	s, err := NewJSONFileStore(base)
	if err != nil {
		t.Fatalf("worker open: %v", err)
	}

	// Announce readiness AFTER all setup, so releasing the barrier is the only thing left
	// between here and the delivery.
	if werr := os.WriteFile(mboxCapReadyPath(base, sigID), []byte("1"), 0600); werr != nil {
		t.Fatalf("worker ready: %v", werr)
	}
	// The child's patience must EXCEED the parent's, or on a contended host the children start
	// giving up while the parent is still waiting for the slowest of them to arrive — every
	// child would fail for a reason that is not the property under test. mboxCapChildBarrier is
	// deliberately the larger of the pair; see its declaration.
	deadline := time.Now().Add(mboxCapChildBarrier)
	for {
		if _, serr := os.Stat(mboxCapGoPath(base)); serr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s: barrier never released after %s", sigID, mboxCapChildBarrier)
		}
		time.Sleep(200 * time.Microsecond)
	}

	// Refusal is a legitimate outcome and must not fail the child — the parent's oracle is the
	// resulting mailbox, not this delivery's verdict.
	s.DeliverSignal(mboxCapWID, Signal{ID: sigID, Name: "approve"}) //nolint:errcheck // refusal is expected and is not a child failure
}

// TestMailboxCap_2Proc is the parent. It seeds the mailbox to one below the cap, then launches N
// real OS processes that each try to deliver a distinct NEW id into it simultaneously. Exactly one
// slot is free, so at most one may be admitted.
//
// RED at 264265d (pre-fix, no lock): 14 entries at cap 8 and TakeSignals wedged with ErrCorruptData.
// GREEN at HEAD: exactly one of the racers admitted.
func TestMailboxCap_2Proc(t *testing.T) {
	if os.Getenv(mboxCapWorkerEnv) != "" {
		t.Skip("worker invocation; the parent drives this scenario")
	}
	if testing.Short() {
		t.Skip("spawns 16 subprocesses; skipped under -short")
	}

	const capN = 8
	const seeded = capN - 1
	const racers = 16

	orig := signalMailboxCap
	signalMailboxCap = capN
	t.Cleanup(func() { signalMailboxCap = orig })

	base := t.TempDir()
	seed, err := NewJSONFileStore(base)
	require.NoError(t, err)
	for i := 0; i < seeded; i++ {
		require.NoError(t, seed.DeliverSignal(mboxCapWID, Signal{ID: fmt.Sprintf("seed%03d", i), Name: "approve"}))
	}

	// Start every racer, wait until ALL are parked on the barrier, then release them together.
	// Starting them is not enough — see mboxCapReadyPath's comment.
	cmds := make([]*exec.Cmd, racers)
	// A child's t.Fatalf goes to the child's own stdout, so without this the parent's failure
	// output shows a breached mailbox and NOTHING about the child that caused it — the most
	// likely reason this test ever fails is a child that never reached DeliverSignal, and that
	// is exactly the diagnosis a silent child withholds.
	childOut := make([]*bytes.Buffer, racers)
	for i := 0; i < racers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestMailboxCap2ProcEntry$") //nolint:gosec // os.Args[0] is this test binary
		childOut[i] = &bytes.Buffer{}
		cmd.Stdout = childOut[i]
		cmd.Stderr = childOut[i]
		cmd.Env = append(os.Environ(),
			mboxCapWorkerEnv+"=1",
			mboxCapDirEnv+"="+base,
			mboxCapIDEnv+"="+fmt.Sprintf("new%03d", i),
			mboxCapCapEnv+"="+strconv.Itoa(capN),
		)
		require.NoError(t, cmd.Start())
		cmds[i] = cmd
	}

	ready := func() int {
		n := 0
		for i := 0; i < racers; i++ {
			if _, serr := os.Stat(mboxCapReadyPath(base, fmt.Sprintf("new%03d", i))); serr == nil {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(mboxCapParentBarrier)
	for ready() < racers {
		// An `if` rather than require.Falsef, and the reason is a DATA RACE the -race gate
		// caught on the first draft: require's arguments are evaluated EAGERLY, so folding
		// mboxCapChildLog into a per-iteration assertion read those buffers every millisecond
		// while exec's copier goroutines were still writing into them. The children must be
		// reaped BEFORE their output can be read at all — which is also why this stopped
		// formatting a 16-child log on every tick of a 60-second loop.
		if time.Now().After(deadline) {
			for _, c := range cmds {
				c.Process.Kill() //nolint:errcheck,gosec // best-effort teardown on a path that is already failing
			}
			for _, c := range cmds {
				c.Wait() //nolint:errcheck // reaps the copier goroutines; the Kill above guarantees an error here
			}
			t.Fatalf("only %d/%d racers reached the barrier in %s; child output:\n%s",
				ready(), racers, mboxCapParentBarrier, mboxCapChildLog(childOut))
		}
		time.Sleep(time.Millisecond)
	}
	require.NoError(t, os.WriteFile(mboxCapGoPath(base), []byte("1"), 0600))

	var wg sync.WaitGroup
	for _, cmd := range cmds {
		wg.Add(1)
		go func(c *exec.Cmd) {
			defer wg.Done()
			c.Wait() //nolint:errcheck // a refused delivery is a normal outcome; the mailbox is the oracle
		}(cmd)
	}
	wg.Wait()

	// The oracle is the mailbox itself, read the way the store reads it.
	entries, rerr := os.ReadDir(filepath.Join(base, mboxCapWID+signalDirSuffix))
	require.NoError(t, rerr)
	got, terr := seed.TakeSignals(mboxCapWID)

	require.Falsef(t, len(entries) > capN || terr != nil,
		"mailbox cap BREACHED ACROSS OS PROCESSES: cap=%d seeded=%d freeSlots=%d entries=%d "+
			"TakeSignals_err=%v returned=%d. %d separate processes each counted the mailbox and then "+
			"wrote into it; nothing serialized them, so each saw the same pre-count and committed. "+
			"This is the level the guard's claim is made at — flock(2) exists to serialize PROCESSES, "+
			"and an in-process arm cannot detect its absence.\nchild output:\n%s",
		capN, seeded, capN-seeded, len(entries), terr, len(got), racers, mboxCapChildLog(childOut))

	require.Lenf(t, got, capN, "exactly the one free slot should have been filled\nchild output:\n%s",
		mboxCapChildLog(childOut))
}

// mboxCapChildLog folds the racers' captured output into the parent's failure message. Only
// non-empty children are shown: a healthy run is 16 silent PASSes and printing them would bury
// the one child that has something to say.
//
// CALL THIS ONLY AFTER EVERY CHILD HAS BEEN Wait()ed. The buffers are filled by exec's copier
// goroutines, which Wait joins; reading one before that is a data race, and the -race gate
// caught exactly that on the first draft of this file.
func mboxCapChildLog(bufs []*bytes.Buffer) string {
	var sb strings.Builder
	for i, b := range bufs {
		if out := strings.TrimSpace(b.String()); out != "" {
			fmt.Fprintf(&sb, "  [child %d] %s\n", i, out)
		}
	}
	if sb.Len() == 0 {
		return "  (all children silent)"
	}
	return sb.String()
}
