// Package doctest is a permanent, CI-visible gate over the Go code samples in
// the project's user-facing documentation (README + docs/getting-started +
// docs/guides). It enumerates EVERY ```go fenced block, classifies each as a
// COMPLETE program (compiled + run) or a SNIPPET (parsed), and accounts for
// every block — a silently-skipped sample is a gap.
//
// It is test-only (no non-test .go files ship in this package) and self-
// contained: complete programs run from a toolchain-ignored ".doctest*" temp
// dir INSIDE this module, so they resolve pkg/workflow via this module's own
// go.mod — no _local/ dependency, offline.
package doctest

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- documentation set under test -------------------------------------------

// docFiles returns the doc files whose Go samples are gated, relative to repo root.
func docFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	files = append(files, filepath.Join(root, "README.md"))
	for _, dir := range []string{"docs/getting-started", "docs/guides"} {
		matches, err := filepath.Glob(filepath.Join(root, dir, "*.md"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		sort.Strings(matches)
		files = append(files, matches...)
	}
	return files
}

// repoRoot resolves the module root from this test package's working directory
// (internal/doctest → ../..), asserting a go.mod sentinel.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root sentinel go.mod not found at %s: %v", root, err)
	}
	return root
}

// --- fence extraction (indent-tolerant) -------------------------------------

// A ```go opening fence, allowing leading indentation (list-item code blocks).
var fenceOpen = regexp.MustCompile("^(\\s*)```go\\s*$")

type block struct {
	file      string // repo-relative path
	index     int    // 1-based ordinal within the file
	startLine int    // 1-based line of the opening fence
	code      string // block body (fence lines excluded)
}

// extractBlocks reads a file and scans it for every ```go block.
func extractBlocks(t *testing.T, absPath, relPath string) []block {
	t.Helper()
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return extractBlocksFromContent(string(data), relPath)
}

// extractBlocksFromContent is the pure fence-scanning core (no filesystem), so
// the extractor can be bite-tested on synthetic markdown. The closing fence is
// the first line whose trimmed content is exactly ``` (matching Markdown's
// indent-tolerant close), so indented list-item blocks are captured too. A
// trailing \r (CRLF files) is absorbed by TrimSpace on the close and by the
// fenceOpen regex's \s* on the open.
func extractBlocksFromContent(content, relPath string) []block {
	lines := strings.Split(content, "\n")
	var out []block
	i, idx := 0, 0
	for i < len(lines) {
		if fenceOpen.MatchString(lines[i]) {
			start := i + 1
			var body []string
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) != "```" {
				body = append(body, lines[i])
				i++
			}
			idx++
			out = append(out, block{
				file:      relPath,
				index:     idx,
				startLine: start,
				code:      strings.Join(body, "\n"),
			})
		}
		i++
	}
	return out
}

// --- classification ---------------------------------------------------------

type kind int

const (
	kindComplete kind = iota // package main + non-empty func main → compile + run
	kindSnippet              // everything else → parse floor
)

func (k kind) String() string {
	if k == kindComplete {
		return "COMPLETE"
	}
	return "SNIPPET"
}

// classify decides COMPLETE vs SNIPPET. A block is COMPLETE iff it parses as a
// full Go file, declares `package main`, and has a `func main` with a non-empty
// body. A `package main` scaffold with an empty/stub main (a progressive-
// tutorial fragment) is NOT a standalone program by Go's own rules → SNIPPET.
func classify(code string) kind {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", code, parser.SkipObjectResolution)
	if err != nil {
		return kindSnippet
	}
	if f.Name == nil || f.Name.Name != "main" {
		return kindSnippet
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name != nil && fn.Name.Name == "main" && fn.Recv == nil &&
			fn.Body != nil && len(fn.Body.List) > 0 {
			return kindComplete
		}
	}
	return kindSnippet
}

// --- snippet parse floor ----------------------------------------------------

