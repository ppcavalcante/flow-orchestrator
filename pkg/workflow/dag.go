package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DAG represents a Directed Acyclic Graph of workflow nodes.
// It maintains the structure of the workflow and handles dependency resolution.
type DAG struct {
	// nodes contains all nodes in the DAG, keyed by name. Sealed by M23 SEAL-02: the
	// map was exported, so a consumer could REPLACE or DROP a node on a graph build()
	// had already validated. Read access is GetNode / GetLevels.
	//
	// ADDING IS STILL OPEN and this comment used to claim otherwise: (*DAG).AddNode and
	// (*Workflow).AddNode remain exported, so a node can still be smuggled onto a
	// validated DAG and it will execute. Sealing the map closed the replace/drop half;
	// SEAL-06 (T6) closes the add half.
	nodes map[string]*Node

	// name is the identifier for this DAG. Sealed by M23 T1c: it had zero
	// out-of-package readers — the only two live reads are in-package (the
	// current_level_* WorkflowData key here, and the suspendable-child error in
	// subworkflow.go) — so exporting it bought nothing and widened the surface.
	name string

	// cycleNodes stores nodes involved in cycles (if any). Unexported by M23 T1c
	// rather than deleted: unlike StartNodes/EndNodes it is genuinely live — Validate
	// reads it back to build the "cycle detected" error message.
	cycleNodes []string

	// config controls execution behavior (e.g. per-level concurrency).
	// Defaults to DefaultConfig(); override via WorkflowBuilder.WithExecutionConfig
	// or DAG.WithExecutionConfig.
	config ExecutionConfig

	// hasFanOut is true iff any node's action is a *fanOutAction (M21). Precomputed
	// at AddNode so DAG.Execute can GATE the withMaxConcurrency ctx-wrap on it: a
	// workflow with no fan-out node pays ZERO — no per-drive context.WithValue alloc
	// on the universal hot path (the det-tax moat). The wrap is only needed so a
	// fan-out node reads its own MaxConcurrency bound from ctx.
	hasFanOut bool

	// boundaries are the consumer-declared (D,V,S) triples build() validated. Unexported
	// and in the RUN-CONSTANCY class: set only by build(), so a resume re-derives them
	// from the rebuilt graph rather than reading them back from a store.
	boundaries []boundaryDecl

	// hasBoundaries is true iff boundaries is non-empty. Precomputed for the same reason
	// hasFanOut is (above): a workflow declaring no boundary must pay ZERO on the drive
	// path. The zero-determinism-tax claim is this project's headline.
	hasBoundaries bool

	// built records that (*WorkflowBuilder).build produced this DAG — M23 SEAL-06's
	// builder token. Execute refuses a DAG without it (ErrDAGNotBuilt), and so does
	// childRunFailed, which renders a run's verdict from a DAG without executing it.
	//
	// THE GUARANTEE IS A BOUNDARY GUARANTEE, NOT A GRAPH INVARIANT. Stated precisely,
	// because the imprecise version is an overclaim and this phase has shipped several:
	//
	//	stamped => topologically unchanged FROM OUTSIDE THE PACKAGE
	//
	// It is NOT true inside. validateReconvergence appends edges (legitimately, as
	// build()'s last act), AddDependency survives merely unexported, and ANY in-package
	// test file can mutate a stamped graph. What the seal closes is the EXTERNAL route,
	// which is what the finding was about — a consumer re-parenting a node on a validated
	// graph, or a DAGFactory handing the dispatch path something build() never saw.
	//
	// So this certifies "build() ran on this object and its validation passed AT THAT
	// MOMENT". It does NOT certify that nothing changed afterwards. In-package code can still append to a node's dependsOn on a stamped
	// DAG and this flag will not notice — validateReconvergence itself does exactly that,
	// legitimately, as build()'s last act. The check that WOULD deliver integrity is
	// re-running the reconvergence validation at every drive; that is deliberately NOT in
	// 117 (architect ruling): unexporting both AddDependency methods closes the EXTERNAL
	// route, which was the actual finding, and buying the in-package residual with an
	// O(V+E) pass on every timer wake is a separate obligation with its own cost. The
	// residual is recorded as a finding rather than left for silence to imply it is closed.
	//
	// WHY THE LAST STATEMENT OF build() AND NOWHERE ELSE. build() runs dag.Validate()
	// and THEN validateReconvergence(dag), which APPENDS the DEC-M11-DEPMODEL merge<-choice
	// edges — so a stamp written after Validate() would certify a graph that then gained
	// edges. Set it anywhere but build()'s final statement and the token certifies
	// something other than "build() validated this", at which point it is decoration that
	// leaves every test green. If you are reading this because you want to stamp inside
	// NewDAG/NewDAGWithCapacity to make some test pass: that is the exact failure this
	// design was chosen to avoid. Use the named in-package test mint instead.
	//
	// RUN-CONSTANCY (the D-07 shape, same as suspendable). NEVER PERSISTED: no serializer
	// touches the DAG struct at all — the stores serialize WorkflowData — and an
	// unexported field is skipped structurally by gob/json regardless. NEVER part of
	// checkGraphIdentity, which compares persisted node-status keys against the live DAG.
	// A resume rebuilds the DAG by re-running the consumer's own construction code, so a
	// fresh process re-derives the flag identically. Nothing here is run-varying.
	//
	// UNFORGEABLE THROUGH SERIALIZATION — a real property of the VALUE-field choice, and
	// the reason not to use a pointer-identity registry (a `map[*DAG]struct{}` is defeated
	// by `cp := *builtDAG`, one line, legal from outside). Measured: json.Marshal of a
	// built DAG gives "{}", json.Unmarshal into a &DAG{} succeeds with the ZERO stamp, and
	// gob.Encode errors outright ("type workflow.DAG has no exported fields"). Bytes handed
	// to us cannot carry a forged stamp.
	//
	// THAT HOLDS ONLY WHILE DAG IMPLEMENTS NO CUSTOM CODEC. Add an UnmarshalJSON and the
	// stamp becomes forgeable by anyone who can hand us bytes. Guarded by
	// TestSealed_NoCustomCodecOnDAGOrNode, because "a workflow is DATA, not CODE" makes
	// serializing a DAG exactly the feature someone eventually wants.
	//
	// WHY A RUNTIME CHECK ENDS A STRUCTURAL PHASE — the question a future reader will ask.
	// Unexporting cannot close construction forms that NAME NO IDENTIFIER: &workflow.DAG{},
	// new(workflow.DAG), var d workflow.DAG, a slice or array element, an embedded
	// `struct{ workflow.DAG }` or a plain DAG-typed field, a reflect.New, a value copied out
	// of a map, a receive from a closed channel, a generic zero value, a bare named result.
	// All are legal from outside with every field unexported, because an expression that
	// sets no fields names no field. They are invisible to go doc, to the AST census, and to
	// any symbol grep. Only a check at CONSUMPTION sees them.
	//
	// THOSE ARE EXAMPLES, NOT AN INVENTORY. They are not distinct constructions; they are
	// ways to spell the ZERO VALUE of an exported type, so the set is OPEN UNDER THE LANGUAGE
	// (generics added one for free) and no enumeration of it can stay complete. Do not read
	// this list as a closed set, and do not "fix" it by adding a count: every count anyone has
	// attached to this class has been wrong within the same phase, each more confident than
	// the last. The population is whatever the sweep below currently exercises — read it
	// there, where it is executable, rather than from a numeral here that cannot stay true.
	//
	// WHAT IS CLOSED IS THE EFFECT, and that is the property to rely on. Every form yields a
	// ZERO graph, never a populated rogue one — reflection reaches the unexported nodes map
	// but CanSet is false — so this is a vacuous-success class, not arbitrary-graph
	// injection. That is why ONE check at consumption is total over the class HOWEVER MANY
	// WAYS there turn out to be to spell it. Checkability here is "re-run the sweep and
	// confirm every form still refuses", never "count the list": a count cannot survive the
	// next syntax addition, and the invariant can. That sweep is
	// TestSealed_EveryExternalZeroFormIsRefused, in package workflow_test because the hole
	// is a BOUNDARY property and cannot be exhibited from inside the package at all.
	//
	// THE REFUSAL NAMES ITS CAUSE, and that is usability more than security. The consumer
	// who trips this will most often be someone who EMBEDDED the type — an ordinary Go
	// idiom that hands them the zero value for free, with no literal written anywhere.
	// Their code compiles, looks normal, and dies at the drive. A bare validation error
	// there is baffling; ErrDAGNotBuilt says construction was the problem.
	//
	// COST (SEAL-05): one bool, no allocation, and the check is a single branch ONCE per
	// drive — not per node. This is hasFanOut's shape, the landed precedent above.
	built bool

	// mu protects concurrent access to the DAG. It is a POINTER, not a value, and that is
	// load-bearing (AUD-002 / 116-GC-F7).
	//
	// A sync.RWMutex is a value type: copying a struct that embeds one BY VALUE copies the
	// lock's internal state. A copy taken while the lock is held inherits a mutex that is
	// LOCKED WITH NO OWNER AND NO UNLOCKER — the next Lock() on the copy blocks forever.
	// DAG is an exported type whose zero value is constructible from outside the package
	// (see the `built` doc above), and Validate() — called by Execute on every drive — takes
	// this write lock; so `cp := *builtDAG` taken during a concurrent drive produced a stamped,
	// seal-admitted graph that hung permanently inside Validate, violating the no-input-hangs
	// hard bar. Reachable with EXPORTED API ONLY: Workflow.DAG() hands out the live pointer, a
	// value copy of the exported struct is one line, and Validate() is exported.
	//
	// A pointer mutex removes the failure at the root: a value copy of a DAG copies the
	// POINTER, so the copy SHARES the one mutex rather than duplicating its locked state.
	// There is no longer a lock value to inherit locked, so no copy can be born wedged.
	//
	// The invariant this rests on: every DAG that ever locks was produced by newDAG /
	// newDAGWithCapacity, which set this field. A raw zero-value DAG (&DAG{}, new(DAG), …) has
	// mu == nil and is never built, so it never reaches a real drive — Execute and
	// childRunFailed both refuse it on the `built` check before any lock. The three exported
	// readers that could still be called directly on such a zero value (GetNode, Validate,
	// GetLevels) nil-guard mu and return the empty-graph answer rather than dereferencing nil.
	// The `built` stamp stays a VALUE bool: its unforgeable-through-serialization property
	// (documented above) is independent of the mutex and is deliberately unchanged here.
	mu *sync.RWMutex
}

