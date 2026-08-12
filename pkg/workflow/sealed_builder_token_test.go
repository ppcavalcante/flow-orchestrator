package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// M23 SEAL-06 — the builder token's TEST-ONLY MINT, and the guard that keeps it honest.
// ---------------------------------------------------------------------------
//
// THE TRAP THIS FILE EXISTS TO AVOID, stated plainly because it is the highest-risk move
// in the phase. A fail-closed token reds ~106 in-package tests that hand-build a DAG. The
// tempting fix is to stamp inside NewDAG/NewDAGWithCapacity so they all go green again —
// and that voids the mechanism completely: the token would then certify "a DAG was
// constructed" rather than "build() validated it", and EVERY TEST WOULD PASS. Green is
// exactly the signature of that mistake, which is why it needs a structural defence
// rather than a resolution to be careful.
//
// The defence is that the concession lives HERE, in a _test.go file. That is strictly
// stronger than naming it: a symbol declared in a _test.go file DOES NOT EXIST IN THE
// PRODUCTION BUILD. It is not a discipline anyone has to maintain — the compiler makes it
// impossible for shipped code to reach these helpers. Combined with the sweep below
// (build() is the only non-test writer of .built), the token means exactly one thing.

// newDAGForTest mints a DAG carrying the M23 SEAL-06 token WITHOUT running build().
//
// It exists so in-package tests can keep hand-assembling graphs — testing executor
// behaviour on a deliberately odd topology is legitimate and predates the seal. What it
// deliberately does NOT do is validate: a test using it is asserting something about
// execution, not about the builder's contract.
//
// A test that wants to exercise the REAL admission path must use WorkflowBuilder, not
// this. In particular the T6 dispatch-path test must not touch this helper — the whole
// finding is about what happens to a graph that never passed build().
func newDAGForTest(name string) *DAG {
	d := newDAG(name)
	d.built = true
	return d
}

// newDAGWithCapacityForTest is newDAGForTest with a capacity hint.
func newDAGWithCapacityForTest(name string, nodeCapacity int) *DAG {
	d := newDAGWithCapacity(name, nodeCapacity)
	d.built = true
	return d
}

// newWorkflowForTest is NewWorkflow with the token applied to the DAG it mints.
//
// NewWorkflow mints an EMPTY DAG that the caller then populates, so it cannot itself be
// "stamped and validated" in any meaningful sense — there is nothing to validate at the
// moment of the mint, and the whole usage pattern is mint-then-mutate. Stamping inside
// NewWorkflow would therefore be the same vacuity as stamping inside NewDAG, just wearing
// a different constructor's name. The concession stays here, test-only, instead.
func newWorkflowForTest(store WorkflowStore) *Workflow {
	w := newWorkflow(store)
	w.dag.built = true
	return w
}

// markBuiltForTest stamps an existing DAG. For the handful of sites that receive a DAG
// from a helper rather than minting one.
func markBuiltForTest(d *DAG) *DAG {
	d.built = true
	return d
}