// parseSnippet verifies a snippet is syntactically valid Go. Doc snippets take
// four shapes; a snippet passes if ANY wrap parses:
//   - file:   a whole file (has its own package clause);
//   - decl:   a declaration fragment (funcs/types/vars at top level);
//   - stmt:   a statement fragment (a function body);
//   - hybrid: a teaching block that shows decls/imports THEN usage statements —
//     valid Go pieces that can't share one scope. hybrid hoists the leading
//     import/decl section to the top and wraps the trailing statements in a
//     function, then parses the assembled unit.
//
// It returns nil on the first success, else a combined error naming every
// strategy's failure (so a genuine break is legible, not masked by the first).
func parseSnippet(code string) error {
	type wrap struct{ name, src string }
	strategies := []wrap{
		{"file", code},
		{"decl", "package p\n" + code},
		{"stmt", "package p\nfunc _dt() {\n" + code + "\n}"},
	}
	if decls, stmts, ok := splitDeclsStmts(code); ok {
		strategies = append(strategies, wrap{
			"hybrid", "package p\n" + decls + "\nfunc _dt() {\n" + stmts + "\n}",
		})
	}
	var errs []string
	for _, s := range strategies {
		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, "snippet.go", s.src, parser.SkipObjectResolution); err == nil {
			return nil
		} else {
			errs = append(errs, s.name+": "+err.Error())
		}
	}
	return fmt.Errorf("%s", strings.Join(errs, " | "))
}

// declStarters are the leading keywords of a top-level declaration; a depth-0
// line beginning with anything else starts a statement (the hybrid split point).
var declStarters = map[string]bool{
	"package": true, "import": true, "func": true,
	"type": true, "var": true, "const": true,
}

// splitDeclsStmts partitions a "[decls...][usage statements...]" teaching block
// at the FIRST depth-0 line that begins a statement. It returns the decl section
// (imports + named decls) and the statement section, with ok=false when the
// block has no trailing-statement shape (nothing to hoist). Brace/paren depth is
// computed via go/scanner, so braces inside strings/comments do not confuse it.
func splitDeclsStmts(code string) (decls, stmts string, ok bool) {
	lines := strings.Split(code, "\n")
	depth := lineStartDepths(code, len(lines))
	split := -1
	for i, ln := range lines {
		if i >= len(depth) || depth[i] != 0 {
			continue
		}
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") {
			continue
		}
		word := t
		if sp := strings.IndexAny(word, " \t("); sp >= 0 {
			word = word[:sp]
		}
		if declStarters[word] {
			continue
		}
		split = i
		break
	}
	if split < 0 {
		return "", "", false
	}
	return strings.Join(lines[:split], "\n"), strings.Join(lines[split:], "\n"), true
}

// lineStartDepths returns, per line index, the bracket depth ((), [], {}) in
// effect at the start of that line, tokenized with go/scanner so that brackets
// inside string/rune literals and comments are ignored.
func lineStartDepths(code string, nLines int) []int {
	out := make([]int, nLines)
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("s.go", fset.Base(), len(code))
	s.Init(file, []byte(code), nil, 0)
	depth := 0
	lastLine := 1
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		line := fset.Position(pos).Line
		// Record the pre-token depth for every line from the last recorded up to
		// this token's line (blank/interleaved lines inherit the running depth).
		for ln := lastLine; ln <= line && ln <= nLines; ln++ {
			out[ln-1] = depth
		}
		lastLine = line + 1
		switch tok {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth > 0 {
				depth--
			}
		}
	}
	for ln := lastLine; ln <= nLines; ln++ {
		out[ln-1] = depth
	}
	return out
}

// --- the gate ---------------------------------------------------------------

// minBlocks is the anti-regression floor for the number of ```go blocks in the
// doc set (118 at ph56). It guards the EXTRACTION axis: if fenceOpen or the
// scanner regresses and drops blocks, enumeration silently shrinks — the very
// "silent skip" this gate exists to prevent, which the self-consistent
// classification accounting cannot detect (it only proves every EXTRACTED block
// was checked, not that every DOC block was extracted). Adding doc samples is
// fine (floor tolerates growth); dropping below the floor is a deliberate,
// documented act (bump this constant), never a silent regression.
const minBlocks = 118

