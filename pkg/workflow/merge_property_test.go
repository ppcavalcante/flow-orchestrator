package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 42 (M11) T3 — bite-proven OR-join property suite. Runs over random
// structured choice-merge DAGs and documents the discriminating mutation that
// turns each property RED. The load-bearing hard-bar check is the anti-vacuity
// bite (red-team MAJOR-1): firing must NOT be keyed on the always-Completed
// structural Choice-dep.

// buildChoiceMergeDAG builds seed -> route(K When branches, each a chain of
// `depth`) -> done = merge.From(all K branch tails) -> after. predBits[j] is
// branch j's predicate; the first true is taken (else the default, index K-1's
// Otherwise... here we always give the last branch an Otherwise so there is a
// taken branch). Returns the DAG, the probe, and the expected taken branch index.
func buildChoiceMergeDAG(t *testing.T, k, depth int, predBits []bool, probe *choiceProbe) (*DAG, int) {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID("cmp")
	wb.AddStartNode("seed").WithAction(probe.action("seed"))
	ch := wb.AddChoice("route").DependsOn("seed")

	tails := make([]string, 0, k)
	addBranch := func(j int) string {
		names := choiceBranchNodes(j, depth, -1) // -1 => never the default sentinel
		for i, name := range names {
			nb := wb.AddNode(name).WithAction(probe.action(name))
			if i > 0 {
				nb.DependsOn(names[i-1])
			}
		}
		return names[len(names)-1] // the tail
	}

	expected := k - 1 // the Otherwise branch, unless an earlier When matches
	for j := 0; j < k; j++ {
		tail := addBranch(j)
		tails = append(tails, tail)
		if j < k-1 {
			val := predBits[j]
			ch.When(func(*WorkflowData) bool { return val }, fmt.Sprintf("b%d", j))
			if val && expected == k-1 {
				expected = j
			}
		} else {
			ch.Otherwise(fmt.Sprintf("b%d", j))
		}
	}
	wb.AddMerge("done").From(tails...)
	wb.AddNode("after").DependsOn("done").WithAction(probe.action("after"))

	dag, err := wb.Build()
	require.NoError(t, err)
	return dag, expected
}

// TestMergeProperties is the T3 gopter suite.
func TestMergeProperties(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 100
	properties := gopter.NewProperties(params)

	// JOIN-01 + totality: over any structured choice-merge, exactly the taken
	// branch's tail is Completed, every other tail is Bypassed, the merge FIRES
	// (Completed) and its downstream runs, and the status map is total (7-set).
	// BITE: making the launch-eligibility count treat a Bypassed tail as taken
	// (drop the depResolved(andDependent) gate) would still fire here (there is
	// always exactly one taken tail) — so the DISCRIMINATING bite for firing is
	// the anti-vacuity test below, not this property. This property guards the
	// happy-path fire + totality + not-taken bypass.
	properties.Property("structured merge fires on the taken tail; others bypassed; map total", prop.ForAll(
		func(k, depth int, predSeed int64) bool {
			probe := newChoiceProbe()
			predBits := predBitsFrom(predSeed, k)
			dag, expected := buildChoiceMergeDAG(t, k, depth, predBits, probe)
			data := NewWorkflowData("cmp")
			if err := dag.Execute(context.Background(), data); err != nil {
				return false
			}
			if st, _ := data.GetNodeStatus("done"); st != Completed { // merge fired
				return false
			}
			if st, _ := data.GetNodeStatus("after"); st != Completed {
				return false
			}
			for j := 0; j < k; j++ {
				for _, name := range choiceBranchNodes(j, depth, -1) {
					st, present := data.GetNodeStatus(name)
					if !present || !the7Statuses[st] {
						return false
					}
					if j == expected {
						if st != Completed {
							return false
						}
					} else if st != Bypassed {
						return false
					}
				}
			}
			return true
		},
		gen.IntRange(2, 4), // K branches (incl. the Otherwise)
		gen.IntRange(1, 3), // branch chain depth
		gen.Int64(),        // predicate-bits seed
	))

	properties.TestingRun(t)
}

