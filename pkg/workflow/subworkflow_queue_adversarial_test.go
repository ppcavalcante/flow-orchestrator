package workflow

// M19 ph94 — ADVERSARIAL suite for the queue-dispatch producer's CROSS-WORKFLOW control signal
// (a child's completion wakes a PARENT) + the ErrSuspended-park seam under the crash/reclaim matrix.
//
// These tests attack the security core (can the input BLOB or a forged/misrouted signal redirect a
// completion to the wrong parent?) and the park→reclaim matrix (repeatedly-parked child, fencing on a
// reclaimed-parked child, completion idempotency). Deterministic where possible (FakeClock lease-lapse),
// race-safe (per-test t.TempDir() stores; no package-global mutation) — the ph93 substrate discipline.
//
// Provenance: authored by the anvil-adversarial-tester gate for ph94. See ADVERSARIAL.md.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkClockedQueueStore opens an mp *SQLiteStore on a fresh temp file with an injected FakeClock + a
// short lease TTL, so a parked child's lease lapses DETERMINISTICALLY on clk.Advance (no wall-clock
// sleep). Returns the store, the path (for a second handle), and the clock.
func mkClockedQueueStore(t *testing.T, clk *FakeClock, ttl time.Duration) (*SQLiteStore, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "adv.db")
	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s, dbPath
}

// approvalChildDAG builds a suspendable child: an approval gate "gate" → a "produce" node that sets
// "result" and bumps produceN (so a double-resume is detectable). Mirrors the ⭐ E2E child.
func approvalChildDAG(produceN *atomic.Int32, out string) (*DAG, error) {
	cb := NewWorkflowBuilder()
	cb.AddApproval("gate")
	cb.AddNode("produce").DependsOn("gate").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		if produceN != nil {
			produceN.Add(1)
		}
		d.Set("result", out)
		return nil
	}))
	return cb.Build()
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// PRIMARY TARGET 1 — the cross-workflow signal (the security core).
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestQueueAdversarial_InputBLOBCannotForgeCompletionTarget — DEC-P94-PARENT-ADDRESS-COLUMN, defense
// by construction. The child's INPUT BLOB carries an injected `parent_id`/`ParentID`/`_subworkflow.parentID`
// aimed at a VICTIM parent. The completion target is the engine-set CONTROL column (item.ParentID), NOT
// the input BLOB — so the injected keys reach the child JOURNAL (as plain data) but the completion signal
// goes to the REAL engine-set parent, never the victim. This is the attacker-cannot-forge-the-target proof.
func TestQueueAdversarial_InputBLOBCannotForgeCompletionTarget(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "queued-result", nil), nil
	}))

	const realParent = "real-parent-A"
	const victim = "victim-parent-B"
	childID := subWorkflowChildID(realParent, "sub")

	// The attacker-controlled input BLOB tries to redirect the completion to the victim via every
	// plausible key name. seedInput will Set these as CHILD data keys — they must NOT reach the columns.
	craft := []byte(`{"parent_id":"` + victim + `","ParentID":"` + victim +
		`","parent_signal":"subworkflow-complete:evil","_subworkflow.parentID":"` + victim + `"}`)

	// Engine-set address = the REAL parent (as the producer would set it). Input is the crafted BLOB.
	_, err := s.EnqueueSubWorkflow(childID, "childType", craft, realParent, completionSignalName("sub"), 1)
	require.NoError(t, err)

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran, "the worker ran the child")

	// The completion went to the REAL engine-set parent — exactly one signal.
	toReal, err := s.TakeSignals(realParent)
	require.NoError(t, err)
	require.Len(t, toReal, 1, "completion delivered to the engine-set parent, not the input-BLOB target")
	assert.Equal(t, completionSignalName("sub"), toReal[0].Name)

	// The VICTIM named in the input BLOB received NOTHING — the input cannot forge the target.
	toVictim, err := s.TakeSignals(victim)
	require.NoError(t, err)
	require.Empty(t, toVictim, "the input-BLOB-named victim parent must receive NO completion signal")

	// BITE: the crafted input WAS processed — it landed in the child journal as ordinary data keys — yet
	// it did not affect routing. (If the assertion below fails, the input never reached the child and the
	// test is vacuous; it passing is what makes the no-forge result load-bearing.)
	child, err := s.Load(childID)
	require.NoError(t, err)
	got, ok := child.Get("parent_id")
	require.True(t, ok, "the crafted input reached the child journal (proves the input was live, not ignored)")
	assert.Equal(t, victim, got, "the injected key is present as inert child DATA — never a work_queue column")
}

