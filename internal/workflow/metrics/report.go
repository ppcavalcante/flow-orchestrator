package metrics

import (
	"fmt"
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
