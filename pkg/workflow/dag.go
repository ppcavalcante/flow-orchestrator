package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
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

	// Execute each level in sequence
	for levelIndex, level := range levels {
		// Skip empty levels
		if len(level) == 0 {
			continue
		}

		// Execute nodes in this level
		levelName := fmt.Sprintf("Level %d", levelIndex)
		data.Set(fmt.Sprintf("current_level_%s", d.Name), levelName)

		// Execute all nodes in this level in parallel, bounded by the
		// configured per-level concurrency limit.
		if err := executeNodesInLevel(ctx, level, data, d.config.MaxConcurrency); err != nil {
			return fmt.Errorf("error executing level %d: %w", levelIndex, err)
		}
	}

	return nil
}

// GetNodeByName returns a node by name
func (d *DAG) GetNodeByName(name string) (*Node, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	node, exists := d.Nodes[name]
	return node, exists
}
