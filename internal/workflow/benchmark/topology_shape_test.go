package benchmark

import (
	"context"
	"fmt"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// THE MIGRATION'S ONE SILENT-FAILURE MODE, pinned.
//
// M23 SEAL-06 moved the three topology helpers off NewDAG/AddNode/AddDependency and
// onto the builder. That inverts how every edge is written: AddDependency(from, to)
// says "to depends on from" and is parent-first, while AddNode(child).DependsOn(parent)
// is child-first. So each loop had to be restated as "each node's parents" where it
// used to say "each node's children".
//
// Get that inversion wrong and NOTHING in this package notices. The reversed graph has
// the same node count, the same edge count, is still acyclic, still builds, still
// executes to completion, and still produces a benchmark number — it is simply the
// wrong shape. No benchmark asserts topology, because a benchmark's job is to be fast,
// not to be right.
//
// So the expectations below are derived from the SHAPE EACH HELPER IS NAMED FOR, not
// transcribed from the code they check. A linear chain is n levels of one node; a
// diamond is source / band / sink; a binary tree puts node j at depth floor(log2(j+1)).
// If the expectation were read off the implementation it would agree with a reversed
// implementation just as happily.
//
// BITE, RUN AND READ END TO END. Flipping createLinearDAG's edge to
// `nb.DependsOn(fmt.Sprintf("node%d", i+1))` — the parent-first direction the old code
// was written in, i.e. the exact transcription error this test exists to catch — reds
// the linear arm with:
//
//	Error:    Not equal:
//	          expected: []string{"node0"}
//	          actual  : []string{"node4"}
//	Messages: linear(5): level 0 must hold exactly node0 — a REVERSED chain has the
//	          same level count and fails only here
//
// THE LEVEL-COUNT ASSERTION STAYS GREEN under that seed — require.Len(levels, 5)
// passes on the reversed chain, because reversing a chain of 5 gives a chain of 5.
// Only the per-level content assertion reds. That is the whole design: a shape test
// that checked counts would have certified the reversed graph.
func TestBenchTopologies_ShapeIsPreserved(t *testing.T) {
	// names renders a level as node names, so a failure prints the shape rather than
	// a pointer soup.
	names := func(level []*workflow.Node) []string {
		out := make([]string, 0, len(level))
		for _, n := range level {
			out = append(out, n.Name())
		}
		return out
	}

	t.Run("linear is a chain of single-node levels", func(t *testing.T) {
		const size = 5
		levels := createLinearDAG(size).GetLevels()
		require.Len(t, levels, size, "linear(%d): a chain has one level per node", size)
		for i, lv := range levels {
			require.Equal(t, []string{fmt.Sprintf("node%d", i)}, names(lv),
				"linear(%d): level %d must hold exactly node%d — a REVERSED chain has the "+
					"same level count and fails only here", size, i, i)
		}
	})

	t.Run("diamond is source, band, sink", func(t *testing.T) {
		const size = 6 // node0 source, node1..node4 band, node5 sink
		levels := createDiamondDAG(size).GetLevels()
		require.Len(t, levels, 3, "diamond(%d): source / fan-out band / sink", size)
		require.Equal(t, []string{"node0"}, names(levels[0]))
		require.ElementsMatch(t,
			[]string{"node1", "node2", "node3", "node4"}, names(levels[1]),
			"the band must fan out from the source; order within a level is not a property")
		require.Equal(t, []string{"node5"}, names(levels[2]),
			"the sink must fan back in, i.e. depend on every band node")
	})

	t.Run("binary tree puts node j at depth floor(log2(j+1))", func(t *testing.T) {
		const size = 7 // a full tree of depth 3: [0], [1,2], [3,4,5,6]
		levels := createBinaryTreeDAG(size).GetLevels()
		require.Len(t, levels, 3, "binaryTree(%d): a full tree of 7 nodes is 3 deep", size)
		require.Equal(t, []string{"node0"}, names(levels[0]))
		require.ElementsMatch(t, []string{"node1", "node2"}, names(levels[1]))
		require.ElementsMatch(t, []string{"node3", "node4", "node5", "node6"}, names(levels[2]))
	})
}

// dagSpec.dep carries the ONE inversion the whole benchmark migration rests on, so it
// gets its own test rather than being trusted because it is short.
//
// The claim under test is a translation, and translations fail silently: dep(from, to)
// must mean exactly what dag.AddDependency(from, to) meant — "to depends on from" —
// even though the builder underneath wants the opposite order. If dep swapped its
// arguments, every migrated helper would produce a reversed graph, every benchmark
// would still run, and the only visible effect would be numbers that moved for a
// reason nobody could name.
//
// The assertion is deliberately asymmetric (a strictly before b), because a symmetric
// fixture is exactly the fixture that cannot tell the two directions apart.
//
// BITE: swapping dep's body to `s.parents[from] = append(s.parents[from], to)` reds
// this with expected ["a"] / actual ["b"] on level 0 — "dep(from,to) must mean 'to
// depends on from'". The three topology arms above stay GREEN under that same seed,
// because they were written against dagSpec's stated contract; only this test reads
// the contract itself.
func TestDAGSpec_DepMeansToDependsOnFrom(t *testing.T) {
	noop := workflow.ActionFunc(func(ctx context.Context, d *workflow.WorkflowData) error { return nil })

	s := newDAGSpec("dep-direction").node("a", noop).node("b", noop)
	s.dep("a", "b") // b depends on a => a runs first

	levels := s.build().GetLevels()
	require.Len(t, levels, 2, "one edge between two nodes must produce two levels")
	require.Equal(t, []string{"a"}, namesOf(levels[0]),
		"dep(from,to) must mean 'to depends on from': a is the dependency, so it runs first")
	require.Equal(t, []string{"b"}, namesOf(levels[1]))
}

// namesOf renders a level as node names.
func namesOf(level []*workflow.Node) []string {
	out := make([]string, 0, len(level))
	for _, n := range level {
		out = append(out, n.Name())
	}
	return out
}
