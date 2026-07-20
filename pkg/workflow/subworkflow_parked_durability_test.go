package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParkedSubWorkflow_CrashResumeMidAwait — durable park (dag.go:569): the parent parks
// Waiting; a crash (drop the in-memory Workflow, reopen the store) leaves it persisted Waiting;
// re-driving re-checks — still parked while the child is non-terminal, then wakes once the child
// completes + a signal drives it. Exercised on FB (survives a real store reopen).
func TestParkedSubWorkflow_CrashResumeMidAwait(t *testing.T) {
	dir := t.TempDir()
	open := func() WorkflowStore {
		s, err := NewFlatBuffersStore(dir)
		require.NoError(t, err)
		return s
	}
	childDAG := childProducing(t, "durable-result", nil)

	// Drive 1: park, then "crash" (discard the Workflow object; the store persists Waiting).
	store1 := open()
	var afterN1 atomic.Int32
	w1 := parkedParent(t, store1, "wf-crash", childDAG, &afterN1)
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)
	parked, err := store1.Load("wf-crash")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sub", Waiting)

	// Reopen the store (fresh instance over the same dir) + a fresh Workflow — the resume path.
	store2 := open()
	var afterN2 atomic.Int32
	w2 := parkedParent(t, store2, "wf-crash", childDAG, &afterN2)

	// Re-drive with the child STILL non-terminal → re-parks (the persisted Waiting survived).
	require.ErrorIs(t, w2.Execute(context.Background()), ErrSuspended, "resume re-checks → still parked")

	// Complete the child out-of-band on the reopened store, deliver the signal + resume → wakes.
	runChildOutOfBand(t, store2, "wf-crash", "sub", childProducing(t, "durable-result", nil))
	require.NoError(t, w2.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")))
	final, err := store2.Load("wf-crash")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "durable-result", result)
}

// TestParkedSubWorkflow_IdempotentReCheck — a wake (re-drive) while the child is STILL
// non-terminal re-parks, NOT a spurious complete (mirrors WaitForCondition re-evaluation).
func TestParkedSubWorkflow_IdempotentReCheck(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-recheck", childProducing(t, "v", nil), &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	// Deliver a (premature) completion signal while the child has NOT run → re-park, not complete.
	require.ErrorIs(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "early")), ErrSuspended,
		"a wake while the child is non-terminal → re-park, not a spurious complete")
	require.EqualValues(t, 0, afterN.Load())
}

// TestParkedSubWorkflow_ResultInt64Fidelity — an int64 child result survives value_long on the
// wake read across InMemory + FB (carries ph91's scalar fidelity through the parked path).
func TestParkedSubWorkflow_ResultInt64Fidelity(t *testing.T) {
	const big = int64(9223372036854775807)
	for _, sc := range []struct {
		name string
		mk   func() WorkflowStore
	}{
		// SQLite is EXCLUDED by design: SQLiteStore has no SignalStore yet (the SQLite mailbox
		// is ph93), and the parked path requires a SignalStore. ph93 adds it + extends this axis.
		{"InMemory", func() WorkflowStore { return NewInMemoryStore() }},
		{"FlatBuffers", func() WorkflowStore { s, e := NewFlatBuffersStore(t.TempDir()); require.NoError(t, e); return s }},
	} {
		t.Run(sc.name, func(t *testing.T) {
			store := sc.mk()
			var afterN atomic.Int32
			w := parkedParent(t, store, "wf-fid", childProducing(t, big, nil), &afterN)
			require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
			runChildOutOfBand(t, store, "wf-fid", "sub", childProducing(t, big, nil))
			require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")))

			final, err := store.Load("wf-fid")
			require.NoError(t, err)
			got, ok := final.GetInt64("result")
			require.True(t, ok, "%s: the int64 child result survives value_long on the parked wake read", sc.name)
			assert.Equal(t, big, got)
		})
	}
}

