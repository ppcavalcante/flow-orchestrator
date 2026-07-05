package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 41 (M11) ADVERSARIAL suite for ChoiceNode + the cause-aware Bypassed
// gate. Independent of the author's own choice_test.go / choice_property_test.go
// / cause_aware_test.go — this file attacks the STRUCTURAL edges, the classifier
// combinations reached only through a full DAG.Execute, predicate totality,
// resume terminality, and concurrency. The oracle for most cases is the minimum
// bar (INV-02 totality: every node ends in one of the 7 statuses, no input
// panics/hangs/corrupts) plus the documented routing/cause contract.
//
// VERDICT: every case below HOLDS — no reproduced defect. The tests are durable
// coverage of the adversarial partitions the happy-path suite structurally
// misses, plus three pinned DESIGN SURPRISES (shared branch entry, branch-target
// force-mark, zero-arm choice) that document real behavior an author might not
// expect. See the adversarial report for the non-blocking observations.

// adversAction returns an Action that increments a counter and completes.
func adversAction(c *atomic.Int32) Action {
	return ActionFunc(func(context.Context, *WorkflowData) error { c.Add(1); return nil })
}

// ---------------------------------------------------------------------------
// A. Structural build-time edges (boundary / partition of the wiring space)
// ---------------------------------------------------------------------------

// A ChoiceNode routing to its OWN name folds a self-dependency (route -> route)
// and MUST be rejected by cycle detection at Build — never a runtime hang.
func TestChoiceAdversarial_SelfTargetIsCycle(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-selftarget")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "route")
	_, err := wb.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle", "a choice routing to itself is a build-time cycle, not a hang")
}

// A ChoiceNode routing to one of its OWN upstream nodes folds a 2-cycle
// (seed -> route -> seed) and MUST be a Build error.
func TestChoiceAdversarial_TargetUpstreamIsCycle(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-target-upstream")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "seed")
	_, err := wb.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle", "routing to an upstream forms a cycle")
}

// A ChoiceNode with ZERO When arms and NO Otherwise is a degenerate router: it
// routes nowhere. The contract is TOTALITY — it must surface a typed
// ErrNoBranchMatched (the choice Fails the run fast), never a panic or a silent
// success. DESIGN SURPRISE: this is accepted at Build and only fails at runtime.
func TestChoiceAdversarial_ZeroArm(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-zeroarm")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed") // no When, no Otherwise
	dag, err := wb.Build()
	require.NoError(t, err, "a zero-arm choice builds (no edges to fold)")

	data := NewWorkflowData("adv-zeroarm")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = dag.Execute(ctx, data)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoBranchMatched, "a router with no arms is a typed dead-end")
	assert.NoError(t, ctx.Err(), "it errors, it does not hang")
	assertNodeStatus(t, data, "route", Failed)
}

// A ChoiceNode with only an Otherwise (no When) routes to the default.
func TestChoiceAdversarial_OnlyOtherwise(t *testing.T) {
	var defN atomic.Int32
	wb := NewWorkflowBuilder().WithWorkflowID("adv-onlyotherwise")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").Otherwise("def")
	wb.AddNode("def").WithAction(adversAction(&defN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-onlyotherwise")
	require.NoError(t, dag.Execute(context.Background(), data))
	assert.Equal(t, int32(1), defN.Load(), "the sole Otherwise default runs")
	assertNodeStatus(t, data, "def", Completed)
}

// The SAME target declared by two When arms folds a DUPLICATE dependency edge
// (target depends on route twice). The node must still run EXACTLY once when
// taken, and be Bypassed EXACTLY once (a clean status) when not — the duplicate
// edge must not double-run or corrupt the status.
func TestChoiceAdversarial_DuplicateWhenTarget(t *testing.T) {
	t.Run("taken-once", func(t *testing.T) {
		var aN atomic.Int32
		wb := NewWorkflowBuilder().WithWorkflowID("adv-dup-taken")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return true }, "a").
			When(func(*WorkflowData) bool { return true }, "a")
		wb.AddNode("a").WithAction(adversAction(&aN))
		dag, err := wb.Build()
		require.NoError(t, err)
		data := NewWorkflowData("adv-dup-taken")
		require.NoError(t, dag.Execute(context.Background(), data))
		assert.Equal(t, int32(1), aN.Load(), "a duplicated target runs exactly once when taken")
		assertNodeStatus(t, data, "a", Completed)
	})
	t.Run("bypassed-clean", func(t *testing.T) {
		wb := NewWorkflowBuilder().WithWorkflowID("adv-dup-bypass")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return false }, "a").
			When(func(*WorkflowData) bool { return false }, "a").
			Otherwise("def")
		wb.AddNode("a").WithAction(choiceNoop())
		wb.AddNode("def").WithAction(choiceNoop())
		dag, err := wb.Build()
		require.NoError(t, err)
		data := NewWorkflowData("adv-dup-bypass")
		require.NoError(t, dag.Execute(context.Background(), data))
		assertNodeStatus(t, data, "a", Bypassed) // a single, clean Bypassed
		assertNodeStatus(t, data, "def", Completed)
	})
}

