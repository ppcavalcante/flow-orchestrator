package workflow

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkQueueStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "q.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

// TestQueueSubWorkflow_ParentAddressRoundTrip — the control-plane plumbing (DEC-P94-PARENT-ADDRESS-
// COLUMN): EnqueueSubWorkflow writes parent_id/parent_signal; ClaimNext projects them into WorkItem.
// A plain Enqueue (no parent) → NULL columns → WorkItem.ParentID == "" (the detectable marker).
func TestQueueSubWorkflow_ParentAddressRoundTrip(t *testing.T) {
	s := mkQueueStore(t)

	// A sub-workflow child carries its parent address.
	_, err := s.EnqueueSubWorkflow("child-1", "T", nil, "parent-wf", "subworkflow-complete:sub", 1)
	require.NoError(t, err)
	item, err := s.ClaimNext("worker", "T")
	require.NoError(t, err)
	assert.Equal(t, "child-1", item.WorkflowID)
	assert.Equal(t, "parent-wf", item.ParentID, "parent id round-trips through the control column")
	assert.Equal(t, "subworkflow-complete:sub", item.ParentSignal)

	// A plain M17 dispatch has no parent → NULL columns → empty.
	_, err = s.Enqueue("plain-1", "T", nil)
	require.NoError(t, err)
	item2, err := s.ClaimNext("worker", "T")
	require.NoError(t, err)
	assert.Equal(t, "plain-1", item2.WorkflowID)
	assert.Equal(t, "", item2.ParentID, "a plain dispatch has no parent (NULL column) — the detectable marker")
}

// TestQueueSubWorkflow_EnqueueSubWorkflow_Guards — empty parent id/sig rejected; non-MP store rejected.
func TestQueueSubWorkflow_EnqueueSubWorkflow_Guards(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.EnqueueSubWorkflow("c", "T", nil, "", "sig", 1)
	require.ErrorIs(t, err, ErrValidation, "empty parent id rejected")
	_, err = s.EnqueueSubWorkflow("c", "T", nil, "p", "", 1)
	require.ErrorIs(t, err, ErrValidation, "empty parent signal rejected")

	// A non-multi-process store cannot enqueue.
	sp, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sp.db"))
	require.NoError(t, err)
	defer sp.Close() //nolint:errcheck // cleanup
	_, err = sp.EnqueueSubWorkflow("c", "T", nil, "p", "sig", 1)
	require.ErrorIs(t, err, ErrValidation, "single-process store rejects EnqueueSubWorkflow")
}

// TestQueueSubWorkflow_CompletionHook_WakesParent — the ⭐ mechanism in miniature: a parent parks on
// a queue child; a worker RunNext-runs the child; the RunNext-after-MarkDone completion hook delivers
// the signal to the parent's mailbox; the parent (re-driven) wakes and reads the child result. All on
// one *SQLiteStore (the 2-OS-process arm is slice B).
func TestQueueSubWorkflow_CompletionHook_WakesParent(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	// The child type: a one-node DAG that sets the "result" data key.
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "queued-result", nil), nil
	}))

	// The parent: a queue sub-workflow node "sub" (child type "childType") + a downstream "after".
	// AddSubWorkflowQueued carries only the TYPE STRING; the Registry (CODE) is injected on the Workflow.
	var afterN atomic.Int32
	pb := NewWorkflowBuilder().WithWorkflowID("parent-wf")
	pb.AddSubWorkflowQueued("sub", "childType").WithResult("result", "result")
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(&afterN))
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent-wf"
	pw.DAG = pdag
	pw.Registry = reg // the ctx-injected Registry (Q2 ruling)

	// First drive: the parent enqueues the child + parks.
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")
	parked, err := s.Load("parent-wf")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sub", Waiting)

	// A worker runs the child. RunNext claims it, runs it, MarkDone → the completion hook fires.
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran, "the worker claimed + ran the enqueued child")

	// The completion HOOK delivered the signal to the parent's mailbox — THIS is what the hook produces
	// (the wake trigger a host DeliverAndResume-drives the parent on). Assert it is present: dropping the
	// hook leaves the mailbox EMPTY (the bite — this assertion reddens without the hook).
	box, terr := s.TakeSignals("parent-wf")
	require.NoError(t, terr)
	require.Len(t, box, 1, "the completion hook delivered a signal to the parent mailbox (the wake trigger)")
	assert.Equal(t, completionSignalName("sub"), box[0].Name, "the signal is named for the parked node")

	// The host wakes the parent on that signal → the parent re-checks the child journal + reads the result.
	require.NoError(t, pw.Execute(context.Background()))
	final, err := s.Load("parent-wf")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "queued-result", result, "the parent reads the queue-child's result on wake")
}

