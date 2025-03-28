package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNode(t *testing.T) {
	t.Run("Node creation", func(t *testing.T) {
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// Simply mark success
			return nil
		})

		node := NewNode("test", action).WithRetries(3)
		if node.Name != "test" {
			t.Errorf("Expected node name 'test', got '%s'", node.Name)
		}
		if node.RetryCount != 3 {
			t.Errorf("Expected retry count 3, got %d", node.RetryCount)
		}
	})

	t.Run("Basic execution", func(t *testing.T) {
		executed := false
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executed = true
			return nil
		})

		node := NewNode("test", action)
		data := NewWorkflowData("test-workflow")

		err := node.Execute(context.Background(), data)
		if err != nil {
			t.Errorf("Node execution failed: %v", err)
		}
		if !executed {
			t.Error("Node action was not executed")
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Completed {
			t.Errorf("Expected status Completed, got %s", status)
		}
	})

	t.Run("Error handling", func(t *testing.T) {
		expectedErr := errors.New("test error")
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return expectedErr
		})

		node := NewNode("test", action)
		data := NewWorkflowData("test-workflow")

		err := node.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected an error, got nil")
		}
		if !errors.Is(err, expectedErr) {
			t.Errorf("Got unexpected error: %v", err)
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Failed {
			t.Errorf("Expected status Failed, got %s", status)
		}
	})

	t.Run("Context cancellation", func(t *testing.T) {
		// Create channel to signal test when action starts and when it detects cancellation
		startedChan := make(chan struct{}, 1)
		cancelledChan := make(chan struct{}, 1)

		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// Signal that we've started
			startedChan <- struct{}{}

			// Wait for either cancellation or timeout
			select {
			case <-ctx.Done():
				// Signal that we detected cancellation
				cancelledChan <- struct{}{}
				return ctx.Err()
			case <-time.After(5 * time.Second): // Should never reach this timeout
				return nil
			}
		})

		node := NewNode("test", action)
		data := NewWorkflowData("test-workflow")

		// Create a context we can cancel
		ctx, cancel := context.WithCancel(context.Background())

		// Execute in a goroutine
		go func() {
			<-startedChan                      // Wait for the action to start
			time.Sleep(100 * time.Millisecond) // Small delay to ensure the action is in the select
			cancel()                           // Cancel the context
		}()

		err := node.Execute(ctx, data)

		// Wait for cancellation to be detected or timeout
		select {
		case <-cancelledChan:
			// Good, we detected cancellation
		case <-time.After(1 * time.Second):
			t.Error("Cancellation was not detected")
		}

		if err == nil {
			t.Error("Expected a context cancellation error, got nil")
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Failed {
			t.Errorf("Expected status Failed, got %s", status)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		})

		node := NewNode("test", action).WithTimeout(100 * time.Millisecond)
		data := NewWorkflowData("test-workflow")

		err := node.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected a timeout error, got nil")
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Failed {
			t.Errorf("Expected status Failed, got %s", status)
		}
	})

	t.Run("Retries", func(t *testing.T) {
		attempts := 0
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			attempts++
			if attempts < 3 {
				return errors.New("temporary failure")
			}
			return nil
		})

		node := NewNode("test", action).WithRetries(3)
		data := NewWorkflowData("test-workflow")

		err := node.Execute(context.Background(), data)
		if err != nil {
			t.Errorf("Node execution failed after retries: %v", err)
		}
		if attempts != 3 {
			t.Errorf("Expected 3 attempts, got %d", attempts)
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Completed {
			t.Errorf("Expected status Completed, got %s", status)
		}
	})

	t.Run("Max retries exceeded", func(t *testing.T) {
		attempts := 0
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			attempts++
			return errors.New("persistent failure")
		})

		node := NewNode("test", action).WithRetries(2)
		data := NewWorkflowData("test-workflow")

		err := node.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected an error after max retries, got nil")
		}
		if attempts != 3 { // Original attempt + 2 retries
			t.Errorf("Expected 3 attempts, got %d", attempts)
		}

		status, exists := data.GetNodeStatus("test")
		if !exists {
			t.Error("Node status not found")
			return
		}
		if status != Failed {
			t.Errorf("Expected status Failed, got %s", status)
		}
	})

	t.Run("Dependency check", func(t *testing.T) {
		executionOrder := make([]string, 0)

		action1 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executionOrder = append(executionOrder, "node1")
			return nil
		})

		action2 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executionOrder = append(executionOrder, "node2")
			return nil
		})

		node1 := NewNode("node1", action1)
		node2 := NewNode("node2", action2).WithDependencies(node1)

		// Create and setup a DAG
		dag := NewDAG("test-dependencies")

		// Add nodes to the DAG
		_ = dag.AddNode(node1)
		_ = dag.AddNode(node2)

		// Add dependencies
		_ = dag.AddDependency("node1", "node2")

		data := NewWorkflowData("test-workflow")

		err := dag.Execute(context.Background(), data)
		if err != nil {
			t.Errorf("DAG execution failed: %v", err)
		}

		if len(executionOrder) != 2 {
			t.Errorf("Expected 2 executions, got %d", len(executionOrder))
		}

		if len(executionOrder) == 2 && executionOrder[0] != "node1" {
			t.Errorf("Expected node1 to execute first, got %s", executionOrder[0])
		}

		if len(executionOrder) == 2 && executionOrder[1] != "node2" {
			t.Errorf("Expected node2 to execute second, got %s", executionOrder[1])
		}
	})
}