// TestQueueAdversarial_CompletionForChildOfA_CannotWakeParentB — TWO parents A,B each parked on their
// OWN queue child. Running A's child to terminal delivers the completion to A ONLY; B's mailbox stays
// empty and B re-parks (its own child never ran). Two independent layers: (1) the engine-set column
// targets A; (2) the ph92 journal-gate — B completes ONLY for ITS OWN deterministic child ID.
func TestQueueAdversarial_CompletionForChildOfA_CannotWakeParentB(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "A-result", nil), nil
	}))

	mkParent := func(id string) *Workflow {
		pb := NewWorkflowBuilder().WithWorkflowID(id)
		pb.AddSubWorkflowQueued("sub", "childType").WithResult("result", "result")
		pdag, err := pb.Build()
		require.NoError(t, err)
		w := NewWorkflow(s)
		w.WorkflowID = id
		w.DAG = pdag
		w.Registry = reg
		return w
	}
	pa, pb := mkParent("parent-A"), mkParent("parent-B")

	// Both parents enqueue their child + park.
	require.ErrorIs(t, pa.Execute(context.Background()), ErrSuspended)
	require.ErrorIs(t, pb.Execute(context.Background()), ErrSuspended)

	// Run ONLY A's child. ClaimNext picks the oldest pending → A's child (enqueued first).
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	aChild := subWorkflowChildID("parent-A", "sub")
	require.Equal(t, wqDone, wqState(t, s, aChild), "A's child ran to done")

	// Layer 1: the completion is addressed to A only.
	require.Len(t, mustTake(t, s, "parent-A"), 1, "A's child completion woke A")
	require.Empty(t, mustTake(t, s, "parent-B"), "A's child completion NEVER lands in B's mailbox")

	// Layer 2 (journal-gate): even a forged completion delivered to B — B's child never terminalized —
	// must NOT wake B. Deliver a completion signal to B's mailbox by hand and re-drive B.
	require.NoError(t, s.DeliverSignal("parent-B", SubWorkflowCompletionSignal("sub", "forged-for-B")))
	require.ErrorIs(t, pb.Execute(context.Background()), ErrSuspended,
		"B re-parks: its own deterministic child is not terminal — the journal-gate ignores a spurious signal")
	bState, err := s.Load("parent-B")
	require.NoError(t, err)
	assertNodeStatus(t, bState, "sub", Waiting)
}

// TestQueueAdversarial_ForgedCompletionWhileChildParked_ReParks — a premature/duplicate completion
// delivered to a parent while its child is STILL parked (not terminal) → the parent re-parks, never a
// false wake. The signal is only a TRIGGER; the child JOURNAL being terminal is the gate.
func TestQueueAdversarial_ForgedCompletionWhileChildParked_ReParks(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s, _ := mkClockedQueueStore(t, clk, 50*time.Millisecond)
	var produceN atomic.Int32
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		return approvalChildDAG(&produceN, "approved")
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild").WithResult("result", "result")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")

	// The worker runs the child → it parks on its own approval (NOT terminal → no real completion yet).
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	childID := subWorkflowChildID("parent", "sub")
	require.Equal(t, wqClaimed, wqState(t, s, childID), "the parked child stays claimed (a park is not a disposition)")

	// A premature/duplicate completion arrives at the parent while the child is still parked.
	require.NoError(t, s.DeliverSignal("parent", SubWorkflowCompletionSignal("sub", "premature")))

	// The parent re-drives → the child journal is NOT terminal → re-park. No false wake.
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended,
		"a premature completion does not wake the parent while the child is non-terminal")
	pState, err := s.Load("parent")
	require.NoError(t, err)
	assertNodeStatus(t, pState, "sub", Waiting)
	require.EqualValues(t, 0, produceN.Load(), "the child never produced (still parked)")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// PRIMARY TARGET 2 — the ErrSuspended-PARK seam under the crash + reclaim matrix.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestQueueAdversarial_RepeatedlyParkedChild_NotDeadLettered_ResumesOnce — the architect's prime target
