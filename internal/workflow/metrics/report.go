package metrics

import (
	"fmt"
	"sort"
	"strings"
)

// FormatDuration formats a duration in nanoseconds to a human-readable string
func FormatDuration(ns int64) string {
	switch {
	case ns < 1000:
		return fmt.Sprintf("%d ns", ns)
	case ns < 1000000:
		return fmt.Sprintf("%.2f µs", float64(ns)/1000)
	case ns < 1000000000:
		return fmt.Sprintf("%.2f ms", float64(ns)/1000000)
	default:
		return fmt.Sprintf("%.2f s", float64(ns)/1000000000)
	}
}

// GenerateMetricsReport generates a formatted metrics report
func GenerateMetricsReport() string {
	var sb strings.Builder

	sb.WriteString("=== Workflow System Metrics Report ===\n\n")

	// Get all operation stats
	allStats := GetAllOperationStats()

	// Sort operations by category
	dataOps := []OperationType{OpSet, OpGet, OpDelete}
	nodeOps := []OperationType{OpSetStatus, OpGetStatus}
	outputOps := []OperationType{OpSetOutput, OpGetOutput}
	lockOps := []OperationType{OpLockAcquire, OpLockRelease, OpRLockAcquire, OpRLockRelease}
	otherOps := []OperationType{OpIsNodeRunnable, OpSnapshot, OpLoadSnapshot}

	// Format operation stats by category
	sb.WriteString("== Data Operations ==\n")
	formatOperationStats(&sb, dataOps, allStats)

	sb.WriteString("\n== Node Status Operations ==\n")
	formatOperationStats(&sb, nodeOps, allStats)

	sb.WriteString("\n== Node Output Operations ==\n")
	formatOperationStats(&sb, outputOps, allStats)

	sb.WriteString("\n== Lock Operations ==\n")
	formatOperationStats(&sb, lockOps, allStats)

	sb.WriteString("\n== Other Operations ==\n")
	formatOperationStats(&sb, otherOps, allStats)

	// Format lock contention stats
	sb.WriteString("\n== Lock Contention Stats ==\n")
	contentionStats := GetLockContentionStats()
	sb.WriteString(fmt.Sprintf("Contention Count: %d\n", contentionStats.Count))
	sb.WriteString(fmt.Sprintf("Total Contention Time: %s\n", FormatDuration(contentionStats.TotalTimeNs)))
	sb.WriteString(fmt.Sprintf("Max Contention Time: %s\n", FormatDuration(contentionStats.MaxTimeNs)))
	if contentionStats.Count > 0 {
		sb.WriteString(fmt.Sprintf("Avg Contention Time: %s\n", FormatDuration(contentionStats.AvgTimeNs)))
	}

	// Find hotspots
	sb.WriteString("\n== Performance Hotspots ==\n")
	identifyHotspots(&sb, allStats)

	// Add recommendations
	sb.WriteString("\n== Recommendations ==\n")
	generateRecommendations(&sb, allStats, contentionStats)

	return sb.String()
}

// formatOperationStats formats operation stats for a category
func formatOperationStats(sb *strings.Builder, ops []OperationType, allStats map[OperationType]OperationStats) {
	for _, op := range ops {
		stats, exists := allStats[op]
		if !exists {
			continue
		}

		sb.WriteString(fmt.Sprintf("%s:\n", op))
		sb.WriteString(fmt.Sprintf("  Count: %d\n", stats.Count))
		if stats.Count > 0 {
			sb.WriteString(fmt.Sprintf("  Min Time: %s\n", FormatDuration(stats.MinTimeNs)))
			sb.WriteString(fmt.Sprintf("  Max Time: %s\n", FormatDuration(stats.MaxTimeNs)))
			sb.WriteString(fmt.Sprintf("  Avg Time: %s\n", FormatDuration(stats.AvgTimeNs)))
		}
		sb.WriteString(fmt.Sprintf("  Active: %d\n", stats.Active))
	}
}

