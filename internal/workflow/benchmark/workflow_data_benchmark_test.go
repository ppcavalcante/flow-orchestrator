package benchmark

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestSimpleWorkflowData is a simplified test for workflow data operations
func TestSimpleWorkflowData(t *testing.T) {
	log.Println("Starting simple workflow data test")

	// Create a small instance
	data := workflow.NewWorkflowData("test")
	log.Println("Created workflow data")

	// Basic set/get operations
	data.Set("key1", "value1")
	log.Println("Set key1")

	val, ok := data.Get("key1")
	log.Println("Get key1 result:", val, ok)

	// Status operations
	data.SetNodeStatus("node1", workflow.Pending)
	log.Println("Set node1 status to Pending")

	status, _ := data.GetNodeStatus("node1")
	log.Println("Get node1 status:", status)

	// Output operations
	data.SetOutput("node1", "output1")
	log.Println("Set node1 output")

	output, ok := data.GetOutput("node1")
	log.Println("Get node1 output:", output, ok)

	log.Println("Simple test completed successfully")
}

// BenchmarkSimpleWorkflowData runs a simple benchmark with timeout
func BenchmarkSimpleWorkflowData(b *testing.B) {
	b.Log("Starting simple benchmark")

	// Create data once outside the loop
	data := workflow.NewWorkflowData("test")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Add timeout to prevent hanging
		done := make(chan bool)
		go func(i int) {
			// Basic operations
			key := fmt.Sprintf("key%d", i)
			data.Set(key, i)
			data.Get(key)

			// Node status operations
			nodeName := fmt.Sprintf("node%d", i%10)
			data.SetNodeStatus(nodeName, workflow.Running)
			data.GetNodeStatus(nodeName)

			done <- true
		}(i)

		// Wait with timeout
		select {
		case <-done:
			// Operation completed
		case <-time.After(1 * time.Second):
			b.Fatalf("Operation timed out at iteration %d", i)
		}
	}

	b.Log("Benchmark completed successfully")
}

// BenchmarkWorkflowDataSetGet tests basic data operations
func BenchmarkWorkflowDataSetGet(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
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
	}
}

// BenchmarkNodeStatus tests node status operations
func BenchmarkNodeStatus(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			data := workflow.NewWorkflowData("test")
			// Setup with initial statuses
			for i := 0; i < size; i++ {
				nodeName := fmt.Sprintf("node%d", i)
				data.SetNodeStatus(nodeName, workflow.Pending)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Mix of status operations
				nodeName := fmt.Sprintf("node%d", i%size)
				data.SetNodeStatus(nodeName, workflow.Running)
				status, _ := data.GetNodeStatus(nodeName)
				_ = status
			}
		})
	}
}

// BenchmarkWorkflowDataDependencyResolution tests dependency resolution performance
func BenchmarkWorkflowDataDependencyResolution(b *testing.B) {
	topologies := []string{"Linear", "Diamond", "Complex"}
	sizes := []int{10, 100, 1000}

	for _, topology := range topologies {
		for _, size := range sizes {
			var nodes []*workflow.Node

			// Create different topologies
			switch topology {
			case "Linear":
				// Linear chain: A → B → C → ...
				nodes = createLinearNodes(size)
			case "Diamond":
				// Diamond pattern with multiple paths
				nodes = createDiamondNodes(size)
			case "Complex":
				// Complex graph with random dependencies
				nodes = createComplexNodes(size)
			}

			b.Run(fmt.Sprintf("%s_Size_%d", topology, size), func(b *testing.B) {
				data := workflow.NewWorkflowData("test")

				// Setup with initial statuses
				for _, node := range nodes {
					data.SetNodeStatus(node.Name, workflow.Pending)
				}

				b.ResetTimer()
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
						_ = data.IsNodeRunnable(node.Name)
					}
				}
			})
		}
	}
}

