package workflow

// M16 ph77 hard-bar: the REAL 2-OS-process re-claim-after-death, END-TO-END through Execute (ph75
// proved the store-level Claim race; this drives the actual executor). Two `go test` subprocesses
// share one DB file. Worker A Executes, commits level 0 (n0), then PAUSES past its (real, short)
// WithLeaseTTL so its lease LAPSES while it still believes it holds token N. Worker B Executes,
// re-claims (token N+1), RESUMES from A's committed frontier (n0 skipped), runs n1, completes. A
// wakes → its n1 checkpoint is FENCED → Execute returns ErrFencedOut, the drive aborts, no stale
// write lands. The final journal byte-checks to B's completion; exactly-once.
//
// The clock is REAL here (a FakeClock is in-process-only and cannot cross the OS-process boundary):
// A uses a short WithLeaseTTL and actually sleeps > TTL at its barrier. Bounded + reliable (B's
// whole drive fits inside A's sleep), not timing-flaky.
//
// Seed-break (DEC-M16-D6, TestMPExecute2Proc_SeedBreak): the config-granular fencing bite (ph74's
// blessed pattern — NO production kill-switch on the safety predicate). A's stale token-1 write is
// attempted on B's committed journal two ways: through the MP store (CAS active → REJECTED with
// ErrFencedOut, B survives) vs a NON-MP handle (CAS absent → the SAME write LANDS → clobbers B). The
// differential proves the fencing CAS is the sole load-bearing thing preventing the stale overwrite.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mpExecEnv holds the worker subprocess config (env-var passed).
const (
	envExecRole    = "M16_EXEC_ROLE"    // "A" | "B"
	envExecDB      = "M16_EXEC_DB"      // shared db path
	envExecSignal  = "M16_EXEC_SIGNAL"  // A touches this file after n0 commits (parent waits on it)
	envExecTTLms   = "M16_EXEC_TTL_MS"  // lease TTL in ms (A pauses > this)
	envExecPausems = "M16_EXEC_PAUSEMS" // how long A pauses at its barrier (> TTL)
)

// TestMPExecuteWorkerEntry is the subprocess worker (skipped in a normal run). Role A or B drives a
// real n0→n1 Execute on the shared mp store. It prints a classification line the parent parses:
//
//	A: "A_RESULT=fenced" (ErrFencedOut, the required outcome) | "A_RESULT=clean" | "A_RESULT=other:<err>"
//	B: "B_RESULT=ok" | "B_RESULT=err:<err>"
func TestMPExecuteWorkerEntry(t *testing.T) {
	role := os.Getenv(envExecRole)
	if role == "" {
		t.Skip("not a worker invocation (set M16_EXEC_ROLE)")
	}
	dbPath := os.Getenv(envExecDB)
	ttlMS, _ := strconv.Atoi(os.Getenv(envExecTTLms))     //nolint:errcheck // test env
	pauseMS, _ := strconv.Atoi(os.Getenv(envExecPausems)) //nolint:errcheck // test env

	opts := []SQLiteOption{WithMultiProcess(), WithLeaseTTL(time.Duration(ttlMS) * time.Millisecond)}
	store, err := NewSQLiteStore(dbPath, opts...)
	if err != nil {
		t.Fatalf("worker %s open: %v", role, err)
	}
	defer func() { _ = store.Close() }() //nolint:errcheck // worker cleanup

	d := NewDAG("mp")
	require.NoError(t, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
		return nil
	}))))
	// n1's action runs ONLY after n0's LEVEL BARRIER committed n0 durably (the M9 per-level flush
	// fires between levels). So when role A enters n1's action, n0 is ALREADY on disk — this is the
	// correct point to signal the parent (L0 committed) and pause past the lease TTL. A's NEXT
	// checkpoint (n1's own barrier, after this action returns) is the one that gets FENCED once B
	// has re-claimed. Role B's n0 is resumed-skipped (loaded Completed from A's commit); B's n1 runs.
	require.NoError(t, d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error {
		if role == "A" {
			if sig := os.Getenv(envExecSignal); sig != "" {
				_ = os.WriteFile(sig, []byte("n0-committed"), 0o600) //nolint:errcheck,gosec // best-effort signal
			}
			time.Sleep(time.Duration(pauseMS) * time.Millisecond) // pause > TTL → A's lease lapses
		}
		return nil
	}))))
	require.NoError(t, d.AddDependency("n0", "n1"))

	w := (&Workflow{DAG: d, WorkflowID: "wf", Store: store}).WithMultiProcessLocker(role)
	execErr := w.Execute(context.Background())

	switch role {
	case "A":
		switch {
		case errors.Is(execErr, ErrFencedOut):
			t.Log("A_RESULT=fenced")
		case execErr == nil:
			t.Log("A_RESULT=clean")
		default:
			t.Logf("A_RESULT=other:%v", execErr)
		}
	case "B":
		if execErr == nil {
			t.Log("B_RESULT=ok")
		} else {
			t.Logf("B_RESULT=err:%v", execErr)
		}
	}
}

