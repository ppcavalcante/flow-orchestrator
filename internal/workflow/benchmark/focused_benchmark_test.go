package benchmark

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/arena"
)

// BenchmarkFocusedStringInterning tests string interning performance
func BenchmarkFocusedStringInterning(b *testing.B) {
	// Create test strings with a mix of common and unique strings
	commonStrings := []string{
		"node", "status", "pending", "running", "completed", "failed", "skipped",
		"workflow", "data", "input", "output", "error", "result", "id", "name",
	}

	// Generate test strings: mix of common strings and unique strings
	testStrings := make([]string, 0, 100)
	for i := 0; i < 50; i++ {
		// Add common strings
		testStrings = append(testStrings, commonStrings[i%len(commonStrings)])
		// Add unique strings
		testStrings = append(testStrings, fmt.Sprintf("unique_string_%d", i))
	}

	b.Run("IndividualIntern", func(b *testing.B) {
		b.ReportAllocs()
		interner := workflow.NewStringInterner()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := interner.Intern(s)
				// Use the interned string to prevent compiler optimizations
				if len(interned) == 0 && len(s) > 0 {
					b.Fatal("Interned string should not be empty")
				}
			}
		}
	})

	b.Run("BatchIntern", func(b *testing.B) {
		b.ReportAllocs()
		interner := workflow.NewStringInterner()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			internedStrings := interner.InternBatch(testStrings)
			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count should match original")
			}
		}
	})

	b.Run("GlobalIntern", func(b *testing.B) {
		b.ReportAllocs()
		// Reset the global interner
		workflow.ConfigureGlobalStringInterner(10000, 30*time.Second, 0.8)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := workflow.InternString(s)
				// Use the interned string to prevent compiler optimizations
				if len(interned) == 0 && len(s) > 0 {
					b.Fatal("Interned string should not be empty")
				}
			}
		}
	})

	b.Run("GlobalBatchIntern", func(b *testing.B) {
		b.ReportAllocs()
		// Reset the global interner
		workflow.ConfigureGlobalStringInterner(10000, 30*time.Second, 0.8)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			internedStrings := workflow.InternStringBatch(testStrings)
			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count should match original")
			}
		}
	})

	// Add arena-based string pool tests
	b.Run("ArenaStringPoolIntern", func(b *testing.B) {
		b.ReportAllocs()
		a := arena.NewArena()
		stringPool := arena.NewStringPool(a)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := stringPool.Intern(s)
				// Use the interned string to prevent compiler optimizations
				if len(interned) == 0 && len(s) > 0 {
					b.Fatal("Interned string should not be empty")
				}
			}
		}
	})

	b.Run("ArenaStringPoolBatchIntern", func(b *testing.B) {
		b.ReportAllocs()
		a := arena.NewArena()
		stringPool := arena.NewStringPool(a)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			internedStrings := stringPool.InternBatch(testStrings)
			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count should match original")
			}
		}
	})

	// Test memory usage and GC impact
	b.Run("MemoryUsage", func(b *testing.B) {
		b.ReportAllocs()
		interner := workflow.NewStringInterner()

		// Generate a large number of unique strings
		uniqueStrings := make([]string, 10000)
		for i := range uniqueStrings {
			uniqueStrings[i] = fmt.Sprintf("unique_large_string_%d", i)
		}

		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		beforeAlloc := stats.TotalAlloc

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Intern a subset of strings each iteration to simulate real usage
			start := (i * 100) % len(uniqueStrings)
			end := start + 100
			if end > len(uniqueStrings) {
				end = len(uniqueStrings)
			}

			for _, s := range uniqueStrings[start:end] {
				_ = interner.Intern(s)
			}

			if i%10 == 0 {
				// Force GC occasionally to measure its impact
				runtime.GC()
			}
		}

		runtime.ReadMemStats(&stats)
		b.ReportMetric(float64(stats.TotalAlloc-beforeAlloc)/float64(b.N), "B/op_total")
	})

	// Test arena memory usage and GC impact
	b.Run("ArenaMemoryUsage", func(b *testing.B) {
		b.ReportAllocs()
		a := arena.NewArena()
		stringPool := arena.NewStringPool(a)

		// Generate a large number of unique strings
		uniqueStrings := make([]string, 10000)
		for i := range uniqueStrings {
			uniqueStrings[i] = fmt.Sprintf("unique_large_string_%d", i)
		}

		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		beforeAlloc := stats.TotalAlloc

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Reset arena every 1000 iterations to simulate periodic cleanup
			if i > 0 && i%1000 == 0 {
				a.Reset()
				stringPool.Reset()
			}

			// Intern a subset of strings each iteration to simulate real usage
			start := (i * 100) % len(uniqueStrings)
			end := start + 100
			if end > len(uniqueStrings) {
				end = len(uniqueStrings)
			}

			for _, s := range uniqueStrings[start:end] {
				_ = stringPool.Intern(s)
			}

			if i%10 == 0 {
				// Force GC occasionally to measure its impact
				runtime.GC()
			}
		}

		runtime.ReadMemStats(&stats)
		b.ReportMetric(float64(stats.TotalAlloc-beforeAlloc)/float64(b.N), "B/op_total")
	})
}