// ErrDAGNotBuilt is returned when a *DAG that did not come from the builder is handed
// to the engine — either to run (Execute) or to render a run's verdict (childRunFailed).
// M23 SEAL-06.
//
// It is an EXECUTION-domain sentinel, deliberately not aliased to the store domain's
// ErrValidation: "this graph was never validated" is a different concept from "this
// request was malformed", and the two-domain separation is a standing property of this
// package's error surface.
//
// The reachable causes, all of them consumer-side: a DAG assembled with the low-level
// constructors instead of WorkflowBuilder (before M23 those were exported); a zero value
// (&workflow.DAG{} stays constructible forever, because the TYPE must remain exported as
// Build's return); or a DAGFactory that returns a hand-rolled graph — which is the M17
// dispatch finding this phase exists to close.
var ErrDAGNotBuilt = errors.New("DAG was not produced by WorkflowBuilder.Build (M23 SEAL-06: only a built, validated graph may execute or render a verdict)")

// newDAG creates a new DAG with the given name
func newDAG(name string) *DAG {
	return &DAG{
		nodes:  make(map[string]*Node),
		name:   name,
		config: DefaultConfig(),
		mu:     &sync.RWMutex{},
	}
}

// newDAGWithCapacity creates a new DAG with the given name and pre-allocated capacity.
// This can improve performance when the approximate number of nodes is known in advance.
func newDAGWithCapacity(name string, nodeCapacity int) *DAG {
	return &DAG{
		nodes:  make(map[string]*Node, nodeCapacity),
		name:   name,
		config: DefaultConfig(),
		mu:     &sync.RWMutex{},
	}
}

