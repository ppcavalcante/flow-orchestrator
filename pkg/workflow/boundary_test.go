package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func plainAct() ActionFunc {
	return ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })
}

// dagOf builds a DAG from an edge spec "a->b,b->c" plus any isolated node names.
// Nodes are created on first mention. It goes through the PUBLIC builder, so every
// graph a test asserts on is one a consumer could actually construct.
func dagOf(t *testing.T, isolated []string, edges ...[2]string) *DAG {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID("bt")
	seen := map[string]*NodeBuilder{}
	get := func(n string) *NodeBuilder {
		if nb, ok := seen[n]; ok {
			return nb
		}
		nb := b.AddNode(n).WithAction(plainAct())
		seen[n] = nb
		return nb
	}
	for _, n := range isolated {
		get(n)
	}
	for _, e := range edges {
		get(e[0])
		get(e[1]).DependsOn(e[0])
	}
	dag, err := b.Build()
	require.NoError(t, err, "the fixture graph must build through the public API")
	return dag
}

func check(t *testing.T, dag *DAG, d, v, s string) error {
	t.Helper()
	return validateBoundaries(dag, []boundaryDecl{{doer: d, verifier: v, sink: s}})
}

// ---------------------------------------------------------------------------
// The two refuted classes, as named regression cases with their minimal witnesses.
// ---------------------------------------------------------------------------

// TestBoundary_AF1_SinkWithInDegreeZero_IsRejected is 118-AF1's minimal witness: two
// nodes, NO edges, V and S both roots.
//
// WHY THIS IS THE CASE THAT MATTERS. The one-sentence root-anchored predicate ACCEPTS
// this graph under edge-walking reachability -- no root reaches S along an edge, so
// "every root that reaches S ..." is vacuously true. That is exactly the unsound accept
// root-anchoring was adopted to close, reproduced beneath a green checker. Clause (a)
// exists so this rejection does not depend on anyone remembering to model the
// zero-length path S->S.
func TestBoundary_AF1_SinkWithInDegreeZero_IsRejected(t *testing.T) {
	dag := dagOf(t, []string{"V", "S", "D"})

	err := check(t, dag, "D", "V", "S")

	require.Error(t, err, "118-AF1: S has in-degree 0 and V cannot precede it -- must be REJECTED")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "is a root",
		"the refusal must name WHY (S is a root), not merely refuse")
}

// TestBoundary_AF2_VerifierIsTheUniqueRoot_IsAccepted is 118-AF2's minimal witness: the
// single edge V->S with V the sole root. V genuinely dominates S.
//
// VB-08's graph clause AS ORIGINALLY WRITTEN rejected this, and every one of the 90,448
// over-strict rejects it produced was this shape -- "the validator is also the first
// node", the most natural boundary a consumer will declare.
func TestBoundary_AF2_VerifierIsTheUniqueRoot_IsAccepted(t *testing.T) {
	dag := dagOf(t, nil, [2]string{"V", "S"})

	require.NoError(t, check(t, dag, "V", "V", "S"),
		"118-AF2: V is the unique root reaching S and genuinely dominates it -- must be ACCEPTED")
}

// ---------------------------------------------------------------------------
// The predicate proper.
// ---------------------------------------------------------------------------

