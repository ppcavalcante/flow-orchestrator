package workflow

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 41 (M11) T4 — the bite-proven property suite for ChoiceNode routing +
// the cause-aware Bypassed status. Each property runs over RANDOM choice-DAGs
// (random branch count, branch depth, and predicate outcomes) and documents the
// discriminating MUTATION that turns it RED — the anti-vacuity discipline
// ([[demand-bite-proof-when-relabeling-a-formal-property]]).

// the7Statuses is the total status codomain after Execute (INV-02).
var the7Statuses = map[NodeStatus]bool{
	Pending: true, Running: true, Completed: true, Failed: true,
	Skipped: true, Waiting: true, Bypassed: true,
}

// choiceProbe holds a per-node call counter so a property can assert a not-taken
// branch's actions NEVER fire.
type choiceProbe struct{ counts map[string]*atomic.Int32 }

func newChoiceProbe() *choiceProbe { return &choiceProbe{counts: map[string]*atomic.Int32{}} }

func (p *choiceProbe) action(name string) Action {
	c := &atomic.Int32{}
	p.counts[name] = c
	return ActionFunc(func(context.Context, *WorkflowData) error { c.Add(1); return nil })
}

func (p *choiceProbe) fired(name string) int32 { return p.counts[name].Load() }

// choiceBranchNodes returns the node names of branch j at the given depth:
// the entry "b{j}" plus interiors "b{j}_1".."b{j}_{depth}". Branch index k (== K)
// is the Otherwise default, named "bdef...".
func choiceBranchNodes(j, depth, k int) []string {
	entry := fmt.Sprintf("b%d", j)
	if j == k {
		entry = "bdef"
	}
	names := []string{entry}
	for i := 1; i <= depth; i++ {
		names = append(names, fmt.Sprintf("%s_%d", entry, i))
	}
	return names
}

// buildChoiceDAG constructs a choice-DAG: seed -> route(choice) with K When
// branches (indices 0..K-1) plus an Otherwise default (index K). Each branch is a
// chain of `depth` interior nodes after its entry. predBits[j] is branch j's
// (constant) predicate value. Returns the DAG, the probe, and the EXPECTED taken
// branch index (first true predicate, else K = default).
func buildChoiceDAG(k, depth int, predBits []bool, probe *choiceProbe) (*DAG, int, bool) {
	b := NewWorkflowBuilder().WithWorkflowID("choiceprop")
	b.AddStartNode("seed").WithAction(probe.action("seed"))
	ch := b.AddChoice("route").DependsOn("seed")

	addBranch := func(j int) {
		names := choiceBranchNodes(j, depth, k)
		for i, name := range names {
			nb := b.AddNode(name).WithAction(probe.action(name))
			if i > 0 {
				nb.DependsOn(names[i-1]) // chain: interior depends on the previous
			}
		}
	}

	expected := k // default unless a When matches
	for j := 0; j < k; j++ {
		addBranch(j)
		val := predBits[j]
		ch.When(func(*WorkflowData) bool { return val }, fmt.Sprintf("b%d", j))
		if val && expected == k {
			expected = j
		}
	}
	addBranch(k)
	ch.Otherwise("bdef")

	dag, err := b.Build()
	if err != nil {
		return nil, 0, false
	}
	return dag, expected, true
}

func predBitsFrom(seed int64, k int) []bool {
	rng := rand.New(rand.NewSource(seed))
	bits := make([]bool, k)
	for i := range bits {
		bits[i] = rng.Intn(2) == 0
	}
	return bits
}

