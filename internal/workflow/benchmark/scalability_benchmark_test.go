package benchmark

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// WorkerCounts defines the worker counts to test
var WorkerCounts = []int{1, 2, 4, 8, 16, 32}

// BenchmarkScalability tests the scalability of the workflow system
func BenchmarkScalability(b *testing.B) {
	// Test with different node counts
	nodeCounts := []int{10, 100, 1000}

	for _, nodeCount := range nodeCounts {
		b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
			benchmarkNodeScaling(b, nodeCount)
		})
	}

	// Test with different concurrency levels
	b.Run("ConcurrencyScaling", func(b *testing.B) {
		for _, workerCount := range WorkerCounts {
			b.Run(fmt.Sprintf("Workers=%d", workerCount), func(b *testing.B) {
				benchmarkConcurrencyScaling(b, workerCount)
			})
		}
	})
}

// benchmarkNodeScaling tests how the system scales with increasing node counts
func benchmarkNodeScaling(b *testing.B, nodeCount int) {
	// Create a DAG with the specified number of nodes
	dag := workflow.NewDAG(fmt.Sprintf("scaling-test-%d", nodeCount))

	// Create nodes
	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate some work
			time.Sleep(10 * time.Microsecond)
			return nil
		})
		node := workflow.NewNode(nodeName, action)
		mustAddNode(dag, node)
	}

	// Create dependencies (simple linear chain)
	for i := 1; i < nodeCount; i++ {
		mustAddDep(dag, fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", i-1))
	}

	// Apply a custom execution config and run the real wired execution path.
	dag.WithExecutionConfig(workflow.ExecutionConfig{
		MaxConcurrency: 4,
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Create workflow data
		data := workflow.NewWorkflowData(fmt.Sprintf("scaling-test-%d", i))

		// Execute the DAG through the real path (DAG.Execute -> level scheduling
		// bounded by the configured concurrency).
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := dag.Execute(ctx, data)
		cancel()

		if err != nil {
			b.Fatalf("DAG execution failed: %v", err)
		}
	}
}

// benchmarkConcurrencyScaling tests how the system scales with increasing concurrency
func benchmarkConcurrencyScaling(b *testing.B, workerCount int) {
	// Create a DAG with a fixed number of nodes
	nodeCount := 100
	dag := workflow.NewDAG(fmt.Sprintf("concurrency-test-%d", workerCount))

	// Create nodes
	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate some work
			time.Sleep(10 * time.Microsecond)
			return nil
		})
		node := workflow.NewNode(nodeName, action)
		mustAddNode(dag, node)
	}

	// Create a star topology (all nodes depend on node0)
	for i := 1; i < nodeCount; i++ {
		mustAddDep(dag, fmt.Sprintf("node%d", i), "node0")
	}

	// Apply the worker-count concurrency limit and run the real wired path.
	dag.WithExecutionConfig(workflow.ExecutionConfig{
		MaxConcurrency: workerCount,
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Create workflow data
		data := workflow.NewWorkflowData(fmt.Sprintf("concurrency-test-%d", i))

		// Execute the DAG through the real path. node0 is the star root; once it
		// completes, the remaining nodes run in parallel bounded by MaxConcurrency.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := dag.Execute(ctx, data)
		cancel()

		if err != nil {
			b.Fatalf("DAG execution failed: %v", err)
		}
	}
}
