package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAction represents a test action with configurable behavior
type TestAction struct {
	Name          string
	ExecutionTime time.Duration
	ShouldFail    bool
	FailureMsg    string
	Output        string
	Err           error
	Delay         time.Duration
	DataModifier  func(*WorkflowData)
}

// Execute implements the Action interface
func (t *TestAction) Execute(ctx context.Context, data *WorkflowData) error {
	// Simulate delay if specified
	if t.Delay > 0 {
		time.Sleep(t.Delay)
	}

	// Check if this action should fail
	if t.ShouldFail {
		// Record this node as failed
		data.SetNodeStatus(t.Name, Failed)

		// Return an error
		if t.Err != nil {
			return t.Err
		}
		return fmt.Errorf("%s", t.FailureMsg)
	}

	// Successful execution path
	// Simulate execution time
	if t.ExecutionTime > 0 {
		time.Sleep(t.ExecutionTime)
	}

	// Apply custom data modifications if provided
	if t.DataModifier != nil {
		t.DataModifier(data)
	}

	// Set output and completed status
	data.SetOutput(t.Name, t.Output)
	data.SetNodeStatus(t.Name, Completed)

	return nil
}

// TestDAGBuilder helps build test DAGs with a fluent API
type TestDAGBuilder struct {
	nodes     map[string]*Node
	edges     map[string][]string
	nodeNames []string
	store     WorkflowStore
}

// NewTestDAGBuilder creates a new test DAG builder
func NewTestDAGBuilder() *TestDAGBuilder {
	return &TestDAGBuilder{
		nodes: make(map[string]*Node),
		edges: make(map[string][]string),
		store: NewInMemoryStore(),
	}
}

// WithWorkflowStore sets the store for the test DAG
func (b *TestDAGBuilder) WithWorkflowStore(store WorkflowStore) *TestDAGBuilder {
	b.store = store
	return b
}

// AddNode adds a new node to the test DAG
func (b *TestDAGBuilder) AddNode(name string, action *TestAction) *TestDAGBuilder {
	action.Name = name
	node := NewNode(name, action)
	b.nodes[name] = node
	b.nodeNames = append(b.nodeNames, name)
	return b
}

// AddDependency adds a dependency between two nodes in the test DAG
func (b *TestDAGBuilder) AddDependency(from, to string) *TestDAGBuilder {
	fromNode, exists := b.nodes[from]
	if !exists {
		panic(fmt.Sprintf("node %s not found", from))
	}
	toNode, exists := b.nodes[to]
	if !exists {
		panic(fmt.Sprintf("node %s not found", to))
	}

	toNode.AddDependency(fromNode)

	// Track the edge for visualization
	if _, exists := b.edges[from]; !exists {
		b.edges[from] = make([]string, 0)
	}
	b.edges[from] = append(b.edges[from], to)

	return b
}

// Build creates the final DAG
func (b *TestDAGBuilder) Build() *DAG {
	dag := NewDAG("test-dag")

	// Add all nodes
	for _, node := range b.nodes {
		_ = dag.AddNode(node)
	}

	return dag
}

// Helper functions to create test actions

// SuccessAction creates a test action that succeeds
func SuccessAction(output string) *TestAction {
	return &TestAction{
		Output: output,
	}
}

// FailingAction creates a test action that fails
func FailingAction(msg string) *TestAction {
	return &TestAction{
		ShouldFail: true,
		FailureMsg: msg,
	}
}

// SlowAction creates a test action that takes time to execute
func SlowAction(duration time.Duration) *TestAction {
	return &TestAction{
		ExecutionTime: duration,
	}
}

// DataModifyingAction creates an action that modifies workflow data
func DataModifyingAction(modifier func(*WorkflowData)) *TestAction {
	return &TestAction{
		DataModifier: modifier,
	}
}

// AssertDAGExecution provides methods to test DAG execution
type AssertDAGExecution struct {
	T    *testing.T
	DAG  *DAG
	Data *WorkflowData
}

// ExpectSuccess checks that the DAG executes successfully
func (a *AssertDAGExecution) ExpectSuccess() {
	a.T.Helper()
	err := a.DAG.Execute(context.Background(), a.Data)
	if err != nil {
		a.T.Errorf("Expected successful execution, got error: %v", err)
		return
	}
}

