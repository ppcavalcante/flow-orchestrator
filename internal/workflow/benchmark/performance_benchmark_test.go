package benchmark

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
	"github.com/pparaujo/flow-orchestrator/pkg/workflow/metrics"
)

// BenchmarkParallelExecutionPerformance measures performance of parallel execution
func BenchmarkParallelExecutionPerformance(b *testing.B) {
	config := DefaultBenchConfig()

	// Create different sizes of DAGs with independent nodes for parallel execution
	for _, size := range config.Sizes[:2] { // Only use smaller sizes (10, 100)
		// First, benchmark with various worker counts
		b.Run(fmt.Sprintf("WorkerCount_Size%d", size), func(b *testing.B) {
			for _, workers := range config.WorkerCounts {
				// Test with standard execution
				b.Run(fmt.Sprintf("Standard_Workers_%d", workers), func(b *testing.B) {
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						// Create a DAG with independent nodes for maximum parallelism
						dag := workflow.NewDAG(fmt.Sprintf("parallel-bench-%d-%d", size, workers))

						// Add independent nodes
						for j := 0; j < size; j++ {
							// Create node with small amount of work
							action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
								// Simulate a small unit of work
								time.Sleep(100 * time.Microsecond)
								return nil
							})
							node := workflow.NewNode(fmt.Sprintf("node%d", j), action)
							dag.AddNode(node)
						}

						data := workflow.NewWorkflowData("parallel-bench")

						// Configure execution
						ctx := context.Background()
						b.StartTimer()

						// Execute with default settings (the main DAG Execute method doesn't accept options)
						_ = dag.Execute(ctx, data)
					}
				})

				// Test with optimized execution
				b.Run(fmt.Sprintf("Optimized_Workers_%d", workers), func(b *testing.B) {
					// Create optimized executor
					executorConfig := &Config{
						MaxParallelism:         workers,
						MaxConcurrentWorkflows: 10,
						NodeExecutionTimeout:   30 * time.Second,
						QueueSize:              1000,
						QueueTimeout:           5 * time.Second,
					}
					executor := NewExecutorWithConfig(executorConfig)

					for i := 0; i < b.N; i++ {
						b.StopTimer()
						// Create a DAG with independent nodes for maximum parallelism
						dag := workflow.NewDAG(fmt.Sprintf("parallel-bench-%d-%d", size, workers))

						// Add independent nodes
						for j := 0; j < size; j++ {
							// Create node with small amount of work
							action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
								// Simulate a small unit of work
								time.Sleep(100 * time.Microsecond)
								return nil
							})
							node := workflow.NewNode(fmt.Sprintf("node%d", j), action)
							dag.AddNode(node)
						}

						data := workflow.NewWorkflowData("parallel-bench")

						// Configure execution
						ctx := context.Background()
						b.StartTimer()

						// Execute with optimized executor
						_, _ = executor.Execute(ctx, dag, data)
					}
				})

				// Test with async execution
				b.Run(fmt.Sprintf("Async_Workers_%d", workers), func(b *testing.B) {
					// Create optimized executor
					executorConfig := &Config{
						MaxParallelism:         workers,
						MaxConcurrentWorkflows: 10,
						NodeExecutionTimeout:   30 * time.Second,
						QueueSize:              1000,
						QueueTimeout:           5 * time.Second,
					}
					executor := NewExecutorWithConfig(executorConfig)

					for i := 0; i < b.N; i++ {
						b.StopTimer()
						// Create a DAG with independent nodes for maximum parallelism
						dag := workflow.NewDAG(fmt.Sprintf("parallel-bench-%d-%d", size, workers))

						// Add independent nodes
						for j := 0; j < size; j++ {
							// Create node with small amount of work
							action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
								// Simulate a small unit of work
								time.Sleep(100 * time.Microsecond)
								return nil
							})
							node := workflow.NewNode(fmt.Sprintf("node%d", j), action)
							dag.AddNode(node)
						}

						data := workflow.NewWorkflowData("parallel-bench")

						// Configure execution
						ctx := context.Background()
						b.StartTimer()

						// Execute asynchronously
						future := executor.ExecuteAsync(ctx, dag, data)

						// Do some other work while waiting
						for j := 0; j < 100; j++ {
							_ = j * j
						}

						// Wait for completion
						_, _ = future.Get()
					}
				})
			}
		})

		// Then, benchmark with mixed-duration nodes
		b.Run(fmt.Sprintf("MixedDuration_Size%d", size), func(b *testing.B) {
			// Create DAG with nodes of varying durations
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dag := workflow.NewDAG(fmt.Sprintf("mixed-duration-bench-%d", size))

				// Add nodes with varying durations
				for j := 0; j < size; j++ {
					// Create node with varying amount of work
					duration := time.Duration(rand.Intn(200)+50) * time.Microsecond
					action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
						// Simulate varying work
						time.Sleep(duration)
						return nil
					})
					node := workflow.NewNode(fmt.Sprintf("node%d", j), action)
					dag.AddNode(node)
				}

				data := workflow.NewWorkflowData("mixed-duration-bench")

				// Configure execution
				ctx := context.Background()
				b.StartTimer()

				// Execute with default settings
				_ = dag.Execute(ctx, data)
			}
		})

		// Test mixed-duration nodes with optimized executor
		b.Run(fmt.Sprintf("MixedDuration_Optimized_Size%d", size), func(b *testing.B) {
			// Create optimized executor
			executorConfig := &Config{
				MaxParallelism:         runtime.NumCPU(),
				MaxConcurrentWorkflows: 10,
				NodeExecutionTimeout:   30 * time.Second,
				QueueSize:              1000,
				QueueTimeout:           5 * time.Second,
			}
			executor := NewExecutorWithConfig(executorConfig)

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dag := workflow.NewDAG(fmt.Sprintf("mixed-duration-bench-%d", size))

				// Add nodes with varying durations
				for j := 0; j < size; j++ {
					// Create node with varying amount of work
					duration := time.Duration(rand.Intn(200)+50) * time.Microsecond
					action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
						// Simulate varying work
						time.Sleep(duration)
						return nil
					})
					node := workflow.NewNode(fmt.Sprintf("node%d", j), action)
					dag.AddNode(node)
				}

				data := workflow.NewWorkflowData("mixed-duration-bench")

				// Configure execution
				ctx := context.Background()
				b.StartTimer()

				// Execute with optimized executor
				_, _ = executor.Execute(ctx, dag, data)
			}
		})
	}
}

