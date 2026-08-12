package workflow

import (
	"flag"
	"fmt"
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

// SEAL-08 — the exported surface of Node, DAG and Workflow, enumerated by the compiler's
// own parser and classified, so that adding an exported identifier to any of the three,
// or silently reclassifying one, reds.
//
// WHY A CENSUS AND NOT A LIST OF KNOWN BYPASSES. Three successive careful passes over
// this surface produced three DIFFERENTLY incomplete answers: the M23 red-team missed
// four exported DAG fields, the architect's field sweep missed the entire method
// surface, and the engineer's field+method pass missed TopologicalSort, GetLevels and
// NewWorkflow. The hand-authored BYPASS-01..10 inventory omitted (*DAG).AddDependency,
// which re-parents a node on an already-validated graph BY NAME. No hand list at any
// level of correction closed this. A census is closed by the parser; a bypass narrative
// can only contain the routes someone thought of.
//
// ═══ WHAT THIS PROVES, AND WHAT IT DOES NOT ═══
//
// D-24. This establishes the DISTRIBUTION of the seal — which identifiers are exported,
// and that each one was classified deliberately. It establishes NOTHING about whether
// any individual accessor is correct. "62 identifiers, 0 unclassified" reads like an
// assurance about the seal and is only an assurance about its spread. Correctness of a
// given accessor needs its own test; the defensive-copy contract on GetDependencies, for
// instance, is guarded in node_test.go and nowhere here.
//
// THE CENSUS DELIBERATELY DOES NOT CLASSIFY BY "DOES IT MUTATE", and the reason is the
// sharpest thing this phase learned. The exploratory version of this sweep carried a
// MUTATES column, computed from whether a method assigns to a field of its receiver.
// That column reported (*DAG).AddDependency as CLEAN — because it writes a REACHED
// node's field, not the receiver's — so the most dangerous mutator on the surface was
// invisible to the very column meant to find it. GetDependencies read clean for the
// mirror-image reason: it returned the live slice header, and handing out a mutable
// interior is not an assignment. MUTATION IS NOT A SYNTACTIC PROPERTY. Export status
// is, which is why that is what this file checks, with the mutation question pushed
// onto the human classification below.
//
// ═══ THE INSTRUMENT'S CEILING ═══
//
// The sweep sees this package's non-test source only. EMBEDDED FIELDS ARE INVISIBLE
// to it: collectStructFields iterates field.Names, which is nil for an embedded field,
// so `type DAG struct { sync.RWMutex }` would export four methods and record nothing
// here. Nothing in the package embeds today; a future embed must be caught by review,
// not by this file. It keys
// methods on the receiver's bare type name, so it would not distinguish two same-named
// types in different files (none exist here).
//
// Regenerate with:  go test ./pkg/workflow/ -run TestSurfaceCensus -update-census

var updateCensus = flag.Bool("update-census", false,
	"print the current exported surface of Node/DAG/Workflow in golden-list form and skip the assertions")

// censusClass is how an exported identifier was DISPOSED of by M23 phase 117.
type censusClass string

const (
	// sanctioned: still exported, deliberately. The reason is recorded on the entry.
	sanctioned censusClass = "SANCTIONED"
	// sealed: was exported at b8f8690, now unexported. Must NOT appear in the sweep.
	sealed censusClass = "SEALED"
	// deleted: was exported at b8f8690, now removed entirely. Must NOT appear in the
	// sweep. NOTE the honest limit: the assertions treat sealed and deleted
	// IDENTICALLY (both must simply be absent), so this class carries documentary
	// meaning, not extra mechanical force. Absence here is a PARSE result, not a
	// compile-proof — the compile-proof of removal is that the package builds.
	deleted censusClass = "DELETED"
	// pending: still exported, and its disposition is OWED by a named later task in this
	// phase. This is a classification, NOT a decision — it exists so that "everything is
	// classified" cannot be mistaken for "everything is decided". The phase's Definition
	// of Done requires this class to be EMPTY at close.
	//
	// BE PRECISE ABOUT WHAT THE RATCHET DOES, because the first version of this comment
	// overclaimed and an audit caught it: TestSurfaceCensus_NoPendingDispositions
	// asserts the count does not GROW. It passes while the class is non-empty. So it
	// makes REGRESSION mechanical; it does NOT make COMPLETION mechanical — reaching
	// zero is still a human act, enforced by the phase gate, not by this file.
	pending censusClass = "PENDING"
)

type censusEntry struct {
	class censusClass
	// why is required for SANCTIONED (T1b: every sanctioned entry owes a recorded
	// reason) and is a short note for the other two.
	why string
}

// surfaceCensus is the golden list. It covers the union of the surface at b8f8690 (the
// phase baseline) and the surface now, so that a SEALED or DELETED entry reappearing is
// caught just as loudly as a new one arriving.
//
// Keys are rendered by censusKey: "Type.Member" for fields and methods, and a bare
// function name for a package-level constructor that mints one of the three types.
var surfaceCensus = map[string]censusEntry{
	// ── Node: the seven fields, all sealed by SEAL-01 ──────────────────────────────
	"Node.Name":            {sealed, "field -> name; readable via the Name() method"},
	"Node.Action":          {sealed, "field -> action; the swap path SEAL-09 depends on being closed"},
	"Node.DependsOn":       {sealed, "field -> dependsOn; readable via GetDependencies (a copy)"},
	"Node.RetryCount":      {sealed, "field -> retryCount"},
	"Node.Timeout":         {sealed, "field -> timeout"},
	"Node.ContinueOnError": {sealed, "field -> continueOnError"},
	"Node.Compensation":    {sealed, "field -> compensation"},

	// ── Node: the SIX post-Build mutators, deleted by SEAL-01 ──────────────────────
	// The requirement said "all five"; that was a hand-count and it was short by one.
	"Node.AddDependency()":       {deleted, "post-Build edge write"},
	"Node.AddDependencies()":     {deleted, "post-Build edge write"},
	"Node.WithDependencies()":    {deleted, "post-Build edge write; stored the CALLER's variadic slice"},
	"Node.WithRetries()":         {deleted, "post-Build policy write; build() assigns the field"},
	"Node.WithTimeout()":         {deleted, "post-Build policy write; build() assigns the field"},
	"Node.WithContinueOnError()": {deleted, "post-Build policy write; build() assigns the field"},

	// ── Node: the rest — Execute is SEALED, the others stay exported ───────────────
	"Node.Execute()": {sealed, "-> execute (BYPASS-03); it let a caller run one node outside the executor"},
	"Node.Name()":    {sanctioned, "read accessor; the name is fixed at mint and is the graph-identity key"},
	"Node.GetDependencies()": {sanctioned,
		"read accessor returning a DEFENSIVE COPY (BYPASS-05). Correctness guarded in node_test.go"},
	"Node.HasDependency()": {sanctioned, "read accessor; answers by name, exposes nothing mutable"},
	"NewNode": {sealed,
		"T6: -> newNode. F117-T6-04: the census had this SANCTIONED while PLAN section T6 ruled ALL SIX " +
			"Node constructors unexported — instrument and plan disagreed, and the instrument was wrong. " +
			"Still one of the two mint chokepoints SEAL-09's derivation depends on; sealing changes who " +
			"may call it, not what it does"},
	"NewNodeWithCapacity": {sealed,
		"T6: -> newNodeWithCapacity. Mis-classified sanctioned with NewNode (F117-T6-04). The other mint " +
			"chokepoint; the builder mints here. Guarded by TestSuspendable_NodeIsOnlyMintedAtTheChokepoint"},

	// ── DAG: structure ─────────────────────────────────────────────────────────────
	"DAG.Nodes": {sealed, "field -> nodes; read access is GetNode / GetLevels"},
	"DAG.Name":  {sealed, "field -> name; the external read is served by the Name() accessor"},
	"DAG.Name()": {sanctioned,
		"read accessor added by T1c. pkg/testutil (a PUBLIC package) builds a WorkflowData from it"},
	"DAG.CycleNodes": {sealed, "field -> cycleNodes. Genuinely live: Validate reads it back to build the cycle error"},
	"DAG.StartNodes": {deleted,
		"T1c: dead COMPUTATION on the Execute path, not merely dead state — 3 writes, 0 engine reads"},
	"DAG.EndNodes": {deleted, "T1c: deleted with StartNodes; same evidence, same dead loop in Validate"},

	// ── DAG: construction and mutation — all sealed by T6 (SEAL-06) ────────────────
	"DAG.AddNode()": {sealed, "T6: -> addNode. A post-Build node write on a validated graph"},
	"DAG.AddDependency()": {sealed,
		"T6: -> addDependency. Re-parented a node on a VALIDATED graph BY NAME — the bypass no hand " +
			"inventory listed, and the one this census's own MUTATES column reads CLEAN (it writes a " +
			"REACHED node's field, not the receiver's)"},
	"NewDAG": {sealed, "T6: -> newDAG (BYPASS-09). Sealed WITH NewDAGWithCapacity, never alone"},
	"NewDAGWithCapacity": {sealed,
		"T6: -> newDAGWithCapacity. The constructor build() ACTUALLY calls — sealing NewDAG alone would " +
			"have left BYPASS-09 intact verbatim while this ratchet still ticked down by one"},

	// ── DAG: reads and config ──────────────────────────────────────────────────────
	"DAG.Execute()": {sanctioned, "one of the two graph-level entry points. T6 landed: it refuses an unstamped DAG with ErrDAGNotBuilt"},
	"DAG.Validate()": {sanctioned,
		"read-only over the built graph. NOTE: validateReconvergence appends DEC-M11-DEPMODEL edges, but it is " +
			"called from build() only, NOT from Validate — checked, because Validate is on the Execute path"},
	"DAG.GetNode()": {sanctioned,
		"read accessor. Returns *Node, which is an OPAQUE HANDLE once SEAL-01 lands (architect ruling, engineer Q2)"},
	"DAG.GetLevels()": {sanctioned,
		"the sanctioned external node-set read. NOT live state: every slice is built locally and the " +
			"*Node elements are opaque handles after SEAL-01. Live callers in saga_rollback.go and dag.go"},
	"DAG.TopologicalSort()": {sanctioned,
		"EVALUATED for deletion (zero non-test callers) and KEPT: it enables no invalid state — fresh " +
			"slices, opaque handles — and costs nothing when uncalled, unlike StartNodes/EndNodes which " +
			"cost on every Execute. Sealing mutation paths is the mandate; pruning a harmless tested read " +
			"API is not. Architect may overturn"},
	"DAG.WithExecutionConfig()": {sanctioned, "config knob, set before Execute; touches no structure"},
	"DAG.WithTracerProvider()":  {sanctioned, "config knob, set before Execute; touches no structure"},
	"DAG.DefinitionDigest()": {sanctioned,
		"AUD-010/C-07: a read-only structural digest of the graph definition (topology, per-node " +
			"policy/compensation, boundary, action kind, suspendability). Consumers use it to detect a " +
			"changed graph; the resume guard stamps and compares it. Touches no structure"},

	// ── Workflow: the graph handle and its writers — sealed by T6 ──────────────────
	"Workflow.DAG": {sealed,
		"T6: field -> dag, read via the DAG() accessor. BYPASS-10 — but the SEAL IS NOT THE CLOSURE: " +
			"workflow_dispatch.go is in-package and still fills the field from a consumer DAGFactory. " +
			"The builder TOKEN at drive time is what refuses an unvalidated graph"},
	"Workflow.DAG()": {sanctioned,
		"read accessor added by T6 beside the sealed field — the SAME shape as T1c's DAG.Name/Name(), " +
			"and it arrived here UNCLASSIFIED and red, which is the '()' key suffix doing its job: the " +
			"field's seal and the method's arrival cannot hide each other. Returns the LIVE *DAG, so the " +
			"caller reaches every exported DAG method; none writes topology after T6, but With* mutate " +
			"execution config and Execute drives, so 'read-only' is the intent and not the guarantee"},
	"Workflow.AddNode()":       {sealed, "T6: -> addNode, with (*DAG).addNode"},
	"Workflow.AddDependency()": {sealed, "T6: -> addDependency, with (*DAG).addDependency"},
	"NewWorkflow": {sealed,
		"T6: -> newWorkflow, NOT deleted (architect ruling, engineer Q3). Stays UNSTAMPED: it mints an " +
			"EMPTY DAG the caller then populates, so stamping would certify a zero-node graph"},
	"WorkflowBuilder.Build()": {sanctioned,
		"THE sanctioned construction path (D-03: Build stays exported, do not conflate it with the sealed " +
			"surface). It mints a *DAG, so it belongs in this census even though its receiver is not a " +
			"tracked type — it was invisible to the sweep until an audit found the gap"},
	// ── The two ADMISSION points, visible for the first time (F-117-ARCH-12) ──────
	// Where a foreign, possibly-unstamped *DAG ENTERS the graph. Both are ruled
	// SANCTIONED rather than sealed: a consumer legitimately composes a child workflow
	// from a DAG it built, so the answer is not to close the door but to CHECK what
	// comes through it — which requireBuiltChild (R-04) now does, refusing an unstamped
	// child with ErrDAGNotBuilt at the PARENT's build().
	"WorkflowBuilder.AddSubWorkflow()": {sanctioned,
		"admission point: takes a caller-supplied child *DAG into the sanctioned builder. Guarded by " +
			"requireBuiltChild (R-04) — provenance is CHECKED here, not assumed. Invisible to this census " +
			"until collectFunc learned to read PARAMS as well as results"},
	"WorkflowBuilder.AddSubWorkflowParked()": {sanctioned,
		"admission point, same guard and same argument as AddSubWorkflow. AddSubWorkflowQueued is NOT in " +
			"this class: it takes a child TYPE STRING, so no graph object crosses the boundary — which is " +
			"the M17 'workflow is DATA' split showing up as a difference in attack surface"},

	"FromBuilder": {sanctioned,
		"the sanctioned construction path; calls build() guard-free and deliberately, carrying builder.store forward"},

	// ── Workflow: the eight sanctioned config knobs ────────────────────────────────
	"Workflow.WorkflowID": {sanctioned, "config knob (architect grounding: the 8 sanctioned knobs)"},
	"Workflow.Store":      {sanctioned, "config knob"},
	"Workflow.Registry": {sealed,
		"T6: field -> registry (architect ruling R-03). Filed here as a config knob, which was the " +
			"mis-classification: it CARRIES CODE by its own doc and is read at execute time to resolve a " +
			"child type -> DAG, so it is the same class as the graph handle, not a value knob like Clock"},
	"Workflow.MaxSubWorkflowDepth": {sanctioned, "config knob"},
	"Workflow.Clock":               {sanctioned, "config knob"},
	"Workflow.Locker":              {sanctioned, "config knob"},
	"Workflow.RollbackTimeout":     {sanctioned, "config knob"},
	"Workflow.MetricsConfig":       {sanctioned, "config knob"},

	// ── Workflow: public API ───────────────────────────────────────────────────────
	"Workflow.Execute()":                 {sanctioned, "the other graph-level entry point. T6 landed: the executeLocked check refuses an unstamped DAG with ErrDAGNotBuilt"},
	"Workflow.DeliverSignal()":           {sanctioned, "M19 public API"},
	"Workflow.DeliverAndResume()":        {sanctioned, "M19 public API"},
	"Workflow.DueTimers()":               {sanctioned, "M20 public API"},
	"Workflow.Tick()":                    {sanctioned, "M20 public API"},
	"Workflow.GetMetrics()":              {sanctioned, "M18 read-model"},
	"Workflow.WithClock()":               {sanctioned, "config setter"},
	"Workflow.WithDefinitionMigration()": {sanctioned, "AUD-070 config setter: installs a definition-mismatch migration handler"},
	"Workflow.ApprovalNonce()":           {sanctioned, "AUD-025 read accessor: derives the correlation nonce a host attaches to an approval decision; pure, exposes no mutable state"},
	"Workflow.WithLocker()":              {sanctioned, "config setter"},
	"Workflow.WithMultiProcessLocker()":  {sanctioned, "config setter"},
	"Workflow.WithRollbackTimeout()":     {sanctioned, "config setter"},
	"Workflow.WithWorkflowID()":          {sanctioned, "config setter"},

	// ── The four DECLARED park constructors ────────────────────────────────────────
	// F117-DEC-01 established that all four have ZERO non-test callers: the builder is
	// the only production path and it mints via NewNodeWithCapacity. That is the same
	// evidential shape T1c uses to make StartNodes/EndNodes deletion candidates, so
	// their disposition is a real question and not a formality.
	"NewTimerNode":                  {sealed, "T6: -> newTimerNode. Zero non-test callers (compiler-derived); 29 in-package test sites"},
	"NewWaitForSignalNode":          {sealed, "T6: -> newWaitForSignalNode. Zero non-test callers (compiler-derived); 7 in-package test sites"},
	"NewWaitForConditionNode":       {sealed, "T6: -> newWaitForConditionNode. Zero non-test callers (compiler-derived); 6 in-package test sites"},
	"NewWaitForSignalOrTimeoutNode": {sealed, "T6: -> newWaitForSignalOrTimeoutNode. Zero non-test callers (compiler-derived); 1 in-package test site"},
}

// censusKey renders a member of one of the three tracked types. A method is suffixed
// "()" only where a field of the same name also existed at the baseline (Node.Name),
// because otherwise the seal of the FIELD and the addition of the METHOD would collide
// on one key and hide each other.
func censusKey(typeName, member string, isMethod bool) string {
	// EVERY method carries a "()" suffix, and that is load-bearing rather than
	// cosmetic: it makes "field" and "method" DIFFERENT KEYS, so a field that becomes
	// a method (seal the field, add an accessor — the exact shape of T1c's DAG.Name
	// and of every pending Workflow config field) CHANGES ITS KEY, so it cannot pass
	// silently: the old key is now stale and the new one is unclassified.
	//
	// Bite-proven by re-keying Workflow.Clock: reds with
	// "UNCLASSIFIED ... [Workflow.Clock (field)]". Only that arm's message appears —
	// both conditions hold, but require aborts at the first, so this is one arm
	// reported, not two.
	//
	// This was originally a hardcoded special case for Node+Name, then for DAG+Name.
	// That fixed the two instances and left the CLASS live for Workflow.Store,
	// Workflow.Clock and every other pending field with a plausible accessor — the
	// same instance-not-class mistake this milestone keeps paying for. An audit caught
	// it while the second hardcode was still warm.
	if isMethod {
		return typeName + "." + member + "()"
	}
	return typeName + "." + member
}

// censusTypes are the three types whose surface this phase seals.
//
// THE FOURTH CEILING (F-117-ARCH-12), beside the three recorded on the guard below.
// This map names the TYPES; what used to go wrong was the rule that decided which
// FUNCTIONS mention them. Collection keyed on receiver and on RESULTS, with no rule for
// PARAMS at all — so a method on an untracked receiver that ACCEPTS a *DAG was invisible
// BY CONSTRUCTION, and the ratchet could have reached zero while two public
// *DAG-accepting entry points had never been enumerated at all.
//
// The half that makes it a lesson rather than a bug: an earlier patch had already
// widened collection once, correctly, to catch (*WorkflowBuilder).Build — and caught
// only MINTS, the direction it was looking at. ADMISSIONS, on the same receiver in the
// same file, adjacent in the source, stayed invisible. A derived-LOOKING rule that
// covers half the surface is more dangerous than an obvious hand list, because nobody
// audits it. collectFunc now reads BOTH directions (see admitsTracked), which SUBSUMES
// the earlier patch instead of adding a fourth special case beside it.
//
// THE FIX HAD TO BE IN THE RULE, NOT THE GOLDEN LIST, and this is not a stylistic
// preference. -update-census gates only the ASSERTIONS (a t.Skip); the collection
// functions run unchanged. So two symbols hand-added to surfaceCensus would have been
// erased by the next regeneration — silently, because the regenerated list is by
// definition "what the sweep sees". VERIFIED by running it: the regenerated list now
// contains both, annotated "// admitting method".
//
// ON THE RATCHET, because the guard below says "never raise it" and a scope widening
// looks exactly like the violation that rule exists to catch. A widening that reveals
// previously-invisible symbols is THE INSTRUMENT GETTING HONEST, not a regression — and
// the architect ruled it the sole circumstance in which the ceiling may rise, provided
// it rises in the same commit as the rule change with the delta enumerated per symbol.
// THE PERMISSION WAS NOT NEEDED HERE: both newly-visible symbols disposed SANCTIONED
// rather than pending, so the pending count stayed 0 and the floor stayed 34. Recorded
// because the next widening may not be so lucky, and because "we were allowed to raise
// it and did not have to" is a materially different fact from "it did not rise".
var censusTypes = map[string]bool{"Node": true, "DAG": true, "Workflow": true}

// sweepExportedSurface parses every non-test .go file in dir and returns the exported
// surface of the three tracked types: their exported fields, their exported methods, and
// the package-level constructors that mint them.
func sweepExportedSurface(t *testing.T, dir string) map[string]string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	fset := token.NewFileSet()
	out := map[string]string{}
	parsed := 0

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
			switch d := decl.(type) {
			case *ast.GenDecl:
				collectStructFields(d, out)
			case *ast.FuncDecl:
				collectFunc(d, out)
			}
		}
	}

	// Anti-vacuity, and it runs before any caller can read the result: a sweep that
	// parses nothing finds no offenders, which is indistinguishable from a clean
	// surface. This is ph116's parity-sweep defect, paid for once up front.
	require.Greater(t, parsed, 1, "sanity: the sweep must parse more than one non-test file")
	require.NotEmpty(t, out, "sanity: the sweep found NO exported surface at all, which means it is broken, not that the surface is empty")

	return out
}