// BenchmarkSnapshotAndRestore tests serialization performance
func BenchmarkSnapshotAndRestore(b *testing.B) {
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

// BenchmarkWorkflowDataTypePerformance tests various parts of WorkflowData performance
func BenchmarkWorkflowDataTypePerformance(b *testing.B) {
	// Test typed data retrieval
	b.Run("TypedDataRetrieval", func(b *testing.B) {
		sizes := []int{10, 100, 1000}

		for _, size := range sizes {
			data := workflow.NewWorkflowData("test")
			// Setup with different types of data
			for i := 0; i < size; i++ {
				data.Set(fmt.Sprintf("int_%d", i), i)
				data.Set(fmt.Sprintf("string_%d", i), fmt.Sprintf("value%d", i))
				data.Set(fmt.Sprintf("bool_%d", i), i%2 == 0)
				data.Set(fmt.Sprintf("float_%d", i), float64(i)*1.5)
				data.Set(fmt.Sprintf("slice_%d", i), []string{fmt.Sprintf("item%d", i)})
				data.Set(fmt.Sprintf("map_%d", i), map[string]int{"value": i})
			}

			b.Run(fmt.Sprintf("GetInt_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("int_%d", i%size)
					val, _ := data.GetInt(key)
					_ = val
				}
			})

			b.Run(fmt.Sprintf("GetString_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("string_%d", i%size)
					val, _ := data.GetString(key)
					_ = val
				}
			})

			b.Run(fmt.Sprintf("GetBool_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("bool_%d", i%size)
					val, _ := data.GetBool(key)
					_ = val
				}
			})

			b.Run(fmt.Sprintf("GetFloat_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("float_%d", i%size)
					val, _ := data.Get(key)
					_ = val.(float64)
				}
			})

			b.Run(fmt.Sprintf("GetSlice_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("slice_%d", i%size)
					val, _ := data.Get(key)
					_ = val.([]string)
				}
			})

			b.Run(fmt.Sprintf("GetMap_%d", size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					key := fmt.Sprintf("map_%d", i%size)
					val, _ := data.Get(key)
					_ = val.(map[string]int)
				}
			})
		}
	})

	// Test concurrent access
	b.Run("ConcurrentAccess", func(b *testing.B) {
		sizes := []int{10, 100}
		workerCounts := []int{2, 4, 8, 16, 32}

		for _, size := range sizes {
			for _, workers := range workerCounts {
				b.Run(fmt.Sprintf("Workers_%d_Size_%d", workers, size), func(b *testing.B) {
					data := workflow.NewWorkflowData("test")

					// Setup initial data
					for i := 0; i < size; i++ {
						data.Set(fmt.Sprintf("key%d", i), i)
						data.SetNodeStatus(fmt.Sprintf("node%d", i), workflow.Pending)
					}

					b.ResetTimer()
					b.ReportAllocs()

					for i := 0; i < b.N; i++ {
						var wg sync.WaitGroup
						wg.Add(workers)

						// Start workers
						for w := 0; w < workers; w++ {
							go func(workerID int) {
								defer wg.Done()

								// Each worker does some operations
								for j := 0; j < 10; j++ {
									key := fmt.Sprintf("key%d", (workerID*10+j)%size)
									data.Set(key, workerID*100+j)

									val, _ := data.Get(key)
									_ = val

									nodeName := fmt.Sprintf("node%d", (workerID*10+j)%size)
									data.SetNodeStatus(nodeName, workflow.Running)
									status, _ := data.GetNodeStatus(nodeName)
									_ = status
								}
							}(w)
						}

						wg.Wait()
					}
				})
			}
		}
	})

	// Test string interning effectiveness
	b.Run("StringInterning", func(b *testing.B) {
		data := workflow.NewWorkflowData("test")

		// Setup with initial data using repeated keys
		repeated := make([]string, 1000)
		for i := 0; i < 20; i++ {
			key := fmt.Sprintf("repeatedKey%d", i)
			for j := 0; j < 50; j++ {
				repeated[i*50+j] = key
			}
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			key := repeated[i%1000]
			data.Set(key, i)
			val, _ := data.Get(key)
			_ = val
		}
	})
}