func TestNodeDependencies(t *testing.T) {
	// Create test nodes
	node1 := NewNode("node1", nil)
	dep1 := NewNode("dep1", nil)
	dep2 := NewNode("dep2", nil)
	dep3 := NewNode("dep3", nil)

	// Test AddDependency
	node1.AddDependency(dep1)
	assert.True(t, node1.HasDependency("dep1"))
	assert.False(t, node1.HasDependency("dep2"))

	// Test AddDependencies
	node1.AddDependencies(dep2, dep3)
	assert.True(t, node1.HasDependency("dep2"))
	assert.True(t, node1.HasDependency("dep3"))

	// Test GetDependencies
	deps := node1.GetDependencies()
	assert.Len(t, deps, 3)
	assert.Equal(t, "dep1", deps[0].Name)
	assert.Equal(t, "dep2", deps[1].Name)
	assert.Equal(t, "dep3", deps[2].Name)

	// Test empty dependencies
	node2 := NewNode("node2", nil)
	assert.Empty(t, node2.GetDependencies())
	assert.False(t, node2.HasDependency("any"))
}

func TestNodeWithCapacity(t *testing.T) {
	node := NewNodeWithCapacity("test", nil, 5)
	assert.NotNil(t, node)
	assert.Equal(t, "test", node.Name)

	// Add dependencies up to capacity
	deps := make([]*Node, 5)
	for i := 0; i < 5; i++ {
		deps[i] = NewNode(fmt.Sprintf("dep%d", i), nil)
		node.AddDependency(deps[i])
	}

	nodeDeps := node.GetDependencies()
	assert.Len(t, nodeDeps, 5)
	for i := 0; i < 5; i++ {
		assert.Equal(t, fmt.Sprintf("dep%d", i), nodeDeps[i].Name)
	}
}

func TestNodeConfiguration(t *testing.T) {
	node := NewNode("test", nil)

	// Test WithRetries
	retryNode := node.WithRetries(3)
	assert.Equal(t, node, retryNode)
	assert.Equal(t, 3, node.RetryCount)

	// Test WithTimeout
	timeout := 5 * time.Second
	timeoutNode := node.WithTimeout(timeout)
	assert.Equal(t, node, timeoutNode)
	assert.Equal(t, timeout, node.Timeout)

	// Test WithDependencies
	dep1 := NewNode("dep1", nil)
	dep2 := NewNode("dep2", nil)
	depsNode := node.WithDependencies(dep1, dep2)
	assert.Equal(t, node, depsNode)
	assert.True(t, node.HasDependency("dep1"))
	assert.True(t, node.HasDependency("dep2"))
}

