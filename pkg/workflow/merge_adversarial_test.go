package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 42 (M11) — INDEPENDENT adversarial suite for the MergeNode / OR-join /
// strict-reconvergence validator. Designed by the anvil-adversarial-tester to hit
// the boundary/partition/property classes the author's own merge_test.go +
// merge_property_test.go structurally miss. The hard bar: no input makes the
// OR-join fire when it must not, bypass when it must fire, panic, hang, or drop
// downstream control flow silently.
//
// This suite originally shipped TWO reproduced defects (RED-by-design); both are
// now FIXED (commit e0d9bf2) and their tests are permanent regression guards:
//
//   - TestMergeAdv_EmptyBranchFromChoiceRejected (was DEFECT1): empty-branch
//     From(<Choice>) is now REJECTED at Build rather than silently mis-firing.
//   - TestMergeAdv_DEFECT2_ExtraDependsOnSiblingVacuousFire: an extra DependsOn
//     sibling no longer inflates the taken count (the gate counts over the From
//     tail set, not the full DependsOn).
//
// Root cause (fixed): the fire-vs-bypass gate in executeNodesInLevel
// (parallel_execution.go, role==mergeDependent) now counts "taken" over the
// recorded From tail set (mergeAction.tails), not the entire node.DependsOn — so
// neither a non-tail DependsOn (over-count) nor the structural Choice-dep can
// inflate firing, and empty-branch From(<Choice>) tails are rejected at Build.

// ---------------------------------------------------------------------------
// Former reproduced defects (now fixed → regression guards)
// ---------------------------------------------------------------------------

// (was DEFECT1) — an empty-branch OR-join tail From(<Choice>) is rejected at Build
// (proper empty-branch support needs a per-branch taken signal — deferred). When
// the taken one. route has a real body branch (big->bigEnd) and an empty branch
// (Otherwise -> emptyMarker, a bodyless placeholder). The merge OR-joins
// From("bigEnd","route") — "route" being the empty branch's tail per the design.
//
// When the empty branch is taken (pickBig=false): bigEnd is Bypassed and the only
// taken tail is the empty branch, represented by the Choice "route". But the fire
// gate excludes every *choiceAction dep, so takenTails==0 and the merge is
// Bypassed — silently dropping `after` and the entire post-merge subgraph, even
// though a perfectly legitimate branch was taken.
//
// RESOLUTION (42-AF1/42-F2): an empty-branch merge tail From(<ChoiceNode>) is a
// silent-misfire trap — the always-Completed Choice carries no per-branch "was
// this empty branch taken?" signal, so the fire gate cannot distinguish a taken
// empty branch from a bypassed one. Rather than half-support it, the validator
// now REJECTS From(<Choice>) at Build (proper empty-branch support needs a
// per-branch taken signal — deferred to a future phase). This test pins the
// rejection.
func TestMergeAdv_EmptyBranchFromChoiceRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("ebfc")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("emptyMarker")
	wb.AddNode("big").WithAction(choiceNoop())
	wb.AddNode("bigEnd").DependsOn("big").WithAction(choiceNoop())
	wb.AddNode("emptyMarker").WithAction(choiceNoop())
	wb.AddMerge("done").From("bigEnd", "route") // a ChoiceNode as a tail -> rejected
	wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())

	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge, "a merge tail that is a ChoiceNode is rejected")
}