// collectStructFields records the exported fields of the tracked struct types.
func collectStructFields(d *ast.GenDecl, out map[string]string) {
	if d.Tok != token.TYPE {
		return
	}
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok || !censusTypes[ts.Name.Name] {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			continue
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				if name.IsExported() {
					out[censusKey(ts.Name.Name, name.Name, false)] = "field"
				}
			}
		}
	}
}

// collectFunc records exported methods on the tracked types, and package-level
// constructors that return one of them.
func collectFunc(fn *ast.FuncDecl, out map[string]string) {
	if !fn.Name.IsExported() {
		return
	}

	// A method on one of the tracked types.
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := bareTypeName(fn.Recv.List[0].Type)
		if censusTypes[recv] {
			out[censusKey(recv, fn.Name.Name, true)] = "method"
			return
		}
		// NOT a tracked receiver — but the signature may still MENTION a tracked type, in
		// EITHER direction, and both directions are surface this phase must rule on.
		//
		// F-117-ARCH-12. The original sweep saw neither; a patch then added MINTS
		// (*WorkflowBuilder).Build returns *DAG and is THE sanctioned mint path — and
		// stopped there. ADMISSIONS stayed invisible by construction, because collection
		// keyed on receiver and RESULTS with no rule for PARAMS at all. So
		// (*WorkflowBuilder).AddSubWorkflow(name string, child *DAG) and
		// AddSubWorkflowParked — the two build-time sites where a FOREIGN, possibly
		// unstamped *DAG ENTERS the graph, i.e. exactly what requireBuiltChild (R-04)
		// guards — were never enumerated, and the ratchet could have reached zero over
		// them.
		//
		// The patch was correct in instinct and half-scoped in fact: same receiver, same
		// file, adjacent in the source, one remembered and one not. Note rule 2's own
		// comment above — "cannot skip the route the builder actually uses" — and then
		// note that it skipped the OTHER route the builder actually uses. A
		// derived-LOOKING rule that covers half the surface is more dangerous than an
		// obvious hand list, because nobody audits it.
		if mints, admits := mintsTracked(fn), admitsTracked(fn); mints || admits {
			out[recv+"."+fn.Name.Name+"()"] = classify(mints, admits) + " method"
		}
		return
	}

	// A package-level function minting or admitting one of the tracked types.
	if mints, admits := mintsTracked(fn), admitsTracked(fn); mints || admits {
		if mints {
			out[fn.Name.Name] = "constructor"
		} else {
			out[fn.Name.Name] = "admitting function"
		}
	}
}