// Name reports the DAG's identifier. It is fixed at construction and never written
// afterwards, so it is safe to hold.
//
// The field behind it is sealed (M23 T1c). This accessor exists because the read is
// genuinely external: pkg/testutil builds a WorkflowData from it. That is the same
// shape as (*Node).Name — seal the field, expose the read — and it is the reason the
// "zero out-of-package readers" reading of this field was wrong: pkg/testutil is a
// PUBLIC package and is easy to miss when sweeping internal/ and examples/.
func (d *DAG) Name() string { return d.name }

// setConfigLocked applies mutate to d.config under the write lock when the DAG has
// a mutex (i.e. was built by newDAG). A raw zero-value &DAG{} has mu == nil and is
// never executed (Execute refuses it on the built check), so there is no drive to
// race and the mutation proceeds unlocked rather than nil-panicking. (AUD-032.)
func (d *DAG) setConfigLocked(mutate func()) {
	if d.mu != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	mutate()
}

// WithExecutionConfig sets the execution configuration (e.g. per-level
// concurrency) and returns the DAG for chaining.
//
// AUD-032: the write is synchronized against a concurrent Execute, which snapshots
// d.config under the read lock at entry. This is a convenience setter for the
// build/setup phase; mutating config while a drive is in flight is still a misuse
// (the in-flight drive keeps its entry snapshot), but it can no longer be a data race.
func (d *DAG) WithExecutionConfig(config ExecutionConfig) *DAG {
	d.setConfigLocked(func() { d.config = config })
	return d
}

// WithTracerProvider sets the OpenTelemetry trace provider used to emit a span
// per executed node (with a parent span per workflow run) and returns the DAG
// for chaining. Passing nil disables tracing (the zero/default state). This is
// API-only: the host owns the SDK and exporter; the library only emits spans
// through the provided provider (DEC-M6-otel-api-only parity, DEC-CHUNK5).
func (d *DAG) WithTracerProvider(tp trace.TracerProvider) *DAG {
	d.setConfigLocked(func() { d.config.TracerProvider = tp }) // AUD-032: synchronized like WithExecutionConfig
	return d
}

// AddNode adds a node to the DAG.
// Returns an error if a node with the same name already exists.
func (d *DAG) addNode(node *Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.nodes[node.name]; exists {
		return fmt.Errorf("node with name %s already exists", node.name)
	}

	d.nodes[node.name] = node
	// Precompute the fan-out flag (M21 det-tax gate): so DAG.Execute wraps the ctx with the MaxConcurrency seam
	// ONLY for a DAG that actually contains a fan-out node — a non-fan-out workflow pays zero on the hot path.
	if _, ok := node.action.(*fanOutAction); ok {
		d.hasFanOut = true
	}
	return nil
}