// DEFECT2 — mergeBuilder.DependsOn (merge.go:114-120, "additional upstream
// dependencies … kept for symmetry") on a SIBLING branch-tail of the same Choice
// is miscounted as a taken OR-join tail. The merge joins only branches b and c
// (From("b","c")) but also DependsOn("a"). When the Choice takes branch a — a
// branch the merge does NOT join — none of the merge's OR-join tails were taken,
// so by the anti-vacuity semantics the merge must be Bypassed (exactly like
// TestMerge_AntiVacuityBite). Instead the merge FIRES, because the fire gate
// counts a's Completed tail (a non-*choiceAction dep) as taken.
//
// This defeats the anti-vacuity guard from the DependsOn direction: only
// same-Choice nodes survive the strict validator as merge deps (a different-Choice
// or unrelated dep is rejected as cross-Choice / dangling), and every such
// same-Choice node inflates the taken count when it is the taken branch.
//
// EXPECTED (asserted below): done=Bypassed, after=Bypassed.
// OBSERVED today: done=Completed, after=Completed. RED until fixed.
func TestMergeAdv_DEFECT2_ExtraDependsOnSiblingVacuousFire(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("extradep")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a"). // take A (NOT joined by the merge)
		When(func(*WorkflowData) bool { return false }, "b").
		Otherwise("c")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("b").WithAction(choiceNoop())
	wb.AddNode("c").WithAction(choiceNoop())
	wb.AddMerge("done").From("b", "c").DependsOn("a") // joins B,C only; extra dep on A
	wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("extradep")
	require.NoError(t, dag.Execute(context.Background(), data))

	assertNodeStatus(t, data, "a", Completed)
	assertNodeStatus(t, data, "b", Bypassed)
	assertNodeStatus(t, data, "c", Bypassed)
	// None of the merge's OR-join tails (b,c) were taken -> vacuous, must Bypass.
	assertNodeStatus(t, data, "done", Bypassed) // <-- RED: observed Completed
	assertNodeStatus(t, data, "after", Bypassed)
}

// ---------------------------------------------------------------------------
// Durable adversarial coverage (all GREEN)
// ---------------------------------------------------------------------------

// A coe-Failed taken tail still FIRES the merge (coe-Failed resolves as "taken").
// Boundary between the fail-fast (non-coe) path (TestMerge_TakenTailFails_FailFast)
// and the tolerated-failure path.
func TestMergeAdv_CoeFailedTakenTailFires(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("coe")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(choiceNoop())
	be := wb.AddNode("bigEnd").DependsOn("big")
	be.WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return context.Canceled }))
	be.WithContinueOnError()
	wb.AddNode("small").WithAction(choiceNoop())
	wb.AddNode("smallEnd").DependsOn("small").WithAction(choiceNoop())
	wb.AddMerge("done").From("bigEnd", "smallEnd")
	wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("coe")
	require.NoError(t, dag.Execute(context.Background(), data))

	assertNodeStatus(t, data, "bigEnd", Failed)     // coe-Failed
	assertNodeStatus(t, data, "smallEnd", Bypassed) // not-taken
	assertNodeStatus(t, data, "done", Completed)    // fired on the coe-Failed taken tail
	assertNodeStatus(t, data, "after", Completed)
}

// Mixed tails in one join: a Completed tail, a Bypassed (not-taken) tail, and a
// coe-Failed tail all under one 3-branch Choice with two of the branches taken is
// impossible (a Choice takes exactly one) — so this exercises the realistic mix a
// single-choice merge sees: taken (Completed OR coe-Failed) + not-taken (Bypassed).
// Here branch "mid" is taken and Completes; big/small are Bypassed. Fires.
func TestMergeAdv_MixedTailsSingleTaken(t *testing.T) {
	for _, taken := range []string{"big", "mid", "small"} {
		taken := taken
		t.Run(taken, func(t *testing.T) {
			wb := NewWorkflowBuilder().WithWorkflowID("mix")
			wb.AddStartNode("seed").WithAction(choiceNoop())
			wb.AddChoice("route").DependsOn("seed").
				When(func(*WorkflowData) bool { return taken == "big" }, "big").
				When(func(*WorkflowData) bool { return taken == "mid" }, "mid").
				Otherwise("small")
			for _, n := range []string{"big", "mid", "small"} {
				wb.AddNode(n).WithAction(choiceNoop())
				wb.AddNode(n + "End").DependsOn(n).WithAction(choiceNoop())
			}
			wb.AddMerge("done").From("bigEnd", "midEnd", "smallEnd")
			wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())
			dag, err := wb.Build()
			require.NoError(t, err)
			data := NewWorkflowData("mix")
			require.NoError(t, dag.Execute(context.Background(), data))

			assertNodeStatus(t, data, taken+"End", Completed)
			assertNodeStatus(t, data, "done", Completed)
			assertNodeStatus(t, data, "after", Completed)
			for _, n := range []string{"big", "mid", "small"} {
				if n == taken {
					continue
				}
				assertNodeStatus(t, data, n+"End", Bypassed)
			}
		})
	}
}

