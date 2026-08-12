package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 116-AF2, the durable half of the census. This is a PARITY sweep: any function that hands
// a value to a callee which RECURSES OVER IT must also bound that value's depth, or appear
// below with a stated reason.
//
// ── WHY THE EXISTING SEAM COULD NOT DO THIS, which is the whole reason this file exists ──
//
// write_guard_parity_test.go's collector records a callee only when call.Fun is a bare
// *ast.Ident. Every qualified call — json.Marshal, reflect.DeepEqual — is an
// *ast.SelectorExpr and is therefore INVISIBLE to it. The one sweep the package had for
// "does every write path guard the same axes" could not see either of the two callees this
// phase's crashes came from.
//
// ── 🔴 WHAT A GREEN HERE MUST NEVER BE CITED FOR ──
//
// This proves ONE thing: a depth guard is PRESENT at every site that calls a named
// recursing callee. It proves NOTHING about whether that guard is correct, whether
// maxWalkFrames is sound on the host's stack, or whether the crash class is closed.
//
//	A green here does NOT discharge AF2.
//	A green here does NOT discharge the reflect.DeepEqual class.
//	A green here over a wrong maxWalkFrames is exactly the shape this phase is about.
//
// Soundness of the bound is TestAF2_BoundIsSoundOnThisBox. Closure of the crash class is
// TestAF2_EveryWriterRefusesInsteadOfDying and TestAF2_DeepEqualClassIsGuarded. This file
// is a drift detector, and a drift detector cited as an assurance is how the last one
// substituted for correctness.
//
// ── ITS CEILING, ON ITS FACE, because the sibling sweep already learned this the hard way ──
//
// This checks the DISTRIBUTION of enforcement, not its correctness. It says a guard is
// present; it says nothing about whether the guard holds.
//
// AND IT IS FUNCTION-GRANULAR, which is a second, sharper limit: it can only see that a
// function calls a guard SOMEWHERE and a recursing callee SOMEWHERE. A function that
// bounds one value and hands a DIFFERENT one to json.Marshal unguarded passes here. That
// is not hypothetical in this package — fanOutAction.Execute guards its branch results and
// separately marshals a flat []int, and the sweep cannot tell those two apart. The
// per-value argument has to live at the call site; this sweep only guarantees the question
// was asked in that function at all. write_guard_parity_test.go's own
// doc comment records how that distinction cost a blocker: the element guard was present at
// every write path and pinned by that sweep, and still enforced nothing under concurrent
// delivery because it counted outside any lock. Consistency-of-enforcement reads like
// coverage. It is not coverage.
//
// ── AND THE LARGER HALF IS DELIBERATELY NOT HERE ──
//
// This sweep only sees callees it is TOLD to look for. It cannot see a newly written self-
// or mutually-recursive function inside this package that receives a host value — which is
// exactly the cloneMap and mapIdentity shape, and exactly the class that has now bitten
// twice. Finding those needs call-graph recursion detection plus a judgement about whether
// a host value can reach the function, and its exemption list would be long enough that
// waving entries through would re-create the blind spot in a form that looks audited.
//
// That half is an OPEN CLASS, named rather than half-built. This file closes the "site
// eleven" case: a new json.Marshal or reflect.DeepEqual on a host value cannot land
// unguarded and unremarked.

// recursesOverItsArgument is the callee set. Both entries are measured, not assumed: each
// dies with `fatal error: stack overflow` on a deep enough value, at ~646 and ~465 bytes of
// goroutine stack per walk frame respectively (see maxWalkFrames).
//
// json.Valid, json.Unmarshal and json.Decoder are NOT here and that is not an oversight —
// the stdlib scanner is a state machine with its own nesting cap, so the decode direction
// refuses rather than recursing. The asymmetry between the two directions is the original
// F2 finding.
var recursesOverItsArgument = map[string]bool{
	"json.Marshal":       true,
	"json.MarshalIndent": true,
	"reflect.DeepEqual":  true,
}

