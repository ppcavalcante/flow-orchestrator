package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-012 / C-04: Build folded choice/merge edges by appending them directly into the
// retained NodeBuilder.dependencies slices. A builder is reusable (registry factories,
// tests, and the phase-121 digest all rebuild the same builder), so a SECOND Build
// appended the same generated edges AGAIN -- topology drifted with Build count. Build
// must be pure: the same builder must produce byte-identical definitions every time.
func TestAUD012_RepeatedBuildIsStable(t *testing.T) {
	newBuilder := func() *WorkflowBuilder {
		wb := NewWorkflowBuilder().WithWorkflowID("aud012")
		wb.AddStartNode("seed").WithAction(choiceNoop())
		// A choice routes to two branch entries (each a folded choiceEdge), which then
		// reconverge at a merge (each a folded mergeEdge). This exercises BOTH fold paths.
		wb.AddChoice("route").DependsOn("seed").
			When(func(*WorkflowData) bool { return true }, "a").
			Otherwise("b")
		wb.AddNode("a").WithAction(choiceNoop())
		wb.AddNode("b").WithAction(choiceNoop())
		wb.AddMerge("done").From("a", "b")
		return wb
	}

	// Same builder, built twice: the digest must not move.
	wb := newBuilder()
	dag1, err := wb.Build()
	require.NoError(t, err)
	d1 := dag1.DefinitionDigest()

	dag2, err := wb.Build()
	require.NoError(t, err, "a reusable builder must Build a second time cleanly")
	d2 := dag2.DefinitionDigest()

	require.Equal(t, d1, d2,
		"AUD-012: repeated Build on the SAME builder drifted the definition digest -- generated choice/merge edges were appended into the builder's retained dependency slices")

	// And the merge node's dependency edge count must be identical, not doubled.
	mn1, ok := dag1.GetNode("done")
	require.True(t, ok)
	mn2, ok := dag2.GetNode("done")
	require.True(t, ok)
	require.Equal(t, len(mn1.dependsOn), len(mn2.dependsOn),
		"AUD-012: the merge node's folded tail edges grew on the second Build")

	// A fresh builder must produce the SAME digest as the first build of the reused one
	// (Build is a pure function of the builder's declared state).
	d3 := func() string {
		dag, err := newBuilder().Build()
		require.NoError(t, err)
		return dag.DefinitionDigest()
	}()
	require.Equal(t, d1, d3, "AUD-012: a fresh identical builder must digest identically to the first Build")
}
