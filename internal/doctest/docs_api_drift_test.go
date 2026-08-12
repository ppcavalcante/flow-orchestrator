package doctest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AUD-048 / V-05 — the parse floor in docs_samples_test.go proves a SNIPPET is
// syntactically valid Go, but a snippet that references a REMOVED or RENAMED
// package-level symbol (e.g. `workflow.WithStore(...)` after that constructor is
// gone) parses cleanly and passes. A doc that names an API the code no longer
// exports is exactly the rot a doctest gate exists to catch, and syntax cannot
// see it.
//
// This gate adds a semantic layer WITHOUT the instrument sprawl a full
// type-checker would need (assembling free-variable teaching snippets into
// compilable units, then filtering "undeclared name" errors — the kind of
// test-software-with-its-own-proof-obligations the audit's own §4 warns against).
// It is a single direct contract: every `workflow.<Ident>` qualified reference in
// the doc set must name a symbol the real pkg/workflow package still exports. The
// exported-symbol set is read by PARSING the package source (no build, no new
// dependency, offline), so it tracks HEAD automatically.
//
// Residual bound (documented, not hidden): method-drift on a LOCAL value
// (`builder.RenamedMethod()`) is NOT caught here — attributing a method to its
// receiver type needs full type inference, which this deliberately avoids. Those
// stay at the parse floor. What this closes is the package-qualified class, which
// is where doc API references overwhelmingly land (81 distinct `workflow.X` refs
// across the current doc set vs. the method-on-local minority).

// collectExportedTopLevel parses every non-test .go file directly under
// pkg/workflow and returns the set of names it exports at package scope: funcs
// (no receiver), types, and package-level vars/consts. Methods (receiver != nil)
// are deliberately excluded — they are not reachable as `workflow.<Name>`.
func collectExportedTopLevel(t *testing.T, root string) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "pkg", "workflow", "*.go"))
	if err != nil {
		t.Fatalf("glob pkg/workflow: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no pkg/workflow/*.go found under %s — cannot build the API set", root)
	}
	set := make(map[string]bool)
	fset := token.NewFileSet()
	parsed := 0
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, m, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", m, err)
		}
		parsed++
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					set[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							set[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								set[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	if parsed == 0 {
		t.Fatalf("collectExportedTopLevel parsed 0 non-test files under pkg/workflow")
	}
	return set
}

// parseFirstFile returns the first *ast.File that parses under the same wrap
// strategies parseSnippet uses (file / decl / stmt / hybrid), so a fragment's
// selectors are still walkable. ok=false means no wrap parsed — the parse-floor
// gate (TestDocSamples) already reddens on that block, so this gate defers to it
// rather than double-reporting.
func parseFirstFile(code string) (*ast.File, bool) {
	srcs := []string{
		code,
		"package p\n" + code,
		"package p\nfunc _dt() {\n" + code + "\n}",
	}
	if decls, stmts, ok := splitDeclsStmts(code); ok {
		srcs = append(srcs, "package p\n"+decls+"\nfunc _dt() {\n"+stmts+"\n}")
	}
	for _, src := range srcs {
		fset := token.NewFileSet()
		if f, err := parser.ParseFile(fset, "ref.go", src, parser.SkipObjectResolution); err == nil {
			return f, true
		}
	}
	return nil, false
}

// workflowRefs walks an AST for every `workflow.<Ident>` selector — a reference
// qualified by the bare package identifier `workflow`. Selectors on any other
// base (a local var, a different package) are ignored, so the check only fires on
// package-qualified API. Deterministic order for stable reporting.
func workflowRefs(f *ast.File) []string {
	seen := make(map[string]bool)
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if ok && base.Name == "workflow" && sel.Sel.IsExported() {
			seen[sel.Sel.Name] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDocSamples_NoStaleWorkflowAPI is the AUD-048 semantic gate: every
// package-qualified `workflow.<Ident>` reference in the doc set must name a
// symbol pkg/workflow still exports at package scope. A doc that survives the
// parse floor while naming a removed/renamed constructor, type, sentinel, or
// option now reddens deterministically.
func TestDocSamples_NoStaleWorkflowAPI(t *testing.T) {
	root := repoRoot(t)
	api := collectExportedTopLevel(t, root)
	files := docFiles(t, root)

	var all []block
	for _, abs := range files {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			rel = abs
		}
		all = append(all, extractBlocks(t, abs, rel)...)
	}
	if len(all) < minBlocks {
		t.Fatalf("extraction regressed: %d blocks < floor %d", len(all), minBlocks)
	}

	checked, refs := 0, 0
	for _, b := range all {
		f, ok := parseFirstFile(b.code)
		if !ok {
			continue // parse-floor gate owns this block's failure
		}
		checked++
		for _, ref := range workflowRefs(f) {
			refs++
			if !api[ref] {
				t.Errorf("%s#%d (fence @L%d): doc references workflow.%s, which pkg/workflow no longer exports "+
					"— the doc names a removed/renamed package-level symbol",
					b.file, b.index, b.startLine, ref)
			}
		}
	}
	t.Logf("AUD-048: verified %d workflow.<Ident> package-qualified refs across %d parseable blocks against %d exported symbols",
		refs, checked, len(api))
}

// TestAPIDriftGateBites pins the non-vacuity of the AUD-048 gate: it must ACCEPT
// a reference to a real exported symbol and REJECT a reference to one the package
// does not export. Without this a future refactor could defang the check (e.g.
// collect the wrong scope) and it would pass everything silently.
func TestAPIDriftGateBites(t *testing.T) {
	root := repoRoot(t)
	api := collectExportedTopLevel(t, root)

	// A representative real symbol from each exported kind must be present, or the
	// collector is under-reading and the live gate would false-RED the docs.
	for _, name := range []string{
		"NewWorkflowBuilder", // func
		"DAG",                // type
		"ErrNotFound",        // sentinel var
		"Completed",          // const (NodeStatus)
		"FromBuilder",        // func
	} {
		if !api[name] {
			t.Errorf("collector missed exported symbol %q — the gate would false-red real docs", name)
		}
	}

	// A name the package does not export must be absent, so a stale doc reference
	// to it reddens.
	for _, name := range []string{"WithStore", "ThisMethodWasRemovedInV2", "NewThingThatNeverExisted"} {
		if api[name] {
			t.Errorf("collector reports %q as exported, but it is not — a stale reference would pass", name)
		}
	}

	// End-to-end: a synthetic block naming a removed symbol must produce a ref the
	// gate rejects; a block naming a real one must not.
	staleBlock := "workflow.ThisMethodWasRemovedInV2()"
	f, ok := parseFirstFile(staleBlock)
	if !ok {
		t.Fatalf("bite setup: stale block did not parse under any wrap")
	}
	refs := workflowRefs(f)
	if len(refs) != 1 || refs[0] != "ThisMethodWasRemovedInV2" {
		t.Fatalf("bite setup: expected the one stale ref, got %v", refs)
	}
	if api[refs[0]] {
		t.Errorf("gate would ACCEPT a reference to the removed %q", refs[0])
	}

	goodBlock := "dag, err := workflow.NewWorkflowBuilder().WithWorkflowID(\"x\").Build()\n_ = dag\n_ = err"
	f, ok = parseFirstFile(goodBlock)
	if !ok {
		t.Fatalf("bite setup: good block did not parse under any wrap")
	}
	for _, ref := range workflowRefs(f) {
		if !api[ref] {
			t.Errorf("gate would REJECT the valid reference workflow.%s", ref)
		}
	}
}
