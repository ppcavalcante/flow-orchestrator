package testutil

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// MockWorkflowStore is a test implementation of WorkflowStore interface
type MockWorkflowStore struct {
	data *workflow.WorkflowData
	mu   sync.RWMutex
	// For testing behavior
	shouldFailSave bool
	shouldFailLoad bool
	SaveFunc       func(*workflow.WorkflowData) error
	LoadFunc       func(string) (*workflow.WorkflowData, error)
	DeleteFunc     func(string) error
}

// NewMockWorkflowStore creates a new mock workflow store with default implementations
func NewMockWorkflowStore() *MockWorkflowStore {
	return &MockWorkflowStore{}
}

// Save implements the WorkflowStore interface Save method
func (m *MockWorkflowStore) Save(data *workflow.WorkflowData) error {
	if m.SaveFunc != nil {
		return m.SaveFunc(data)
	}
	if m.shouldFailSave {
		return fmt.Errorf("mock save failure")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = data.Clone()
	return nil
}

// Load implements the WorkflowStore interface Load method
func (m *MockWorkflowStore) Load(id string) (*workflow.WorkflowData, error) {
	if m.LoadFunc != nil {
		return m.LoadFunc(id)
	}
	if m.shouldFailLoad {
		return nil, fmt.Errorf("mock load failure")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.data == nil {
		return nil, fmt.Errorf("no data found for id: %s", id)
	}
	return m.data.Clone(), nil
}

// Delete implements the WorkflowStore interface Delete method
func (m *MockWorkflowStore) Delete(id string) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = nil
	return nil
}

// SetShouldFailSave sets whether Save should fail
func (m *MockWorkflowStore) SetShouldFailSave(should bool) {
	m.shouldFailSave = should
}

// SetShouldFailLoad sets whether Load should fail
func (m *MockWorkflowStore) SetShouldFailLoad(should bool) {
	m.shouldFailLoad = should
}

// TestAction represents a test action with configurable behavior
type TestAction struct {
	Name          string
	ExecutionTime time.Duration
	ShouldFail    bool
	ErrorMsg      string
	DataModifier  func(*workflow.WorkflowData)
	Output        string
	Err           error
	Delay         time.Duration
}

// ToAction converts the test action to an Action
func (t *TestAction) ToAction() workflow.Action {
	return workflow.ActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		if t.Delay > 0 {
			time.Sleep(t.Delay)
		}
		if t.ShouldFail {
			return fmt.Errorf("%s: %w", t.ErrorMsg, t.Err)
		}
		if t.Output != "" {
			data.SetOutput(t.Name, t.Output)
		}
		if t.DataModifier != nil {
			t.DataModifier(data)
		}
		return nil
	})
}

// CreateTestNode creates a test node with the provided action
func CreateTestNode(name string, action workflow.Action) *workflow.Node {
	return workflow.NewNode(name, action)
}

// CreateTestAction creates a simple test action that doesn't fail
func CreateTestAction(name string) workflow.Action {
	return workflow.ActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.SetOutput(name, fmt.Sprintf("output-%s", name))
		return nil
	})
}

// CreateFailingAction creates a test action that always fails
func CreateFailingAction(errMsg string) workflow.Action {
	return workflow.ActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
		return fmt.Errorf("%s", errMsg)
	})
}

// CreateDelayedAction creates a test action that waits for the given duration
func CreateDelayedAction(delay time.Duration) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, _ *workflow.WorkflowData) error {
		select {
		case <-time.After(delay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// WorkflowAssertion provides a utility for testing workflows
type WorkflowAssertion struct {
	DAG  *workflow.DAG
	Data *workflow.WorkflowData
}

// NewWorkflowAssertion creates a new workflow assertion utility
func NewWorkflowAssertion(dag *workflow.DAG) *WorkflowAssertion {
	return &WorkflowAssertion{
		DAG:  dag,
		Data: workflow.NewWorkflowData(dag.Name),
	}
}

// WithInitialData sets the initial data for workflow execution
func (a *WorkflowAssertion) WithInitialData(data *workflow.WorkflowData) *WorkflowAssertion {
	a.Data = data
	return a
}

// ExpectSuccess executes the workflow and expects it to succeed
func (a *WorkflowAssertion) ExpectSuccess() *WorkflowAssertion {
	ctx := context.Background()
	err := a.DAG.Execute(ctx, a.Data)
	if err != nil {
		panic(fmt.Sprintf("Expected workflow to succeed, but got error: %v", err))
	}
	return a
}

// ExpectFailure executes the workflow and expects it to fail
func (a *WorkflowAssertion) ExpectFailure() *WorkflowAssertion {
	ctx := context.Background()
	err := a.DAG.Execute(ctx, a.Data)
	if err == nil {
		panic("Expected workflow to fail, but it succeeded")
	}
	return a
}

// ExpectNodeCompleted checks if a node has completed successfully
func (a *WorkflowAssertion) ExpectNodeCompleted(nodeName string) *WorkflowAssertion {
	status, ok := a.Data.GetNodeStatus(nodeName)
	if !ok {
		panic(fmt.Sprintf("Node %s not found", nodeName))
	}
	if status != workflow.Completed {
		panic(fmt.Sprintf("Expected node %s to be completed, but status was %s", nodeName, status))
	}
	return a
}

// ExpectNodeFailed checks if a node has failed
func (a *WorkflowAssertion) ExpectNodeFailed(nodeName string) *WorkflowAssertion {
	status, ok := a.Data.GetNodeStatus(nodeName)
	if !ok {
		panic(fmt.Sprintf("Node %s not found", nodeName))
	}
	if status != workflow.Failed {
		panic(fmt.Sprintf("Expected node %s to be failed, but status was %s", nodeName, status))
	}
	return a
}

// ExpectOutput checks if a node's output matches the expected value
func (a *WorkflowAssertion) ExpectOutput(nodeName string, expected string) *WorkflowAssertion {
	output, found := a.Data.GetOutput(nodeName)
	if !found {
		panic(fmt.Sprintf("Expected output for node %s, but none was found", nodeName))
	}
	if output != expected {
		panic(fmt.Sprintf("Expected node %s to have output %s, but got %s", nodeName, expected, output))
	}
	return a
}

// ExpectValue checks if a key has the expected value
func (a *WorkflowAssertion) ExpectValue(key string, expected interface{}) *WorkflowAssertion {
	value, found := a.Data.Get(key)
	if !found {
		panic(fmt.Sprintf("Expected key %s to be present, but it was not found", key))
	}
	if value != expected {
		panic(fmt.Sprintf("Expected key %s to have value %v, but got %v", key, expected, value))
	}
	return a
}

// ExpectNodesInStatus checks if multiple nodes are in the expected status
func (a *WorkflowAssertion) ExpectNodesInStatus(status workflow.NodeStatus, nodeNames ...string) *WorkflowAssertion {
	for _, nodeName := range nodeNames {
		nodeStatus, ok := a.Data.GetNodeStatus(nodeName)
		if !ok {
			panic(fmt.Sprintf("Node %s not found", nodeName))
		}
		if nodeStatus != status {
			panic(fmt.Sprintf("Expected node %s to be in status %s, but was %s", nodeName, status, nodeStatus))
		}
	}
	return a
}

// BuildTestWorkflow builds a test workflow with the specified nodes and dependencies
func BuildTestWorkflow(id string) *workflow.WorkflowBuilder {
	builder := workflow.NewWorkflowBuilder()
	builder.WithWorkflowID(id)
	return builder
}
