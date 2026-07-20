package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// M19 ph92 — WAKE parked-await ADVERSARIAL suite.
//
// Independent of the author's subworkflow_parked_test.go /
// subworkflow_parked_durability_test.go. This file attacks the frontiers those
// fixtures do not reach:
//
//   1. VERDICT PARITY across coe/terminal shapes the author's 2-case parity
//      test excludes — mix, fail-fast cascade, M11 Bypassed, M12 saga, multi-fail
//      (DEC-P92-COE-VERDICT-FROM-DAG: parked childRunFailed MUST equal the inline
//      child.Execute verdict for the SAME child).
//   2. Wake / durable-park under adversarial signal interleavings.
//   3. childTerminal / childRunFailed on non-standard child journals.
//   4. Ack / mailbox hygiene — especially the FAILURE path.
//
// Findings are recorded in
// .planning/phases/92-wake-parked-await/ADVERSARIAL.md. Tests that pin a
// CONFIRMED defect are characterization tests (green, so the suite is durable)
// with a `DEFECT F-xx` banner; they flip RED when the defect is fixed.
// ============================================================================

// mustBuildDAG builds a workflow whose construction is known-good (a test child factory) and
// panics on an unexpected build error (keeps the factories no-arg while satisfying errcheck).
func mustBuildDAG(b *WorkflowBuilder) *DAG {
	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	return dag
}

// boomAction returns a non-nil error (a hard, non-coe node failure).
func boomAction() Action {
	return ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })
}

// setResultAction sets the child "result" data key (so a parent WithResult finds it).
func setResultAction(v any) Action {
	return ActionFunc(func(_ context.Context, d *WorkflowData) error { d.Set("result", v); return nil })
}

// oobRun runs childDAG out-of-band under the parent's deterministic child ID on the
// same store (the ph94 producer, simulated) and RETURNS the child's own verdict
// (nil or error) — unlike the author's runChildOutOfBand, which require.NoError's it,
// so it cannot model a child that terminalizes FAILED / rolled-back.
func oobRun(t *testing.T, store WorkflowStore, parentWF, nodeName string, childDAG *DAG) error {
	t.Helper()
	childID := subWorkflowChildID(parentWF, nodeName)
	child := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	return child.Execute(context.Background())
}

// mailbox returns the current (non-destructive) mailbox contents for workflowID.
func mailbox(t *testing.T, store WorkflowStore, workflowID string) []Signal {
	t.Helper()
	ss, ok := store.(SignalStore)
	require.True(t, ok, "store is not a SignalStore")
	sigs, err := ss.TakeSignals(workflowID)
	require.NoError(t, err)
	return sigs
}

// ----------------------------------------------------------------------------
// Adversarial child SHAPES (each is a fresh-DAG factory; identical structure per
// call, so the inline copy, the parked-parent copy, and the out-of-band copy all
// agree on node names + ContinueOnError flags — the shared-rule precondition).
// ----------------------------------------------------------------------------

// mixChild: ONE coe-Failed node AND ONE non-coe-Failed node → the run FAILS
// (the coe failure is tolerated; the non-coe one is fail-fast).
func mixChild() *DAG {
	cb := NewWorkflowBuilder()
	cb.AddStartNode("acoe").WithAction(boomAction()).WithContinueOnError()
	cb.AddNode("bhard").WithAction(boomAction()) // independent root, non-coe → run fails
	return mustBuildDAG(cb)
}

// cascadeChild: a non-coe fail whose downstream is Skipped by the fail-fast sweep.
func cascadeChild() *DAG {
	cb := NewWorkflowBuilder()
	cb.AddStartNode("root").WithAction(boomAction()) // non-coe → fail-fast
	cb.AddNode("down").DependsOn("root").WithAction(choiceNoop())
	return mustBuildDAG(cb)
}

// bypassedChild: an M11 ChoiceNode routes to "taken" (which sets the result);
// "nottaken" + its interior are Bypassed. The run SUCCEEDS.
func bypassedChild() *DAG {
	truePred := func(*WorkflowData) bool { return true }
	cb := NewWorkflowBuilder()
	cb.AddStartNode("seed").WithAction(choiceNoop())
	cb.AddChoice("route").DependsOn("seed").
		When(truePred, "taken").
		Otherwise("nottaken")
	cb.AddNode("taken").WithAction(setResultAction("ok"))
	cb.AddNode("nottaken").WithAction(choiceNoop())
	cb.AddNode("nottakenChild").DependsOn("nottaken").WithAction(choiceNoop())
	return mustBuildDAG(cb)
}

