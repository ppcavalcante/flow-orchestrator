package doctest

// Adversarial complement to docs_samples_test.go. These tests attack the three
// gate mechanisms the author's own TestGateBites does NOT pin down:
//
//  1. the DRIFT false-negative surface — can a genuinely-invalid snippet slip
//     through parseSnippet? The prime suspect is the hybrid split, which trusts
//     lineStartDepths (go/scanner) to compute bracket depth. If depth were ever
//     miscomputed — a brace inside a string/rune/comment counted as real — the
//     split point would move and a real syntax error could be masked. The
//     package comment CLAIMS "braces inside strings/comments do not confuse it";
//     TestAdversarialDepthCalc PROVES it with assertions that redden the instant
//     someone swaps go/scanner for a naive brace count.
//  2. the classify() COMPLETE/SNIPPET boundary at len(fn.Body.List) > 0.
//  3. the buildAndRun() err!=nil contract across the exit-status matrix, plus
//     temp-dir cleanup.
//
// Every case states its oracle. parseSnippet is a documented SYNTAX floor
// ("verifies a snippet is syntactically valid Go"), so its oracle is syntactic
// validity only — semantically-orphaned-but-syntactically-valid statements
// (a bare `return`/`break`/`goto` in an implied body) are ACCEPTED BY DESIGN and
// are pinned as such below, not reported as defects.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdversarialDepthCalc is the load-bearing vector-1 guard. lineStartDepths
// must report bracket depth using go/scanner semantics, under which brackets
// inside string/rune literals and comments are NOT structural. Each case pairs a
// bracket hidden in a literal/comment (must NOT move depth) with the true depth
// the following line should see. A regression to naive character counting would
// compute the `wantNaive` column instead and fail every asymmetric row — that
// asymmetry is what makes this bite rather than pass vacuously.
func TestAdversarialDepthCalc(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		line      int // 0-based line index whose start-depth we assert
		wantDepth int // correct (go/scanner) depth
		wantNaive int // what a naive brace-counter would wrongly produce (documentation of the bite)
	}{
		// Positive controls: REAL brackets must count (guards against a
		// depth-always-0 stub that would pass every string case vacuously).
		{"real_open_brace", "if true {\nfoo()", 1, 1, 1},
		{"real_open_paren", "foo(\nbar", 1, 1, 1},
		{"real_nested", "a{\nb{\nc", 2, 2, 2},
		// Braces hidden in an interpreted string literal.
		{"open_brace_in_string", "s := \"{\"\nfoo()", 1, 0, 1},
		{"triple_open_in_string", "s := \"{{{\"\nfoo()", 1, 0, 3},
		{"close_in_string", "s := \"}\"\nfoo()", 1, 0, -1},
		{"parens_in_string", "s := \"(((\"\nfoo()", 1, 0, 3},
		{"escaped_quote_then_brace", "s := \"a\\\"{\"\nfoo()", 1, 0, 1},
		// Braces hidden in a rune literal.
		{"open_brace_in_rune", "c := '{'\nfoo()", 1, 0, 1},
		{"open_paren_in_rune", "c := '('\nfoo()", 1, 0, 1},
		// Braces hidden in a line comment (scanner mode 0 skips comments).
		{"brace_in_line_comment", "x := 1 // {{{\nfoo()", 1, 0, 3},
		{"brace_in_block_comment", "x := 1 /* } { ( */\nfoo()", 1, 0, 1},
		// Braces hidden in a multi-line raw string (spans lines, still one token).
		{"raw_string_multiline", "s := `l1 {\nl2 }`\nfoo()", 2, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := strings.Count(c.code, "\n") + 1
			depths := lineStartDepths(c.code, n)
			if c.line >= len(depths) {
				t.Fatalf("line %d out of range (have %d)", c.line, len(depths))
			}
			if depths[c.line] != c.wantDepth {
				t.Errorf("lineStartDepths[%d] = %d, want %d (naive-brace-count would give %d) — go/scanner literal/comment isolation regressed\ncode=%q\ndepths=%v",
					c.line, depths[c.line], c.wantDepth, c.wantNaive, c.code, depths)
			}
		})
	}
}

// TestAdversarialSplitNotCorruptedByLiterals proves the depth guarantee actually
// protects the split point: a decl whose value literal contains a `{`/statement-
// shaped text must NOT cause splitDeclsStmts to split inside it. If depth were
// miscomputed, the split would fall mid-declaration and a real break could be
// masked (or a valid block wrongly rejected).
func TestAdversarialSplitNotCorruptedByLiterals(t *testing.T) {
	// A var decl whose string value looks like a statement, then real usage.
	code := "var greeting = \"x := notAStatement { } foo()\"\nprintln(greeting)"
	decls, stmts, ok := splitDeclsStmts(code)
	if !ok {
		t.Fatalf("expected a split (decl then statement), got ok=false")
	}
	if !strings.Contains(decls, "var greeting") || strings.Contains(decls, "println") {
		t.Errorf("split corrupted: decls=%q should be exactly the var decl", decls)
	}
	if !strings.Contains(stmts, "println(greeting)") {
		t.Errorf("split corrupted: stmts=%q should hold the usage line", stmts)
	}
}