// (2a + 2b). A child parks, is reclaimed, parks again — N times, N > maxAttempts (default 3). The park
// path returns BEFORE disposeExecErr, so attempts-exhaustion NEVER dead-letters the parked child: the row
// stays `claimed`, never `failed`, across all N park→reclaim cycles. Then its approval arrives → a final
// reclaim resumes it to `done` EXACTLY ONCE (produce runs once). Deterministic via FakeClock lease-lapse.
func TestQueueAdversarial_RepeatedlyParkedChild_NotDeadLettered_ResumesOnce(t *testing.T) {
	const ttl = 50 * time.Millisecond
	clk := NewFakeClock(time.Unix(1000, 0))
	s, _ := mkClockedQueueStore(t, clk, ttl)
	var produceN atomic.Int32
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		return approvalChildDAG(&produceN, "resumed-once")
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild").WithResult("result", "result")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended)
	childID := subWorkflowChildID("parent", "sub")

	// Drive #1 (fresh claim): the child parks on its approval. attempts -> 1.
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, wqClaimed, wqState(t, s, childID))

	// Park→reclaim N more times WITHOUT delivering the approval. Each reclaim bumps attempts.
	const parks = 5 // > defaultMaxAttempts (3)
	for i := range parks {
		clk.Advance(ttl + time.Millisecond) // lapse the lease → the row becomes a reclaim candidate
		ran, err = RunNext(context.Background(), s, reg, "worker")
		require.NoError(t, err)
		require.True(t, ran, "the lapsed parked child was reclaimed + re-driven (park %d)", i+1)
		require.Equal(t, wqClaimed, wqState(t, s, childID),
			"a repeatedly-parked child is NEVER dead-lettered by attempts exhaustion (park %d)", i+1)
	}
	require.NotEqual(t, wqFailed, wqState(t, s, childID), "the parked child was never dead-lettered")
	require.EqualValues(t, 0, produceN.Load(), "still parked at the gate — never produced")

	// Now deliver the approval; one more reclaim resumes the child to done — EXACTLY once.
	require.NoError(t, s.DeliverSignal(childID, ApproveSignal("gate", "alice", "ok", "appr")))
	clk.Advance(ttl + time.Millisecond)
	ran, err = RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, wqDone, wqState(t, s, childID), "the approved child resumes to done after N parks")
	require.EqualValues(t, 1, produceN.Load(), "the child resumed EXACTLY once (no double-apply across parks)")

	// The completion woke the parent; it reads the result.
	require.Len(t, mustTake(t, s, "parent"), 1, "the completion hook fired once on the terminal child")
	require.NoError(t, pw.Execute(context.Background()))
	final, err := s.Load("parent")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "resumed-once", result)
}

