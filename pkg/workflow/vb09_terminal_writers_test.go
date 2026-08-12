package workflow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// VB-09 — the change-detecting guard over the set of non-test sites that write a
// terminal NodeStatus or a node output OUTSIDE executeNodesInLevel.
//
// WHAT THIS GUARD IS FOR. The engine's complete-mediation story says the executor is
// the place node state becomes terminal. Every member of the set below is a place that
// story is not literally true: a write that reaches terminal node state without passing
// the executor's mediation point. That is not automatically a defect -- LoadSnapshot
// restoring a persisted run is a legitimate member -- but each one is a CLAIM, and a new
// one must be argued in review rather than absorbed silently.
//
// WHY A GOLDEN FILE AND NOT A COUNT (DEC-M23-VB09-MECHANISM, user-ratified). The useful
// signal is WHICH member changed, not how many there are. A count tells a reviewer that
// something moved and nothing about what; it also rots the moment anyone restates it
// outside the assertion. So this guard reports SET DIFFERENCE in both directions and
// prints no population count anywhere -- not in the failure message, not in the golden's
// header, not in this comment.
//
// WHY A GO TEST AND NOT AN SMTC ANALYZER SPEC, which is the mechanism the requirement
// names: at 41f91fd no analyzer spec is wired into any gate. .smtc/analyzers/ exists and
// nothing in the Makefile or .github runs it. A spec-shaped check would be a second
// orphan that never detects anything, and a guard that does not run is theater.
//
// REGENERATION is available as `VB09_WRITE_GOLDEN=1 go test -run TestVB09 ./pkg/workflow/`
// and is DELIBERATELY ABSENT FROM THE FAILURE MESSAGE. A failure that names its own
// silencer teaches the reader to run it; the diff is meant to be argued, not regenerated.
//
// ---------------------------------------------------------------------------
// WHAT THE DERIVER CAN AND CANNOT SEE -- read this before trusting a green.
// ---------------------------------------------------------------------------
//
// The matcher is SYNTACTIC (stdlib go/ast; golang.org/x/tools is not a module dependency
// and a test-only import still lands in go.mod and go.sum for a library whose pitch is
// zero infrastructure). Without go/types, `data.SetNodeStatus(...)` cannot be resolved to
// (*WorkflowData).SetNodeStatus, so the selector name is matched on ANY receiver.
//
// DIRECTION OF THE RESIDUAL, STATED: this OVER-approximates. It can name a member that is
// not a real write to WorkflowData; it cannot MISS one for this reason. The
// over-approximation is kept sound by the ambiguity floors below, which assert that
// exactly one declaration of each matched name exists module-wide and red as AMBIGUOUS --
// rather than quietly widening -- if a second ever appears.
//
// The forms it declares it CANNOT see are listed in vb09Limitations and pinned as
// negative fixtures by TestVB09_ASTFilterFormsAreWitnessed. An AST filter silently drops
// the forms it does not match, so the completeness argument is not "the corpus is green"
// -- it is "each evasive form is witnessed against a synthetic source, and the ones that
// evade it are written down".
//
// BEFORE EDITING THE PROSE IN THIS FILE, read the note below it: this file sits inside
// another guard's population and its margin there is one word wide.

// 🔴 THIS FILE IS INSIDE boundary_claimscope_test.go's POPULATION, AND ITS MARGIN THERE IS
// ONE WORD WIDE.
//
// That guard sweeps every .go file in this directory and reds on comment blocks that
// restate the declared-boundary property too widely. Its subject test is a disjunction over
// the boundary vocabulary ANDed with one of its three role words, and the long comment
// above contains none of them -- so none of its blocks qualifies, and the terms on the
// guard's list that this file uses freely are never examined.
//
// The day a comment above starts using the guard's third role word -- a term this project's
// own tooling speaks constantly in its ordinary data-flow sense -- those blocks begin to
// qualify and the guard reds on prose making no claim about a declared boundary at all.
//
// TWO THINGS NOT TO DO IF THAT HAPPENS, both of which make it worse:
//
//   - Do NOT reach for the block-level opt-out that guard honours. It suppresses a WHOLE
//     comment block, and letting it spread over text it was never meant to cover is how a
//     guard becomes advisory. MEASURED, NOT HYPOTHETICAL: an earlier draft of this very
//     note mentioned that marker by name in prose, which ACTIVATED it -- the run then
//     listed this file's whole 60-line header as ALLOWED, exempting it from the guard
//     inside the note warning against exempting it.
//   - Do NOT widen the guard. Reword the new sentence instead.
//
// This note deliberately names neither the role word nor the marker, for exactly that
// reason. Recorded as F119-ENG-05 so a future red here is diagnosed rather than silenced.

const (
	vb09KindSetNodeStatus = "setnodestatus"
	vb09KindSetOutput     = "setoutput"
	vb09KindFieldWrite    = "fieldwrite"

	vb09StatusIndeterminate = "indeterminate"
	vb09StatusNotApplicable = "n/a"

	vb09GoldenPath = "testdata/vb09_terminal_writers.golden"

	vb09MediationFunc = "executeNodesInLevel"
	vb09TerminalFunc  = "isTerminalStatus"

	vb09FieldNodeStatus = "nodeStatus"
	vb09FieldOutputs    = "outputs"
)

// vb09NonTerminalStatuses is the EXPLICIT COMPLEMENT of the derived terminal set, and it
// is the only hand-written list in this guard. It is not an inherited answer: the terminal
// side is derived from isTerminalStatus's own AST body and cannot drift, and this side
// exists solely so that ADDING a tenth NodeStatus const without classifying it makes the
// partition floor red NAMING it. A number or a list that is asserted against a measurement
// is a check on the code; only one restated in prose is the defect.
//
// NodeStatus is NINE-state. CompensationFailed is the ninth and a prior hand-written
// census dropped it, which is why nothing here enumerates the terminal side by hand.
var vb09NonTerminalStatuses = map[string]bool{
	"Pending": true,
	"Running": true,
	"Waiting": true,
}

// vb09Limitations is the guard's own statement of the write forms it structurally cannot
// see. It is printed by the guard on every run and asserted by the form witness, so a
// matcher that starts seeing one of these reds here rather than silently widening.
func vb09Limitations() string {
	return strings.Join([]string{
		"VB-09 deriver -- DECLARED LIMITATIONS (syntactic AST match, no type resolution):",
		"  - a method VALUE call: `f := data.SetNodeStatus; f(n, Completed)` is NOT seen.",
		"    The call's Fun is a plain identifier and nothing syntactic ties it to the setter.",
		"  - a write reached through an interface or a func field is NOT seen, for the same reason.",
		"  - a closure DECLARED outside " + vb09MediationFunc + " and CALLED from inside it is counted",
		"    as UNMEDIATED: mediation is an AST position test, and a position test cannot follow a call.",
		"  - the receiver is not resolved, so the match is over-approximate on that axis; the",
		"    ambiguity floors keep that sound by refusing a second declaration of a matched name.",
		"  - a COMPOSITE-LITERAL field init is NOT seen: &WorkflowData{nodeStatus: map[...]{k: Completed}}.",
		"    The walk has arms for calls and assignments only, and this is neither. workflow_data.go",
		"    ALREADY uses this form for both fields -- as empty-map allocations, so nothing is lost",
		"    today, which is precisely what makes the form idiomatic here and the next one dangerous.",
		"  - a LOCAL ALIAS is NOT seen: m := w.nodeStatus; m[k] = Completed. The write's left-hand",
		"    side is a plain identifier and nothing syntactic ties it back to the field.",
		"  - a RANGE-ASSIGN is NOT seen: for w.nodeStatus[k], v = range src. It is an *ast.RangeStmt",
		"    and the walk never inspects one.",
	}, "\n")
}

// ---------------------------------------------------------------------------
// The corpus.
// ---------------------------------------------------------------------------

// vb09File is one parsed non-test source file of THIS module, keyed by its slash-separated
// path relative to the module root. The path is relative because it is part of a member's
// identity and an absolute path would make the golden machine-specific.
type vb09File struct {
	rel  string
	file *ast.File
}

