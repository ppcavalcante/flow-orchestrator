package workflow

// M23 SEAL-09 guards for DEC-M23-PARK-CAPABILITY (as AMENDED 2026-07-27).
//
// The amendment exists because the DEC's original wording — "suspendable is set only by
// the in-package declared constructors" — named a path PRODUCTION NEVER TAKES. The four
// exported declared constructors (NewTimerNode, NewWaitForSignalNode,
// NewWaitForSignalOrTimeoutNode, NewWaitForConditionNode) have ZERO non-test callers; the
// builder mints every real node at builder.go via NewNodeWithCapacity. Built literally,
// every timer/signal/approval node created through the builder would have carried
// suspendable == false, node.execute would have stopped honouring ErrSuspended, and every
// durable park in the library would have become a hard failure — while the 48 tests that
// call those constructors directly stayed GREEN, because they take the one path that set it.
//
// That is why these two guards are structural rather than a spot-check: a test that mints
// through the constructors cannot see the defect, because the constructors are the arm that
// works. Arm A pins the CHOKEPOINT; arm B pins the SET.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// markerImplementorsFromSource returns every type in non-test package source that declares
// a suspendable() method — i.e. the suspendableAction marker's implementors, enumerated by
// the compiler's own view of the source rather than by a hand list. A hand list here would
// reproduce inside the guard the exact defect the guard exists to prevent.
// CEILING: this records only *ast.StarExpr receivers, so a marker implemented on a
// VALUE receiver (`func (a xAction) suspendable() {}`) is invisible to it. Latent
// rather than live — all seven implementors are pointer receivers today — but a value
// receiver added later would silently shrink the set this arm cross-checks against,
// which is the direction that reads as "no offenders".
func markerImplementorsFromSource(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "sanity: the sweep must find package source to parse")

	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		ast.Inspect(af, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "suspendable" || fd.Recv == nil {
				return true
			}
			if len(fd.Recv.List) != 1 {
				return true
			}
			if st, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := st.X.(*ast.Ident); ok {
					out = append(out, id.Name)
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// ARM A — the CHOKEPOINT. Every *Node in the package must be born in one of the two
// constructors, because that is the only place suspendable is derived.
//
// WHAT THIS SWEEP WOULD AND WOULD NOT HAVE CAUGHT — the original claim here was that it
// would have caught internal/workflow/memory/node_pool.go, which wrote .Action onto an
// already-minted node and would have recycled a STALE suspendable. That claim was FALSE
// three ways, and a review caught it: this sweep globs pkg/workflow ONLY (node_pool.go
// lived in internal/workflow/memory), it matches composite LITERALS rather than
// assignments, and it requires an *ast.Ident that the qualified `workflow.Node{` does not
// produce.
//
// The node_pool hazard is genuinely closed — by SEAL-01 unexporting the fields, which
// makes an out-of-package post-mint write a compile error. This sweep closes a DIFFERENT
// and narrower hole: an IN-PACKAGE `&Node{}` that bypasses the two constructors and so
// never derives suspendable at all. Both are worth having; only the second is this test's.
func TestSuspendable_NodeIsOnlyMintedAtTheChokepoint(t *testing.T) {
	const (
		// The names are STRING LITERALS because the sweep matches the AST, so a rename
		// cannot carry them along. T6 proved that is a real hazard rather than a
		// theoretical one TWICE, in one task: unexporting NewNodeWithCapacity left this
		// pair stale, and then unexporting NewNode left it stale AGAIN, in a later commit,
		// after the first had been fixed. Both times the anti-vacuity assertion below is
		// what caught it — reporting "expected 2, found 1: the SWEEP is broken, not the
		// code" rather than silently finding no offenders in a chokepoint it could no
		// longer see. A sweep that has lost its target reports CLEAN, which is
		// indistinguishable from a pass; that is why the count assertion sits AHEAD of the
		// substantive one, and it is the single most load-bearing line in this file.
		mintA = "newNode"
		mintB = "newNodeWithCapacity"
	)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var offenders []string
	var mintSites int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)

		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			inMint := fd.Recv == nil && (fd.Name.Name == mintA || fd.Name.Name == mintB)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				id, ok := cl.Type.(*ast.Ident)
				if !ok || id.Name != "Node" {
					return true
				}
				if inMint {
					mintSites++
					return true
				}
				offenders = append(offenders, fset.Position(cl.Pos()).String()+
					" (in "+fd.Name.Name+")")
				return true
			})
		}
	}

	// Anti-vacuity FIRST, and deliberately before the substantive assertion: if the sweep
	// finds no &Node{} literals at all it is broken, and a broken sweep reports "no
	// offenders" — the instrument accusing the code of being clean. (ph116 landed exactly
	// this defect in the parity sweep and it took pointing it at real history to see it.)
	require.Equal(t, 2, mintSites,
		"the sweep expected exactly 2 &Node{} literals inside %s/%s; found %d. "+
			"The SWEEP is broken, not the code — do not read a pass below as a clean chokepoint",
		mintA, mintB, mintSites)

	require.Empty(t, offenders,
		"a *Node is minted outside %s/%s at %v.\n"+
			"Node.suspendable is derived ONLY at those two sites, so a node born elsewhere "+
			"carries suspendable=false regardless of its action, and node.execute will refuse "+
			"to honour its ErrSuspended (the node.go 'not a declared suspension node' arm). "+
			"Mint through a constructor, or derive the flag at the new site.",
		mintA, mintB, offenders)
}