// valueDepthGuards are the calls that discharge the obligation.
//
// checkJSONDepth is deliberately ABSENT. It takes BYTES, which only exist if an encoder
// already returned, so it cannot discharge a crash-axis obligation — and accepting it here
// would rebuild the exact substitution this phase spent its cycle undoing, where seven
// sites read as COVERED because a checkJSONDepth sat nearby.
var valueDepthGuards = map[string]bool{
	"checkValueDepth": true,
	// The DeepEqual-axis guard (116-AF2 BLOCKER 1). A crash-axis guard, like
	// checkValueDepth and unlike checkJSONDepth — it takes a VALUE, before the
	// recursing consumer runs. Added because this sweep correctly reported both
	// DeepEqual sites as UNGUARDED the moment they were swapped onto it.
	"checkDeepEqualPairDepth": true,
	"deepEqualFrames":         true,
	"encodeHostValue":         true, // walks, then marshals, then checks the bytes
	"valueDepth":              true,
	"encodeOutput":            true, // thin wrapper over encodeHostValue
	"encodeKV":                true, // ditto
	"marshalSignalPayload":    true,
}

// ── EXEMPTIONS, SPLIT BY WHAT KIND OF CLAIM THEY ARE ──
//
// A single flat table would make four unlike claims look identical, which is this phase's
// recurring defect appearing inside the instrument built to prevent it. Two kinds:
//
//	CHECKED  a TYPE FACT. Mechanically verifiable, and verified below by an actual
//	         assertion, so the exemption EXPIRES BY ITSELF the day the fact stops holding.
//	ARGUED   a claim about a CALLER, not about a type. Categorically weaker: nothing in
//	         the function itself makes it true, and it can go silently false while this
//	         table still reads green. Carries the condition that would break it.
//
// There is exactly one ARGUED entry and it is the one to distrust.
// censusExemptionsChecked is EMPTY, and that is a result rather than an omission.
//
// fanout.go's __failed__ site was drafted into it — a genuine type-fact exemption — and the
// "every exemption must be EXERCISED" invariant rejected it, because the sweep is
// FUNCTION-GRANULAR and fanOutAction.Execute already calls checkValueDepth for its branch
// results. The sweep classifies the whole function as guarded and never consults the
// exemption.
//
// So the granularity ceiling written at the top of this file is not abstract: there is a
// real recursing call in this package that this sweep passes without ever examining. The
// type facts it actually rests on are asserted by TestAF2Census_CheckedExemptionsStillHold
// below, and the per-value arguments live at the call sites — which is where they have to
// live, because this sweep cannot express them.
var censusExemptionsChecked = map[string]string{}

var censusExemptionsArgued = map[string]string{
	"workflow_store.go:JSONFileStore.Save": "ARGUED, and this is the weak one: the re-marshalled value is " +
		"the OUTPUT OF A DECODER, whose scanner caps nesting, so its depth is bounded by construction and " +
		"it cannot be cyclic (a decoded document is a tree). NOTHING IN THIS FUNCTION MAKES THAT TRUE — it " +
		"is inherited from the caller that produced snapshotData by marshalling successfully. BREAKS IF: " +
		"this function is ever fed bytes from a producer that marshals lazily, or the guard upstream moves. " +
		"The exemption would go silently false and this table would still read green.",
}

func censusExemption(site string) (string, bool) {
	if r, ok := censusExemptionsChecked[site]; ok {
		return r, true
	}
	r, ok := censusExemptionsArgued[site]
	return r, ok
}