// TestQueueAdversarial_ParkReclaim_PreservesRetryBudget — FINDING F-P94-ADV-01 (the "any OTHER problem"
// PRIMARY TARGET 2a asks to check). The ErrSuspended seam fix stops the immediate dead-letter, but each
// park→reclaim cycle used to bump the SHARED `attempts` counter (F-P94-ADV-01 / F-P94-01) → after >=
// maxAttempts benign parks the retry budget was spent, dead-lettering a legitimately-resumable child on a
// coincident transient fault. FIXED: a park RESETS attempts (durable progress, not a failed attempt). This
// test now asserts the FIXED contract — parks do not accumulate attempts, and a post-many-parks transient
// fault REQUEUES the child (budget preserved). It flipped from the reproduced defect the day the fix landed.
func TestQueueAdversarial_ParkReclaim_PreservesRetryBudget(t *testing.T) {
	const ttl = 50 * time.Millisecond
	const maxAttempts = 3
	clk := NewFakeClock(time.Unix(1000, 0))
	s, _ := mkClockedQueueStore(t, clk, ttl)
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		return approvalChildDAG(nil, "x")
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended)
	childID := subWorkflowChildID("parent", "sub")

	// Fresh claim + (maxAttempts-1) reclaims — each a benign PARK. FIXED (F-P94-01): a park RESETS
	// attempts (a park is durable progress, not a failed attempt), so attempts does NOT accumulate —
	// the retry budget is preserved across arbitrarily many parks.
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	for range maxAttempts + 2 { // MORE than maxAttempts parks — the pre-fix defect would have exhausted the budget
		clk.Advance(ttl + time.Millisecond)
		ran, err = RunNext(context.Background(), s, reg, "worker")
		require.NoError(t, err)
		require.True(t, ran)
	}
	require.EqualValues(t, 0, wqAttempts(t, s, childID),
		"F-P94-01 FIXED: benign park→reclaim cycles do NOT accumulate attempts (a park resets the budget)")

	// END-TO-END: feed the EXACT production disposition a transient infra fault (bare ErrBusy, retryable,
	// not poison). With the budget PRESERVED (parks didn't spend it), the child is REQUEUED, not dead-
	// lettered — a legitimately-resumable long-parked child survives a coincident transient fault.
	require.Equal(t, wqClaimed, wqState(t, s, childID), "the parked child is claimed under the worker's live token")
	dispErr := disposeExecErr(s, childID, maxAttempts, ErrBusy)
	require.ErrorIs(t, dispErr, ErrBusy)
	require.Equal(t, wqPending, wqState(t, s, childID),
		"F-P94-01 FIXED: a post-many-parks transient fault REQUEUES the child (budget preserved), never dead-letters")

	// CONTROL (differential): a FRESH child (attempts=1) hit by the SAME ErrBusy is ALSO requeued — both
	// paths now behave identically (parks no longer differentiate the budget), confirming the fix. Give it
	// its OWN type so ClaimNext targets it unambiguously (the parked child above is now pending too).
	require.NoError(t, reg.Register("freshType", func() (*DAG, error) { return approvalChildDAG(nil, "y") }))
	_, err = s.Enqueue("fresh-child", "freshType", nil)
	require.NoError(t, err)
	fresh, err := s.ClaimNext("worker2", "freshType")
	require.NoError(t, err)
	require.Equal(t, "fresh-child", fresh.WorkflowID)
	dispErr = disposeExecErr(s, "fresh-child", maxAttempts, ErrBusy)
	require.ErrorIs(t, dispErr, ErrBusy)
	require.Equal(t, wqPending, wqState(t, s, "fresh-child"),
		"the fresh child is also requeued — parity with the parked child confirms parks no longer erode the budget")
}

