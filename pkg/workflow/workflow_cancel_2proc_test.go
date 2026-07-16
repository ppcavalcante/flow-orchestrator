package workflow

// M18 ph87 hard bar (adversarial-tester rejoin, O-ENG-P87-2PROC) — the two REAL 2-OS-process cancel bites
// the store-level Slice-1 bites (TestDispositionGate_*, TestReclaimTerminalizesCancelled_*,
// TestCtxWatcher_*) structurally cannot reach: cross-process cancel delivery + the cancel-during-reclaim
// TOCTOU under a SIGKILL'd owner. Both mirror the M16 ph77 / M17 ph83 re-exec harness (a subprocess of
// `go test` itself → a genuine separate OS-process *sql.DB open → store-per-worker, the ph82 model). The
// operator/reclaimer share ONE .db file with WithMultiProcess() so cancel_requested is a durable column
// visible across the process boundary.
//
// BITE A — cross-process cancel-terminalizes end-to-end. Worker W drives runNext (so the real ctx-watcher
// is live) on a multi-level DAG whose L0 node blocks until cancelled; the operator process calls
// CancelRunning; W's watcher observes the durable flag, cancels Execute at the level barrier, the
// disposition gate terminalizes `cancelled`, and the L1 node NEVER runs (a durable marker file witnesses
// it). Seed-break = neuter the disposition gate's flag read → W leaves-claimed → row not `cancelled`.
//
// BITE B — cancel-during-reclaim TOCTOU with a SIGKILL'd owner. Worker A claims + enters Execute (holds
// the lease + token), then is SIGKILL'd mid-flight while cancel_requested is set. Reclaimer B (a real 2nd
// process) drives a runNext scan loop while A's lease lapses. B must NEVER resume the row from the frontier
// — ClaimNext's in-txn cancel-gate terminalizes it `cancelled`. Seed-break = move the reclaim cancel-check
// out of the IMMEDIATE txn's terminalize → the row either resumes (L1 marker appears) or strands `claimed`.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// ---- shared 2-proc cancel worker plumbing ----

const (
	envCanRole     = "M18_CAN_ROLE"      // "W" (bite A worker) | "A" (bite B owner) | "B" (bite B reclaimer)
	envCanDB       = "M18_CAN_DB"        // shared db path
	envCanEntrySig = "M18_CAN_ENTRY_SIG" // L0 node touches this on entry (parent waits on it)
	envCanL1Marker = "M18_CAN_L1_MARKER" // L1 node touches this IFF it ran (the never-run witness)
	envCanOwner    = "M18_CAN_OWNER"     // worker owner id
	envCanTTLms    = "M18_CAN_TTL_MS"    // lease TTL ms (bite B: short, so A's lease lapses fast)
	canWFID        = "cancel-wf"
	canType        = "cancel-chain"
)

// canWorkerPoll is the ctx-watcher poll the worker subprocess installs (fast, so cancel is observed well
// under the lease TTL — the whole point is cancel wins the reclaim race). Set in the subprocess entry.
const canWorkerPoll = 25 * time.Millisecond

// canRegistry builds the 2-level DAG every cancel worker drives. L0 (n0) signals entry then BLOCKS on the
// run ctx (a long-running node at level 0); L1 (n1) touches the L1 marker file — so if n1 ever runs, the
// marker EXISTS on disk, cross-process observable. n0 returns ctx.Err() when cancelled (a well-behaved node
// → dag.go returns a bare context.Canceled at the barrier → the disposition-gate path, NOT poison).
func canRegistry(entrySig, l1Marker string) *Registry {
	reg := NewRegistry()
	_ = reg.Register(canType, func() (*DAG, error) { //nolint:errcheck // fixed valid registration
		d := NewDAG(canType)
		if err := d.AddNode(NewNode("n0", ActionFunc(func(ctx context.Context, _ *WorkflowData) error {
			if entrySig != "" {
				_ = os.WriteFile(entrySig, []byte("entered"), 0o600) //nolint:errcheck,gosec // best-effort signal
			}
			<-ctx.Done() // block until the ctx-watcher cancels (or a parent shutdown)
			return ctx.Err()
		}))); err != nil {
			return nil, err
		}
		if err := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error {
			// L1 witness: if this ever runs, the marker lands on disk. Under cancel it must NEVER run.
			_ = os.WriteFile(l1Marker, []byte("n1-ran"), 0o600) //nolint:errcheck,gosec // the never-run witness
			return nil
		}))); err != nil {
			return nil, err
		}
		return d, d.AddDependency("n0", "n1")
	})
	return reg
}