// vb09ModuleRoot walks up from the test's working directory to the directory holding
// go.mod. The corpus is MODULE-WIDE and not package-wide on purpose: SetNodeStatus and
// SetOutput are EXPORTED, so a writer can live in any package of this module -- and one
// does, in pkg/testutil, whose files are not _test.go, which ships in the module and which
// a consumer can import. A sweep confined to this package directory would be blind to it.
func vb09ModuleRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("VB-09: no go.mod found above the test's working directory")
		}
		dir = parent
	}
}

// vb09ParseCorpus parses every non-test .go file of THIS module.
//
// 🔴 THE PRUNE RULES ARE THE GO TOOL'S OWN, NOT A HAND-CURATED EXCLUSION LIST, and each
// one is load-bearing at HEAD rather than defensive:
//
//   - a directory holding its OWN go.mod is a DIFFERENT MODULE and is pruned. The repo
//     contains several (playground/, examples/observability/, and spikes under _local/).
//     Sweeping them would put another module's sources in this module's population.
//   - a directory whose name begins with "." or "_" is IGNORED BY THE GO TOOL ITSELF and is
//     pruned here for the same reason it is not compiled. This is the decisive one: .claude/
//     holds complete agent WORKTREE CHECKOUTS of this repository. Without this rule the
//     golden would gain a duplicate of every member per live worktree and would change
//     whenever an agent worktree appeared or vanished -- a guard that reds for reasons
//     unrelated to its property gets silenced by regeneration, which is the exact failure
//     the golden's design is meant to avoid.
//   - testdata/ and vendor/ are pruned by the same toolchain convention.
//
// The rules are cited to the toolchain rather than to a list of names someone maintains.
// Measured at 41f91fd: the surviving directory set is the one `go list ./...` reports.
//
// BOUND, STATED: build tags are NOT considered (go/parser does not, and parser.ParseDir is
// deprecated as of Go 1.25 for exactly this reason). A file excluded from the build by a
// tag is still in this population. That direction is over-approximate and therefore safe
// for a guard whose failure mode is missing a member; the same bound is documented on the
// oracle's sweep in this package.
func vb09ParseCorpus(root string, fset *token.FileSet) ([]vb09File, error) {
	var out []vb09File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" {
				return fs.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("VB-09: parsing %s: %w", path, parseErr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, vb09File{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// ---------------------------------------------------------------------------
// Axis 1 -- the terminal status set, derived from isTerminalStatus's own body.
// ---------------------------------------------------------------------------

// vb09NodeStatusConsts returns every identifier declared as a NodeStatus constant,
// derived from the const block rather than enumerated. Enumerating by hand is how
// CompensationFailed, the ninth state, has been dropped before.
//
// 🔴 IT IS SCOPED TO THE ENGINE'S OWN PACKAGE DIRECTORY, AND THAT IS NOT TIDINESS --
// IT IS A DEFECT THIS GUARD'S PARTITION FLOOR CAUGHT ON ITS FIRST RUN. A second,
// entirely unrelated `NodeStatus` type exists in this module:
// internal/workflow/fb/workflow/NodeStatus.go declares the generated FlatBuffers wire
// enum, `type NodeStatus int8`, with its own nine constants -- and its package is ALSO
// named `workflow`, so the package NAME does not separate them either. A module-wide
// sweep keyed on the type identifier swept those nine in and reported every one of them
// unaccounted. Go packages are directories, so the directory holding isTerminalStatus is
// what identifies the engine's NodeStatus, and it is derived rather than named here.
//
// The call and field matchers stay MODULE-wide on purpose -- their names are exported and
// a writer can live anywhere -- and their soundness is carried by the ambiguity floors
// instead. This axis is different: the type is not what is being matched, it is what
// gives the matched values their meaning, and two types of the same name have no shared
// meaning to give.
func vb09NodeStatusConsts(files []vb09File) map[string]bool {
	consts := map[string]bool{}
	for _, vf := range vb09EnginePackage(files) {
		for _, decl := range vf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "NodeStatus" {
					continue
				}
				for _, n := range vs.Names {
					consts[n.Name] = true
				}
			}
		}
	}
	return consts
}

// vb09EnginePackage returns the corpus files that share a DIRECTORY with the single
// declaration of isTerminalStatus -- the engine's own package, derived rather than named.
// It returns the whole corpus when that declaration is not found, so the callers that
// depend on it report the missing declaration through their own error rather than through
// an empty set here.
func vb09EnginePackage(files []vb09File) []vb09File {
	dir := ""
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == vb09TerminalFunc {
				dir = filepath.Dir(vf.rel)
			}
		}
	}
	if dir == "" {
		return files
	}
	var out []vb09File
	for _, vf := range files {
		if filepath.Dir(vf.rel) == dir {
			out = append(out, vf)
		}
	}
	return out
}

// vb09TypeDeclsNamed returns "path:Name" for every type declaration of the given name in
// the files handed to it. It is the const axis's ambiguity control: scoped to the engine's
// package it must find exactly one NodeStatus, and a second one there would make the
// constant set mean two things at once.
func vb09TypeDeclsNamed(files []vb09File, name string) []string {
	var out []string
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					out = append(out, vf.rel+":"+name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// vb09TerminalStatusSet derives the terminal states from isTerminalStatus's AST body.
//
// 🔴 IT ERRORS RATHER THAN RETURNING A PARTIAL SET. If the body stops being a single
// return of a disjunction of `status == <ident>` comparisons -- a switch, an early return,
// a lookup table -- the deriver refuses. A filter that returns {} for a shape it did not
// understand produces a guard that is green over an empty population, and a pass predicate
// over the empty set returns true.
func vb09TerminalStatusSet(files []vb09File) (map[string]bool, error) {
	var bodies []*ast.BlockStmt
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != vb09TerminalFunc {
				continue
			}
			bodies = append(bodies, fd.Body)
		}
	}
	if len(bodies) != 1 {
		return nil, fmt.Errorf("VB-09: expected exactly ONE %s declaration in the module, found %d; "+
			"the terminal-status axis is ambiguous and the deriver refuses to pick one",
			vb09TerminalFunc, len(bodies))
	}
	body := bodies[0]
	if body == nil || len(body.List) != 1 {
		return nil, fmt.Errorf("VB-09: %s's body is no longer a single statement; the deriver does not "+
			"understand its shape and REFUSES rather than returning a partial terminal set", vb09TerminalFunc)
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil, fmt.Errorf("VB-09: %s's body is not a single return of one expression; the deriver "+
			"does not understand its shape and REFUSES rather than returning a partial terminal set",
			vb09TerminalFunc)
	}
	set := map[string]bool{}
	if err := vb09CollectDisjunction(ret.Results[0], set); err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("VB-09: %s yielded an EMPTY terminal set; the deriver is broken, not the tree",
			vb09TerminalFunc)
	}
	return set, nil
}

func vb09CollectDisjunction(e ast.Expr, out map[string]bool) error {
	if be, ok := vb09Unparen(e).(*ast.BinaryExpr); ok {
		switch be.Op {
		case token.LOR:
			if err := vb09CollectDisjunction(be.X, out); err != nil {
				return err
			}
			return vb09CollectDisjunction(be.Y, out)
		case token.EQL:
			lhs, lok := vb09Unparen(be.X).(*ast.Ident)
			rhs, rok := vb09Unparen(be.Y).(*ast.Ident)
			if lok && rok && lhs.Name == "status" {
				out[rhs.Name] = true
				return nil
			}
		}
	}
	return fmt.Errorf("VB-09: %s's body is not a disjunction of `status == <ident>` comparisons; the "+
		"deriver does not understand this shape and REFUSES rather than returning a partial terminal set",
		vb09TerminalFunc)
}

// ---------------------------------------------------------------------------
// Axis 2 -- the mediation boundary.
// ---------------------------------------------------------------------------

