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

// topologyFields are the three fields that ARE the graph. Everything else on Node and
// DAG is policy, config or bookkeeping; these three decide what runs and in what order.
var topologyFields = map[string]bool{
	"nodes":     true, // DAG.nodes  — the node set
	"dependsOn": true, // Node.dependsOn — the edge set
	"action":    true, // Node.action — the code a node runs
}

// SEAL-08's THIRD CLAUSE: the golden list widens from "no exported identifier was added"
// to "the WRITER SET of the topology fields is exactly this list".
//
// # Why the census cannot do this job
//
// The census tracks IDENTIFIERS. It answers "did an exported symbol appear or come
// back", which is what makes the T6 seal durable against a re-export. It cannot answer
// "did someone write to the edge set", and the two are genuinely different questions —
// the phase's own worst case is a NEW in-package write added later, under an existing
// exported method, with no identifier changing at all.
//
// That gap is what makes provenance⇒integrity durable rather than true-as-of-today. The
// builder token certifies that a graph passed build(); it does NOT certify that nothing
// touched the graph afterwards. The only thing that can keep the second claim honest is
// a guard over the writes themselves.
//
// # The instrument is FIELD-WRITE shaped, not METHOD shaped, and qa ruled it so
//
// The obvious implementation asks "which exported METHODS write topology" — walk the
// exported methods of DAG and Node and check their bodies. That instrument reads CLEAN
// over the single most interesting line in the package:
//
//	validateReconvergence:  e.merge.dependsOn = append(e.merge.dependsOn, e.choice)
//
// It is a write to a REACHED node's edge set, from a package-level function with no
// receiver at all, appending to a node it does not own. A method-shaped sweep never
// looks there, and it is not hypothetical — it is the write that makes "stamped ⇒
// topologically unchanged" false INSIDE the package, and the reason the stamp is
// documented as provenance rather than integrity.
//
// So this sweeps EVERY assignment in every non-test file of the package, receiver or
// not, method or free function, and reports the enclosing function of each. The census's
// own MUTATES column has the same method-shaped blind spot and says so in its doc; this
// guard is the answer to it.
//
// # Reading a failure
//
// A red here is not automatically a defect — build() is supposed to write these fields.
// It means the writer set CHANGED, and the change needs a deliberate decision: either
// the write belongs (add it here, with the reason) or it is a post-Build mutation of a
// validated graph, which is the entire subject of this phase.
//
// # Bites — both run, both against seeds already in the tree
//
// 1. qa's named seed. Dropping "validateReconvergence" from the list below reds with
//
//			UNDECLARED writer(s) of graph topology: [reconvergence.go:184:3 dependsOn (in validateReconvergence)]
//
//		   A method-shaped guard cannot produce that failure AT ALL — the write has no
//		   receiver — which is the entire argument for this instrument's shape.
//
//	 2. Anti-vacuity. Renaming the three entries in topologyFields so they match nothing
//	    reds with "the sweep found only 0 topology writes in the whole package; it is
//	    BROKEN, not the code" — rather than reporting an empty undeclared list, which is
//	    what a clean pass also looks like.
//
// # The mirror check found a hole in this very sweep, which is why it is here
//
// The stale-entry assertion at the bottom is not symmetry for its own sake. Its first
// run reported "addNode" as a declared writer that writes nothing — and addNode's entire
// body is `d.nodes[node.name] = node`. The sweep had matched only SelectorExpr on the
// left of an assignment, so an INDEX write was invisible, and the single most important
// topology write in the package was going unchecked while the guard reported clean. The
// substantive assertion could never have caught that; only asking "does every declared
// writer still write?" could.
//
// It also caught an entry that was pure invention on my part — "expandFanOut", a
// function that does not exist. M21 fan-out runs N branches as ONE node and writes no
// graph topology at runtime, so the residual I had attributed to it is not real.
func TestSealed_TopologyWriterSetIsDeclared(t *testing.T) {
	// THE DECLARED WRITER SET: enclosing function -> why it may write one of these fields.
	//
	// CEILING, stated because it decides how a failure must be read: the sweep matches
	// FIELD NAMES, not resolved types. Go's type checker is what would tell Node.action
	// apart from NodeBuilder.action, and wiring go/types into a test would mean adding a
	// dependency for a guard — so this instrument deliberately guards a SUPERSET: every
	// write to a field NAMED nodes, dependsOn or action anywhere in the package.
	//
	// That is qa's ruling taken literally ("sweep FIELD WRITES package-wide, receiver or
	// not"), and the superset is a feature rather than a concession: the builder's own
	// assembly gets the same treatment as the graph's. The cost is that each entry must
	// SAY WHICH TYPE it writes, because the sweep cannot, and a red must be read against
	// the label rather than assumed to be about the graph.
	allowed := map[string]string{
		// ── The graph itself: Node and DAG. These are the entries the seal is about. ──
		"newDAG":              "DAG.nodes — mints the empty node map",
		"newDAGWithCapacity":  "DAG.nodes — capacity-hinted mint; the constructor build() calls",
		"newNode":             "Node.action/dependsOn — mints a node",
		"newNodeWithCapacity": "Node.action/dependsOn — the builder's mint",
		"addNode":             "DAG.nodes — sealed by T6; reachable from build() and in-package tests only",
		"addDependency":       "Node.dependsOn on a REACHED node — sealed by T6",
		"build":               "Node/DAG — assembles the whole graph, then stamps it. The sanctioned path",
		"validateReconvergence": "Node.dependsOn on a REACHED node, from a function with NO RECEIVER, " +
			"inside build() and before the stamp (DEC-M11-DEPMODEL). THE ENTRY THIS INSTRUMENT'S SHAPE " +
			"EXISTS FOR: a method-shaped sweep cannot see it, and it is why the stamp is documented as " +
			"provenance and not integrity",
		"validateBoundary": "Node.action on a REACHED node, from a function with NO RECEIVER, inside " +
			"build() and before the stamp — the same shape and the same seat as validateReconvergence " +
			"above (M23 VB-01, 118-D4). IT REPLACES A VALIDATED ACTION WITH A SNAPSHOT OF ITSELF, and " +
			"only for a declared boundary's verifier and sink. The write exists BECAUSE of the seal " +
			"rather than in tension with it: CompositeAction.Add is exported and appends to the slice " +
			"the built DAG holds by pointer, so without the snapshot the action clause certifies " +
			"something the consumer can still change afterwards — the built token would certify a state " +
			"the graph no longer has. Declared here rather than moved, because it must run AFTER the " +
			"clause it snapshots the result of. Adding this entry was forced by this guard, not chosen: " +
			"the fix landed and the suite went red",

		// ── NodeBuilder.action — the builder recording which action a node will get. ──
		// Pre-build, on a builder, so none of these touches a validated graph.
		"WithAction":              "NodeBuilder.action — the generic action setter",
		"WithActionFunc":          "NodeBuilder.action — AUD-041 typed func setter (wraps a bare func in ActionFunc; same pre-build seat as WithAction)",
		"AddTimer":                "NodeBuilder.action — declared timer node",
		"AddWaitForSignal":        "NodeBuilder.action — declared signal-wait node",
		"AddWaitForSignalTimeout": "NodeBuilder.action — declared first-of(signal, timer) node",
		"AddWaitForCondition":     "NodeBuilder.action — declared condition-wait node",
		"AddApproval":             "NodeBuilder.action — declared approval gate",
		"AddSubWorkflow":          "NodeBuilder.action — inline sub-workflow",
		"AddSubWorkflowParked":    "NodeBuilder.action — parked sub-workflow",
		"AddSubWorkflowQueued":    "NodeBuilder.action — queue-dispatched sub-workflow",
		"AddChoice":               "NodeBuilder.action — M11 choice node",
		"AddMerge":                "NodeBuilder.action — M11 merge node",
		"AddFanOut":               "NodeBuilder.action — M21 fan-out node",

		// ── Same-named fields on unrelated types. Not graph topology at all; present
		// only because the sweep is name-based, and listed so that is visible. ──
		"NewWorkflowBuilder": "WorkflowBuilder.nodes — the builder's own node-builder slice",
		"AddNode":            "WorkflowBuilder.nodes — appends a NodeBuilder, not a graph node",
		"NewRetryableAction": "RetryableAction.action — the wrapped action, not a node's",
		"snapshotBoundaryAction": "RetryableAction.action ON A LOCAL COPY (`c := *v; c.action = …`) — " +
			"not a node's, and not even the original wrapper's. Same category as NewRetryableAction " +
			"above: the sweep is name-based and cannot see that the receiver is a stack copy, so the " +
			"entry is here to make that visible rather than to grant a permission",
		"recordDelta":    "the WorkflowData delta's touched-node set (M15)",
		"emptyShadow":    "the SQLite incremental shadow's node map (M15)",
		"clone":          "the SQLite incremental shadow's node map (M15)",
		"shadowFromData": "the SQLite incremental shadow's node map (M15)",
		"applyTo":        "the SQLite delta's node map (M15) — surfaced only once index writes were matched",
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "sanity: the sweep must find package source to parse")

	fset := token.NewFileSet()
	var undeclared []string
	var seen = map[string]bool{}
	writeCount := 0

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
			fn := fd.Name.Name

			// record notes a write to `field` at pos inside fn.
			record := func(field string, pos token.Pos) {
				if !topologyFields[field] {
					return
				}
				writeCount++
				seen[fn] = true
				if _, ok := allowed[fn]; !ok {
					undeclared = append(undeclared,
						fset.Position(pos).String()+" "+field+" (in "+fn+")")
				}
			}

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				// Any assignment whose LHS selects one of the three fields — x.nodes = …,
				// e.merge.dependsOn = …, n.action = …. The RECEIVER IS NOT EXAMINED, which
				// is the point: a write to someone else's node counts.
				if as, ok := n.(*ast.AssignStmt); ok {
					for _, lhs := range as.Lhs {
						// x.field = …
						if sel, ok := lhs.(*ast.SelectorExpr); ok {
							record(sel.Sel.Name, sel.Pos())
							continue
						}
						// x.field[k] = … — an INDEX write, and missing this was a real hole
						// rather than a hypothetical one: (*DAG).addNode's whole body is
						// `d.nodes[node.name] = node`, so the single most important write in
						// the package was invisible to the first version of this sweep. It
						// was the STALE-ENTRY mirror below that surfaced it, by reporting
						// addNode as a declared writer that writes nothing.
						if ix, ok := lhs.(*ast.IndexExpr); ok {
							if sel, ok := ix.X.(*ast.SelectorExpr); ok {
								record(sel.Sel.Name, sel.Pos())
							}
						}
					}
					return true
				}
				// Composite literals — &Node{action: …, dependsOn: …} inside the mints.
				if cl, ok := n.(*ast.CompositeLit); ok {
					for _, elt := range cl.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								record(id.Name, kv.Pos())
							}
						}
					}
				}
				return true
			})
		}
	}

	// ANTI-VACUITY, and ahead of the substantive assertion on purpose. If the sweep
	// matched nothing — a parse that silently found no files, a field renamed out from
	// under topologyFields — it would report an EMPTY undeclared list, i.e. a clean pass,
	// while checking nothing whatsoever. That is the failure mode this phase has hit
	// twice already in the mint sweep, both times caught only by a count assertion.
	require.Greater(t, writeCount, 10,
		"the sweep found only %d topology writes in the whole package; it is BROKEN, not the "+
			"code. Do not read the assertion below as a clean writer set", writeCount)

	sort.Strings(undeclared)
	require.Empty(t, undeclared,
		"UNDECLARED writer(s) of graph topology: %v\n"+
			"Something now writes nodes/dependsOn/action from a function that is not in this "+
			"test's declared writer set. That is not automatically wrong — build() writes them "+
			"legitimately — but it IS a change to the set the seal's 'stamped => topologically "+
			"unchanged FROM OUTSIDE' claim rests on. Decide deliberately: add it here with its "+
			"reason, or move the write inside build().", undeclared)

	// The mirror check. A declared writer that no longer writes anything means this list
	// has gone stale, and a stale allow-list silently widens: it keeps permitting a
	// function that may since have been repurposed.
	var vestigial []string
	for fn := range allowed {
		if !seen[fn] {
			vestigial = append(vestigial, fn)
		}
	}
	sort.Strings(vestigial)
	require.Empty(t, vestigial,
		"declared topology writer(s) that write nothing: %v. The allow-list is stale — an entry "+
			"that permits a write nobody makes is a permission waiting to be silently reused.",
		vestigial)
}