// TestQueueAdversarial_TwoWorkersReclaimParkedChild_ExactlyOneResumes — 2c: the M16 fencing must still
// hold on a parked-then-reclaimed child. Two workers race to reclaim the SAME lapsed-parked child; exactly
// ONE claims + resumes it (the atomic BEGIN IMMEDIATE ClaimNext serializes the contenders). Race-safe.
func TestQueueAdversarial_TwoWorkersReclaimParkedChild_ExactlyOneResumes(t *testing.T) {
	const ttl = 40 * time.Millisecond
	// Real clock here (concurrent goroutines + a FakeClock would need shared advance coordination); a
	// short real TTL + a sleep past it is deterministic enough for the "exactly one" invariant.
	dbPath := filepath.Join(t.TempDir(), "race.db")
	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(ttl))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup

	var produceN atomic.Int32
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		return approvalChildDAG(&produceN, "raced")
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended)
	childID := subWorkflowChildID("parent", "sub")

	// Drive #1: the child parks on its approval. Deliver the approval NOW so a reclaim resumes it fully.
	ran, err := RunNext(context.Background(), s, reg, "worker-0")
	require.NoError(t, err)
	require.True(t, ran)
	require.NoError(t, s.DeliverSignal(childID, ApproveSignal("gate", "alice", "ok", "appr")))

	// Let the parked child's lease lapse, then TWO workers race a single reclaim each.
	time.Sleep(ttl + 20*time.Millisecond)
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
	)
	for i := range 2 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, e := RunNext(context.Background(), s, reg, fmt.Sprintf("racer-%d", n))
			require.NoError(t, e)
			if r {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()

	require.EqualValues(t, 1, winners.Load(), "exactly ONE worker reclaimed the lapsed-parked child (M16 fencing holds)")
	require.Equal(t, wqDone, wqState(t, s, childID), "the winner resumed the child to done")
	require.EqualValues(t, 1, produceN.Load(), "the child resumed exactly once — no double-drive under the reclaim race")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// PRIMARY TARGET 3 — completion-hook idempotency across reclaim / re-run.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestQueueAdversarial_CompletionHookIdempotent_NoDoubleWake — a re-run RunNext on an already-done child
// and a re-delivered completion (deterministic sig.ID) must leave the parent mailbox with EXACTLY ONE
// signal, one wake, no double-complete of the parent node.
func TestQueueAdversarial_CompletionHookIdempotent_NoDoubleWake(t *testing.T) {
	s := mkQueueStore(t)
	var afterN atomic.Int32
	reg := NewRegistry()
	require.NoError(t, reg.Register("childType", func() (*DAG, error) {
		return childProducing(t, "v", nil), nil
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "childType").WithResult("result", "result")
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(&afterN))
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended)
	childID := subWorkflowChildID("parent", "sub")

	// Run the child to done → the completion hook delivers ONE signal.
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Len(t, mustTake(t, s, "parent"), 1, "one completion after the child terminalizes")

	// A re-run RunNext claims NOTHING (the done row is terminal, never re-claimed) → no second delivery.
	ran2, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.False(t, ran2, "an already-done child is not re-claimed → no re-run, no second signal")

	// A re-delivered completion with the SAME deterministic sig.ID collapses to one mailbox row.
	dupID := "subworkflow-complete:" + childID // the exact deterministic id the hook uses
	require.NoError(t, s.DeliverSignal("parent", Signal{ID: dupID, Name: completionSignalName("sub")}))
	require.Len(t, mustTake(t, s, "parent"), 1, "a re-delivered completion (same sig.ID) is idempotent — still ONE row")

	// The parent wakes exactly once; "after" runs exactly once (no double-complete of the parent node).
	require.NoError(t, pw.Execute(context.Background()))
	final, err := s.Load("parent")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load(), "the downstream node ran exactly once — no double-wake")

	// A further re-drive of the terminal parent is a no-op (idempotent completion).
	require.NoError(t, pw.Execute(context.Background()))
	require.EqualValues(t, 1, afterN.Load(), "re-driving a terminal parent never re-runs downstream")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// PRIMARY TARGET 4 — EnqueueSubWorkflow / the columns + fencing orthogonality.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestQueueAdversarial_UnregisteredType_LoudAtProducer — a type-ref child whose type is unregistered on
// the producer's Registry → the producer fails LOUD (ErrValidation) at Execute, NOT a silent park that
// enqueues a pending-forever row. (The pending-forever risk is the residual when the type is registered
// on the producer but on NO worker — documented in ADVERSARIAL.md as F-P94-ADV-02.)
func TestQueueAdversarial_UnregisteredType_LoudAtProducer(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry() // EMPTY — "nope" is not registered

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "nope")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg

	err = pw.Execute(context.Background())
	require.ErrorIs(t, err, ErrValidation, "an unregistered child type fails LOUD at the producer, not a forever-park")
	require.NotErrorIs(t, err, ErrSuspended, "the producer must NOT park on an unresolvable type")

	// Nothing was enqueued — no pending row the parent would park on forever.
	pending, err := s.ListPending(0)
	require.NoError(t, err)
	require.Empty(t, pending, "a loud producer failure enqueues NO child row")
}