// BenchmarkWorkflowRecoveryPattern tests the performance of determining which nodes can run after a failure
func BenchmarkWorkflowRecoveryPattern(b *testing.B) {
	sizes := []int{50, 100, 200}

	for _, size := range sizes {
		// Create a workflow with a complex pattern
		nodes := createComplexNodes(size)

		b.Run(fmt.Sprintf("DetermineRunnableAfterFailure/Nodes_%d", size), func(b *testing.B) {
			data := workflow.NewWorkflowData("test")

			// Setup: Mark half the nodes as completed, one as failed
			for i, node := range nodes {
				if i < size/2 {
					data.SetNodeStatus(node.Name, workflow.Completed)
				} else if i == size/2 {
					data.SetNodeStatus(node.Name, workflow.Failed)
				} else {
					data.SetNodeStatus(node.Name, workflow.Pending)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				// Find all nodes that are runnable
				runnableNodes := make([]*workflow.Node, 0, size/4)
				for _, node := range nodes {
					if data.IsNodeRunnable(node.Name) {
						runnableNodes = append(runnableNodes, node)
					}
				}
			}
		})
	}
}

// BenchmarkLargeWorkflowOperations tests operations on very large workflows
func BenchmarkLargeWorkflowOperations(b *testing.B) {
	sizes := []int{1000, 5000, 10000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			b.Skip("Skipping large workflow test for now - uncomment to run")

			data := workflow.NewWorkflowData("test")

			// Setup with a large number of nodes/data items
			for i := 0; i < size; i++ {
				data.Set(fmt.Sprintf("key%d", i), i)
				data.SetNodeStatus(fmt.Sprintf("node%d", i), workflow.Pending)
				data.SetOutput(fmt.Sprintf("node%d", i), fmt.Sprintf("output%d", i))
			}

			// Test operations
			b.Run("Snapshot", func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					snapshot, _ := data.Snapshot()
					_ = snapshot
				}
			})

			b.Run("BulkStatusUpdate", func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					// Update status of 10% of nodes
					for j := 0; j < size/10; j++ {
						idx := (i*100 + j) % size
						data.SetNodeStatus(fmt.Sprintf("node%d", idx), workflow.Running)
					}
				}
			})

			b.Run("KeyEnumeration", func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					keys := data.Keys()
					_ = len(keys)
				}
			})
		})
	}
}

// BenchmarkWorkflowDataArenaComparison compares WorkflowData with and without arena
func BenchmarkWorkflowDataArenaComparison(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			// Standard WorkflowData
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()
				data := workflow.NewWorkflowData("test")

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Mix of operations for realistic workload
					for j := 0; j < size; j++ {
						key := fmt.Sprintf("key%d", j)
						nodeName := fmt.Sprintf("node%d", j)

						// Set data
						data.Set(key, fmt.Sprintf("value%d", j))

						// Get data
						val, _ := data.Get(key)
						_ = val

						// Set and get node status
						data.SetNodeStatus(nodeName, workflow.Running)
						status, _ := data.GetNodeStatus(nodeName)
						_ = status

						// Set and get output
						data.SetOutput(nodeName, fmt.Sprintf("output%d", j))
						output, _ := data.GetOutput(nodeName)
						_ = output
					}
				}
			})

			// Arena-based WorkflowData
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()
				config := workflow.DefaultWorkflowDataConfig()
				config.ExpectedNodes = size
				config.ExpectedData = size * 2
				data := workflow.NewWorkflowDataWithArena("test", config)

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Reset arena at the beginning of each iteration
					data.ResetArena()

					// Mix of operations for realistic workload
					for j := 0; j < size; j++ {
						key := fmt.Sprintf("key%d", j)
						nodeName := fmt.Sprintf("node%d", j)

						// Set data
						data.Set(key, fmt.Sprintf("value%d", j))

						// Get data
						val, _ := data.Get(key)
						_ = val

						// Set and get node status
						data.SetNodeStatus(nodeName, workflow.Running)
						status, _ := data.GetNodeStatus(nodeName)
						_ = status

						// Set and get output
						data.SetOutput(nodeName, fmt.Sprintf("output%d", j))
						output, _ := data.GetOutput(nodeName)
						_ = output
					}
				}
			})

			// Arena-based WorkflowData with custom block size
			b.Run("ArenaCustomBlock", func(b *testing.B) {
				b.ReportAllocs()
				config := workflow.DefaultWorkflowDataConfig()
				config.ExpectedNodes = size
				config.ExpectedData = size * 2

				// Calculate a reasonable block size based on the expected data size
				blockSize := 4096 * (size/10 + 1) // Scale block size with data size
				data := workflow.NewWorkflowDataWithArenaBlockSize("test", config, blockSize)

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Reset arena at the beginning of each iteration
					data.ResetArena()

					// Mix of operations for realistic workload
					for j := 0; j < size; j++ {
						key := fmt.Sprintf("key%d", j)
						nodeName := fmt.Sprintf("node%d", j)

						// Set data
						data.Set(key, fmt.Sprintf("value%d", j))

						// Get data
						val, _ := data.Get(key)
						_ = val

						// Set and get node status
						data.SetNodeStatus(nodeName, workflow.Running)
						status, _ := data.GetNodeStatus(nodeName)
						_ = status

						// Set and get output
						data.SetOutput(nodeName, fmt.Sprintf("output%d", j))
						output, _ := data.GetOutput(nodeName)
						_ = output
					}
				}
			})
		})
	}
}