// BenchmarkFocusedConcurrency tests different concurrency approaches
func BenchmarkFocusedConcurrency(b *testing.B) {
	// Define different workloads
	workloads := []struct {
		name      string
		taskCount int
		workTime  time.Duration
	}{
		{"Light_10", 10, 100 * time.Microsecond},
		{"Medium_100", 100, 100 * time.Microsecond},
		{"Heavy_1000", 1000, 100 * time.Microsecond},
		{"Mixed", 100, 0}, // Will use random durations
	}

	for _, wl := range workloads {
		b.Run(wl.name, func(b *testing.B) {
			// Create tasks based on the workload
			tasks := make([]func(), wl.taskCount)
			for i := 0; i < wl.taskCount; i++ {
				duration := wl.workTime
				if wl.name == "Mixed" {
					// For mixed workload, use random durations
					duration = time.Duration(rand.Intn(500)+50) * time.Microsecond
				}

				tasks[i] = func() {
					// Simulate work
					time.Sleep(duration)
				}
			}

			b.Run("DirectGoroutines", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					wg.Add(len(tasks))

					for _, task := range tasks {
						task := task // Capture for closure
						go func() {
							defer wg.Done()
							task()
						}()
					}

					wg.Wait()
				}
			})

			b.Run("LimitedConcurrency", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					wg.Add(len(tasks))

					// Create a semaphore to limit concurrency
					maxConcurrency := 4
					semaphore := make(chan struct{}, maxConcurrency)

					for _, task := range tasks {
						task := task // Capture for closure
						go func() {
							defer wg.Done()

							// Acquire semaphore
							semaphore <- struct{}{}
							defer func() { <-semaphore }()

							task()
						}()
					}

					wg.Wait()
				}
			})
		})
	}

	// Test high contention scenario
	b.Run("HighContention", func(b *testing.B) {
		b.ReportAllocs()

		// Create a shared resource that will be accessed by all tasks
		var counter int64
		var mu sync.Mutex

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			const numTasks = 1000
			var wg sync.WaitGroup
			wg.Add(numTasks)

			for j := 0; j < numTasks; j++ {
				go func() {
					defer wg.Done()

					// Simulate contention by having all tasks access the same resource
					mu.Lock()
					counter++
					mu.Unlock()

					// Do some work
					time.Sleep(10 * time.Microsecond)
				}()
			}

			wg.Wait()
		}
	})
}

// BenchmarkFocusedWorkflowDataSetGet tests basic data operations
func BenchmarkFocusedWorkflowDataSetGet(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			// Standard WorkflowData
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()
				data := workflow.NewWorkflowData("test")
				// Setup with initial data
				for i := 0; i < size; i++ {
					data.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i))
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Mix of operations for realistic workload
					key := fmt.Sprintf("key%d", i%size)
					data.Set(key, fmt.Sprintf("value%d", i))
					val, _ := data.Get(key)
					_ = val
				}
			})

			// Arena-based WorkflowData
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()
				config := workflow.DefaultWorkflowDataConfig()
				config.ExpectedNodes = size
				config.ExpectedData = size * 2
				data := workflow.NewWorkflowDataWithArena("test", config)

				// Setup with initial data
				for i := 0; i < size; i++ {
					data.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i))
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Mix of operations for realistic workload
					key := fmt.Sprintf("key%d", i%size)
					data.Set(key, fmt.Sprintf("value%d", i))
					val, _ := data.Get(key)
					_ = val
				}
			})

			// Arena-based WorkflowData with reset
			b.Run("ArenaWithReset", func(b *testing.B) {
				b.ReportAllocs()
				config := workflow.DefaultWorkflowDataConfig()
				config.ExpectedNodes = size
				config.ExpectedData = size * 2
				data := workflow.NewWorkflowDataWithArena("test", config)

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Reset arena every 100 iterations
					if i > 0 && i%100 == 0 {
						data.ResetArena()
					}

					// Mix of operations for realistic workload
					key := fmt.Sprintf("key%d", i%size)
					data.Set(key, fmt.Sprintf("value%d", i))
					val, _ := data.Get(key)
					_ = val
				}
			})
		})
	}
}