// sagaTriggerChild: a->fail saga. "a" Completes (compensable), "fail" hard-fails →
// rollback → a Compensated, fail stays Failed. child.Execute returns non-nil.
func sagaTriggerChild() *DAG {
	cb := NewWorkflowBuilder()
	cb.AddNode("a").WithAction(choiceNoop()).WithCompensation(okCompFn)
	cb.AddNode("fail").DependsOn("a").WithAction(boomAction())
	return mustBuildDAG(cb)
}

// multiFailChild: TWO non-coe fails in the SAME level. childRunFailed must pick the
// deterministic lowest-name offender; the verdict (fail) must match the executor.
func multiFailChild() *DAG {
	cb := NewWorkflowBuilder()
	cb.AddStartNode("zeta").WithAction(boomAction())
	cb.AddNode("alpha").WithAction(boomAction())
	return mustBuildDAG(cb)
}

// ----------------------------------------------------------------------------
// TARGET 1 — VERDICT PARITY across the shapes the author's 2-case test excludes.
//
// runParity runs childFactory BOTH inline (AddSubWorkflow, blocks on child.Execute)
// AND parked (AddSubWorkflowParked + out-of-band run + wake), returning both parent
// verdicts + downstream-ran counts. A divergence is a HIGH defect: the two consumers
// of the shared coe-rule have drifted.
// ----------------------------------------------------------------------------

type parityOutcome struct {
	inlineErr, parkedErr       error
	inlineAfter, parkedAfter   int32
	inlineChildErr, parkedWake error
}

func runParity(t *testing.T, name string, childFactory func() *DAG, declareResult bool) parityOutcome {
	t.Helper()
	var out parityOutcome

	// INLINE (ph91): the parent blocks on child.Execute; its returned error IS the verdict.
	inlineStore := NewInMemoryStore()
	var inlineAfter atomic.Int32
	ib := NewWorkflowBuilder().WithWorkflowID("wf-inline-" + name)
	isub := ib.AddSubWorkflow("sub", childFactory())
	if declareResult {
		isub.WithResult("result", "result")
	}
	ib.AddNode("after").DependsOn("sub").WithAction(countingAction(&inlineAfter))
	idag, err := ib.Build()
	require.NoError(t, err, "inline parent build")
	iw := NewWorkflow(inlineStore)
	iw.WorkflowID = "wf-inline-" + name
	iw.DAG = idag
	out.inlineErr = iw.Execute(context.Background())
	out.inlineAfter = inlineAfter.Load()

	// PARKED (ph92): park, run the child out-of-band, then wake via the completion signal.
	parkedStore := NewInMemoryStore()
	var parkedAfter atomic.Int32
	pb := NewWorkflowBuilder().WithWorkflowID("wf-parked-" + name)
	psub := pb.AddSubWorkflowParked("sub", childFactory())
	if declareResult {
		psub.WithResult("result", "result")
	}
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(&parkedAfter))
	pdag, err := pb.Build()
	require.NoError(t, err, "parked parent build")
	pw := NewWorkflow(parkedStore)
	pw.WorkflowID = "wf-parked-" + name
	pw.DAG = pdag
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "first drive parks")
	out.inlineChildErr = oobRun(t, parkedStore, "wf-parked-"+name, "sub", childFactory())
	out.parkedErr = pw.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "c1"))
	out.parkedAfter = parkedAfter.Load()
	return out
}

