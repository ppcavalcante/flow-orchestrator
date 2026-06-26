package workflow

// Mutation-kill test (M8 Phase B, chunk 6 part B): DAG.Validate populates the
// PUBLIC StartNodes / EndNodes fields, but no existing test asserted their
// CONTENTS — so the gremlins mutant on dag.go:177 (`len(node.DependsOn) == 0`
// -> `!= 0`) and its EndNodes companion (dag.go:181 `!hasDependents[name]`)
// survived: a caller reading those fields could be silently mis-served and the
// suite would not notice. These are public surface, so their correctness is a
// real contract. This test pins both sets on a known DAG.

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// names extracts and sorts the names of a []*Node for set comparison.
func names(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out
}

// TestValidate_StartAndEndNodes pins the contents of DAG.StartNodes (nodes with
// no dependencies) and DAG.EndNodes (nodes nothing depends on) after Validate.
//
// DAG shape (edges point from dependency -> dependent):
//
//	a ─┐
//	   ├─> c ─> d
//	b ─┘
//	e (isolated)
//
// Start nodes (no deps): a, b, e.  End nodes (no dependents): d, e.
func TestValidate_StartAndEndNodes(t *testing.T) {
	dag := NewDAG("start-end")
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		mustAddNode(t, dag, NewNode(name, ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	}
	// c depends on a and b; d depends on c; e is isolated.
	mustAddDep(t, dag, "a", "c")
	mustAddDep(t, dag, "b", "c")
	mustAddDep(t, dag, "c", "d")

	require.NoError(t, dag.Validate())

	// StartNodes = nodes with len(DependsOn)==0. The mutant ==0 -> !=0 would make
	// this the COMPLEMENT (c, d), so asserting the exact set kills it.
	assert.Equal(t, []string{"a", "b", "e"}, names(dag.StartNodes),
		"StartNodes must be exactly the dependency-free nodes")

	// EndNodes = nodes nothing depends on. The mutant on the !hasDependents guard
	// would flip this set too.
	assert.Equal(t, []string{"d", "e"}, names(dag.EndNodes),
		"EndNodes must be exactly the nodes with no dependents")
}
