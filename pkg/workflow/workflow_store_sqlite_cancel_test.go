package workflow

// M18 ph87 Slice 1 — the deterministic (store-level) cancel-of-running bites. The 2 real-2-proc bites
// (cancel-terminalizes-not-resumes end-to-end + cancel-during-reclaim TOCTOU) are Slice 2. Each bite here
// has a gate-drop seed-break noted; the dedicated adversarial-tester rejoins at verify.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCancelRunning_Idempotent (fencing-safe + idempotent, BITE 4 store-level) — CancelRunning sets the
// flag once; a double-cancel + a cancel-of-terminal are detectable 0-row no-ops; no token required.
func TestCancelRunning_Idempotent(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)

	requested, err := s.CancelRunning("wf") // pending row → flag set
	require.NoError(t, err)
	require.True(t, requested, "first cancel sets the flag")

	requested, err = s.CancelRunning("wf") // double-cancel → 0-row no-op (flag already set)
	require.NoError(t, err)
	require.False(t, requested, "double-cancel is a detectable no-op (idempotent)")

	// cancel of a TERMINAL row → no-op (state not in pending|claimed).
	_, err = s.ClaimNext("w")
	require.NoError(t, err)
	s.setToken("wf", 1)
	_, err = s.MarkDone("wf") // now terminal
	require.NoError(t, err)
	requested, err = s.CancelRunning("wf")
	require.NoError(t, err)
	require.False(t, requested, "cancel of a terminal row is a no-op (never resurrects)")
}

// TestDispositionGate_CancelTerminalizes (BLOCKER-1) — a bare context.Canceled with cancel_requested SET
// terminalizes `cancelled` (NOT leave-claimed). SEED-BREAK: drop the isCancelRequested gate → the row
// would leave-claimed (AF1 path) → the cancelled assertion reddens.
func TestDispositionGate_CancelTerminalizes(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w") // claimed, token 1
	require.NoError(t, err)
	s.setToken("wf", 1)

	// operator requests cancel while A owns it.
	requested, err := s.CancelRunning("wf")
	require.NoError(t, err)
	require.True(t, requested)

	// A's Execute returns a bare context.Canceled (the ctx-watcher cancelled it). disposeExecErr with the
	// flag SET → terminalize cancelled.
	require.Error(t, disposeExecErr(s, "wf", 3, wrapCtxErr()))
	require.Equal(t, wqCancelled, wqState(t, s, "wf"), "BLOCKER-1: cancel_requested SET + context.Canceled → terminal `cancelled` (not leave-claimed)")
}

// TestDispositionGate_DrainStillLeavesClaimed (cancel≠drain, BITE 2 — AF1 NOT regressed) — a bare
// context.Canceled with cancel_requested UNSET STILL leaves the row `claimed` (the AF1 path). SEED-BREAK:
// fire the gate on any context.Canceled ignoring the flag → drain wrongly terminalizes → this reddens.
func TestDispositionGate_DrainStillLeavesClaimed(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w")
	require.NoError(t, err)
	s.setToken("wf", 1)

	// NO CancelRunning — a graceful drain (parent ctx cancelled, flag UNSET).
	require.Error(t, disposeExecErr(s, "wf", 3, wrapCtxErr()))
	require.Equal(t, wqClaimed, wqState(t, s, "wf"), "cancel≠drain: an UNSET-flag context.Canceled STILL leaves `claimed` for TTL-reclaim (AF1 not regressed)")
}

// TestReclaimTerminalizesCancelled_ClosesLimbo (BLOCKER-2 liveness, the 5th bite store-level) — a
// crashed-while-cancel-pending row (claimed + cancel_requested SET + lease lapsed, owner gone) is
// TERMINALIZED `cancelled` by a reclaimer's ClaimNext IN the txn — NOT stranded in claimed limbo, NOT
// resumed. SEED-BREAK: restore the `AND cancel_requested IS NULL` exclusion (or drop the in-txn
// terminalize) → the row strands `claimed` (ClaimNext returns ErrNoWork, never cleans it) → reddens.
func TestReclaimTerminalizesCancelled_ClosesLimbo(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("A") // A claims, token 1
	require.NoError(t, err)

	// operator sets cancel while A owns it; then A "crashes" (never terminalizes) and its lease lapses.
	requested, err := s.CancelRunning("wf")
	require.NoError(t, err)
	require.True(t, requested)
	clk.Advance(6 * time.Second) // A's lease lapses → the row is reclaim-eligible

	// a reclaimer B scans: the row is lapsed-claimed + cancel-set → B terminalizes it `cancelled` IN the
	// txn (does NOT resume it), then finds no more work → ErrNoWork. The limbo is CLOSED.
	_, err = s.ClaimNext("B")
	require.ErrorIs(t, err, ErrNoWork, "no runnable work remains — the cancelled row was cleaned up, not offered")
	require.Equal(t, wqCancelled, wqState(t, s, "wf"), "BLOCKER-2 liveness: a crashed-while-cancel-pending row is terminalized `cancelled` by the reclaimer — not stranded in claimed limbo, not resumed")
}