// BenchmarkFocusedConcurrentAccess tests concurrent access to workflow data
func BenchmarkFocusedConcurrentAccess(b *testing.B) {
	workerCounts := []int{2, 4, 8, 16, 32}
	sizes := []int{10, 100}

	for _, size := range sizes {
		for _, workerCount := range workerCounts {
			b.Run(fmt.Sprintf("Workers_%d_Size_%d", workerCount, size), func(b *testing.B) {
				data := workflow.NewWorkflowData("test")
				// Setup with initial data
				for i := 0; i < size; i++ {
					data.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i))
				}

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					wg.Add(workerCount)

					for w := 0; w < workerCount; w++ {
						w := w
						go func() {
							defer wg.Done()
							// Each worker does a mix of reads and writes
							for j := 0; j < 10; j++ {
								key := fmt.Sprintf("key%d", (i+w+j)%size)
								if j%2 == 0 {
									data.Set(key, fmt.Sprintf("value%d_%d", i, w))
								} else {
									val, _ := data.Get(key)
									_ = val
								}
							}
						}()
					}

					wg.Wait()
				}
			})
		}
	}
}

// BenchmarkFocusedDependencyResolution tests dependency resolution performance
func BenchmarkFocusedDependencyResolution(b *testing.B) {
	topologies := []string{"Linear", "Diamond", "Complex"}
	sizes := []int{10, 100, 500}

	for _, topology := range topologies {
		for _, size := range sizes {
			var nodes []*workflow.Node
			var dag *workflow.DAG

			// Create different topologies
			switch topology {
			case "Linear":
				// Linear chain: A → B → C → ...
				dag, nodes = createLinearDAGForBenchmark(size)
			case "Diamond":
				// Diamond pattern with multiple paths
				dag, nodes = createDiamondDAGForBenchmark(size)
			case "Complex":
				// Complex graph with random dependencies
				dag, nodes = createComplexDAGForBenchmark(size)
			}

			b.Run(fmt.Sprintf("%s_Size_%d", topology, size), func(b *testing.B) {
				data := workflow.NewWorkflowData("test")

				// Setup with initial statuses
				for _, node := range nodes {
					data.SetNodeStatus(node.Name, workflow.Pending)
				}

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					// Reset statuses
					for j, node := range nodes {
						if j < len(nodes)/3 {
							// Mark first third as completed
							data.SetNodeStatus(node.Name, workflow.Completed)
						} else {
							data.SetNodeStatus(node.Name, workflow.Pending)
						}
					}

					// Test dependency resolution for each node
					for _, node := range nodes {
						// Use the DAG to check if the node is runnable
						nodeInDag, _ := dag.GetNode(node.Name)
						if nodeInDag != nil {
							_ = data.IsNodeRunnable(node.Name)
						}
					}
				}
			})
		}
	}
}

// BenchmarkFocusedSerialization tests serialization performance
func BenchmarkFocusedSerialization(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			data := workflow.NewWorkflowData("test")
			// Setup with data, statuses and outputs
			for i := 0; i < size; i++ {
				key := fmt.Sprintf("key%d", i)
				nodeName := fmt.Sprintf("node%d", i)
				data.Set(key, i)
				data.SetNodeStatus(nodeName, workflow.Completed)
				data.SetOutput(nodeName, strconv.Itoa(i))
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Create snapshot
				snapshot, _ := data.Snapshot()

				// Create new data store and restore
				newData := workflow.NewWorkflowData("test")
				newData.LoadSnapshot(snapshot)
			}
		})
	}
}

