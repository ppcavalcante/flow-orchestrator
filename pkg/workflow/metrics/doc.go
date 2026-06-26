/*
Package metrics provides functionality for collecting and reporting performance metrics
for workflow execution in the Flow Orchestrator.

This package offers a simplified public API for the internal metrics system, allowing users
to track operation performance, generate reports, and configure metrics collection without
exposing internal implementation details.

Metrics are per-instance: each collector owns its own state, so there is no process-global
metrics. The workflow data plane uses its own collector internally; create your own collector
when you want to track operations directly.

Basic usage:

	// Create a collector (enabled, full sampling)
	collector := metrics.NewMetricsCollector()

	// Track an operation
	collector.TrackOperation(metrics.OpGet, func() {
		// Your operation code here
		value := workflow.GetNodeOutput("node1")
	})

	// Get statistics for a specific operation type
	stats, exists := collector.GetOperationStats(metrics.OpGet)
	if exists {
		fmt.Printf("Get operations: %d, Avg time: %s\n",
			stats.Count, metrics.FormatDuration(stats.AvgTimeNs))
	}

	// Generate a report
	reporter := metrics.NewReporter(collector)
	reporter.WriteReport(os.Stdout)

Configuration:

	// Create a custom configuration
	config := metrics.NewConfig().
		WithEnabled(true).
		WithSamplingRate(0.1).  // Sample 10% of operations
		WithOperationTiming(true).
		WithSlowOperationThreshold(time.Millisecond * 100)

	// Build a collector with that configuration
	collector := metrics.NewMetricsCollectorWithConfig(config.GetInternalConfig())
*/
package metrics
