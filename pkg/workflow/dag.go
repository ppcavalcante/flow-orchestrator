package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DAG represents a Directed Acyclic Graph of workflow nodes.
// It maintains the structure of the workflow and handles dependency resolution.
type DAG struct {
	// Nodes contains all nodes in the DAG, keyed by name
	Nodes map[string]*Node

	// StartNodes are nodes with no dependencies
	StartNodes []*Node

	// EndNodes are nodes with no dependents
	EndNodes []*Node

	// Name is the identifier for this DAG
	Name string

	// CycleNodes stores nodes involved in cycles (if any)
	CycleNodes []string

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

	// mu protects concurrent access to the DAG
	mu sync.RWMutex
}

// NewDAG creates a new DAG with the given name
func NewDAG(name string) *DAG {
	return &DAG{
		Nodes:      make(map[string]*Node),
		StartNodes: make([]*Node, 0, 4), // Pre-allocate with small capacity
		EndNodes:   make([]*Node, 0, 4), // Pre-allocate with small capacity
		Name:       name,
		config:     DefaultConfig(),
	}
}

// NewDAGWithCapacity creates a new DAG with the given name and pre-allocated capacity.
// This can improve performance when the approximate number of nodes is known in advance.
func NewDAGWithCapacity(name string, nodeCapacity int) *DAG {
	return &DAG{
		Nodes:      make(map[string]*Node, nodeCapacity),
		StartNodes: make([]*Node, 0, nodeCapacity/4+1), // Estimate start nodes
		EndNodes:   make([]*Node, 0, nodeCapacity/4+1), // Estimate end nodes
		Name:       name,
		config:     DefaultConfig(),
	}
}

// WithExecutionConfig sets the execution configuration (e.g. per-level
// concurrency) and returns the DAG for chaining.
func (d *DAG) WithExecutionConfig(config ExecutionConfig) *DAG {
	d.config = config
	return d
}

// WithTracerProvider sets the OpenTelemetry trace provider used to emit a span
// per executed node (with a parent span per workflow run) and returns the DAG
// for chaining. Passing nil disables tracing (the zero/default state). This is
// API-only: the host owns the SDK and exporter; the library only emits spans
// through the provided provider (DEC-M6-otel-api-only parity, DEC-CHUNK5).
func (d *DAG) WithTracerProvider(tp trace.TracerProvider) *DAG {
	d.config.TracerProvider = tp
	return d
}

// AddNode adds a node to the DAG.
// Returns an error if a node with the same name already exists.
func (d *DAG) AddNode(node *Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.Nodes[node.Name]; exists {
		return fmt.Errorf("node with name %s already exists", node.Name)
	}

	d.Nodes[node.Name] = node
	// Precompute the fan-out flag (M21 det-tax gate): so DAG.Execute wraps the ctx with the MaxConcurrency seam
	// ONLY for a DAG that actually contains a fan-out node — a non-fan-out workflow pays zero on the hot path.
	if _, ok := node.Action.(*fanOutAction); ok {
		d.hasFanOut = true
	}
	return nil
}

// GetNode retrieves a node by name.
// Returns the node and a boolean indicating if the node exists.
func (d *DAG) GetNode(name string) (*Node, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	node, exists := d.Nodes[name]
	return node, exists
}

// AddDependency creates a dependency between two nodes.
// The toNode will depend on the fromNode, meaning fromNode must complete before toNode can start.
// Returns an error if either node doesn't exist or if adding the dependency would create a cycle.
func (d *DAG) AddDependency(fromNode, toNode string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	from, fromExists := d.Nodes[fromNode]
	to, toExists := d.Nodes[toNode]

	if !fromExists {
		return fmt.Errorf("node %s does not exist", fromNode)
	}

	if !toExists {
		return fmt.Errorf("node %s does not exist", toNode)
	}

	// toNode depends on fromNode (fromNode must complete first)
	to.DependsOn = append(to.DependsOn, from)
	return nil
}