// A target used by BOTH a When and the Otherwise is always taken (either the
// When matches, or the default routes to it): it can never be Bypassed.
func TestChoiceAdversarial_WhenAndOtherwiseSameTarget(t *testing.T) {
	for _, whenVal := range []bool{true, false} {
		t.Run(fmt.Sprintf("when=%v", whenVal), func(t *testing.T) {
			var xN atomic.Int32
			wb := NewWorkflowBuilder().WithWorkflowID("adv-when-otherwise-same")
			wb.AddStartNode("seed").WithAction(choiceNoop())
			wb.AddChoice("route").DependsOn("seed").
				When(func(*WorkflowData) bool { return whenVal }, "x").
				Otherwise("x")
			wb.AddNode("x").WithAction(adversAction(&xN))
			dag, err := wb.Build()
			require.NoError(t, err)
			data := NewWorkflowData("adv-when-otherwise-same")
			require.NoError(t, dag.Execute(context.Background(), data))
			assert.Equal(t, int32(1), xN.Load(), "a When-or-Otherwise target always runs")
			assertNodeStatus(t, data, "x", Completed)
		})
	}
}

// ---------------------------------------------------------------------------
// B. Classifier combinations reached only through a full DAG.Execute
//    (cause_aware_test.go covers classifyBlockedStatus directly; these prove
//     the same rules end-to-end, where markSkippedFrom / the launch gate feed it
//     real cross-level statuses.)
// ---------------------------------------------------------------------------

// (was CoeFailedPlusBypassedDiamond → ph42 MERGE form, DEC-M11-P42-STRICT-SUBSUMES;
// dimension 3 = merge fire semantics) — a MERGE below a coe-FAILED taken branch
// (+ a Bypassed sibling) FIRES: a coe-Failed tail is a taken path that ran
// (depResolved unifies Completed and coe-Failed), so it counts as a taken tail.
func TestMergeAdversarial_CoeFailedTailFires(t *testing.T) {
	boom := ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })
	wb := NewWorkflowBuilder().WithWorkflowID("adv-coefail-merge")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(boom).WithContinueOnError()
	wb.AddNode("small").WithAction(choiceNoop())
	wb.AddMerge("done").From("big", "small")

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-coefail-merge")
	require.NoError(t, dag.Execute(context.Background(), data), "a coe-failed branch tolerates the run")
	assertNodeStatus(t, data, "big", Failed)     // coe-Failed, but a resolved taken tail
	assertNodeStatus(t, data, "small", Bypassed) // not-taken
	assertNodeStatus(t, data, "done", Completed) // >=1 taken tail (coe-Failed) -> merge FIRES
}