// THE PER-CLAUSE MUTATION MATRIX, AND WHY IT IS RECORDED HERE RATHER THAN IN A REPORT.
//
// The arms below assert MESSAGES, not just verdicts -- VB-02 requires a typed error
// NAMING THE OFFENDING PATH, so "an error occurred" is not the property. A message arm
// can rot into vacuity without any verdict changing, and 118-F4 changed reachAvoiding's
// signature underneath these arms, which is exactly the kind of edit that does it.
//
// So the matrix was re-run against the current predicate. Each mutation was applied
// ALONE in a throwaway worktree and reverted; scored by the failure MESSAGE, because a
// mutation that reds some arm is not evidence that the arm aimed at it noticed.
//
//	mutation                                     FAILs  message observed
//	-------------------------------------------  -----  ---------------------------------
//	M1  clause (0) V!=S disabled                     1  expected error ... got nil
//	M2  clause (a) S-is-root disabled                2  falls through to "no path from doer"
//	M3  anti-vacuity disabled                        1  expected error ... got nil
//	M4  clause (b) drops the R==V exemption          0  SURVIVOR -- see below
//	M5  clause (b) avoid=nil (the 118-F4 axis)       5  "A -> V -> S reaches sink ..."
//	M6a renderPath truncated to the root only        4  "B reaches sink ..."   (path gone)
//	M6b renderPath reversed                          4  "S -> X -> B reaches sink ..." (order)
//	M7  action clause never opaque                   7  "Should be true"
//
// THE RESULT: the message arms BITE. 118-F4's signature change did not leave them
// vacuous -- passing nil for avoid reds 5 arms, and DEGRADING THE RENDERED PATH reds 4
// either way. Truncation and reversal were run separately on purpose: together they
// establish that the assertion is on the path's CONTENT and its ORDER, which is what
// VB-02 actually requires. A single "path missing" mutation could not have shown that.
//
// M2 is the one worth reading twice. Disabling clause (a) reds two arms ON THE MESSAGE,
// because the refusal falls through to anti-vacuity and says the wrong thing. That is
// "anti-vacuity carries the soundness, (a) is diagnostic" re-derived from the opposite
// direction -- by mutation here, by per-clause firing counters in the review.
//
// 🔴 M4 IS AN EQUIVALENT MUTANT, NOT A BLIND CELL, and the distinction is the whole
// value of scoring a matrix rather than a count. Removing `if name == d.verifier {
// continue }` from clause (b) changes no verdict because reachAvoiding OPENS with
// `if avoid != nil && from == *avoid { return nil }` -- the walk already refuses to
// start at the avoided node. TWO INDEPENDENT SIGNALS AGREE: 0 of 111 arms distinguish
// the two trees, and the early return explains why. A survivor with a mechanism is a
// diagnosis; a survivor without one is an untested cell, and they must not be reported
// alike. The `continue` is kept deliberately -- see the note at its site in boundary.go.

// TestBoundary_MultiRootBypass_IsRejectedWithTheOffendingPath is where rejection
// actually bites: a second root reaching S on a branch V does not sit on. VB-02
// requires the error to NAME the offending path, so this asserts the rendered path,
// not merely that an error occurred.
func TestBoundary_MultiRootBypass_IsRejectedWithTheOffendingPath(t *testing.T) {
	// A -> V -> S  and  B -> X -> S. B's route bypasses V.
	dag := dagOf(t, nil,
		[2]string{"A", "V"}, [2]string{"V", "S"},
		[2]string{"B", "X"}, [2]string{"X", "S"})

	err := check(t, dag, "A", "V", "S")

	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "B -> X -> S",
		"VB-02 requires a CONCRETE offending path in the refusal; got: %v", err)
}

func TestBoundary_AllRoutesThroughVerifier_IsAccepted(t *testing.T) {
	// A -> V -> S and B -> V -> S: both roots route through V.
	dag := dagOf(t, nil,
		[2]string{"A", "V"}, [2]string{"B", "V"}, [2]string{"V", "S"})

	require.NoError(t, check(t, dag, "A", "V", "S"))
}

func TestBoundary_VerifierEqualsSink_IsRejected(t *testing.T) {
	dag := dagOf(t, nil, [2]string{"D", "S"})
	err := check(t, dag, "D", "S", "S")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "same node")
}

// ---------------------------------------------------------------------------
// The graph anti-vacuity clause (DEC-M23-VB08-R2) and name resolution.
// ---------------------------------------------------------------------------

// TestBoundary_DoerWithNoPathToSink_IsRejected: D must be an ancestor of S, or the
// declaration quantifies over nothing. This clause never consults dominance, so it
// cannot reproduce 118-AF2 -- TestBoundary_AF2... above is the control that proves it.
func TestBoundary_DoerWithNoPathToSink_IsRejected(t *testing.T) {
	// D is off to one side; A -> V -> S is the real chain.
	dag := dagOf(t, []string{"D"}, [2]string{"A", "V"}, [2]string{"V", "S"})

	err := check(t, dag, "D", "V", "S")

	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "no path from doer")
}

func TestBoundary_UnknownNodeName_IsRejected(t *testing.T) {
	dag := dagOf(t, nil, [2]string{"D", "S"})
	for _, c := range []struct{ d, v, s, want string }{
		{"nope", "D", "S", "doer"},
		{"D", "nope", "S", "verifier"},
		{"D", "D", "nope", "sink"},
	} {
		err := check(t, dag, c.d, c.v, c.s)
		require.ErrorIs(t, err, ErrValidation)
		require.Contains(t, err.Error(), c.want)
		require.Contains(t, err.Error(), "not a node of this workflow")
	}
}

// ---------------------------------------------------------------------------
// The action clause, through the public builder.
// ---------------------------------------------------------------------------