// TestCancel2ProcWorkerEntry is the shared subprocess entry for both bites (skipped in a normal run).
//
//	role W (bite A): claim + drive runNext ONCE with the fast ctx-watcher; print W_RESULT=<state>.
//	role A (bite B): claim + enter Execute (blocks in n0) — the PARENT SIGKILLs it mid-flight, so it never
//	                 prints a result; it exists only to hold the claim + lease, then die.
//	role B (bite B): a runNext reclaim loop that drains until the row is terminal; print B_RESULT=<state>.
func TestCancel2ProcWorkerEntry(t *testing.T) {
	role := os.Getenv(envCanRole)
	if role == "" {
		t.Skip("not a worker invocation (set M18_CAN_ROLE)")
	}
	dbPath := os.Getenv(envCanDB)
	owner := os.Getenv(envCanOwner)
	entrySig := os.Getenv(envCanEntrySig)
	l1Marker := os.Getenv(envCanL1Marker)
	ttlMS, _ := strconv.Atoi(os.Getenv(envCanTTLms)) //nolint:errcheck // test env
	if ttlMS < 1 {
		ttlMS = 30000
	}

	// Fast ctx-watcher poll IN THE SUBPROCESS (the package var is process-local; the subprocess must set
	// its own, the parent's override does not cross the process boundary).
	cancelPollIntervalForTest = canWorkerPoll

	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(time.Duration(ttlMS)*time.Millisecond))
	if err != nil {
		t.Fatalf("worker %s open: %v", role, err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	reg := canRegistry(entrySig, l1Marker)

	switch role {
	case "W": // BITE A: one drive; the watcher observes the operator cancel and terminalizes.
		_, rerr := runNext(context.Background(), s, reg, owner, 3)
		if rerr != nil {
			t.Logf("W_DRIVE_ERR=%v", rerr)
		}
		t.Logf("W_RESULT=%s", canWQState(s))
	case "A": // BITE B: claim + enter Execute, block in n0 forever — the parent SIGKILLs us mid-flight.
		// A single drive that will block in n0's <-ctx.Done() until killed; it never returns.
		_, _ = runNext(context.Background(), s, reg, owner, 3) //nolint:errcheck // killed mid-flight; never reached
		t.Logf("A_UNEXPECTED_RETURN=%s", canWQState(s))        // should never print (we get SIGKILL'd)
	case "B": // BITE B: reclaim loop — drain until the row is terminal (or a wall bound).
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if st := canWQState(s); st == wqCancelled || st == wqDone || st == wqFailed {
				t.Logf("B_RESULT=%s", st)
				return
			}
			_, _ = runNext(context.Background(), s, reg, owner, 3) //nolint:errcheck // oracle reads store state
			time.Sleep(15 * time.Millisecond)
		}
		t.Logf("B_RESULT=%s", canWQState(s)) // whatever it converged to (parent oracle is authoritative)
	}
}

// canWQState reads the work_queue state for the cancel workflow (subprocess-local; the parent oracle uses
// its own handle).
func canWQState(s *SQLiteStore) string {
	var st string
	_ = s.db.QueryRow(`SELECT state FROM work_queue WHERE workflow_id=?`, canWFID).Scan(&st) //nolint:errcheck // best-effort
	return st
}

// spawnCanWorker re-execs THIS test binary as a cancel worker (the M16/M17 re-exec pattern).
func spawnCanWorker(role, db, owner, entrySig, l1Marker string, ttlMS int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run", "^TestCancel2ProcWorkerEntry$", "-test.v")
	cmd.Env = append(os.Environ(),
		envCanRole+"="+role,
		envCanDB+"="+db,
		envCanOwner+"="+owner,
		envCanEntrySig+"="+entrySig,
		envCanL1Marker+"="+l1Marker,
		envCanTTLms+"="+strconv.Itoa(ttlMS),
	)
	return cmd
}