// A fail-fast halt elsewhere in the DAG must not corrupt the cause-aware sweep:
// a not-taken branch's DEEP interior chain is Bypassed (not Skipped) via
// markSkippedFrom, the failed branch's own child is Skipped, and the taken
// branch that was never reached stays Pending ("stopped before me").
func TestChoiceAdversarial_FailFastMarksDeepBypassChain(t *testing.T) {
	boom := ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })
	wb := NewWorkflowBuilder().WithWorkflowID("adv-failfast-bypass")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	// An independent hard failure at the choice's level.
	wb.AddNode("boom").DependsOn("seed").WithAction(boom)
	wb.AddNode("boomChild").DependsOn("boom").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("nt")
	wb.AddNode("taken").WithAction(choiceNoop())
	wb.AddNode("nt").WithAction(choiceNoop())
	wb.AddNode("nt1").DependsOn("nt").WithAction(choiceNoop())
	wb.AddNode("nt2").DependsOn("nt1").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-failfast-bypass")
	err = dag.Execute(context.Background(), data)
	require.Error(t, err) // fail-fast

	assertNodeStatus(t, data, "boom", Failed)
	assertNodeStatus(t, data, "boomChild", Skipped) // failure cascade
	// The not-taken interior chain is Bypassed all the way down (cause = routing,
	// NOT the unrelated failure) — the sweep must classify by cause, not blanket-Skip.
	assertNodeStatus(t, data, "nt", Bypassed)
	assertNodeStatus(t, data, "nt1", Bypassed)
	assertNodeStatus(t, data, "nt2", Bypassed)
	// The taken branch was never reached (run halted); it is Pending, not Skipped.
	assertNodeStatus(t, data, "taken", Pending)
	// INV-02 totality: every node ended in one of the 7 statuses.
	assertAllStatusesValid(t, data, []string{"seed", "boom", "boomChild", "route", "taken", "nt", "nt1", "nt2"})
}

// (was TwoBypassedJoinIsBypassed → ph42 MERGE form, DEC-M11-P42-STRICT-SUBSUMES;
// dimension 3 = merge bypass semantics) — a MERGE over TWO not-taken branches
// (the taken branch NOT among its tails) sees all tails Bypassed -> the merge is
// itself Bypassed (MH-2, 0 taken tails).
func TestMergeAdversarial_MergeOverBypassedBranchesIsBypassed(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-two-bypassed")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		When(func(*WorkflowData) bool { return false }, "b1").
		When(func(*WorkflowData) bool { return false }, "b2")
	wb.AddNode("taken").WithAction(choiceNoop())
	wb.AddNode("b1").WithAction(choiceNoop())
	wb.AddNode("b2").WithAction(choiceNoop())
	wb.AddMerge("joinBypass").From("b1", "b2") // joins ONLY the two not-taken branches

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-two-bypassed")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "b1", Bypassed)
	assertNodeStatus(t, data, "b2", Bypassed)
	assertNodeStatus(t, data, "joinBypass", Bypassed) // 0 taken tails -> merge Bypassed
	assertNodeStatus(t, data, "taken", Completed)
}

// (was MultiMergeDiamond, DEC-M11-P42-STRICT-SUBSUMES) — multiple PLAIN nodes
// reconverging the same Choice's branches (incl. a transitive one) is rejected at
// Build. The structured multi-merge FIRING (merge-feeding-merge, order-independent)
// is covered by TestMergeAdv_MergeFeedingMerge.
func TestMergeAdversarial_MultiReconvergenceRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-multi-merge")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(choiceNoop())
	wb.AddNode("small").WithAction(choiceNoop())
	wb.AddNode("j1").DependsOn("big", "small").WithAction(choiceNoop()) // plain reconvergence
	wb.AddNode("j2").DependsOn("small", "big").WithAction(choiceNoop())
	wb.AddNode("j3").DependsOn("j1", "j2").WithAction(choiceNoop())
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge)
}

