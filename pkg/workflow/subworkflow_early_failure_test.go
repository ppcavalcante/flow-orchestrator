package workflow

// Phase A — the two prerequisite lifecycle fixes for input-aware factories (also pre-existing bugs
// reachable today with a zero-arg factory that fails at the worker after enqueue):
//
//   BUG-2 (this file, A1): a QUEUE child that terminalizes WITHOUT a journal (a constructor/seed
//   failure leaves a terminal `failed`/`cancelled` work_queue row and no journal) must resolve as
//   PARENT FAILURE on re-drive, not park forever. The parked-await consults the journal on the
//   ErrNotFound branch and returns ErrSuspended without checking the queue authority (C15).
//
//   BUG-1 (this file, A2): runNext's construction-failure early returns MarkFailed the row but never
//   call deliverSubWorkflowCompletion, so a waiting parent is never woken to observe the failure.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// mkFailParent builds a parent with a single queued sub-workflow node "sub" over childType and drives
// it once so it enqueues the child + parks (child row `pending`, no journal). Returns the parent and childID.
func mkFailParent(t *testing.T, s *SQLiteStore, reg *Registry, childType string) (*Workflow, string) {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID("parent-wf")
	pb.AddSubWorkflowQueued("sub", childType).WithResult("result", "result")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := newWorkflowForTest(s)
	pw.WorkflowID = "parent-wf"
	pw.dag = pdag
	pw.registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")
	return pw, SubWorkflowChildID("parent-wf", "sub")
}

// A1 — BUG-2: a terminally-failed queue child with NO journal makes the re-driven parent FAIL, not park.
func TestQueueChild_TerminalFailedWithoutJournal_ParentResolvesFailure(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "result", nil), nil
	}))
	pw, childID := mkFailParent(t, s, reg, "childType")

	// Simulate a construction/seed failure at the worker: CLAIM the child (pending->claimed, the
	// realistic path — a worker claims before building), then terminalize `failed` WITHOUT ever
	// running it, so no journal is written.
	_, cerr := s.ClaimNext("worker", "childType")
	require.NoError(t, cerr)
	ok, err := s.MarkFailed(childID)
	require.NoError(t, err)
	require.True(t, ok, "child queue row terminalized failed")
	_, lerr := s.Load(childID)
	require.ErrorIs(t, lerr, ErrNotFound, "the failed child has NO journal (construction failed before any action)")

	// Re-drive the parent: queue authority says `failed` → the parent node must FAIL, not park forever.
	rerr := pw.Execute(context.Background())
	require.Error(t, rerr, "a terminally-failed queue child must resolve the parent, not park")
	require.NotErrorIs(t, rerr, ErrSuspended, "the parent must NOT park forever on a terminal-failed child")
	final, err := s.Load("parent-wf")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Failed)
}

// A1 — BUG-2 sibling: a `cancelled` queue child with no journal also resolves as parent failure (not park).
func TestQueueChild_TerminalCancelledWithoutJournal_ParentResolvesFailure(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "result", nil), nil
	}))
	pw, childID := mkFailParent(t, s, reg, "childType")

	// Claim (pending->claimed, sets the fencing token so the terminal flip lands), then terminalize
	// `cancelled` without a journal (an operator cancel before any action ran).
	_, cerr := s.ClaimNext("worker", "childType")
	require.NoError(t, cerr)
	ok, err := s.flipTerminalFenced(childID, wqCancelled)
	require.NoError(t, err)
	require.True(t, ok, "child queue row terminalized cancelled")

	rerr := pw.Execute(context.Background())
	require.Error(t, rerr, "a terminally-cancelled queue child must resolve the parent, not park")
	require.NotErrorIs(t, rerr, ErrSuspended, "the parent must NOT park forever on a terminal-cancelled child")
	final, err := s.Load("parent-wf")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Failed)
}

// A1 — control: a still-pending queue child (no journal) keeps the parent PARKED (the fix must not
// mistake not-yet-run for terminal).
func TestQueueChild_PendingWithoutJournal_ParentStillParks(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "result", nil), nil
	}))
	pw, childID := mkFailParent(t, s, reg, "childType") // child row is `pending`, no journal

	// Re-drive without running the child: still pending → the parent must re-park.
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "a pending queue child → the parent re-parks")
	require.Equal(t, wqPending, wqState(t, s, childID), "the child is still pending")
}
