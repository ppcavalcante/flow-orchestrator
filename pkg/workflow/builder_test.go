package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkflowBuilder(t *testing.T) {
	t.Run("Building workflow with builder pattern", func(t *testing.T) {
		// Create workflow store
		store := NewInMemoryStore()

		// Create workflow using the builder API
		builder := NewWorkflowBuilder().
			WithStore(store)

		// Build workflow DAG with fluent API
		builder.AddStartNode("fetch").
			WithAction(func(ctx context.Context, data *WorkflowData) error {
				return nil
			}).
			WithRetries(1).
			WithTimeout(100 * time.Millisecond)

		builder.AddNode("process").
			WithAction(func(ctx context.Context, data *WorkflowData) error {
				return errors.New("validation error") // Return a standard error
			}).
			WithRetries(1).
			DependsOn("fetch").
			WithTimeout(100 * time.Millisecond)

		// Build the DAG
		dag, err := builder.Build()
		if err != nil {
			t.Errorf("Failed to build workflow: %v", err)
			return
		}

		// Create initial data
		workflowData := NewWorkflowData("test-workflow")

		// Execute the workflow
		err = dag.Execute(context.Background(), workflowData)
		if err == nil {
			t.Error("Expected workflow execution to fail")
		}
	})

	t.Run("Builder validation", func(t *testing.T) {
		builder := NewWorkflowBuilder()

		// Try to create invalid dependencies
		builder.AddNode("B").
			WithAction(func(ctx context.Context, data *WorkflowData) error {
				return nil
			}).
			DependsOn("A") // Depends on non-existent node

		// Build should fail
		_, err := builder.Build()
		if err == nil {
			t.Error("Expected build to fail due to invalid dependency")
		}
	})

	t.Run("Cycle detection during build", func(t *testing.T) {
		// Create a simple workflow with a self-dependency (guaranteed cycle)
		builder := NewWorkflowBuilder().
			WithWorkflowID("self-cycle-workflow")

		// Add a node that depends on itself (clear cycle)
		builder.AddNode("self-cycle").
			WithAction(func(ctx context.Context, data *WorkflowData) error {
				return nil
			}).
			DependsOn("self-cycle") // Self-dependency creates an obvious cycle

		// Build should fail due to cycle detection
		_, err := builder.Build()
		if err == nil {
			t.Error("Expected build to fail due to cycle")
		} else if !strings.Contains(err.Error(), "cycle") {
			t.Errorf("Expected error to mention cycle, got: %v", err)
		}
	})
}