// A branch entry that is ITSELF a ChoiceNode: when the outer choice bypasses the
// inner choice, the inner never runs to mark its own targets, so the gate must
// propagate Bypassed to the inner choice AND its (now unreached) branch targets.
func TestChoiceAdversarial_NestedChoiceBypassPropagates(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-nested")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("outer").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("inner")
	wb.AddNode("taken").WithAction(choiceNoop())
	// inner is a ChoiceNode wired as outer's Otherwise target (bypassed here).
	wb.AddChoice("inner").
		When(func(*WorkflowData) bool { return true }, "innerA").
		Otherwise("innerB")
	wb.AddNode("innerA").WithAction(choiceNoop())
	wb.AddNode("innerB").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-nested")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "taken", Completed)
	assertNodeStatus(t, data, "inner", Bypassed)  // the bypassed inner choice never decides
	assertNodeStatus(t, data, "innerA", Bypassed) // ... so its targets are Bypassed by the gate
	assertNodeStatus(t, data, "innerB", Bypassed)
}

// A taken branch whose entry ALSO depends on an unrelated coe-Failed node still
// runs — both deps resolve (choice Completed + coe-Failed).
func TestChoiceAdversarial_MixedDepTakenBranchRuns(t *testing.T) {
	boom := ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })
	var aN atomic.Int32
	wb := NewWorkflowBuilder().WithWorkflowID("adv-mixed-taken")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddNode("x").DependsOn("seed").WithAction(boom).WithContinueOnError()
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a").
		Otherwise("b")
	wb.AddNode("a").DependsOn("x").WithAction(adversAction(&aN)) // a depends on route + x
	wb.AddNode("b").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-mixed-taken")
	require.NoError(t, dag.Execute(context.Background(), data))
	assert.Equal(t, int32(1), aN.Load(), "a taken branch with a coe-Failed extra parent still runs")
	assertNodeStatus(t, data, "a", Completed)
	assertNodeStatus(t, data, "b", Bypassed)
}

// ---------------------------------------------------------------------------
// C. Predicate totality
// ---------------------------------------------------------------------------

// A predicate that PANICS is caller code that violates its contract. The engine
// does NOT recover any action's panic (this is engine-wide, not choice-specific:
// n.Execute has no recover), so the totality boundary is "a panicking predicate
// panics the caller" — documented here at the synchronous choiceAction.Execute
// so the suite records the boundary without aborting the process (a panic inside
// the executor goroutine would crash the test binary).
func TestChoiceAdversarial_PanickingPredicate(t *testing.T) {
	a := &choiceAction{
		nodeName: "route",
		branches: []choiceBranch{{
			predicate: func(*WorkflowData) bool { panic("predicate blew up") },
			target:    "x",
		}},
	}
	data := NewWorkflowData("adv-panic")
	assert.Panics(t, func() {
		// The call must panic; if it ever returns, the boundary changed.
		if err := a.Execute(context.Background(), data); err != nil {
			t.Errorf("unreachable: predicate should have panicked, got %v", err)
		}
	}, "a panicking predicate is caller misuse; the engine recovers no action panic (documented totality boundary)")
}

// A side-effecting predicate (mutating data, reading a key an earlier arm set) is
// caller misuse, but first-match over DECLARED ORDER makes the outcome
// deterministic and independent of run count — the routing contract holds even
// for an impure predicate.
func TestChoiceAdversarial_SideEffectingPredicateDeterministic(t *testing.T) {
	build := func() *DAG {
		wb := NewWorkflowBuilder().WithWorkflowID("adv-sideeffect")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			// arm 0: false, but sets "flag" as a side effect.
			When(func(d *WorkflowData) bool { d.Set("flag", 1); return false }, "a").
			// arm 1: reads the side effect written by arm 0 (declared-order visible).
			When(func(d *WorkflowData) bool { v, _ := d.GetInt("flag"); return v == 1 }, "b").
			Otherwise("c")
		wb.AddNode("a").WithAction(choiceNoop())
		wb.AddNode("b").WithAction(choiceNoop())
		wb.AddNode("c").WithAction(choiceNoop())
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag
	}
	// Same spec routes to "b" every run (arm 0's side effect makes arm 1 true).
	for range 5 {
		data := NewWorkflowData("adv-sideeffect")
		require.NoError(t, build().Execute(context.Background(), data))
		assertNodeStatus(t, data, "a", Bypassed)
		assertNodeStatus(t, data, "b", Completed) // first-match after the side effect
		assertNodeStatus(t, data, "c", Bypassed)
	}
}

