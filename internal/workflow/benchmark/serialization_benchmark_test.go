package benchmark

import (
	"fmt"
	"os"
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// BenchmarkSerialization compares the performance of different serialization formats
func BenchmarkSerialization(b *testing.B) {
	// Setup: Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "workflow-serialization-benchmark")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create benchmarks for different workflow sizes
	for _, nodeCount := range []int{10, 100, 1000} {
		// Create test workflows with different sizes
		b.Run(fmt.Sprintf("Workflow_%dNodes", nodeCount), func(b *testing.B) {
			// Setup a workflow with the specified number of nodes
			workflowData := createTestWorkflow(b, nodeCount)

			// JSON Serialization
			b.Run("JSON_Save", func(b *testing.B) {
				jsonPath := fmt.Sprintf("%s/workflow_%d.json", tempDir, nodeCount)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					err := workflowData.SaveToJSON(jsonPath)
					if err != nil {
						b.Fatalf("Failed to save JSON: %v", err)
					}
				}
			})

			// JSON Deserialization
			jsonPath := fmt.Sprintf("%s/workflow_%d.json", tempDir, nodeCount)
			err = workflowData.SaveToJSON(jsonPath)
			if err != nil {
				b.Fatalf("Failed to create JSON file for load test: %v", err)
			}

			b.Run("JSON_Load", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					data := workflow.NewWorkflowData(fmt.Sprintf("benchmark_%d", i))
					err := data.LoadFromJSON(jsonPath)
					if err != nil {
						b.Fatalf("Failed to load JSON: %v", err)
					}
				}
			})

			// FlatBuffer Serialization
			b.Run("FlatBuffer_Save", func(b *testing.B) {
				fbPath := fmt.Sprintf("%s/workflow_%d.fb", tempDir, nodeCount)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					err := workflowData.SaveToFlatBuffer(fbPath)
					if err != nil {
						b.Fatalf("Failed to save FlatBuffer: %v", err)
					}
				}
			})

			// FlatBuffer Deserialization
			fbPath := fmt.Sprintf("%s/workflow_%d.fb", tempDir, nodeCount)
			err = workflowData.SaveToFlatBuffer(fbPath)
			if err != nil {
				b.Fatalf("Failed to create FlatBuffer file for load test: %v", err)
			}

			b.Run("FlatBuffer_Load", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					data := workflow.NewWorkflowData(fmt.Sprintf("benchmark_%d", i))
					err := data.LoadFromFlatBuffer(fbPath)
					if err != nil {
						b.Fatalf("Failed to load FlatBuffer: %v", err)
					}
				}
			})
		})
	}
}

// createTestWorkflow creates a test workflow with the specified number of nodes
func createTestWorkflow(b *testing.B, nodeCount int) *workflow.WorkflowData {
	// Create a workflow data config with the expected capacity
	dataConfig := workflow.DefaultWorkflowDataConfig()
	dataConfig.ExpectedNodes = nodeCount
	dataConfig.ExpectedData = nodeCount * 2

	// Create workflow data with the specified configuration
	workflowData := workflow.NewWorkflowDataWithConfig(fmt.Sprintf("test_workflow_%d", nodeCount), dataConfig)

	// Add general data
	workflowData.Set("name", fmt.Sprintf("Test Workflow %d", nodeCount))
	workflowData.Set("description", "Benchmark test workflow")
	workflowData.Set("created_at", "2023-06-15T10:00:00Z")
	workflowData.Set("priority", 5)
	workflowData.Set("is_critical", true)
	workflowData.Set("retry_limit", 3)

	// Add a complex data structure
	configData := map[string]interface{}{
		"timeout":       30,
		"max_retries":   5,
		"backoff_start": 1.5,
		"backoff_max":   60.0,
		"tags":          []string{"performance", "benchmark", "test"},
		"environment": map[string]string{
			"WORKFLOW_ENV": "benchmark",
			"DEBUG":        "true",
			"LOG_LEVEL":    "info",
		},
	}
	workflowData.Set("config", configData)

	// Add node statuses and outputs
	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node_%d", i)

		// Set node status (distribute statuses across the spectrum)
		switch i % 5 {
		case 0:
			workflowData.SetNodeStatus(nodeName, workflow.Pending)
		case 1:
			workflowData.SetNodeStatus(nodeName, workflow.Running)
		case 2:
			workflowData.SetNodeStatus(nodeName, workflow.Completed)
		case 3:
			workflowData.SetNodeStatus(nodeName, workflow.Failed)
		case 4:
			workflowData.SetNodeStatus(nodeName, workflow.Skipped)
		}

		// Set node output (different types for different nodes)
		switch i % 4 {
		case 0:
			workflowData.SetOutput(nodeName, fmt.Sprintf("Output from node %d", i))
		case 1:
			workflowData.SetOutput(nodeName, i*10)
		case 2:
			workflowData.SetOutput(nodeName, i%2 == 0)
		case 3:
			// Complex output
			workflowData.SetOutput(nodeName, map[string]interface{}{
				"status_code": 200,
				"message":     fmt.Sprintf("Node %d completed successfully", i),
				"timestamp":   "2023-06-15T10:05:00Z",
				"metrics": map[string]float64{
					"duration_ms": float64(i * 100),
					"cpu_usage":   float64(i) * 1.5,
					"memory_mb":   float64(i*10) + 50.0,
				},
			})
		}
	}

	return workflowData
}
