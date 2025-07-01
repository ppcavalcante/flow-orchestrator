package benchmark

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// wrapDefaultConfig wraps the DefaultWorkflowDataConfig to match the function signature
func wrapDefaultConfig(size int) workflow.WorkflowDataConfig {
	config := workflow.DefaultWorkflowDataConfig()
	config.ExpectedNodes = size
	config.ExpectedData = size * 2
	return config
}

// wrapReadOptimizedConfig creates a read-optimized configuration
func wrapReadOptimizedConfig(size int) workflow.WorkflowDataConfig {
	config := workflow.DefaultWorkflowDataConfig()
	config.ExpectedNodes = size
	config.ExpectedData = size * 2
	// Additional settings for read optimization could be added here
	return config
}

// wrapHighConcurrencyConfig creates a high-concurrency configuration
func wrapHighConcurrencyConfig(size int) workflow.WorkflowDataConfig {
	config := workflow.DefaultWorkflowDataConfig()
	config.ExpectedNodes = size
	config.ExpectedData = size * 2
	// Additional settings for high concurrency could be added here
	return config
}

// BenchmarkLockFreeStructures benchmarks the performance of lock-free data structures
func BenchmarkLockFreeStructures(b *testing.B) {
	// Define sizes to benchmark
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		// Benchmark different map implementations
		b.Run(fmt.Sprintf("Maps_Size_%d", size), func(b *testing.B) {
			// Define different map configurations to test
			configurations := []struct {
				name   string
				config func(int) workflow.WorkflowDataConfig
			}{
				{"Default", wrapDefaultConfig},
				{"ReadOptimized", wrapReadOptimizedConfig},
				{"HighConcurrency", wrapHighConcurrencyConfig},
			}

			for _, cfg := range configurations {
				b.Run(fmt.Sprintf("%s_Size_%d", cfg.name, size), func(b *testing.B) {
					// Create WorkflowData with specific configuration
					config := cfg.config(size)
					data := workflow.NewWorkflowDataWithConfig("test", config)

					// Benchmark operations
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						// Set operations
						for j := 0; j < size; j++ {
							key := fmt.Sprintf("key-%d", j)
							data.Set(key, j)
						}

						// Get operations
						for j := 0; j < size; j++ {
							key := fmt.Sprintf("key-%d", j)
							_, _ = data.Get(key)
						}
					}
				})
			}
		})

		// Benchmark concurrent map operations
		b.Run(fmt.Sprintf("ConcurrentMaps_Size_%d", size), func(b *testing.B) {
			// Define different map configurations to test
			configurations := []struct {
				name   string
				config func(int) workflow.WorkflowDataConfig
			}{
				{"Default", wrapDefaultConfig},
				{"ReadOptimized", wrapReadOptimizedConfig},
				{"HighConcurrency", wrapHighConcurrencyConfig},
			}

			for _, cfg := range configurations {
				b.Run(fmt.Sprintf("%s_Size_%d", cfg.name, size), func(b *testing.B) {
					// Create WorkflowData with specific configuration
					config := cfg.config(size)
					data := workflow.NewWorkflowDataWithConfig("test", config)

					// Number of goroutines to use
					workers := 4

					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						var wg sync.WaitGroup
						wg.Add(workers)
						b.StartTimer()

						// Start worker goroutines
						for w := 0; w < workers; w++ {
							go func(workerID int) {
								defer wg.Done()

								// Each worker operates on its own range of keys
								start := (size / workers) * workerID
								end := start + (size / workers)
								if workerID == workers-1 {
									end = size // Last worker takes any remainder
								}

								// Set operations
								for j := start; j < end; j++ {
									key := fmt.Sprintf("key-%d", j)
									data.Set(key, j)
								}

								// Get operations
								for j := start; j < end; j++ {
									key := fmt.Sprintf("key-%d", j)
									_, _ = data.Get(key)
								}
							}(w)
						}

						// Wait for all workers to complete
						wg.Wait()
					}
				})
			}
		})

		// Benchmark node status operations
		b.Run(fmt.Sprintf("NodeStatus_Size_%d", size), func(b *testing.B) {
			// Define different map configurations to test
			configurations := []struct {
				name   string
				config func(int) workflow.WorkflowDataConfig
			}{
				{"Default", wrapDefaultConfig},
				{"ReadOptimized", wrapReadOptimizedConfig},
				{"HighConcurrency", wrapHighConcurrencyConfig},
			}

			for _, cfg := range configurations {
				b.Run(fmt.Sprintf("%s_Size_%d", cfg.name, size), func(b *testing.B) {
					// Create WorkflowData with specific configuration
					config := cfg.config(size)
					data := workflow.NewWorkflowDataWithConfig("test", config)

					// Benchmark operations
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						// Set node status operations
						for j := 0; j < size; j++ {
							nodeName := fmt.Sprintf("node-%d", j)
							data.SetNodeStatus(nodeName, workflow.Running)
						}

						// Get node status operations
						for j := 0; j < size; j++ {
							nodeName := fmt.Sprintf("node-%d", j)
							_, _ = data.GetNodeStatus(nodeName)
						}
					}
				})
			}
		})
	}
}
