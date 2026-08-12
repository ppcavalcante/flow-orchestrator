package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewWorkflow(t *testing.T) {
	store := NewInMemoryStore()
	workflow := newWorkflowForTest(store)

	// t.Fatal (not t.Error) on the nil check: t.Error continues execution, so the
	// subsequent field derefs would nil-panic if workflow were nil (SA5011). Fatal
	// halts the goroutine before the derefs.
	if workflow == nil {
		t.Fatal("NewWorkflow should return a non-nil workflow")
	}
	if workflow.Store == nil {
		t.Error("Workflow should have a store")
	}
	// .dag, the FIELD, not .DAG() the accessor — and go vet is what forced the
	// distinction. M23 SEAL-06 added a DAG() method beside the sealed field, so
	// `workflow.DAG == nil` stopped being a field comparison and silently became a
	// comparison against a METHOD VALUE, which is never nil. It compiled. vet caught it
	// with "comparison of function DAG == nil is always false"; without vet this
	// assertion would have passed forever on any Workflow, graph or no graph.
	if workflow.dag == nil {
		t.Error("Workflow should have a DAG")
	}
	if workflow.WorkflowID == "" {
		t.Error("Workflow should have an ID")
	}
}

func TestWorkflowAddNode(t *testing.T) {
	workflow := newWorkflowForTest(NewInMemoryStore())
	node := newNode("test", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	}))

	err := workflow.addNode(node)
	if err != nil {
		t.Errorf("AddNode returned error: %v", err)
	}

	// Verify node was added by checking the DAG
	if len(workflow.dag.nodes) != 1 {
		t.Error("Workflow DAG should have one node")
	}
}

func TestWorkflowExecution(t *testing.T) {
	t.Run("Successful execution", func(t *testing.T) {
		store := NewInMemoryStore()
		workflow := newWorkflowForTest(store).WithWorkflowID("test-workflow")

		executionOrder := make([]string, 0)

		// Add two nodes
		err := workflow.addNode(newNode("node1", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executionOrder = append(executionOrder, "node1")
			data.Set("node1-executed", true)
			return nil
		})))
		if err != nil {
			t.Fatalf("Failed to add node1: %v", err)
		}

		err = workflow.addNode(newNode("node2", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executionOrder = append(executionOrder, "node2")
			node1Executed, _ := data.GetBool("node1-executed")
			if !node1Executed {
				return errors.New("node1 did not execute before node2")
			}
			return nil
		})))
		if err != nil {
			t.Fatalf("Failed to add node2: %v", err)
		}

		// Add dependency: node1 -> node2
		err = workflow.addDependency("node1", "node2")
		if err != nil {
			t.Fatalf("Failed to add dependency: %v", err)
		}

		// Execute workflow
		err = workflow.Execute(context.Background())
		if err != nil {
			t.Errorf("Workflow execution failed: %v", err)
		}

		// Check execution order
		if len(executionOrder) != 2 {
			t.Errorf("Expected 2 node executions, got %d", len(executionOrder))
		}
		if len(executionOrder) >= 1 && executionOrder[0] != "node1" {
			t.Errorf("Expected node1 to execute first, got %s", executionOrder[0])
		}
		if len(executionOrder) >= 2 && executionOrder[1] != "node2" {
			t.Errorf("Expected node2 to execute second, got %s", executionOrder[1])
		}
	})

	t.Run("Context cancellation", func(t *testing.T) {
		store := NewInMemoryStore()
		workflow := newWorkflowForTest(store)

		// Add a node with sleep
		err := workflow.addNode(newNode("sleep-node", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			select {
			case <-time.After(500 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})))
		if err != nil {
			t.Fatalf("Failed to add node: %v", err)
		}

		// Create a context that will be canceled
		ctx, cancel := context.WithCancel(context.Background())

		// Cancel after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		// Execute workflow with cancellable context
		err = workflow.Execute(ctx)

		// Should fail with context canceled error wrapped in node execution error
		if err == nil {
			t.Error("Expected context cancellation error, got nil")
		} else if !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("Expected error to contain 'context canceled', got: %v", err)
		}
	})

	t.Run("Node execution failure", func(t *testing.T) {
		store := NewInMemoryStore()
		workflow := newWorkflowForTest(store)

		expectedErr := errors.New("node execution failed")

		// Add a failing node
		err := workflow.addNode(newNode("failing-node", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return expectedErr
		})))
		if err != nil {
			t.Fatalf("Failed to add node: %v", err)
		}

		// Execute workflow
		err = workflow.Execute(context.Background())

		// Should fail with the expected error wrapped in node execution error
		if err == nil {
			t.Error("Expected error, got nil")
		} else if !strings.Contains(err.Error(), expectedErr.Error()) {
			t.Errorf("Expected error to contain '%v', got '%v'", expectedErr, err)
		}
	})

	t.Run("Corrupt Store.Load error is propagated, not swallowed", func(t *testing.T) {
		// A corrupt persisted payload must surface as an error from Execute.
		// Swallowing it would start fresh and overwrite the persisted state on
		// the next Save, silently losing it.
		store := newMockWorkflowStore()
		store.loadError = fmt.Errorf("%w: malformed JSON workflow data", ErrCorruptData)

		var ran bool
		wf := &Workflow{
			dag:        newDAGForTest("corrupt-resume"),
			WorkflowID: "corrupt-resume",
			Store:      store,
		}
		require.NoError(t, wf.addNode(newNode("only", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			ran = true
			return nil
		}))))

		err := wf.Execute(context.Background())

		require.Error(t, err)
		require.ErrorIs(t, err, ErrCorruptData, "corrupt load must propagate")
		require.False(t, ran, "DAG must not run when resume load is corrupt")
		require.Nil(t, store.data, "must not save fresh state over the corrupt entry")
	})

	t.Run("ErrNotFound from Store.Load starts fresh and succeeds", func(t *testing.T) {
		// A missing workflow is the expected "no prior state" case — resume
		// starts fresh and runs to completion.
		store := newMockWorkflowStore() // empty -> Load returns ErrNotFound
		wf := &Workflow{
			dag:        newDAGForTest("fresh-start"),
			WorkflowID: "fresh-start",
			Store:      store,
		}
		var ran bool
		require.NoError(t, wf.addNode(newNode("only", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			ran = true
			return nil
		}))))

		err := wf.Execute(context.Background())

		require.NoError(t, err, "ErrNotFound must be treated as start-fresh")
		require.True(t, ran, "DAG should run on a fresh start")
		require.NotNil(t, store.data, "fresh final state should be saved")
	})
}

