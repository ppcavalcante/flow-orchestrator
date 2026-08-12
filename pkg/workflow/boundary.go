package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// boundaryDecl is one consumer-declared (doer, verifier, sink) triple, appended by
// WithBoundary and folded in build(). It is the choiceEdges/mergeEdges shape.
type boundaryDecl struct{ doer, verifier, sink string }

// THE ONE SENTENCE THIS PROPERTY MUST NEVER BE RESTATED AS.
//
// The property is scoped to CONTROL FLOW. Do not restate it at the effect scope — "no
// doer's effect reaches S unverified" is a strictly wider claim, it is FALSE at that
// scope, and restating it that way is the 116-AF9 family (a locally-true statement
// widened). boundary_claimscope_test.go reds if the contract below drifts that way; it
// exists because getting the sentence right once is worth less than a check that
// re-runs.
//
// claimscope:prohibition — this block QUOTES the banned phrasing in order to rule it
// out, so the scope-phrasing guard skips it.
//
// THE MARKER ON THIS BLOCK AND ON THE ONE BELOW IS LOAD-BEARING. It was INERT when
// written -- neither block contains the substring "boundar", which the trigger then
// required as a MANDATORY conjunct -- and 118-D2 replaced that conjunct with a
// DISJUNCTION over role vocabulary, which made both blocks reachable. CONFIRMED BY
// EXECUTION rather than inferred: the claim-scope run now lists boundary.go:13 and
// boundary.go:40 among its ALLOWED blocks, and listed neither before. Delete either
// marker and this file reds.
//
// 🔴 THIS BLOCK IS DELIBERATELY SEPARATE FROM THE CONTRACT BELOW, and the separation is
// load-bearing rather than cosmetic. The opt-out suppresses the guard for a WHOLE
// COMMENT BLOCK. While the prohibition and the contract were one block, everything in
// the contract — including the 118-F3 sentence, the one most likely to be widened —
// was exempt from the very check it was written for. An opt-out that grows to cover the
// text it was meant to protect is how a guard becomes advisory. Keep this block SMALL:
// it may hold only sentences that must quote the banned phrasing in order to forbid it.

// THE PATH UNIVERSE, and why the word "effect" appears here at all.
//
// claimscope:prohibition — this block names the EFFECT relation in order to EXCLUDE it.
//
// 🔴 THE PATH UNIVERSE IS `dependsOn` AND NOTHING ELSE — DEC-M23-COMP-UNIVERSE.
// COMPENSATION EDGES ARE EXCLUDED, BY DECLARATION RATHER THAN BY OMISSION. `successors`
// below is built only from `dependsOn`, so a compensation never appears in any path the
// predicate quantifies over. Three reasons, and the third is the binding one:
//
//  1. The property is DIRECTIONAL. Precedence(V,S) is over the FORWARD relation, and
//     dependsOn is that relation; a rollback runs in reverse.
//  2. Compensation NEVER INVOKES S. It invokes n.compensation, a distinct action —
//     during a rollback S's own action does not run at all, and undoing S is not S
//     occurring.
//  3. Including compensation would model an EFFECT relation, which DEC-M23-NAMING bars
//     M23 from claiming. Widening the universe would therefore not strengthen the
//     guarantee — IT WOULD MAKE THE GUARANTEE'S STATED SCOPE FALSE.
//
// An exclusion visible only as the ABSENCE of a word is not checkable by a reader, which
// is why it is written down rather than left to be inferred from `successors`. The
// residual is filed, not absorbed: a compensation on a node between V and S can reach
// the sink without V having run on that drive (F118-COMP-01, M24).