// classify names WHY a symbol is in the census, so the output says which direction
// pulled it in rather than merely that it is present.
func classify(mints, admits bool) string {
	switch {
	case mints && admits:
		return "minting+admitting"
	case mints:
		return "minting"
	default:
		return "admitting"
	}
}

// mintsTracked reports whether fn RETURNS one of the tracked types — a graph object
// leaving the engine.
func mintsTracked(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, res := range fn.Type.Results.List {
		if censusTypes[bareTypeName(res.Type)] {
			return true
		}
	}
	return false
}

// admitsTracked reports whether fn ACCEPTS one of the tracked types — a graph object
// entering the engine. The exact mirror of mintsTracked over Params, and it exists
// because a rule encoding one direction of dataflow will keep missing the other.
func admitsTracked(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, par := range fn.Type.Params.List {
		if censusTypes[bareTypeName(par.Type)] {
			return true
		}
	}
	return false
}

// bareTypeName renders a type expression as a bare name (T, *T, []T -> T).
func bareTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return bareTypeName(v.X)
	case *ast.ArrayType:
		return bareTypeName(v.Elt)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return bareTypeName(v.X)
	}
	return ""
}

// TestSurfaceCensus_ExportedSurfaceIsClassified is the assertion. It is bidirectional on
// purpose: a new exported identifier reds because it is unclassified, and a SANCTIONED
// one vanishing reds because the golden list has gone stale — the failure mode a
// one-directional check ships with.
func TestSurfaceCensus_ExportedSurfaceIsClassified(t *testing.T) {
	current := sweepExportedSurface(t, ".")

	if *updateCensus {
		printCensus(t, current)
		t.Skip("-update-census: printed the current surface, assertions skipped")
	}

	var unclassified, resurrected, stale []string

	for key, kind := range current {
		entry, known := surfaceCensus[key]
		switch {
		case !known:
			unclassified = append(unclassified, fmt.Sprintf("%s (%s)", key, kind))
		case entry.class == sealed || entry.class == deleted:
			resurrected = append(resurrected, fmt.Sprintf("%s (%s, recorded as %s)", key, kind, entry.class))
		}
	}

	for key, entry := range surfaceCensus {
		if entry.class == sanctioned || entry.class == pending {
			if _, present := current[key]; !present {
				stale = append(stale, key)
			}
		}
	}

	sort.Strings(unclassified)
	sort.Strings(resurrected)
	sort.Strings(stale)

	require.Empty(t, unclassified,
		"UNCLASSIFIED exported identifier(s) on Node/DAG/Workflow: %v\n"+
			"Every exported member of these three types must be classified SANCTIONED, SEALED or DELETED "+
			"in surfaceCensus (SEAL-08 / T1b). Regenerate with -update-census, then classify each new "+
			"entry deliberately — an exported identifier that nobody decided to export is exactly what "+
			"this phase exists to prevent.", unclassified)

	require.Empty(t, resurrected,
		"RESURRECTED identifier(s): %v\n"+
			"These are recorded as SEALED or DELETED by M23 phase 117 but are exported again. "+
			"Re-exporting one re-opens a bypass the seal closed.", resurrected)

	require.Empty(t, stale,
		"STALE census entr(ies): %v\n"+
			"These are recorded SANCTIONED but no longer appear in the exported surface. If they were "+
			"sealed or deleted deliberately, reclassify them; otherwise the golden list is lying about "+
			"what this package exports.", stale)

	// Every SANCTIONED and PENDING entry owes a recorded reason (T1b) — for a sanctioned
	// entry the justification, for a pending one the task that owes the ruling. Checked
	// mechanically so the obligation cannot be discharged by leaving the field blank.
	for key, entry := range surfaceCensus {
		if entry.class == sanctioned || entry.class == pending {
			require.NotEmpty(t, entry.why,
				"%s entry %q has no recorded reason; T1b requires one inline", entry.class, key)
		}
	}
}