// TestMerge_AntiVacuityBite is the hard-bar bite (red-team MAJOR-1): a merge that
// joins ONLY the NOT-taken branches of a Choice, while the Choice took a different
// branch, has zero taken tails -> it is Bypassed. The merge DOES directly depend
// on the (always-Completed) Choice (DEC-M11-DEPMODEL edge added by the validator),
// so firing MUST exclude that structural Choice-dep from the taken count.
//
// BITE: in the launch-eligibility loop (parallel_execution.go), count over the
// full `node.DependsOn` instead of the recorded `mergeAction.tails` set (drop the
// `if !tailSet[dep.Name] { continue }` filter). Then the DEPMODEL Choice-dep
// (Completed, in DependsOn but NOT a From tail) counts as taken -> takenTails
// becomes 1 -> the merge FIRES (Completed) instead of Bypassed -> this test FAILS.
// Verified to falsify. This is the anti-vacuity guard: firing is keyed on a REAL
// taken join tail, never on the structural Choice or any extra dependency.
func TestMerge_AntiVacuityBite(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("antivac")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "taken"). // route takes "taken"
		When(func(*WorkflowData) bool { return false }, "nt1").
		Otherwise("nt2")
	wb.AddNode("taken").WithAction(choiceNoop())
	wb.AddNode("nt1").WithAction(choiceNoop())
	wb.AddNode("nt2").WithAction(choiceNoop())
	// The merge joins ONLY the two NOT-taken branches. route is Completed (took
	// "taken"), nt1/nt2 are Bypassed -> zero taken tails -> merge Bypassed.
	wb.AddMerge("done").From("nt1", "nt2")

	dag, err := wb.Build()
	require.NoError(t, err)

	// Confirm the DEPMODEL edge is present (the anti-vacuity target): the merge
	// directly depends on the always-Completed Choice.
	mergeNode, ok := dag.GetNode("done")
	require.True(t, ok)
	require.True(t, dependsOn(mergeNode, "route"), "the merge must depend on its source Choice (DEPMODEL)")

	data := NewWorkflowData("antivac")
	require.NoError(t, dag.Execute(context.Background(), data))
	assertNodeStatus(t, data, "taken", Completed)
	assertNodeStatus(t, data, "nt1", Bypassed)
	assertNodeStatus(t, data, "nt2", Bypassed)
	// The merge must NOT fire on the Completed Choice — only real taken tails count.
	assertNodeStatus(t, data, "done", Bypassed)
}

// TestMerge_NestingComposes is JOIN-03: an inner choice+merge inside a TAKEN outer
// branch runs and fires normally; the all-bypassed nested case (bypassed outer
// branch) is covered by TestMerge_AllBypassed_IsBypassed.
func TestMerge_NestingComposes(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("nest")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	// Outer choice takes the branch that contains the inner choice+merge.
	wb.AddChoice("oc").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "ic").
		Otherwise("dead")
	wb.AddNode("dead").WithAction(choiceNoop())
	// Inner choice under the taken outer branch.
	wb.AddChoice("ic").
		When(func(*WorkflowData) bool { return true }, "ia").
		Otherwise("ib")
	wb.AddNode("ia").WithAction(choiceNoop())
	wb.AddNode("ib").WithAction(choiceNoop())
	wb.AddMerge("im").From("ia", "ib")
	wb.AddNode("afterInner").DependsOn("im").WithAction(choiceNoop())

	dag, err := wb.Build()
	require.NoError(t, err)
	data := NewWorkflowData("nest")
	require.NoError(t, dag.Execute(context.Background(), data))

	assertNodeStatus(t, data, "dead", Bypassed)
	assertNodeStatus(t, data, "ic", Completed) // inner choice ran
	assertNodeStatus(t, data, "ia", Completed) // inner taken branch
	assertNodeStatus(t, data, "ib", Bypassed)  // inner not-taken
	assertNodeStatus(t, data, "im", Completed) // inner merge fired
	assertNodeStatus(t, data, "afterInner", Completed)
}

// TestMerge_Determinism: the same structured choice-merge routes + fires the same
// across repeated in-process Execute (CHOICE-02 down-payment for the merge).
func TestMerge_Determinism(t *testing.T) {
	predBits := []bool{false, true} // K=3: b0 no, b1 yes -> b1 taken
	var first map[string]NodeStatus
	for run := 0; run < 3; run++ {
		dag, _ := buildChoiceMergeDAG(t, 3, 2, predBits, newChoiceProbe())
		data := NewWorkflowData("cmp")
		require.NoError(t, dag.Execute(context.Background(), data))
		snap := map[string]NodeStatus{}
		data.ForEachNodeStatus(func(name string, st NodeStatus) { snap[name] = st })
		if run == 0 {
			first = snap
		} else {
			assert.Equal(t, first, snap, "merge routing + firing must be deterministic")
		}
	}
}