// TestDocSamples is the mechanical gate. It enumerates every ```go block in the
// doc set, classifies each, and accounts for every one: SNIPPETs must parse,
// COMPLETE programs must compile AND run (exit 0). No block is skipped.
func TestDocSamples(t *testing.T) {
	root := repoRoot(t)
	files := docFiles(t, root)

	var all []block
	for _, abs := range files {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			rel = abs
		}
		all = append(all, extractBlocks(t, abs, rel)...)
	}

	// Extraction floor — a drop below the known block count means the scanner
	// silently lost samples (fixes the vacuity where fewer/zero extracted blocks
	// still "account" cleanly). Checked BEFORE the per-block subtests so a
	// `-run` filter cannot mask it.
	if len(all) < minBlocks {
		t.Fatalf("extraction regressed: %d blocks < floor %d — a fence was silently dropped",
			len(all), minBlocks)
	}

	// Classify in the enumeration loop (not inside the subtests) so the counts
	// are complete regardless of any `-run` subtest filter.
	var nComplete, nSnippet int
	for _, b := range all {
		switch classify(b.code) {
		case kindComplete:
			nComplete++
		default:
			nSnippet++
		}
	}

	for _, b := range all {
		b := b
		name := b.file + "#" + strconv.Itoa(b.index)
		t.Run(name, func(t *testing.T) {
			switch classify(b.code) {
			case kindComplete:
				runComplete(t, root, b)
			default:
				if err := parseSnippet(b.code); err != nil {
					t.Errorf("%s (fence @L%d): snippet does not parse under any wrapper: %v",
						name, b.startLine, err)
				}
			}
		})
	}

	t.Logf("accounted for %d blocks across %d files: %d COMPLETE (compiled+run), %d SNIPPET (parsed)",
		len(all), len(files), nComplete, nSnippet)
}

// --- complete-program compile + run -----------------------------------------

// runComplete compiles + runs a COMPLETE program and reports a failure on the
// test if it does not exit 0.
func runComplete(t *testing.T, root string, b block) {
	t.Helper()
	out, err := buildAndRun(root, b.code)
	if err != nil {
		t.Errorf("%s#%d (fence @L%d): complete program failed to compile/run: %v\n%s",
			b.file, b.index, b.startLine, err, out)
	}
}

// buildAndRun writes a complete program to a toolchain-ignored ".doctest*" dir
// inside the module (so it resolves pkg/workflow via this module's own go.mod,
// with no _local/ dependency and offline), `go run`s it, and returns the
// combined output plus a non-nil error if it fails to compile, panics, or exits
// non-zero. A ".":prefixed directory is skipped by `go build ./...`, so the
// sample never pollutes the module's own build. This is the error-returning
// core that the gate driver and the non-vacuity self-test both exercise.
func buildAndRun(root, code string) ([]byte, error) {
	dir, err := os.MkdirTemp(root, ".doctest")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp build dir
	}()

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o644); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, context.DeadlineExceeded
	}
	return out, err
}

// --- non-vacuity: the gate must BITE ----------------------------------------

