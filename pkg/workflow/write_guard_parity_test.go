package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This is the mechanical check that would have caught F1 — the defect where the
// element-count axis reached FOUR write sites while the byte axis reached FIVE, and
// the missing one was found only by a reviewer reading the diff.
//
// Hand-authored site inventories were wrong SEVEN times in this phase (write sites
// 2 -> 3 -> 4 -> 5). So the guarantee is not "someone counted correctly", it is
// "a write path that guards one axis and not its twin fails a test".
//
// Deliberately stronger than a count comparison. Equal counts pass a package that
// guards one site twice and another not at all; per-function coverage cannot.

// elementAxisGuards are the write-side guards for the element-count axis. Two, not
// one, because the quantity differs by what is being written: checkWriteElements
// bounds the largest SECTION/VECTOR inside one snapshot, checkMailboxEntries bounds
// the number of ENTRIES in a signal mailbox. Both mirror a read-side bound.
// jsonWriters names the byte-guarded write paths that produce a JSON document read back
// through a depth-limited decoder, so they must also carry the depth guard (F2). An
// explicit list rather than an inferred one: "writes JSON" is not something the AST can
// tell you, and a wrong inference here would either nag a FlatBuffers writer forever or
// silently exempt a real JSON one. Adding a JSON write path means adding it here — which
// is the point, since the alternative is the drift that produced F1.
//
// FlatBuffersStore.Save and writeFullSnapshotLocked are deliberately absent: they write
// FlatBuffers buffers, and the JSON depth ceiling does not apply to them.
//
// deliverSignalToDir is also absent, and for a different reason worth stating: it is
// CODEC-INJECTED, so whether it writes JSON depends on the encode func handed to it. Its
// depth guard therefore lives one level down, inside each codec — which is where it must
// live anyway, since the SQLite store shares marshalSignalPayload and never goes through
// deliverSignalToDir at all. Both codecs are pinned by requiredDepthSites below.
var jsonWriters = map[string]bool{
	"workflow_store.go:JSONFileStore.Save":     true,
	"workflow_data.go:WorkflowData.SaveToJSON": true,
}

// requiredDepthSites are the functions that MUST carry checkJSONDepth. Naming them
// explicitly is what catches a removal: the byte-implies-depth rule above only fires on
// paths that also call checkWriteSize, and the two signal codecs do not — they encode, they
// do not write. Without this set, deleting the guard from marshalSignalPayload would
// silently un-guard the FlatBuffers and SQLite mailboxes and no assertion here would notice.
var requiredDepthSites = []string{
	"workflow_store.go:JSONFileStore.Save",
	"workflow_data.go:WorkflowData.SaveToJSON",
	"signal_store.go:encodeSignalJSON",
	"signal_store.go:marshalSignalPayload",
}

var elementAxisGuards = map[string]bool{
	"checkWriteElements":  true,
	"checkMailboxEntries": true,
}

// parsePackageFuncCalls returns, for every non-test function in the package source,
// the set of function names it calls directly.
func parsePackageFuncCalls(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "sanity: the sweep must find package source to parse")

	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	var parsed int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(f) //nolint:gosec // test-local sweep of this package's own source
		require.NoError(t, rerr)
		file, perr := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, perr, "parsing %s", f)
		parsed++

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = types(fn.Recv.List[0].Type) + "." + name
			}
			key := f + ":" + name
			calls := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if id, isIdent := call.Fun.(*ast.Ident); isIdent {
					calls[id.Name] = true
				}
				return true
			})
			out[key] = calls
		}
	}
	require.Greater(t, parsed, 1, "sanity: the sweep must parse more than one file")
	return out
}

// types renders a receiver type expression as a bare name (T or *T -> T).
func types(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return types(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return types(v.X)
	}
	return "?"
}