// sealedDeletedFloor guards the census's own MEMORY, and it exists because an audit
// found the population it protects was protected by nothing at all.
//
// unclassified and resurrected both iterate the CURRENT surface, and stale filters to
// sanctioned/pending. So every SEALED and DELETED entry could be DELETED FROM THIS MAP
// and all three assertions would stay green — and the next time someone re-exported
// Node.Action, the resurrection guard would have no record that it was ever sealed.
// The guard against re-export was only as strong as a list nothing guarded.
//
// A floor rather than an exact count, for the mirror of the ceiling's reason: it may
// rise as the seal proceeds and may never fall. Both values written here have been
// MEASURED: 19 replaced a hand-count of 22 that this assertion rejected, and T6 raised
// it to 34 by setting it absurdly high and reading the number the guard printed back —
// never by adding 15 to 19, which is how the thirteen short hand-counts of this
// milestone were all produced.
const sealedDeletedFloor = 34

// TestSurfaceCensus_RemembersWhatItSealed pins that memory.
func TestSurfaceCensus_RemembersWhatItSealed(t *testing.T) {
	var kept int
	for _, entry := range surfaceCensus {
		if entry.class == sealed || entry.class == deleted {
			kept++
		}
	}
	require.GreaterOrEqual(t, kept, sealedDeletedFloor,
		"the census has FORGOTTEN a sealed or deleted identifier: %d recorded, floor %d. Removing an "+
			"entry from surfaceCensus silently disarms the resurrection guard for that identifier — "+
			"nothing else in this file would notice. Raise this floor as the seal proceeds; never lower it.",
		kept, sealedDeletedFloor)
}