// TestQueueSubWorkflow_PlainDispatch_NoCompletionSignal — the detectable-marker + no-forge property:
// a PLAIN M17-dispatched workflow (Enqueue, no parent columns) runs via RunNext and emits NO
// completion signal (nothing to signal — ParentID == ""). Bite: the completion hook only fires when
// ParentID != "", so a plain dispatch never delivers a spurious signal.
func TestQueueSubWorkflow_PlainDispatch_NoCompletionSignal(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("plainType", func() (*DAG, error) {
		return childProducing(t, "v", nil), nil
	}))
	_, err := s.Enqueue("plain-wf", "plainType", nil) // plain — NO parent
	require.NoError(t, err)

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)

	// No parent → the completion hook emitted nothing. (A spurious signal would land under some
	// mailbox; there is no parent workflow, so assert the plain workflow's own mailbox is empty AND
	// no signal was misdelivered — the ParentID=="" guard prevents any DeliverSignal.)
	box, terr := s.TakeSignals("plain-wf")
	require.NoError(t, terr)
	require.Empty(t, box, "a plain M17 dispatch (no parent) emits no completion signal")
}

// TestQueueSubWorkflow_CancelPropagation — parent-cancel of a QUEUE child sets the child's
// cancel_requested (via the deterministic child ID) → the cancelWatcher (running inside RunNext)
// observes it and cancels the child's run ctx → the child terminalizes `cancelled`, no orphaned
// journal writes. This is the queue arm of DEC-M19-FAILURE cancel-prop (inline arm = parent ctx, ph91).
func TestQueueSubWorkflow_CancelPropagation(t *testing.T) {
	// Fast ctx-watcher poll so the cancel is observed promptly (package var, restored via defer).
	orig := cancelPollIntervalForTest
	cancelPollIntervalForTest = 25 * time.Millisecond
	defer func() { cancelPollIntervalForTest = orig }()

	s := mkQueueStore(t)
	reg := NewRegistry()
	// A child whose single node BLOCKS on ctx until cancelled (models a long-running / parked child).
	reg.Register("blockType", func() (*DAG, error) { //nolint:errcheck // test registry
		cb := NewWorkflowBuilder()
		cb.AddStartNode("block").WithAction(ActionFunc(func(ctx context.Context, _ *WorkflowData) error {
			<-ctx.Done() // block until the cancelWatcher cancels the run ctx
			return ctx.Err()
		}))
		dag, _ := cb.Build() //nolint:errcheck // known-good
		return dag, nil
	})

	// The parent's deterministic child ID (as the queue producer would compute it).
	childID := subWorkflowChildID("parent-wf", "sub")
	_, err := s.EnqueueSubWorkflow(childID, "blockType", nil, "parent-wf", completionSignalName("sub"), 1)
	require.NoError(t, err)

	// A worker runs the child in a goroutine — it will block in "block" until cancelled.
	done := make(chan struct{})
	go func() {
		_, _ = runNext(context.Background(), s, reg, "worker", 3) //nolint:errcheck // the child is cancelled mid-run
		close(done)
	}()

	// Give the worker time to claim + enter the blocking node, then cancel the parent's child.
	require.Eventually(t, func() bool { return wqState(t, s, childID) == wqClaimed }, 3*time.Second, 10*time.Millisecond,
		"the worker claims + starts the child")
	requested, err := s.CancelRunning(childID)
	require.NoError(t, err)
	require.True(t, requested, "parent-cancel sets the child's cancel_requested")

	// The cancelWatcher observes it → cancels the child ctx → the child returns ctx.Err() → cancelled.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the child did not terminate after cancel (cancelWatcher did not propagate)")
	}
	require.Eventually(t, func() bool { return wqState(t, s, childID) == wqCancelled }, 3*time.Second, 10*time.Millisecond,
		"the cancelled child terminalizes `cancelled` (no orphaned run)")
}