func TestWorkflowBuilderWithWorkflowID(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Set a custom workflow ID
	customID := "custom-workflow-id"
	returnedBuilder := builder.WithWorkflowID(customID)

	// Check that the ID was set correctly
	if builder.workflowID != customID {
		t.Errorf("Expected workflowID to be %q, got %q", customID, builder.workflowID)
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != builder {
		t.Error("WithWorkflowID did not return the builder for chaining")
	}
}

func TestWorkflowBuilderWithStore(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Create a mock store
	store := &mockStoreForBuilder{}

	// Set the store
	returnedBuilder := builder.WithStore(store)

	// Check that the store was set correctly
	if builder.store != store {
		t.Error("Expected store to be set")
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != builder {
		t.Error("WithStore did not return the builder for chaining")
	}
}

func TestWorkflowBuilderWithStateStore(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Create a mock store
	store := &mockStoreForBuilder{}

	// Set the store using the alias method
	returnedBuilder := builder.WithStateStore(store)

	// Check that the store was set correctly
	if builder.store != store {
		t.Error("Expected store to be set")
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != builder {
		t.Error("WithStateStore did not return the builder for chaining")
	}
}

func TestWorkflowBuilderAddNode(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Add a node
	nodeName := "test-node"
	nodeBuilder := builder.AddNode(nodeName)

	// Check that the node was added correctly
	if len(builder.nodes) != 1 {
		t.Errorf("Expected 1 node, got %d", len(builder.nodes))
	}
	if builder.nodes[0] != nodeBuilder {
		t.Error("Node builder not added to nodes list")
	}
	if nodeBuilder.name != nodeName {
		t.Errorf("Expected node name to be %q, got %q", nodeName, nodeBuilder.name)
	}
	if nodeBuilder.workflow != builder {
		t.Error("Node builder's workflow reference not set correctly")
	}
}

func TestWorkflowBuilderAddStartNode(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Add a start node
	nodeName := "start-node"
	nodeBuilder := builder.AddStartNode(nodeName)

	// Check that the node was added correctly
	if len(builder.nodes) != 1 {
		t.Errorf("Expected 1 node, got %d", len(builder.nodes))
	}
	if len(builder.startNodes) != 1 {
		t.Errorf("Expected 1 start node, got %d", len(builder.startNodes))
	}
	if builder.startNodes[0] != nodeName {
		t.Errorf("Expected start node name to be %q, got %q", nodeName, builder.startNodes[0])
	}
	if nodeBuilder.name != nodeName {
		t.Errorf("Expected node name to be %q, got %q", nodeName, nodeBuilder.name)
	}
}

func TestNodeBuilderWithAction(t *testing.T) {
	// Create a new workflow builder and node
	builder := NewWorkflowBuilder()
	nodeBuilder := builder.AddNode("test-node")

	// Test with an Action interface
	action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})
	returnedBuilder := nodeBuilder.WithAction(action)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set")
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != nodeBuilder {
		t.Error("WithAction did not return the builder for chaining")
	}

	// Test with a function
	nodeBuilder = builder.AddNode("function-node")
	fn := func(ctx context.Context, data *WorkflowData) error {
		return nil
	}
	nodeBuilder.WithAction(fn)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from function")
	}

	// Test with a legacy function signature
	nodeBuilder = builder.AddNode("legacy-node")
	legacyFn := func(ctx context.Context, state interface{}) (interface{}, interface{}) {
		return "output", nil
	}
	nodeBuilder.WithAction(legacyFn)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from legacy function")
	}

	// Test with nil action
	nodeBuilder = builder.AddNode("nil-node")
	nodeBuilder.WithAction(nil)

	// Check that a default action was created
	if nodeBuilder.action == nil {
		t.Error("Expected default action to be created for nil")
	}

	// Execute the default action to verify it returns an error
	err := nodeBuilder.action.Execute(context.Background(), NewWorkflowData("test"))
	if err == nil {
		t.Error("Expected default action to return an error")
	}

	// Test with a composite action
	nodeBuilder = builder.AddNode("composite-node")
	compositeAction := NewCompositeAction(
		ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}),
		ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}),
	)
	nodeBuilder.WithAction(compositeAction)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from composite action")
	}

	// Test with a validation action
	nodeBuilder = builder.AddNode("validation-node")
	validationAction := NewValidationAction(
		"input",
		func(v interface{}) error { return nil },
		"valid",
		"error",
	)
	nodeBuilder.WithAction(validationAction)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from validation action")
	}

	// Test with a map action
	nodeBuilder = builder.AddNode("map-node")
	mapAction := NewMapAction(
		"input",
		"output",
		func(v interface{}) (interface{}, error) { return v, nil },
	)
	nodeBuilder.WithAction(mapAction)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from map action")
	}

	// Test with a retryable action
	nodeBuilder = builder.AddNode("retry-node")
	retryableAction := NewRetryableAction(
		ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return nil
		}),
		3,
		time.Second,
	)
	nodeBuilder.WithAction(retryableAction)

	// Check that the action was set correctly
	if nodeBuilder.action == nil {
		t.Error("Expected action to be set from retryable action")
	}

	// Test with an unsupported type
	nodeBuilder = builder.AddNode("unsupported-node")
	nodeBuilder.WithAction("not an action")

	// Check that a default action was created
	if nodeBuilder.action == nil {
		t.Error("Expected default action to be created")
	}

	// Execute the default action to verify it returns an error
	err = nodeBuilder.action.Execute(context.Background(), NewWorkflowData("test"))
	if err == nil {
		t.Error("Expected default action to return an error")
	}
}