func TestParkedAdv_VerdictParity_AcrossShapes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		factory       func() *DAG
		declareResult bool
		wantParentOK  bool // the EXPECTED shared verdict (from the executor semantics)
	}{
		// The author's baseline (clean success) — the positive control.
		{"clean_success", func() *DAG { return childProducing(t, "ok", nil) }, true, true},
		// 1(a): a MIX — one tolerated coe fail + one fail-fast non-coe fail → FAIL.
		{"mix_coe_and_noncoe", mixChild, false, false},
		// 1(b): a fail-fast failure that CASCADES (downstream Skipped) → FAIL.
		{"failfast_cascade_skipped", cascadeChild, false, false},
		// 1(c): an M11 Bypassed node (choice not-taken) → SUCCEED.
		{"m11_bypassed_choice", bypassedChild, true, true},
		// 1(d): an M12 saga child (node-triggered rollback: Compensated + Failed) → FAIL.
		{"m12_saga_node_triggered", sagaTriggerChild, false, false},
		// 1(e): a MULTI-node child where two nodes fail (deterministic first-failed) → FAIL.
		{"multi_two_noncoe_fail", multiFailChild, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := runParity(t, tc.name, tc.factory, tc.declareResult)

			inlineOK := o.inlineErr == nil
			parkedOK := o.parkedErr == nil

			// The load-bearing PARITY assertion: inline and parked render the IDENTICAL verdict.
			require.Equalf(t, inlineOK, parkedOK,
				"VERDICT DRIFT (%s): inline ok=%v (err=%v) vs parked ok=%v (err=%v) — shared coe-rule broken",
				tc.name, inlineOK, o.inlineErr, parkedOK, o.parkedErr)
			require.Equalf(t, o.inlineAfter, o.parkedAfter,
				"DOWNSTREAM DRIFT (%s): inline after=%d vs parked after=%d", tc.name, o.inlineAfter, o.parkedAfter)

			// And the shared verdict is the EXPECTED one (anti-vacuity: if both paths
			// were uniformly wrong the equality above would still pass).
			require.Equalf(t, tc.wantParentOK, parkedOK,
				"UNEXPECTED VERDICT (%s): want parentOK=%v, got %v (parkedErr=%v)",
				tc.name, tc.wantParentOK, parkedOK, o.parkedErr)
		})
	}
}

// TestParkedAdv_ChildRunFailed_FirstFailedDeterministic — 1(e) refinement: on a
// multi-fail journal childRunFailed returns the LOWEST-name offender, deterministically,
// regardless of map iteration order.
func TestParkedAdv_ChildRunFailed_FirstFailedDeterministic(t *testing.T) {
	dag := multiFailChild() // nodes: zeta, alpha (both non-coe)
	cd := NewWorkflowData("cd")
	cd.SetNodeStatus("zeta", Failed)
	cd.SetNodeStatus("alpha", Failed)
	for range 64 { // hammer to defeat any accidental map-order dependence
		failed, first := childRunFailed(dag, cd)
		require.True(t, failed)
		require.Equal(t, "alpha", first, "first-failed must be the deterministic lowest name")
	}
}

// ----------------------------------------------------------------------------
// TARGET 1(d) HARDENING — the LATENT coe-verdict gap: childRunFailed keys ONLY on
// status==Failed. It ignores the rolling_back marker AND the CompensationFailed
// status. A saga child that CLEANLY rolled back (all effects Compensated, NO Failed
// node — the cancel/deadline-triggered shape) is rendered SUCCESS by the parked path,
// while child.Execute (executeLocked) returns non-nil for the same journal.
//
// This is currently MASKED on InMemory/FB: a REAL out-of-band saga run always leaves
// either a Failed node (node-triggered → childRunFailed catches it) or a Pending node
// (cancel-between-levels → childTerminal parks). The {all-terminal, rolling_back,
// no-Failed} journal is only produced by a synthetic/seeded state — but that state IS
// a faithful representation of a completed cancel-triggered rollback (the author's own
// saga resume tests seed exactly it), so the gap is real, just not reachable through
// the ph92 InMemory/FB producer today. Recorded as FINDING F-92-03 (informational).
// ----------------------------------------------------------------------------