// TestParkedSubWorkflow_ResultKeyCollision — a declared result key shadowing a pre-existing
// parent key (foreign value) is loud on the wake path (ph91's DeepEqual check, reused).
func TestParkedSubWorkflow_ResultKeyCollision(t *testing.T) {
	store := NewInMemoryStore()
	pb := NewWorkflowBuilder().WithWorkflowID("wf-pcollide")
	pb.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", "foreign")
		return nil
	}))
	pb.AddSubWorkflowParked("sub", childProducing(t, "child", nil)).DependsOn("before").WithResult("result", "result")
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = "wf-pcollide"
	w.DAG = dag

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	runChildOutOfBand(t, store, "wf-pcollide", "sub", childProducing(t, "child", nil))
	err = w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1"))
	require.ErrorIs(t, err, ErrSubWorkflowResultKeyCollision, "a result-key collision is loud on the wake path")
}

// TestParkedSubWorkflow_ChildFailed_ParentFails — INV-01: a child that terminalized with a
// non-coe Failed node → the parent node fails on wake; downstream never runs.
func TestParkedSubWorkflow_ChildFailed_ParentFails(t *testing.T) {
	store := NewInMemoryStore()
	// A child whose single node fails (non-coe).
	cb := NewWorkflowBuilder()
	cb.AddStartNode("boom").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("child boom")
	}))
	childDAG, err := cb.Build()
	require.NoError(t, err)

	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-pfail", childDAG, &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Run the child out-of-band (it fails), then wake.
	childID := subWorkflowChildID("wf-pfail", "sub")
	child := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	require.Error(t, child.Execute(context.Background())) // the child fails
	err = w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1"))
	require.Error(t, err, "a failed child fails the parent node on wake (INV-01)")

	final, lerr := store.Load("wf-pfail")
	require.NoError(t, lerr)
	assertNodeStatus(t, final, "sub", Failed)
	require.EqualValues(t, 0, afterN.Load(), "downstream never runs after a child-fail")
}

// coeBearingChild builds a child with a ContinueOnError node that fails + a downstream node that
// sets the result. When toleratedOnly=true the ONLY failure is the coe node (child SUCCEEDS); when
// false it adds a NON-coe failing node (child FAILS). Used by the verdict-parity test.
func coeBearingChild(t *testing.T, toleratedOnly bool) *DAG {
	t.Helper()
	cb := NewWorkflowBuilder()
	cb.AddStartNode("coe-fail").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		return errors.New("tolerated")
	})).WithContinueOnError()
	cb.AddNode("produce").DependsOn("coe-fail").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", "ok")
		return nil
	}))
	if !toleratedOnly {
		cb.AddNode("hard-fail").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			return errors.New("fail-fast")
		})) // a NON-coe failing node → the run fails
	}
	dag, err := cb.Build()
	require.NoError(t, err)
	return dag
}

// TestSubWorkflow_InlineParkedVerdictParity — DEC-P92-COE-VERDICT-FROM-DAG condition: the INLINE
// path (ph91, defers to child.Execute) and the PARKED path (ph92, childRunFailed over journal+DAG)
// MUST render the IDENTICAL coe-aware verdict for the same child — else the two shared-rule
// consumers have drifted. Proven for BOTH a tolerated-coe child (both succeed) AND a
// fail-fast-non-coe child (both fail). This is the bite that guards against two coe implementations.
func TestSubWorkflow_InlineParkedVerdictParity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		toleratedOnly bool
		wantParentOK  bool // true = parent completes; false = parent fails (INV-01)
	}{
		{"tolerated_coe_child_both_succeed", true, true},
		{"fail_fast_non_coe_child_both_fail", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// INLINE (ph91): AddSubWorkflow blocks on child.Execute.
			inlineStore := NewInMemoryStore()
			var inlineAfter atomic.Int32
			ib := NewWorkflowBuilder().WithWorkflowID("wf-inline")
			ib.AddSubWorkflow("sub", coeBearingChild(t, tc.toleratedOnly)).WithResult("result", "result")
			ib.AddNode("after").DependsOn("sub").WithAction(countingAction(&inlineAfter))
			idag, err := ib.Build()
			require.NoError(t, err)
			iw := NewWorkflow(inlineStore)
			iw.WorkflowID = "wf-inline"
			iw.DAG = idag
			inlineErr := iw.Execute(context.Background())

			// PARKED (ph92): run the child out-of-band, then wake.
			parkedStore := NewInMemoryStore()
			var parkedAfter atomic.Int32
			pw := parkedParent(t, parkedStore, "wf-parked", coeBearingChild(t, tc.toleratedOnly), &parkedAfter)
			require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended)
			childID := subWorkflowChildID("wf-parked", "sub")
			child := &Workflow{DAG: coeBearingChild(t, tc.toleratedOnly), WorkflowID: childID, Store: parkedStore}
			_ = child.Execute(context.Background()) //nolint:errcheck // the child's own verdict is under test via the parent, not here
			parkedErr := pw.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1"))

			// PARITY: both paths render the same parent verdict.
			if tc.wantParentOK {
				require.NoError(t, inlineErr, "inline: tolerated-coe child → parent completes")
				require.NoError(t, parkedErr, "parked: tolerated-coe child → parent completes")
				require.EqualValues(t, 1, inlineAfter.Load())
				require.EqualValues(t, 1, parkedAfter.Load(), "PARITY: parked downstream ran iff inline did")
			} else {
				require.Error(t, inlineErr, "inline: fail-fast-non-coe child → parent fails")
				require.Error(t, parkedErr, "parked: fail-fast-non-coe child → parent fails")
				require.EqualValues(t, 0, inlineAfter.Load())
				require.EqualValues(t, 0, parkedAfter.Load(), "PARITY: parked downstream skipped iff inline was")
			}
		})
	}
}

