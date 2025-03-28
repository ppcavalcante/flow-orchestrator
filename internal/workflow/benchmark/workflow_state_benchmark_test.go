package benchmark

import (
	"context"
	"fmt"
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// BenchmarkWorkflowStateManagement measures performance of workflow state operations
func BenchmarkWorkflowStateManagement(b *testing.B) {
	// Create a sample workflow data with different numbers of nodes and status
	for _, size := range []int{10, 100, 1000} {
		// First, test simple state operations
		b.Run(fmt.Sprintf("StateOperations_%d", size), func(b *testing.B) {
			// Setup test data
			data := workflow.NewWorkflowData(fmt.Sprintf("state-bench-%d", size))

			// Generate node names
			nodeNames := make([]string, size)
			for i := 0; i < size; i++ {
				nodeNames[i] = fmt.Sprintf("node%d", i)
			}

			// Benchmark setting node status
			b.Run("SetNodeStatus", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					nodeIdx := i % size
					data.SetNodeStatus(nodeNames[nodeIdx], workflow.Running)
				}
			})

			// Benchmark getting node status
			b.Run("GetNodeStatus", func(b *testing.B) {
				// First set statuses
				for i := 0; i < size; i++ {
					if i%2 == 0 {
						data.SetNodeStatus(nodeNames[i], workflow.Completed)
					} else {
						data.SetNodeStatus(nodeNames[i], workflow.Failed)
					}
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					nodeIdx := i % size
					_, _ = data.GetNodeStatus(nodeNames[nodeIdx])
				}
			})

			// Benchmark setting and getting node outputs
			b.Run("NodeOutputs", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					nodeIdx := i % size

					// Alternate between set and get
					if i%2 == 0 {
						data.SetOutput(nodeNames[nodeIdx], fmt.Sprintf("output-%d", i))
					} else {
						_, _ = data.GetOutput(nodeNames[nodeIdx])
					}
				}
			})

			// Benchmark setting and getting custom data
			b.Run("CustomData", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("custom-%d", i%size)

					// Alternate between set and get
					if i%2 == 0 {
						data.Set(key, fmt.Sprintf("value-%d", i))
					} else {
						_, _ = data.Get(key)
					}
				}
			})
		})
	}
}

// BenchmarkDAGConstructionPerformance measures the performance of DAG construction
func BenchmarkDAGConstructionPerformance(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("DAGConstruction_%d_Nodes", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				nodeNames := make([]string, size)
				for j := 0; j < size; j++ {
					nodeNames[j] = fmt.Sprintf("node%d", j)
				}

				// Simple action for all nodes
				action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
					return nil
				})

				b.StartTimer()

				// Create DAG and add nodes
				dag := workflow.NewDAG(fmt.Sprintf("dag-construction-bench-%d", size))
				for j := 0; j < size; j++ {
					node := workflow.NewNode(nodeNames[j], action)
					dag.AddNode(node)
				}

				// Add some dependencies (linear chain for simplicity)
				if size > 1 {
					for j := 0; j < size-1; j++ {
						dag.AddDependency(nodeNames[j], nodeNames[j+1])
					}
				}
			}
		})
	}

	// Benchmark the performance of DAG topologies
	b.Run("DAGTopologyCreation", func(b *testing.B) {
		size := 100 // Fixed size for all topologies

		// Test creating linear DAGs
		b.Run("LinearDAG", func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = createLinearDAG(size)
			}
		})

		// Test creating diamond DAGs
		b.Run("DiamondDAG", func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = createDiamondDAG(size)
			}
		})

		// Test creating binary tree DAGs
		b.Run("BinaryTreeDAG", func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = createBinaryTreeDAG(size)
			}
		})

		// Test creating random DAGs
		b.Run("RandomDAG", func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = createRandomDAG(size, 0.3)
			}
		})
	})
}