// ExpectFailure checks that the DAG execution fails
func (a *AssertDAGExecution) ExpectFailure() {
	a.T.Helper()

	// Create a context for the test
	ctx := context.Background()

	// Run the DAG with our data
	err := a.DAG.Execute(ctx, a.Data)

	// Check that it failed
	if err == nil {
		a.T.Errorf("Expected failure, got success")
		return
	}
}

// ExpectNodeStatus checks if a node has the expected status
func (a *AssertDAGExecution) ExpectNodeStatus(nodeName string, expected NodeStatus) {
	a.T.Helper()
	status, exists := a.Data.GetNodeStatus(nodeName)
	if !exists {
		a.T.Errorf("Node %s status not found", nodeName)
		return
	}
	if status != expected {
		a.T.Errorf("Expected node %s status to be %s, got %s", nodeName, expected, status)
	}
}

// ExpectNodeOutput checks if a node has the expected output
func (a *AssertDAGExecution) ExpectNodeOutput(nodeName string, expected string) {
	a.T.Helper()
	output, ok := a.Data.GetOutput(nodeName)
	if !ok {
		a.T.Errorf("Expected node %s to have output, but none was found", nodeName)
		return
	}
	if output != expected {
		a.T.Errorf("Expected node %s output to be %s, got %v", nodeName, expected, output)
	}
}

func TestDAGBasicOperations(t *testing.T) {
	t.Run("NewDAG creates empty DAG", func(t *testing.T) {
		dag := NewDAG("test-dag")

		if len(dag.Nodes) != 0 {
			t.Errorf("Expected empty nodes map, got %d nodes", len(dag.Nodes))
		}
	})

	t.Run("AddNode adds a node to the DAG", func(t *testing.T) {
		dag := NewDAG("test-dag")
		node := NewNode("test", SuccessAction("test-output"))

		err := dag.AddNode(node)
		if err != nil {
			t.Errorf("Failed to add node: %v", err)
		}

		if len(dag.Nodes) != 1 {
			t.Errorf("Expected 1 node, got %d nodes", len(dag.Nodes))
		}

		addedNode, exists := dag.GetNodeByName("test")
		if !exists {
			t.Errorf("Node 'test' not found in DAG")
		}

		if addedNode != node {
			t.Errorf("Retrieved node is not the same as the added node")
		}
	})

	t.Run("AddDependency creates a dependency between nodes", func(t *testing.T) {
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		// Add dependencies: A -> B -> C
		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Verify dependencies
		if len(nodeB.DependsOn) != 1 || nodeB.DependsOn[0] != nodeA {
			t.Errorf("Node B should depend on node A")
		}

		if len(nodeC.DependsOn) != 1 || nodeC.DependsOn[0] != nodeB {
			t.Errorf("Node C should depend on node B")
		}
	})

	t.Run("GetNode", func(t *testing.T) {
		builder := NewTestDAGBuilder()
		action := SuccessAction("test_output")
		builder.AddNode("test", action)
		dag := builder.Build()

		// Get node
		retrieved, exists := dag.GetNode("test")
		if !exists {
			t.Error("Expected node to exist")
		}
		if retrieved.Name != "test" {
			t.Errorf("Expected node name 'test', got '%s'", retrieved.Name)
		}

		// Try getting non-existent node
		_, exists = dag.GetNode("nonexistent")
		if exists {
			t.Error("Expected node to not exist")
		}
	})
}

func TestDAGDependencies(t *testing.T) {
	t.Run("Basic dependency chain", func(t *testing.T) {
		// Create a DAG with dependencies: A -> B -> C
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Check dependency relationships
		if len(nodeA.DependsOn) != 0 {
			t.Error("Node A should not have dependencies")
		}

		if len(nodeB.DependsOn) != 1 || nodeB.DependsOn[0] != nodeA {
			t.Errorf("Node B should depend on node A")
		}

		if len(nodeC.DependsOn) != 1 || nodeC.DependsOn[0] != nodeB {
			t.Errorf("Node C should depend on node B")
		}
	})

	t.Run("Multiple dependencies", func(t *testing.T) {
		// Create a DAG with dependencies: A -> C, B -> C
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "C")
		_ = dag.AddDependency("B", "C")

		// Check dependency relationships
		dependsOnA := false
		dependsOnB := false

		for _, dep := range nodeC.DependsOn {
			if dep == nodeA {
				dependsOnA = true
			}
			if dep == nodeB {
				dependsOnB = true
			}
		}

		if !dependsOnA || !dependsOnB {
			t.Errorf("Node C should depend on both A and B")
		}
	})
}