// TestQueueSubWorkflow_Routing_SealedInlineRefusalStands — DEC-P92-MODE-SEAM (Q1=(b)): AddSubWorkflow
// (inline) keeps its ph91 SEALED refusal of a suspendable child (build error), while
// AddSubWorkflowQueued accepts a queue child. The two entries make the inline-vs-queue choice explicit;
// the closure-scan predicate validates which is legal for a given child.
func TestQueueSubWorkflow_Routing_SealedInlineRefusalStands(t *testing.T) {
	// A suspendable child (contains an approval node).
	suspendableChild := childWithSuspendable(t)

	// AddSubWorkflow (inline) REFUSES it — the ph91 sealed behavior, unchanged.
	inline := NewWorkflowBuilder().WithWorkflowID("wf-inline-susp")
	inline.AddSubWorkflow("sub", suspendableChild)
	_, err := inline.Build()
	require.ErrorIs(t, err, ErrSubWorkflowSuspendableChild, "inline AddSubWorkflow refuses a suspendable child (ph91 sealed)")

	// AddSubWorkflowQueued ACCEPTS a queue child by type (the queue path handles suspendable children).
	// It builds clean — the suspendability is fine on the queue path (the worker runs + parks the child).
	queued := NewWorkflowBuilder().WithWorkflowID("wf-queued")
	queued.AddSubWorkflowQueued("sub", "someType")
	_, qerr := queued.Build()
	require.NoError(t, qerr, "AddSubWorkflowQueued accepts a type-ref child (the explicit queue opt-in)")

	// An empty child type is a build error.
	bad := NewWorkflowBuilder().WithWorkflowID("wf-badtype")
	bad.AddSubWorkflowQueued("sub", "")
	_, berr := bad.Build()
	require.ErrorIs(t, berr, ErrValidation, "an empty child type is rejected at build")
}

// TestQueueSubWorkflow_NoRegistry_LoudFail — a queue node reached with NO Registry on ctx →
// ErrSubWorkflowRequiresRegistry (the honesty analog of ErrWaitRequiresSignalStore), never a silent
// failure. The Registry is the execution environment's (injected on the Workflow); a workflow driven
// without one cannot resolve the child type.
func TestQueueSubWorkflow_NoRegistry_LoudFail(t *testing.T) {
	s := mkQueueStore(t)
	pb := NewWorkflowBuilder().WithWorkflowID("wf-noreg")
	pb.AddSubWorkflowQueued("sub", "someType")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "wf-noreg"
	pw.DAG = pdag
	// NO pw.Registry set → the queue node fails loudly.
	err = pw.Execute(context.Background())
	require.ErrorIs(t, err, ErrSubWorkflowRequiresRegistry, "no Registry → loud failure, never a silent no-op")
}