// vb09Interval is executeNodesInLevel's lexical extent. A site is MEDIATED iff its
// position lies inside it.
//
// FUNCTION LITERALS LEXICALLY INSIDE THE BODY COUNT AS INSIDE -- the executor's launch
// goroutine is one, and a write in it is as mediated as a write in the surrounding
// statement list. A closure DECLARED elsewhere and CALLED from inside is NOT detectable by
// a position test and is counted as unmediated; that limitation is declared in
// vb09Limitations rather than left for a reader to infer.
type vb09Interval struct {
	rel        string
	start, end token.Pos
}

func (iv vb09Interval) contains(p token.Pos) bool { return p >= iv.start && p < iv.end }

func vb09MediationInterval(files []vb09File) (vb09Interval, error) {
	var found []vb09Interval
	var where []string
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != vb09MediationFunc {
				continue
			}
			found = append(found, vb09Interval{rel: vf.rel, start: fd.Pos(), end: fd.End()})
			where = append(where, vf.rel)
		}
	}
	if len(found) != 1 {
		return vb09Interval{}, fmt.Errorf("VB-09: expected exactly ONE %s declaration in the module, "+
			"found %d (%s); \"outside %s\" is ambiguous with more than one and the guard refuses to pick",
			vb09MediationFunc, len(found), strings.Join(where, ", "), vb09MediationFunc)
	}
	return found[0], nil
}

// ---------------------------------------------------------------------------
// Axis 3 -- the write sites.
// ---------------------------------------------------------------------------

// vb09Site is one syntactic write, before the terminal and mediation filters.
type vb09Site struct {
	rel, symbol, kind, status string
	field                     string // "nodeStatus", "outputs" or "" for a setter call
	pos                       token.Pos
	loc                       token.Position
}

// vb09Sites collects every syntactic write of node status or node output.
//
// 🔴 THE POPULATION IS KEYED ON THE STATE, NOT ON THE API NAME (DEC-119-POPULATION-KEYED-
// ON-STATE). It is the union of calls to SetNodeStatus/SetOutput AND direct writes to the
// nodeStatus/outputs maps, including whole-map reassignment. Measured at 41f91fd,
// LoadSnapshot and Clone write statuses straight into the map with no call to the setter:
// a deriver keyed on the setter NAME would report those clean, and an unmediated writer
// that bypasses the front door is precisely the member this guard exists to notice.
//
// AN UNDECIDABLE STATUS ARGUMENT IS INCLUDED AND MARKED indeterminate, NEVER DROPPED. A
// filter that required a terminal-const identifier would silently drop the conversion and
// variable forms that exist at HEAD, and silence in the flattering direction is the one
// failure mode a bound must not have. indeterminate is a distinct, reviewable member kind,
// not an absence.
func vb09Sites(fset *token.FileSet, files []vb09File, consts map[string]bool,
	engineImportPath, engineDir string) []vb09Site {
	var out []vb09Site
	for _, vf := range files {
		vf := vf
		qualifiers := vb09EngineQualifiers(vf, engineImportPath)
		inEngine := filepath.Dir(vf.rel) == engineDir
		add := func(pos token.Pos, kind, status, field string) {
			out = append(out, vb09Site{
				rel:    vf.rel,
				symbol: vb09EnclosingSymbol(vf, pos),
				kind:   kind,
				status: status,
				field:  field,
				pos:    pos,
				loc:    fset.Position(pos),
			})
		}
		ast.Inspect(vf.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// The callee is matched through parentheses on ANY receiver shape:
				// (data).SetOutput, s.d.SetNodeStatus, arr[0].SetNodeStatus and
				// get().SetNodeStatus all present as a SelectorExpr once the call's
				// Fun is unparenthesised. A plain-identifier callee -- the method-value
				// form -- is NOT matched and is declared in vb09Limitations.
				sel, ok := vb09Unparen(node.Fun).(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					return true
				}
				switch sel.Sel.Name {
				case "SetNodeStatus":
					status := vb09StatusIndeterminate
					if len(node.Args) >= 2 {
						status = vb09StatusOf(node.Args[1], consts, qualifiers, inEngine)
					}
					add(sel.Sel.Pos(), vb09KindSetNodeStatus, status, vb09FieldNodeStatus)
				case "SetOutput":
					add(sel.Sel.Pos(), vb09KindSetOutput, vb09StatusNotApplicable, vb09FieldOutputs)
				}
			case *ast.AssignStmt:
				// Tuple assignment is walked per left-hand side, so
				// `w.nodeStatus[k], w.outputs[k] = Completed, v` yields BOTH members.
				// When the right-hand side is a single multi-value expression the
				// per-position value is undecidable and the status is indeterminate.
				paired := len(node.Rhs) == len(node.Lhs)
				for i, lhs := range node.Lhs {
					field := vb09FieldTarget(lhs)
					if field == "" {
						continue
					}
					status := vb09StatusNotApplicable
					if field == vb09FieldNodeStatus {
						if paired {
							status = vb09StatusOf(node.Rhs[i], consts, qualifiers, inEngine)
						} else {
							status = vb09StatusIndeterminate
						}
					}
					add(lhs.Pos(), vb09KindFieldWrite, status, field)
				}
			}
			return true
		})
	}
	return out
}

// vb09FieldTarget reports which WorkflowData map an assignment's left-hand side writes:
// "nodeStatus", "outputs", or "" for anything else. It matches both the indexed form
// (w.nodeStatus[k] = ...) and whole-map reassignment (w.nodeStatus = make(...)).
func vb09FieldTarget(lhs ast.Expr) string {
	switch v := vb09Unparen(lhs).(type) {
	case *ast.IndexExpr:
		return vb09FieldTarget(v.X)
	case *ast.SelectorExpr:
		if v.Sel != nil && (v.Sel.Name == vb09FieldNodeStatus || v.Sel.Name == vb09FieldOutputs) {
			return v.Sel.Name
		}
	}
	return ""
}

// vb09StatusOf resolves a status argument to a declared NodeStatus constant, or to
// indeterminate. It never drops the site.
//
// IT RESOLVES THE PACKAGE-QUALIFIED FORM, `workflow.Completed`, AND THAT IS LOAD-BEARING
// RATHER THAN A CONVENIENCE. Every consumer outside the engine's own package writes the
// qualified form -- examples/new_simple/main.go writes `workflow.Running` and
// `workflow.Completed` -- so an Ident-only resolver marks all of them indeterminate. That
// direction is safe (indeterminate is INCLUDED) but it is not honest: the non-terminal
// filter never sees them, so Running writes land in the golden looking like undecidable
// terminal ones, and `indeterminate` stops meaning "the deriver could not decide".
//
// 🔴 IT RESOLVES A QUALIFIER ONLY WHEN THAT FILE IMPORTS THE ENGINE'S PACKAGE UNDER THAT
// NAME. This is the one place resolution can move a site OUT of the population (a status
// resolved to Running is excluded as non-terminal), so it is the one place a wrong guess
// would lose a member rather than add one. Matching `<anything>.Completed` would do exactly
// that for an unrelated package's identically-named constant. The import list is the file's
// own statement of which package that qualifier means, so it is what gets consulted.
func vb09StatusOf(e ast.Expr, consts map[string]bool, engineQualifiers map[string]bool, inEngine bool) string {
	if e == nil {
		return vb09StatusIndeterminate
	}
	switch v := vb09Unparen(e).(type) {
	case *ast.Ident:
		// 🔴 THE PACKAGE GATE HERE IS NOT SYMMETRY FOR ITS OWN SAKE (119-F2, found by review).
		// An UNQUALIFIED identifier can only denote the engine's constant inside the engine's
		// own package, or in a file that dot-imports it. Resolving it anywhere else means a
		// consumer package that happens to declare `Running` -- its own, unrelated constant --
		// has its SetNodeStatus site resolved as non-terminal and DROPPED from the population.
		// Reproduced with a control: the same file with the identifier renamed to Zork keeps
		// its member; named Running it loses it, and the name collision alone is the
		// difference. Collisions on TERMINAL names only mislabel, which reds; only the three
		// non-terminal names lose a member, and that is silence in the flattering direction --
		// the one failure mode this guard's own doc says it must not have. The Selector branch
		// below was gated for exactly this reason and this branch, four lines above it, was not.
		if consts[v.Name] && (inEngine || engineQualifiers["."]) {
			return strings.ToLower(v.Name)
		}
	case *ast.SelectorExpr:
		qualifier, ok := vb09Unparen(v.X).(*ast.Ident)
		if ok && v.Sel != nil && engineQualifiers[qualifier.Name] && consts[v.Sel.Name] {
			return strings.ToLower(v.Sel.Name)
		}
	}
	return vb09StatusIndeterminate
}