// TestSealed_BuiltIsStampedOnlyInBuild is the guard that makes the token mean what it
// says: build() must be the ONLY non-test writer of DAG.built.
//
// Modelled on TestSuspendable_NodeIsOnlyMintedAtTheChokepoint, which is landed and
// bite-proven — including its most important feature, which is that the ANTI-VACUITY
// ASSERTION RUNS FIRST. A broken sweep finds zero assignments and reports "no offenders",
// and that reads exactly like a clean chokepoint. Pay for the distinction up front.
//
// CEILING — WHAT THIS GUARD DOES NOT PROVE, found by biting it rather than by reasoning
// about it, and stated here because an unstated ceiling is how a guard gets over-trusted.
// This checks the stamp's LOCATION. Its sibling below checks the stamp's ORDER. NEITHER
// CHECKS ITS VALUE: changing build()'s final statement to `dag.built = false` leaves both
// of these GREEN, because there is still exactly one assignment to .built, still inside
// build(), still immediately before the return. That mutation is caught by the test suite
// (every builder-produced run fails closed), not by these guards. So the completeness
// argument for the token is "these two guards AND the suite", never these guards alone.
//
// Bite results, all four run and read end-to-end:
//   - stamp added inside NewDAG (THE vacuity trap)  -> this guard reds: "found 2"
//   - stamp moved to Build() (same file, wrong fn)  -> this guard reds naming builder.go:Build,
//     AND FromBuilder breaks behaviourally, because FromBuilder calls build() directly and
//     deliberately — which is what makes the placement load-bearing rather than stylistic
//   - stamp moved before validateReconvergence      -> ONLY the order guard reds (this one
//     correctly passes), which is why the two are separate tests and not one
//   - stamp changed to false                        -> NEITHER guard reds; the suite does
func TestSealed_BuiltIsStampedOnlyInBuild(t *testing.T) {
	const (
		stampFile = "builder.go" // the file build() lives in
		stampFunc = "build"      // the only non-test function allowed to write .built
	)

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var (
		offenders []string
		stamps    int
	)

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue // the test-only mint above is the sanctioned concession
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, "parsing %s", path)

		// Track the enclosing function so an offender can be reported by name, not
		// only by line — a line number drifts, a symbol does not.
		var enclosing string
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "built" {
						continue
					}
					stamps++
					if path != stampFile || enclosing != stampFunc {
						offenders = append(offenders,
							path+":"+enclosing+" (line "+
								fset.Position(node.Pos()).String()+")")
					}
				}
			}
			return true
		})
	}

	// ANTI-VACUITY, DELIBERATELY FIRST. If the sweep matched nothing at all it would
	// report a clean chokepoint, which is indistinguishable from the real thing.
	require.Equal(t, 1, stamps,
		"the sweep expected exactly ONE non-test assignment to .built (in %s's %s); found %d. "+
			"THE SWEEP IS BROKEN, NOT THE CODE — do not read the emptiness below as a clean seal.",
		stampFile, stampFunc, stamps)

	sort.Strings(offenders)
	require.Empty(t, offenders,
		"DAG.built is assigned outside %s's %s at %v.\n"+
			"The token certifies that build() ran and its validation passed. Stamped anywhere "+
			"else it certifies only that SOME code path chose to set a flag — the mechanism is "+
			"void and every test still passes, which is why this guard exists. If a test needs "+
			"an unvalidated-but-executable DAG, use newDAGForTest / newWorkflowForTest in "+
			"sealed_builder_token_test.go, which cannot exist in the production build.",
		stampFile, stampFunc, offenders)
}