// F-92-03 / F-P92-01 (FIXED): childRunFailed now recognizes a saga ROLLBACK (Compensated /
// CompensationFailed) as a run FAILURE — a rollback only happens on a failed run — so a cleanly
// rolled-back child (no Failed node) verdicts as FAILURE, IDENTICAL to the inline child.Execute
// verdict. This regression guard asserts the FIXED contract; it flipped from the reproduced
// defect the day the Compensated/CompensationFailed arm was added to childRunFailed.
func TestParkedAdv_ChildRunFailed_RecognizesRollbackAsFailure(t *testing.T) {
	dag := sagaTriggerChild() // nodes a, fail

	t.Run("cleanly_rolled_back_no_failed_node", func(t *testing.T) {
		cd := NewWorkflowData("cd-rb")
		cd.SetNodeStatus("a", Compensated) // undone
		cd.SetRollingBack(true)
		cd.SetTriggerCause(TriggerCanceled)
		require.True(t, childTerminal(cd), "a Compensated node is terminal")
		failed, _ := childRunFailed(dag, cd)
		require.True(t, failed,
			"a cleanly rolled-back child (Compensated, no Failed node) → FAILURE (rollback implies failure)")
	})

	t.Run("compensation_failed_is_failure", func(t *testing.T) {
		cd := NewWorkflowData("cd-cf")
		cd.SetNodeStatus("a", CompensationFailed) // effect NOT undone — never a silent success
		cd.SetRollingBack(true)
		require.True(t, childTerminal(cd), "CompensationFailed is terminal")
		failed, _ := childRunFailed(dag, cd)
		require.True(t, failed, "a CompensationFailed node → FAILURE (an effect was not undone)")
	})

	// Parity: child.Execute on the SAME cleanly-rolled-back journal returns NON-NIL, and the
	// parked path (childRunFailed) now AGREES — both render FAILURE (the drift is closed).
	t.Run("inline_and_parked_agree", func(t *testing.T) {
		store := NewInMemoryStore()
		childID := "sub:standalone"
		seed := NewWorkflowData(childID)
		seed.SetNodeStatus("a", Compensated)
		seed.SetRollingBack(true)
		seed.SetTriggerCause(TriggerCanceled)
		require.NoError(t, store.Save(seed))
		child := &Workflow{DAG: sagaTriggerChild(), WorkflowID: childID, Store: store}
		inlineErr := child.Execute(context.Background())
		require.Error(t, inlineErr, "inline: a rolled-back child is a FAILURE")
		reloaded, _ := store.Load(childID) //nolint:errcheck // asserted below
		parkedFailed, _ := childRunFailed(sagaTriggerChild(), reloaded)
		require.True(t, parkedFailed, "parked: agrees — a rolled-back child is a FAILURE (F-P92-01 closed)")
	})
}

// ----------------------------------------------------------------------------
// TARGET 2 — WAKE under adversarial signal interleavings.
// ----------------------------------------------------------------------------

// 2(a): DUPLICATE completion signals.
func TestParkedAdv_DuplicateSignals(t *testing.T) {
	t.Run("same_id_twice_one_wake_no_leak", func(t *testing.T) {
		store := NewInMemoryStore()
		var afterN atomic.Int32
		w := parkedParent(t, store, "wf-dup-same", childProducing(t, "v", nil), &afterN)
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
		runChildOutOfBand(t, store, "wf-dup-same", "sub", childProducing(t, "v", nil))

		// Deliver the SAME sig.ID twice, then resume. The ID-keyed mailbox dedupes → one entry.
		require.NoError(t, w.DeliverSignal(SubWorkflowCompletionSignal("sub", "dup")))
		require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "dup")))

		final, err := store.Load("wf-dup-same")
		require.NoError(t, err)
		assertNodeStatus(t, final, "sub", Completed)
		require.EqualValues(t, 1, afterN.Load(), "exactly one wake / one downstream run")
		require.Empty(t, mailbox(t, store, "wf-dup-same"), "the completion signal is drained on success")
	})

	t.Run("different_ids_no_double_complete_no_leak", func(t *testing.T) {
		store := NewInMemoryStore()
		var afterN atomic.Int32
		w := parkedParent(t, store, "wf-dup-diff", childProducing(t, "v", nil), &afterN)
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
		runChildOutOfBand(t, store, "wf-dup-diff", "sub", childProducing(t, "v", nil))

		// Two DIFFERENT sig.IDs, both name-matching the completion. Both are enqueued;
		// the node still completes exactly once, and BOTH are acked (name-matched drain).
		require.NoError(t, w.DeliverSignal(SubWorkflowCompletionSignal("sub", "d1")))
		require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "d2")))

		final, err := store.Load("wf-dup-diff")
		require.NoError(t, err)
		assertNodeStatus(t, final, "sub", Completed)
		require.EqualValues(t, 1, afterN.Load(), "one wake despite two distinct completion signals")
		require.Empty(t, mailbox(t, store, "wf-dup-diff"),
			"both name-matching completion signals are drained on success")

		// A THIRD drive with the node already terminal is a no-op (no double-complete).
		require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "d3")))
		require.EqualValues(t, 1, afterN.Load(), "already-terminal node does not re-run downstream")
	})
}