// BenchmarkFocusedWorkflowExecution tests end-to-end workflow execution
func BenchmarkFocusedWorkflowExecution(b *testing.B) {
	sizes := []int{10, 50, 100}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			// Create a workflow with the specified number of nodes
			dag := workflow.NewDAG("benchmark-workflow")

			// Create nodes with simple actions
			for i := 0; i < size; i++ {
				nodeName := fmt.Sprintf("node%d", i)
				action := createBenchmarkAction(nodeName)
				node := workflow.NewNode(nodeName, action)
				dag.AddNode(node)

				// Add dependencies to create a realistic workflow
				if i > 0 {
					// Connect to previous node
					dag.AddDependency(nodeName, fmt.Sprintf("node%d", i-1))

					// Add some cross-dependencies for more complex graphs
					if i > 5 && i%5 == 0 {
						dag.AddDependency(nodeName, fmt.Sprintf("node%d", i-5))
					}
				}
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Create a new executor for each iteration
				executor := NewExecutor()

				// Execute the workflow
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				data := workflow.NewWorkflowData("benchmark-data")
				_, err := executor.Execute(ctx, dag, data)
				cancel()

				if err != nil {
					b.Fatalf("Workflow execution failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkFocusedArenaWorkflowExecution tests workflow execution with arena-based memory management
func BenchmarkFocusedArenaWorkflowExecution(b *testing.B) {
	// Test different DAG topologies
	topologies := []struct {
		name      string
		createDAG func(int) (*workflow.DAG, []*workflow.Node)
	}{
		{"Linear", createLinearDAGForBenchmark},
		{"Diamond", createDiamondDAGForBenchmark},
		{"Complex", createComplexDAGForBenchmark},
	}

	// Test different sizes
	sizes := []int{10, 50, 100}

	for _, topo := range topologies {
		for _, size := range sizes {
			b.Run(fmt.Sprintf("%s_Size_%d", topo.name, size), func(b *testing.B) {
				// Standard execution
				b.Run("Standard", func(b *testing.B) {
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						// Create a new DAG for each iteration
						dag, _ := topo.createDAG(size)

						// Create workflow data
						data := workflow.NewWorkflowData(fmt.Sprintf("test-%d", i))

						// Execute the workflow
						ctx := context.Background()
						executor := NewExecutor()
						_, err := executor.Execute(ctx, dag, data)
						if err != nil {
							b.Fatalf("Workflow execution failed: %v", err)
						}
					}
				})

				// Arena-based execution
				b.Run("Arena", func(b *testing.B) {
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						// Create a new DAG for each iteration
						dag, _ := topo.createDAG(size)

						// Create workflow data with arena
						config := workflow.DefaultWorkflowDataConfig()
						config.ExpectedNodes = size
						config.ExpectedData = size * 2
						data := workflow.NewWorkflowDataWithArena(fmt.Sprintf("test-%d", i), config)

						// Execute the workflow
						ctx := context.Background()
						executor := NewExecutor()
						_, err := executor.Execute(ctx, dag, data)
						if err != nil {
							b.Fatalf("Workflow execution failed: %v", err)
						}
					}
				})

				// Arena-based execution with custom block size
				b.Run("ArenaCustomBlock", func(b *testing.B) {
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						// Create a new DAG for each iteration
						dag, _ := topo.createDAG(size)

						// Create workflow data with arena and custom block size
						config := workflow.DefaultWorkflowDataConfig()
						config.ExpectedNodes = size
						config.ExpectedData = size * 2
						blockSize := 4096 * (size/10 + 1) // Scale block size with data size
						data := workflow.NewWorkflowDataWithArenaBlockSize(fmt.Sprintf("test-%d", i), config, blockSize)

						// Execute the workflow
						ctx := context.Background()
						executor := NewExecutor()
						_, err := executor.Execute(ctx, dag, data)
						if err != nil {
							b.Fatalf("Workflow execution failed: %v", err)
						}
					}
				})
			})
		}
	}
}

// BenchmarkIntegratedWorkflow tests the entire workflow system with different memory management strategies
func BenchmarkIntegratedWorkflow(b *testing.B) {
	// Test different workflow complexities
	complexities := []struct {
		name      string
		nodeCount int
		dataCount int
		depth     int
	}{
		{"Small", 10, 20, 3},
		{"Medium", 50, 100, 5},
		{"Large", 200, 400, 8},
	}

	for _, complexity := range complexities {
		b.Run(fmt.Sprintf("Complexity_%s", complexity.name), func(b *testing.B) {
			// Standard implementation
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					// Create a workflow with the specified complexity
					_, nodes := createComplexWorkflow(complexity.nodeCount, complexity.depth)

					// Create standard workflow data
					data := workflow.NewWorkflowData(fmt.Sprintf("test-%d", i))

					// Initialize data
					for j := 0; j < complexity.dataCount; j++ {
						data.Set(fmt.Sprintf("key%d", j), fmt.Sprintf("value%d", j))
					}

					// Set all nodes to Completed status (since we're not actually executing)
					for _, node := range nodes {
						data.SetNodeStatus(node.Name, workflow.Completed)
					}

					// Verify some results
					for j := 0; j < len(nodes); j += len(nodes) / 5 {
						status, _ := data.GetNodeStatus(nodes[j].Name)
						if status != workflow.Completed && status != workflow.Skipped {
							b.Fatalf("Node %s should be completed or skipped, got %v", nodes[j].Name, status)
						}
					}
				}
			})

			// Arena-based implementation
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					// Create a workflow with the specified complexity
					_, nodes := createComplexWorkflow(complexity.nodeCount, complexity.depth)

					// Create arena-based workflow data
					config := workflow.DefaultWorkflowDataConfig()
					config.ExpectedNodes = complexity.nodeCount
					config.ExpectedData = complexity.dataCount
					data := workflow.NewWorkflowDataWithArena(fmt.Sprintf("test-%d", i), config)

					// Initialize data
					for j := 0; j < complexity.dataCount; j++ {
						data.Set(fmt.Sprintf("key%d", j), fmt.Sprintf("value%d", j))
					}

					// Set all nodes to Completed status (since we're not actually executing)
					for _, node := range nodes {
						data.SetNodeStatus(node.Name, workflow.Completed)
					}

					// Verify some results
					for j := 0; j < len(nodes); j += len(nodes) / 5 {
						status, _ := data.GetNodeStatus(nodes[j].Name)
						if status != workflow.Completed && status != workflow.Skipped {
							b.Fatalf("Node %s should be completed or skipped, got %v", nodes[j].Name, status)
						}
					}
				}
			})

			// Arena-based implementation with custom block size
			b.Run("ArenaCustomBlock", func(b *testing.B) {
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					// Create a workflow with the specified complexity
					_, nodes := createComplexWorkflow(complexity.nodeCount, complexity.depth)

					// Create arena-based workflow data with custom block size
					config := workflow.DefaultWorkflowDataConfig()
					config.ExpectedNodes = complexity.nodeCount
					config.ExpectedData = complexity.dataCount

					// Calculate a reasonable block size based on the expected data size
					blockSize := 4096 * (complexity.nodeCount/10 + 1)
					data := workflow.NewWorkflowDataWithArenaBlockSize(fmt.Sprintf("test-%d", i), config, blockSize)

					// Initialize data
					for j := 0; j < complexity.dataCount; j++ {
						data.Set(fmt.Sprintf("key%d", j), fmt.Sprintf("value%d", j))
					}

					// Set all nodes to Completed status (since we're not actually executing)
					for _, node := range nodes {
						data.SetNodeStatus(node.Name, workflow.Completed)
					}

					// Verify some results
					for j := 0; j < len(nodes); j += len(nodes) / 5 {
						status, _ := data.GetNodeStatus(nodes[j].Name)
						if status != workflow.Completed && status != workflow.Skipped {
							b.Fatalf("Node %s should be completed or skipped, got %v", nodes[j].Name, status)
						}
					}
				}
			})
		})
	}
}

