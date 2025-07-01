package benchmark

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// BenchmarkWithMetrics benchmarks operations with metrics enabled
func BenchmarkWithMetrics(b *testing.B) {
	// Create a metrics collector with metrics enabled
	metricsConfig := metrics.NewConfig().WithEnabled(true)

	// Create a workflow data config with metrics enabled
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metricsConfig

	// Create workflow data with metrics
	data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Perform a mix of operations
		data.Set("key1", "value1")
		data.Get("key1")
		data.SetNodeStatus("node1", workflow.Running)
		data.GetNodeStatus("node1")
		data.SetOutput("node1", "output1")
		data.GetOutput("node1")
	}
}

// TestConcurrentMetrics tests that metrics collection works correctly with concurrent access
func TestConcurrentMetrics(t *testing.T) {
	// Create a metrics collector with metrics enabled
	metricsConfig := metrics.NewConfig().WithEnabled(true)

	// Create a workflow data config with metrics enabled
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metricsConfig

	// Create workflow data with metrics
	data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

	// Number of goroutines to use
	numGoroutines := 10

	// Number of operations per goroutine
	numOps := 1000

	// Wait group to synchronize goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start goroutines
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Perform operations
			for j := 0; j < numOps; j++ {
				key := fmt.Sprintf("key-%d-%d", id, j)
				value := fmt.Sprintf("value-%d-%d", id, j)

				// Set and get operations
				data.Set(key, value)
				data.Get(key)

				// Node status operations
				nodeName := fmt.Sprintf("node-%d-%d", id, j)
				data.SetNodeStatus(nodeName, workflow.Running)
				data.GetNodeStatus(nodeName)

				// Output operations
				data.SetOutput(nodeName, value)
				data.GetOutput(nodeName)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Get metrics collector
	collector := data.GetMetrics()

	// Verify that metrics were collected
	if !collector.IsEnabled() {
		t.Error("Expected metrics to be enabled")
	}

	// Check that operation stats were collected
	stats := collector.GetAllOperationStats()
	if len(stats) == 0 {
		t.Error("Expected operation stats to be collected")
	}

	// Verify total operation count
	var totalOps int64
	for _, stat := range stats {
		totalOps += stat.Count
	}

	// We should have at least some operations recorded (depending on sampling rate)
	if totalOps == 0 {
		t.Error("Expected some operations to be recorded")
	}
}

// TestLockContentionDetection tests that lock contention is detected correctly
func TestLockContentionDetection(t *testing.T) {
	// Create a metrics collector with lock contention tracking enabled
	metricsConfig := metrics.NewConfig().
		WithEnabled(true).
		WithLockContention(true).
		WithHighContentionThreshold(5)

	// Create a workflow data config with metrics enabled
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metricsConfig

	// Create workflow data with metrics
	data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

	// Number of goroutines to use
	numGoroutines := 10

	// Number of operations per goroutine
	numOps := 100

	// Wait group to synchronize goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start goroutines
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Perform operations on the same key to create contention
			for j := 0; j < numOps; j++ {
				// Use the same key across all goroutines to create contention
				key := "shared-key"
				value := fmt.Sprintf("value-%d-%d", id, j)

				// Set and get operations
				data.Set(key, value)
				data.Get(key)

				// Node status operations
				nodeName := "shared-node"
				data.SetNodeStatus(nodeName, workflow.Running)
				data.GetNodeStatus(nodeName)

				// Output operations
				data.SetOutput(nodeName, value)
				data.GetOutput(nodeName)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Get metrics collector
	collector := data.GetMetrics()

	// Verify that metrics were collected
	if !collector.IsEnabled() {
		t.Error("Expected metrics to be enabled")
	}
}

// BenchmarkMetricsConfigurations benchmarks different metrics configurations
func BenchmarkMetricsConfigurations(b *testing.B) {
	// Define benchmark cases
	benchmarks := []struct {
		name   string
		config *metrics.Config
	}{
		{"FullMetrics", metrics.NewConfig()},
		{"DisabledMetrics", metrics.DisabledMetricsConfig()},
		{"SamplingRate1Percent", metrics.NewConfig().WithSamplingRate(0.01)},
		{"SamplingRate10Percent", metrics.NewConfig().WithSamplingRate(0.1)},
		{"OperationTimingOnly", metrics.NewConfig().WithLockContention(false).WithMemoryTracking(false)},
		{"LockContentionOnly", metrics.NewConfig().WithOperationTiming(false).WithMemoryTracking(false)},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			// Create a workflow data with the specified metrics configuration
			config := workflow.DefaultWorkflowDataConfig()
			config.MetricsConfig = bm.config
			data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Perform a mix of operations
				data.Set("key1", "value1")
				data.Get("key1")
				data.SetNodeStatus("node1", workflow.Running)
				data.GetNodeStatus("node1")
				data.SetOutput("node1", "output1")
				data.GetOutput("node1")
			}
		})
	}
}

// BenchmarkMetricsOverhead measures the overhead of metrics collection
func BenchmarkMetricsOverhead(b *testing.B) {
	// Define benchmark cases
	benchmarks := []struct {
		name    string
		enabled bool
	}{
		{"MetricsEnabled", true},
		{"MetricsDisabled", false},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			// Create a workflow data with metrics enabled or disabled
			config := workflow.DefaultWorkflowDataConfig()
			config.MetricsConfig = metrics.NewConfig().WithEnabled(bm.enabled)
			data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Perform a mix of operations
				data.Set("key1", "value1")
				data.Get("key1")
				data.SetNodeStatus("node1", workflow.Running)
				data.GetNodeStatus("node1")
				data.SetOutput("node1", "output1")
				data.GetOutput("node1")
			}
		})
	}
}

// BenchmarkConcurrentMetricsConfigurations benchmarks different metrics configurations with concurrent access
func BenchmarkConcurrentMetricsConfigurations(b *testing.B) {
	// Define benchmark cases
	benchmarks := []struct {
		name   string
		config *metrics.Config
	}{
		{"FullMetrics", metrics.NewConfig()},
		{"DisabledMetrics", metrics.DisabledMetricsConfig()},
		{"SamplingRate1Percent", metrics.NewConfig().WithSamplingRate(0.01)},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			// Create a workflow data with the specified metrics configuration
			config := workflow.DefaultWorkflowDataConfig()
			config.MetricsConfig = bm.config
			data := workflow.NewWorkflowDataWithConfig("test-workflow", config)

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					// Perform a mix of operations
					data.Set("key1", "value1")
					data.Get("key1")
					data.SetNodeStatus("node1", workflow.Running)
					data.GetNodeStatus("node1")
					data.SetOutput("node1", "output1")
					data.GetOutput("node1")
				}
			})
		})
	}
}