// vb09ModulePath reads the module path from go.mod. It is half of the engine package's
// import path; the other half is the directory that declares isTerminalStatus.
func vb09ModulePath(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("VB-09: go.mod at %s declares no module path", root)
}

// vb09EngineQualifiers reports, per file, the local identifiers that name the engine's
// package in that file -- the import's explicit name when it has one, otherwise the last
// segment of its path. A file inside the engine package itself has none and needs none,
// because there the constants are unqualified identifiers.
func vb09EngineQualifiers(vf vb09File, engineImportPath string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range vf.file.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		if path != engineImportPath {
			continue
		}
		switch {
		case imp.Name != nil && imp.Name.Name == "_":
			// A blank import binds no name.
		case imp.Name != nil:
			names[imp.Name.Name] = true
		default:
			names[path[strings.LastIndex(path, "/")+1:]] = true
		}
	}
	return names
}

func vb09Unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// vb09EnclosingSymbol names the innermost function declaration containing a position, so a
// member is cited by SYMBOL rather than by line. A file:line drifts on every edit above it;
// a symbol does not.
func vb09EnclosingSymbol(vf vb09File, pos token.Pos) string {
	symbol := "<file-level>"
	for _, decl := range vf.file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fd.Pos() || pos >= fd.End() {
			continue
		}
		symbol = vb09FuncName(fd)
	}
	return symbol
}

func vb09FuncName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return "(" + vb09TypeString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

// vb09TypeString renders a receiver type. *ast.StarExpr is handled explicitly because
// dropping it is the classic AST-filter miss: a filter that takes only *ast.Ident silently
// loses every pointer-receiver method.
func vb09TypeString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return "*" + vb09TypeString(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.ParenExpr:
		return vb09TypeString(v.X)
	case *ast.IndexExpr: // generic receiver: T[P]
		return vb09TypeString(v.X)
	case *ast.IndexListExpr: // generic receiver: T[P, Q]
		return vb09TypeString(v.X)
	}
	return "?"
}

// ---------------------------------------------------------------------------
// Members: the filtered population, keyed line-free.
// ---------------------------------------------------------------------------

// vb09Member is one member of VB-09's population. Its KEY carries no line number
// (DEC-119-GOLDEN-KEY-IS-LINE-FREE): a line-keyed golden reds on every unrelated edit
// above a member, and a guard that reds for reasons unrelated to its property gets
// silenced by regeneration. The occurrence index is required rather than cosmetic --
// measured at 41f91fd, (*Node).execute contains several writes that collide on every other
// component of the key. Line and column ride along as a LOCATOR for the failure message;
// they are navigation, never identity.
type vb09Member struct {
	path, symbol, kind, status string
	occ                        int
	loc                        token.Position
}

func (m vb09Member) key() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t#%d", m.path, m.symbol, m.kind, m.status, m.occ)
}

// vb09Derivation is one full derivation over a corpus, with the counts the anti-vacuity
// floors are asserted on. The excluded counts are computed INDEPENDENTLY over the whole
// site set rather than sequentially, so a site that is both mediated and non-terminal
// contributes to both floors and neither floor can be starved by the other's filter
// running first.
type vb09Derivation struct {
	files               int
	sites               int
	members             []vb09Member
	excludedMediated    int
	excludedNonTerminal int
	unaccounted         []string
	terminal            map[string]bool
	consts              map[string]bool
	interval            vb09Interval
}

func vb09Derive(fset *token.FileSet, files []vb09File, engineImportPath, engineDir string) (vb09Derivation, error) {
	d := vb09Derivation{files: len(files)}

	consts := vb09NodeStatusConsts(files)
	d.consts = consts

	terminal, err := vb09TerminalStatusSet(files)
	if err != nil {
		return d, err
	}
	d.terminal = terminal

	// The partition: every operand of isTerminalStatus must be a declared NodeStatus
	// const, and every declared const must be classified as terminal or as non-terminal.
	// A tenth const that is neither is UNACCOUNTED and is reported by name.
	for name := range terminal {
		if !consts[name] {
			return d, fmt.Errorf("VB-09: %s admits %q, which is not a declared NodeStatus constant",
				vb09TerminalFunc, name)
		}
	}
	for name := range consts {
		if !terminal[name] && !vb09NonTerminalStatuses[name] {
			d.unaccounted = append(d.unaccounted, name)
		}
	}
	sort.Strings(d.unaccounted)

	interval, err := vb09MediationInterval(files)
	if err != nil {
		return d, err
	}
	d.interval = interval

	// terminalLower was BUILT AND NEVER READ (found by review). Exclusion consulted only the
	// hand-written non-terminal list, which is equivalent at HEAD because floor 7 forces the
	// partition to be total -- but it read as though the DERIVED terminal set drove inclusion
	// when nothing derived was in the path at all. It is now the thing consulted, with the
	// hand-written list kept as the complement that makes an unclassified addition red.
	terminalLower := map[string]bool{}
	for name := range terminal {
		terminalLower[strings.ToLower(name)] = true
	}
	nonTerminalLower := map[string]bool{}
	for name := range vb09NonTerminalStatuses {
		nonTerminalLower[strings.ToLower(name)] = true
	}
	sites := vb09Sites(fset, files, consts, engineImportPath, engineDir)
	d.sites = len(sites)

	var kept []vb09Site
	for _, s := range sites {
		mediated := interval.contains(s.pos)
		// EXCLUDE ONLY A STATUS THAT IS EXPLICITLY CLASSIFIED NON-TERMINAL AND IS NOT ALSO
		// ADMITTED BY THE DERIVED TERMINAL SET. Both conjuncts are load-bearing:
		//
		//   - the hand-written list must be consulted, because an UNACCOUNTED status (a tenth
		//     NodeStatus nobody classified) has to stay INCLUDED. Dropping what the deriver
		//     cannot classify is silence in the flattering direction; floor 7 reds on it by
		//     name instead.
		//   - the DERIVED set must also be consulted, and until review found terminalLower
		//     built-and-never-read it was not. It is what catches the two sets OVERLAPPING:
		//     add Bypassed to vb09NonTerminalStatuses while isTerminalStatus still admits it
		//     and, without this conjunct, every Bypassed write silently leaves the population.
		//     Floor 7 checks totality and cannot see an overlap.
		nonTerminal := s.field == vb09FieldNodeStatus &&
			nonTerminalLower[s.status] && !terminalLower[s.status]
		if mediated {
			d.excludedMediated++
		}
		if nonTerminal {
			d.excludedNonTerminal++
		}
		if mediated || nonTerminal {
			continue
		}
		kept = append(kept, s)
	}

	d.members = vb09Members(kept)
	return d, nil
}

