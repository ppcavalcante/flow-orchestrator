package benchmark

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// BenchmarkDependencyResolution measures how quickly the system can determine which nodes are ready to execute
func BenchmarkDependencyResolution(b *testing.B) {
	// Test with different DAG patterns
	b.Run("LinearDependencies", func(b *testing.B) {
		sizes := []int{10, 100, 1000}
		for _, size := range sizes {
			b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
				// Create a linear chain DAG
				dag := createLinearDAG(size)
				// Create workflow data with just the first node completed
				data := workflow.NewWorkflowData(fmt.Sprintf("linear_%d", size))
				// Mark the first node as completed to test dependency resolution
				data.SetNodeStatus("node0", workflow.Completed)

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// This is what we're benchmarking: how quickly can we determine the next node(s) to run
					readyNodes := getReadyNodes(dag, data)
					// Prevent the compiler from optimizing away the result
					if len(readyNodes) < 1 {
						b.Fatalf("Expected at least one ready node")
					}
				}
			})
		}
	})

	b.Run("ComplexDependencies", func(b *testing.B) {
		sizes := []int{10, 50, 100}
		for _, size := range sizes {
			b.Run(fmt.Sprintf("Diamond_%d", size), func(b *testing.B) {
				// Create a diamond pattern DAG
				dag := createDiamondDAG(size)
				// Create workflow data with half the nodes completed
				data := workflow.NewWorkflowData(fmt.Sprintf("diamond_%d", size))
				// Mark "start" node as completed
				data.SetNodeStatus("start", workflow.Completed)
				// Mark half of the middle nodes as completed
				for i := 0; i < size/2; i++ {
					data.SetNodeStatus(fmt.Sprintf("middle%d", i), workflow.Completed)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Time how quickly we can find nodes that are ready to execute
					readyNodes := getReadyNodes(dag, data)
					if len(readyNodes) < 1 {
						b.Fatalf("Expected at least one ready node")
					}
				}
			})

			b.Run(fmt.Sprintf("Random_%d", size), func(b *testing.B) {
				// Create a random DAG with 30% connectivity
				dag := createRandomDAG(size, 0.3)
				// Create workflow data with some nodes completed
				data := workflow.NewWorkflowData(fmt.Sprintf("random_%d", size))
				// Mark first 30% of nodes as completed
				for i := 0; i < int(float64(size)*0.3); i++ {
					data.SetNodeStatus(fmt.Sprintf("node%d", i), workflow.Completed)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Measure dependency resolution speed
					readyNodes := getReadyNodes(dag, data)
					// Use the result to prevent optimization
					if readyNodes == nil {
						b.Fatalf("Expected non-nil slice")
					}
				}
			})
		}
	})
}

// BenchmarkBuilderAPI measures the efficiency of the workflow builder API
func BenchmarkBuilderAPI(b *testing.B) {
	b.Run("SimpleWorkflow", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			builder := workflow.NewWorkflowBuilder().
				WithWorkflowID("test-workflow")

			builder.AddNode("fetch-data").
				WithAction(createSimpleAction()).
				WithRetries(3)

			builder.AddNode("process-data").
				WithAction(createSimpleAction()).
				DependsOn("fetch-data")

			builder.AddNode("save-results").
				WithAction(createSimpleAction()).
				DependsOn("process-data")

			_, _ = builder.Build()
		}
	})

	b.Run("ComplexWorkflow", func(b *testing.B) {
		nodeCounts := []int{10, 50, 100}
		for _, count := range nodeCounts {
			b.Run(fmt.Sprintf("Nodes_%d", count), func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					builder := workflow.NewWorkflowBuilder().
						WithWorkflowID(fmt.Sprintf("complex-workflow-%d", count))

					// Create a complex workflow with many nodes and dependencies
					for j := 0; j < count; j++ {
						nodeName := fmt.Sprintf("node%d", j)
						nodeBuilder := builder.AddNode(nodeName).
							WithAction(createSimpleAction())

						// Add some dependencies - each node depends on up to 3 previous nodes
						for k := 1; k <= 3; k++ {
							depIdx := j - k
							if depIdx >= 0 {
								nodeBuilder.DependsOn(fmt.Sprintf("node%d", depIdx))
							}
						}
					}

					_, _ = builder.Build()
				}
			})
		}
	})
}

// BenchmarkCoordinationOverhead measures the overhead of the coordination logic
func BenchmarkCoordinationOverhead(b *testing.B) {
	b.Run("DirectVsOrchestrated", func(b *testing.B) {
		actionCount := 10

		// First benchmark: direct function calls
		b.Run("DirectCalls", func(b *testing.B) {
			ctx := context.Background()
			data := workflow.NewWorkflowData("direct")

			// Create action functions
			actions := make([]workflow.Action, actionCount)
			for i := 0; i < actionCount; i++ {
				actions[i] = createFastAction()
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Call each action directly
				for _, action := range actions {
					_ = action.Execute(ctx, data)
				}
			}
		})

		// Second benchmark: orchestrated through workflow
		b.Run("Orchestrated", func(b *testing.B) {
			ctx := context.Background()

			// Create a linear workflow with the same actions
			builder := workflow.NewWorkflowBuilder().
				WithWorkflowID("orchestrated")

			// Add nodes in a linear chain
			var lastNode string
			for i := 0; i < actionCount; i++ {
				nodeName := fmt.Sprintf("node%d", i)
				nodeBuilder := builder.AddNode(nodeName).
					WithAction(createFastAction())

				if lastNode != "" {
					nodeBuilder.DependsOn(lastNode)
				}
				lastNode = nodeName
			}

			dag, _ := builder.Build()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				data := workflow.NewWorkflowData("orchestrated")
				_ = dag.Execute(ctx, data)
			}
		})
	})
}