// GetNode retrieves a node by name.
// Returns the node and a boolean indicating if the node exists.
func (d *DAG) GetNode(name string) (*Node, bool) {
	// AUD-002: a raw zero-value DAG (never through newDAG) has a nil mu. It also has a nil
	// nodes map, so the answer is unconditionally "not found" — return it without locking,
	// exactly as a locked read of the nil map would, instead of dereferencing nil mu.
	if d.mu == nil {
		return nil, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	node, exists := d.nodes[name]
	return node, exists
}

// addDependency creates a dependency between two nodes. It was exported until SEAL-06
// unexported it; `go doc DAG` reports no AddDependency.
// The toNode will depend on the fromNode, meaning fromNode must complete before toNode can start.
// Returns an error if either node doesn't exist or if adding the dependency would create a cycle.
func (d *DAG) addDependency(fromNode, toNode string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	from, fromExists := d.nodes[fromNode]
	to, toExists := d.nodes[toNode]

	if !fromExists {
		return fmt.Errorf("node %s does not exist", fromNode)
	}

	if !toExists {
		return fmt.Errorf("node %s does not exist", toNode)
	}

	// toNode depends on fromNode (fromNode must complete first)
	to.dependsOn = append(to.dependsOn, from)
	return nil
}

// Validate checks the DAG for validity, including cycle detection.
// Returns an error if the DAG is invalid.
func (d *DAG) Validate() error {
	// AUD-002: a raw zero-value DAG (never through newDAG) has a nil mu and a nil nodes map.
	// An empty graph is valid (the nodesCount == 0 branch below), so return that answer
	// without locking rather than dereferencing nil mu. A drive never reaches here on such a
	// DAG — Execute refuses it on the `built` check first — so this only guards a direct
	// Validate() call on a bare zero value, and preserves its pre-AUD-002 result (nil).
	if d.mu == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	nodesCount := len(d.nodes)

	// If the DAG is empty, it's valid
	if nodesCount == 0 {
		return nil
	}

	// Check for cycles
	visited := make(map[string]bool, nodesCount)
	inProgress := make(map[string]bool, nodesCount/2+1)
	d.cycleNodes = make([]string, 0, nodesCount/2+1) // Preallocate with reasonable capacity

	// Check each node for cycles
	for name := range d.nodes {
		if !visited[name] {
			if d.detectCycle(name, visited, inProgress) {
				return fmt.Errorf("cycle detected in graph: %s", strings.Join(d.cycleNodes, " -> "))
			}
		}
	}

	// M23 T1c: the start/end-node identification that used to run here is GONE, with
	// the two fields it populated. It was not merely production-dead state — it was
	// dead COMPUTATION on the hot path: Validate is called from Execute, so every run
	// built a hasDependents map sized to the node count, plus two capacity-hinted
	// slices, to fill two exported fields that nothing in the engine ever read.
	// Measured before removing: three writes each, zero reads outside one test.
	return nil
}

// detectCycle detects cycles in the DAG using DFS
func (d *DAG) detectCycle(nodeName string, visited, inProgress map[string]bool) bool {
	visited[nodeName] = true
	inProgress[nodeName] = true

	node := d.nodes[nodeName]
	for _, dep := range node.dependsOn {
		if !visited[dep.name] {
			if d.detectCycle(dep.name, visited, inProgress) {
				d.cycleNodes = append([]string{nodeName}, d.cycleNodes...)
				return true
			}
		} else if inProgress[dep.name] {
			// Cycle detected
			d.cycleNodes = append([]string{nodeName, dep.name}, d.cycleNodes...)
			return true
		}
	}

	inProgress[nodeName] = false
	return false
}

// GetLevels returns the nodes organized into levels for parallel execution.
// Uses O(V+E) algorithm with a reverse adjacency list.
func (d *DAG) GetLevels() [][]*Node {
	// AUD-002: a raw zero-value DAG (never through newDAG) has a nil mu and a nil nodes map,
	// which yields nil levels — return that without locking rather than dereferencing nil mu.
	if d.mu == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(d.nodes) == 0 {
		return nil
	}

	// Build reverse adjacency list: for each node, which nodes depend on it
	dependents := make(map[string][]*Node, len(d.nodes))
	inDegree := make(map[string]int, len(d.nodes))
	nodeLevels := make(map[string]int, len(d.nodes))
	queue := make([]*Node, 0)

	// Initialize and build reverse edges
	for _, node := range d.nodes {
		inDegree[node.name] = len(node.dependsOn)
		if len(node.dependsOn) == 0 {
			queue = append(queue, node)
			nodeLevels[node.name] = 0
		}
		for _, dep := range node.dependsOn {
			dependents[dep.name] = append(dependents[dep.name], node)
		}
	}

	// Process nodes level by level using reverse adjacency list
	maxLevel := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		// Process only nodes that depend on this node (O(out-degree))
		for _, dependent := range dependents[node.name] {
			// Track max level from all parents
			if candidateLevel := nodeLevels[node.name] + 1; candidateLevel > nodeLevels[dependent.name] {
				nodeLevels[dependent.name] = candidateLevel
			}
			inDegree[dependent.name]--
			if inDegree[dependent.name] == 0 {
				if nodeLevels[dependent.name] > maxLevel {
					maxLevel = nodeLevels[dependent.name]
				}
				queue = append(queue, dependent)
			}
		}
	}

	// Create level slices
	levels := make([][]*Node, maxLevel+1)
	for name, level := range nodeLevels {
		levels[level] = append(levels[level], d.nodes[name])
	}

	// Sort nodes within each level by name for deterministic ordering
	for i := range levels {
		sort.Slice(levels[i], func(j, k int) bool {
			return levels[i][j].name < levels[i][k].name
		})
	}

	return levels
}

