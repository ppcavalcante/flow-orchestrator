package workflow

import (
	"context"
	"fmt"
	"time"
)

// NodeBuilder provides a fluent API for configuring workflow nodes.
// It is part of the builder pattern for creating workflows.
type NodeBuilder struct {
	name         string
	action       Action
	dependencies []string
	retryCount   int
	timeout      time.Duration
	workflow     *WorkflowBuilder
}

// WorkflowBuilder provides a fluent API for creating workflow definitions.
// It simplifies the process of defining workflows with dependencies between nodes.
type WorkflowBuilder struct {
	nodes      []*NodeBuilder
	startNodes []string
	workflowID string
	store      WorkflowStore
}

// NewWorkflowBuilder creates a new workflow builder.
func NewWorkflowBuilder() *WorkflowBuilder {
	return &WorkflowBuilder{
		nodes:      make([]*NodeBuilder, 0),
		startNodes: make([]string, 0),
		workflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
	}
}

// WithWorkflowID sets the workflow ID.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithWorkflowID(id string) *WorkflowBuilder {
	b.workflowID = id
	return b
}

// WithStore sets the workflow store for persisting workflow state.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithStore(store WorkflowStore) *WorkflowBuilder {
	b.store = store
	return b
}

// WithStateStore is an alias for WithStore for backward compatibility.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithStateStore(store WorkflowStore) *WorkflowBuilder {
	b.store = store
	return b
}

// AddNode adds a regular node to the workflow and returns a NodeBuilder for
// further configuration.
func (b *WorkflowBuilder) AddNode(name string) *NodeBuilder {
	node := &NodeBuilder{
		name:         name,
		dependencies: make([]string, 0),
		workflow:     b,
	}
	b.nodes = append(b.nodes, node)
	return node
}

// AddStartNode adds a starting node (no dependencies) to the workflow and
// returns a NodeBuilder for further configuration.
func (b *WorkflowBuilder) AddStartNode(name string) *NodeBuilder {
	node := b.AddNode(name)
	b.startNodes = append(b.startNodes, name)
	return node
}

// WithAction sets the action for the node.
// The action can be an Action interface or a function with the signature
// func(ctx context.Context, data *WorkflowData) error.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithAction(action interface{}) *NodeBuilder {
	switch a := action.(type) {
	case Action:
		// Already an Action
		n.action = a
	case func(ctx context.Context, data *WorkflowData) error:
		// Function with new signature
		n.action = ActionFunc(a)
	case func(ctx context.Context, state interface{}) (interface{}, interface{}):
		// Legacy style function, adapt it to new Action
		n.action = ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// This is a simple wrapper that ignores the return values
			// In a real implementation, you might want to handle errors or state deltas
			_, _ = a(ctx, data)
			return nil
		})
	default:
		// Default handler if we can't recognize the type
		// Here we create a no-op action that logs an error
		n.action = ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			return fmt.Errorf("unsupported action type: %T", action)
		})
	}
	return n
}

// WithRetry sets the number of retries for the node.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithRetry(count int) *NodeBuilder {
	return n.WithRetries(count)
}

// DependsOn specifies dependencies for this node by name.
// Returns the builder for method chaining.
func (n *NodeBuilder) DependsOn(deps ...string) *NodeBuilder {
	n.dependencies = append(n.dependencies, deps...)
	return n
}

// WithRetries sets the number of retries for the node.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithRetries(count int) *NodeBuilder {
	n.retryCount = count
	return n
}

// WithTimeout sets a timeout for the node execution.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithTimeout(timeout time.Duration) *NodeBuilder {
	n.timeout = timeout
	return n
}

// Build creates a DAG from the workflow definition.
// Returns an error if the workflow definition is invalid (e.g., has cycles).
func (b *WorkflowBuilder) Build() (*DAG, error) {
	// Create a new DAG with capacity hints based on the number of nodes
	nodeCount := len(b.nodes)
	dag := NewDAGWithCapacity(b.workflowID, nodeCount)

	// Map to track node dependency counts for capacity hints
	nodeDependencyCounts := make(map[string]int, nodeCount)

	// First pass: count dependencies per node
	for _, builder := range b.nodes {
		for _, depName := range builder.dependencies {
			nodeDependencyCounts[depName]++
		}
	}

	// Create real nodes from builders
	for _, builder := range b.nodes {
		if builder.action == nil {
			return nil, fmt.Errorf("node %s has no action defined", builder.name)
		}

		// Use capacity hints for dependencies
		depCapacity := len(builder.dependencies)
		node := NewNodeWithCapacity(builder.name, builder.action, depCapacity)

		if builder.retryCount > 0 {
			node.WithRetries(builder.retryCount)
		}
		if builder.timeout > 0 {
			node.WithTimeout(builder.timeout)
		}

		// Add node to DAG
		if err := dag.AddNode(node); err != nil {
			return nil, fmt.Errorf("failed to add node %s: %w", builder.name, err)
		}
	}

	// Add dependencies
	for _, builder := range b.nodes {
		if len(builder.dependencies) == 0 {
			continue
		}

		node, exists := dag.GetNode(builder.name)
		if !exists {
			return nil, fmt.Errorf("node %s not found", builder.name)
		}

		deps := make([]*Node, 0, len(builder.dependencies))

		// Collect all dependencies first
		for _, depName := range builder.dependencies {
			depNode, exists := dag.GetNode(depName)
			if !exists {
				return nil, fmt.Errorf("dependency %s for node %s not found",
					depName, builder.name)
			}
			deps = append(deps, depNode)
		}

		// Add all dependencies in one operation
		node.AddDependencies(deps...)
	}

	// Validate the DAG
	if err := dag.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	return dag, nil
}