// vb09Members assigns each site its occurrence index within its (path, symbol, kind,
// status) group and returns the members sorted byte-wise by key -- the golden's order.
func vb09Members(sites []vb09Site) []vb09Member {
	ordered := append([]vb09Site(nil), sites...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		switch {
		case a.rel != b.rel:
			return a.rel < b.rel
		case a.symbol != b.symbol:
			return a.symbol < b.symbol
		case a.kind != b.kind:
			return a.kind < b.kind
		case a.status != b.status:
			return a.status < b.status
		case a.loc.Line != b.loc.Line:
			return a.loc.Line < b.loc.Line
		default:
			return a.loc.Column < b.loc.Column
		}
	})

	seen := map[string]int{}
	out := make([]vb09Member, 0, len(ordered))
	for _, s := range ordered {
		group := s.rel + "\x00" + s.symbol + "\x00" + s.kind + "\x00" + s.status
		occ := seen[group]
		seen[group]++
		out = append(out, vb09Member{
			path:   s.rel,
			symbol: s.symbol,
			kind:   s.kind,
			status: s.status,
			occ:    occ,
			loc:    s.loc,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ---------------------------------------------------------------------------
// The golden file.
// ---------------------------------------------------------------------------

const vb09GoldenHeader = `# VB-09 -- the golden set of non-test sites that write a terminal NodeStatus or a node
# output OUTSIDE executeNodesInLevel.
#
# A MEMBER IS A CLAIM ABOUT THIS ENGINE'S COMPLETE-MEDIATION PROPERTY. Each line is a place
# node state reaches a terminal value without passing the executor's mediation point. Some
# are legitimate -- restoring a persisted run is one -- but every one of them is an
# assertion that the executor is not the only writer, and a NEW one has to be argued.
#
# ADDING A LINE HERE IS NOT A FIX. It is a claim, and it belongs in a pull request with the
# reason the new writer is sound. Removing a line is equally a claim: confirm the site was
# removed rather than renamed or moved behind an indirection this AST deriver cannot follow.
#
# THIS FILE IS GENERATED BUT NEVER AUTO-UPDATED. It is regenerated only by a deliberate,
# separately documented act; nothing in the failing path will offer to do it for you.
#
# FORMAT, one member per line, tab-separated, sorted byte-wise:
#   <path>\t<enclosing symbol>\t<kind>\t<status>\t#<occurrence within that group>
# There is NO LINE NUMBER in the key. A line-keyed golden reds on every unrelated edit above
# a member; line and column appear only in the failure message, as a locator.
# status "n/a" marks an output write; "indeterminate" marks a status argument the syntactic
# deriver cannot decide -- included on purpose, never dropped.
`

func vb09ReadGolden(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys, nil
}

func vb09WriteGolden(path string, members []vb09Member) error {
	var b strings.Builder
	b.WriteString(vb09GoldenHeader)
	for _, m := range members {
		b.WriteString(m.key())
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// ---------------------------------------------------------------------------
// The guard.
// ---------------------------------------------------------------------------

// TestVB09_TerminalWriterSetMatchesTheGolden is VB-09.
//
// 🔴 THIS GUARD'S POPULATION IS THE MODULE, so a `-run` filter aimed anywhere else
// deselects it entirely. A scoped green is not evidence that it passed; only a package-wide
// run selects it. That is not a style note -- it has fired three times on this milestone at
// commits whose own scoped runs were fully green.
func TestVB09_TerminalWriterSetMatchesTheGolden(t *testing.T) {
	root, err := vb09ModuleRoot()
	require.NoError(t, err)

	fset := token.NewFileSet()
	files, err := vb09ParseCorpus(root, fset)
	require.NoError(t, err)

	// -----------------------------------------------------------------------
	// Anti-vacuity floors, ON THE VERDICT. A pass predicate over the empty set
	// returns true, and a machine consumer cannot read a scope string -- so each
	// floor is its own require with its own message, and every message says the
	// SWEEP is broken rather than telling the reader to adjust the floor.
	//
	// 🔴 THE CORPUS FLOORS RUN BEFORE THE DERIVATION, AND THE ORDER IS LOAD-BEARING.
	// A collapsed corpus makes the DERIVER fail first -- it finds no isTerminalStatus
	// and errors -- so with the derivation first these two floors are unreachable and a
	// walk pointed at the wrong directory reports a missing declaration instead of a
	// missing corpus. A guard placed before a cheaper, more specific check substitutes
	// its error for the one the reader needs.
	// -----------------------------------------------------------------------

	// Floor 1 -- the corpus was actually walked. The floor sits well below the measured
	// population so a legitimate file removal never trips it; its job is to catch a walk
	// that collapsed, not to pin a count.
	require.Greater(t, len(files), 50,
		"the corpus walk parsed only %d non-test file(s); the SWEEP is BROKEN, not the tree -- "+
			"this module has many times that across pkg/, internal/ and examples/", len(files))

	// Floor 1b -- the corpus is MODULE-wide, not package-wide, and that is a separate
	// property from its size: this package alone holds more non-test files than floor 1's
	// threshold, so a sweep that collapsed to its own directory would sail past it. The
	// setters are EXPORTED and a writer can live in any package of this module -- one does,
	// in pkg/testutil, whose files are not _test.go and which a consumer can import.
	dirs := map[string]bool{}
	outside := 0
	for _, f := range files {
		dirs[filepath.Dir(f.rel)] = true
		if !strings.HasPrefix(f.rel, "pkg/workflow/") {
			outside++
		}
	}
	require.Greater(t, len(dirs), 1,
		"the corpus spans only %d directory: the walk collapsed to one package and the SWEEP is "+
			"BROKEN, not the tree", len(dirs))
	require.NotZero(t, outside,
		"every corpus file is under pkg/workflow/: the walk is PACKAGE-scoped and this guard's "+
			"population is the MODULE -- the SWEEP is BROKEN, not the tree")

	modulePath, err := vb09ModulePath(root)
	require.NoError(t, err)
	engineDir := "."
	if pkg := vb09EnginePackage(files); len(pkg) > 0 {
		engineDir = filepath.Dir(pkg[0].rel)
	}
	engineImportPath := modulePath + "/" + filepath.ToSlash(engineDir)

	d, err := vb09Derive(fset, files, engineImportPath, filepath.ToSlash(engineDir))
	require.NoError(t, err)

	t.Log(vb09Limitations())
	t.Logf("engine package resolved to %s", engineImportPath)

	// Floor 2 -- the mediation function is unambiguous. (Zero or two declarations is
	// reported by the deriver itself, above; this pins that the interval is real.)
	require.NotZero(t, d.interval.start,
		"the mediation interval is the zero position: the %s lookup is BROKEN, not the tree",
		vb09MediationFunc)

	// Floor 3 -- THE HIGHEST-VALUE FLOOR. At least one site is excluded FOR BEING
	// MEDIATED. This pins the input axis of the one predicate that cannot otherwise
	// announce its own failure: a containment test that matches nothing produces a
	// green from a filter that filters nothing, and the population silently WIDENS
	// to include the mediated writes it exists to exclude.
	require.NotZero(t, d.excludedMediated,
		"no site was excluded for being inside %s, and there are writes in it: the mediation "+
			"filter is BROKEN, not the tree -- it has silently widened and is filtering nothing",
		vb09MediationFunc)

	// Floor 4 -- the same argument on the status axis.
	require.NotZero(t, d.excludedNonTerminal,
		"no site was excluded for writing a NON-TERMINAL status, and this engine writes Running "+
			"and Pending: the terminal filter is BROKEN, not the tree")

	// Floor 5a -- every KIND the deriver claims to match is represented. A matcher that
	// silently stops matching a whole form otherwise shows up only as members vanishing,
	// which reads identically to the code being deleted.
	byKind := map[string]int{}
	byStatus := map[string]int{}
	for _, m := range d.members {
		byKind[m.kind]++
		byStatus[m.status]++
	}
	for _, kind := range []string{vb09KindSetNodeStatus, vb09KindSetOutput, vb09KindFieldWrite} {
		require.NotZero(t, byKind[kind],
			"the deriver claims to match %q and derived no member of that kind: the MATCHER is "+
				"BROKEN, not the tree", kind)
	}

	// Floor 5b -- indeterminate is a STATUS, not a kind, and it needs its own floor.
	// Dropping an undecidable status argument is the flattering-direction failure this
	// guard must not have, and it would be invisible to the per-kind floor above.
	require.NotZero(t, byStatus[vb09StatusIndeterminate],
		"no member carries an indeterminate status, and this module writes statuses the syntactic "+
			"deriver cannot decide: undecidable arguments are being DROPPED rather than marked, "+
			"which is the SWEEP being broken in the flattering direction")

	// Floor 6 -- the P-04 ambiguity control on the CALL axis. The matcher is syntactic and
	// cannot resolve a receiver, so it is sound only while exactly one declaration of each
	// matched name exists. A second one makes the match AMBIGUOUS and this reds rather than
	// letting the population quietly widen.
	for _, name := range []string{"SetNodeStatus", "SetOutput"} {
		decls := vb09DeclarationsNamed(files, name)
		require.Len(t, decls, 1,
			"the syntactic matcher assumes exactly ONE declaration of %s module-wide and found %d "+
				"(%s); the match has become AMBIGUOUS -- resolve it rather than widening the population",
			name, len(decls), strings.Join(decls, ", "))
	}

	// Floor 6b -- the SAME control on the FIELD axis, which the call-axis control does not
	// cover. A second type declaring a nodeStatus or outputs map field would widen the
	// fieldwrite matcher exactly the way a second SetNodeStatus would widen the call matcher.
	for _, name := range []string{vb09FieldNodeStatus, vb09FieldOutputs} {
		decls := vb09MapFieldsNamed(files, name)
		require.Len(t, decls, 1,
			"the fieldwrite matcher assumes exactly ONE declaration of a map field named %q "+
				"module-wide and found %d (%s); the match has become AMBIGUOUS",
			name, len(decls), strings.Join(decls, ", "))
	}

	// Floor 6c -- the CONST axis's ambiguity control, scoped to the engine's own package.
	// Module-wide this name is genuinely ambiguous at HEAD: the generated FlatBuffers wire
	// enum in internal/workflow/fb/workflow is also a `NodeStatus`, in a package also named
	// `workflow`. That is legitimate and is not what this floor guards. What it guards is a
	// SECOND NodeStatus appearing in the ENGINE's package, where the constant set would
	// then mean two things at once and the terminal partition would be over a union of them.
	engineTypes := vb09TypeDeclsNamed(vb09EnginePackage(files), "NodeStatus")
	require.Len(t, engineTypes, 1,
		"the engine's package must declare exactly ONE NodeStatus type and declares %d (%s); "+
			"the terminal partition would be computed over a union of two unrelated constant sets",
		len(engineTypes), strings.Join(engineTypes, ", "))

	// Floor 7 -- the partition is total, and neither set is empty.
	require.Empty(t, d.unaccounted,
		"NodeStatus constant(s) %v are classified neither terminal by %s nor non-terminal by "+
			"vb09NonTerminalStatuses. A new state must be CLASSIFIED, not absorbed: until it is, this "+
			"guard cannot say whether a write of it belongs in the population",
		d.unaccounted, vb09TerminalFunc)
	require.NotEmpty(t, d.members, "the derived member set is EMPTY: the deriver is BROKEN, not the tree")

	golden, err := vb09ReadGolden(vb09GoldenPath)
	if os.Getenv("VB09_WRITE_GOLDEN") == "1" {
		require.NoError(t, vb09WriteGolden(vb09GoldenPath, d.members))
		t.Fatalf("VB09_WRITE_GOLDEN=1: %s rewritten. Review the diff and argue it; "+
			"re-run without the variable to gate.", vb09GoldenPath)
	}
	require.NoError(t, err, "the golden file must be committed and readable")
	require.NotEmpty(t, golden, "the golden file holds no members: it is BROKEN, not the tree")

	// -----------------------------------------------------------------------
	// The verdict: SET DIFFERENCE in both directions, naming members. Never a count.
	// -----------------------------------------------------------------------
	if msg := vb09Diff(golden, d.members); msg != "" {
		t.Fatal(msg)
	}
}

// vb09Diff renders the two-directional set difference, or "" when the sets agree.
//
// THE MESSAGE NAMES WHICH MEMBERS CHANGED, ON BOTH SIDES, AND STATES NO COUNT. It also
// does not name the regeneration path: the diff is meant to be argued in review, and a
// failure that offers its own silencer teaches the reader to reach for it.
func vb09Diff(golden []string, members []vb09Member) string {
	inGolden := map[string]bool{}
	for _, k := range golden {
		inGolden[k] = true
	}
	locOf := map[string]token.Position{}
	pathOf := map[string]string{}
	inDerived := map[string]bool{}
	for _, m := range members {
		inDerived[m.key()] = true
		locOf[m.key()] = m.loc
		pathOf[m.key()] = m.path
	}

	var added, removed []string
	for _, m := range members {
		if !inGolden[m.key()] {
			added = append(added, m.key())
		}
	}
	for _, k := range golden {
		if !inDerived[k] {
			removed = append(removed, k)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(removed)

	var b strings.Builder
	b.WriteString("VB-09: the set of non-test sites writing a terminal NodeStatus or node output\n")
	b.WriteString("outside " + vb09MediationFunc + " has CHANGED.\n")
	if len(added) > 0 {
		b.WriteString("\nADDED (each of these writes terminal node state without passing through the\n")
		b.WriteString("executor's mediation point -- that is a claim about this engine's complete-mediation\n")
		b.WriteString("property and it needs a justification in the PR, not a golden-file edit):\n")
		for _, k := range added {
			// The locator is rendered from the member's RELATIVE path, not from the
			// FileSet's absolute filename: an absolute path is machine-specific noise in a
			// message a reviewer reads, and the relative path is already the member's
			// identity. Line and column are navigation only -- they are not in the key.
			loc := locOf[k]
			fmt.Fprintf(&b, "  + %s    @ %s:%d:%d\n",
				k, pathOf[k], loc.Line, loc.Column)
		}
	}
	if len(removed) > 0 {
		b.WriteString("\nREMOVED (a mediation site disappeared; confirm it was removed rather than renamed\n")
		b.WriteString("or moved behind an indirection this AST deriver cannot follow):\n")
		for _, k := range removed {
			b.WriteString("  - " + k + "\n")
		}
	}
	b.WriteString("\n" + vb09Limitations() + "\n")
	return b.String()
}

// vb09DeclarationsNamed returns "path:(recv).name" for every function or method
// declaration of the given name -- the P-04 ambiguity control's instrument.
func vb09DeclarationsNamed(files []vb09File, name string) []string {
	var out []string
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != name {
				continue
			}
			out = append(out, vf.rel+":"+vb09FuncName(fd))
		}
	}
	sort.Strings(out)
	return out
}

// vb09MapFieldsNamed returns "path:TypeName.field" for every struct field of the given
// name whose type is a map -- the field-axis counterpart of the ambiguity control.
func vb09MapFieldsNamed(files []vb09File, name string) []string {
	var out []string
	for _, vf := range files {
		for _, decl := range vf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				for _, f := range st.Fields.List {
					if _, isMap := f.Type.(*ast.MapType); !isMap {
						continue
					}
					for _, n := range f.Names {
						if n.Name == name {
							out = append(out, vf.rel+":"+ts.Name.Name+"."+n.Name)
						}
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The D-04 completeness witness: the deriver's filters against a SYNTHETIC SOURCE.
// ---------------------------------------------------------------------------
//
// AN AST FILTER SILENTLY DROPS THE FORMS IT DOES NOT MATCH, and a deriver that is green
// over a corpus which happens to contain none of the evasive forms proves nothing. So the
// completeness argument is not the corpus -- it is this table. Each row carries one
// receiver, status or assignment form, and asserts the EXACT member set the derivers
// produce for it. The known-answer expectation is legitimate here and only here: the
// derivation itself is the thing under test, so the expected value IS the instrument.
//
// THE NEGATIVE ROWS ARE HALF THE ARGUMENT. A witness that only pins what the filter sees
// cannot notice the filter quietly widening, and it lets the declared limitations rot. F6
// pins a form the deriver CANNOT see and asserts that vb09Limitations still says so; the
// day that form starts being matched, this reds and the limitation text gets corrected
// rather than left standing as a false disclaimer.

const vb09FixtureModulePath = "example.test"

// vb09FixturePrelude gives every fixture the two axes the derivers read from the tree:
// the NodeStatus constants and isTerminalStatus. All nine states are present, so the
// terminal partition is total for every row that does not deliberately break it.
const vb09FixturePrelude = `package workflow

type NodeStatus string

const (
	Pending            NodeStatus = "pending"
	Running            NodeStatus = "running"
	Waiting            NodeStatus = "waiting"
	Completed          NodeStatus = "completed"
	Failed             NodeStatus = "failed"
	Skipped            NodeStatus = "skipped"
	Bypassed           NodeStatus = "bypassed"
	Compensated        NodeStatus = "compensated"
	CompensationFailed NodeStatus = "compensation_failed"
)

func isTerminalStatus(status NodeStatus) bool {
	return status == Completed || status == Failed || status == Skipped || status == Bypassed || status == Compensated || status == CompensationFailed
}
`

// vb09FixtureSubject wraps a row's body in a file that also declares the mediation
// boundary. The mediated write is a CLOSURE LEXICALLY INSIDE executeNodesInLevel, so
// every row in the table witnesses F12 as well as its own form: if the position test ever
// stops treating a function literal's body as inside, every row gains a member at once.
func vb09FixtureSubject(body string) string {
	return `package workflow

func executeNodesInLevel(data *WorkflowData) {
	func() { data.SetNodeStatus("mediated", Completed) }()
}

` + body
}

type vb09Fixture struct {
	form string // the form under test, named so a failure says WHICH form regressed
	body string
	// consumer, when non-empty, is a COMPLETE file in another package placed at
	// consumer/consumer.go. It exists for the package-qualified-status form, which cannot
	// be exhibited from inside the engine package at all.
	consumer string
	// switchPrelude rewrites isTerminalStatus's body into a switch, which is the shape the
	// deriver must REFUSE rather than answer.
	switchPrelude bool
	want          []string // exact expected member keys, in key order
	wantErr       string   // when non-empty the deriver must ERROR, and the error must contain this
	check         func(t *testing.T, d vb09Derivation)
}

// vb09RunFixture derives over the prelude, one synthetic subject file in the engine
// package, and optionally one consumer-package file. Nothing touches the working tree:
// the sources are strings parsed in memory, which keeps the clean-tree hazard out of this
// task entirely and makes every derived predicate a pure function over parsed files.
func vb09RunFixture(t *testing.T, f vb09Fixture) (vb09Derivation, error) {
	t.Helper()

	prelude := vb09FixturePrelude
	if f.switchPrelude {
		prelude = strings.Replace(prelude,
			"return status == Completed || status == Failed || status == Skipped || "+
				"status == Bypassed || status == Compensated || status == CompensationFailed",
			"switch status {\n\tcase Completed:\n\t\treturn true\n\t}\n\treturn false", 1)
		require.NotEqual(t, vb09FixturePrelude, prelude, "the F16 rewrite did not apply")
	}

	sources := []struct{ rel, text string }{
		{"engine/prelude.go", prelude},
		{"engine/subject.go", vb09FixtureSubject(f.body)},
	}
	if f.consumer != "" {
		sources = append(sources, struct{ rel, text string }{"consumer/consumer.go", f.consumer})
	}

	fset := token.NewFileSet()
	files := make([]vb09File, 0, len(sources))
	for _, src := range sources {
		parsed, err := parser.ParseFile(fset, src.rel, src.text, 0)
		require.NoError(t, err, "fixture %q does not parse (%s)", f.form, src.rel)
		files = append(files, vb09File{rel: src.rel, file: parsed})
	}

	engineDir := "engine"
	if pkg := vb09EnginePackage(files); len(pkg) > 0 {
		engineDir = filepath.Dir(pkg[0].rel)
	}
	return vb09Derive(fset, files, vb09FixtureModulePath+"/"+engineDir, engineDir)
}

// TestVB09_ASTFilterFormsAreWitnessed is Task B5 and it is not discretionary: without it
// Track B has no completeness argument.
func TestVB09_ASTFilterFormsAreWitnessed(t *testing.T) {
	const subj = "engine/subject.go"

	fixtures := []vb09Fixture{
		{
			form: "F1 pointer-receiver method declaration (*ast.StarExpr) -- the classic AST-filter miss",
			body: `func (w *WorkflowData) touch(n string) { w.nodeStatus[n] = Completed }`,
			want: []string{subj + "\t(*WorkflowData).touch\tfieldwrite\tcompleted\t#0"},
		},
		{
			form: "F2 parenthesised receiver (data).SetOutput(n, v)",
			body: `func f(data *WorkflowData) { (data).SetOutput("n", 1) }`,
			want: []string{subj + "\tf\tsetoutput\tn/a\t#0"},
		},
		{
			form: "F3 selector chain s.d.SetNodeStatus(n, Completed)",
			body: `func f(s *holder) { s.d.SetNodeStatus("n", Completed) }`,
			want: []string{subj + "\tf\tsetnodestatus\tcompleted\t#0"},
		},
		{
			form: "F4 index receiver arr[0].SetNodeStatus(n, Completed)",
			body: `func f(arr []*WorkflowData) { arr[0].SetNodeStatus("n", Completed) }`,
			want: []string{subj + "\tf\tsetnodestatus\tcompleted\t#0"},
		},
		{
			form: "F5 call receiver get().SetNodeStatus(n, Completed)",
			body: `func f() { get().SetNodeStatus("n", Completed) }`,
			want: []string{subj + "\tf\tsetnodestatus\tcompleted\t#0"},
		},
		{
			form: "F6 NEGATIVE -- method value f := data.SetNodeStatus; f(n, Completed) is NOT seen",
			body: `func f(data *WorkflowData) {
	g := data.SetNodeStatus
	g("n", Completed)
}`,
			want: nil,
			check: func(t *testing.T, _ vb09Derivation) {
				require.Contains(t, vb09Limitations(), "method VALUE call",
					"a form the deriver cannot see must be DECLARED in its own output; if this form "+
						"has started being matched, correct the limitation text rather than this assertion")
			},
		},
		{
			form: "F7 status is a conversion NodeStatus(s) -- included as indeterminate, never dropped",
			body: `func f(data *WorkflowData, s string) { data.SetNodeStatus("n", NodeStatus(s)) }`,
			want: []string{subj + "\tf\tsetnodestatus\tindeterminate\t#0"},
		},
		{
			form: "F8 status is a variable -- included as indeterminate",
			body: `func f(data *WorkflowData, st NodeStatus) { data.SetNodeStatus("n", st) }`,
			want: []string{subj + "\tf\tsetnodestatus\tindeterminate\t#0"},
		},
		{
			form: "F9 untyped ValueSpec var x = Completed -- included as indeterminate",
			body: `func f(data *WorkflowData) {
	x := Completed
	data.SetNodeStatus("n", x)
}`,
			want: []string{subj + "\tf\tsetnodestatus\tindeterminate\t#0"},
		},
		{
			form: "F10 direct map write w.nodeStatus[k] = Completed -- the setter is not the only door",
			body: `func f(w *WorkflowData, k string) { w.nodeStatus[k] = Completed }`,
			want: []string{subj + "\tf\tfieldwrite\tcompleted\t#0"},
		},
		{
			form: "F11 tuple assign w.nodeStatus[k], w.outputs[k] = Completed, v -- BOTH sides seen",
			body: `func f(w *WorkflowData, k string, v interface{}) { w.nodeStatus[k], w.outputs[k] = Completed, v }`,
			want: []string{
				subj + "\tf\tfieldwrite\tcompleted\t#0",
				subj + "\tf\tfieldwrite\tn/a\t#0",
			},
		},
		{
			form: "F12 write inside a closure lexically inside executeNodesInLevel -- EXCLUDED as mediated",
			body: `func unrelated() {}`,
			want: nil,
			check: func(t *testing.T, d vb09Derivation) {
				require.Equal(t, 1, d.excludedMediated,
					"the write in the closure inside %s must be excluded as mediated; a position test "+
						"that stops descending into function literals would let it through",
					vb09MediationFunc)
			},
		},
		{
			form: "F13 the identical write immediately AFTER that function -- INCLUDED",
			body: `func f(data *WorkflowData) { data.SetNodeStatus("n", Completed) }`,
			want: []string{subj + "\tf\tsetnodestatus\tcompleted\t#0"},
			check: func(t *testing.T, d vb09Derivation) {
				require.Equal(t, 1, d.excludedMediated,
					"F13's control: the mediated write is still excluded, so the difference between "+
						"F12 and F13 is POSITION and nothing else")
			},
		},
		{
			form: "F14 a SECOND executeNodesInLevel declaration -- the deriver ERRORS, it does not pick one",
			body: `func executeNodesInLevel(data *WorkflowData) {}

func f(data *WorkflowData) { data.SetNodeStatus("n", Completed) }`,
			wantErr: "expected exactly ONE executeNodesInLevel declaration",
		},
		{
			form: "F15 a tenth NodeStatus const, unclassified -- the partition reds NAMING it",
			body: `const Quarantined NodeStatus = "quarantined"

func f(data *WorkflowData) { data.SetNodeStatus("n", Quarantined) }`,
			want: []string{subj + "\tf\tsetnodestatus\tquarantined\t#0"},
			check: func(t *testing.T, d vb09Derivation) {
				require.Equal(t, []string{"Quarantined"}, d.unaccounted,
					"a NodeStatus classified neither terminal nor non-terminal must be reported BY "+
						"NAME; NodeStatus is nine-state and a hand-written census has dropped the ninth before")
			},
		},
		{
			form:          "F16 isTerminalStatus written as a SWITCH -- the deriver ERRORS, it does not return {}",
			body:          `func f() {}`,
			switchPrelude: true,
			wantErr:       "does not understand",
		},
		{
			form: "F17 package-qualified status wf.Completed from a CONSUMER package -- resolved, not indeterminate",
			consumer: `package consumer

import wf "example.test/engine"

func f(data *wf.WorkflowData) {
	data.SetNodeStatus("n", wf.Running)
	data.SetNodeStatus("n", wf.Completed)
}`,
			want: []string{"consumer/consumer.go\tf\tsetnodestatus\tcompleted\t#0"},
			check: func(t *testing.T, d vb09Derivation) {
				require.Equal(t, 1, d.excludedNonTerminal,
					"wf.Running must resolve and be EXCLUDED as non-terminal; left unresolved it would "+
						"be included as indeterminate and the golden would carry a Running write")
			},
		},
	}

	for _, f := range fixtures {
		f := f
		t.Run(f.form, func(t *testing.T) {
			d, err := vb09RunFixture(t, f)
			if f.wantErr != "" {
				require.Error(t, err, "form %q must make the deriver ERROR rather than answer a shape "+
					"it does not understand -- a partial or empty set here would make the guard green "+
					"over nothing, and a pass predicate over the empty set returns true", f.form)
				require.Contains(t, err.Error(), f.wantErr)
				return
			}
			require.NoError(t, err)

			got := make([]string, 0, len(d.members))
			for _, m := range d.members {
				got = append(got, m.key())
			}
			want := f.want
			if want == nil {
				want = []string{}
			}
			require.Equal(t, want, got, "form %q: the deriver's member set is not what this form claims", f.form)
			if f.check != nil {
				f.check(t, d)
			}
		})
	}
}

// TestVB09_ReviewFoundForms carries the rows an independent review added after the first
// seventeen. Each is a form the reviewer REPRODUCED against this file's own derivers, so
// none of them is hypothetical.
//
// 🔴 THE THREE NEGATIVE ROWS ARE THE POINT. vb09Limitations is a DISCLAIMER, and a
// disclaimer that omits forms the matcher cannot see is worse than no disclaimer: it tells
// a reader the gaps are enumerated when they are not. These three were neither matched nor
// declared, and the composite-literal one is the spelling workflow_data.go already uses for
// both fields. Matching them is optional; DECLARING them is not.
func TestVB09_ReviewFoundForms(t *testing.T) {

	t.Run("119-F2 POSITIVE: a consumer's own Running does not silently drop the site", func(t *testing.T) {
		// The reviewer's P4/P5 pair, landed as a permanent fixture. P4 named the constant
		// Running and LOST the member; P5 was byte-identical but named Zork and kept it --
		// the name collision alone was the difference, and only the three NON-terminal names
		// lose a member, which is silence in the flattering direction.
		f := vb09Fixture{
			form: "119-F2 consumer package declares its OWN Running",
			consumer: `package consumer

import wf "example.test/engine"

const Running = "totally unrelated to the engine"

func f(data *wf.WorkflowData) { data.SetNodeStatus("n", Running) }`,
		}
		d, err := vb09RunFixture(t, f)
		require.NoError(t, err)
		keys := make([]string, 0, len(d.members))
		for _, m := range d.members {
			keys = append(keys, m.key())
		}
		require.Equal(t, []string{"consumer/consumer.go\tf\tsetnodestatus\tindeterminate\t#0"}, keys,
			"an UNQUALIFIED identifier outside the engine's package cannot denote the engine's "+
				"constant, so it must resolve to indeterminate and stay INCLUDED. Resolving it as "+
				"the engine's Running excludes the site as non-terminal and LOSES a member")
		require.Zero(t, d.excludedNonTerminal,
			"nothing may be excluded as non-terminal here: the consumer's Running is its own constant")
	})

	t.Run("119-F2 companion (NOT a control): the same file with a non-colliding name", func(t *testing.T) {
		// 🔴 THIS IS NOT A CONTROL AND MUST NOT BE READ AS ONE (renamed after review).
		// Before the fix it discriminated: `Running` lost its member and `Zork` kept it, and
		// the name alone was the difference. AFTER the fix both arms pass identically,
		// because neither identifier resolves outside the engine package -- so this arm can
		// no longer fail in any scenario where the arm above passes. It is kept as a
		// regression witness for the historical asymmetry, NOT as independent evidence.
		// Whoever prunes here: keep the arm ABOVE, which is the one that can still fail.
		f := vb09Fixture{
			form: "119-F2 companion (NOT a control), identifier renamed",
			consumer: `package consumer

import wf "example.test/engine"

const Zork = "totally unrelated to the engine"

func f(data *wf.WorkflowData) { data.SetNodeStatus("n", Zork) }`,
		}
		d, err := vb09RunFixture(t, f)
		require.NoError(t, err)
		require.Len(t, d.members, 1,
			"if this and the arm above ever DISAGREE, membership is being decided by an "+
				"identifier's NAME rather than by what it denotes -- which is the defect 119-F2 "+
				"was. Agreement here is expected and is not, by itself, evidence of anything")
	})

	for _, row := range []struct{ form, body, why string }{
		{
			form: "119-F1a composite-literal field init -- NOT seen, and DECLARED",
			body: `func f(k string) *WorkflowData {
	return &WorkflowData{nodeStatus: map[string]NodeStatus{k: Completed}}
}`,
			why: "COMPOSITE-LITERAL field init",
		},
		{
			form: "119-F1b local alias -- NOT seen, and DECLARED",
			body: `func f(w *WorkflowData, k string) {
	m := w.nodeStatus
	m[k] = Completed
}`,
			why: "LOCAL ALIAS",
		},
		{
			form: "119-F1c range-assign -- NOT seen (an *ast.RangeStmt), and DECLARED",
			body: `func f(w *WorkflowData, src map[string]NodeStatus) {
	var v NodeStatus
	for w.nodeStatus["k"], v = range src {
		_ = v
	}
}`,
			why: "RANGE-ASSIGN",
		},
	} {
		row := row
		t.Run(row.form, func(t *testing.T) {
			d, err := vb09RunFixture(t, vb09Fixture{form: row.form, body: row.body})
			require.NoError(t, err)
			require.Empty(t, d.members,
				"this form is declared UNSEEN. If the matcher has started seeing it, correct "+
					"vb09Limitations rather than this assertion -- a disclaimer that is false in the "+
					"generous direction is still false")
			require.Contains(t, vb09Limitations(), row.why,
				"a form the deriver cannot see MUST be named in its own printed limitations; an "+
					"omitted gap tells a reader the gaps are enumerated when they are not")
		})
	}
}