// waitForFile polls for a signal file (A's "n0 committed" marker) up to a deadline.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestMPExecute2Proc_ReClaimThroughExecute — the hard bar. See file header.
func TestMPExecute2Proc_ReClaimThroughExecute(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns OS subprocesses + real sleeps; skipped under -short")
	}
	runReClaim(t) // real fencing → A fenced, journal is B's, exactly-once
}

// TestMPExecute2Proc_SeedBreak — DEC-M16-D6, the config-granular fencing bite (ph74's blessed
// pattern — no production kill-switch on the safety predicate). After B owns the committed journal
// (token 2), A's STALE token-1 write is attempted two ways on the SAME durable state:
//
//	(a) through the MP store (fencing CAS active) → REJECTED with ErrFencedOut → B's journal SURVIVES.
//	(b) through a NON-MP store handle (the CAS is absent — held==0 short-circuit) → the write LANDS →
//	    it CLOBBERS B's journal (A's stale n1 output overwrites B's).
//
// The differential proves the fencing CAS is the sole load-bearing thing preventing the stale
// overwrite: remove it (non-mp) and the same write that was rejected now corrupts the journal.
// Deterministic (no subprocess/clock) — the token seam models A's stale in-process token, exactly as
// the ph76 AbortOnSupersession bite does.
func TestMPExecute2Proc_SeedBreak(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seed.db")
	// One shared FakeClock across every handle so lease-lapse is deterministic + consistent (a
	// per-handle SystemClock vs FakeClock mismatch would make expiry comparisons meaningless).
	clk := NewFakeClock(time.Unix(1000, 0))
	openMP := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		return s
	}

	// B's committed journal: n0+n1 Completed under token 1 (the re-claimer's state).
	sB := openMP()
	_, err := sB.Claim("wf", "B") // token 1
	require.NoError(t, err)
	dB := NewWorkflowData("wf")
	dB.SetNodeStatus("n0", Completed)
	dB.SetNodeStatus("n1", Completed)
	dB.SetOutput("n1", "B-wrote-this")
	require.NoError(t, sB.Save(dB), "B commits the completed journal")

	// A's would-be STALE write: A believes it holds token 1, but a re-claim bumped current to 2.
	aStale := NewWorkflowData("wf")
	aStale.SetNodeStatus("n0", Completed)
	aStale.SetNodeStatus("n1", Completed)
	aStale.SetOutput("n1", "A-STALE-clobber")

	// Bump the durable token past A: advance the shared clock past the TTL → B's lease lapses → a
	// re-claim bumps the durable fencing_token to 2. A (held token 1) is now stale.
	clk.Advance(2 * time.Second)          // > 1s TTL → B's lease lapses (shared clock, consistent)
	_, err = openMP().Claim("wf", "Bump") // → durable token 2; A (token 1) is now stale
	require.NoError(t, err)

	// (a) MP store, fencing ACTIVE: A's stale token-1 write is REJECTED.
	sA := openMP()
	sA.setToken("wf", 1) // A's stale in-process token (held=1 < current=2)
	errFenced := sA.Save(aStale)
	require.ErrorIs(t, errFenced, ErrFencedOut, "(a) MP fencing ON → A's stale token-1 write is REJECTED")
	survived, err := sB.Load("wf")
	require.NoError(t, err)
	out, _ := survived.GetOutput("n1")
	require.Equal(t, "B-wrote-this", out, "(a) B's journal SURVIVES — the fence rejected A's clobber")

	// (b) NON-MP store handle, fencing ABSENT: the SAME stale write LANDS (no CAS) → clobbers B.
	sNoMP, err := NewSQLiteStore(dbPath) // no WithMultiProcess → checkFencingLocked no-ops
	require.NoError(t, err)
	defer func() { _ = sNoMP.Close() }() //nolint:errcheck // cleanup
	require.NoError(t, sNoMP.Save(aStale), "(b) fencing OFF → A's stale write LANDS (no CAS to reject it)")
	clobbered, err := sNoMP.Load("wf")
	require.NoError(t, err)
	outC, _ := clobbered.GetOutput("n1")
	require.Equal(t, "A-STALE-clobber", outC,
		"SEED-BREAK: with the fencing CAS removed (non-mp), A's stale write CLOBBERS B's journal — the fence is the sole load-bearing difference (ph74 config-granular bite)")
}