func TestNodeBuilderWithRetry(t *testing.T) {
	// Create a new workflow builder and node
	builder := NewWorkflowBuilder()
	nodeBuilder := builder.AddNode("test-node")

	// Set retry count
	retryCount := 3
	returnedBuilder := nodeBuilder.WithRetry(retryCount)

	// Check that the retry count was set correctly
	if nodeBuilder.retryCount != retryCount {
		t.Errorf("Expected retryCount to be %d, got %d", retryCount, nodeBuilder.retryCount)
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != nodeBuilder {
		t.Error("WithRetry did not return the builder for chaining")
	}
}

func TestNodeBuilderDependsOn(t *testing.T) {
	// Create a new workflow builder and nodes
	builder := NewWorkflowBuilder()
	nodeBuilder := builder.AddNode("test-node")

	// Add dependencies
	dep1 := "dep1"
	dep2 := "dep2"
	returnedBuilder := nodeBuilder.DependsOn(dep1, dep2)

	// Check that the dependencies were added correctly
	if len(nodeBuilder.dependencies) != 2 {
		t.Errorf("Expected 2 dependencies, got %d", len(nodeBuilder.dependencies))
	}
	if nodeBuilder.dependencies[0] != dep1 {
		t.Errorf("Expected first dependency to be %q, got %q", dep1, nodeBuilder.dependencies[0])
	}
	if nodeBuilder.dependencies[1] != dep2 {
		t.Errorf("Expected second dependency to be %q, got %q", dep2, nodeBuilder.dependencies[1])
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != nodeBuilder {
		t.Error("DependsOn did not return the builder for chaining")
	}

	// Add more dependencies
	dep3 := "dep3"
	nodeBuilder.DependsOn(dep3)

	// Check that the new dependency was added correctly
	if len(nodeBuilder.dependencies) != 3 {
		t.Errorf("Expected 3 dependencies, got %d", len(nodeBuilder.dependencies))
	}
	if nodeBuilder.dependencies[2] != dep3 {
		t.Errorf("Expected third dependency to be %q, got %q", dep3, nodeBuilder.dependencies[2])
	}
}

func TestNodeBuilderWithRetries(t *testing.T) {
	// Create a new workflow builder and node
	builder := NewWorkflowBuilder()
	nodeBuilder := builder.AddNode("test-node")

	// Set retry count
	retryCount := 5
	returnedBuilder := nodeBuilder.WithRetries(retryCount)

	// Check that the retry count was set correctly
	if nodeBuilder.retryCount != retryCount {
		t.Errorf("Expected retryCount to be %d, got %d", retryCount, nodeBuilder.retryCount)
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != nodeBuilder {
		t.Error("WithRetries did not return the builder for chaining")
	}
}

func TestNodeBuilderWithTimeout(t *testing.T) {
	// Create a new workflow builder and node
	builder := NewWorkflowBuilder()
	nodeBuilder := builder.AddNode("test-node")

	// Set timeout
	timeout := 5 * time.Second
	returnedBuilder := nodeBuilder.WithTimeout(timeout)

	// Check that the timeout was set correctly
	if nodeBuilder.timeout != timeout {
		t.Errorf("Expected timeout to be %v, got %v", timeout, nodeBuilder.timeout)
	}

	// Check that the method returns the builder for chaining
	if returnedBuilder != nodeBuilder {
		t.Error("WithTimeout did not return the builder for chaining")
	}
}

func TestWorkflowBuilderBuild(t *testing.T) {
	// Create a new workflow builder
	builder := NewWorkflowBuilder()

	// Add some nodes with actions
	action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Add nodes and configure them
	builder.AddStartNode("A").WithAction(action)
	builder.AddNode("B").WithAction(action).DependsOn("A").WithRetries(3)
	builder.AddNode("C").WithAction(action).DependsOn("A").WithTimeout(5 * time.Second)
	builder.AddNode("D").WithAction(action).DependsOn("B", "C")

	// Build the DAG
	dag, err := builder.Build()

	// Check that the DAG was built correctly
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if dag == nil {
		t.Fatal("Expected DAG to be created")
	}

	// Check that all nodes were added
	if len(dag.Nodes) != 4 {
		t.Errorf("Expected 4 nodes in DAG, got %d", len(dag.Nodes))
	}

	// Check that dependencies were set up correctly
	nodeAInDag, exists := dag.GetNode("A")
	if !exists {
		t.Fatal("Node A not found in DAG")
	}
	if len(nodeAInDag.DependsOn) != 0 {
		t.Errorf("Expected node A to have 0 dependencies, got %d", len(nodeAInDag.DependsOn))
	}

	nodeBInDag, exists := dag.GetNode("B")
	if !exists {
		t.Fatal("Node B not found in DAG")
	}
	if len(nodeBInDag.DependsOn) != 1 {
		t.Errorf("Expected node B to have 1 dependency, got %d", len(nodeBInDag.DependsOn))
	}
	if nodeBInDag.DependsOn[0].Name != "A" {
		t.Errorf("Expected node B to depend on A, got %s", nodeBInDag.DependsOn[0].Name)
	}

	// Check that configuration was applied
	if nodeBInDag.RetryCount != 3 {
		t.Errorf("Expected node B to have retry count 3, got %d", nodeBInDag.RetryCount)
	}

	nodeCInDag, exists := dag.GetNode("C")
	if !exists {
		t.Fatal("Node C not found in DAG")
	}
	if nodeCInDag.Timeout != 5*time.Second {
		t.Errorf("Expected node C to have timeout 5s, got %v", nodeCInDag.Timeout)
	}

	// Test building with a node that has no action
	builder = NewWorkflowBuilder()
	builder.AddNode("no-action")
	_, err = builder.Build()
	if err == nil {
		t.Error("Expected error when building with a node that has no action")
	}

	// Test building with a non-existent dependency
	builder = NewWorkflowBuilder()
	builder.AddNode("node").WithAction(action).DependsOn("non-existent")
	_, err = builder.Build()
	if err == nil {
		t.Error("Expected error when building with a non-existent dependency")
	}

	// Test building with a cycle
	builder = NewWorkflowBuilder()
	builder.AddNode("A").WithAction(action).DependsOn("B")
	builder.AddNode("B").WithAction(action).DependsOn("A")
	_, err = builder.Build()
	if err == nil {
		t.Error("Expected error when building with a cycle")
	}
}

func TestNodeBuilderWithActionErrorCases(t *testing.T) {
	builder := NewWorkflowBuilder()

	t.Run("Legacy function with error", func(t *testing.T) {
		nodeBuilder := builder.AddNode("legacy-error-node")
		legacyFn := func(ctx context.Context, state interface{}) (interface{}, interface{}) {
			return nil, errors.New("legacy error")
		}
		nodeBuilder.WithAction(legacyFn)

		// Execute the action - should not return error since legacy function errors are ignored
		err := nodeBuilder.action.Execute(context.Background(), NewWorkflowData("test"))
		if err != nil {
			t.Errorf("Legacy function errors should be ignored, got: %v", err)
		}
	})

	t.Run("Panicking function", func(t *testing.T) {
		nodeBuilder := builder.AddNode("panic-node")
		nodeBuilder.WithAction(ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			panic("test panic")
		}))

		// Execute the action in a deferred function to catch the panic
		defer func() {
			if r := recover(); r == nil {
				t.Error("Expected panic was not caught")
			}
		}()

		_ = nodeBuilder.action.Execute(context.Background(), NewWorkflowData("test"))
	})

	t.Run("Cancelled context", func(t *testing.T) {
		nodeBuilder := builder.AddNode("context-node")
		nodeBuilder.WithAction(func(ctx context.Context, data *WorkflowData) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := nodeBuilder.action.Execute(ctx, NewWorkflowData("test"))
		if err != context.Canceled {
			t.Errorf("Expected context.Canceled error, got: %v", err)
		}
	})
}

func TestNodeBuilderWithActionComplexCases(t *testing.T) {
	builder := NewWorkflowBuilder()

	t.Run("Composite action with error", func(t *testing.T) {
		nodeBuilder := builder.AddNode("composite-error-node")
		compositeAction := NewCompositeAction(
			ActionFunc(func(ctx context.Context, data *WorkflowData) error {
				data.Set("step1", "done")
				return nil
			}),
			ActionFunc(func(ctx context.Context, data *WorkflowData) error {
				return errors.New("step2 failed")
			}),
		)
		nodeBuilder.WithAction(compositeAction)

		data := NewWorkflowData("test")
		err := nodeBuilder.action.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected error from composite action")
		}
		if _, exists := data.Get("step1"); !exists {
			t.Error("Step 1 should have executed before error")
		}
	})

	t.Run("Map action with type error", func(t *testing.T) {
		nodeBuilder := builder.AddNode("map-error-node")
		mapAction := NewMapAction(
			"input",
			"output",
			func(v interface{}) (interface{}, error) {
				str, ok := v.(string)
				if !ok {
					return nil, errors.New("invalid input type")
				}
				return strings.ToUpper(str), nil
			},
		)
		nodeBuilder.WithAction(mapAction)

		data := NewWorkflowData("test")
		data.Set("input", 123) // Wrong type
		err := nodeBuilder.action.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected error from map action type mismatch")
		}
	})

	t.Run("Validation action with missing input", func(t *testing.T) {
		nodeBuilder := builder.AddNode("validation-error-node")
		validationAction := NewValidationAction(
			"missing-input",
			func(v interface{}) error { return nil },
			"valid",
			"error",
		)
		nodeBuilder.WithAction(validationAction)

		data := NewWorkflowData("test")
		err := nodeBuilder.action.Execute(context.Background(), data)
		if err == nil {
			t.Error("Expected error from missing validation input")
		}
		if errMsg, exists := data.Get("error"); !exists || !strings.Contains(errMsg.(string), "not found") {
			t.Error("Validation error for missing input not properly recorded")
		}
	})
}

// Mock workflow store for testing
type mockStoreForBuilder struct {
	data map[string]*WorkflowData
}

func (m *mockStoreForBuilder) Save(data *WorkflowData) error {
	return nil
}

func (m *mockStoreForBuilder) Load(id string) (*WorkflowData, error) {
	return nil, nil
}

func (m *mockStoreForBuilder) ListWorkflows() ([]string, error) {
	return nil, nil
}

func (m *mockStoreForBuilder) Delete(id string) error {
	return nil
}