// canParentWQState reads the work_queue state via the parent (operator) handle.
func canParentWQState(t *testing.T, s *SQLiteStore) string {
	t.Helper()
	var st string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM work_queue WHERE workflow_id=?`, canWFID).Scan(&st))
	return st
}

// canN1Ran reports whether the L1 marker file exists (i.e. the later-level node ran — under cancel it must
// not).
func canN1Ran(l1Marker string) bool {
	_, err := os.Stat(l1Marker)
	return err == nil
}

// ---- BITE A: cross-process cancel-terminalizes end-to-end ----

// TestCancel2ProcA_CrossProcessCancelTerminalizes — worker W (a real OS process) is mid-Execute of a
// 2-level DAG (n0 blocking at L0), the operator process (parent, store handle only) calls CancelRunning,
// and the durable flag crosses the process boundary → W's ctx-watcher cancels Execute at the level barrier
// → the disposition gate terminalizes `cancelled` → the L1 node NEVER ran. This is the cross-process half
// of TestCtxWatcher_CancelTerminalizesNotResumes (which is in-process only). ORACLE = store state + the L1
// marker file (never created). Seed-break: neuter the disposition-gate flag read (see file header) → W
// leaves-claimed → the row is not `cancelled` → this reddens.
func TestCancel2ProcA_CrossProcessCancelTerminalizes(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns an OS subprocess + drives a real blocking Execute; skipped under -short")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cancelA.db")
	entrySig := filepath.Join(dir, "n0.entry")
	l1Marker := filepath.Join(dir, "n1.ran")

	// Operator handle: pre-create schema (C3) + enqueue the workflow. Long lease TTL so the reclaim race is
	// NOT what terminalizes — the ctx-watcher + disposition gate must be the load-bearing path here.
	op, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(30*time.Second))
	require.NoError(t, err)
	defer func() { _ = op.Close() }() //nolint:errcheck // cleanup
	_, err = op.Enqueue(canWFID, canType, nil)
	require.NoError(t, err)

	// Worker W: claim + drive runNext; n0 blocks at L0 until the ctx-watcher cancels.
	wCmd := spawnCanWorker("W", dbPath, "W-owner", entrySig, l1Marker, 30000)
	var wBuf bytes.Buffer
	wCmd.Stdout = &wBuf
	wCmd.Stderr = &wBuf
	require.NoError(t, wCmd.Start())
	wDone := make(chan error, 1)
	go func() { wDone <- wCmd.Wait() }()

	// Wait for W to be genuinely mid-Execute at L0 (n0 entered) BEFORE the operator cancels. Do NOT read
	// wBuf here — os/exec's copy goroutine is still writing it concurrently (a harness race under -race);
	// wBuf is only read AFTER wCmd.Wait() returns (below), which serializes the copy goroutines.
	require.True(t, waitForFile(entrySig, 5*time.Second), "worker W never entered n0 (mid-Execute)")

	// OPERATOR CANCEL (cross-process): sets the durable cancel_requested flag on W's claimed row.
	requested, err := op.CancelRunning(canWFID)
	require.NoError(t, err)
	require.True(t, requested, "operator sets cancel while W is mid-Execute (cross-process)")

	// W's ctx-watcher observes the flag (≤ canWorkerPoll), cancels Execute at the barrier, the disposition
	// gate terminalizes. W's drive returns → its subprocess exits.
	select {
	case werr := <-wDone: // Wait() returned → the copy goroutines are done → wBuf is safe to read.
		require.NoError(t, werr, "worker W exited cleanly:\n%s", wBuf.String())
	case <-time.After(15 * time.Second):
		_ = wCmd.Process.Kill() //nolint:errcheck // teardown
		<-wDone                 // reap so wBuf is safe to read (no concurrent copy)
		t.Fatalf("worker W did not exit — the ctx-watcher failed to deliver the cross-process cancel:\n%s", wBuf.String())
	}
	wOut := wBuf.String() // captured once, post-Wait — safe under -race

	// ORACLE: the row is terminal `cancelled`, and the L1 node NEVER ran (no marker file).
	require.Equal(t, wqCancelled, canParentWQState(t, op),
		"BITE A: cross-process cancel-of-running → terminal `cancelled` (ctx-watcher + disposition gate delivered it):\n%s", wOut)
	require.False(t, canN1Ran(l1Marker),
		"BITE A: the L1 node NEVER ran — cancel stopped progression at the level barrier (no later-level side effect):\n%s", wOut)

	// Fencing/liveness cross-check: the cancelled row is NOT re-offered to a fresh claim (never resumed).
	fresh, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(30*time.Second))
	require.NoError(t, err)
	defer func() { _ = fresh.Close() }() //nolint:errcheck // cleanup
	_, cerr := fresh.ClaimNext("late-worker", canType)
	require.ErrorIs(t, cerr, ErrNoWork, "BITE A: a `cancelled` row is terminal — never re-offered for a fresh claim (never resumed)")
}

// ---- BITE B: cancel-during-reclaim TOCTOU with a SIGKILL'd owner ----

// TestCancel2ProcB_CancelDuringReclaimSIGKILL — worker A (a real OS process) claims + enters Execute
// (holds the lease + token, blocks in n0), then is SIGKILL'd mid-flight while cancel_requested is set. A
// real 2nd process (reclaimer B) drives a runNext reclaim loop while A's lease lapses. B must NEVER resume
// the cancelled row from the committed frontier — ClaimNext's in-txn cancel-gate terminalizes it
// `cancelled` under B's fresh bumped token (BLOCKER-2). ORACLE = store state (`cancelled`, not stranded
// `claimed`, not `done`) + the L1 marker file (never created — the frontier was never resumed). Seed-break:
// move the reclaim cancel-check terminalize out of the IMMEDIATE txn (see file header) → the interleaving
// either resumes the row (L1 marker appears / state `done`) or strands it `claimed` → this reddens.
func TestCancel2ProcB_CancelDuringReclaimSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns + SIGKILLs OS subprocesses over several seconds; skipped under -short")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cancelB.db")
	entrySig := filepath.Join(dir, "n0.entry")
	l1Marker := filepath.Join(dir, "n1.ran")

	const ttlMS = 400 // short lease TTL: A's claim lapses fast after the SIGKILL → B's reclaim scan discovers it.

	// Operator handle: pre-create schema + enqueue.
	op, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(time.Duration(ttlMS)*time.Millisecond))
	require.NoError(t, err)
	defer func() { _ = op.Close() }() //nolint:errcheck // cleanup
	_, err = op.Enqueue(canWFID, canType, nil)
	require.NoError(t, err)

	// Worker A: claim + enter Execute (blocks in n0), holding the lease + token.
	aCmd := spawnCanWorker("A", dbPath, "A-owner", entrySig, l1Marker, ttlMS)
	var aBuf bytes.Buffer
	aCmd.Stdout = &aBuf
	aCmd.Stderr = &aBuf
	require.NoError(t, aCmd.Start())
	aDone := make(chan struct{})
	go func() { _ = aCmd.Wait(); close(aDone) }() //nolint:errcheck // reaped; A is SIGKILL'd

	// Wait for A to be genuinely mid-Execute (n0 entered, lease + token held). Do NOT read aBuf here —
	// os/exec's copy goroutine is still writing it (a harness race under -race); it is read only after
	// aCmd.Wait() (<-aDone below).
	require.True(t, waitForFile(entrySig, 5*time.Second), "worker A never entered n0 (mid-Execute)")

	// TOCTOU WINDOW: set cancel_requested WHILE A is claimed + running, then SIGKILL A mid-flight (owner
	// crashes with cancel pending — the exact "owner sets/receives cancel then dies before terminalizing"
	// liveness limbo). Order: cancel lands first (A is still claimed), then A dies without terminalizing.
	requested, err := op.CancelRunning(canWFID)
	require.NoError(t, err)
	require.True(t, requested, "operator sets cancel while A owns the claimed row")
	require.NotNil(t, aCmd.Process)
	require.NoError(t, aCmd.Process.Signal(syscall.SIGKILL), "SIGKILL A mid-Execute (crashed while cancel pending)")
	<-aDone // A is dead; its claim + lease persist until the TTL lapses.

	require.Equal(t, wqClaimed, canParentWQState(t, op), "precondition: A left the row `claimed` (crashed mid-flight, never terminalized)")
	require.False(t, canN1Ran(l1Marker), "precondition: A never got past L0 (killed in n0) — L1 never ran")

	// Reclaimer B (a real 2nd process): drive a runNext reclaim loop while A's lease lapses. B's ClaimNext
	// scan must terminalize the lapsed-claimed + cancel-set row `cancelled` IN its IMMEDIATE txn — never
	// resume it from the frontier.
	bOut, _ := spawnCanWorker("B", dbPath, "B-owner", entrySig, l1Marker, ttlMS).CombinedOutput() //nolint:errcheck // B's exit code is not the oracle — the store state is

	// ORACLE: the row converged to `cancelled` (not resumed to `done`, not stranded `claimed`), and the L1
	// node NEVER ran across BOTH processes (the frontier was never resumed).
	require.Equal(t, wqCancelled, canParentWQState(t, op),
		"BITE B: a lapsed-claimed + cancel-set row is terminalized `cancelled` by the reclaimer in-txn — not resumed, not stranded `claimed`:\nB:%s", string(bOut))
	require.Contains(t, string(bOut), "B_RESULT=cancelled",
		"BITE B: reclaimer B observed the row reach `cancelled` (never resumed from the frontier):\n%s", string(bOut))
	require.False(t, canN1Ran(l1Marker),
		"BITE B: the L1 node NEVER ran — B never resumed the cancelled row's committed frontier (no later-level side effect):\nB:%s", string(bOut))

	// Liveness cross-check: the limbo is CLOSED — no non-terminal row strands, and the cancelled row is
	// never re-offered.
	_, cerr := op.ClaimNext("late-worker", canType)
	require.ErrorIs(t, cerr, ErrNoWork, "BITE B: no runnable work remains — the cancelled row was cleaned up, never re-offered (limbo closed)")
}