// TestBoundary_OpaqueVerifierOrSink_IsRejectedThroughTheBuilder drives the VB-08 action
// clause on a graph a consumer really builds, rather than on a hand-made action value.
func TestBoundary_OpaqueVerifierOrSink_IsRejectedThroughTheBuilder(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("bt-op")
	b.AddNode("D").WithAction(plainAct())
	b.AddTimer("T", 0).DependsOn("D")
	b.AddNode("S").WithAction(plainAct()).DependsOn("T")
	dag, err := b.Build()
	require.NoError(t, err)

	err = check(t, dag, "D", "T", "S")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "timerAction")
	require.Contains(t, err.Error(), "clock alone",
		"the refusal must carry the criterion's reason; got: %v", err)
}

// TestBoundary_CheckOrderIsCheapestFirst pins the ordering that keeps a specific
// refusal from being replaced by a general one (a guard before a cheaper check
// substitutes its error). A declaration that violates BOTH the action clause and the
// predicate must report the ACTION clause.
func TestBoundary_CheckOrderIsCheapestFirst(t *testing.T) {
	// S is a root (violates clause (a)) AND is a merge-shaped opaque kind... use a
	// timer as the sink, isolated so it is also a root.
	b := NewWorkflowBuilder().WithWorkflowID("bt-ord")
	b.AddNode("D").WithAction(plainAct())
	b.AddNode("V").WithAction(plainAct()).DependsOn("D")
	b.AddTimer("S", 0) // a root AND an opaque kind
	dag, err := b.Build()
	require.NoError(t, err)

	err = check(t, dag, "D", "V", "S")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "timerAction",
		"the action clause is checked before the predicate, so the more specific "+
			"refusal must win; got: %v", err)
	require.NotContains(t, err.Error(), "is a root")

	// J1's position in the same order (VB-07). J1 sits AHEAD of the action clause because
	// it is the cheapest refusal in that function -- one bool field read against a kind
	// classification plus an allocation. A verifier that is BOTH an opaque kind and
	// continue-on-error violates both clauses; the cheaper, more specific one must answer.
	//
	// It is NOT ordered first to avoid a mutation. An earlier draft said so and that was
	// false: build() allocates a fresh node and snapshotBoundaryAction allocates its
	// result, so the write lands on a DAG Build is about to discard. See the note at the
	// clause, and TestBoundary_J1_RefusalLeavesTheBuilderReusable.
	t.Run("J1 is ordered before the action clause", func(t *testing.T) {
		b := NewWorkflowBuilder().WithWorkflowID("bt-ord-j1")
		b.AddNode("D").WithAction(plainAct())
		b.AddNode("V2").WithAction(plainAct()).DependsOn("D")
		b.AddTimer("V", 0).DependsOn("D").WithContinueOnError() // opaque kind AND the flag
		b.AddNode("S").WithAction(plainAct()).DependsOn("V")
		dag, err := b.Build()
		require.NoError(t, err)

		err = check(t, dag, "D", "V", "S")
		require.ErrorIs(t, err, ErrValidation)
		require.Contains(t, err.Error(), "ContinueOnError",
			"J1 is the cheaper, non-mutating clause and must answer; got: %v", err)
		require.NotContains(t, err.Error(), "timerAction",
			"J1 is the cheaper, more specific refusal and must not be replaced by the more "+
				"general action clause; got: %v", err)
	})
}

// ---------------------------------------------------------------------------
// Anti-vacuity of this file itself.
// ---------------------------------------------------------------------------

// TestBoundary_NoDeclarationsIsAccepted is the control. Without it every test above
// would still pass if validateBoundaries returned an error unconditionally... and the
// converse control is that the accept-cases above are absolute require.NoError calls,
// so a validateBoundaries that returned nil unconditionally reds every reject-case.
func TestBoundary_NoDeclarationsIsAccepted(t *testing.T) {
	dag := dagOf(t, nil, [2]string{"A", "B"})
	require.NoError(t, validateBoundaries(dag, nil))
}

// TestBoundary_RefusalNamesAConcretePath is the shape check VB-02 asks for: the
// rendered path must be a real walk, not a set.
func TestBoundary_RefusalNamesAConcretePath(t *testing.T) {
	dag := dagOf(t, nil,
		[2]string{"A", "V"}, [2]string{"V", "S"},
		[2]string{"B", "M"}, [2]string{"M", "N"}, [2]string{"N", "S"})

	err := check(t, dag, "A", "V", "S")
	require.Error(t, err)

	// Extract exactly the rendered path: the segment before " reaches sink".
	msg := err.Error()
	i := strings.Index(msg, " reaches sink")
	require.GreaterOrEqual(t, i, 0, "no rendered path in %v", err)
	got := msg[strings.LastIndex(msg[:i], ": ")+2 : i]

	require.Equal(t, "B -> M -> N -> S", got,
		"the refusal must render ONE concrete walk from the offending root to the sink, "+
			"avoiding the verifier -- not a set and not a summary")
	require.NotContains(t, got, "V", "the offending path must AVOID the verifier; got %q", got)
	require.True(t, errors.Is(err, ErrValidation))
}