// TopologicalSort returns the nodes in a single flat topological order (a linear
// dependency-respecting sequence), deterministic by name within each ready set.
//
// AUD-039: this used to return [][]*Node{sorted} -- a flat list wrapped in a
// one-element outer slice, which read as if it returned parallel LEVELS. It does
// not; it is one linear order. For the level-wise structure the executor uses (each
// inner slice a set of independent, concurrently-runnable nodes), call GetLevels.
// The return type is now the honest []*Node.
func (d *DAG) TopologicalSort() ([]*Node, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}

	if len(d.nodes) == 0 {
		return []*Node{}, nil
	}

	// Build reverse adjacency list
	dependents := make(map[string][]*Node, len(d.nodes))
	inDegree := make(map[string]int, len(d.nodes))
	queue := make([]*Node, 0)
	sorted := make([]*Node, 0, len(d.nodes))

	for _, node := range d.nodes {
		inDegree[node.name] = len(node.dependsOn)
		if len(node.dependsOn) == 0 {
			queue = append(queue, node)
		}
		for _, dep := range node.dependsOn {
			dependents[dep.name] = append(dependents[dep.name], node)
		}
	}

	// Process nodes in topological order
	for len(queue) > 0 {
		// Find node with minimum name (for deterministic ordering)
		minIdx := 0
		for i := 1; i < len(queue); i++ {
			if queue[i].name < queue[minIdx].name {
				minIdx = i
			}
		}

		// Remove node from queue
		node := queue[minIdx]
		queue = append(queue[:minIdx], queue[minIdx+1:]...)
		sorted = append(sorted, node)

		// Process dependents using reverse adjacency list
		for _, dependent := range dependents[node.name] {
			inDegree[dependent.name]--
			if inDegree[dependent.name] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	return sorted, nil
}

// Execute runs the DAG with the provided workflow data.
// Nodes are executed in topological order, with independent nodes potentially running in parallel.
// Returns an error if execution fails.
func (d *DAG) Execute(ctx context.Context, data *WorkflowData) (retErr error) {
	// M23 SEAL-06 — EXECUTION MEDIATION. Every forward route terminates here:
	// executeNodesInLevel has exactly one non-test caller (this function), and (*Node).execute
	// in turn has exactly one (executeNodesInLevel), so this is the SOLE NON-TEST GATEWAY to
	// forward node execution.
	//
	// THAT IS A REACHABILITY CLAIM, NOT A MEDIATION ONE, and the distinction is load-bearing:
	// an earlier version of this comment called the check "total over every drive family in
	// the engine", which conflated the two and was false in BOTH directions — it claimed
	// mediation, and it claimed the whole engine rather than the forward half. Sole gateway
	// says every forward route PASSES here; it does not say this is where an unstamped graph
	// is CAUGHT. On the ph94 queue path executeLocked refuses one well upstream, so this
	// check is never reached there at all.
	//
	// Nor is it total over the engine: COMPENSATION IS A SECOND DISJOINT CHANNEL
	// (finishRollback -> driveRollback -> compensateLevel -> n.compensation.Execute) that
	// never touches this function, and is covered by the executeLocked check, sited above the
	// IsRollingBack branch for exactly that reason. TWO CHECKS, TWO CHANNELS — NEITHER IS
	// TOTAL ALONE.
	//
	// Caller counts here are FUNCTIONS, not entry conditions — one caller can hold several
	// call sites under different conditions (finishRollback is reached from two), and the
	// call-graph tools dedup per caller function, so a condition-level claim needs a
	// reference-level instrument and is deliberately not made here.
	//
	// Placed HERE, not at (*Workflow).Execute, because there are
	// THREE public Workflow drive entries and two of them deliberately bypass the public
	// method: Tick (timer fire) and DeliverAndResume (signal delivery) each take the
	// per-ID lease themselves and call executeLocked directly — the single-funnel lease
	// discipline, stated in their own comments. A token checked at (*Workflow).Execute
	// would have been silently absent from exactly the durable-suspend wake, which is the
	// most crash-relevant drive here. (F-117-ARCH-02.)
	//
	// Everything funnels through this method: Workflow.Execute/Tick/DeliverAndResume ->
	// executeLocked -> w.dag.Execute; a direct consumer dag.Execute; the inline
	// sub-workflow's child.Execute; M17 dispatch and the Pool worker via w.Execute; and
	// the M21 fan-out's per-branch child.Execute.
	//
	// It does NOT cover a DAG that is consumed without being executed — the ph92 parked path
	// reads a child DAG to render a verdict and never runs it. That is a real second hole,
	// and it is closed at childRunFailed for the ph92 PARKED path. The ph94 queue path never
	// reaches childRunFailed — queueTerminalState returns exists=true from the durable
	// work_queue row and renders the verdict from the row, above that call site — so on the
	// queue path the verdict is never rendered FROM the child DAG at all.
	//
	// That scopes the VERDICT path only. A queue child's forward EXECUTION is mediated, but
	// NOT BY THIS CHECK: runNext builds &Workflow{dag: …} around the consumer factory's graph
	// and calls w.Execute, so executeLocked's token check refuses it BEFORE this method is
	// entered. MEASURED, not inferred, and the measurement is IN THE TREE: the refusal for an
	// unstamped queue child names the WORKFLOW (`workflow %q`, workflow.go), never the GRAPH
	// (`DAG %q`, here), and TestDispatch_QueueChildIsRefusedByExecuteLocked_NotByDAGExecute
	// asserts exactly that on identity. If this paragraph ever goes stale, that test reds
	// first.
	//
	// And note that nothing here seals CONSTRUCTION: &DAG{} is legal from outside the
	// package, so the guarantee comes from the built check, NOT from newDAG being
	// unexported — see the construction-forms enumeration on the built field above before
	// concluding this branch is redundant. Neither check subsumes the other.
	//
	// CUR-002 / AUD-001: a nil WorkflowData is caller misuse, not a valid drive. Reject it with a
	// typed ErrValidation up front rather than nil-dereferencing deep in an executor goroutine —
	// which has no recover(), so the panic would take the host process down (SIGSEGV exit 2).
	if data == nil {
		return fmt.Errorf("%w: DAG.Execute requires a non-nil WorkflowData", ErrValidation)
	}
	if !d.built {
		return fmt.Errorf("%w: DAG %q", ErrDAGNotBuilt, d.name)
	}

	// AUD-032: snapshot the execution config ONCE under the read lock, so a concurrent
	// WithExecutionConfig / WithTracerProvider (both take the write lock) cannot race
	// this drive. ExecutionConfig is a pure value type (an int + an interface value), so
	// the copy is a stack copy with no heap allocation — the det-tax hot path is
	// unaffected. Every config read below uses `cfg`, never d.config. mu is non-nil here:
	// the built check just passed, and only newDAG (which sets mu) produces a built DAG.
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()

	// Validate the DAG. It checks structure only — despite what this comment used to say,
	// Validate() computes no StartNodes/EndNodes (measured: zero references in its body).
	if err := d.Validate(); err != nil {
		return err
	}

	// M23 VB-01: project the validated boundary declarations into the run's data, so a
	// snapshot carries what was declared. GATED on the precomputed d.hasBoundaries for
	// the reason hasFanOut is gated below (the det-tax moat): a workflow declaring no
	// boundary must not even make the call — no encode, no Set, no allocation on the
	// universal hot path.
	//
	// INERT (DEC-M23-SEAM-INERT): write-only. Nothing reads this back as policy, and the
	// validated set the predicate and the oracle use comes from the rebuilt DAG, never
	// from the store. See boundary_envelope.go for why that is a commitment rather than
	// an ordering accident.
	//
	// Re-projected on every drive rather than once: the encoding is deterministic in
	// declaration order, so a resume rewrites the same bytes. Idempotent, not a diff.
	if d.hasBoundaries {
		if err := d.projectBoundaries(data); err != nil {
			return err
		}
	}

	// Get the levels for parallel execution (uses already-validated DAG)
	levels := d.GetLevels()

	// Resolve the tracer once (noop when tracing is off) and open the parent
	// workflow span. Per-node spans started in executeNodesInLevel are children
	// of this span because spanCtx flows down. The skipped_count attribute is
	// set just before the span ends, once the final node statuses are known.
	// (DEC-CHUNK5.)
	tracer := resolveTracer(cfg.TracerProvider)
	spanCtx, span := tracer.Start(ctx, workflowSpanName)
	defer func() {
		// Record how many nodes ended Skipped (Skipped nodes get no span of
		// their own — a span implies execution — so the count is surfaced on
		// the parent instead). Computed at span close so it reflects the final
		// status map after any post-halt Skipped sweep.
		span.SetAttributes(attribute.Int(attrWorkflowSkipped, countSkipped(levels, data)))
		span.End()
	}()
	ctx = spanCtx

	// Resolve the per-Execute durable checkpoint callback from ctx. M10-P37 T1
	// (MH37-5a): the callback is ctx-scoped, NOT a shared `d.config` field — each
	// Execute carries its own, so two concurrent drivers of one *Workflow are
	// memory-safe (no shared-field write, no `defer …=nil` racing another run).
	// nil here means no Checkpointer Store was wired (the semantics the park /
	// level-barrier flush sites below depend on).
	checkpoint := checkpointFrom(ctx)
	// M14 ph61: the durability-floor callback (group-commit). nil only for a
	// NON-Syncer store. A Syncer store (incl. a Strict FlatBuffersStore) injects a
	// non-nil callback that the park forces after its checkpoint so a suspend is
	// fsync-durable even under Batched(K); under Strict that call is a cheap no-op
	// (pending is always empty) — the per-park lock is intentional and negligible.
	forceSync := syncFrom(ctx)

	// Suspend chokepoint — the SINGLE enforcement point of the suspend
	// durability invariant: a node is persisted Waiting IFF this Execute returned
	// ErrSuspended (a durable park actually succeeded). On EVERY other exit — nil
	// (run complete), cancellation, fail-fast, checkpoint==nil, a failed flush,
	// and any new suspend/wake exit later chunks add to this function — no node
	// may be left Waiting: a non-suspended run must never persist a stray
	// "suspended" frontier (which a store inspector or a re-entry would misread).
	// Enforcing this once here, by construction, replaces the scattered per-exit
	// resets: two consecutive per-exit misses (AF1 checkpoint==nil, then N2
	// flush-error) proved per-exit is the fragility, and phases 36/37 add
	// timer-fire / signal-deliver re-entry exits that this defer protects
	// automatically. The scan also clears a Waiting that was PRESERVED from a
	// resumed run but never re-reached this run (a path per-exit tracking missed).
	// (DEC review: structural single-point over per-exit; FIND-M10-P35-N2.)
	defer func() {
		if !errors.Is(retErr, ErrSuspended) {
			clearWaiting(data)
		}
	}()

	// Initialize every node to Pending so status is total over the DAG: a node
	// that is never reached (e.g. a run that halts before it, with no failed or
	// skipped dependency) is observably Pending rather than absent from the map.
	// A node already carrying a terminal status from a resumed/persisted run is
	// left as-is (DEC-CHUNK3-status). A persisted non-terminal Waiting node is
	// ALSO left as-is: it is a parked node from a suspended run being resumed, and
	// resetting it to Pending would lose the accounting that the run was Waiting
	// here. (Correctness does not depend on this — executeNodesInLevel re-runs any
	// non-terminal node with resolved deps, so a reset Waiting node would re-run
	// and re-park identically; preserving it keeps the status honest and total.)
	// (M10 / DEC-M10, D-08.)
	for _, level := range levels {
		for _, node := range level {
			if status, ok := data.GetNodeStatus(node.name); !ok || (!isTerminalStatus(status) && status != Waiting) {
				data.SetNodeStatus(node.name, Pending)
			}
		}
	}

	// M21 ph105 (GATED, det-tax fix): inject the per-level concurrency bound onto the ctx a fan-out node's Execute
	// reads its OWN pool bound from — but ONLY when the DAG actually contains a fan-out node (d.hasFanOut,
	// precomputed at AddNode). A workflow with NO fan-out pays ZERO: no per-drive context.WithValue alloc on the
	// universal hot path (the earlier UNCONDITIONAL wrap added +1 alloc/drive on every workflow → breached the
	// det-tax ceiling on amd64; a non-fan-out drive is now genuinely byte-identical to pre-M21). Set ONCE per drive
	// (constant across levels), never per-level.
	levelCtx := ctx
	if d.hasFanOut {
		levelCtx = withMaxConcurrency(ctx, cfg.MaxConcurrency)
	}

	// The engine-reserved current-level key is invariant across the loop (d.name is
	// fixed), so build it ONCE. Built by string concat, not fmt.Sprintf, deliberately:
	// fmt recycles its printer structs through an internal sync.Pool (ppFree) that GC
	// drains on every cycle, so a per-level fmt call pays a spurious +alloc/op under GC
	// pressure that the det-tax ceiling must not see. Concat + strconv touch no pool, so
	// the non-durable drive allocates a GC-independent constant count. (det-tax root cause.)
	currentLevelKey := "__current_level_" + d.name

	// Execute each level in sequence
	for levelIndex, level := range levels {
		// Stop scheduling further levels if the context has been cancelled or
		// timed out. Without this check the executor would keep launching every
		// remaining level even after the caller cancelled — the level barrier
		// only bounds work within a level, not across the loop. Returning the
		// wrapped ctx error surfaces the cancellation to the caller instead of
		// running to completion. (Per-node cancellation within a level is handled
		// by executeNodesInLevel via the level context.)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workflow cancelled before level %d: %w", levelIndex, err)
		}

		// Skip empty levels
		if len(level) == 0 {
			continue
		}

		// Execute nodes in this level. The key is in the engine-reserved namespace
		// (AUD-018: __-prefixed, so a consumer cannot clobber it); written on the
		// executor's unsealed data via setReserved.
		levelName := "Level " + strconv.Itoa(levelIndex)
		data.setReserved(currentLevelKey, levelName)

		// Execute all nodes in this level in parallel, bounded by the configured
		// per-level concurrency limit. Fail-fast (non-continue-on-error)
		// failures are returned; when several fail concurrently they are ALL
		// captured (not just the first). Continue-on-error failures are tolerated
		// and never returned here (observable via node status). levelCtx carries the
		// per-level concurrency bound for a fan-out node's own pool (set once above).
		levelFailures, parkedNodes := executeNodesInLevel(levelCtx, level, data, cfg.MaxConcurrency, tracer)

		// Cancellation ALWAYS wins (DEC-CHUNK6, FORK 1 = a). If the context was
		// cancelled or timed out, return the wrapped ctx error regardless of
		// whether this level also produced fail-fast failures — those failures are
		// incidental to the cancel (a well-behaved action returns ctx.Err() when it
		// observes the cancel, which the executor records as a NodeError) and are
		// DROPPED here so a cancelled run never returns an *ExecutionError. The
		// caller's question "why did the workflow stop?" is answered by the ctx
		// error; any genuine node failure stays observable via GetNodeStatus.
		// Checking here (after the level, before building the ExecutionError)
		// unifies the mid-level cancel path with the between-levels guard above and
		// catches a cancellation in the LAST level even when it was a pure
		// continue-on-error level that produced no fail-fast failure. We do NOT run
		// the Skipped sweep on this path: unreached and downstream nodes stay
		// Pending ("stopped before reaching me", not "an upstream you needed
		// failed"), preserving the chunk-3 distinction (DEC-CHUNK3-status).
		if err := ctx.Err(); err != nil {
			// Cancel outranks park: a cancelled run does not suspend. Any node that
			// parked in this level is reset off Waiting by the suspend chokepoint
			// defer (this return is not ErrSuspended).
			return fmt.Errorf("workflow cancelled during level %d: %w", levelIndex, err)
		}

		// A fail-fast failure halts the workflow: aggregate THIS level's failures
		// (which may be more than one) into a single *ExecutionError and stop
		// scheduling further levels. Before returning, run the Skipped sweep so
		// nodes transitively blocked by the failure are marked Skipped (and
		// independent unreached nodes stay Pending). (DEC-CHUNK3-status.)
		if execErr := newExecutionError(levelFailures); execErr != nil {
			// Fail-fast outranks park: a failing run does not suspend. A parked
			// sibling in this level is reset off Waiting by the suspend chokepoint
			// defer (this return is not ErrSuspended).
			markSkippedFrom(levels, levelIndex+1, data)
			return execErr
		}

		// A park suspends the whole run (Model A, whole-run suspend). Checked
		// AFTER cancellation and fail-fast (both of which outrank a park: a
		// cancelled or failing run does not suspend), and only when this level had
		// no fail-fast failure. The parked node(s) are already Waiting; the level
		// has drained to the barrier. Flush the durable checkpoint FIRST so the
		// park's bytes are down before we return (MH-3 / D-10,
		// durable-flush-before-suspend — "the park IS a checkpoint" only if it is
		// persisted), then return ErrSuspended. Re-entering Execute later (same
		// WorkflowID + Store, via the M9 resume path + graph-identity guard)
		// resumes from here. (M10 / DEC-M10.)
		if len(parkedNodes) > 0 {
			// Two non-suspend exits here (no checkpointer; a failed flush) leave
			// the parked nodes Waiting; the suspend chokepoint defer resets them
			// because neither returns ErrSuspended. Only the successful
			// `return ErrSuspended` below — the durable park actually succeeded —
			// keeps Waiting.
			if checkpoint == nil {
				// No durable checkpoint wired: a park cannot honor
				// durable-flush-before-suspend, so this is a configuration error,
				// never a silently non-durable ErrSuspended. (D-11 / Review AF1.)
				return ErrSuspendRequiresCheckpointer
			}
			if err := checkpoint(data); err != nil {
				// The flush errored — the durable park did NOT succeed, so this is a
				// failure, not a suspend. (FIND-M10-P35-N2.)
				return fmt.Errorf("workflow checkpoint failed while suspending after level %d: %w", levelIndex, err)
			}
			// M14 ph61 durability floor: a park MUST be fsync-durable even under
			// group-commit (D-10/D-11 — "the park IS a checkpoint" only if persisted).
			// Under Batched(K) the checkpoint above may have DEFERRED its fsync; force
			// it now so a crash right after the park still finds it on resume. Strict /
			// non-Syncer stores have forceSync==nil (already durable) → skipped.
			if forceSync != nil {
				if err := forceSync(); err != nil {
					return fmt.Errorf("workflow durability sync failed while suspending after level %d: %w", levelIndex, err)
				}
			}
			// The ONE exit that legitimately keeps Waiting: a durable park happened.
			return ErrSuspended
		}

		// Durable checkpoint at the level barrier (M9 crash-resume). The level
		// completed without cancellation or a fail-fast failure, so every node in
		// it is now terminal in `data`; flushing here persists that progress so a
		// process crash during a LATER level resumes from this point (completed
		// nodes skipped). The callback is wired by Workflow.Execute only when the
		// Store implements Checkpointer; a nil callback (the default) is zero
		// overhead. A checkpoint write failure aborts the run rather than
		// continuing with unrecorded progress that a later crash would silently
		// lose. The cancel and fail-fast return paths above deliberately do NOT
		// checkpoint here — Workflow.Execute performs a final Save on those paths.
		// (DEC-M9, chunk 2.)
		if checkpoint != nil {
			if err := checkpoint(data); err != nil {
				return fmt.Errorf("workflow checkpoint failed after level %d: %w", levelIndex, err)
			}
		}
	}

	return nil
}

