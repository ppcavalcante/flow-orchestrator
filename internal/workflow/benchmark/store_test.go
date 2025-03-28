package benchmark

import (
	"fmt"
	"os"
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// createBenchmarkDataForStore creates a workflow data with n nodes for benchmarking storage
func createBenchmarkDataForStore(size int) *workflow.WorkflowData {
	data := workflow.NewWorkflowData(fmt.Sprintf("bench-workflow-%d", size))

	// Add node statuses and outputs
	for i := 0; i < size; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		data.SetNodeStatus(nodeName, workflow.Completed)
		data.SetOutput(nodeName, fmt.Sprintf("output-%d", i))
	}

	// Add some workflow data
	for i := 0; i < size; i++ {
		key := fmt.Sprintf("key%d", i)
		value := fmt.Sprintf("value-%d", i)
		data.Set(key, value)
	}

	return data
}

// BenchmarkJSONSave benchmarks saving workflow data to JSON
func BenchmarkJSONSave(b *testing.B) {
	sizes := []int{10, 100, 1000}
	for _, size := range sizes {
		data := createBenchmarkDataForStore(size)

		// Create temporary directory
		tempDir, err := os.MkdirTemp("", "json-bench-*")
		if err != nil {
			b.Fatalf("Failed to create temp dir: %v", err)
		}

		// Create store
		store, err := workflow.NewJSONFileStore(tempDir)
		if err != nil {
			b.Fatalf("Failed to create store: %v", err)
		}

		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := store.Save(data)
				if err != nil {
					b.Fatalf("Failed to save: %v", err)
				}
			}
		})

		// Clean up
		os.RemoveAll(tempDir)
	}
}

// BenchmarkJSONLoad benchmarks loading workflow data from JSON
func BenchmarkJSONLoad(b *testing.B) {
	sizes := []int{10, 100, 1000}
	for _, size := range sizes {
		data := createBenchmarkDataForStore(size)

		// Create temporary directory
		tempDir, err := os.MkdirTemp("", "json-bench-*")
		if err != nil {
			b.Fatalf("Failed to create temp dir: %v", err)
		}

		// Create store
		store, err := workflow.NewJSONFileStore(tempDir)
		if err != nil {
			b.Fatalf("Failed to create store: %v", err)
		}

		// Save first
		err = store.Save(data)
		if err != nil {
			b.Fatalf("Failed to save: %v", err)
		}

		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := store.Load(data.GetWorkflowID())
				if err != nil {
					b.Fatalf("Failed to load: %v", err)
				}
			}
		})

		// Clean up
		os.RemoveAll(tempDir)
	}
}

// Uncomment and use these benchmarks once FlatBuffersStore is implemented
/*
// BenchmarkFlatBuffersSave benchmarks saving workflow data to FlatBuffers
func BenchmarkFlatBuffersSave(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}
	for _, size := range sizes {
		data := createBenchmarkDataForStore(size)
		store := workflow.NewFlatBuffersStore(".")

		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := store.Save(data)
				if err != nil {
					b.Fatalf("Failed to save: %v", err)
				}
			}
		})

		// Clean up the file
		os.Remove(fmt.Sprintf("%s.fbs", data.GetID()))
	}
}

// BenchmarkFlatBuffersLoad benchmarks loading workflow data from FlatBuffers
func BenchmarkFlatBuffersLoad(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}
	for _, size := range sizes {
		data := createBenchmarkDataForStore(size)
		store := workflow.NewFlatBuffersStore(".")

		// Save first
		err := store.Save(data)
		if err != nil {
			b.Fatalf("Failed to save: %v", err)
		}

		workflowID := data.GetID()

		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := store.Load(workflowID)
				if err != nil {
					b.Fatalf("Failed to load: %v", err)
				}
			}
		})

		// Clean up the file
		os.Remove(fmt.Sprintf("%s.fbs", workflowID))
	}
}

// Benchmark a 100MB workflow data
func BenchmarkLargeDataSerialization(b *testing.B) {
	// Create a large workflow data (approximately 100MB)
	data := workflow.NewWorkflowData("large-workflow")

	// Add 100,000 nodes with statuses and outputs (1KB each)
	for i := 0; i < 100000; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		data.SetNodeStatus(nodeName, workflow.Completed)

		// Create a 1KB output
		output := make([]byte, 1024)
		for j := range output {
			output[j] = byte(i % 256)
		}
		data.SetOutput(nodeName, string(output))
	}

	workflowID := data.GetID()

	// Run benchmark for JSON
	b.Run("JSON_100MB", func(b *testing.B) {
		store := workflow.NewJSONStore(".")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			err := store.Save(data)
			if err != nil {
				b.Fatalf("Failed to save: %v", err)
			}
			_, err = store.Load(workflowID)
			if err != nil {
				b.Fatalf("Failed to load: %v", err)
			}
		}
		// Clean up
		os.Remove(fmt.Sprintf("%s.json", workflowID))
	})

	// Run benchmark for FlatBuffers
	b.Run("FlatBuffers_100MB", func(b *testing.B) {
		store := workflow.NewFlatBuffersStore(".")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			err := store.Save(data)
			if err != nil {
				b.Fatalf("Failed to save: %v", err)
			}
			_, err = store.Load(workflowID)
			if err != nil {
				b.Fatalf("Failed to load: %v", err)
			}
		}
		// Clean up
		os.Remove(fmt.Sprintf("%s.fbs", workflowID))
	})
}

// Benchmark comparing JSON and FlatBuffers file sizes
func BenchmarkSerializationSizes(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}

	fmt.Println("File Size Comparison (JSON vs FlatBuffers):")
	fmt.Println("Size\tJSON\tFlatBuffers\tRatio")

	for _, size := range sizes {
		data := createBenchmarkDataForStore(size)
		workflowID := data.GetID()

		// JSON
		jsonStore := workflow.NewJSONStore(".")
		err := jsonStore.Save(data)
		if err != nil {
			b.Fatalf("Failed to save JSON: %v", err)
		}

		jsonFilePath := fmt.Sprintf("%s.json", workflowID)
		jsonFileInfo, err := os.Stat(jsonFilePath)
		if err != nil {
			b.Fatalf("Failed to stat JSON file: %v", err)
		}
		jsonSize := jsonFileInfo.Size()

		// FlatBuffers
		fbsStore := workflow.NewFlatBuffersStore(".")
		err = fbsStore.Save(data)
		if err != nil {
			b.Fatalf("Failed to save FlatBuffers: %v", err)
		}

		fbsFilePath := fmt.Sprintf("%s.fbs", workflowID)
		fbsFileInfo, err := os.Stat(fbsFilePath)
		if err != nil {
			b.Fatalf("Failed to stat FlatBuffers file: %v", err)
		}
		fbsSize := fbsFileInfo.Size()

		ratio := float64(fbsSize) / float64(jsonSize)
		fmt.Printf("%d\t%d\t%d\t%.2f\n", size, jsonSize, fbsSize, ratio)

		// Clean up
		os.Remove(jsonFilePath)
		os.Remove(fbsFilePath)
	}
}
*/