// ---------------------------------------------------------------------------
// D. Persistence / resume
// ---------------------------------------------------------------------------

// A persisted Bypassed node is TERMINAL: re-entering Execute (the M9 resume path)
// leaves it Bypassed and never re-runs its action — the resume-stability the
// Bypassed status must guarantee. Also confirms the taken branch (Completed) is
// not re-run.
func TestChoiceAdversarial_BypassedSurvivesResume(t *testing.T) {
	var takenN, ntN atomic.Int32
	build := func() *DAG {
		wb := NewWorkflowBuilder().WithWorkflowID("adv-resume")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return true }, "taken").
			Otherwise("nt")
		wb.AddNode("taken").WithAction(adversAction(&takenN))
		wb.AddNode("nt").WithAction(adversAction(&ntN))
		wb.AddNode("ntChild").DependsOn("nt").WithAction(choiceNoop())
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag
	}

	data := NewWorkflowData("adv-resume")
	require.NoError(t, build().Execute(context.Background(), data))
	assertNodeStatus(t, data, "nt", Bypassed)
	assertNodeStatus(t, data, "ntChild", Bypassed)
	require.Equal(t, int32(1), takenN.Load())
	require.Equal(t, int32(0), ntN.Load())

	// Round-trip through a store, then re-enter Execute on the loaded data.
	store := NewInMemoryStore()
	require.NoError(t, store.Save(data))
	loaded, err := store.Load("adv-resume")
	require.NoError(t, err)
	require.NoError(t, build().Execute(context.Background(), loaded))

	// Bypassed nodes stayed Bypassed; nothing re-ran across the resume.
	assertNodeStatus(t, loaded, "nt", Bypassed)
	assertNodeStatus(t, loaded, "ntChild", Bypassed)
	assert.Equal(t, int32(1), takenN.Load(), "the taken branch does not re-run on resume")
	assert.Equal(t, int32(0), ntN.Load(), "a Bypassed node NEVER runs, even on resume")
}

// A truncated FlatBuffers file that contained a Bypassed node must load as a
// typed ErrCorruptData — never a panic. The new 7th status opens no decode hole.
func TestChoiceAdversarial_TruncatedFBWithBypassed(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFlatBuffersStore(dir)
	require.NoError(t, err)
	const id = "adv-truncated-fb"

	d := NewWorkflowData(id)
	d.SetNodeStatus("bypass", Bypassed)
	d.SetNodeStatus("done", Completed)
	require.NoError(t, store.Save(d))

	p := filepath.Join(dir, id+".fb")
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Greater(t, len(raw), 8)
	// Truncate to a short prefix — a classic short-file decode hazard.
	require.NoError(t, os.WriteFile(p, raw[:len(raw)/3], 0o600))

	require.NotPanics(t, func() {
		_, loadErr := store.Load(id)
		require.Error(t, loadErr)
		assert.ErrorIs(t, loadErr, ErrCorruptData, "a truncated FB is typed CorruptData, not a panic")
	})
}

// ---------------------------------------------------------------------------
// E. Concurrency / determinism / design-surprise pins
// ---------------------------------------------------------------------------