// TestWriteGuardParity_EveryByteGuardHasAnElementTwin is the assertion itself. Any
// function that refuses an over-size write must also refuse an over-count one; a new
// write path that copies the byte guard and forgets the element guard reds here
// rather than shipping and surfacing at someone's resume.
//
// WHAT THIS SWEEP DOES NOT ESTABLISH, and the distinction cost a blocker. It checks that
// every write path guards the same AXES — a property about the DISTRIBUTION of enforcement.
// It says nothing about whether any one of those guards is CORRECT. The output below reads
// "5 byte / 8 element / 0 unpaired", which looks like an assurance about the guards and is
// only an assurance about their spread.
//
// That is exactly how the F1 concurrency blocker survived: the element guard was present at
// every write path and pinned by this sweep, and it still enforced nothing under concurrent
// delivery because it counted outside any lock. qa named the shape better than I did — "I
// verified that the guard is drift-proof and never asked whether the guard holds" — and
// consistency-of-enforcement reads like coverage, which is how it silently substitutes for
// correctness-of-enforcement. Correctness needs its own tests, under concurrency; see
// TestMailboxWriteGuard_HoldsUnderConcurrentDelivery and its separate-handles sibling.
//
// BLIND SPOT, written down rather than left for a future reader to rediscover: the
// rule is one-directional, so it CANNOT see a new file-writing path that lands with an
// element guard and no byte guard. That direction is deliberately unchecked — the
// converse is not a defect in general, because SQLite and InMemory legitimately carry
// an element guard with no byte ceiling to twin it (see below) — but it does mean a
// genuinely file-writing path arriving half-guarded the other way around passes here.
// A check whose limits are stated is worth more than one that appears total.
//
// Before tidying this test: the ORDER of the assertions in its body is load-bearing,
// and the symmetric-looking anti-vacuity assertion that appears to be missing was
// removed deliberately. The reason, and the input that proves it, are at that site.
func TestWriteGuardParity_EveryByteGuardHasAnElementTwin(t *testing.T) {
	funcs := parsePackageFuncCalls(t)

	var sizeSites, elementSites, depthSites, unpaired, depthUnpaired []string
	for name, calls := range funcs {
		hasSize := calls["checkWriteSize"]
		var hasElement bool
		for g := range elementAxisGuards {
			if calls[g] {
				hasElement = true
			}
		}
		hasDepth := calls["checkJSONDepth"]
		if hasSize {
			sizeSites = append(sizeSites, name)
			if !hasElement {
				unpaired = append(unpaired, name)
			}
			// The depth axis rides the same rule, on the JSON writers only — a
			// FlatBuffers writer has no JSON document to nest.
			if jsonWriters[name] && !hasDepth {
				depthUnpaired = append(depthUnpaired, name)
			}
		}
		if hasElement {
			elementSites = append(elementSites, name)
		}
		if hasDepth {
			depthSites = append(depthSites, name)
		}
	}
	sort.Strings(sizeSites)
	sort.Strings(elementSites)
	sort.Strings(depthSites)
	sort.Strings(unpaired)
	sort.Strings(depthUnpaired)

	// Anti-vacuity: the sweep must actually be finding guards. A rename or a broken
	// glob would otherwise make this test pass by seeing nothing at all — the failure
	// mode of a mechanical check nobody re-derives.
	//
	// ONLY the byte-axis set is the broken-sweep sentinel, and the ordering here is
	// load-bearing. An earlier version also asserted elementSites non-empty BEFORE the
	// unpaired check, which inverted the diagnosis on the one input that matters most:
	// pointed at 560a979^ — real history, before the element axis existed — it reported
	// "the sweep is broken, not the code" when the sweep was fine and the code genuinely
	// had no element guards. An empty element set with a non-empty byte set is not a
	// broken sweep, it is every site unpaired, and the assertion below says so with the
	// sites named. checkWriteSize is the sentinel because if the sweep cannot find even
	// that, it is resolving nothing.
	require.NotEmpty(t, sizeSites, "the sweep found NO checkWriteSize call sites; the sweep is broken, not the code")

	t.Logf("byte-axis write sites (%d): %s", len(sizeSites), strings.Join(sizeSites, ", "))
	t.Logf("element-axis write sites (%d): %s", len(elementSites), strings.Join(elementSites, ", "))

	require.Empty(t, unpaired,
		"these write paths guard the BYTE axis but not the ELEMENT axis — the exact shape of F1, where "+
			"checkWriteSize reached 5 sites and checkWriteElements only 4, so DeliverSignal wrote a mailbox "+
			"TakeSignals then refused to read, permanently. Add the element guard, or if the path genuinely "+
			"has no element quantity, say why here: %s", strings.Join(unpaired, ", "))

	t.Logf("depth-axis write sites (%d): %s", len(depthSites), strings.Join(depthSites, ", "))
	require.NotEmpty(t, depthSites, "the sweep found NO checkJSONDepth call sites; the sweep is broken, not the code")
	depthSet := map[string]bool{}
	for _, d := range depthSites {
		depthSet[d] = true
	}
	for _, want := range requiredDepthSites {
		require.True(t, depthSet[want],
			"%s no longer calls checkJSONDepth. The depth ceiling belongs to encoding/json's decoder, "+
				"so an unguarded writer here produces a document that is unreadable under ANY "+
				"configuration - a permanent wedge, not a tunable one.", want)
	}
	require.Empty(t, depthUnpaired,
		"these JSON write paths guard the BYTE axis but not the NESTING-DEPTH axis. The depth axis was "+
			"added in the same phase as this sweep and initially sat outside it, which is exactly how F1 "+
			"happened - a third axis reaching fewer writers than the first, with nothing mechanical to "+
			"notice. If a path here genuinely writes no JSON, remove it from jsonWriters and say why: %s",
		strings.Join(depthUnpaired, ", "))

	// The rule is DIRECTIONAL: byte-guarded implies element-guarded, never the
	// converse. Asserting equal counts — the obvious first form of this check, and
	// the one F1 was described by ("5 sites vs 4") — is actually WRONG as a standing
	// assertion, and this fix is what makes it wrong: the element axis now correctly
	// covers three paths with no byte twin. InMemoryStore.DeliverSignal writes no
	// file, and SQLiteStore.DeliverSignal writes a row (SQLite has no byte ceiling on
	// EITHER side — symmetric by absence, deliberately, per HYG-00). Requiring those
	// to carry a byte guard would be requiring a ceiling that does not exist.
	//
	// So the set relation, not the counts: every byte site is an element site.
	elementSet := map[string]bool{}
	for _, s := range elementSites {
		elementSet[s] = true
	}
	for _, s := range sizeSites {
		require.True(t, elementSet[s], "byte-axis site %s is not an element-axis site", s)
	}
	require.GreaterOrEqual(t, len(elementSites), len(sizeSites),
		"the element axis can cover MORE paths than the byte axis, never fewer")
}

