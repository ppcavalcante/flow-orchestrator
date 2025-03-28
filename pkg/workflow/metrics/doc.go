/*
Package metrics provides functionality for collecting and reporting performance metrics
for workflow execution in the Flow Orchestrator.

This package offers a simplified public API for the internal metrics system, allowing users
to track operation performance, generate reports, and configure metrics collection without
exposing internal implementation details.

Basic usage:

	// Enable metrics collection (enabled by default)
	metrics.Enable()

	// Track an operation
	metrics.TrackOperation(metrics.OpGet, func() {
		// Your operation code here
		value := workflow.GetNodeOutput("node1")
	})

	// Get statistics for a specific operation type
	stats, exists := metrics.GetOperationStats(metrics.OpGet)
	if exists {
		fmt.Printf("Get operations: %d, Avg time: %s\n",
			stats.Count, metrics.FormatDuration(stats.AvgTimeNs))
	}

	// Generate a report
	var buf bytes.Buffer
	metrics.WriteReport(&buf)
	fmt.Println(buf.String())

Configuration:

	// Create a custom configuration
	config := metrics.NewConfig().
		WithEnabled(true).
		WithSamplingRate(0.1).  // Sample 10% of operations
		WithOperationTiming(true).
		WithSlowOperationThreshold(time.Millisecond * 100)

	// Apply the configuration to the default collector
	config.Apply()

For more advanced usage, you can create custom collectors:

	// Create a custom collector
	collector := metrics.NewMetricsCollector().
		WithSamplingRate(0.5)  // Sample 50% of operations

	// Use the custom collector
	collector.TrackOperation(metrics.OpSet, func() {
		// Your operation code here
	})

	// Generate a report for the custom collector
	reporter := metrics.NewReporter(collector)
	reporter.WriteReport(os.Stdout)
*/
package metrics