// TestAdversarialParseSnippetRejects hammers parseSnippet with genuinely
// syntactically-invalid Go — including breaks positioned to probe whether the
// hybrid decls/stmts hoist can spuriously rescue them. Oracle: go/parser rejects
// each of these as a standalone unit under all four wraps, so parseSnippet MUST
// return non-nil. A single acceptance here is a DRIFT FALSE-NEGATIVE (blocker).
func TestAdversarialParseSnippetRejects(t *testing.T) {
	bad := []struct{ name, code string }{
		{"unbalanced_open_paren", "func f( {"},
		{"unterminated_struct", "type T struct { a int"},
		{"missing_comma_args", "data.Set(\"k\" 123)"},
		{"unclosed_call_across_lines", "func H() {}\nresult := Process(cfg\nfmt.Println(result)"},
		{"missing_rhs", "func H() {}\nx :="},
		{"two_bare_idents", "func H() {}\ncfg fmt"},
		{"stray_close_after_decl", "var x = 1\nfoo()\n}"},
		{"import_after_statement", "foo()\nimport \"fmt\""},
		{"stmt_with_extra_open_brace", "foo() {\nbar()"},
		// The break is genuinely in a string, so the trailing `}` is a real
		// stray close — the depth guard must NOT be fooled into balancing it.
		{"brace_in_string_masks_nothing", "x := \"{\"\nfoo()\n}"},
		// Unterminated raw string swallowing following lines is still invalid.
		{"unterminated_raw_string", "x := `unterminated\ny := 2\nfoo()"},
		// A hoisted decl fragment that itself is broken.
		{"broken_decl_before_usage", "type = int\nfoo()"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := parseSnippet(c.code); err == nil {
				t.Errorf("DRIFT FALSE-NEGATIVE: parseSnippet ACCEPTED syntactically-invalid snippet (should reject):\n%q", c.code)
			}
		})
	}
}

// TestAdversarialParseSnippetSyntaxFloorContract documents — and locks in — the
// designed leniency: parseSnippet is a SYNTAX floor, so a statement that is
// syntactically valid inside a function body but semantically orphaned (bare
// return/break/goto after a decl) is ACCEPTED via the hybrid/stmt wraps. This is
// NOT a defect; it is the stated contract. Pinned so a future tightening (or a
// claim that these are bugs) is a conscious, visible change — and so the
// distinction between "syntax floor" and "semantic check" stays legible.
func TestAdversarialParseSnippetSyntaxFloorContract(t *testing.T) {
	acceptedByDesign := []struct{ name, code string }{
		{"orphan_return_after_func", "func f() int {\n\treturn 1\n}\nreturn 2"},
		{"bare_break", "func f() {}\nbreak"},
		{"bare_goto_label", "func f() {}\ngoto L\nL:"},
	}
	for _, c := range acceptedByDesign {
		t.Run(c.name, func(t *testing.T) {
			if err := parseSnippet(c.code); err != nil {
				t.Errorf("syntax-floor contract changed: parseSnippet now REJECTS a syntactically-valid-in-body statement %q: %v\n(if this is intentional tightening, update this test)", c.code, err)
			}
		})
	}
}

// TestAdversarialClassifyBoundary probes the COMPLETE/SNIPPET decision at the
// len(fn.Body.List) > 0 boundary and the func-main identity checks. Oracle: a
// block is COMPLETE iff it is a full file, package main, with a receiver-free
// `func main` whose body has ≥1 statement. The failure this guards is a runnable
// program silently classified SNIPPET (only parsed, never run → API drift in it
// goes uncaught) — and its inverse.
func TestAdversarialClassifyBoundary(t *testing.T) {
	cases := []struct {
		name string
		code string
		want kind
	}{
		// Stub mains: nothing to run → SNIPPET (by design).
		{"comment_only_main", "package main\nfunc main() { /* later */ }", kindSnippet},
		{"whitespace_only_main", "package main\nfunc main() {\n\n\n}", kindSnippet},
		{"empty_body_main", "package main\nfunc main() {}", kindSnippet},
		// Receiver-bearing main is not the entry point → SNIPPET.
		{"receiver_main", "package main\ntype T int\nfunc (T) main() { println(1) }", kindSnippet},
		// main declared in a non-main package is not a program entry point.
		{"main_in_other_pkg", "package demo\nfunc main() { println(1) }", kindSnippet},
		// Not a full file (bare fragment) → SNIPPET.
		{"bare_fragment", "data.Set(\"k\", 1)", kindSnippet},
		// Real runnable programs → COMPLETE (must be run).
		{"real_main", "package main\nfunc main() { println(1) }", kindComplete},
		{"single_empty_stmt", "package main\nfunc main() { ; }", kindComplete},
		{"decl_stmt_body", "package main\nfunc main() { var x int; _ = x }", kindComplete},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.code); got != c.want {
				t.Errorf("classify(%q) = %v, want %v", c.code, got, c.want)
			}
		})
	}
}