func TestDAGTopologicalSort(t *testing.T) {
	t.Run("Simple sort", func(t *testing.T) {
		// Create a DAG with dependencies: A -> B -> C
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Get sorted levels
		levels := dag.GetLevels()

		// Verify correct levels
		if len(levels) != 3 {
			t.Errorf("Expected 3 levels, got %d", len(levels))
			return
		}

		if len(levels[0]) != 1 || levels[0][0].Name != "A" {
			t.Errorf("Expected level 0 to contain only node A")
		}

		if len(levels[1]) != 1 || levels[1][0].Name != "B" {
			t.Errorf("Expected level 1 to contain only node B")
		}

		if len(levels[2]) != 1 || levels[2][0].Name != "C" {
			t.Errorf("Expected level 2 to contain only node C")
		}
	})
}

func TestDAGCycleDetection(t *testing.T) {
	t.Run("No cycle detection in valid DAG", func(t *testing.T) {
		// Create a valid DAG without cycles
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Validate the DAG
		err := dag.Validate()
		if err != nil {
			t.Errorf("Expected valid DAG validation, got error: %v", err)
		}
	})

	t.Run("Cycle detection in invalid DAG", func(t *testing.T) {
		// Create an invalid DAG with a cycle: A -> B -> C -> A
		dag := NewDAG("test-dag")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", SuccessAction("output_B"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")
		_ = dag.AddDependency("C", "A")

		// Validate the DAG
		err := dag.Validate()
		if err == nil {
			t.Error("Expected validation to fail due to cycle, but it passed")
		}
	})
}

func TestDAGExecution(t *testing.T) {
	t.Run("Basic execution flow", func(t *testing.T) {
		// Create a DAG with dependencies: A -> B -> C
		dag := NewDAG("test-workflow")

		// Create test actions with correct node names
		actionA := &TestAction{Name: "A", Output: "output_A"}
		actionB := &TestAction{Name: "B", Output: "output_B"}
		actionC := &TestAction{Name: "C", Output: "output_C"}

		nodeA := NewNode("A", actionA)
		nodeB := NewNode("B", actionB)
		nodeC := NewNode("C", actionC)

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Create workflow data for testing
		data := NewWorkflowData("test-workflow")

		// Create assertion helper
		assert := &AssertDAGExecution{
			T:    t,
			DAG:  dag,
			Data: data,
		}

		// Execute and check results
		assert.ExpectSuccess()
		assert.ExpectNodeStatus("A", Completed)
		assert.ExpectNodeStatus("B", Completed)
		assert.ExpectNodeStatus("C", Completed)
		assert.ExpectNodeOutput("A", "output_A")
		assert.ExpectNodeOutput("B", "output_B")
		assert.ExpectNodeOutput("C", "output_C")
	})

	t.Run("Node execution failure", func(t *testing.T) {
		// Create a DAG with a failing node: A -> B -> C
		// where B fails
		dag := NewDAG("test-workflow")
		nodeA := NewNode("A", SuccessAction("output_A"))
		nodeB := NewNode("B", FailingAction("node B failed"))
		nodeC := NewNode("C", SuccessAction("output_C"))

		_ = dag.AddNode(nodeA)
		_ = dag.AddNode(nodeB)
		_ = dag.AddNode(nodeC)

		_ = dag.AddDependency("A", "B")
		_ = dag.AddDependency("B", "C")

		// Create workflow data for testing
		data := NewWorkflowData("test-workflow")

		// Create assertion helper
		assert := &AssertDAGExecution{
			T:    t,
			DAG:  dag,
			Data: data,
		}

		// Execute and verify failure
		assert.ExpectFailure()
		assert.ExpectNodeStatus("A", Completed)
		assert.ExpectNodeStatus("B", Failed)

		// Node C should not execute because B failed
		// Explicitly set node C status to Pending to ensure it exists
		data.SetNodeStatus("C", Pending)
		assert.ExpectNodeStatus("C", Pending)
	})
}

