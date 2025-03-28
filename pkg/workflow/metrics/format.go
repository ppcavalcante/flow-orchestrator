// Package metrics provides functionality for collecting and reporting performance metrics
// for workflow execution.
package metrics

import (
	internal "github.com/pparaujo/flow-orchestrator/internal/workflow/metrics"
)

// FormatDuration formats a duration in nanoseconds to a human-readable string
func FormatDuration(ns int64) string {
	return internal.FormatDuration(ns)
}