// pendingDispositionCeiling is a RATCHET, and it must reach zero before phase 117 closes
// — the Definition of Done admits no exported identifier whose disposition is merely
// deferred. Lower it as each ruling lands; it may never be raised.
//
// It is a ceiling rather than an exact count for one reason: raising it is how a
// deferral silently becomes permanent. A new exported identifier arriving as PENDING
// reds here, which is the drift this whole file exists to catch.
//
// # T6 took it 12 -> 0. The descent, PER SYMBOL, because one number hides a swap
//
//	NewDAG                        -> newDAG                        BYPASS-09
//	NewDAGWithCapacity            -> newDAGWithCapacity            the one build() calls
//	(*DAG).AddNode                -> addNode
//	(*DAG).AddDependency          -> addDependency                 in no hand inventory
//	(*Workflow).AddNode           -> addNode
//	(*Workflow).AddDependency     -> addDependency
//	Workflow.DAG                  -> dag + DAG()                   BYPASS-10
//	NewWorkflow                   -> newWorkflow                   stays UNSTAMPED
//	NewTimerNode                  -> newTimerNode
//	NewWaitForSignalNode          -> newWaitForSignalNode
//	NewWaitForConditionNode       -> newWaitForConditionNode
//	NewWaitForSignalOrTimeoutNode -> newWaitForSignalOrTimeoutNode
//
// EVERY ONE IS A REAL SEAL. None reached zero by RELABELLING a disposition, which is the
// cheat this ratchet is most exposed to: T6 both edits this instrument and is measured
// by it. NewDAG and NewDAGWithCapacity moved TOGETHER for that reason — build() calls
// the second, so sealing the first alone would have left BYPASS-09 intact verbatim while
// this number still ticked down by one.
//
// THREE MORE WERE SEALED THAT THIS COUNT DOES NOT SHOW, and the arithmetic must be said
// out loud rather than left to look tidy (F117-T6-04). NewNode, NewNodeWithCapacity and
// Workflow.Registry were classified SANCTIONED here while PLAN section T6 ruled all six
// Node constructors sealed and the architect ruled Registry into the seal (R-03). The
// instrument and the plan disagreed and the instrument was wrong. Sealing them moves
// sanctioned -> sealed, so it does NOT change the pending count: 12 -> 0 is done by the
// twelve above, and these three are extra work the ratchet is structurally unable to
// report. A ratchet that can reach zero while three ruled seals go unrecorded is a
// ratchet worth distrusting on exactly that axis.
//
// # RE-BITE-PROVEN AFTER THE REWRITE, because the original proof does not survive one
//
// All four assertions, seeded one at a time at the post-T6 census:
//
//	A resurrect a DELETED symbol (re-add Node.AddDependency)
//	  -> "RESURRECTED identifier(s): [Node.AddDependency() (method, recorded as DELETED)]"
//	B a NEW exported identifier arrives (add DAG.NewlyExportedThing)
//	  -> "UNCLASSIFIED exported identifier(s) on Node/DAG/Workflow: [DAG.NewlyExportedThing() (method)]"
//	C revert a disposition to PENDING (NewDAG sealed -> pending)
//	  -> reds as "STALE census entr(ies): [NewDAG]", NOT via this ceiling. Recorded as
//	     measured rather than as expected: with the symbol already unexported, a pending
//	     entry is stale first, so the stale check reaches it before the ceiling does. The
//	     ceiling now guards only a genuinely-still-exported deferral.
//	D delete a sealed entry from the map (drop Node.Timeout)
//	  -> "the census has FORGOTTEN a sealed or deleted identifier: 33 recorded, floor 34"
//
// D IS ALSO THIS SESSION'S LESSON ABOUT BITES. Its first seed silently did not apply —
// one space too many in the match string — and the run came back with NO OUTPUT, which
// reads exactly like "the guard did not fire". It was re-seeded with an assertion that
// the match landed. A bite that cannot prove its seed applied is not evidence of
// anything.
//
// The value is MEASURED, not counted. The first value written here was a hand-count of 17 and
// this assertion rejected it — the thirteenth short hand-count of this milestone, caught
// by the check within a minute of the check existing.
const pendingDispositionCeiling = 0