// TestAF2Census_CheckedExemptionsStillHold is what makes a `checked:` exemption different
// from an argued one: the TYPE FACT it rests on is asserted here, so the exemption cannot
// outlive its reason. An exemption that only a human re-reads is an argued one wearing a
// checked one's clothes.
func TestAF2Census_CheckedExemptionsStillHold(t *testing.T) {
	// fanout.go's __failed__ check: the second argument is a string, so DeepEqual's
	// type comparison short-circuits before any descent. MEASURED at depth 650,000 in a
	// child process: this exits 0, while DeepEqual(deep, deep) dies.
	deep := nestValue(2000)
	require.False(t, reflect.DeepEqual(deep, "a string"),
		"precondition of the fanout __failed__ exemption")
	require.NotEqual(t, reflect.TypeOf(deep), reflect.TypeOf(""),
		"THE EXEMPTION'S TYPE FACT HAS CHANGED. fanout.go's __failed__ collision check is exempt from "+
			"the depth census ONLY because its second operand is unconditionally a string, which makes "+
			"reflect.DeepEqual return on the type comparison without recursing. If that operand can now "+
			"be a host value, the site needs a real guard and the exemption must be deleted")

	// The failed-list is []int — a flat slice of scalars, no host value reachable.
	var failed []int
	require.Equal(t, reflect.Slice, reflect.TypeOf(failed).Kind())
	require.Equal(t, reflect.Int, reflect.TypeOf(failed).Elem().Kind(),
		"the failed-list marshal is exempt only while its element type is a scalar; an element type "+
			"that can carry a host value reopens the crash axis at that marshal")
}

// TestAF2Census_EveryRecursingCalleeHasADepthBound is the assertion.
func TestAF2Census_EveryRecursingCalleeHasADepthBound(t *testing.T) {
	calls := af2CensusSweep(t)

	var unguarded []string
	var usedExemptions []string
	for site, callees := range calls {
		if !af2CallsAny(callees, recursesOverItsArgument) {
			continue
		}
		if af2CallsAny(callees, valueDepthGuards) {
			continue
		}
		if _, exempt := censusExemption(site); exempt {
			usedExemptions = append(usedExemptions, site)
			continue
		}
		unguarded = append(unguarded, site)
	}
	sort.Strings(unguarded)

	require.Empty(t, unguarded,
		"UNGUARDED RECURSION OVER A HOST VALUE at %v.\n\n"+
			"json.Marshal and reflect.DeepEqual both RECURSE, and a Go stack overflow is a `fatal error`, "+
			"not a panic: unrecoverable, no host's deferred recover() fires, the process dies. A guard that "+
			"runs AFTER the call cannot help — that is what checkJSONDepth does and it is why it does not "+
			"count here.\n\n"+
			"Either call checkValueDepth on the value before handing it over, or add the site to "+
			"censusExemptions WITH THE REASON AND THE CONDITION THAT WOULD BREAK IT. Do not add it bare.",
		unguarded)

	// ANTI-VACUITY, both directions. Without these the sweep goes green when it has found
	// nothing at all — which is how a census reassures without checking, and this package
	// has an arm that accepted 248 of 248 deliveries for exactly that reason.
	var guardedSites int
	for site, callees := range calls {
		if af2CallsAny(callees, recursesOverItsArgument) && af2CallsAny(callees, valueDepthGuards) {
			_ = site
			guardedSites++
		}
	}
	require.GreaterOrEqual(t, guardedSites, 4,
		"NOT A PASS — the sweep found almost no GUARDED sites, so it is not seeing the calls it claims to "+
			"check. The likeliest cause is the collector losing qualified calls again: json.Marshal is an "+
			"*ast.SelectorExpr, and the sibling sweep in write_guard_parity_test.go records only bare "+
			"*ast.Ident, which is precisely why it could never see this class")

	// EVERY EXEMPTION MUST BE EXERCISED, which is stricter than "its site still exists" and
	// is the check that keeps this table honest. An entry that never fires is a reason
	// nobody re-reads, sitting in the file looking considered — and the first draft of this
	// table had two of them. Both were removed by this assertion, not by review.
	sort.Strings(usedExemptions)
	var declared []string
	for site := range censusExemptionsChecked {
		require.Contains(t, calls, site,
			"STALE EXEMPTION: %q no longer exists in the package", site)
		declared = append(declared, site)
	}
	for site := range censusExemptionsArgued {
		require.Contains(t, calls, site,
			"STALE EXEMPTION: %q no longer exists in the package. An exemption for a site that is gone "+
				"hides the day a similarly-named site arrives", site)
		declared = append(declared, site)
	}
	sort.Strings(declared)
	require.Equal(t, declared, usedExemptions,
		"an exemption is declared that the sweep never needed. Either the site stopped calling a "+
			"recursing callee (delete the entry) or it grew a real guard (delete the entry) — a dead "+
			"exemption is indistinguishable from a considered one at a glance, which is exactly the "+
			"failure this table exists to avoid")
	t.Logf("MEASURED: %d functions sweep-visible; %d call a recursing callee AND a depth guard; "+
		"%d exercised exemptions (%v)", len(calls), guardedSites, len(usedExemptions), usedExemptions)
}