// identifyHotspots identifies performance hotspots
func identifyHotspots(sb *strings.Builder, stats map[OperationType]OperationStats) {
	// Sort operations by total time
	type opStat struct {
		op    OperationType
		stats OperationStats
	}

	var sortedStats []opStat
	for op, s := range stats {
		if s.Count > 0 {
			sortedStats = append(sortedStats, opStat{op, s})
		}
	}

	sort.Slice(sortedStats, func(i, j int) bool {
		return sortedStats[i].stats.TotalTimeNs > sortedStats[j].stats.TotalTimeNs
	})

	// Display top 5 hotspots by total time
	if len(sortedStats) > 0 {
		sb.WriteString("Top Operations by Total Time:\n")
		count := minInt(5, len(sortedStats))
		for i := 0; i < count; i++ {
			op := sortedStats[i]
			sb.WriteString(fmt.Sprintf("  %s: %s (%d calls)\n",
				op.op,
				FormatDuration(op.stats.TotalTimeNs),
				op.stats.Count))
		}
	}

	// Sort operations by average time
	sort.Slice(sortedStats, func(i, j int) bool {
		return sortedStats[i].stats.AvgTimeNs > sortedStats[j].stats.AvgTimeNs
	})

	// Display top 5 hotspots by average time
	if len(sortedStats) > 0 {
		sb.WriteString("\nTop Operations by Average Time:\n")
		count := minInt(5, len(sortedStats))
		for i := 0; i < count; i++ {
			op := sortedStats[i]
			sb.WriteString(fmt.Sprintf("  %s: %s/call\n",
				op.op,
				FormatDuration(op.stats.AvgTimeNs)))
		}
	}
}

// generateRecommendations generates optimization recommendations
func generateRecommendations(sb *strings.Builder, stats map[OperationType]OperationStats, contention LockContentionStats) {
	hasRecommendations := false

	// Check for lock contention
	if contention.Count > 0 && contention.AvgTimeNs > 50000 { // 50µs threshold
		sb.WriteString("- High lock contention detected. Consider:\n")
		sb.WriteString("  * Reducing critical section size in operations\n")
		sb.WriteString("  * Using more fine-grained locking\n")
		sb.WriteString("  * Restructuring data access patterns to minimize contention\n")
		hasRecommendations = true
	}

	// Check for expensive Snapshot/LoadSnapshot operations
	if snapshotStats, ok := stats[OpSnapshot]; ok && snapshotStats.Count > 0 && snapshotStats.AvgTimeNs > 1000000 { // 1ms threshold
		sb.WriteString("- Snapshot operations are expensive. Consider:\n")
		sb.WriteString("  * Reducing snapshot frequency\n")
		sb.WriteString("  * Implementing incremental snapshots\n")
		sb.WriteString("  * Using more efficient serialization methods\n")
		hasRecommendations = true
	}

	if loadStats, ok := stats[OpLoadSnapshot]; ok && loadStats.Count > 0 && loadStats.AvgTimeNs > 1000000 { // 1ms threshold
		sb.WriteString("- LoadSnapshot operations are expensive. Consider:\n")
		sb.WriteString("  * Optimizing deserialization\n")
		sb.WriteString("  * Using lazy loading for large snapshots\n")
		hasRecommendations = true
	}

	// Check for expensive dependency resolution
	if runnableStats, ok := stats[OpIsNodeRunnable]; ok && runnableStats.Count > 0 && runnableStats.AvgTimeNs > 100000 { // 100µs threshold
		sb.WriteString("- Dependency resolution is expensive. Consider:\n")
		sb.WriteString("  * Optimizing the IsNodeRunnable method\n")
		sb.WriteString("  * Caching dependency resolution results\n")
		sb.WriteString("  * Using more specialized data structures for tracking dependencies\n")
		hasRecommendations = true
	}

	if !hasRecommendations {
		sb.WriteString("No specific optimizations recommended at this time.\n")
		sb.WriteString("The system is performing within expected parameters.\n")
	}
}

// minInt returns the minimum of two integers
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