func runReClaim(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mpexec.db")
	sigPath := filepath.Join(dir, "n0.signal")

	// Pre-create the schema once (C3) so both workers open a ready DB.
	pre, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	require.NoError(t, pre.Close())

	const ttlMS, pauseMS = 800, 4000 // A pauses 4s > 0.8s TTL; B's drive fits well inside.

	spawn := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestMPExecuteWorkerEntry$", "-test.v")
		cmd.Env = append(os.Environ(),
			envExecRole+"="+role, envExecDB+"="+dbPath, envExecSignal+"="+sigPath,
			envExecTTLms+"="+strconv.Itoa(ttlMS), envExecPausems+"="+strconv.Itoa(pauseMS))
		return cmd
	}

	// Worker A: start it, let it commit n0 (level barrier) + pause inside n1 past its lease TTL.
	aCmd := spawn("A")
	var aBuf bytes.Buffer
	aCmd.Stdout = &aBuf
	aCmd.Stderr = &aBuf
	aDone := make(chan struct{})
	var aErr error
	require.NoError(t, aCmd.Start())
	go func() { aErr = aCmd.Wait(); close(aDone) }()

	// Wait for A to durably commit n0 (its signal file) before starting B.
	require.True(t, waitForFile(sigPath, 3*time.Second), "worker A never signalled n0 commit")
	// A's last checkpoint (n0's barrier) renewed its lease to now+TTL; A is now paused inside n1's
	// action and will NOT checkpoint again until it wakes. Wait past the TTL so A's lease genuinely
	// LAPSES before B claims — otherwise B sees a live foreign lease → ErrClaimLost. A's pause
	// (pauseMS) is sized well beyond TTL + this wait + B's whole drive, so A is still asleep when B
	// finishes and only then wakes into the fence.
	time.Sleep(time.Duration(ttlMS+400) * time.Millisecond) // > TTL → A's lease has lapsed
	// A is now paused past its TTL barrier → its lease has lapsed. Run B to completion.
	bOut, bErr := spawn("B").CombinedOutput()
	require.NoError(t, bErr, "worker B (re-claimer) failed:\n%s", bOut)
	require.Contains(t, string(bOut), "B_RESULT=ok", "B must re-claim + resume + complete:\n%s", bOut)

	// Let A wake, attempt n1, and finish.
	<-aDone
	aOut := aBuf.Bytes()

	// Read the final durable journal (whoever's it is).
	fin, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer func() { _ = fin.Close() }() //nolint:errcheck // cleanup
	data, err := fin.Load("wf")
	require.NoError(t, err)
	n0st, _ := data.GetNodeStatus("n0")
	n1st, _ := data.GetNodeStatus("n1")
	require.NoError(t, aErr, "worker A's subprocess exited cleanly (ErrFencedOut is a return value, not a test failure)")

	// A's n1 checkpoint was FENCED → Execute returned ErrFencedOut (A prints the classification).
	require.Contains(t, string(aOut), "A_RESULT=fenced",
		"MAJOR-4 / DEC-M16-D6: A's superseded checkpoint MUST be fenced → Execute returns ErrFencedOut:\n%s", aOut)
	// The surviving journal is B's completed run: n0 + n1 both Completed, exactly-once (A did not
	// clobber). n0 was committed by A and resumed by B (skipped, not re-run); n1 by B.
	require.Equal(t, Completed, n0st, "n0 durably Completed (A's commit, survived)")
	require.Equal(t, Completed, n1st, "n1 durably Completed (B's completion — A's fenced write did NOT overwrite)")
}