// BenchmarkWorkflowDataConfigurations benchmarks different WorkflowData configurations
func BenchmarkWorkflowDataConfigurations(b *testing.B) {
	// Define sizes to benchmark
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		// Standard configuration
		b.Run(fmt.Sprintf("Standard_Size_%d", size), func(b *testing.B) {
			config := workflow.DefaultWorkflowDataConfig()
			benchmarkWorkflowDataConfig(b, size, config)
		})

		// Arena-based configuration
		b.Run(fmt.Sprintf("Arena_Size_%d", size), func(b *testing.B) {
			config := workflow.DefaultWorkflowDataConfig()
			// Configure for arena usage
			benchmarkWorkflowDataConfig(b, size, config)
		})

		// Metrics disabled
		b.Run(fmt.Sprintf("MetricsDisabled_Size_%d", size), func(b *testing.B) {
			config := workflow.DefaultWorkflowDataConfig()
			config.MetricsConfig = metrics.DisabledMetricsConfig()
			benchmarkWorkflowDataConfig(b, size, config)
		})

		// Metrics sampled
		b.Run(fmt.Sprintf("MetricsSampled_Size_%d", size), func(b *testing.B) {
			config := workflow.DefaultWorkflowDataConfig()
			config.MetricsConfig = metrics.NewConfig().WithSamplingRate(0.01)
			benchmarkWorkflowDataConfig(b, size, config)
		})
	}
}