// TestSurfaceCensus_NoPendingDispositions keeps the phase's remaining seal work
// MECHANICALLY ENUMERABLE instead of remembered. PENDING means "still exported, ruling
// owed by a named task" — it is a classification, never a decision, and the distinction
// is the point: without a separate class, deferring a disposition and sanctioning it
// look identical in the census, and "0 unclassified" would read green over undecided
// surface.
func TestSurfaceCensus_NoPendingDispositions(t *testing.T) {
	var owed []string
	for key, entry := range surfaceCensus {
		if entry.class == pending {
			owed = append(owed, fmt.Sprintf("  %-34s %s", key, entry.why))
		}
	}
	sort.Strings(owed)

	if len(owed) > 0 {
		t.Logf("%d exported identifier(s) still awaiting a disposition ruling:\n%s",
			len(owed), strings.Join(owed, "\n"))
	}

	require.LessOrEqual(t, len(owed), pendingDispositionCeiling,
		"the PENDING set GREW to %d (ceiling %d). Either a new exported identifier arrived without a "+
			"deliberate classification, or a disposition was reverted. Lower this ceiling as rulings land; "+
			"never raise it.", len(owed), pendingDispositionCeiling)
}

// printCensus emits the current surface in golden-list form, so the list is regenerable
// rather than hand-maintained. An artifact whose whole purpose is to abolish
// hand-maintained inventories, maintained by hand across six edits, would only ever bite
// drift from the last hand edit.
func printCensus(t *testing.T, current map[string]string) {
	t.Helper()

	// Emit the UNION of the golden list and the current surface, not just the current
	// surface. Printing only what is exported today would drop every SEALED and DELETED
	// entry, and pasting that back would annihilate the very union this list exists to
	// hold — the resurrection guard would silently lose its whole population.
	keys := map[string]bool{}
	for k := range current {
		keys[k] = true
	}
	for k := range surfaceCensus {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var b strings.Builder
	fmt.Fprintf(&b, "\n// census union: %d entr(ies) (%d currently exported)\n", len(ordered), len(current))
	for _, k := range ordered {
		if e, ok := surfaceCensus[k]; ok {
			fmt.Fprintf(&b, "\t%q: {%s, %q}, // %s\n", k, strings.ToLower(string(e.class)), e.why, current[k])
			continue
		}
		// UNKNOWN key: emit it so it CANNOT pass. It is classified pending with an
		// EMPTY reason, which fails the T1b recorded-reason assertion.
		//
		// The earlier version defaulted an unknown key to `sanctioned` with a
		// "TODO: record why" string. That laundered a brand-new unclassified export
		// into a sanctioned one, and the non-empty TODO satisfied the very assertion
		// meant to prevent it — the artifact discharging its own obligation. An
		// unclassified export must cost a human decision, so regeneration hands you a
		// test that fails until you make one.
		fmt.Fprintf(&b, "\t%q: {pending, \"\"}, // %s — UNCLASSIFIED, record a reason\n", k, current[k])
	}
	t.Log(b.String())
}
