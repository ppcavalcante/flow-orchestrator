package workflow

import (
	"context"
	"fmt"
	"time"
)

// NodeStatus represents the execution status of a node
type NodeStatus string

const (
	// Pending is the initial state: the node has not started execution. Every
	// node in a DAG is set to Pending when Execute begins, so a node that is
	// never reached (e.g. a run that halted before it, with no failed/skipped
	// dependency of its own) remains observably Pending rather than absent.
	Pending NodeStatus = "pending"
	// Running indicates the node is currently executing
	Running NodeStatus = "running"
	// Completed indicates the node has completed successfully
	Completed NodeStatus = "completed"
	// Failed indicates the node's action returned an error
	Failed NodeStatus = "failed"
	// Skipped indicates the node did not run because at least one dependency was
	// in a terminal non-resolving state — a non-continue-on-error dependency
	// that Failed, or a dependency that was itself Skipped. Skipped is
	// transitive and is NOT a failure (it never appears in ExecutionError).
	Skipped NodeStatus = "skipped"
)

// Node represents a unit of work in a workflow.
// Each node has an action that will be executed when the node is processed,
// and can have dependencies on other nodes.
type Node struct {
	// Name uniquely identifies the node within a workflow
	Name string

	// Action is the executable work for this node
	Action Action

	// DependsOn contains nodes that must complete before this node can execute
	DependsOn []*Node

	// RetryCount specifies how many times to retry the action on failure
	RetryCount int

	// Timeout specifies the maximum execution time for the node
	Timeout time.Duration

	// ContinueOnError, when true, changes how a failure of this node is
	// handled by the executor: instead of failing the workflow (the default
	// fail-fast behavior), the node is marked Failed, its siblings and the
	// rest of the DAG continue, and dependents may inspect this node's Failed
	// status and branch on it. Default false preserves fail-fast.
	ContinueOnError bool
}

// NewNode creates a new node with the given name and action
func NewNode(name string, action Action) *Node {
	return &Node{
		Name:       name,
		Action:     action,
		DependsOn:  make([]*Node, 0, 4), // Pre-allocate with small capacity
		RetryCount: 0,
	}
}

// NewNodeWithCapacity creates a new node with capacity hints to reduce allocations
// This is useful when the approximate number of dependencies is known in advance.
func NewNodeWithCapacity(name string, action Action, dependencyCapacity int) *Node {
	return &Node{
		Name:       name,
		Action:     action,
		DependsOn:  make([]*Node, 0, dependencyCapacity),
		RetryCount: 0,
	}
}

// AddDependency adds a dependency to this node
// The node will only execute after the dependency has completed successfully.
func (n *Node) AddDependency(dep *Node) {
	n.DependsOn = append(n.DependsOn, dep)
}

// AddDependencies adds multiple dependencies to this node in one operation
// This is more efficient than adding dependencies one by one.
func (n *Node) AddDependencies(deps ...*Node) {
	if len(deps) == 0 {
		return
	}

	// Pre-grow the slice if needed to avoid multiple allocations
	currentCap := cap(n.DependsOn)
	currentLen := len(n.DependsOn)
	neededCap := currentLen + len(deps)

	if currentCap < neededCap {
		// Create a new slice with sufficient capacity
		newDeps := make([]*Node, currentLen, neededCap)
		copy(newDeps, n.DependsOn)
		n.DependsOn = newDeps
	}

	// Add all dependencies
	n.DependsOn = append(n.DependsOn, deps...)
}

// Execute runs the node action with retries and timeout
// Updates the node status in the workflow data.
// Returns an error if execution fails.
func (n *Node) Execute(ctx context.Context, data *WorkflowData) error {
	// Mark node as running
	data.SetNodeStatus(n.Name, Running)

	// Create timeout context if needed
	var execCtx context.Context
	var cancel context.CancelFunc

	if n.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, n.Timeout)
		defer cancel()
	} else {
		execCtx = ctx
	}

	// Execute with retries
	var err error
	if n.RetryCount > 0 {
		retryAction := NewRetryableAction(n.Action, n.RetryCount, time.Second)
		err = retryAction.Execute(execCtx, data)
	} else {
		err = n.Action.Execute(execCtx, data)
	}

	// Update node status based on result
	if err != nil {
		data.SetNodeStatus(n.Name, Failed)
		return fmt.Errorf("node %s execution failed: %w", n.Name, err)
	}

	data.SetNodeStatus(n.Name, Completed)
	return nil
}

// GetDependencies returns the dependencies of this node
func (n *Node) GetDependencies() []*Node {
	return n.DependsOn
}

// WithRetries configures the node to retry on failure
// Returns the node for method chaining.
func (n *Node) WithRetries(count int) *Node {
	n.RetryCount = count
	return n
}

// WithTimeout sets a timeout for node execution
// Returns the node for method chaining.
func (n *Node) WithTimeout(timeout time.Duration) *Node {
	n.Timeout = timeout
	return n
}

// WithDependencies sets the dependencies of this node
func (n *Node) WithDependencies(deps ...*Node) *Node {
	n.DependsOn = deps
	return n
}

// WithContinueOnError marks the node so that a failure does not fail the
// workflow: the node is recorded as Failed and the rest of the DAG continues.
// Returns the node for method chaining.
func (n *Node) WithContinueOnError() *Node {
	n.ContinueOnError = true
	return n
}

// HasDependency checks if this node depends on the given node
func (n *Node) HasDependency(nodeName string) bool {
	for _, dep := range n.DependsOn {
		if dep.Name == nodeName {
			return true
		}
	}
	return false
}