// TestWriteGuardParity_TheSweepBites is the bite-proof for the sweep itself, in code
// rather than as a one-off manual mutation. It feeds the same analysis a synthetic
// function that guards bytes and not elements and requires the rule to reject it —
// so the parity test cannot rot into a check that passes because it sees nothing.
func TestWriteGuardParity_TheSweepBites(t *testing.T) {
	const src = `package workflow
func guardedBoth() error {
	if err := checkWriteSize(1, 2, "w"); err != nil { return err }
	if err := checkWriteElements(1, 2, "w"); err != nil { return err }
	return nil
}
func guardedBytesOnly() error {
	if err := checkWriteSize(1, 2, "w"); err != nil { return err }
	return nil
}
func guardedMailbox() error {
	if err := checkWriteSize(1, 2, "w"); err != nil { return err }
	return checkMailboxEntries(1, 2, "w")
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	require.NoError(t, err)

	verdict := map[string]bool{} // name -> paired
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		calls := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, isCall := n.(*ast.CallExpr); isCall {
				if id, isIdent := call.Fun.(*ast.Ident); isIdent {
					calls[id.Name] = true
				}
			}
			return true
		})
		if !calls["checkWriteSize"] {
			continue
		}
		paired := false
		for g := range elementAxisGuards {
			if calls[g] {
				paired = true
			}
		}
		verdict[fn.Name.Name] = paired
	}

	require.True(t, verdict["guardedBoth"], "a both-axes function must pass the rule")
	require.True(t, verdict["guardedMailbox"], "the mailbox guard must count as the element axis")
	require.False(t, verdict["guardedBytesOnly"],
		"a bytes-only function must FAIL the rule — if this passes, the parity test is theater")
	require.Len(t, verdict, 3, "sanity: all three synthetic functions were analysed")
}