// TestGateBites is the permanent, encoded proof that the gate is not vacuous —
// it verifies every checking mechanism REJECTS a deliberately-broken sample and
// ACCEPTS a good one. A gate that silently passes broken docs is worse than no
// gate; this locks the bite in so a future refactor can't quietly defang it.
func TestGateBites(t *testing.T) {
	root := repoRoot(t)

	// (0) The EXTRACTION axis — the mechanism whose silent failure IS this
	// phase's cardinal sin. Pin the fence scanner on the edge cases so a
	// regression (esp. losing indent-tolerance and dropping the list-item
	// fences) reddens deterministically, independent of the live doc corpus.
	t.Run("extract", func(t *testing.T) {
		fence := "```"
		cases := []struct {
			name         string
			content      string
			wantCount    int
			wantFirstHas string
		}{
			{"bare", fence + "go\nA := 1\n" + fence, 1, "A := 1"},
			{"indented list-item", "1. text\n\n   " + fence + "go\n   B := 2\n   " + fence + "\n", 1, "B := 2"},
			{"trailing space on open", fence + "go  \nC := 3\n" + fence, 1, "C := 3"},
			{"adjacent blocks", fence + "go\nD := 4\n" + fence + "\n" + fence + "go\nE := 5\n" + fence, 2, "D := 4"},
			{"non-go fence ignored", fence + "bash\necho hi\n" + fence + "\n" + fence + "go\nF := 6\n" + fence, 1, "F := 6"},
			{"CRLF endings", fence + "go\r\nG := 7\r\n" + fence + "\r\n", 1, "G := 7"},
			{"unterminated at EOF is captured", fence + "go\nH := 8", 1, "H := 8"},
		}
		for _, c := range cases {
			blocks := extractBlocksFromContent(c.content, c.name)
			if len(blocks) != c.wantCount {
				t.Errorf("%s: got %d blocks, want %d", c.name, len(blocks), c.wantCount)
				continue
			}
			if c.wantCount > 0 && !strings.Contains(blocks[0].code, c.wantFirstHas) {
				t.Errorf("%s: first block %q missing %q", c.name, blocks[0].code, c.wantFirstHas)
			}
		}
	})

	// (1) parseSnippet rejects syntactically broken Go and accepts valid snippets.
	t.Run("snippet_parse", func(t *testing.T) {
		bad := []string{
			"func f( {",             // unbalanced paren
			"data.Set(\"k\" 123)",   // missing comma between args
			"x := f(\n  arg\n)",     // multi-line call, no trailing comma (middleware#16's class)
			"type T struct { a int", // unterminated struct
		}
		for _, s := range bad {
			if err := parseSnippet(s); err == nil {
				t.Errorf("parseSnippet ACCEPTED broken snippet (should reject): %q", s)
			}
		}
		good := []string{
			"data.Set(\"user_id\", 123)",                      // stmt fragment
			"func Add(a, b int) int { return a + b }",         // decl fragment
			"type Rule func(*WorkflowData) error\nvar r Rule", // decl fragment
			"func Wrap() {}\nx := Wrap()",                     // hybrid: decl + usage
		}
		for _, s := range good {
			if err := parseSnippet(s); err != nil {
				t.Errorf("parseSnippet REJECTED valid snippet %q: %v", s, err)
			}
		}
	})

	// (2) classify distinguishes a runnable program from a stub scaffold.
	t.Run("classify", func(t *testing.T) {
		complete := "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hi\") }"
		if classify(complete) != kindComplete {
			t.Errorf("classify: real program not COMPLETE")
		}
		stub := "package main\nimport \"fmt\"\nfunc main() { /* later */ }"
		if classify(stub) != kindSnippet {
			t.Errorf("classify: empty-main scaffold should be SNIPPET, not COMPLETE")
		}
		frag := "data.Set(\"k\", 1)"
		if classify(frag) != kindSnippet {
			t.Errorf("classify: fragment should be SNIPPET")
		}
	})

	// (3) buildAndRun rejects a complete program that does not compile/run — the
	// exact mechanism that guards the runnable-doc samples.
	t.Run("build_and_run", func(t *testing.T) {
		good := "package main\n\nimport (\n\t\"context\"\n\n\t\"github.com/ppcavalcante/flow-orchestrator/pkg/workflow\"\n)\n\nfunc main() {\n\tb := workflow.NewWorkflowBuilder().WithWorkflowID(\"x\")\n\tb.AddStartNode(\"n\").WithAction(func(ctx context.Context, d *workflow.WorkflowData) error { return nil })\n\tdag, err := b.Build()\n\tif err != nil { panic(err) }\n\t_ = dag.Execute(context.Background(), workflow.NewWorkflowData(\"x\"))\n}\n"
		if out, err := buildAndRun(root, good); err != nil {
			t.Errorf("buildAndRun REJECTED a valid program: %v\n%s", err, out)
		}
		// stale API — a renamed method the current code does not export.
		stale := "package main\n\nimport \"github.com/ppcavalcante/flow-orchestrator/pkg/workflow\"\n\nfunc main() {\n\tworkflow.NewWorkflowBuilder().ThisMethodWasRemovedInV2()\n}\n"
		if _, err := buildAndRun(root, stale); err == nil {
			t.Errorf("buildAndRun ACCEPTED a stale-API program (should fail to compile)")
		}
	})
}