// BenchmarkWorkflowWithGC tests workflow execution with forced garbage collection
func BenchmarkWorkflowWithGC(b *testing.B) {
	// Test different workflow sizes
	sizes := []int{50, 200, 500}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			// Standard implementation
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()

				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)
				beforeAlloc := memStats.TotalAlloc

				for i := 0; i < b.N; i++ {
					// Create a workflow
					dag, _ := createComplexDAGForBenchmark(size)

					// Create standard workflow data
					data := workflow.NewWorkflowData(fmt.Sprintf("test-%d", i))

					// Initialize data
					for j := 0; j < size*2; j++ {
						data.Set(fmt.Sprintf("key%d", j), fmt.Sprintf("value%d", j))
					}

					// Execute the workflow
					ctx := context.Background()
					executor := NewExecutor()
					_, err := executor.Execute(ctx, dag, data)
					if err != nil {
						b.Fatalf("Workflow execution failed: %v", err)
					}

					// Force GC every few iterations
					if i > 0 && i%5 == 0 {
						runtime.GC()
					}
				}

				runtime.ReadMemStats(&memStats)
				b.ReportMetric(float64(memStats.TotalAlloc-beforeAlloc)/float64(b.N), "B/op_total")
			})

			// Arena-based implementation
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()

				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)
				beforeAlloc := memStats.TotalAlloc

				for i := 0; i < b.N; i++ {
					// Create a workflow
					dag, _ := createComplexDAGForBenchmark(size)

					// Create arena-based workflow data
					config := workflow.DefaultWorkflowDataConfig()
					config.ExpectedNodes = size
					config.ExpectedData = size * 2
					data := workflow.NewWorkflowDataWithArena(fmt.Sprintf("test-%d", i), config)

					// Initialize data
					for j := 0; j < size*2; j++ {
						data.Set(fmt.Sprintf("key%d", j), fmt.Sprintf("value%d", j))
					}

					// Execute the workflow
					ctx := context.Background()
					executor := NewExecutor()
					_, err := executor.Execute(ctx, dag, data)
					if err != nil {
						b.Fatalf("Workflow execution failed: %v", err)
					}

					// Reset arena every few iterations instead of relying on GC
					if i > 0 && i%5 == 0 {
						data.ResetArena()
					}
				}

				runtime.ReadMemStats(&memStats)
				b.ReportMetric(float64(memStats.TotalAlloc-beforeAlloc)/float64(b.N), "B/op_total")
			})
		})
	}
}