// benchmarkWorkflowDataConfig benchmarks a specific WorkflowData configuration
func benchmarkWorkflowDataConfig(b *testing.B, size int, config workflow.WorkflowDataConfig) {
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Create workflow data with the specified configuration
		data := workflow.NewWorkflowDataWithConfig(fmt.Sprintf("workflow-%d", i), config)

		// Perform a mix of operations
		b.StartTimer()

		// Write operations
		for j := 0; j < size; j++ {
			key := fmt.Sprintf("key-%d", j)
			value := fmt.Sprintf("value-%d", j)
			data.Set(key, value)
		}

		// Read operations
		for j := 0; j < size; j++ {
			key := fmt.Sprintf("key-%d", j)
			_, _ = data.Get(key)
		}

		// Node status operations
		for j := 0; j < size/2; j++ {
			nodeKey := fmt.Sprintf("node-%d", j)
			data.SetNodeStatus(nodeKey, workflow.Running)
			_, _ = data.GetNodeStatus(nodeKey)
		}

		// Output operations
		for j := 0; j < size/2; j++ {
			nodeKey := fmt.Sprintf("node-%d", j)
			data.SetOutput(nodeKey, fmt.Sprintf("output-%d", j))
			_, _ = data.GetOutput(nodeKey)
		}

		// Snapshot operations
		_, _ = data.Snapshot()
	}
}

// BenchmarkConcurrentWorkflowDataConfigurations benchmarks different WorkflowData configurations with concurrent access
func BenchmarkConcurrentWorkflowDataConfigurations(b *testing.B) {
	// Define sizes and worker counts to benchmark
	sizes := []int{100, 1000}
	workers := []int{2, 4, 8}

	for _, size := range sizes {
		for _, workerCount := range workers {
			// Standard configuration
			b.Run(fmt.Sprintf("Standard_Size_%d_Workers_%d", size, workerCount), func(b *testing.B) {
				config := workflow.DefaultWorkflowDataConfig()
				benchmarkConcurrentWorkflowDataConfig(b, size, workerCount, config)
			})

			// Metrics disabled
			b.Run(fmt.Sprintf("MetricsDisabled_Size_%d_Workers_%d", size, workerCount), func(b *testing.B) {
				config := workflow.DefaultWorkflowDataConfig()
				config.MetricsConfig = metrics.DisabledMetricsConfig()
				benchmarkConcurrentWorkflowDataConfig(b, size, workerCount, config)
			})
		}
	}
}

// benchmarkConcurrentWorkflowDataConfig benchmarks a specific WorkflowData configuration under concurrency
func benchmarkConcurrentWorkflowDataConfig(b *testing.B, size, workers int, config workflow.WorkflowDataConfig) {
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Create workflow data with the specified configuration
		data := workflow.NewWorkflowDataWithConfig(fmt.Sprintf("workflow-%d", i), config)

		// Perform concurrent operations
		var wg sync.WaitGroup
		b.StartTimer()

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				// Each worker performs a mix of operations
				start := workerID * (size / workers)
				end := start + (size / workers)
				if workerID == workers-1 {
					end = size // Last worker takes any remainder
				}

				// Write operations
				for j := start; j < end; j++ {
					key := fmt.Sprintf("key-%d", j)
					value := fmt.Sprintf("value-%d", j)
					data.Set(key, value)
				}

				// Read operations
				for j := start; j < end; j++ {
					key := fmt.Sprintf("key-%d", j)
					_, _ = data.Get(key)
				}

				// Node status operations
				for j := start; j < end; j += 2 {
					nodeKey := fmt.Sprintf("node-%d", j)
					data.SetNodeStatus(nodeKey, workflow.Running)
					_, _ = data.GetNodeStatus(nodeKey)
				}

				// Output operations
				for j := start; j < end; j += 2 {
					nodeKey := fmt.Sprintf("node-%d", j)
					data.SetOutput(nodeKey, fmt.Sprintf("output-%d", j))
					_, _ = data.GetOutput(nodeKey)
				}
			}(w)
		}

		wg.Wait()

		// Snapshot at the end
		_, _ = data.Snapshot()
	}
}

// createRandomDAG creates a random DAG with the specified number of nodes and connection probability
func createRandomDAG(size int, connectionProb float64) *workflow.DAG {
	dag := workflow.NewDAG(fmt.Sprintf("random-dag-%d", size))

	// Create nodes
	for i := 0; i < size; i++ {
		action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simple operation
			data.Set(fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i))
			return nil
		})
		node := workflow.NewNode(fmt.Sprintf("node%d", i), action)
		dag.AddNode(node)
	}

	// Add random dependencies
	for i := 0; i < size; i++ {
		for j := 0; j < i; j++ {
			if rand.Float64() < connectionProb {
				dag.AddDependency(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", j))
			}
		}
	}

	return dag
}
