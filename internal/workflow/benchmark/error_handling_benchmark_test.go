package benchmark

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// BenchmarkErrorHandling benchmarks the performance of error handling mechanisms
func BenchmarkErrorHandling(b *testing.B) {
	// Benchmark retry middleware performance
	b.Run("RetryPerformance", func(b *testing.B) {
		// Use smaller retry counts to avoid timeouts
		retryCounts := []int{0, 1, 2, 3}
		for _, retryCount := range retryCounts {
			b.Run(fmt.Sprintf("Retries=%d", retryCount), func(b *testing.B) {
				benchmarkRetryPerformance(b, retryCount)
			})
		}
	})

	// Benchmark recovery pattern after node failure
	b.Run("RecoveryAfterFailure", func(b *testing.B) {
		// Use smaller node failure counts
		nodeFailureCounts := []int{1, 2, 5}
		for _, failureCount := range nodeFailureCounts {
			b.Run(fmt.Sprintf("FailedNodes=%d", failureCount), func(b *testing.B) {
				benchmarkRecoveryAfterFailure(b, failureCount)
			})
		}
	})

	// Benchmark panic recovery with different middleware configurations
	b.Run("PanicRecovery", func(b *testing.B) {
		benchmarkPanicRecovery(b)
	})
}

// Benchmark retry middleware with various retry configurations
func benchmarkRetryPerformance(b *testing.B, retryCount int) {
	// Reset timer for setup
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		// Create a workflow with a single node that fails and retries
		builder := workflow.NewWorkflowBuilder().
			WithWorkflowID(fmt.Sprintf("retry-benchmark-%d", i))

		// Create a failing action that will succeed after the specified number of attempts
		// Note: Action succeeds on attempt (retryCount), so we need to adjust the retryCount
		// for the middleware to allow exactly that many retries
		baseAction := createFastRetryAction(retryCount)

		// Wrap with no-delay retry middleware - use non-verbose mode (false) for benchmarks
		middleware := workflow.NoDelayRetryMiddleware(retryCount, false)
		wrappedAction := middleware(baseAction)

		// Add the action to the workflow
		builder.AddStartNode("retry-node").
			WithAction(wrappedAction)

		dag, err := builder.Build()
		if err != nil {
			b.Fatalf("Failed to build workflow: %v", err)
		}

		data := workflow.NewWorkflowData(fmt.Sprintf("retry-data-%d", i))
		ctx := context.Background()

		b.StartTimer()
		err = dag.Execute(ctx, data)
		if err != nil {
			b.Fatalf("Expected successful execution with %d retries, got error: %v", retryCount, err)
		}
	}
}

// Create an action that will fail until the retry count is reached, but doesn't sleep
func createFastRetryAction(succeededAfterRetries int) workflow.Action {
	var attemptCount int
	var mu sync.Mutex

	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		mu.Lock()
		attempt := attemptCount
		attemptCount++
		mu.Unlock()

		// If succeededAfterRetries is 0, always succeed
		// Otherwise, succeed when attempt equals or exceeds succeededAfterRetries
		if succeededAfterRetries > 0 && attempt < succeededAfterRetries {
			return errors.New("simulated fast transient error")
		}
		return nil
	})
}

// Create an action that will fail until the retry count is reached (original version)
func createRetryAction(succeededAfterRetries int) workflow.Action {
	var attemptCount int
	var mu sync.Mutex

	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		mu.Lock()
		attempt := attemptCount
		attemptCount++
		mu.Unlock()

		if attempt < succeededAfterRetries {
			return errors.New("simulated transient error")
		}
		return nil
	})
}

// Benchmark recovery after node failures in a DAG
func benchmarkRecoveryAfterFailure(b *testing.B, failureCount int) {
	const nodeCount = 20 // Reduced from 50 to make the benchmark faster

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		// Create a DAG with some failing nodes
		dag, failedNodes := createDAGWithFailingNodes(nodeCount, failureCount)
		data := workflow.NewWorkflowData(fmt.Sprintf("recovery-data-%d", i))

		// First execution - will fail
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_ = dag.Execute(ctx, data) // Some nodes will fail
		cancel()

		// Fix the failed nodes for recovery (mark them as ready to run again)
		for _, nodeName := range failedNodes {
			data.SetNodeStatus(nodeName, workflow.Pending)
		}

		// Second execution - benchmark recovery performance
		ctx = context.Background()
		b.StartTimer()
		_ = dag.Execute(ctx, data)
	}
}

// Create a DAG with a specific number of failing nodes
func createDAGWithFailingNodes(totalNodes, failingNodes int) (*workflow.DAG, []string) {
	dag := workflow.NewDAG("recovery-dag")
	failedNodeNames := make([]string, 0, failingNodes)

	// Create all nodes
	for i := 0; i < totalNodes; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		var action workflow.Action

		// Make some nodes fail
		if i < failingNodes {
			action = workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
				return errors.New("simulated failure")
			})
			failedNodeNames = append(failedNodeNames, nodeName)
		} else {
			action = createTestAction()
		}

		node := workflow.NewNode(nodeName, action)
		dag.AddNode(node)

		// Add dependencies - make a linear chain for simplicity
		if i > 0 {
			prevNodeName := fmt.Sprintf("node-%d", i-1)
			dag.AddDependency(prevNodeName, nodeName)
		}
	}

	return dag, failedNodeNames
}

// Benchmark panic recovery with middleware
func benchmarkPanicRecovery(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		// Create a workflow with nodes that panic
		builder := workflow.NewWorkflowBuilder().
			WithWorkflowID(fmt.Sprintf("panic-benchmark-%d", i))

		// Create recovery middleware
		recoveryMiddleware := createRecoveryMiddleware()

		// Create a node that panics
		panicAction := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			panic("simulated panic")
		})

		// Wrap panic action with recovery middleware
		wrappedAction := recoveryMiddleware(panicAction)

		// Add to workflow
		builder.AddStartNode("panic-node").
			WithAction(wrappedAction)

		dag, err := builder.Build()
		if err != nil {
			b.Fatalf("Failed to build workflow: %v", err)
		}

		data := workflow.NewWorkflowData(fmt.Sprintf("panic-data-%d", i))
		ctx := context.Background()

		b.StartTimer()
		_ = dag.Execute(ctx, data) // Should not actually panic
	}
}

// Create a middleware that recovers from panics
func createRecoveryMiddleware() workflow.Middleware {
	return func(next workflow.Action) workflow.Action {
		return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("recovered from panic: %v", r)
				}
			}()
			return next.Execute(ctx, data)
		})
	}
}