// BenchmarkWorkflowReuse tests reusing workflow data across multiple executions
func BenchmarkWorkflowReuse(b *testing.B) {
	// Test different workflow sizes
	sizes := []int{50, 200}

	// Number of executions per workflow data
	executions := []int{5, 20}

	for _, size := range sizes {
		for _, execCount := range executions {
			b.Run(fmt.Sprintf("Size_%d_Executions_%d", size, execCount), func(b *testing.B) {
				// Standard implementation
				b.Run("Standard", func(b *testing.B) {
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						// Create standard workflow data once
						data := workflow.NewWorkflowData(fmt.Sprintf("test-%d", i))

						// Execute multiple workflows with the same data
						for j := 0; j < execCount; j++ {
							// Create a new DAG for each execution
							dag, _ := createComplexDAGForBenchmark(size)

							// Execute the workflow
							ctx := context.Background()
							executor := NewExecutor()
							_, err := executor.Execute(ctx, dag, data)
							if err != nil {
								b.Fatalf("Workflow execution failed: %v", err)
							}
						}
					}
				})

				// Arena-based implementation
				b.Run("Arena", func(b *testing.B) {
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						// Create arena-based workflow data once
						config := workflow.DefaultWorkflowDataConfig()
						config.ExpectedNodes = size
						config.ExpectedData = size * 2
						data := workflow.NewWorkflowDataWithArena(fmt.Sprintf("test-%d", i), config)

						// Execute multiple workflows with the same data
						for j := 0; j < execCount; j++ {
							// Reset arena before each execution
							if j > 0 {
								data.ResetArena()
							}

							// Create a new DAG for each execution
							dag, _ := createComplexDAGForBenchmark(size)

							// Execute the workflow
							ctx := context.Background()
							executor := NewExecutor()
							_, err := executor.Execute(ctx, dag, data)
							if err != nil {
								b.Fatalf("Workflow execution failed: %v", err)
							}
						}
					}
				})
			})
		}
	}
}