// TestChildRunFailed_RollbackNoFailedNode — F-P92-01 regression: a child journal with ONLY
// rollback statuses (Compensated / CompensationFailed) and NO Failed node MUST verdict as a run
// FAILURE (a rollback implies the run failed) — else the parked path renders a false SUCCESS,
// diverging from the inline path (which returns the *SagaError). Directly exercises childRunFailed
// over a synthetic rollback journal (the narrow shape a cancel/deadline rollback can leave).
func TestChildRunFailed_RollbackNoFailedNode(t *testing.T) {
	dag := NewDAG("saga-child")
	require.NoError(t, dag.AddNode(NewNode("a", choiceNoop())))
	require.NoError(t, dag.AddNode(NewNode("b", choiceNoop())))

	// Compensated-only (a clean rollback) → run failed.
	cd := NewWorkflowData("saga-child")
	cd.SetNodeStatus("a", Compensated)
	cd.SetNodeStatus("b", Completed)
	failed, first := childRunFailed(dag, cd)
	require.True(t, failed, "a Compensated node (rollback) → run failed, even with NO Failed node")
	assert.Equal(t, "a", first)

	// CompensationFailed → run failed (an effect was NOT undone — never a silent success).
	cd2 := NewWorkflowData("saga-child")
	cd2.SetNodeStatus("a", CompensationFailed)
	cd2.SetNodeStatus("b", Completed)
	failed2, _ := childRunFailed(dag, cd2)
	require.True(t, failed2, "a CompensationFailed node → run failed")

	// All-Completed → NOT failed (the happy baseline, unchanged).
	cd3 := NewWorkflowData("saga-child")
	cd3.SetNodeStatus("a", Completed)
	cd3.SetNodeStatus("b", Completed)
	failed3, _ := childRunFailed(dag, cd3)
	require.False(t, failed3, "all-Completed → not failed")
}