// Merge feeding a merge under a SINGLE Choice: m2.From(m1). nearestBranch must
// trace m1 back to its source Choice and the DEPMODEL edge is added at each level.
// The chained OR-join fires end to end.
func TestMergeAdv_MergeFeedingMerge(t *testing.T) {
	for _, pickA := range []bool{true, false} {
		pickA := pickA
		t.Run(fmt.Sprintf("pickA=%v", pickA), func(t *testing.T) {
			wb := NewWorkflowBuilder().WithWorkflowID("m2m")
			wb.AddStartNode("seed").WithAction(choiceNoop())
			wb.AddChoice("route").DependsOn("seed").
				When(func(*WorkflowData) bool { return pickA }, "a").
				Otherwise("b")
			wb.AddNode("a").WithAction(choiceNoop())
			wb.AddNode("b").WithAction(choiceNoop())
			wb.AddMerge("m1").From("a", "b")
			wb.AddMerge("m2").From("m1") // tail is another merge
			wb.AddNode("after").DependsOn("m2").WithAction(choiceNoop())
			dag, err := wb.Build()
			require.NoError(t, err)
			// Both merges carry the DEPMODEL edge to the source Choice.
			m1, _ := dag.GetNode("m1")
			require.True(t, dependsOn(m1, "route"), "m1 gets the DEPMODEL Choice-dep")

			data := NewWorkflowData("m2m")
			require.NoError(t, dag.Execute(context.Background(), data))
			assertNodeStatus(t, data, "m1", Completed)
			assertNodeStatus(t, data, "m2", Completed)
			assertNodeStatus(t, data, "after", Completed)
		})
	}
}

// Two merges sharing the same From tails both fire independently.
func TestMergeAdv_TwoMergesSharingTails(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("share")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a").Otherwise("b")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("b").WithAction(choiceNoop())
	wb.AddMerge("m1").From("a", "b")
	wb.AddMerge("m2").From("a", "b")
	wb.AddNode("after1").DependsOn("m1").WithAction(choiceNoop())
	wb.AddNode("after2").DependsOn("m2").WithAction(choiceNoop())
	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("share")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "m1", Completed)
	assertNodeStatus(t, data, "m2", Completed)
	assertNodeStatus(t, data, "after1", Completed)
	assertNodeStatus(t, data, "after2", Completed)
}

// A merge with a single From tail is a valid degenerate OR-join: it fires iff that
// one tail is taken, and is Bypassed if the branch was not taken (anti-vacuity).
func TestMergeAdv_SingleTail(t *testing.T) {
	// Build a 2-branch Choice but the merge joins only ONE branch's tail.
	build := func(takeJoined bool) (*DAG, *WorkflowData) {
		wb := NewWorkflowBuilder().WithWorkflowID("single")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return takeJoined }, "j").
			Otherwise("o")
		wb.AddNode("j").WithAction(choiceNoop())
		wb.AddNode("o").WithAction(choiceNoop())
		wb.AddMerge("done").From("j") // single-tail join over branch j only
		wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag, NewWorkflowData("single")
	}

	dag, data := build(true) // joined branch taken -> fires
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "done", Completed)
	assertNodeStatus(t, data, "after", Completed)

	dag, data = build(false) // joined branch NOT taken -> anti-vacuity Bypass
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "o", Completed)
	assertNodeStatus(t, data, "done", Bypassed)
	assertNodeStatus(t, data, "after", Bypassed)
}

// A Waiting (suspended timer) branch-tail upstream of a merge must NOT make the
// merge misfire or prematurely bypass: the merge stays Pending across the park,
// and fires only after the timer wakes. Totality across the suspend boundary
// (M10 x M11 composition). Uses Workflow.Execute (timers require a Store+Clock).
func TestMergeAdv_WaitingTailDoesNotMisfire(t *testing.T) {
	store := NewInMemoryStore()
	clk := NewFakeClock(time.Unix(1000, 0))
	const id = "wm"
	mk := func() *Workflow {
		wb := NewWorkflowBuilder().WithWorkflowID(id).WithStore(store).WithClock(clk)
		wb.AddStartNode("seed").WithAction(choiceNoop())
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return true }, "sleep").
			Otherwise("other")
		wb.AddTimer("sleep", time.Minute)
		wb.AddNode("other").WithAction(choiceNoop())
		wb.AddMerge("done").From("sleep", "other")
		wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())
		w, err := FromBuilder(wb)
		require.NoError(t, err)
		return w
	}

	// Run 1: the taken branch parks on the timer; the merge must be left Pending
	// (NOT Bypassed, NOT Completed) — its taken tail is still Waiting.
	require.ErrorIs(t, mk().Execute(context.Background()), ErrSuspended)
	parked, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, parked, "sleep", Waiting)
	assertNodeStatus(t, parked, "done", Pending) // did NOT misfire while a tail waits
	assertNodeStatus(t, parked, "after", Pending)

	// Run 2: after the timer wakes, the merge fires and downstream runs.
	clk.Advance(time.Minute)
	require.NoError(t, mk().Execute(context.Background()))
	final, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, final, "sleep", Completed)
	assertNodeStatus(t, final, "done", Completed)
	assertNodeStatus(t, final, "after", Completed)
}