// ARM B — the SET. Every action type carrying the marker must mint a suspendable node.
//
// The pairs below are a hand list, and that is safe ONLY because the count is cross-checked
// against the AST-derived implementor set: adding an eighth marker type without adding it
// here reds this test. That cross-check is the whole point — an unchecked hand list is what
// produced the defect this file documents.
//
// BITE, DISCHARGED BY READING THE WHOLE FAILURE TEXT (D-15) — an earlier pass read only
// testify's assertion line and did not earn this. The seed is not synthetic: it is the
// DEC's own literal original wording, `suspendable` set only by the declared constructors,
// i.e. newNodeWithCapacity — the constructor build() mints through — stops deriving it.
//
// THE INSTRUMENT, stated because the number is meaningless without it (117-F3): the
// seed is applied and the WHOLE PACKAGE is run, `go test ./pkg/workflow/ -count=1`.
// Measured that way it reds 57 TOP-LEVEL TESTS, 107 including subtests, across 20
// files (FAIL, 486.453s).
//
// Two earlier records of this same bite said "three" and then "SEVEN" — each produced by
// a FILTERED run reported as an unqualified enumeration, so the correction of an
// undercount was itself an 8x undercount. A count is a property of the instrument that
// produced it; `-run` narrows the instrument and the number silently follows. That is
// this project's truncated-view lesson wearing a test-filter costume, and the error ran
// in the dangerous direction: it UNDERSTATED how load-bearing the mint derivation is,
// inside the very comment whose purpose is to prove it load-bearing, and it left a
// regression baseline under which a future change reddening 7 and greening 50 would read
// as equivalent.
//
// THREE CONTROLS, so every red is seed-attributable rather than compile cascade or flake:
// the package COMPILES under the seed (`go build ./...` exit 0, so no red is a build
// cascade); the full unseeded suite is GREEN at head (ok, 407.395s); and the named tests
// run unseeded are green too.
//
// The seven below are EXEMPLARS chosen for what they demonstrate — not the population.
// FIVE of the seven are BEHAVIORAL, so the mechanism is load-bearing for a guarantee and
// not only for representation fidelity:
//
//	repr  TestSuspendable_EveryMarkedActionMintsSuspendable  all 7 subtests
//	repr  TestSuspendable_RunConstantAcrossRebuild           anti-vacuity precondition
//	BEHAV TestSubWorkflow_DirectSuspendableChild_Refused     "must fail Build" — got nil
//	BEHAV TestSubWorkflow_DeepNestedSuspendable_Refused
//	BEHAV TestSubWorkflow_TransitiveSuspendableGrandchild_Refused
//	BEHAV TestSuspendableChildError_StillFiresOnInlineChild  "must still be refused" — got nil
//	BEHAV TestParkedAdv_SuspendableChildReParks              surfaces the ENGINE's own message:
//	        `node sub returned ErrSuspended but is not a declared suspension node`
//
// The repr/BEHAV column is there so "five" is countable from the list rather than
// asserted beside it — the two are not the same claim, and the earlier "three" sat
// beside a list that already contradicted it.
//
// That last line is node.go's refusal arm, and it is EXACTLY the breakage F117-DEC-01
// predicted the DEC's original wording would cause. The bite reproduces the predicted
// failure verbatim rather than merely reddening something.
//
// TestSuspendable_NodeIsOnlyMintedAtTheChokepoint correctly stays GREEN — it asserts a
// different property (where nodes are minted, not what the mint derives). Reported as a
// non-response, not dressed up as an eighth green.
//
// NOT VACUOUS UNDER A UNIFORM REMOVAL EITHER — checked separately, because the second
// require below is worded differentially ("diverged from NewNode") and a reader could
// take the guard for a NewNode-vs-NewNodeWithCapacity parity check that both constructors
// failing together would satisfy. Both predicates are absolute require.True calls on
// .suspendable. Dropping the derivation from BOTH constructors reds the FIRST arm with
// "…carries the suspendableAction marker but NewNode minted a node with suspendable=false
// — node.execute will refuse to honour its park".
func TestSuspendable_EveryMarkedActionMintsSuspendable(t *testing.T) {
	cases := []struct {
		typeName string
		action   Action
	}{
		{"timerAction", &timerAction{nodeName: "n", duration: time.Second}},
		{"waitForSignalAction", &waitForSignalAction{nodeName: "n", signalName: "s"}},
		{"waitForConditionAction", &waitForConditionAction{predicate: func(*WorkflowData) bool { return true }}},
		{"waitForSignalOrTimeoutAction", &waitForSignalOrTimeoutAction{nodeName: "n", signalName: "s", duration: time.Second}},
		{"approvalAction", &approvalAction{}},
		{"queueSubWorkflowAction", &queueSubWorkflowAction{}},
		{"parkedSubWorkflowAction", &parkedSubWorkflowAction{}},
	}

	fromSource := markerImplementorsFromSource(t)
	var covered []string
	for _, c := range cases {
		covered = append(covered, c.typeName)
	}
	sort.Strings(covered)

	// The cross-check that keeps the hand list honest. If this reds, a marker type was
	// added or removed and this test — not the engine — is what is stale.
	require.Equal(t, fromSource, covered,
		"the suspendableAction implementors in source (%v) do not match the types covered "+
			"here (%v). A new declared suspension primitive must be added to this test, or "+
			"the guard silently stops covering it.", fromSource, covered)

	for _, c := range cases {
		t.Run(c.typeName, func(t *testing.T) {
			require.True(t, newNode("n", c.action).suspendable,
				"%s carries the suspendableAction marker but NewNode minted a node with "+
					"suspendable=false — node.execute will refuse to honour its park", c.typeName)
			require.True(t, newNodeWithCapacity("n", c.action, 2).suspendable,
				"%s: NewNodeWithCapacity diverged from NewNode", c.typeName)
		})
	}

	// The converse, and it is the arm that keeps the flag from becoming "always true":
	// an ordinary action must NOT mint suspendable.
	require.False(t, newNode("plain", ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })).suspendable,
		"an ordinary action minted a SUSPENDABLE node — the derivation is vacuous, and "+
			"node.execute would honour ErrSuspended from any action, breaking WaitingSound")
}