// 2(b): a completion signal delivered BEFORE the child is terminal must NOT
// spuriously wake (the gate is the JOURNAL, not the signal); a later signal, after
// the child terminalizes, wakes correctly.
func TestParkedAdv_SignalBeforeTerminal_ThenComplete(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-ooo", childProducing(t, "v", nil), &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Signal #1 arrives while the child has NOT been run → the drive re-parks (no wake).
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "early")),
		ErrSuspended, "a completion signal before the child is terminal must NOT wake (journal-gated)")
	parked, err := store.Load("wf-ooo")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sub", Waiting)
	require.EqualValues(t, 0, afterN.Load())

	// The child now terminalizes; signal #2 (distinct ID) wakes correctly.
	runChildOutOfBand(t, store, "wf-ooo", "sub", childProducing(t, "v", nil))
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "real")))

	final, err := store.Load("wf-ooo")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	require.EqualValues(t, 1, afterN.Load())
	require.Empty(t, mailbox(t, store, "wf-ooo"),
		"the stale early signal AND the real signal are both drained on the eventual completion")
}

// 2(c): crash-resume across a REAL FlatBuffers store reopen — deliver the signal to
// the REOPENED store and wake there.
func TestParkedAdv_CrashResume_FBReopenDeliverToReopened(t *testing.T) {
	dir := t.TempDir()
	store1, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	var afterN atomic.Int32
	w1 := parkedParent(t, store1, "wf-fb-crash", childProducing(t, "fbv", nil), &afterN)
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)

	// Run the child out-of-band on store1, then DROP w1/store1 (simulate a crash).
	runChildOutOfBand(t, store1, "wf-fb-crash", "sub", childProducing(t, "fbv", nil))

	// Reopen the store from disk; rebuild the workflow; deliver + resume on the REOPENED store.
	store2, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	w2 := parkedParent(t, store2, "wf-fb-crash", childProducing(t, "fbv", nil), &afterN)
	require.NoError(t, w2.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "fbsig")))

	final, err := store2.Load("wf-fb-crash")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	assertNodeStatus(t, final, "after", Completed)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "fbv", result)
	require.Empty(t, mailbox(t, store2, "wf-fb-crash"), "completion signal drained after the reopened wake")
}

// 2(d): a completion signal is a PURE TRIGGER (DEC-P92-SIGNAL-IS-TRIGGER, architect-ratified):
// the wake decision is journal-gated, NOT signal-name-gated. Two consequences, both intended /
// documented (not defects):
//
//	 (i) BY DESIGN: the wake is not name-gated. A signal addressed to node "ghost" still drives a
//	     full re-check, so a parked node whose OWN child is terminal completes regardless of the
//	     delivered signal's name. This IS the ratified trigger contract — the completion GATE is
//	     the child JOURNAL, the signal only causes the host DeliverAndResume. Name-gating the wake
//	     would contradict DEC-P92-SIGNAL-IS-TRIGGER.
//	(ii) DOCUMENTED LIMITATION: a MISADDRESSED completion signal (a host bug — a name matching no
//	     parked node that completes) is never acked and stays inert in the mailbox until
//	     Store.Delete (bounded by the F37 mailbox cap; no GC). A correctly-addressed signal on a
//	     single-parked-node workflow does not leak. Acceptable minor hygiene cost of the trigger
//	     contract; a name-gated ack would re-introduce the coupling the trigger design avoids.
//
// The genuine isolation guarantee — a target whose OWN child is non-terminal stays parked — holds
// (asserted separately below).
func TestParkedAdv_WrongNodeSignal_TriggerNotNameGated(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-wrongsig", childProducing(t, "v", nil), &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	runChildOutOfBand(t, store, "wf-wrongsig", "sub", childProducing(t, "v", nil))

	// Deliver a signal named for a DIFFERENT (non-existent) node. The child of "sub"
	// IS terminal, so the re-drive completes "sub" anyway (trigger, not name gate).
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("ghost", "g1")))

	final, err := store.Load("wf-wrongsig")
	require.NoError(t, err)
	// (i) the wake is NOT name-gated — "sub" completed on a signal addressed to "ghost".
	assertNodeStatus(t, final, "sub", Completed)
	require.EqualValues(t, 1, afterN.Load())

	// (ii) the misaddressed signal has no consumer → it LEAKS (F-92-02, MEDIUM).
	leaked := mailbox(t, store, "wf-wrongsig")
	names := make([]string, 0, len(leaked))
	for _, s := range leaked {
		names = append(names, s.Name)
	}
	require.Contains(t, names, completionSignalName("ghost"),
		"a misaddressed completion signal is never acked and leaks in the mailbox (F-92-02)")
}