// WHAT A DECLARED BOUNDARY MEANS — stated in the PRECEDENCE form, which is the only
// form M23 proves (DEC-M23-NAMING). EVERYTHING FROM HERE DOWN IS CHECKED by
// boundary_claimscope_test.go.
//
//	A boundary (D, V, S) asserts Precedence(V, S): along every route the executor can
//	take through the built graph, S does not occur before V.
//
// That is exactly `V dom S` over the built graph. AUD-020: it is NOT order ALONE. The
// declaration ALSO constrains V to be a GATE rather than a bare ordering marker — clause
// J1 below refuses a verifier carrying ContinueOnError, because such a V's Failed status
// resolves as satisfied and S would then launch on V's FAILURE as readily as on its
// success. With J1 in force, S launches only when V did not fail: V can WITHHOLD the sink.
// So a boundary is a SUCCESS-gated precedence — order (S never before V) PLUS the gate
// capability (V's failure withholds S). An earlier form of this line said "and NOTHING
// MORE", which read as pure order and contradicted J1 (the audit's C-20/AUD-020). Both
// halves are CONTROL-FLOW claims over the built graph and the executor's traversal; the
// boundary still says nothing about what V's action computed from D's work — that relation
// stays out of M23's scope (DEC-M23-NAMING), which is why the strengthening is phrased as
// a gate on V's own run status, not as V standing for D.
//
// D IS NOT LOAD-BEARING IN THE PREDICATE, and that is intended rather than an
// oversight: root-anchoring quantifies over ROOTS, not over D, so D appears only in
// the anti-vacuity clause below. V may legitimately run BEFORE D (whenever V dom D)
// and the boundary still holds.
//
// THE PREDICATE, in the two clauses DEC-M23-DOERSET-R2 was amended into. It is stated
// as two branches rather than one loop ON PURPOSE — see the note on clause (a).
//
//	(0) V != S. A sink that is its own verifier constrains nothing.
//	(a) If S is a ROOT (len(S.dependsOn) == 0) — REJECT. Nothing can precede S.
//	(b) Otherwise, for every root R that reaches S: R == V, or every R->S path
//	    contains V.
//
// 🔴 THE PROPERTY IS ABOUT THE EXECUTOR'S TRAVERSAL, AND A NODE'S STATUS IS NOT
// PROTECTED — 118-F3. "S does not occur before V" quantifies over the routes the
// EXECUTOR can take through the built graph. It does NOT say a consumer cannot make S
// LOOK done: SetNodeStatus is exported, the *WorkflowData is shared with every action,
// and the executor skips a node that is already terminal (parallel_execution.go). So a
// doer can mark the sink Completed and the sink's action never runs, with no DAG and no
// boundary involved. The control-flow claim survives literally — the executor did not
// run S before V, it did not run S at all — but nothing here defends the bridge from
// that claim to "the sink did not happen". Status forgery is a separate channel, routed
// to security; the point of writing it down is that this is the sentence most likely to
// be restated one notch too wide, and one notch too wide is the 116-AF9 family.
//
// WHY (a) IS ITS OWN BRANCH — AND WHAT IT IS NOT. The one-sentence form of this
// predicate ("every root that reaches S is either V or has all its S-paths through V")
// is UNSOUND under edge-walking reachability, which is the only kind an implementation
// has: 118-AF1's witness is two nodes and no edges, V and S both roots, so no root
// reaches S along an edge, the quantifier is vacuously true, and the predicate ACCEPTS.
// Making S-is-a-root an explicit branch removes the dependence on a reflexive-closure
// convention an implementer must remember to seed.
//
// 🔴 BUT CLAUSE (a) DOES NOT CLOSE 118-AF1, AND EARLIER ARTIFACTS SAID IT DID.
// MEASURED, with per-clause firing counters over the corpus: dropping (a) IN THE SHIPPED
// ORDER changes ZERO verdicts, because the anti-vacuity clause below absorbs those
// inputs downstream regardless. ANTI-VACUITY CARRIES THE SOUNDNESS; (a) IS A DIAGNOSTIC
// BRANCH — it fires 41,210 times and gives the pointed message instead of a true but
// misleading one, which is why it is ordered first and why that ordering stays. The
// correction matters because the false version is load-bearing in the dangerous
// direction: someone relaxing anti-vacuity in the belief that (a) is the real guard
// would remove the actual soundness check and be left with a branch that still fires and
// a test that still passes.
//
// A USEFUL CONSEQUENCE OF ORDERING (a) BEFORE (b): after (a), S is not a root, so no
// root can BE S, so every R->S route found in (b) already has at least one edge. The
// "along at least one edge" qualifier needs no separate check.
//
// validateBoundaries reports the FIRST declaration the built graph does not satisfy,
// as an ErrValidation naming a concrete offending root->S path (VB-02). It is called
// from build() after validateReconvergence has appended its merge<-choice edges and
// before the built stamp, because dominance evaluated any earlier is evaluated against
// a graph that then gains edges.
func validateBoundaries(dag *DAG, decls []boundaryDecl) error {
	if len(decls) == 0 {
		return nil
	}
	succ := successors(dag)
	for _, d := range decls {
		if err := validateBoundary(dag, succ, d); err != nil {
			return err
		}
	}
	return nil
}

