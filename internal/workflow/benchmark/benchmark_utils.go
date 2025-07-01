package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// BenchConfig holds common benchmark configuration parameters
type BenchConfig struct {
	Sizes        []int
	WorkerCounts []int
	Timeout      time.Duration
}

// DefaultBenchConfig returns default benchmark configuration
func DefaultBenchConfig() *BenchConfig {
	return &BenchConfig{
		Sizes:        []int{10, 100, 1000},
		WorkerCounts: []int{1, 2, 4, 8, 16},
		Timeout:      1 * time.Second,
	}
}

// CreateNoOpAction creates a no-op action for benchmarking
func CreateNoOpAction() workflow.Action {
	return workflow.ActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
		return nil
	})
}

// createTestAction creates a simple test action for benchmarking
// nolint:unused
func createTestAction() workflow.Action {
	return workflow.ActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set("test", "value")
		return nil
	})
}

// NewFileStore is a temporary wrapper to create a store for benchmarking
// This will be removed once the main codebase exposes appropriate store creation methods
func NewFileStore(dir string, format string) workflow.WorkflowStore {
	// Create a simple implementation of WorkflowStore for testing
	return &testStore{dir: dir, format: format}
}

// testStore implements WorkflowStore interface for benchmarking
type testStore struct {
	dir    string
	format string
}

// Save implements the WorkflowStore interface Save method
func (s *testStore) Save(_ *workflow.WorkflowData) error {
	return nil
}

// Load implements WorkflowStore.Load
func (s *testStore) Load(id string) (*workflow.WorkflowData, error) {
	// For benchmarking, just return a dummy workflow data
	data := workflow.NewWorkflowData(id)
	time.Sleep(1 * time.Microsecond)
	return data, nil
}

// Delete implements the WorkflowStore interface Delete method
func (s *testStore) Delete(_ string) error {
	return nil
}

// ListWorkflows implements WorkflowStore.ListWorkflows
func (s *testStore) ListWorkflows() ([]string, error) {
	// For benchmarking, just return some dummy IDs
	return []string{"workflow1", "workflow2", "workflow3"}, nil
}

// createLinearDAG creates a linear DAG with the specified number of nodes
// nolint:unused
func createLinearDAG(size int) *workflow.DAG {
	dag := workflow.NewDAG("linear")

	// Create nodes
	for i := 0; i < size; i++ {
		node := workflow.NewNode(fmt.Sprintf("node%d", i), CreateNoOpAction())
		if err := dag.AddNode(node); err != nil {
			panic(fmt.Sprintf("Failed to add node: %v", err))
		}
	}

	// Add dependencies to create a linear chain
	for i := 0; i < size-1; i++ {
		if err := dag.AddDependency(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", i+1)); err != nil {
			panic(fmt.Sprintf("Failed to add dependency: %v", err))
		}
	}

	return dag
}

// createDiamondDAG creates a diamond-shaped DAG with the specified number of nodes
// nolint:unused
func createDiamondDAG(size int) *workflow.DAG {
	dag := workflow.NewDAG("diamond")

	// Calculate sizes for each level to distribute nodes
	firstLevelSize := 1
	middleLevelSize := size - 2

	// Create nodes
	for i := 0; i < size; i++ {
		node := workflow.NewNode(fmt.Sprintf("node%d", i), CreateNoOpAction())
		if err := dag.AddNode(node); err != nil {
			panic(fmt.Sprintf("Failed to add node: %v", err))
		}
	}

	// Connect first level to middle level (fan out)
	for j := 0; j < middleLevelSize; j++ {
		if err := dag.AddDependency(
			fmt.Sprintf("node%d", 0),
			fmt.Sprintf("node%d", firstLevelSize+j),
		); err != nil {
			panic(fmt.Sprintf("Failed to add dependency: %v", err))
		}
	}

	// Connect middle level to last level (fan in)
	for i := 0; i < middleLevelSize; i++ {
		if err := dag.AddDependency(
			fmt.Sprintf("node%d", firstLevelSize+i),
			fmt.Sprintf("node%d", firstLevelSize+middleLevelSize),
		); err != nil {
			panic(fmt.Sprintf("Failed to add dependency: %v", err))
		}
	}

	return dag
}

// createBinaryTreeDAG creates a binary tree DAG with the specified number of nodes
// nolint:unused
func createBinaryTreeDAG(size int) *workflow.DAG {
	dag := workflow.NewDAG("binary-tree")

	// Create nodes
	for i := 0; i < size; i++ {
		node := workflow.NewNode(fmt.Sprintf("node%d", i), CreateNoOpAction())
		if err := dag.AddNode(node); err != nil {
			panic(fmt.Sprintf("Failed to add node: %v", err))
		}
	}

	// Add dependencies to form a binary tree
	// Each node i has children 2i+1 and 2i+2 (if they exist)
	for i := 0; i < size; i++ {
		leftChild := 2*i + 1
		if leftChild < size {
			if err := dag.AddDependency(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", leftChild)); err != nil {
				panic(fmt.Sprintf("Failed to add dependency: %v", err))
			}
		}

		rightChild := 2*i + 2
		if rightChild < size {
			if err := dag.AddDependency(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", rightChild)); err != nil {
				panic(fmt.Sprintf("Failed to add dependency: %v", err))
			}
		}
	}

	return dag
}

// createBenchmarkWorkflowData creates a workflow data instance for benchmarking
// nolint:unused
func createBenchmarkWorkflowData(size int) *workflow.WorkflowData {
	data := workflow.NewWorkflowData(fmt.Sprintf("benchmark-%d", size))
	return data
}

// createBenchmarkStores creates benchmark stores and returns a cleanup function
// nolint:unused
func createBenchmarkStores() (workflow.WorkflowStore, workflow.WorkflowStore, func()) {
	// Create temporary directories for testing
	tempDir, err := os.MkdirTemp("", "workflow-benchmark")
	if err != nil {
		panic(fmt.Sprintf("Failed to create temp dir: %v", err))
	}

	// Create directories for each store type
	jsonDir := filepath.Join(tempDir, "json")
	fbDir := filepath.Join(tempDir, "fb")

	if err := os.MkdirAll(jsonDir, 0750); err != nil {
		panic(fmt.Sprintf("Failed to create JSON dir: %v", err))
	}

	if err := os.MkdirAll(fbDir, 0750); err != nil {
		panic(fmt.Sprintf("Failed to create FB dir: %v", err))
	}

	jsonStore := NewFileStore(jsonDir, "json")
	fbStore := NewFileStore(fbDir, "flatbuffer")

	// Return a cleanup function
	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			fmt.Printf("Warning: failed to clean up temp dir %s: %v\n", tempDir, err)
		}
	}

	return jsonStore, fbStore, cleanup
}

// runBenchmark runs a benchmark with different sizes
// nolint:unused
func runBenchmark(b *testing.B, name string, sizes []int, fn func(b *testing.B, size int)) {
	b.Helper()
	for _, size := range sizes {
		b.Run(fmt.Sprintf("%s-%d", name, size), func(b *testing.B) {
			fn(b, size)
		})
	}
}