// ---------------------------------------------------------------------------
// T2 — the builder surface and the build() seat.
// ---------------------------------------------------------------------------

// TestBoundary_WithBoundary_RefusedAtBuild is the end-to-end property Q clause 2 asks
// for: a consumer declares a boundary the graph does not satisfy, and Build REFUSES
// with a typed error naming a concrete offending path.
func TestBoundary_WithBoundary_RefusedAtBuild(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("e2e-bad")
	b.AddNode("A").WithAction(plainAct())
	b.AddNode("V").WithAction(plainAct()).DependsOn("A")
	b.AddNode("B").WithAction(plainAct())
	b.AddNode("S").WithAction(plainAct()).DependsOn("V", "B")
	b.WithBoundary("A", "V", "S")

	dag, err := b.Build()

	require.Nil(t, dag, "a refused declaration must not yield a DAG")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "B -> S",
		"Build's refusal must name the concrete offending path; got: %v", err)
}

// TestBoundary_WithBoundary_AcceptedAtBuild is the control: the same shape with B
// routed through V builds clean and stamps the token.
func TestBoundary_WithBoundary_AcceptedAtBuild(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("e2e-ok")
	b.AddNode("A").WithAction(plainAct())
	b.AddNode("B").WithAction(plainAct())
	b.AddNode("V").WithAction(plainAct()).DependsOn("A", "B")
	b.AddNode("S").WithAction(plainAct()).DependsOn("V")
	b.WithBoundary("A", "V", "S")

	dag, err := b.Build()

	require.NoError(t, err)
	require.True(t, dag.built, "an accepted declaration must still stamp the builder token")
	require.True(t, dag.hasBoundaries)
	require.Len(t, dag.boundaries, 1)
}

// TestBoundary_NoDeclaration_LeavesTheMoatClosed is the zero-determinism-tax arm: a
// workflow declaring no boundary must carry hasBoundaries=false, the gate every
// boundary cost is behind.
func TestBoundary_NoDeclaration_LeavesTheMoatClosed(t *testing.T) {
	dag := dagOf(t, nil, [2]string{"A", "B"})
	require.False(t, dag.hasBoundaries)
	require.Empty(t, dag.boundaries)
}

// TestBoundary_ValidatedAfterReconvergenceAppendsItsEdges pins the SEAT, which is the
// half of T2 that is easy to get wrong and impossible to see afterwards.
//
// validateReconvergence does not merely check -- it APPENDS the DEC-M11-DEPMODEL
// merge<-choice edges as its final act. A dominance predicate evaluated before it runs
// is evaluated against a graph that THEN GAINS EDGES, and adding edges can only add
// routes, so a declaration can pass on the pre-append graph and be false on the one
// that actually executes.
//
// A DISCRIMINATING GRAPH EXISTS AND THIS IS IT (the plan asked for one, or an honest
// report that none does). Single-branch choice: D -> C -> x -> M -> S. Measured, after
// Build: M.dependsOn == [x C] -- the C edge is the appended one.
//
//	pre-append  routes to S: D->C->x->M->S              => x dominates S  => ACCEPT
//	post-append routes to S: D->C->x->M->S, D->C->M->S  => the second bypasses x => REJECT
//
// The sink is S rather than M because a mergeAction is refused as a sink by the VB-08
// action clause -- the action clause runs first, so declaring (D,x,M) would report THAT
// refusal and never reach the predicate.
//
// So the verdict FLIPS on the seat. The refusal below is the post-append answer, which
// is the correct one; boundary_seat_bite documents the pre-append run.
func TestBoundary_ValidatedAfterReconvergenceAppendsItsEdges(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("seat")
	b.AddStartNode("D").WithAction(plainAct())
	ch := b.AddChoice("C").DependsOn("D")
	b.AddNode("x").WithAction(plainAct())
	ch.Otherwise("x")
	b.AddMerge("M").From("x")
	b.AddNode("S").WithAction(plainAct()).DependsOn("M")
	b.WithBoundary("D", "x", "S")

	_, err := b.Build()

	require.ErrorIs(t, err, ErrValidation,
		"the appended merge<-choice edge gives C a direct route to M that bypasses x, "+
			"so this declaration is FALSE on the graph that executes")
	require.Contains(t, err.Error(), "C -> M -> S",
		"the refusal must name the appended edge's route; got: %v", err)
}