// countSkipped returns the number of nodes across all levels whose final status
// is Skipped. It is used only to annotate the parent workflow span
// (workflow.skipped_count); Skipped nodes get no span of their own because a
// span implies execution (DEC-CHUNK5).
func countSkipped(levels [][]*Node, data *WorkflowData) int {
	n := 0
	for _, level := range levels {
		for _, node := range level {
			if status, _ := data.GetNodeStatus(node.name); status == Skipped {
				n++
			}
		}
	}
	return n
}

// clearWaiting resets EVERY node currently Waiting back to Pending. It is the
// reset half of the suspend chokepoint (DAG.Execute's deferred guard): on any
// non-ErrSuspended exit, no node may remain Waiting, so that "a persisted Waiting
// node ⟺ DAG.Execute returned ErrSuspended (a durable park succeeded)" holds by
// construction across every exit — current and future. Scanning all node
// statuses (rather than only the last level's parked set) also clears a Waiting
// that was preserved from a resumed run but never re-reached on this pass.
//
// It collects the Waiting names first and sets them after, because
// forEachNodeStatusLocked holds the read lock while iterating and SetNodeStatus takes
// the write lock (mutating inside the callback would deadlock). (DEC review:
// structural single-point over per-exit; F1 / AF1 / FIND-M10-P35-N2.) It uses the
// non-allocating locked iterator (this runs on every DAG.Execute exit; the public
// snapshot form would breach the det-tax alloc ceiling — see forEachNodeStatusLocked).
func clearWaiting(data *WorkflowData) {
	var waiting []string
	data.forEachNodeStatusLocked(func(name string, status NodeStatus) {
		if status == Waiting {
			waiting = append(waiting, name)
		}
	})
	for _, name := range waiting {
		data.SetNodeStatus(name, Pending)
	}
}