// A Bypassed merge round-trips through the InMemory and FlatBuffers stores as
// Bypassed (durable-status fidelity for the 7th status through the OR-join).
func TestMergeAdv_BypassedMergePersists(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("persist")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("oc").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "live").Otherwise("ic")
	wb.AddNode("live").WithAction(choiceNoop())
	wb.AddChoice("ic").
		When(func(*WorkflowData) bool { return true }, "ia").Otherwise("ib")
	wb.AddNode("ia").WithAction(choiceNoop())
	wb.AddNode("ib").WithAction(choiceNoop())
	wb.AddMerge("im").From("ia", "ib")
	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("persist")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "im", Bypassed)

	fb, ferr := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, ferr)
	for _, store := range []WorkflowStore{NewInMemoryStore(), fb} {
		require.NoError(t, store.Save(data))
		got, err := store.Load("persist")
		require.NoError(t, err)
		st, ok := got.GetNodeStatus("im")
		require.True(t, ok)
		assert.Equal(t, Bypassed, st, "%T must round-trip a Bypassed merge", store)
	}
}

// ---------------------------------------------------------------------------
// Validator adversarial cases (totality: every malformed shape is a clean typed
// Build error or a sound no-op — never a panic or a runtime surprise)
// ---------------------------------------------------------------------------

// A self-referential merge (From itself) is a cycle -> clean Build error, no hang.
func TestMergeAdv_SelfReferentialMergeRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("selfref")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a").Otherwise("b")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("b").WithAction(choiceNoop())
	wb.AddMerge("done").From("a", "b", "done") // self-tail
	_, err := wb.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle", "a self-referential merge is a cycle, not a panic")
}

// The validator is a no-op on a DAG with no Choices/Merges — a plain reconvergent
// diamond (two paths joining at one node with NO Choice) must build and run: the
// non-merge-reconvergence rule only bites branches OF a ChoiceNode.
func TestMergeAdv_ValidatorNoOpWithoutChoices(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("plain")
	wb.AddStartNode("a").WithAction(choiceNoop())
	wb.AddNode("b").DependsOn("a").WithAction(choiceNoop())
	wb.AddNode("c").DependsOn("a").WithAction(choiceNoop())
	wb.AddNode("join").DependsOn("b", "c").WithAction(choiceNoop()) // plain diamond, no Choice
	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("plain")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "join", Completed)
}

// A plain (non-merge, non-choice) node sitting BETWEEN a branch tail and the merge
// is a legal branch interior — it traces to the one Choice and builds. Guards that
// nearestBranch climbs through interior nodes rather than mis-flagging them.
func TestMergeAdv_InteriorNodeBetweenTailAndMerge(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("interior")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a").Otherwise("b")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("aMid").DependsOn("a").WithAction(choiceNoop())
	wb.AddNode("aEnd").DependsOn("aMid").WithAction(choiceNoop())
	wb.AddNode("b").WithAction(choiceNoop())
	wb.AddNode("bEnd").DependsOn("b").WithAction(choiceNoop())
	wb.AddMerge("done").From("aEnd", "bEnd")
	wb.AddNode("after").DependsOn("done").WithAction(choiceNoop())
	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("interior")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "aEnd", Completed)
	assertNodeStatus(t, data, "done", Completed)
	assertNodeStatus(t, data, "after", Completed)
}

