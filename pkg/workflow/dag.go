package workflow

import (
	"context"
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
func (d *DAG) Execute(ctx context.Context, data *WorkflowData) error {
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

	// Initialize every node to Pending so status is total over the DAG: a node
	// that is never reached (e.g. a run that halts before it, with no failed or
	// skipped dependency) is observably Pending rather than absent from the map.
	// A node already carrying a terminal status from a resumed/persisted run is
	// left as-is. (DEC-CHUNK3-status.)
	for _, level := range levels {
		for _, node := range level {
			if status, ok := data.GetNodeStatus(node.Name); !ok || !isTerminalStatus(status) {
				data.SetNodeStatus(node.Name, Pending)
			}
		}
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
		// and never returned here (observable via node status).
		levelFailures := executeNodesInLevel(ctx, level, data, d.config.MaxConcurrency, tracer)

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
			return fmt.Errorf("workflow cancelled during level %d: %w", levelIndex, err)
		}

		// A fail-fast failure halts the workflow: aggregate THIS level's failures
		// (which may be more than one) into a single *ExecutionError and stop
		// scheduling further levels. Before returning, run the Skipped sweep so
		// nodes transitively blocked by the failure are marked Skipped (and
		// independent unreached nodes stay Pending). (DEC-CHUNK3-status.)
		if execErr := newExecutionError(levelFailures); execErr != nil {
			markSkippedFrom(levels, levelIndex+1, data)
			return execErr
		}
	}

	return nil
}

// markSkippedFrom sweeps the levels at index startLevel and beyond in
// topological order, marking Skipped every not-yet-terminal node that has at
// least one dependency in a terminal non-resolving state (a non-coe Failed dep,
// or an already-Skipped dep). Because the sweep runs in level order, a node
// marked Skipped here causes its own dependents (in later levels) to be marked
// Skipped too — transitivity (DEC-CHUNK3-status, S1). A node whose dependencies
// all resolved, or that has no failed/skipped ancestor, is left untouched
// (stays Pending) — it was simply never reached.
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

func markSkippedFrom(levels [][]*Node, startLevel int, data *WorkflowData) {
	for li := startLevel; li < len(levels); li++ {
		for _, node := range levels[li] {
			if status, _ := data.GetNodeStatus(node.Name); isTerminalStatus(status) {
				continue
			}
			for _, dep := range node.DependsOn {
				depStatus, _ := data.GetNodeStatus(dep.Name)
				if depResolved(dep, depStatus) {
					continue
				}
				if isSkipCause(depStatus) {
					data.SetNodeStatus(node.Name, Skipped)
					break
				}
			}
		}
	}
}