func TestParkedAdv_TargetWithNonTerminalChildStaysParked(t *testing.T) {
	// The genuine isolation: node "sub"'s OWN child is never run (non-terminal) → even
	// after a wake drive it stays parked. (A second parked node's readiness cannot pull
	// it out.)
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-stayparked", childProducing(t, "v", nil), &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	// Do NOT run the child out-of-band. Deliver a completion signal + resume.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "s1")),
		ErrSuspended, "a signal without a terminal child re-parks the target")
	parked, err := store.Load("wf-stayparked")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sub", Waiting)
	require.EqualValues(t, 0, afterN.Load())
}

// ----------------------------------------------------------------------------
// TARGET 3 — childTerminal / childRunFailed on non-standard child journals.
// ----------------------------------------------------------------------------

func TestParkedAdv_ChildTerminal_Edges(t *testing.T) {
	t.Run("empty_journal_not_terminal", func(t *testing.T) {
		require.False(t, childTerminal(NewWorkflowData("empty")), "no nodes → not terminal (park)")
	})

	t.Run("waiting_node_not_terminal", func(t *testing.T) {
		cd := NewWorkflowData("wait")
		cd.SetNodeStatus("done", Completed)
		cd.SetNodeStatus("parked", Waiting) // a suspendable child mid-park
		require.False(t, childTerminal(cd), "a Waiting node is non-terminal → re-park, never a false-complete")
	})

	t.Run("all_terminal_variants_are_terminal", func(t *testing.T) {
		for _, st := range []NodeStatus{Completed, Failed, Skipped, Bypassed, Compensated, CompensationFailed} {
			cd := NewWorkflowData("t")
			cd.SetNodeStatus("n", st)
			require.Truef(t, childTerminal(cd), "%s is terminal", st)
		}
	})
}

// 3(a): a suspendable (Waiting) child, driven through the actual parked wake path, must
// RE-PARK (childTerminal false), never falsely complete. AddSubWorkflowParked does NOT
// run the closure-scan (unlike AddSubWorkflow), so a suspendable child is constructible.
func TestParkedAdv_SuspendableChildReParks(t *testing.T) {
	store := NewInMemoryStore()
	// child: a WaitForSignal node that parks forever (no signal for it is ever delivered).
	childDAG := NewDAG("susp-child")
	require.NoError(t, childDAG.AddNode(NewWaitForSignalNode("childwait", "child-inner-signal")))

	var afterN atomic.Int32
	pb := NewWorkflowBuilder().WithWorkflowID("wf-suspchild")
	pb.AddSubWorkflowParked("sub", childDAG)
	pb.AddNode("after").DependsOn("sub").WithAction(countingAction(&afterN))
	pdag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = "wf-suspchild"
	w.DAG = pdag
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Run the child out-of-band → it PARKS (a Waiting node persists), not terminal.
	childID := subWorkflowChildID("wf-suspchild", "sub")
	cw := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	require.ErrorIs(t, cw.Execute(context.Background()), ErrSuspended, "child parks")

	// Wake the parent: the child is NON-terminal (a Waiting node) → the parent RE-PARKS,
	// it must NOT falsely complete over a suspended child.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "p1")),
		ErrSuspended, "a suspendable child that parked → the parent re-parks, never a false-complete")
	final, err := store.Load("wf-suspchild")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Waiting)
	require.EqualValues(t, 0, afterN.Load())
}

// 3(c): childTerminal is JOURNAL-ONLY (no DAG cross-check). A journal with a DAG node
// ABSENT (not merely Pending) is judged terminal on the present statuses alone. Real
// InMemory/FB runs pre-seed EVERY node Pending (dag.go:405) before the first Save, so an
// absent-node journal is not naturally produced there — but childTerminal cannot detect
// the gap itself. Documented as FINDING F-92-04 (informational / hardening).
func TestParkedAdv_ChildTerminal_AbsentNodeIsInvisible(t *testing.T) {
	cd := NewWorkflowData("partial")
	cd.SetNodeStatus("a", Completed)
	// node "b" of a two-node child DAG is entirely ABSENT from the journal.
	require.True(t, childTerminal(cd),
		"childTerminal sees only persisted statuses; an absent node is invisible (F-92-04). "+
			"Safe on InMemory/FB because the executor pre-seeds all nodes Pending before the first Save.")
}

