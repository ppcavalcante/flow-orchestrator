// Package metrics provides functionality for collecting and reporting performance metrics
// for workflow execution.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Reporter provides methods for generating human-readable reports of metrics
type Reporter struct {
	collector *MetricsCollector
}

// NewReporter creates a new metrics reporter for the given collector
func NewReporter(collector *MetricsCollector) *Reporter {
	return &Reporter{
		collector: collector,
	}
}

// DefaultReporter returns a reporter for the default metrics collector
func DefaultReporter() *Reporter {
	return NewReporter(defaultCollector)
}

// WriteReport writes a human-readable report of the collected metrics to the given writer
func (r *Reporter) WriteReport(w io.Writer) error {
	report := r.generateReport()
	_, err := fmt.Fprint(w, report)
	return err
}

// WriteOperationTimingReport writes a report focused on operation timing
func (r *Reporter) WriteOperationTimingReport(w io.Writer) error {
	report := r.generateOperationTimingReport()
	_, err := fmt.Fprint(w, report)
	return err
}

// WriteSlowOperationsReport writes a report of operations that exceeded the slow threshold
func (r *Reporter) WriteSlowOperationsReport(w io.Writer, threshold time.Duration) error {
	report := r.generateSlowOperationsReport(threshold)
	_, err := fmt.Fprint(w, report)
	return err
}

// generateReport generates a complete metrics report
func (r *Reporter) generateReport() string {
	var sb strings.Builder

	sb.WriteString("=== Workflow System Metrics Report ===\n\n")

	// Get all operation stats
	allStats := r.collector.GetAllOperationStats()

	// Sort operations by category
	dataOps := []OperationType{OpSet, OpGet, OpDelete}
	nodeOps := []OperationType{OpSetStatus, OpGetStatus}
	outputOps := []OperationType{OpSetOutput, OpGetOutput}
	lockOps := []OperationType{OpLockAcquire, OpLockRelease, OpRLockAcquire, OpRLockRelease}
	otherOps := []OperationType{OpIsNodeRunnable, OpSnapshot, OpLoadSnapshot}

	// Format operation stats by category
	sb.WriteString("== Data Operations ==\n")
	r.formatOperationStats(&sb, dataOps, allStats)

	sb.WriteString("\n== Node Status Operations ==\n")
	r.formatOperationStats(&sb, nodeOps, allStats)

	sb.WriteString("\n== Node Output Operations ==\n")
	r.formatOperationStats(&sb, outputOps, allStats)

	sb.WriteString("\n== Lock Operations ==\n")
	r.formatOperationStats(&sb, lockOps, allStats)

	sb.WriteString("\n== Other Operations ==\n")
	r.formatOperationStats(&sb, otherOps, allStats)

	return sb.String()
}

// generateOperationTimingReport generates a report focused on operation timing
func (r *Reporter) generateOperationTimingReport() string {
	var sb strings.Builder

	sb.WriteString("=== Operation Timing Report ===\n\n")

	// Get all operation stats
	allStats := r.collector.GetAllOperationStats()

	// Sort operations by average time
	var operations []struct {
		Type  OperationType
		Stats OperationStats
	}

	for opType, stats := range allStats {
		if stats.Count > 0 {
			operations = append(operations, struct {
				Type  OperationType
				Stats OperationStats
			}{opType, stats})
		}
	}

	sort.Slice(operations, func(i, j int) bool {
		return operations[i].Stats.AvgTimeNs > operations[j].Stats.AvgTimeNs
	})

	// Format operation stats
	for _, op := range operations {
		sb.WriteString(fmt.Sprintf("%s:\n", op.Type))
		sb.WriteString(fmt.Sprintf("  Count: %d\n", op.Stats.Count))
		sb.WriteString(fmt.Sprintf("  Min Time: %s\n", FormatDuration(op.Stats.MinTimeNs)))
		sb.WriteString(fmt.Sprintf("  Max Time: %s\n", FormatDuration(op.Stats.MaxTimeNs)))
		sb.WriteString(fmt.Sprintf("  Avg Time: %s\n", FormatDuration(op.Stats.AvgTimeNs)))
		sb.WriteString("\n")
	}

	return sb.String()
}

// generateSlowOperationsReport generates a report of operations that exceeded the slow threshold
func (r *Reporter) generateSlowOperationsReport(threshold time.Duration) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("=== Slow Operations Report (>%s) ===\n\n", threshold))

	// Get all operation stats
	allStats := r.collector.GetAllOperationStats()

	// Find slow operations
	var slowOps []struct {
		Type  OperationType
		Stats OperationStats
	}

	thresholdNs := threshold.Nanoseconds()
	for opType, stats := range allStats {
		if stats.Count > 0 && stats.MaxTimeNs > thresholdNs {
			slowOps = append(slowOps, struct {
				Type  OperationType
				Stats OperationStats
			}{opType, stats})
		}
	}

	if len(slowOps) == 0 {
		sb.WriteString("No operations exceeded the threshold.\n")
		return sb.String()
	}

	// Sort by max time
	sort.Slice(slowOps, func(i, j int) bool {
		return slowOps[i].Stats.MaxTimeNs > slowOps[j].Stats.MaxTimeNs
	})

	// Format slow operations
	for _, op := range slowOps {
		sb.WriteString(fmt.Sprintf("%s:\n", op.Type))
		sb.WriteString(fmt.Sprintf("  Count: %d\n", op.Stats.Count))
		sb.WriteString(fmt.Sprintf("  Max Time: %s\n", FormatDuration(op.Stats.MaxTimeNs)))
		sb.WriteString(fmt.Sprintf("  Avg Time: %s\n", FormatDuration(op.Stats.AvgTimeNs)))
		sb.WriteString("\n")
	}

	return sb.String()
}

// formatOperationStats formats operation stats for a category
func (r *Reporter) formatOperationStats(sb *strings.Builder, ops []OperationType, allStats map[OperationType]OperationStats) {
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
	}
}

// WriteReport writes a report using the default reporter
func WriteReport(w io.Writer) error {
	return DefaultReporter().WriteReport(w)
}

// WriteOperationTimingReport writes an operation timing report using the default reporter
func WriteOperationTimingReport(w io.Writer) error {
	return DefaultReporter().WriteOperationTimingReport(w)
}

// WriteSlowOperationsReport writes a slow operations report using the default reporter
func WriteSlowOperationsReport(w io.Writer, threshold time.Duration) error {
	return DefaultReporter().WriteSlowOperationsReport(w, threshold)
}