// markSkippedFrom sweeps the levels at index startLevel and beyond in
// topological order, assigning each not-yet-terminal node its CAUSE-AWARE
// terminal status via the shared classifyBlockedStatus predicate — the SAME
// classifier the launch gate uses, so the sweep and the gate cannot drift
// (DEC-M11-STATUS-CAUSE). A node blocked by a non-coe Failed / Skipped ancestor
// becomes Skipped; a node blocked purely by a Bypassed branch interior becomes
// Bypassed; a bypassed node with a surviving taken ancestor becomes Skipped
// (the diamond rule, DEC-M11-P41-DIAMOND). Because the sweep runs in level
// order, a node settled here propagates its cause to its own dependents in later
// levels — transitivity (DEC-CHUNK3-status, S1). A node whose dependencies all
// resolved, or that has only a not-reached-yet (Pending/Running/Waiting)
// ancestor, is left untouched (stays Pending) — it was simply never reached.
func markSkippedFrom(levels [][]*Node, startLevel int, data *WorkflowData) {
	for li := startLevel; li < len(levels); li++ {
		for _, node := range levels[li] {
			if status, _ := data.GetNodeStatus(node.name); isTerminalStatus(status) {
				continue
			}
			if status, assign := classifyBlockedStatus(node, data, dependentRole(node)); assign {
				data.SetNodeStatus(node.name, status)
			}
		}
	}
}