// TestSealed_NoCustomCodecOnDAGOrNode is the clause that keeps the token UNFORGEABLE
// THROUGH SERIALIZATION — and, unlike the two guards above, it protects a property that
// is true today only by accident of what has not been written yet.
//
// The stamp is an unexported value field, so encoding/json and encoding/gob skip it
// STRUCTURALLY. Measured: json.Marshal(builtDAG) yields "{}"; json.Unmarshal into a
// &DAG{} succeeds with the ZERO stamp; gob.Encode errors outright with "type
// workflow.DAG has no exported fields". So bytes handed to us by anyone cannot carry a
// forged stamp — a decoded DAG always arrives unbuilt and is refused.
//
// THAT HOLDS ONLY BECAUSE DAG IMPLEMENTS NO CUSTOM CODEC, and this project is unusually
// likely to add one. The whole strategy narrative is "a workflow is DATA, not CODE", so
// serializing a DAG is precisely the feature someone eventually wants. Add an
// UnmarshalJSON to DAG and the stamp becomes forgeable by anyone who can hand us bytes:
// their graph gets populated by our decoder and the token certifies something build()
// never saw. The forgery route would arrive as a FEATURE, written by someone with no
// idea they were dismantling a seal — this project's "pre-armed for silent removal"
// shape, which is exactly how the flock-per-open-file-description guarantee and the
// GetDependencies copy each nearly died.
//
// One mechanical assertion turns a property that is true today into one that stays true
// or reds a guard.
func TestSealed_NoCustomCodecOnDAGOrNode(t *testing.T) {
	// The full stdlib marshaller surface. A type implementing ANY of these takes over its
	// own encoding, and the "unexported fields are skipped structurally" argument dies.
	codecMethods := map[string]bool{
		"MarshalJSON": true, "UnmarshalJSON": true,
		"GobEncode": true, "GobDecode": true,
		"MarshalBinary": true, "UnmarshalBinary": true,
		"MarshalText": true, "UnmarshalText": true,
	}
	sealedTypes := map[string]bool{"DAG": true, "Node": true}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var (
		offenders []string
		scanned   int
	)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, "parsing %s", path)

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			scanned++
			// Resolve the receiver's type name through an optional pointer.
			expr := fn.Recv.List[0].Type
			if star, isPtr := expr.(*ast.StarExpr); isPtr {
				expr = star.X
			}
			ident, ok := expr.(*ast.Ident)
			if !ok || !sealedTypes[ident.Name] {
				continue
			}
			if codecMethods[fn.Name.Name] {
				offenders = append(offenders, ident.Name+"."+fn.Name.Name+" in "+path)
			}
		}
	}

	// ANTI-VACUITY. A sweep that resolved no methods at all would report a clean result.
	// The exact count is irrelevant and would be a maintenance tax; that it found a
	// substantial method surface is what distinguishes "checked" from "matched nothing".
	require.Greater(t, scanned, 50,
		"the sweep resolved only %d methods with receivers across non-test files — THE SWEEP "+
			"IS BROKEN, NOT THE CODE. Do not read the emptiness below as a clean result.", scanned)

	sort.Strings(offenders)
	require.Empty(t, offenders,
		"DAG or Node implements a custom codec: %v.\n"+
			"The M23 SEAL-06 builder token is an UNEXPORTED field, and it is unforgeable through "+
			"json/gob ONLY because those skip unexported fields structurally — measured: "+
			"json.Marshal gives \"{}\", gob.Encode errors with \"no exported fields\". A custom "+
			"marshaller takes over that encoding, so a caller who can hand us bytes can hand us a "+
			"populated graph, and the stamp then certifies something build() never validated.\n"+
			"If serializing a DAG is genuinely wanted, the token must stop being a bool and become "+
			"something the decoder cannot supply — do not simply delete this test.",
		offenders)
}

// TestSealed_StampIsTheLastStatementOfBuild pins the ORDER, which is a separate property
// from the location and is not implied by it.
//
// build() runs dag.Validate() and THEN validateReconvergence(dag) — and the latter APPENDS
// the DEC-M11-DEPMODEL merge<-choice edges as its final act. A stamp written after
// Validate() would therefore certify a graph that subsequently gained edges: the token
// would be true of a graph that no longer exists. Nothing about "build() is the only
// writer" catches that, so it is asserted separately.
func TestSealed_StampIsTheLastStatementOfBuild(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "builder.go", nil, 0)
	require.NoError(t, err)

	var body []ast.Stmt
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "build" && fn.Recv != nil {
			body = fn.Body.List
		}
	}
	require.NotEmpty(t, body, "could not find (*WorkflowBuilder).build — THE SWEEP IS BROKEN")

	// The stamp must be the statement immediately before the final return.
	require.GreaterOrEqual(t, len(body), 2, "build() is too short to hold a stamp and a return")
	stamp, ok := body[len(body)-2].(*ast.AssignStmt)
	require.True(t, ok,
		"the second-to-last statement of build() is not an assignment — the stamp must be the "+
			"LAST thing build() does before returning, because validateReconvergence appends "+
			"edges and a stamp written before it certifies a graph that then changed")

	sel, ok := stamp.Lhs[0].(*ast.SelectorExpr)
	require.True(t, ok && sel.Sel.Name == "built",
		"the second-to-last statement of build() does not assign .built; a statement was "+
			"inserted between the stamp and the return, or the stamp moved")
}