func TestAddDependencies(t *testing.T) {
	t.Run("Add multiple dependencies", func(t *testing.T) {
		// Create test nodes
		node := NewNode("main", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}))
		dep1 := NewNode("dep1", nil)
		dep2 := NewNode("dep2", nil)
		dep3 := NewNode("dep3", nil)

		// Add multiple dependencies at once
		node.AddDependencies(dep1, dep2, dep3)

		// Verify all dependencies were added
		if len(node.DependsOn) != 3 {
			t.Errorf("Expected 3 dependencies, got %d", len(node.DependsOn))
		}

		// Verify each dependency
		expectedDeps := map[string]bool{
			"dep1": false,
			"dep2": false,
			"dep3": false,
		}

		for _, dep := range node.DependsOn {
			if _, exists := expectedDeps[dep.Name]; !exists {
				t.Errorf("Unexpected dependency: %s", dep.Name)
			}
			expectedDeps[dep.Name] = true
		}

		// Verify all expected dependencies were found
		for name, found := range expectedDeps {
			if !found {
				t.Errorf("Dependency %s was not added", name)
			}
		}
	})

	t.Run("Add empty dependencies", func(t *testing.T) {
		// Create test node
		node := NewNode("main", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}))

		// Initial state
		initialCap := cap(node.DependsOn)

		// Add empty dependencies
		node.AddDependencies()

		// Verify nothing changed
		if len(node.DependsOn) != 0 {
			t.Error("Expected no dependencies to be added")
		}
		if cap(node.DependsOn) != initialCap {
			t.Error("Capacity should not change when adding no dependencies")
		}
	})

	t.Run("Add dependencies incrementally", func(t *testing.T) {
		// Create test nodes
		node := NewNode("main", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}))
		dep1 := NewNode("dep1", nil)
		dep2 := NewNode("dep2", nil)

		// Add first dependency
		node.AddDependencies(dep1)
		if len(node.DependsOn) != 1 {
			t.Errorf("Expected 1 dependency, got %d", len(node.DependsOn))
		}

		// Record capacity after first add
		firstCap := cap(node.DependsOn)

		// Add second dependency
		node.AddDependencies(dep2)
		if len(node.DependsOn) != 2 {
			t.Errorf("Expected 2 dependencies, got %d", len(node.DependsOn))
		}

		// Verify capacity was optimized
		if cap(node.DependsOn) < 2 {
			t.Error("Capacity should be at least 2")
		}
		if cap(node.DependsOn) < firstCap {
			t.Error("Capacity should not decrease")
		}
	})

	t.Run("Add duplicate dependencies", func(t *testing.T) {
		// Create test nodes
		node := NewNode("main", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}))
		dep1 := NewNode("dep1", nil)

		// Add same dependency twice
		node.AddDependencies(dep1)
		node.AddDependencies(dep1)

		// Verify dependency was added twice (current behavior)
		if len(node.DependsOn) != 2 {
			t.Errorf("Expected 2 dependencies (duplicate allowed), got %d", len(node.DependsOn))
		}

		// Verify both entries point to the same node
		if node.DependsOn[0] != dep1 || node.DependsOn[1] != dep1 {
			t.Error("Dependencies should point to the same node")
		}
	})

	t.Run("Add nil dependencies", func(t *testing.T) {
		// Create test node
		node := NewNode("main", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}))
		dep1 := NewNode("dep1", nil)

		// Add mix of nil and valid dependencies
		node.AddDependencies(dep1, nil, dep1)

		// Verify nil dependencies are added (current behavior)
		if len(node.DependsOn) != 3 {
			t.Errorf("Expected 3 dependencies (including nil), got %d", len(node.DependsOn))
		}

		// Count nil dependencies
		nilCount := 0
		for _, dep := range node.DependsOn {
			if dep == nil {
				nilCount++
			}
		}
		if nilCount != 1 {
			t.Errorf("Expected 1 nil dependency, got %d", nilCount)
		}
	})

	t.Run("Add dependencies with pre-allocated capacity", func(t *testing.T) {
		// Create test node with pre-allocated capacity
		node := NewNodeWithCapacity("main", nil, 5)
		initialCap := cap(node.DependsOn)

		// Create test dependencies
		deps := make([]*Node, 3)
		for i := range deps {
			deps[i] = NewNode(fmt.Sprintf("dep%d", i), nil)
		}

		// Add dependencies
		node.AddDependencies(deps...)

		// Verify capacity was preserved
		if cap(node.DependsOn) != initialCap {
			t.Errorf("Expected capacity to remain %d, got %d", initialCap, cap(node.DependsOn))
		}

		// Verify all dependencies were added
		if len(node.DependsOn) != 3 {
			t.Errorf("Expected 3 dependencies, got %d", len(node.DependsOn))
		}
	})

	t.Run("Add dependencies exceeding pre-allocated capacity", func(t *testing.T) {
		// Create test node with small pre-allocated capacity
		node := NewNodeWithCapacity("main", nil, 2)
		initialCap := cap(node.DependsOn)

		// Create more dependencies than initial capacity
		deps := make([]*Node, 4)
		for i := range deps {
			deps[i] = NewNode(fmt.Sprintf("dep%d", i), nil)
		}

		// Add dependencies
		node.AddDependencies(deps...)

		// Verify capacity was increased
		if cap(node.DependsOn) <= initialCap {
			t.Error("Expected capacity to increase")
		}

		// Verify all dependencies were added
		if len(node.DependsOn) != 4 {
			t.Errorf("Expected 4 dependencies, got %d", len(node.DependsOn))
		}
	})

	t.Run("Add dependencies with zero initial capacity", func(t *testing.T) {
		// Create test node with zero capacity
		node := NewNodeWithCapacity("main", nil, 0)

		// Create test dependencies
		deps := make([]*Node, 3)
		for i := range deps {
			deps[i] = NewNode(fmt.Sprintf("dep%d", i), nil)
		}

		// Add dependencies
		node.AddDependencies(deps...)

		// Verify capacity was allocated
		if cap(node.DependsOn) < len(deps) {
			t.Errorf("Expected capacity of at least %d, got %d", len(deps), cap(node.DependsOn))
		}

		// Verify all dependencies were added
		if len(node.DependsOn) != 3 {
			t.Errorf("Expected 3 dependencies, got %d", len(node.DependsOn))
		}
	})

	t.Run("Add dependencies in multiple batches", func(t *testing.T) {
		// Create test node
		node := NewNode("main", nil)

		// Create test dependencies
		batch1 := []*Node{
			NewNode("dep1", nil),
			NewNode("dep2", nil),
		}
		batch2 := []*Node{
			NewNode("dep3", nil),
			NewNode("dep4", nil),
		}

		// Add first batch
		node.AddDependencies(batch1...)
		firstBatchCap := cap(node.DependsOn)

		// Add second batch
		node.AddDependencies(batch2...)

		// Verify final state
		if len(node.DependsOn) != 4 {
			t.Errorf("Expected 4 dependencies, got %d", len(node.DependsOn))
		}

		// Verify capacity growth
		if cap(node.DependsOn) < firstBatchCap {
			t.Error("Capacity should not decrease")
		}

		// Verify order of dependencies
		expectedNames := []string{"dep1", "dep2", "dep3", "dep4"}
		for i, name := range expectedNames {
			if node.DependsOn[i].Name != name {
				t.Errorf("Expected dependency %d to be %s, got %s", i, name, node.DependsOn[i].Name)
			}
		}
	})

	t.Run("Add dependencies with mixed node states", func(t *testing.T) {
		// Create test node
		node := NewNode("main", nil)

		// Create dependencies with different states
		dep1 := NewNode("dep1", nil).WithRetries(3)
		dep2 := NewNode("dep2", nil).WithTimeout(time.Second)
		dep3 := NewNode("dep3", nil).WithRetries(1).WithTimeout(time.Minute)

		// Add dependencies
		node.AddDependencies(dep1, dep2, dep3)

		// Verify dependencies were added with their states intact
		if node.DependsOn[0].RetryCount != 3 {
			t.Error("Retry count not preserved for dep1")
		}
		if node.DependsOn[1].Timeout != time.Second {
			t.Error("Timeout not preserved for dep2")
		}
		if node.DependsOn[2].RetryCount != 1 || node.DependsOn[2].Timeout != time.Minute {
			t.Error("Configuration not preserved for dep3")
		}

		// Verify dependency references are correct
		for _, dep := range node.DependsOn {
			if dep == nil {
				t.Error("Unexpected nil dependency")
			}
		}
	})
}