// TestAdversarialClassifyBodyListInvariant is a white-box guard: for every
// COMPLETE classification, the parsed func main body must in fact hold ≥1
// statement (the len>0 rule the classifier applies). This makes the "runnable"
// oracle explicit rather than trusting classify's internal logic.
func TestAdversarialClassifyBodyListInvariant(t *testing.T) {
	completePrograms := []string{
		"package main\nfunc main() { println(1) }",
		"package main\nfunc main() { ; }",
		"package main\nfunc main() { var x int; _ = x }",
	}
	for _, code := range completePrograms {
		if classify(code) != kindComplete {
			t.Fatalf("precondition: %q expected COMPLETE", code)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "s.go", code, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", code, err)
		}
		found := false
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Name != nil && fn.Name.Name == "main" && fn.Recv == nil && fn.Body != nil {
				found = true
				if len(fn.Body.List) == 0 {
					t.Errorf("COMPLETE program has empty main body but was not SNIPPET: %q", code)
				}
			}
		}
		if !found {
			t.Errorf("COMPLETE program has no receiver-free func main: %q", code)
		}
	}
}

// TestAdversarialBuildAndRunContract exercises the exit-status matrix that the
// runnable-doc guard depends on: buildAndRun returns err!=nil EXACTLY when a
// complete program does not cleanly exit 0. Oracle: `go run` exit status.
// A program that writes to stderr but exits 0 must PASS (else legitimate
// warning-printing samples would false-red). goroutine leaks at exit-0 must
// PASS (the process exits when main returns). These are the boundaries that keep
// the gate both sound (catches breaks) and non-flaky (accepts valid output).
func TestAdversarialBuildAndRunContract(t *testing.T) {
	if testing.Short() {
		t.Skip("buildAndRun invokes the go toolchain; skipped in -short")
	}
	root := repoRoot(t)
	cases := []struct {
		name       string
		code       string
		wantErrNil bool
	}{
		{"clean_exit0", "package main\nfunc main() {}", true},
		{"explicit_exit0", "package main\nimport \"os\"\nfunc main() { os.Exit(0) }", true},
		{"exit1", "package main\nimport \"os\"\nfunc main() { os.Exit(1) }", false},
		{"panic", "package main\nfunc main() { panic(\"boom\") }", false},
		{"compile_error", "package main\nfunc main() { undefinedSymbol() }", false},
		{"stderr_then_exit0", "package main\nimport (\"fmt\"; \"os\")\nfunc main() { fmt.Fprintln(os.Stderr, \"warn\"); os.Exit(0) }", true},
		{"goroutine_leak_exit0", "package main\nfunc main() { go func() { for {} }() }", true},
		{"nonzero_via_return_path", "package main\nimport \"os\"\nfunc main() { if true { os.Exit(3) } }", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := buildAndRun(root, c.code)
			if (err == nil) != c.wantErrNil {
				t.Errorf("buildAndRun err==nil = %v, want %v\nerr=%v\nout=%s", err == nil, c.wantErrNil, err, out)
			}
		})
	}
}

// TestAdversarialBuildAndRunCleanup asserts the temp .doctest* build dir is
// removed on the normal return path (deferred os.RemoveAll), for both the
// success and failure branches — a leaked dir would pollute `go build ./...`
// and accumulate across CI runs.
func TestAdversarialBuildAndRunCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain; skipped in -short")
	}
	root := repoRoot(t)
	before := countDoctestDirs(t, root)
	// A success and a failure, to cover both defer paths. Results ignored on
	// purpose: this test asserts cleanup, not the run outcome.
	_, _ = buildAndRun(root, "package main\nfunc main() {}")               //nolint:errcheck // asserting cleanup, not result
	_, _ = buildAndRun(root, "package main\nfunc main() { panic(\"x\") }") //nolint:errcheck // asserting cleanup, not result
	after := countDoctestDirs(t, root)
	if after != before {
		t.Errorf("buildAndRun leaked temp dirs: %d before, %d after (should be equal)", before, after)
	}
}

func countDoctestDirs(t *testing.T, root string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".doctest*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	n := 0
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			n++
		}
	}
	return n
}