// A merge-of-merge that would span TWO different Choice levels (an inner merge
// under choice c2's branch, joined with an outer sibling branch of c1) is REJECTED
// at Build with a typed cross-Choice error — fails safe, never a runtime misfire.
// Documents the strict validator's boundary (a real, intended limitation).
func TestMergeAdv_NestedMergeAcrossChoiceLevelsRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("deep")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("c1").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "c2").Otherwise("d1")
	wb.AddNode("d1").WithAction(choiceNoop())
	wb.AddChoice("c2").
		When(func(*WorkflowData) bool { return true }, "leaf").Otherwise("d2")
	wb.AddNode("d2").WithAction(choiceNoop())
	wb.AddNode("leaf").WithAction(choiceNoop())
	wb.AddMerge("m2").From("leaf", "d2") // inner merge: reconverges c2
	wb.AddMerge("m1").From("m2", "d1")   // m2 traces to c2, d1 to c1 -> cross-Choice
	wb.AddNode("after").DependsOn("m1").WithAction(choiceNoop())
	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge)
}

// The DEPMODEL edge the validator ADDS (merge -> source Choice) must not create a
// double edge when the merge already lists that Choice via an explicit
// mergeBuilder.DependsOn. Guards the dependsOn() idempotence guard in the validator.
func TestMergeAdv_DepModelEdgeNoDoubleAdd(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("nodouble")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "a").Otherwise("b")
	wb.AddNode("a").WithAction(choiceNoop())
	wb.AddNode("aEnd").DependsOn("a").WithAction(choiceNoop())
	wb.AddNode("b").WithAction(choiceNoop())
	wb.AddNode("bEnd").DependsOn("b").WithAction(choiceNoop())
	// The merge already lists its source Choice via an explicit DependsOn.
	wb.AddMerge("done").From("aEnd", "bEnd").DependsOn("route")
	dag, err := wb.Build()
	require.NoError(t, err)
	done, _ := dag.GetNode("done")
	routeCount := 0
	for _, d := range done.DependsOn {
		if d.Name == "route" {
			routeCount++
		}
	}
	assert.Equal(t, 1, routeCount, "the Choice must appear exactly once in the merge's deps (no DEPMODEL double-add)")
}

// ---------------------------------------------------------------------------
// Property: determinism of the OR-join across repeated runs and store round-trips.
// ---------------------------------------------------------------------------

// Repeated Execute of the same structured choice-merge under a non-trivial
// concurrency limit yields byte-identical status maps (CHOICE-02 for the merge,
// stressed under many parallel independent merges). Run with -race for the
// concurrency safety of the fire gate.
func TestMergeAdv_ManyMergesDeterministicUnderConcurrency(t *testing.T) {
	const N = 24
	build := func() *DAG {
		wb := NewWorkflowBuilder().WithWorkflowID("many").
			WithExecutionConfig(ExecutionConfig{MaxConcurrency: 8})
		wb.AddStartNode("seed").WithAction(choiceNoop())
		for i := 0; i < N; i++ {
			ci := fmt.Sprintf("c%d", i)
			a := fmt.Sprintf("a%d", i)
			b := fmt.Sprintf("b%d", i)
			m := fmt.Sprintf("m%d", i)
			take := i%2 == 0
			wb.AddChoice(ci).DependsOn("seed").
				When(func(*WorkflowData) bool { return take }, a).Otherwise(b)
			wb.AddNode(a).WithAction(choiceNoop())
			wb.AddNode(b).WithAction(choiceNoop())
			wb.AddMerge(m).From(a, b)
			wb.AddNode("after" + m).DependsOn(m).WithAction(choiceNoop())
		}
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag
	}

	var first map[string]NodeStatus
	for run := 0; run < 4; run++ {
		data := NewWorkflowData("many")
		require.NoError(t, build().Execute(context.Background(), data))
		snap := map[string]NodeStatus{}
		data.ForEachNodeStatus(func(name string, st NodeStatus) { snap[name] = st })
		// Every merge fired (each choice takes exactly one branch).
		for i := 0; i < N; i++ {
			assert.Equal(t, Completed, snap[fmt.Sprintf("m%d", i)], "merge m%d must fire", i)
		}
		if run == 0 {
			first = snap
		} else {
			assert.Equal(t, first, snap, "OR-join firing must be deterministic across runs")
		}
	}
}