// ----------------------------------------------------------------------------
// TARGET 4 — ACK / MAILBOX HYGIENE, especially the FAILURE path.
//
// F-92-01 dispositioned as BY-DESIGN (the ph90 reject-no-ack shape): on a child-FAILED wake the
// completion signal is deliberately NOT acked. Workflow.executeLocked's failure arm explicitly
// skips ackConsumed (workflow.go:515-518: "Do NOT ack here: the run failed, so a consumed signal
// stays INERT ... reclaimed by Store.Delete"). The parked action was corrected to NOT stage an ack
// it cannot flush on the failure path (F-P92-04). So on failure the signal stays inert in the
// mailbox until Delete — NOT a correctness leak: the parent run is terminal, a re-drive is a no-op
// that never re-consumes it, and the mailbox is bounded (F37 cap). The test below characterizes
// this intended behavior (the signal remains, one entry, inert) as the control for the drained
// SUCCESS path above.
// ----------------------------------------------------------------------------

func TestParkedAdv_AckHygiene_SuccessPathDrains(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-ack-ok", childProducing(t, "v", nil), &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	runChildOutOfBand(t, store, "wf-ack-ok", "sub", childProducing(t, "v", nil))
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "ok1")))
	require.Empty(t, mailbox(t, store, "wf-ack-ok"),
		"on the SUCCESS path the completion signal is drained (control for the failure-path leak)")
}

func TestParkedAdv_AckHygiene_FailurePathLeaks(t *testing.T) {
	store := NewInMemoryStore()
	// A child whose single node fails non-coe → the parent node fails on wake.
	cb := NewWorkflowBuilder()
	cb.AddStartNode("boom").WithAction(boomAction())
	childDAG, err := cb.Build()
	require.NoError(t, err)

	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-ack-fail", childDAG, &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	require.Error(t, oobRun(t, store, "wf-ack-fail", "sub", childDAG), "child fails out-of-band")

	err = w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "f1"))
	require.Error(t, err, "a failed child fails the parent node on wake (INV-01)")

	final, lerr := store.Load("wf-ack-fail")
	require.NoError(t, lerr)
	assertNodeStatus(t, final, "sub", Failed)

	// BY-DESIGN (ph90 reject-no-ack): on a child-FAILED wake the signal is deliberately NOT
	// acked (the failure arm keeps it inert; the parent is terminal so it is never re-consumed;
	// reclaimed by Store.Delete). The signal remains as one inert entry — not a correctness leak.
	remaining := mailbox(t, store, "wf-ack-fail")
	require.Lenf(t, remaining, 1,
		"a failed-child completion signal stays inert (reject-no-ack, reclaimed by Delete) — not re-consumed")
	require.Equal(t, completionSignalName("sub"), remaining[0].Name)
}

// TestParkedAdv_AckHygiene_NoAccumulationAcrossReParks — while the child is non-terminal,
// re-parks with the SAME signal ID do not accumulate (ID-keyed mailbox), and the
// eventual success drains it. (A DISTINCT id per re-park WOULD accumulate until the
// completion drains all name-matches — bounded by signalMailboxCap; the single-signal
// host contract keeps it at one.)
func TestParkedAdv_AckHygiene_NoAccumulationAcrossReParks(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := parkedParent(t, store, "wf-noaccum", childProducing(t, "v", nil), &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Re-park three times with the SAME completion signal ID while the child is non-terminal.
	for range 3 {
		require.ErrorIs(t,
			w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "same")),
			ErrSuspended)
	}
	require.Len(t, mailbox(t, store, "wf-noaccum"), 1, "same-ID re-parks do not accumulate")

	// Complete: the single buffered signal drains.
	runChildOutOfBand(t, store, "wf-noaccum", "sub", childProducing(t, "v", nil))
	require.NoError(t, w.DeliverAndResume(context.Background(), SubWorkflowCompletionSignal("sub", "same")))
	require.Empty(t, mailbox(t, store, "wf-noaccum"))
	require.EqualValues(t, 1, afterN.Load())
}