// validateBoundary checks one declaration. ORDER IS LOAD-BEARING: a cheaper, more
// specific refusal must not be replaced by a more general one. The order is
// resolution -> J1 (the verifier's ContinueOnError flag) -> action clause -> predicate
// (0) and (a) -> anti-vacuity -> predicate (b). One of those positions is derived at its
// site rather than chosen: (a) sits ahead of anti-vacuity, which is the difference
// between clause (a) being reachable and being dead. J1 sits ahead of the action clause
// on cost alone -- see the note at that clause, and read it for what it does NOT claim.
func validateBoundary(dag *DAG, succ map[string][]string, d boundaryDecl) error {
	// 1. All three names resolve to nodes of this graph.
	doer, verifier, sink := dag.nodes[d.doer], dag.nodes[d.verifier], dag.nodes[d.sink]
	for _, r := range []struct {
		role, name string
		node       *Node
	}{
		{"doer", d.doer, doer},
		{"verifier", d.verifier, verifier},
		{"sink", d.sink, sink},
	} {
		if r.node == nil {
			return fmt.Errorf("%w: boundary (%s, %s, %s): %s %q is not a node of this workflow",
				ErrValidation, d.doer, d.verifier, d.sink, r.role, r.name)
		}
	}

	// 2. J1 (VB-07). THE VERIFIER MAY NOT CARRY ContinueOnError.
	//
	// WHY THIS IS A REFUSAL, stated in the control-flow scope and no wider.
	// ContinueOnError changes the DEPENDENCY RESOLUTION RULE the executor applies to a
	// node: depResolved (parallel_execution.go) reports a continue-on-error dependency
	// that Failed as RESOLVED. So a sink depending on such a verifier launches on the
	// verifier's FAILURE exactly as it does on its success. Precedence over the built
	// graph still holds -- the executor still reaches the verifier first -- but the
	// declaration then constrains ORDER ALONE, and a consumer writing WithBoundary is
	// declaring more than an order. Refuse it at Build rather than accept a declaration
	// whose verifier cannot withhold the sink.
	//
	// SCOPE: structural over the built graph, like every clause in this function. It is
	// a property of the DECLARED FLAG, not of anything a node's action does at run time.
	//
	// PLACEMENT, decided and pinned rather than incidental. It sits AFTER name resolution
	// because before it `verifier` may be nil, and BEFORE the action clause because it is
	// the CHEAPEST refusal in this function -- one bool field read, no traversal, no
	// allocation. Cheapest-first is the whole argument and it is sufficient on its own.
	// Pinned by the J1 arm of TestBoundary_CheckOrderIsCheapestFirst.
	//
	// 🔴 AND HERE IS WHAT THIS PLACEMENT DOES NOT CLAIM, because an earlier draft claimed
	// it and it is FALSE (found by review). That draft argued J1 must precede the action
	// clause because the action clause MUTATES -- it assigns snapshotBoundaryAction's
	// result over the node's action -- so a graph about to be refused would be "mutated on
	// the way out". The mutation is real and it is UNOBSERVABLE:
	//
	//	build() allocates a FRESH *Node per build (builder.go, newNodeWithCapacity) from
	//	the NodeBuilder's own action value, and snapshotBoundaryAction ALLOCATES its
	//	result rather than editing in place. So the write lands on a node belonging to a
	//	DAG that Build is about to discard, and the builder's action is never touched.
	//
	// Pinned by TestBoundary_J1_RefusalLeavesTheBuilderReusable, so this correction is
	// something that re-runs rather than a comment that can quietly rot back.
	if verifier.continueOnError {
		return fmt.Errorf("%w: boundary (%s, %s, %s): verifier %q carries ContinueOnError, so a "+
			"sink depending on it launches on its FAILURE as well as on its success -- the "+
			"declaration would constrain order alone",
			ErrValidation, d.doer, d.verifier, d.sink, d.verifier)
	}

	// 3. Action clause (VB-08, DEC-M23-VB08-R3). V and S must be node kinds whose
	//    Completed status implies an attributable act occurred AT that node.
	for _, r := range []struct {
		role string
		node *Node
	}{{"verifier", verifier}, {"sink", sink}} {
		if why, opaque := boundaryOpaqueReason(r.node.action); opaque {
			return fmt.Errorf("%w: boundary (%s, %s, %s): %s %q is a %s and may not be a boundary %s -- %s",
				ErrValidation, d.doer, d.verifier, d.sink,
				r.role, r.node.name, actionKindName(r.node.action), r.role, why)
		}
		// 🔴 118-D4: SNAPSHOT WHAT WAS JUST VALIDATED. CompositeAction.Add is exported
		// and appends to the slice the built DAG holds by pointer, so without this the
		// check above certifies an action the consumer can still change afterwards --
		// validated-then-mutated, on a graph already stamped as built. Mutating the node
		// here is a build()-time act, like validateReconvergence appending its edges.
		// See snapshotBoundaryAction for the measurement and for why this is not done at
		// Execute.
		r.node.action = snapshotBoundaryAction(r.node.action, 0)
	}

	// 4. Predicate clauses (0) and (a).
	//
	// 🔴 THESE SIT BEFORE THE ANTI-VACUITY CLAUSE, AND THE PLAN ORDERED THEM AFTER IT.
	// The deviation is deliberate and it is the only one in T1; the derivation, because
	// it is not obvious:
	//
	//	S is a root  <=>  S has NO incoming edges  =>  NOTHING has a path to S
	//	                                           =>  D is not an ancestor of S
	//
	// So with the anti-vacuity clause first, clause (a) is UNREACHABLE -- every input it
	// exists to reject is absorbed one check earlier and refused as "no path from doer",
	// and a dead branch READS as coverage while exercising nothing. Ordering (a) first
	// makes the branch reachable and gives a consumer the pointed diagnosis rather than a
	// true but misleading one. Reported to the architect as F118-ENG-02 and ruled correct.
	//
	// WHAT THIS ORDERING IS NOT: it is not what makes 118-AF1 sound. Measured with
	// per-clause counters, dropping (a) in this order changes ZERO verdicts -- anti-vacuity
	// below refuses those inputs anyway. (a) is a DIAGNOSTIC branch, not the soundness
	// guard. See the contract comment above; the distinction decides which check is safe
	// to relax and the earlier artifacts had it backwards.
	//
	// (0)
	if d.verifier == d.sink {
		return fmt.Errorf("%w: boundary (%s, %s, %s): verifier and sink are the same node %q",
			ErrValidation, d.doer, d.verifier, d.sink, d.sink)
	}
	// (a) -- 118-AF1's class, as an explicit branch.
	if len(sink.dependsOn) == 0 {
		return fmt.Errorf("%w: boundary (%s, %s, %s): sink %q is a root (it has no dependencies), "+
			"so nothing can precede it and no verifier can",
			ErrValidation, d.doer, d.verifier, d.sink, d.sink)
	}

	// 5. Graph anti-vacuity (VB-08 re-derived, DEC-M23-VB08-R2): D must be an ancestor
	//    of S. A doer with no control-flow channel to the sink quantifies over nothing,
	//    which is exactly what this clause exists to reject. It never consults
	//    dominance, so it cannot reproduce 118-AF2.
	if reachAvoiding(succ, d.doer, d.sink, nil) == nil {
		return fmt.Errorf("%w: boundary (%s, %s, %s): no path from doer %q to sink %q -- "+
			"the declaration constrains nothing",
			ErrValidation, d.doer, d.verifier, d.sink, d.doer, d.sink)
	}

	// 6. Predicate clause (b).
	for _, name := range rootNames(dag) {
		// THIS `continue` IS REDUNDANT TODAY AND IS KEPT ON PURPOSE. reachAvoiding
		// opens with `if avoid != nil && from == *avoid { return nil }`, so the walk
		// already refuses to start at the avoided node and removing this branch changes
		// no verdict -- MEASURED: it is mutation M4 in the matrix in boundary_test.go,
		// a SURVIVOR, and an equivalent mutant rather than an untested cell (0 of 111
		// arms distinguish the two trees, and the early return explains why).
		//
		// It stays because deleting it would make clause (b)'s correctness depend on a
		// REMOTE invariant in another function -- true today, and silently false the day
		// someone edits that early return, with no test able to notice. Cheap
		// defence-in-depth at the site that needs it. Do not read it as load-bearing,
		// and do not delete it as dead: it is neither.
		if name == d.verifier {
			continue
		}
		if path := reachAvoiding(succ, name, d.sink, &d.verifier); path != nil {
			return fmt.Errorf("%w: boundary (%s, %s, %s): %s reaches sink %q without passing verifier %q",
				ErrValidation, d.doer, d.verifier, d.sink, renderPath(path), d.sink, d.verifier)
		}
	}
	return nil
}

