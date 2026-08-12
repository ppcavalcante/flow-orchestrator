package workflow

import (
	"context"
	"errors"
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

		node := newNode("test", action)
		node.retryCount = 3
		if node.name != "test" {
			t.Errorf("Expected node name 'test', got '%s'", node.name)
		}
		if node.retryCount != 3 {
			t.Errorf("Expected retry count 3, got %d", node.retryCount)
		}
	})

	t.Run("Basic execution", func(t *testing.T) {
		executed := false
		action := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			executed = true
			return nil
		})

		node := newNode("test", action)
		data := NewWorkflowData("test-workflow")

		err := node.execute(context.Background(), data)
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

		node := newNode("test", action)
		data := NewWorkflowData("test-workflow")

		err := node.execute(context.Background(), data)
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

		node := newNode("test", action)
		data := NewWorkflowData("test-workflow")

		// Create a context we can cancel
		ctx, cancel := context.WithCancel(context.Background())

		// Execute in a goroutine
		go func() {
			<-startedChan                      // Wait for the action to start
			time.Sleep(100 * time.Millisecond) // Small delay to ensure the action is in the select
			cancel()                           // Cancel the context
		}()

		err := node.execute(ctx, data)

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

		node := newNode("test", action)
		node.timeout = 100 * time.Millisecond
		data := NewWorkflowData("test-workflow")

		err := node.execute(context.Background(), data)
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

		node := newNode("test", action)
		node.retryCount = 3
		data := NewWorkflowData("test-workflow")

		err := node.execute(context.Background(), data)
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

		node := newNode("test", action)
		node.retryCount = 2
		data := NewWorkflowData("test-workflow")

		err := node.execute(context.Background(), data)
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

		node1 := newNode("node1", action1)
		node2 := newNode("node2", action2)
		node2.dependsOn = []*Node{node1}

		// Create and setup a DAG
		dag := newDAGForTest("test-dependencies")

		// Add nodes to the DAG
		mustAddNode(t, dag, node1)
		mustAddNode(t, dag, node2)
		// Add dependencies
		mustAddDep(t, dag, "node1", "node2")
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

// TestNodeDependencies covers the two live read accessors on Node.
//
// Its former subject — (*Node).AddDependency and AddDependencies — was deleted by M23
// SEAL-01, so the fixture builds the edge set directly in-package. That is deliberate:
// the subject here is ACCESSOR behaviour, and how the fixture is constructed is not
// what is being asserted. The sanctioned external path (the builder's DependsOn, wired
// by build()) is covered in the builder tests.
func TestNodeDependencies(t *testing.T) {
	newFixture := func() (*Node, *Node) {
		node := newNode("node1", nil)
		dep1 := newNode("dep1", nil)
		node.dependsOn = append(node.dependsOn, dep1, newNode("dep2", nil), newNode("dep3", nil))
		return node, dep1
	}

	t.Run("HasDependency matches by name", func(t *testing.T) {
		node, _ := newFixture()
		assert.True(t, node.HasDependency("dep1"))
		assert.True(t, node.HasDependency("dep3"))
		assert.False(t, node.HasDependency("absent"))
	})

	t.Run("GetDependencies returns every edge, in order", func(t *testing.T) {
		node, _ := newFixture()
		deps := node.GetDependencies()
		if len(deps) != 3 {
			t.Fatalf("expected 3 dependencies, got %d", len(deps))
		}
		assert.Equal(t, "dep1", deps[0].name)
		assert.Equal(t, "dep2", deps[1].name)
		assert.Equal(t, "dep3", deps[2].name)
	})

	t.Run("a node with no dependencies", func(t *testing.T) {
		node := newNode("node2", nil)
		assert.Empty(t, node.GetDependencies())
		assert.False(t, node.HasDependency("any"))
	})

	// M23 BYPASS-05, and this guard did not exist before: GetDependencies used to
	// return the LIVE slice header, so a caller could re-parent a node by writing
	// through a READ accessor. The copy is the entire fix — and it looks exactly like a
	// wasteful allocation, which is what makes it pre-armed for silent removal by a
	// later performance pass (SEAL-05's det-tax work is precisely such a pass).
	//
	// WHICH ARM ACTUALLY BITES: the element overwrite. Reslicing or truncating the
	// returned value could never reach the node — a slice header is a value, so the
	// caller only ever truncates its own copy — so that is NOT evidence of the fix and
	// is not asserted as though it were.
	t.Run("the returned slice is a copy, so a caller cannot re-parent through it", func(t *testing.T) {
		node, dep1 := newFixture()

		deps := node.GetDependencies()
		if len(deps) != 3 {
			t.Fatalf("expected 3 dependencies, got %d", len(deps))
		}
		deps[0] = newNode("attacker", nil)

		assert.Same(t, dep1, node.dependsOn[0],
			"overwriting an element of the returned slice must not re-parent the node (BYPASS-05)")
		assert.True(t, node.HasDependency("dep1"))
		assert.False(t, node.HasDependency("attacker"))
	})
}

func TestNodeWithCapacity(t *testing.T) {
	node := newNodeWithCapacity("test", nil, 5)
	assert.NotNil(t, node)
	assert.Equal(t, "test", node.name)

	// The capacity hint is this constructor's whole reason to exist, so assert it
	// rather than only that appends land.
	assert.GreaterOrEqual(t, cap(node.dependsOn), 5, "the dependency capacity hint must be honored")
	assert.Empty(t, node.GetDependencies(), "a capacity hint must not create edges")
}

// TestNodeConfiguration was DELETED by M23 SEAL-01. It exercised WithRetries/
// WithTimeout/WithDependencies — three of the six post-build mutators the seal
// removes. Rewriting it against the unexported fields would assert that Go struct
// assignment works, not that any engine contract holds. Node construction is now
// covered where it belongs: build() (builder_test.go) and the SEAL-09 mint
// chokepoint (suspendable_capability_test.go).

// TestAddDependencies was DELETED by M23 SEAL-01, on the same standard and after the
// same check. Its subject was (*Node).AddDependencies, one of the six deleted mutators.
// Migrated mechanically, all ten of its subtests became assertions about Go's built-in
// append and about slice capacity growth — "appending nothing changes nothing",
// "capacity does not decrease", "duplicate and nil elements are both appended" — and
// its last subtest asserted that struct fields retain the values just assigned to them.
// None of that is an engine contract; it is the runtime's slice implementation, and
// `go vet` flagged one rewritten line as `append with no values`.
//
// What was worth keeping was moved rather than dropped: the accessor behaviour it
// incidentally covered is now asserted directly in TestNodeDependencies above,
// including the BYPASS-05 defensive-copy guard that no test covered at all.