// Validate checks the DAG for validity, including cycle detection.
// Returns an error if the DAG is invalid.
func (d *DAG) Validate() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Reset start and end nodes with capacity hints
	nodesCount := len(d.Nodes)
	startCapacity := cap(d.StartNodes)
	if startCapacity < nodesCount/4+1 {
		startCapacity = nodesCount/4 + 1
	}
	endCapacity := cap(d.EndNodes)
	if endCapacity < nodesCount/4+1 {
		endCapacity = nodesCount/4 + 1
	}

	d.StartNodes = make([]*Node, 0, startCapacity)
	d.EndNodes = make([]*Node, 0, endCapacity)

	// If the DAG is empty, it's valid
	if nodesCount == 0 {
		return nil
	}

	// Check for cycles
	visited := make(map[string]bool, nodesCount)
	inProgress := make(map[string]bool, nodesCount/2+1)
	d.CycleNodes = make([]string, 0, nodesCount/2+1) // Preallocate with reasonable capacity

	// Check each node for cycles
	for name := range d.Nodes {
		if !visited[name] {
			if d.detectCycle(name, visited, inProgress) {
				return fmt.Errorf("cycle detected in graph: %s", strings.Join(d.CycleNodes, " -> "))
			}
		}
	}

	// Build set of nodes that are depended upon
	hasDependents := make(map[string]bool, nodesCount)
	for _, node := range d.Nodes {
		for _, dep := range node.DependsOn {
			hasDependents[dep.Name] = true
		}
	}

	// Identify start and end nodes
	for name, node := range d.Nodes {
		if len(node.DependsOn) == 0 {
			d.StartNodes = append(d.StartNodes, node)
		}
		if !hasDependents[name] {
			d.EndNodes = append(d.EndNodes, node)
		}
	}

	return nil
}

// detectCycle detects cycles in the DAG using DFS
func (d *DAG) detectCycle(nodeName string, visited, inProgress map[string]bool) bool {
	visited[nodeName] = true
	inProgress[nodeName] = true

	node := d.Nodes[nodeName]
	for _, dep := range node.DependsOn {
		if !visited[dep.Name] {
			if d.detectCycle(dep.Name, visited, inProgress) {
				d.CycleNodes = append([]string{nodeName}, d.CycleNodes...)
				return true
			}
		} else if inProgress[dep.Name] {
			// Cycle detected
			d.CycleNodes = append([]string{nodeName, dep.Name}, d.CycleNodes...)
			return true
		}
	}

	inProgress[nodeName] = false
	return false
}

// GetLevels returns the nodes organized into levels for parallel execution.
// Uses O(V+E) algorithm with a reverse adjacency list.
func (d *DAG) GetLevels() [][]*Node {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(d.Nodes) == 0 {
		return nil
	}

	// Build reverse adjacency list: for each node, which nodes depend on it
	dependents := make(map[string][]*Node, len(d.Nodes))
	inDegree := make(map[string]int, len(d.Nodes))
	nodeLevels := make(map[string]int, len(d.Nodes))
	queue := make([]*Node, 0)

	// Initialize and build reverse edges
	for _, node := range d.Nodes {
		inDegree[node.Name] = len(node.DependsOn)
		if len(node.DependsOn) == 0 {
			queue = append(queue, node)
			nodeLevels[node.Name] = 0
		}
		for _, dep := range node.DependsOn {
			dependents[dep.Name] = append(dependents[dep.Name], node)
		}
	}

	// Process nodes level by level using reverse adjacency list
	maxLevel := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		// Process only nodes that depend on this node (O(out-degree))
		for _, dependent := range dependents[node.Name] {
			// Track max level from all parents
			if candidateLevel := nodeLevels[node.Name] + 1; candidateLevel > nodeLevels[dependent.Name] {
				nodeLevels[dependent.Name] = candidateLevel
			}
			inDegree[dependent.Name]--
			if inDegree[dependent.Name] == 0 {
				if nodeLevels[dependent.Name] > maxLevel {
					maxLevel = nodeLevels[dependent.Name]
				}
				queue = append(queue, dependent)
			}
		}
	}

	// Create level slices
	levels := make([][]*Node, maxLevel+1)
	for name, level := range nodeLevels {
		levels[level] = append(levels[level], d.Nodes[name])
	}

	// Sort nodes within each level by name for deterministic ordering
	for i := range levels {
		sort.Slice(levels[i], func(j, k int) bool {
			return levels[i][j].Name < levels[i][k].Name
		})
	}

	return levels
}