// TestAF2Census_TheSweepBites is the census's own bite, and it is not optional: a sweep
// that cannot fail is a sweep nobody should believe. It runs the identical detection over
// a SYNTHETIC source file rather than over the package, so it proves the mechanism without
// requiring anyone to break real code to check it.
func TestAF2Census_TheSweepBites(t *testing.T) {
	const bad = `package p
import ("encoding/json"; "reflect")
func unguarded(v any) { _, _ = json.Marshal(v) }
func alsoUnguarded(a, b any) bool { return reflect.DeepEqual(a, b) }
func guarded(v any) { _ = checkValueDepth(v, "x"); _, _ = json.Marshal(v) }
`
	calls := af2ParseSource(t, "synthetic.go", bad)

	var unguarded []string
	for site, callees := range calls {
		if af2CallsAny(callees, recursesOverItsArgument) && !af2CallsAny(callees, valueDepthGuards) {
			unguarded = append(unguarded, site)
		}
	}
	sort.Strings(unguarded)
	require.Equal(t, []string{"synthetic.go:alsoUnguarded", "synthetic.go:unguarded"}, unguarded,
		"the sweep must flag BOTH recursing callees when unguarded, and must NOT flag the guarded one. "+
			"If reflect.DeepEqual is missing here the collector has lost qualified calls; if `guarded` "+
			"appears, the guard set is not being consulted")
}

func af2CallsAny(callees map[string]bool, set map[string]bool) bool {
	for c := range callees {
		if set[c] {
			return true
		}
	}
	return false
}

// af2CensusSweep parses this package's non-test source and returns, per function, the set
// of callees it invokes directly — QUALIFIED CALLS INCLUDED, which is the whole point.
func af2CensusSweep(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	out := map[string]map[string]bool{}
	var parsed int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(f) //nolint:gosec // test-local sweep of this package's own source
		require.NoError(t, rerr)
		for k, v := range af2ParseSource(t, f, string(src)) {
			out[k] = v
		}
		parsed++
	}
	require.Greater(t, parsed, 1, "sanity: the sweep must parse more than one file")
	return out
}

func af2ParseSource(t *testing.T, name, src string) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	require.NoError(t, err, "parsing %s", name)

	out := map[string]map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fname := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			fname = af2RecvName(fn.Recv.List[0].Type) + "." + fname
		}
		callees := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				callees[fun.Name] = true
			case *ast.SelectorExpr:
				// THE LINE THE SIBLING SWEEP IS MISSING. `json.Marshal` and
				// `reflect.DeepEqual` are SelectorExprs; without this the whole class is
				// invisible and the sweep passes by seeing nothing.
				if pkg, isIdent := fun.X.(*ast.Ident); isIdent {
					callees[pkg.Name+"."+fun.Sel.Name] = true
				}
				callees[fun.Sel.Name] = true // also record the bare name, for method calls
			}
			return true
		})
		out[name+":"+fname] = callees
	}
	return out
}

func af2RecvName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return af2RecvName(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return af2RecvName(v.X)
	}
	return "?"
}