// TestChildRunFailed_TerminalStatusCompleteness — the self-defending parity guard (DEC-P92-COE-
// VERDICT-FROM-DAG). childRunFailed is a RE-DERIVATION of the executor's coe rule (not a shared
// extraction — parallel_execution.go is byte-unchanged), so drift is guarded by tests. This test
// closes the remaining hole: a NEW terminal status added to isTerminalStatus without a matching
// childRunFailed verdict would SILENTLY reintroduce inline/parked divergence. It enumerates EVERY
// NodeStatus the enum defines and:
//
//	(1) asserts the test's own terminal-classification matches isTerminalStatus EXACTLY — so a
//	    new terminal status not added to this table fails here (the completeness lock); and
//	(2) for each TERMINAL status, asserts childRunFailed's verdict for a single-node child of that
//	    status matches the intended run-verdict contract.
//
// Bite: add a fake terminal status to isTerminalStatus (or drop a row) → this test reddens.
func TestChildRunFailed_TerminalStatusCompleteness(t *testing.T) {
	// The COMPLETE NodeStatus enum (node.go) with, per status: whether it is terminal, and — for
	// terminal statuses — whether a single non-coe node of that status makes the RUN fail.
	// Keep this table in lockstep with node.go's NodeStatus constants + isTerminalStatus.
	table := []struct {
		status     NodeStatus
		isTerminal bool
		runFails   bool // only meaningful when isTerminal
	}{
		{Pending, false, false},
		{Running, false, false},
		{Waiting, false, false},
		{Completed, true, false},         // clean success
		{Skipped, true, false},           // a skip is not itself a failure (its cause is a Failed node)
		{Bypassed, true, false},          // an M11 not-taken branch — not a failure
		{Failed, true, true},             // a non-coe failure fails the run
		{Compensated, true, true},        // a rollback occurred → the run failed
		{CompensationFailed, true, true}, // an effect was not undone → the run failed
	}

	// (1) Completeness lock: the table's terminal set EXACTLY equals isTerminalStatus's.
	// If a new terminal status is added to isTerminalStatus but not this table, the two disagree.
	for _, row := range table {
		require.Equalf(t, row.isTerminal, isTerminalStatus(row.status),
			"status %q: table.isTerminal must match isTerminalStatus — a new terminal status needs a row here", row.status)
	}

	// (2) Per-terminal-status verdict: a single non-coe node of the status → childRunFailed verdict.
	for _, row := range table {
		if !row.isTerminal {
			continue
		}
		dag := NewDAG("one")
		require.NoError(t, dag.AddNode(NewNode("n", choiceNoop()))) // non-coe node "n"
		cd := NewWorkflowData("one")
		cd.SetNodeStatus("n", row.status)
		failed, _ := childRunFailed(dag, cd)
		require.Equalf(t, row.runFails, failed,
			"status %q: childRunFailed verdict must match the run-verdict contract (a new terminal status must decide its verdict here)", row.status)
	}
}

// TestParkedSubWorkflow_ManualChildOnSQLite_JournalGateUnchanged — the "ph92 sealed behavior is
// byte-unchanged" witness for DEC-P94-QUEUE-TERMINAL-AUTHORITY. A MANUAL-signal parked child (run
// out-of-band via runChildOutOfBand — NEVER enqueued via EnqueueSubWorkflow) has NO work_queue row,
// so on a SQLite store queueTerminalState returns exists=false and the parked-await gate falls through
// to the JOURNAL-gate exactly as ph92 sealed it. Runs the full ph92 park-then-wake spine on SQLite
// (now that ph93 gives SQLite a SignalStore) and asserts the manual path is unaffected by the queue-
// authority arm. SEED-BREAK equivalence: this is the counterpart to the queue-authority bite — it
// proves the OR-clause is false (not merely inert) for a non-queue child, so ph92 stays byte-identical.
func TestParkedSubWorkflow_ManualChildOnSQLite_JournalGateUnchanged(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "manual.db"), WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // cleanup

	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-manual", childProducing(t, "manual-result", nil), &afterN)

	// Parent parks (child not spawned). No work_queue row exists for the child ID.
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	childID := subWorkflowChildID("wf-manual", "sub")
	_, exists, qerr := store.queueTerminalState(childID)
	require.NoError(t, qerr)
	require.False(t, exists, "a MANUAL child has NO work_queue row → the queue-authority arm is skipped, journal-gate applies")

	// A premature wake while the child is non-terminal → re-park (journal-gate, unchanged).
	require.ErrorIs(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "early")), ErrSuspended,
		"journal-gate: a wake before the child journal is terminal re-parks (ph92 behavior byte-unchanged)")

	// Run the child out-of-band (manual producer) → its JOURNAL is terminal; still no queue row.
	runChildOutOfBand(t, store, "wf-manual", "sub", childProducing(t, "manual-result", nil))
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1")),
		"journal-gate: a terminal child journal + wake completes the parent (ph92 spine, on SQLite via ph93 SignalStore)")

	final, err := store.Load("wf-manual")
	require.NoError(t, err)
	got, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "manual-result", got, "the manual child's result populated the parent key via the journal-gate path")
	require.EqualValues(t, 1, afterN.Load(), "downstream ran once after the journal-gated wake")
}