// BenchmarkStateManagement measures the efficiency of saving and loading minimal workflow state
func BenchmarkStateManagement(b *testing.B) {
	b.Run("SaveWorkflowState", func(b *testing.B) {
		sizes := []int{10, 100, 1000}
		for _, nodeCount := range sizes {
			b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
				// Create a workflow state with the given number of nodes
				data := workflow.NewWorkflowData(fmt.Sprintf("state_%d", nodeCount))

				// Set status for all nodes
				for i := 0; i < nodeCount; i++ {
					data.SetNodeStatus(fmt.Sprintf("node%d", i), workflow.Completed)
					// Set minimal output for coordination
					data.SetOutput(fmt.Sprintf("node%d", i), fmt.Sprintf("signal_%d", i))
				}

				// Create an in-memory store for testing
				store := workflow.NewInMemoryStore()

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					err := store.Save(data)
					if err != nil {
						b.Fatalf("Failed to save state: %v", err)
					}
				}
			})
		}
	})

	b.Run("LoadWorkflowState", func(b *testing.B) {
		sizes := []int{10, 100, 1000}
		for _, nodeCount := range sizes {
			b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
				// Create and save a workflow state with the given number of nodes
				workflowID := fmt.Sprintf("state_%d", nodeCount)
				data := workflow.NewWorkflowData(workflowID)

				// Set status for all nodes
				for i := 0; i < nodeCount; i++ {
					data.SetNodeStatus(fmt.Sprintf("node%d", i), workflow.Completed)
					// Set minimal output for coordination
					data.SetOutput(fmt.Sprintf("node%d", i), fmt.Sprintf("signal_%d", i))
				}

				// Create an in-memory store and save the state
				store := workflow.NewInMemoryStore()
				err := store.Save(data)
				if err != nil {
					b.Fatalf("Failed to save state: %v", err)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, err := store.Load(workflowID)
					if err != nil {
						b.Fatalf("Failed to load state: %v", err)
					}
				}
			})
		}
	})
}

// BenchmarkRecoveryPattern measures how efficiently the system can recover after a failure
func BenchmarkRecoveryPattern(b *testing.B) {
	b.Run("DetermineRunnableAfterFailure", func(b *testing.B) {
		nodeCounts := []int{50, 100, 200}
		for _, nodeCount := range nodeCounts {
			b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
				// Create a diamond DAG
				dag := createDiamondDAG(nodeCount)

				// Create workflow data with 70% of nodes completed and 10% failed
				data := workflow.NewWorkflowData(fmt.Sprintf("recovery_%d", nodeCount))

				// Setup partial execution state
				completionThreshold := int(float64(nodeCount) * 0.7)
				failureThreshold := completionThreshold + int(float64(nodeCount)*0.1)

				// Mark nodes as completed or failed
				data.SetNodeStatus("start", workflow.Completed)
				for i := 0; i < nodeCount; i++ {
					nodeName := fmt.Sprintf("middle%d", i)
					if i < completionThreshold {
						data.SetNodeStatus(nodeName, workflow.Completed)
					} else if i < failureThreshold {
						data.SetNodeStatus(nodeName, workflow.Failed)
					}
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Measure how quickly we can determine what nodes to run next after failure
					readyNodes := getReadyNodes(dag, data)
					if len(readyNodes) < 1 {
						b.Fatalf("Expected at least one ready node")
					}
				}
			})
		}
	})
}

// Utility function to determine which nodes are ready to execute
func getReadyNodes(dag *workflow.DAG, data *workflow.WorkflowData) []*workflow.Node {
	var readyNodes []*workflow.Node
	// Find all nodes in the DAG
	for _, node := range dag.Nodes {
		// Skip already completed or failed nodes
		status, _ := data.GetNodeStatus(node.Name)
		if status == workflow.Completed || status == workflow.Failed || status == workflow.Running {
			continue
		}

		// Check if all dependencies are satisfied
		allDependenciesMet := true
		for _, dep := range node.DependsOn {
			depStatus, _ := data.GetNodeStatus(dep.Name)
			if depStatus != workflow.Completed {
				allDependenciesMet = false
				break
			}
		}

		if allDependenciesMet {
			readyNodes = append(readyNodes, node)
		}
	}

	return readyNodes
}

// createSimpleAction creates a simple action for benchmarking the builder API
func createSimpleAction() workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		// Just a placeholder action that does minimal work
		return nil
	})
}

// createFastAction creates a very fast action for measuring coordination overhead
func createFastAction() workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		// An action that does minimal work - just enough to not be optimized away
		data.Set("timestamp", time.Now().UnixNano())
		return nil
	})
}