// Helper functions to create different DAG topologies for testing

func createLinearNodes(size int) []*workflow.Node {
	nodes := make([]*workflow.Node, size)

	// Create nodes
	for i := 0; i < size; i++ {
		nodes[i] = workflow.NewNode(fmt.Sprintf("node%d", i), createNoopAction())
	}

	// Add dependencies (linear chain)
	for i := 1; i < size; i++ {
		nodes[i].DependsOn = []*workflow.Node{nodes[i-1]}
	}

	return nodes
}

func createDiamondNodes(size int) []*workflow.Node {
	nodes := make([]*workflow.Node, size)

	// Create nodes
	for i := 0; i < size; i++ {
		nodes[i] = workflow.NewNode(fmt.Sprintf("node%d", i), createNoopAction())
	}

	// Create a diamond pattern: first node splits to multiple paths,
	// which all converge to the last node
	pathCount := 3 // Use 3 parallel paths

	// First node has no dependencies

	// Middle nodes depend on first node
	for i := 1; i <= pathCount; i++ {
		nodes[i].DependsOn = []*workflow.Node{nodes[0]}
	}

	// Create parallel paths
	for i := pathCount + 1; i < size-1; i++ {
		dep := nodes[(i-pathCount-1)%pathCount+1]
		nodes[i].DependsOn = []*workflow.Node{dep}
	}

	// Last node depends on the end of all paths
	lastDeps := make([]*workflow.Node, pathCount)
	for i := 0; i < pathCount; i++ {
		lastDeps[i] = nodes[size-2-i]
	}
	nodes[size-1].DependsOn = lastDeps

	return nodes
}

func createComplexNodes(size int) []*workflow.Node {
	nodes := make([]*workflow.Node, size)

	// Create nodes
	for i := 0; i < size; i++ {
		nodes[i] = workflow.NewNode(fmt.Sprintf("node%d", i), createNoopAction())
	}

	// Add complex dependencies - a mix of patterns
	for i := 5; i < size; i++ {
		// Each node depends on 2-3 random previous nodes
		depCount := (i % 2) + 2 // Either 2 or 3 dependencies
		deps := make([]*workflow.Node, 0, depCount)

		for d := 0; d < depCount; d++ {
			// Pick a random previous node as dependency
			depIdx := (i - 1 - d - (i % 5)) % i
			if depIdx < 0 {
				depIdx = 0
			}
			deps = append(deps, nodes[depIdx])
		}

		nodes[i].DependsOn = deps
	}

	return nodes
}

func createNoopAction() workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		return nil
	})
}