// createComplexWorkflow creates a complex workflow with the specified number of nodes and depth
func createComplexWorkflow(nodeCount, depth int) (*workflow.DAG, []*workflow.Node) {
	dag := workflow.NewDAG(fmt.Sprintf("complex-workflow-%d-%d", nodeCount, depth))
	nodes := make([]*workflow.Node, 0, nodeCount)

	// Create nodes
	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		action := createBenchmarkAction(nodeName)
		node := workflow.NewNode(nodeName, action)
		dag.AddNode(node)
		nodes = append(nodes, node)
	}

	// Create dependencies with a maximum depth
	for i := depth; i < nodeCount; i++ {
		// Each node depends on 1-3 previous nodes, but not more than 'depth' levels back
		dependencyCount := rand.Intn(3) + 1
		for j := 0; j < dependencyCount; j++ {
			// Pick a random node from the previous 'depth' nodes
			depIndex := i - (rand.Intn(depth) + 1)
			if depIndex >= 0 {
				dag.AddDependency(nodes[i].Name, nodes[depIndex].Name)
			}
		}
	}

	return dag, nodes
}

// Helper functions

// createBenchmarkAction creates a test action that simulates work
func createBenchmarkAction(name string) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		// Simulate work with a short delay
		time.Sleep(1 * time.Millisecond)
		// Set some output data
		data.SetOutput(name, fmt.Sprintf("output from %s", name))
		return nil
	})
}

// createLinearDAGForBenchmark creates a linear chain of nodes: A → B → C → ...
func createLinearDAGForBenchmark(count int) (*workflow.DAG, []*workflow.Node) {
	dag := workflow.NewDAG("linear-benchmark")
	nodes := make([]*workflow.Node, count)

	// Create all nodes first
	for i := 0; i < count; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		node := workflow.NewNode(nodeName, createBenchmarkAction(nodeName))
		nodes[i] = node
		dag.AddNode(node)
	}

	// Add dependencies
	for i := 1; i < count; i++ {
		fromName := fmt.Sprintf("node%d", i-1)
		toName := fmt.Sprintf("node%d", i)
		dag.AddDependency(toName, fromName)
	}

	return dag, nodes
}

// createDiamondDAGForBenchmark creates a diamond pattern with multiple paths
func createDiamondDAGForBenchmark(count int) (*workflow.DAG, []*workflow.Node) {
	dag := workflow.NewDAG("diamond-benchmark")
	nodes := make([]*workflow.Node, count)

	// Create all nodes first
	for i := 0; i < count; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		node := workflow.NewNode(nodeName, createBenchmarkAction(nodeName))
		nodes[i] = node
		dag.AddNode(node)
	}

	// Add dependencies to create diamond pattern
	for i := 1; i < count; i++ {
		// First half depends on node0
		if i <= count/2 {
			dag.AddDependency(fmt.Sprintf("node%d", i), "node0")
		} else {
			// Second half depends on middle nodes
			dep1 := fmt.Sprintf("node%d", i/2)
			dep2 := fmt.Sprintf("node%d", i/2+1)
			dag.AddDependency(fmt.Sprintf("node%d", i), dep1)
			if dep1 != dep2 {
				dag.AddDependency(fmt.Sprintf("node%d", i), dep2)
			}
		}
	}

	return dag, nodes
}

// createComplexDAGForBenchmark creates a complex graph with random dependencies
func createComplexDAGForBenchmark(count int) (*workflow.DAG, []*workflow.Node) {
	dag := workflow.NewDAG("complex-benchmark")
	nodes := make([]*workflow.Node, count)

	// Create all nodes first
	for i := 0; i < count; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		node := workflow.NewNode(nodeName, createBenchmarkAction(nodeName))
		nodes[i] = node
		dag.AddNode(node)
	}

	// Add dependencies to create complex pattern
	for i := 1; i < count; i++ {
		// Always depend on at least one previous node
		dag.AddDependency(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", i-1))

		// Add some random dependencies
		maxDeps := 3
		if i < maxDeps {
			maxDeps = i
		}

		numDeps := rand.Intn(maxDeps) + 1
		for d := 0; d < numDeps; d++ {
			depIdx := rand.Intn(i)
			depName := fmt.Sprintf("node%d", depIdx)
			dag.AddDependency(fmt.Sprintf("node%d", i), depName)
		}
	}

	return dag, nodes
}