// A wide choice fan-out (many branches, deep interiors, a shared entry) under
// HIGH MaxConcurrency, run repeatedly. The cross-goroutine SetNodeStatus of
// branch entries must be race-clean (run with -race) and the final status map
// must be identical every run (determinism).
func TestChoiceAdversarial_RaceHighConcurrency(t *testing.T) {
	build := func() (*DAG, []string) {
		wb := NewWorkflowBuilder().
			WithWorkflowID("adv-race").
			WithExecutionConfig(ExecutionConfig{MaxConcurrency: 32})
		wb.AddStartNode("seed").WithAction(choiceNoop())
		names := []string{"seed"}
		// A choice with 8 branches; branch 0 taken, rest bypassed, each with a chain.
		ch := wb.AddChoice("route").DependsOn("seed")
		for j := range 8 {
			taken := j == 0
			entry := fmt.Sprintf("b%d", j)
			wb.AddNode(entry).WithAction(choiceNoop())
			names = append(names, entry)
			prev := entry
			for i := 1; i <= 3; i++ {
				n := fmt.Sprintf("b%d_%d", j, i)
				wb.AddNode(n).DependsOn(prev).WithAction(choiceNoop())
				names = append(names, n)
				prev = n
			}
			ch.When(func(*WorkflowData) bool { return taken }, entry)
		}
		names = append(names, "route")
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag, names
	}

	var first map[string]NodeStatus
	for run := range 8 {
		dag, names := build()
		data := NewWorkflowData("adv-race")
		require.NoError(t, dag.Execute(context.Background(), data))
		snap := map[string]NodeStatus{}
		for _, n := range names {
			st, ok := data.GetNodeStatus(n)
			require.True(t, ok, "node %s missing", n)
			require.True(t, the7Statuses[st], "node %s status %q outside the 7-set", n, st)
			snap[n] = st
		}
		if run == 0 {
			first = snap
		} else {
			assert.Equal(t, first, snap, "final status map must be deterministic across runs")
		}
	}
}

// 41-F2 RESOLVED (ph42): a branch entry SHARED by two different ChoiceNodes — the
// footgun ph41 could only pin as deterministic "bypass wins" — is now REJECTED at
// build by the strict reconvergence validator (D-P42-STRICT(c)). The ambiguous
// shared ownership can no longer be constructed, so the surprising runtime
// behavior is unreachable.
func TestChoiceAdversarial_SharedEntryRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("adv-shared")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("A").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "shared"). // A routes TO shared
		Otherwise("aOther")
	wb.AddChoice("B").DependsOn("seed").
		When(func(*WorkflowData) bool { return false }, "shared"). // B also targets shared
		Otherwise("bOther")
	wb.AddNode("shared").WithAction(choiceNoop())
	wb.AddNode("aOther").WithAction(choiceNoop())
	wb.AddNode("bOther").WithAction(choiceNoop())

	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSharedBranch, "a branch entry owned by two Choices is a build error (41-F2)")
}

// DESIGN SURPRISE (pinned, non-blocking): a not-taken branch entry that ALSO has
// an independent, fully-resolved (non-choice) parent is STILL Bypassed. The
// choice's direct force-mark wins over the gate's would-be resolve of the
// independent parent. Deterministic and total; pinned so a change is deliberate.
func TestChoiceAdversarial_BranchTargetForceMarkOverIndependentParent(t *testing.T) {
	var ntN atomic.Int32
	wb := NewWorkflowBuilder().WithWorkflowID("adv-forcemark")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddNode("other").DependsOn("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken").
		Otherwise("nt")
	wb.AddNode("taken").WithAction(choiceNoop())
	// nt is a not-taken entry that also depends on the completed "other".
	wb.AddNode("nt").DependsOn("other").WithAction(adversAction(&ntN))

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("adv-forcemark")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "other", Completed)
	assertNodeStatus(t, data, "nt", Bypassed) // force-mark wins over the resolvable parent
	assert.Equal(t, int32(0), ntN.Load(), "the bypassed entry never runs despite a completed independent parent")
}

// assertAllStatusesValid checks INV-02 totality: every named node ends in one of
// the 7 statuses (the codomain of the status map after Execute).
func assertAllStatusesValid(t *testing.T, data *WorkflowData, names []string) {
	t.Helper()
	for _, n := range names {
		st, ok := data.GetNodeStatus(n)
		require.True(t, ok, "node %s absent from status map (totality)", n)
		assert.True(t, the7Statuses[st], "node %s status %q outside the 7-set (INV-02)", n, st)
	}
}