// TestQueueSubWorkflow_EndToEnd_ApprovalChild_CrashResume — ⭐ THE phase's reason to exist. A parent
// awaits a QUEUE-dispatched child that ITSELF parks on a ph90 approval, across a parent crash+resume:
//
//	parent enqueues + parks → worker RunNexts the child → child parks on ITS approval (not terminal, no
//	completion yet) → [parent "crash": reopen store] → deliver the child's approval + re-drive the child
//	→ child terminal → completion hook wakes the parent → parent (resumed) reads the child's result.
//
// Proven on *SQLiteStore end-to-end. This is the full composition pillar: queue + a suspendable child +
// the wake + crash-safety, all on the durable mailbox.
func TestQueueSubWorkflow_EndToEnd_ApprovalChild_CrashResume(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2e.db")
	// A short lease TTL so the parked child's lease lapses quickly → the reclaim scan re-offers + resumes
	// it after its approval arrives. A suspendable queue child resumes via reclaim-after-lease-lapse (the
	// same crash-recovery path — a park leaves the row claimed; resume latency is bounded by the TTL).
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(50*time.Millisecond))
		require.NoError(t, err)
		return s
	}
	// The child TYPE: an approval gate "gate" → a "produce" node that sets "result". A suspendable child.
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddApproval("gate")
		cb.AddNode("produce").DependsOn("gate").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", "approved-e2e")
			return nil
		}))
		return cb.Build()
	}))

	// --- Drive 1: the parent enqueues the child + parks. ---
	store1 := open()
	pb := NewWorkflowBuilder().WithWorkflowID("e2e-parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild").WithResult("result", "result")
	var afterN atomic.Int32
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(&afterN))
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw1 := NewWorkflow(store1)
	pw1.WorkflowID = "e2e-parent"
	pw1.DAG = pdag
	pw1.Registry = reg
	require.ErrorIs(t, pw1.Execute(context.Background()), ErrSuspended, "parent enqueues the child + parks")

	// The worker runs the child → the child parks on its own approval (not terminal → no completion yet).
	ran, err := RunNext(context.Background(), store1, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran, "worker claimed + ran the child; the child parks on its approval")
	childID := subWorkflowChildID("e2e-parent", "sub")
	childState, _ := store1.Load(childID) //nolint:errcheck // asserted below
	assertNodeStatus(t, childState, "gate", Waiting)
	// No completion signal yet — the child is not terminal.
	require.Empty(t, mustTake(t, store1, "e2e-parent"), "no completion until the child is terminal")

	// --- "Crash": drop the in-memory stores, reopen fresh on the same file (the resume path). ---
	require.NoError(t, store1.Close())
	store2 := open()
	defer store2.Close() //nolint:errcheck // cleanup

	// Deliver the child's approval to the CHILD's mailbox. Wait for the parked child's lease to lapse
	// (store1 was closed without releasing it — TTL-lapse is the reclaim trigger), then a worker
	// reclaims + resumes the child → its approval completes it → terminal → the completion hook fires.
	require.NoError(t, store2.DeliverSignal(childID, ApproveSignal("gate", "alice", "ok", "d1")))
	require.Eventually(t, func() bool {
		ran, rerr := RunNext(context.Background(), store2, reg, "worker")
		require.NoError(t, rerr)
		return ran && wqState(t, store2, childID) == wqDone
	}, 3*time.Second, 25*time.Millisecond, "the lapsed child is reclaimed + resumed to done after its approval")

	// The completion hook fired on the reopened store → the parent's mailbox has the wake trigger.
	require.Len(t, mustTake(t, store2, "e2e-parent"), 1, "the completion hook woke the parent across the crash")

	// The parent (resumed on the reopened store) wakes + reads the child's result.
	pw2 := NewWorkflow(store2)
	pw2.WorkflowID = "e2e-parent"
	pw2.DAG = pdag
	pw2.Registry = reg
	require.NoError(t, pw2.Execute(context.Background()))
	final, err := store2.Load("e2e-parent")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	assertNodeStatus(t, final, "after", Completed)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "approved-e2e", result, "⭐ the parent reads the approval-child's result end-to-end, across a crash")
}

// mustTake is a non-destructive mailbox read for the E2E assertions.
func mustTake(t *testing.T, s *SQLiteStore, wf string) []Signal {
	t.Helper()
	sigs, err := s.TakeSignals(wf)
	require.NoError(t, err)
	return sigs
}