// TestQueueAdversarial_StaleFencedWorker_CompletionDoesNotClobberReclaimer — 4 (columns orthogonal to the
// M16 fencing CAS). A stale worker A (superseded by a reclaimer B) must not, via its best-effort completion
// path, (1) clobber B's queue row, nor (2) double-wake the parent. Two store handles = two processes.
func TestQueueAdversarial_StaleFencedWorker_CompletionDoesNotClobberReclaimer(t *testing.T) {
	const ttl = 5 * time.Second
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "fence.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
		return s
	}
	storeA, storeB := open(), open()

	const parent = "P"
	childID := subWorkflowChildID(parent, "sub")
	_, err := storeA.EnqueueSubWorkflow(childID, "T", nil, parent, completionSignalName("sub"), 1)
	require.NoError(t, err)

	// A claims (token 1).
	itemA, err := storeA.ClaimNext("procA", "T")
	require.NoError(t, err)
	require.Equal(t, childID, itemA.WorkflowID)
	require.Equal(t, parent, itemA.ParentID)

	// A's lease lapses; B reclaims (token 2 > 1) → B owns the row.
	clk.Advance(ttl + time.Second)
	itemB, err := storeB.ClaimNext("procB", "T")
	require.NoError(t, err)
	require.Equal(t, childID, itemB.WorkflowID)
	require.Greater(t, int64(itemB.Token), int64(itemA.Token), "the reclaim bumped the fencing token")

	// A (zombie) finishes late: its terminal flip is FENCED (held token 1 < current 2) → a 0-row no-op.
	// A cannot clobber B's live `claimed` row.
	flipped, err := storeA.MarkDone(childID)
	require.NoError(t, err)
	require.False(t, flipped, "the stale worker's terminal flip is fenced — cannot clobber the reclaimer's row")
	require.Equal(t, wqClaimed, wqState(t, storeA, childID), "the row is still B's (claimed), not A-terminalized")

	// A's best-effort completion delivery runs anyway — but the row is not terminal → NO signal.
	deliverSubWorkflowCompletion(storeA, itemA)
	require.Empty(t, mustTake(t, storeA, parent), "a fenced worker delivers NO completion while the row is non-terminal")

	// B completes legitimately (token 2) → exactly one completion.
	flipped, err = storeB.MarkDone(childID)
	require.NoError(t, err)
	require.True(t, flipped, "the reclaimer terminalizes under its live token")
	deliverSubWorkflowCompletion(storeB, itemB)

	// Even if A's stale hook fires AGAIN now (after B's done), the deterministic sig.ID makes it idempotent.
	deliverSubWorkflowCompletion(storeA, itemA)
	require.Len(t, mustTake(t, storeB, parent), 1,
		"exactly ONE completion in the parent mailbox — the stale worker never double-wakes (deterministic sig.ID)")
}