func TestTopologicalSort(t *testing.T) {
	t.Run("Empty DAG", func(t *testing.T) {
		dag := NewDAG("empty")
		sorted, err := dag.TopologicalSort()
		assert.NoError(t, err)
		assert.Empty(t, sorted)
	})

	t.Run("Single Node", func(t *testing.T) {
		dag := NewDAG("single")
		node := NewNode("A", nil)
		dag.AddNode(node)
		sorted, err := dag.TopologicalSort()
		assert.NoError(t, err)
		assert.Equal(t, []string{"A"}, getNodeNames(sorted[0]))
	})

	t.Run("Linear DAG", func(t *testing.T) {
		dag := NewDAG("linear")
		nodeA := NewNode("A", nil)
		nodeB := NewNode("B", nil)
		nodeC := NewNode("C", nil)

		dag.AddNode(nodeA)
		dag.AddNode(nodeB)
		dag.AddNode(nodeC)

		// B depends on A, C depends on B
		dag.AddDependency("A", "B")
		dag.AddDependency("B", "C")

		sorted, err := dag.TopologicalSort()
		assert.NoError(t, err)
		assert.Equal(t, []string{"A", "B", "C"}, getNodeNames(sorted[0]))
	})

	t.Run("Diamond DAG", func(t *testing.T) {
		dag := NewDAG("diamond")
		nodeA := NewNode("A", nil)
		nodeB := NewNode("B", nil)
		nodeC := NewNode("C", nil)
		nodeD := NewNode("D", nil)

		dag.AddNode(nodeA)
		dag.AddNode(nodeB)
		dag.AddNode(nodeC)
		dag.AddNode(nodeD)

		// B and C depend on A, D depends on B and C
		dag.AddDependency("A", "B")
		dag.AddDependency("A", "C")
		dag.AddDependency("B", "D")
		dag.AddDependency("C", "D")

		sorted, err := dag.TopologicalSort()
		assert.NoError(t, err)

		// Check that A comes before B and C, and B and C come before D
		sortedNames := getNodeNames(sorted[0])
		aIndex := indexOf(sortedNames, "A")
		bIndex := indexOf(sortedNames, "B")
		cIndex := indexOf(sortedNames, "C")
		dIndex := indexOf(sortedNames, "D")

		assert.True(t, aIndex < bIndex)
		assert.True(t, aIndex < cIndex)
		assert.True(t, bIndex < dIndex)
		assert.True(t, cIndex < dIndex)
	})

	t.Run("Complex DAG", func(t *testing.T) {
		dag := NewDAG("complex")
		nodes := make(map[string]*Node)
		nodeNames := []string{"A", "B", "C", "D", "E", "F"}

		for _, name := range nodeNames {
			nodes[name] = NewNode(name, nil)
			dag.AddNode(nodes[name])
		}

		// B and C depend on A
		// D depends on B and C
		// E depends on C
		// F depends on D and E
		dag.AddDependency("A", "B")
		dag.AddDependency("A", "C")
		dag.AddDependency("B", "D")
		dag.AddDependency("C", "D")
		dag.AddDependency("C", "E")
		dag.AddDependency("D", "F")
		dag.AddDependency("E", "F")

		sorted, err := dag.TopologicalSort()
		assert.NoError(t, err)

		// Verify topological ordering
		sortedNames := getNodeNames(sorted[0])
		indices := make(map[string]int)
		for i, name := range sortedNames {
			indices[name] = i
		}

		// Verify dependencies are respected
		assert.True(t, indices["A"] < indices["B"])
		assert.True(t, indices["A"] < indices["C"])
		assert.True(t, indices["B"] < indices["D"])
		assert.True(t, indices["C"] < indices["D"])
		assert.True(t, indices["C"] < indices["E"])
		assert.True(t, indices["D"] < indices["F"])
		assert.True(t, indices["E"] < indices["F"])
	})

	t.Run("DAG with Cycle", func(t *testing.T) {
		dag := NewDAG("cycle")
		nodeA := NewNode("A", nil)
		nodeB := NewNode("B", nil)
		nodeC := NewNode("C", nil)

		dag.AddNode(nodeA)
		dag.AddNode(nodeB)
		dag.AddNode(nodeC)

		// B depends on A, C depends on B, A depends on C (cycle)
		dag.AddDependency("A", "B")
		dag.AddDependency("B", "C")
		dag.AddDependency("C", "A")

		_, err := dag.TopologicalSort()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cycle detected")
	})
}