// successors inverts dependsOn once per build. The executor stores predecessors; the
// predicate walks forward.
func successors(dag *DAG) map[string][]string {
	succ := make(map[string][]string, len(dag.nodes))
	for name, n := range dag.nodes {
		for _, dep := range n.dependsOn {
			succ[dep.name] = append(succ[dep.name], name)
		}
	}
	// Map iteration is random; the walk must pick the same offending path every run or
	// a refusal message is nondeterministic.
	for _, s := range succ {
		sort.Strings(s)
	}
	return succ
}

// rootNames returns the nodes the executor seeds its first level from -- those with
// zero dependsOn entries (dag.go:337, dag.go:402). NOT b.startNodes, which no non-test
// code reads (118-AF3). Sorted so a refusal names the same offending path on every run.
func rootNames(dag *DAG) []string {
	var roots []string
	for name, n := range dag.nodes {
		if len(n.dependsOn) == 0 {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	return roots
}

// reachAvoiding returns a from->to path that never enters avoid, or nil if none
// exists. A NIL avoid avoids nothing. The walk always takes at least one edge, so it
// never reports the zero-length path from a node to itself.
//
// "every from->to path contains avoid" is exactly "to is unreachable from `from` in
// the graph with avoid deleted", which is what a nil return means.
//
// 🔴 avoid IS A *string AND THAT IS NOT STYLE (118-F4). It was `avoid string` with ""
// meaning "avoid nothing" -- but "" IS A LEGAL NODE NAME, so a graph containing one
// collided with the sentinel. Reproduced before the fix:
//
//	nodes ""->V->S, decl(doer: "")  ->  `no path from doer "" to sink "S"`
//
// which is FACTUALLY FALSE: the path exists. It failed CLOSED, so no declaration was
// wrongly accepted -- but a checker that refuses a legal graph with a false reason sends
// a consumer to look for a missing edge that is present.
//
// WHY THE EXHAUSTIVE CORPUS COULD NOT SEE IT: it names nodes n0..n5. An oracle is only
// as complete as its generator's alphabet, and no amount of graph coverage reaches a
// defect that lives in the NAME space. A *string makes the two states unrepresentable
// as one another rather than distinguishable by convention.
func reachAvoiding(succ map[string][]string, from, to string, avoid *string) []string {
	if avoid != nil && from == *avoid {
		return nil
	}
	seen := map[string]bool{from: true}
	var walk func(at string, path []string) []string
	walk = func(at string, path []string) []string {
		for _, next := range succ[at] {
			if (avoid != nil && next == *avoid) || seen[next] {
				continue
			}
			p := append(append([]string{}, path...), next)
			if next == to {
				return p
			}
			seen[next] = true
			if found := walk(next, p); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(from, []string{from})
}

func renderPath(path []string) string { return strings.Join(path, " -> ") }

// WithBoundary declares that the sink must not occur before the verifier: on every
// route the executor can take through the built graph, verifier precedes sink. It is
// the Precedence(verifier, sink) property, scoped to CONTROL FLOW -- see the contract
// on validateBoundaries above, which is the one place it is stated in full.
//
// build() refuses a declaration this graph does not satisfy, naming a concrete
// offending path. The doer names the actor the declaration is about; it must have a
// path to the sink, or the declaration would constrain nothing.
//
// 🔴 ONE CONSEQUENCE A CALLER CANNOT DISCOVER FROM ANYWHERE ELSE (118-D10). Build takes
// a SNAPSHOT of the verifier's and the sink's actions, so for those two nodes the graph
// stops tracking later edits to the action VALUE you passed in: after Build, a mutation
// through a retained handle -- CompositeAction.Add is the one that exists today -- is
// SILENTLY DISCARDED for them. The same call on any other node still applies.
//
// Same operation, outcome depending on the position, and NO ERROR AND NO LOG either way.
// It is deliberate: the boundary was validated against the actions those two nodes held
// at Build, and a set that can be edited afterwards was never validated at all. But a
// caller who does not know it will conclude the mutation was lost, so it is stated here
// rather than only in the unexported guard that implements it.
//
// Declarations are folded in Build, which is the choiceEdges/mergeEdges shape.
func (b *WorkflowBuilder) WithBoundary(doer, verifier, sink string) *WorkflowBuilder {
	b.boundaries = append(b.boundaries, boundaryDecl{doer: doer, verifier: verifier, sink: sink})
	return b
}