// TestQueueAdversarial_ReclaimCancelledChild_FailsFastNotDeadlock — FINDING F-P94-02. A sub-workflow queue child
// is cancel-set while claimed, then its owner "crashes" (lease lapses). A reclaimer's ClaimNext terminalizes
// it `cancelled` INSIDE the txn (BLOCKER-2 limbo-close) — the row NEVER reaches RunNext, so RunNext's
// completion hook can never fire for it. WITHOUT the in-txn wake, the parent parks FOREVER on a child that
// is durably terminal (cancelled). This asserts the parent's mailbox receives the completion signal from the
// ClaimNext-internal terminalize, and the parent then un-parks with a cancelled verdict.
// SEED-BREAK: drop the in-txn `INSERT INTO signals` in the cancel-terminalize branch → the parent mailbox
// stays EMPTY → this reddens (proves the wake is the load-bearing fix, not the pre-existing hook).
func TestQueueAdversarial_ReclaimCancelledChild_FailsFastNotDeadlock(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s, _ := mkClockedQueueStore(t, clk, 50*time.Millisecond)
	reg := NewRegistry()
	require.NoError(t, reg.Register("approvalChild", func() (*DAG, error) {
		return approvalChildDAG(nil, "x")
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("parent")
	pb.AddSubWorkflowQueued("sub", "approvalChild").WithResult("result", "result")
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := NewWorkflow(s)
	pw.WorkflowID = "parent"
	pw.DAG = pdag
	pw.Registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues its child + parks")
	childID := subWorkflowChildID("parent", "sub")

	// A worker claims + runs the child; it parks on its approval gate (claimed, not terminal).
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, wqClaimed, wqState(t, s, childID), "the child is parked (claimed)")

	// Operator cancels the child; the worker "crashes" (never terminalizes) and its lease lapses.
	requested, err := s.CancelRunning(childID)
	require.NoError(t, err)
	require.True(t, requested)
	clk.Advance(60 * time.Millisecond) // the parked child's lease lapses → reclaim-eligible

	// A reclaimer's ClaimNext finds the lapsed cancel-set child → terminalizes it `cancelled` IN the txn
	// and (F-P94-02) delivers the completion signal to the parent — then no runnable work remains.
	_, err = s.ClaimNext("reclaimer", "approvalChild")
	require.ErrorIs(t, err, ErrNoWork, "the cancelled child was cleaned up in-txn, never offered")
	require.Equal(t, wqCancelled, wqState(t, s, childID), "the child is terminalized cancelled")

	// F-P94-02 (necessary prerequisite): the parent's mailbox received the completion signal from the
	// ClaimNext-internal terminalize — WITHOUT it the parent is never even re-driven. Seed-break: drop the
	// in-txn `INSERT INTO signals` in the cancel-terminalize branch → this reddens (mailbox empty).
	sigs := mustTake(t, s, "parent")
	require.Len(t, sigs, 1, "F-P94-02: the ClaimNext-internal cancel-terminalize WOKE the parent (else it never re-drives)")
	assert.Equal(t, completionSignalName("sub"), sigs[0].Name)

	// F-P94-05 CLOSED (DEC-P94-QUEUE-TERMINAL-AUTHORITY): a queue-layer cancel terminalizes the work_queue
	// row (`cancelled`) but NOT the child's DATA journal. The parked-await gate now consults the QUEUE ROW
	// as the terminal authority for a queue-dispatched child, so the woken parent sees `wqState=cancelled`
	// → terminal → renders parent-node FAIL (INV-01), NOT another park. This is the sufficient completion of
	// the F-P94-02 wake. SEED-BREAK: revert the queue-state check in parkedSubWorkflowAction.Execute → the
	// journal-only gate re-parks forever → this reddens (perr becomes ErrSuspended, not a fail).
	perr := pw.Execute(context.Background())
	require.Error(t, perr, "F-P94-05: the parent fails-fast on the cancelled queue child (queue-state authority), never a deadlock")
	require.NotErrorIs(t, perr, ErrSuspended, "the parent does NOT re-park — the queue row is terminal (cancelled)")
	assert.Contains(t, perr.Error(), "cancelled", "the parent-node failure names the queue-terminal cancelled disposition")
}

// wqAttempts reads a row's attempts counter (adversarial test helper).
func wqAttempts(t *testing.T, s *SQLiteStore, wf string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT attempts FROM work_queue WHERE workflow_id=?`, wf).Scan(&n))
	return n
}
