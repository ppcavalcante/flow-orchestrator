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

// M23 SEAL-06: the three topology helpers below assemble their graphs through the
// SANCTIONED builder API. They previously used workflow.NewDAG + NewNode + AddNode +
// AddDependency, all of which T6 unexported — this package is `package benchmark`,
// i.e. OUT of package workflow, so the census's "nearly every test file is in-package, so
// unexporting is a rename" mitigation never covered it (F-117-ARCH-03).
//
// EDGE DIRECTION IS INVERTED, DELIBERATELY, AND IT IS THE ONLY PLACE A SILENT ERROR
// COULD HIDE HERE. dag.AddDependency(from, to) means "to depends on from" — it is
// written parent-first. The builder is written child-first: AddNode(child).DependsOn(
// parent). Every loop below therefore enumerates each node's PARENTS where the old
// code enumerated each node's CHILDREN. A transcription that kept the old direction
// would still build a valid DAG of the same node count and would still execute
// cleanly; it would simply be the reverse topology, and no benchmark asserts shape.
// TestBenchTopologies_ShapeIsPreserved pins level-by-level shape for exactly that
// reason.
//
// These now run build(), i.e. full Validate + validateReconvergence. Callers must
// construct OUTSIDE the timed region — see the note on BenchmarkFocusedArena.

// createLinearDAG creates a linear DAG with the specified number of nodes
// nolint:unused
func createLinearDAG(size int) *workflow.DAG {
	b := workflow.NewWorkflowBuilder().WithWorkflowID("linear")
	for i := 0; i < size; i++ {
		nb := b.AddNode(fmt.Sprintf("node%d", i)).WithAction(CreateNoOpAction())
		if i > 0 {
			nb.DependsOn(fmt.Sprintf("node%d", i-1))
		}
	}
	return mustBuild(b)
}

// createDiamondDAG creates a diamond-shaped DAG with the specified number of nodes
// nolint:unused
func createDiamondDAG(size int) *workflow.DAG {
	// The original shape: node0 is the source, nodes 1..middle are the fan-out band,
	// and node(1+middle) is the sink that fans back in.
	const firstLevelSize = 1
	middleLevelSize := size - 2

	b := workflow.NewWorkflowBuilder().WithWorkflowID("diamond")
	for i := 0; i < size; i++ {
		name := fmt.Sprintf("node%d", i)
		nb := b.AddNode(name).WithAction(CreateNoOpAction())
		switch {
		case i == 0:
			// the source
		case i < firstLevelSize+middleLevelSize:
			nb.DependsOn("node0")
		case i == firstLevelSize+middleLevelSize:
			for j := 0; j < middleLevelSize; j++ {
				nb.DependsOn(fmt.Sprintf("node%d", firstLevelSize+j))
			}
		}
	}
	return mustBuild(b)
}

// createBinaryTreeDAG creates a binary tree DAG with the specified number of nodes
// nolint:unused
func createBinaryTreeDAG(size int) *workflow.DAG {
	// Child-first restatement of "node i has children 2i+1 and 2i+2": node j depends
	// on its parent (j-1)/2, for every j > 0.
	b := workflow.NewWorkflowBuilder().WithWorkflowID("binary-tree")
	for i := 0; i < size; i++ {
		nb := b.AddNode(fmt.Sprintf("node%d", i)).WithAction(CreateNoOpAction())
		if i > 0 {
			nb.DependsOn(fmt.Sprintf("node%d", (i-1)/2))
		}
	}
	return mustBuild(b)
}

// mustBuild panics on a build error, matching what the AddNode/AddDependency loops
// these helpers replaced already did.
// nolint:unused
func mustBuild(b *workflow.WorkflowBuilder) *workflow.DAG {
	dag, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("failed to build benchmark DAG: %v", err))
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
