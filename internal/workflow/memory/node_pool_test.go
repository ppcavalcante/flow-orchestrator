package memory

import (
	"context"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestNodePoolBasic tests basic node pool functionality
func TestNodePoolBasic(t *testing.T) {
	// Create a pool
	pool := NewNodePool(10)

	// Get a node from the pool
	node := pool.Get()
	if node == nil {
		t.Fatal("Expected non-nil node from pool")
	}

	// Configure the node
	node.Name = "test-node"
	node.RetryCount = 3
	node.Timeout = 5 * time.Second

	// Return the node to the pool
	pool.Put(node)

	// Get another node from the pool (should be the same one, reset)
	node2 := pool.Get()
	if node2 == nil {
		t.Fatal("Expected non-nil node from pool")
	}

	// Verify the node was reset
	if node2.Name != "" {
		t.Errorf("Expected empty name, got %q", node2.Name)
	}
	if node2.RetryCount != 0 {
		t.Errorf("Expected retry count 0, got %d", node2.RetryCount)
	}
	if node2.Timeout != 0 {
		t.Errorf("Expected timeout 0, got %v", node2.Timeout)
	}

	// Check stats
	gets, puts, _, capacity := pool.GetStats()
	if gets != 2 {
		t.Errorf("Expected 2 gets, got %d", gets)
	}
	if puts != 1 {
		t.Errorf("Expected 1 put, got %d", puts)
	}
	if capacity != 10 {
		t.Errorf("Expected capacity 10, got %d", capacity)
	}
}

// TestNodePoolMultiple tests getting multiple nodes from the pool
func TestNodePoolMultiple(t *testing.T) {
	// Create a pool
	pool := NewNodePool(5)

	// Get multiple nodes
	nodes := make([]*workflow.Node, 10)
	for i := 0; i < 10; i++ {
		nodes[i] = pool.Get()
		if nodes[i] == nil {
			t.Fatalf("Expected non-nil node at index %d", i)
		}

		// Configure the node
		nodes[i].Name = "node-" + string(rune('A'+i))
	}

	// Verify all nodes are distinct
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			if nodes[i] == nodes[j] {
				t.Errorf("Nodes at indices %d and %d are the same instance", i, j)
			}
		}
	}

	// Return nodes to the pool
	for i := 0; i < len(nodes); i++ {
		pool.Put(nodes[i])
	}

	// Check stats
	gets, puts, _, _ := pool.GetStats()
	if gets != 10 {
		t.Errorf("Expected 10 gets, got %d", gets)
	}
	if puts != 10 {
		t.Errorf("Expected 10 puts, got %d", puts)
	}
}

// TestCreateNode tests the CreateNode helper function
func TestCreateNode(t *testing.T) {
	// Create a node
	action := &testAction{}
	node := CreateNode("test-node", action)

	// Verify the node
	if node == nil {
		t.Fatal("Expected non-nil node")
	}
	if node.Name != "test-node" {
		t.Errorf("Expected name 'test-node', got %q", node.Name)
	}

	// Clean up
	PutNode(node)
}

// TestCreateNodeWithDependencies tests the CreateNodeWithDependencies helper function
func TestCreateNodeWithDependencies(t *testing.T) {
	// Create parent nodes
	parent1 := CreateNode("parent1", &testAction{})
	parent2 := CreateNode("parent2", &testAction{})

	// Create child node with dependencies
	child := CreateNodeWithDependencies("child", &testAction{}, parent1, parent2)

	// Verify the child node
	if child == nil {
		t.Fatal("Expected non-nil child node")
	}
	if child.Name != "child" {
		t.Errorf("Expected name 'child', got %q", child.Name)
	}
	if len(child.DependsOn) != 2 {
		t.Errorf("Expected 2 dependencies, got %d", len(child.DependsOn))
	}
	if child.DependsOn[0] != parent1 || child.DependsOn[1] != parent2 {
		t.Error("Dependency mismatch")
	}

	// Clean up
	PutNode(parent1)
	PutNode(parent2)
	PutNode(child)
}

// TestCreateNodeWithRetries tests the CreateNodeWithRetries helper function
func TestCreateNodeWithRetries(t *testing.T) {
	// Create a node with retries
	node := CreateNodeWithRetries("test-node", &testAction{}, 3)

	// Verify the node
	if node == nil {
		t.Fatal("Expected non-nil node")
	}
	if node.Name != "test-node" {
		t.Errorf("Expected name 'test-node', got %q", node.Name)
	}
	if node.RetryCount != 3 {
		t.Errorf("Expected retry count 3, got %d", node.RetryCount)
	}

	// Clean up
	PutNode(node)
}

// TestCreateNodeWithTimeout tests the CreateNodeWithTimeout helper function
func TestCreateNodeWithTimeout(t *testing.T) {
	// Create a node with timeout
	timeout := 5 * time.Second
	node := CreateNodeWithTimeout("test-node", &testAction{}, timeout)

	// Verify the node
	if node == nil {
		t.Fatal("Expected non-nil node")
	}
	if node.Name != "test-node" {
		t.Errorf("Expected name 'test-node', got %q", node.Name)
	}
	if node.Timeout != timeout {
		t.Errorf("Expected timeout %v, got %v", timeout, node.Timeout)
	}

	// Clean up
	PutNode(node)
}

// testAction is a simple action implementation for testing
type testAction struct{}

func (a *testAction) Execute(ctx context.Context, data *workflow.WorkflowData) error {
	return nil
}