// TestPostReclaimReRead_CancelBeforeExecute (BLOCKER-2 defense-in-depth) — a cancel landing on a claimed
// row between ClaimNext and Execute-start is caught by runNext's post-reclaim re-read → terminalized
// cancelled, never executed. We simulate the window by setting the flag AFTER a claim but exercising the
// re-read path via runNext directly.
func TestPostReclaimReRead_CancelBeforeExecute(t *testing.T) {
	s := mkDispatchStore(t)
	ctr := newRunCounter()
	reg := NewRegistry()
	require.NoError(t, reg.Register("T", func() (*DAG, error) {
		d := NewDAG("T")
		return d, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n0"); return nil })))
	}))
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	// pre-set the cancel flag on the pending row → RunNext claims it, the post-reclaim re-read sees the
	// flag → terminalizes cancelled WITHOUT running n0.
	requested, err := s.CancelRunning("wf")
	require.NoError(t, err)
	require.True(t, requested)

	ran, err := RunNext(context.Background(), s, reg, "w")
	require.NoError(t, err)
	require.True(t, ran, "the item was handled (terminalized cancelled)")
	require.Equal(t, wqCancelled, wqState(t, s, "wf"), "post-reclaim re-read: a cancel-set claim is terminalized cancelled before Execute")
	require.Zero(t, ctr.get("n0"), "n0 NEVER ran — the workflow was cancelled before Execute")
}

// wrapCtxErr returns a bare %w-wrapped context.Canceled (what dag.go returns at the level barrier on a
// cancelled parent ctx) — NOT an *ExecutionError, so it takes the disposition-gate path.
func wrapCtxErr() error { return fmt.Errorf("workflow cancelled during level 0: %w", context.Canceled) }

// TestCtxWatcher_CancelTerminalizesNotResumes (Slice 2, BITE 1 — the delivery end-to-end) — a worker mid-
// Execute of a MULTI-LEVEL DAG (n0 blocks until released, then n1) has cancel_requested set mid-flight;
// the ctx-watcher (short poll) cancels the Execute ctx → Execute stops at the next LEVEL barrier (dag.go's
// guard) → runNext's disposeExecErr disposition gate terminalizes `cancelled`; n1 (the later level) NEVER
// ran. In-process but faithful to the real path (RunNext drives the real ctx-watcher + Execute + gate).
// SEED-BREAK: the disposition-gate drop (proven by TestDispositionGate_CancelTerminalizes) covers the
// terminalize; here we prove the DELIVERY (the watcher actually cancels a running Execute).
func TestCtxWatcher_CancelTerminalizesNotResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real Execute with a blocking node + the ctx-watcher poll")
	}
	// Short poll so the test doesn't wait 500ms — inject via a package-var override for the test.
	oldPoll := cancelPollIntervalForTest
	cancelPollIntervalForTest = 20 * time.Millisecond
	defer func() { cancelPollIntervalForTest = oldPoll }()

	s := mkDispatchStore(t)
	ctr := newRunCounter()
	n0Entered := make(chan struct{})
	reg := NewRegistry()
	require.NoError(t, reg.Register("chain", func() (*DAG, error) {
		d := NewDAG("chain")
		// n0: signal entry, then block until the ctx is cancelled (models a long-running node at level 0).
		if err := d.AddNode(NewNode("n0", ActionFunc(func(ctx context.Context, _ *WorkflowData) error {
			ctr.inc("n0")
			close(n0Entered)
			<-ctx.Done() // block until cancel → return the ctx error (a well-behaved node)
			return ctx.Err()
		}))); err != nil {
			return nil, err
		}
		// n1 at level 1 — must NEVER run (cancel stops progression at the level barrier after n0).
		if err := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n1"); return nil }))); err != nil {
			return nil, err
		}
		return d, d.AddDependency("n0", "n1")
	}))
	_, err := s.Enqueue("wf", "chain", nil)
	require.NoError(t, err)

	// drive RunNext in a goroutine; once n0 is entered, the operator sets cancel_requested.
	runDone := make(chan error, 1)
	go func() {
		_, rerr := RunNext(context.Background(), s, reg, "w")
		runDone <- rerr
	}()

	<-n0Entered // n0 is running (mid-Execute at level 0)
	requested, err := s.CancelRunning("wf")
	require.NoError(t, err)
	require.True(t, requested, "operator sets cancel while the worker is mid-Execute")

	select {
	case <-runDone: // RunNext returned — the watcher cancelled Execute, the disposition gate terminalized.
	case <-time.After(5 * time.Second):
		t.Fatal("RunNext did not return — the ctx-watcher failed to cancel the running Execute")
	}

	require.Equal(t, wqCancelled, wqState(t, s, "wf"), "BITE 1: cancel-of-running → terminal `cancelled` (delivered via the ctx-watcher + disposition gate)")
	require.Equal(t, 1, ctr.get("n0"), "n0 ran (it was executing when cancelled)")
	require.Zero(t, ctr.get("n1"), "n1 (the later level) NEVER ran — cancel stopped progression at the level barrier")
}