// TopologicalSort returns the nodes sorted in topological order for execution
func (d *DAG) TopologicalSort() ([][]*Node, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}

	if len(d.Nodes) == 0 {
		return make([][]*Node, 0), nil
	}

	// Build reverse adjacency list
	dependents := make(map[string][]*Node, len(d.Nodes))
	inDegree := make(map[string]int, len(d.Nodes))
	queue := make([]*Node, 0)
	sorted := make([]*Node, 0, len(d.Nodes))

	for _, node := range d.Nodes {
		inDegree[node.Name] = len(node.DependsOn)
		if len(node.DependsOn) == 0 {
			queue = append(queue, node)
		}
		for _, dep := range node.DependsOn {
			dependents[dep.Name] = append(dependents[dep.Name], node)
		}
	}

	// Process nodes in topological order
	for len(queue) > 0 {
		// Find node with minimum name (for deterministic ordering)
		minIdx := 0
		for i := 1; i < len(queue); i++ {
			if queue[i].Name < queue[minIdx].Name {
				minIdx = i
			}
		}

		// Remove node from queue
		node := queue[minIdx]
		queue = append(queue[:minIdx], queue[minIdx+1:]...)
		sorted = append(sorted, node)

		// Process dependents using reverse adjacency list
		for _, dependent := range dependents[node.Name] {
			inDegree[dependent.Name]--
			if inDegree[dependent.Name] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	return [][]*Node{sorted}, nil
}

// Execute runs the DAG with the provided workflow data.
// Nodes are executed in topological order, with independent nodes potentially running in parallel.
// Returns an error if execution fails.
func (d *DAG) Execute(ctx context.Context, data *WorkflowData) (retErr error) {
	// Validate the DAG (also computes StartNodes/EndNodes)
	if err := d.Validate(); err != nil {
		return err
	}

	// Get the levels for parallel execution (uses already-validated DAG)
	levels := d.GetLevels()

	// Resolve the tracer once (noop when tracing is off) and open the parent
	// workflow span. Per-node spans started in executeNodesInLevel are children
	// of this span because spanCtx flows down. The skipped_count attribute is
	// set just before the span ends, once the final node statuses are known.
	// (DEC-CHUNK5.)
	tracer := resolveTracer(d.config.TracerProvider)
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
			if status, ok := data.GetNodeStatus(node.Name); !ok || (!isTerminalStatus(status) && status != Waiting) {
				data.SetNodeStatus(node.Name, Pending)
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
		levelCtx = withMaxConcurrency(ctx, d.config.MaxConcurrency)
	}

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

		// Execute nodes in this level
		levelName := fmt.Sprintf("Level %d", levelIndex)
		data.Set(fmt.Sprintf("current_level_%s", d.Name), levelName)

		// Execute all nodes in this level in parallel, bounded by the configured
		// per-level concurrency limit. Fail-fast (non-continue-on-error)
		// failures are returned; when several fail concurrently they are ALL
		// captured (not just the first). Continue-on-error failures are tolerated
		// and never returned here (observable via node status). levelCtx carries the
		// per-level concurrency bound for a fan-out node's own pool (set once above).
		levelFailures, parkedNodes := executeNodesInLevel(levelCtx, level, data, d.config.MaxConcurrency, tracer)

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
// countSkipped returns the number of nodes across all levels whose final status
// is Skipped. It is used only to annotate the parent workflow span
// (workflow.skipped_count); Skipped nodes get no span of their own because a
// span implies execution (DEC-CHUNK5).
func countSkipped(levels [][]*Node, data *WorkflowData) int {
	n := 0
	for _, level := range levels {
		for _, node := range level {
			if status, _ := data.GetNodeStatus(node.Name); status == Skipped {
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
// ForEachNodeStatus holds the read lock while iterating and SetNodeStatus takes
// the write lock (mutating inside the callback would deadlock). (DEC review:
// structural single-point over per-exit; F1 / AF1 / FIND-M10-P35-N2.)
func clearWaiting(data *WorkflowData) {
	var waiting []string
	data.ForEachNodeStatus(func(name string, status NodeStatus) {
		if status == Waiting {
			waiting = append(waiting, name)
		}
	})
	for _, name := range waiting {
		data.SetNodeStatus(name, Pending)
	}
}

func markSkippedFrom(levels [][]*Node, startLevel int, data *WorkflowData) {
	for li := startLevel; li < len(levels); li++ {
		for _, node := range levels[li] {
			if status, _ := data.GetNodeStatus(node.Name); isTerminalStatus(status) {
				continue
			}
			if status, assign := classifyBlockedStatus(node, data, dependentRole(node)); assign {
				data.SetNodeStatus(node.Name, status)
			}
		}
	}
}