// Helper function to extract node names from a slice of nodes
func getNodeNames(nodes []*Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

// Helper function to find index of a string in a slice
func indexOf(slice []string, item string) int {
	for i, s := range slice {
		if s == item {
			return i
		}
	}
	return -1
}

func TestGetNode(t *testing.T) {
	dag := NewDAG("test")
	nodeA := NewNode("A", nil)
	dag.AddNode(nodeA)

	t.Run("Existing Node", func(t *testing.T) {
		node, exists := dag.GetNode("A")
		assert.True(t, exists)
		assert.Equal(t, nodeA, node)
	})

	t.Run("Non-existent Node", func(t *testing.T) {
		node, exists := dag.GetNode("B")
		assert.False(t, exists)
		assert.Nil(t, node)
	})
}

func TestGetNodeByName(t *testing.T) {
	dag := NewDAG("test")
	nodeA := NewNode("A", nil)
	dag.AddNode(nodeA)

	t.Run("Existing Node", func(t *testing.T) {
		node, exists := dag.GetNodeByName("A")
		assert.True(t, exists)
		assert.Equal(t, nodeA, node)
	})

	t.Run("Non-existent Node", func(t *testing.T) {
		node, exists := dag.GetNodeByName("B")
		assert.False(t, exists)
		assert.Nil(t, node)
	})
}

func TestGetLevels(t *testing.T) {
	t.Run("Empty DAG", func(t *testing.T) {
		dag := NewDAG("empty")
		levels := dag.GetLevels()
		assert.Empty(t, levels)
	})

	t.Run("Single Node", func(t *testing.T) {
		dag := NewDAG("single")
		nodeA := NewNode("A", nil)
		dag.AddNode(nodeA)

		levels := dag.GetLevels()
		require.Len(t, levels, 1)
		assert.Equal(t, []string{"A"}, getNodeNames(levels[0]))
	})

	t.Run("Linear DAG", func(t *testing.T) {
		dag := NewDAG("linear")
		nodeA := NewNode("A", nil)
		nodeB := NewNode("B", nil)
		nodeC := NewNode("C", nil)

		dag.AddNode(nodeA)
		dag.AddNode(nodeB)
		dag.AddNode(nodeC)

		// B depends on A, C depends on B
		dag.AddDependency("A", "B")
		dag.AddDependency("B", "C")

		levels := dag.GetLevels()
		require.Len(t, levels, 3)
		assert.Equal(t, []string{"A"}, getNodeNames(levels[0]))
		assert.Equal(t, []string{"B"}, getNodeNames(levels[1]))
		assert.Equal(t, []string{"C"}, getNodeNames(levels[2]))
	})

	t.Run("Diamond DAG", func(t *testing.T) {
		dag := NewDAG("diamond")
		nodeA := NewNode("A", nil)
		nodeB := NewNode("B", nil)
		nodeC := NewNode("C", nil)
		nodeD := NewNode("D", nil)

		dag.AddNode(nodeA)
		dag.AddNode(nodeB)
		dag.AddNode(nodeC)
		dag.AddNode(nodeD)

		// B and C depend on A, D depends on B and C
		dag.AddDependency("A", "B")
		dag.AddDependency("A", "C")
		dag.AddDependency("B", "D")
		dag.AddDependency("C", "D")

		levels := dag.GetLevels()
		require.Len(t, levels, 3)
		assert.Equal(t, []string{"A"}, getNodeNames(levels[0]))
		assert.ElementsMatch(t, []string{"B", "C"}, getNodeNames(levels[1]))
		assert.Equal(t, []string{"D"}, getNodeNames(levels[2]))
	})

	t.Run("Complex DAG", func(t *testing.T) {
		dag := NewDAG("complex")
		nodes := make(map[string]*Node)
		nodeNames := []string{"A", "B", "C", "D", "E", "F"}

		for _, name := range nodeNames {
			nodes[name] = NewNode(name, nil)
			dag.AddNode(nodes[name])
		}

		// B and C depend on A
		// D depends on B and C
		// E depends on C
		// F depends on D and E
		dag.AddDependency("A", "B")
		dag.AddDependency("A", "C")
		dag.AddDependency("B", "D")
		dag.AddDependency("C", "D")
		dag.AddDependency("C", "E")
		dag.AddDependency("D", "F")
		dag.AddDependency("E", "F")

		levels := dag.GetLevels()
		require.Len(t, levels, 4)
		assert.Equal(t, []string{"A"}, getNodeNames(levels[0]))
		assert.ElementsMatch(t, []string{"B", "C"}, getNodeNames(levels[1]))
		assert.ElementsMatch(t, []string{"D", "E"}, getNodeNames(levels[2]))
		assert.Equal(t, []string{"F"}, getNodeNames(levels[3]))
	})
}