// TestChoiceProperties is the T4 hard-bar suite.
func TestChoiceProperties(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 300
	params.MaxShrinkCount = 100
	properties := gopter.NewProperties(params)

	// --- Property A: routing + bypass + totality ------------------------------
	// Over any random choice-DAG: exactly the EXPECTED branch's subgraph runs and
	// is Completed; every OTHER branch's subgraph is Bypassed (never Skipped,
	// never Completed) and its actions NEVER fire; the ChoiceNode itself
	// Completes; and the status map is TOTAL (every node present, in the 7-set).
	//
	// BITES:
	//  - classifyBlockedStatus rule 4 `return Bypassed,true` -> `return Skipped,true`:
	//    not-taken INTERIORS become Skipped -> "interior is Bypassed" FALSIFIES.
	//  - choiceAction.Execute drop the `return nil` on first match (take a later
	//    match): the wrong entry Completes -> "expected branch taken" FALSIFIES.
	properties.Property("choice routes exactly one branch; others Bypassed; map total", prop.ForAll(
		func(k, depth int, predSeed int64) bool {
			probe := newChoiceProbe()
			predBits := predBitsFrom(predSeed, k)
			dag, expected, ok := buildChoiceDAG(k, depth, predBits, probe)
			if !ok {
				return false
			}
			data := NewWorkflowData("choiceprop")
			if err := dag.Execute(context.Background(), data); err != nil {
				return false // a fully-defaulted choice-DAG never errors
			}

			// route Completed (a ChoiceNode always Completes, D38-01).
			if st, _ := data.GetNodeStatus("route"); st != Completed {
				return false
			}

			for j := 0; j <= k; j++ {
				taken := j == expected
				for _, name := range choiceBranchNodes(j, depth, k) {
					st, present := data.GetNodeStatus(name)
					if !present || !the7Statuses[st] {
						return false // totality: every node has a status in the 7-set
					}
					if taken {
						if st != Completed || probe.fired(name) != 1 {
							return false
						}
					} else {
						// not-taken: Bypassed, NEVER Skipped/Completed, action never fired
						if st != Bypassed || probe.fired(name) != 0 {
							return false
						}
					}
				}
			}
			return true
		},
		gen.IntRange(2, 4), // K When-branches
		gen.IntRange(0, 3), // branch interior depth
		gen.Int64(),        // predicate-bits seed
	))

	// --- Property B: routing determinism (CHOICE-02 down-payment) -------------
	// The same spec + same data routes the SAME branch across repeated Execute in
	// one process (the full resume-determinism check is phase 43).
	// BITE: make a predicate consult rand instead of predBits -> the taken branch
	// varies across runs -> this FALSIFIES.
	properties.Property("routing is deterministic across repeated Execute", prop.ForAll(
		func(k, depth int, predSeed int64) bool {
			predBits := predBitsFrom(predSeed, k)
			var first map[string]NodeStatus
			for run := 0; run < 3; run++ {
				dag, _, ok := buildChoiceDAG(k, depth, predBits, newChoiceProbe())
				if !ok {
					return false
				}
				data := NewWorkflowData("choiceprop")
				if err := dag.Execute(context.Background(), data); err != nil {
					return false
				}
				snap := map[string]NodeStatus{}
				for j := 0; j <= k; j++ {
					for _, name := range choiceBranchNodes(j, depth, k) {
						st, _ := data.GetNodeStatus(name)
						snap[name] = st
					}
				}
				if run == 0 {
					first = snap
				} else {
					for name, st := range snap {
						if first[name] != st {
							return false
						}
					}
				}
			}
			return true
		},
		gen.IntRange(2, 4),
		gen.IntRange(0, 3),
		gen.Int64(),
	))

	// --- Property C: final statuses survive an FB round-trip ------------------
	// A choice-DAG's final statuses (incl. Bypassed) round-trip through
	// FlatBuffersStore unchanged.
	// BITE (statusToFBStatus): delete the `case Bypassed` -> Bypassed reloads as
	// Pending -> this FALSIFIES (a Bypassed node's status is lost).
	properties.Property("Bypassed statuses survive an FB round-trip", prop.ForAll(
		func(k, depth int, predSeed int64) bool {
			predBits := predBitsFrom(predSeed, k)
			dag, _, ok := buildChoiceDAG(k, depth, predBits, newChoiceProbe())
			if !ok {
				return false
			}
			data := NewWorkflowData("choiceprop")
			if err := dag.Execute(context.Background(), data); err != nil {
				return false
			}
			store, err := NewFlatBuffersStore(t.TempDir())
			if err != nil {
				return false
			}
			if err := store.Save(data); err != nil {
				return false
			}
			got, err := store.Load("choiceprop")
			if err != nil {
				return false
			}
			for j := 0; j <= k; j++ {
				for _, name := range choiceBranchNodes(j, depth, k) {
					want, _ := data.GetNodeStatus(name)
					reloaded, present := got.GetNodeStatus(name)
					if !present || reloaded != want {
						return false
					}
				}
			}
			return true
		},
		gen.IntRange(2, 4),
		gen.IntRange(0, 3),
		gen.Int64(),
	))

	properties.TestingRun(t)
}

// TestChoice_DiamondBelowChoice_NowRejected pins the ph41->ph42 evolution: the
// plain-node reconvergence below a Choice that ph41 tested (a non-merge `join`
// depending on both branches -> Skipped, the D-03 diamond) is now a BUILD ERROR
// under the phase-42 strict reconvergence validator — a reconvergence MUST be a
// MergeNode (D-P42-STRICT(a)). The D-03 diamond RULE itself is unchanged and stays
// bite-covered by the direct classifier test TestCauseAware_Diamond (which does
// not go through Build); the ph42 firing semantics of the structured merge that
// replaces this shape are covered by TestMerge_FiresOnTakenTail.
func TestChoice_DiamondBelowChoice_NowRejected(t *testing.T) {
	wb := NewWorkflowBuilder().WithWorkflowID("choice-diamond")
	wb.AddStartNode("seed").WithAction(choiceNoop())
	wb.AddChoice("route").DependsOn("seed").
		When(func(*WorkflowData) bool { return true }, "big").
		Otherwise("small")
	wb.AddNode("big").WithAction(choiceNoop())
	wb.AddNode("small").WithAction(choiceNoop())
	// A plain node reconverging both branches is an implicit OR-join -> rejected.
	wb.AddNode("join").DependsOn("big", "small").WithAction(choiceNoop())

	_, err := wb.Build()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnstructuredMerge, "a plain reconvergence must now be a MergeNode")
}