func TestWorkflowBuilderOperations(t *testing.T) {
	t.Run("Create workflow from builder", func(t *testing.T) {
		// Create a builder with nodes and dependencies
		builder := NewWorkflowBuilder().
			WithWorkflowID("test-workflow")

		// Add nodes with actions
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		})

		// Add start node A
		builder.AddStartNode("A").
			WithAction(action)

		// Add node B that depends on A
		builder.AddNode("B").
			WithAction(action).
			DependsOn("A")

		// Create workflow from builder
		workflow, err := FromBuilder(builder)
		require.NoError(t, err)
		require.NotNil(t, workflow)
		require.Equal(t, "test-workflow", workflow.WorkflowID)

		// Verify nodes and dependencies
		nodeA, exists := workflow.dag.GetNode("A")
		require.True(t, exists)
		require.Empty(t, nodeA.dependsOn)

		nodeB, exists := workflow.dag.GetNode("B")
		require.True(t, exists)
		require.Len(t, nodeB.dependsOn, 1)
		require.Equal(t, "A", nodeB.dependsOn[0].name)
	})

	t.Run("Builder validation error", func(t *testing.T) {
		// Create a builder with invalid configuration
		builder := NewWorkflowBuilder()
		builder.AddNode("A") // Node without action

		// Attempt to create workflow
		workflow, err := FromBuilder(builder)
		require.Error(t, err)
		require.Nil(t, workflow)
		require.Contains(t, err.Error(), "has no action defined")
	})

	t.Run("FromBuilder_freshBuilder", func(t *testing.T) {
		// Test that a fresh builder can drive FromBuilder end to end
		builder := NewWorkflowBuilder()
		require.NotNil(t, builder)
		require.Empty(t, builder.nodes)
		require.Empty(t, builder.startNodes)

		// Test that we can use the builder to create a workflow
		builder.WithWorkflowID("test-workflow")
		builder.AddStartNode("A").
			WithAction(ActionFunc(func(ctx context.Context, data *WorkflowData) error {
				return nil
			}))

		workflow, err := FromBuilder(builder)
		require.NoError(t, err)
		require.NotNil(t, workflow)
		require.Equal(t, "test-workflow", workflow.WorkflowID)
	})
}

// mockWorkflowStore is a mock implementation of WorkflowStore for testing
type mockWorkflowStore struct {
	data         *WorkflowData
	saveError    error
	loadError    error
	deleteError  error
	workflowList []string
}

func newMockWorkflowStore() *mockWorkflowStore {
	return &mockWorkflowStore{
		workflowList: []string{},
	}
}

func (m *mockWorkflowStore) Save(data *WorkflowData) error {
	if m.saveError != nil {
		return m.saveError
	}
	m.data = data

	// Add to workflow list if not already present
	workflowID := data.GetWorkflowID()
	found := false
	for _, id := range m.workflowList {
		if id == workflowID {
			found = true
			break
		}
	}
	if !found {
		m.workflowList = append(m.workflowList, workflowID)
	}

	return nil
}

func (m *mockWorkflowStore) Load(id string) (*WorkflowData, error) {
	if m.loadError != nil {
		return nil, m.loadError
	}
	if m.data == nil || m.data.GetWorkflowID() != id {
		// Mirror the real stores' contract: a missing workflow is ErrNotFound
		// (the expected "start fresh" signal), not a generic error. Workflow.Execute
		// treats ErrNotFound as start-fresh and propagates any other load error.
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return m.data, nil
}

func (m *mockWorkflowStore) ListWorkflows() ([]string, error) {
	return m.workflowList, nil
}

func (m *mockWorkflowStore) Delete(id string) error {
	if m.deleteError != nil {
		return m.deleteError
	}

	// Remove from list
	for i, wfID := range m.workflowList {
		if wfID == id {
			m.workflowList = append(m.workflowList[:i], m.workflowList[i+1:]...)
			break
		}
	}

	// Clear data if it matches
	if m.data != nil && m.data.GetWorkflowID() == id {
		m.data = nil
	}

	return nil
}
