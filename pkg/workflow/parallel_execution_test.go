package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParallelExecution(t *testing.T) {
	t.Run("DefaultExecutionConfig", func(t *testing.T) {
		config := DefaultConfig()
		if config.MaxConcurrency != 4 {
			t.Errorf("Expected default MaxConcurrency to be 4, got %d", config.MaxConcurrency)
		}
		if !config.PreserveOrder {
			t.Error("Expected default PreserveOrder to be true")
		}
	})

	t.Run("ParallelExecutionOfIndependentNodes", func(t *testing.T) {
		// Create a DAG
		dag := NewDAG("test-dag")

		// Track execution order
		var mu sync.Mutex
		executionTimes := make([]time.Time, 0)
		nodeNames := make([]string, 0)

		// Create nodes with different sleep durations
		node1 := NewNode("slow", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			executionTimes = append(executionTimes, time.Now())
			nodeNames = append(nodeNames, "slow")
			mu.Unlock()
			return nil
		}))

		node2 := NewNode("fast", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			executionTimes = append(executionTimes, time.Now())
			nodeNames = append(nodeNames, "fast")
			mu.Unlock()
			return nil
		}))

		// Add nodes to the DAG
		dag.AddNode(node1)
		dag.AddNode(node2)

		// Create workflow data
		data := NewWorkflowData("test-dag")

		// Execute the DAG
		err := dag.Execute(context.Background(), data)
		if err != nil {
			t.Errorf("DAG execution failed: %v", err)
		}

		// Check that both nodes executed
		if len(executionTimes) != 2 {
			t.Errorf("Expected 2 node executions, got %d", len(executionTimes))
		}

		// Check node statuses - they should all be completed
		if status, exists := data.GetNodeStatus("slow"); !exists {
			t.Errorf("Slow node status not found")
		} else if status != Completed {
			t.Errorf("Expected slow node status to be Completed, got %v", status)
		}
		if status, exists := data.GetNodeStatus("fast"); !exists {
			t.Errorf("Fast node status not found")
		} else if status != Completed {
			t.Errorf("Expected fast node status to be Completed, got %v", status)
		}

		// Note: The order of execution might vary depending on the implementation,
		// so we're simply checking that all nodes have executed successfully
	})

	t.Run("NodeFailureInParallelExecution", func(t *testing.T) {
		// Create a DAG
		dag := NewDAG("test-fail")

		// Create a failing node
		failingNode := NewNode("failing", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return fmt.Errorf("intentional failure")
		}))

		// Add node to the DAG
		dag.AddNode(failingNode)

		// Create workflow data
		data := NewWorkflowData("test-fail")

		// Execute the DAG
		err := dag.Execute(context.Background(), data)
		if err == nil {
			t.Errorf("Expected DAG execution to fail")
		}
	})

	t.Run("ContextCancellationInParallelExecution", func(t *testing.T) {
		// Create a context with cancellation
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create a DAG
		dag := NewDAG("test-cancel")

		// Create a node that checks for cancellation
		node := NewNode("long-running", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}))

		// Add node to the DAG
		dag.AddNode(node)

		// Create workflow data
		data := NewWorkflowData("test-cancel")

		// Cancel the context after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		// Execute the DAG
		err := dag.Execute(ctx, data)

		// We expect an error that contains "context canceled"
		if err == nil {
			t.Errorf("Expected execution to fail due to context cancellation")
		} else if !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("Expected error to contain 'context canceled', got: %v", err)
		}
	})
}

type testAction struct {
	executed bool
	delay    time.Duration
	err      error
}

func (a *testAction) Execute(ctx context.Context, data *WorkflowData) error {
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
			a.executed = true
			return a.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.executed = true
	return a.err
}

func TestParallelExecutionWithDefaultConfig(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes
	action1 := &testAction{}
	action2 := &testAction{delay: 100 * time.Millisecond}
	action3 := &testAction{}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)
	node3 := NewNode("node3", action3)

	// Create executor with default config
	executor := NewParallelNodeExecutor(DefaultConfig())

	// Test parallel execution
	nodes := []*Node{node1, node2, node3}
	err := executor.ExecuteNodes(context.Background(), nodes, data)
	require.NoError(t, err)

	// Verify all nodes were executed
	assert.True(t, action1.executed)
	assert.True(t, action2.executed)
	assert.True(t, action3.executed)

	// Verify node statuses
	status1, _ := data.GetNodeStatus("node1")
	status2, _ := data.GetNodeStatus("node2")
	status3, _ := data.GetNodeStatus("node3")

	assert.Equal(t, Completed, status1)
	assert.Equal(t, Completed, status2)
	assert.Equal(t, Completed, status3)
}

func TestParallelExecutionWithError(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes with one failing node
	action1 := &testAction{}
	action2 := &testAction{err: assert.AnError}
	action3 := &testAction{}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)
	node3 := NewNode("node3", action3)

	// Create executor with default config
	executor := NewParallelNodeExecutor(DefaultConfig())

	// Test parallel execution
	nodes := []*Node{node1, node2, node3}
	err := executor.ExecuteNodes(context.Background(), nodes, data)
	require.Error(t, err)

	// Verify node statuses
	status2, _ := data.GetNodeStatus("node2")
	assert.Equal(t, Failed, status2)
}

func TestParallelExecutionWithContext(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes with delays longer than the timeout
	action1 := &testAction{delay: 1 * time.Second}
	action2 := &testAction{delay: 1 * time.Second}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)

	// Create executor with default config
	executor := NewParallelNodeExecutor(DefaultConfig())

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Test parallel execution with timeout
	nodes := []*Node{node1, node2}
	err := executor.ExecuteNodes(ctx, nodes, data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")

	// Verify that actions were not completed
	assert.False(t, action1.executed)
	assert.False(t, action2.executed)
}