// D-07 — RUN-CONSTANCY. The TLA model holds Suspendable as a CONSTANT, so its invariance
// across Crash/Recover is true by construction THERE. The real system satisfies it only if
// the set is deterministically re-derived identically from the rebuilt DAG on every resume.
//
// This asserts the INVARIANT (the two sets are equal), never a schedule — a rebuild is not
// an interleaving, so there is nothing here for a future correct change to break. (D-22.)
func TestSuspendable_RunConstantAcrossRebuild(t *testing.T) {
	build := func() *DAG {
		b := NewWorkflowBuilder().WithWorkflowID("run-constancy")
		b.AddStartNode("start").WithActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })
		b.AddTimer("timer", time.Second).DependsOn("start")
		b.AddWaitForSignal("sig", "go").DependsOn("start")
		b.AddWaitForSignalTimeout("sigt", "go2", time.Second).DependsOn("start")
		dag, err := b.Build()
		require.NoError(t, err)
		return dag
	}

	setOf := func(d *DAG) []string {
		var s []string
		for name, n := range d.nodes {
			if n.suspendable {
				s = append(s, name)
			}
		}
		sort.Strings(s)
		return s
	}

	first := setOf(build())

	// Anti-vacuity: if the set were empty, "identical across rebuilds" would hold trivially
	// and this test would pass over a completely broken derivation.
	require.ElementsMatch(t, []string{"sig", "sigt", "timer"}, first,
		"the suspendable set is not what the builder declared; a later equality check "+
			"would be vacuous")

	// A rebuild is what a resume does: the same builder code runs again in a fresh process
	// and must produce the same park-capable set, because nothing about it is persisted.
	for i := 0; i < 3; i++ {
		require.Equal(t, first, setOf(build()),
			"rebuild %d produced a DIFFERENT suspendable set. The set must be a run-constant "+
				"(D-07): the TLA model holds Suspendable fixed across Crash/Recover, and a node "+
				"journaled Waiting under one build but rebuilt non-suspendable violates "+
				"WaitingSound in the real system while the model cannot see it.", i)
	}
}
