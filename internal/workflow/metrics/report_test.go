package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	testCases := []struct {
		ns       int64
		expected string
	}{
		{500, "500 ns"},
		{1500, "1.50 µs"},
		{1500000, "1.50 ms"},
		{1500000000, "1.50 s"},
	}

	for _, tc := range testCases {
		result := FormatDuration(tc.ns)
		if result != tc.expected {
			t.Errorf("FormatDuration(%d) = %s, expected %s", tc.ns, result, tc.expected)
		}
	}
}

func TestGenerateMetricsReport(t *testing.T) {
	// Reset metrics before test
	Reset()

	// Record some metrics
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	config.EnableLockContention = true

	// Set the global config
	SetGlobalConfig(config)

	// Record operation timings using global functions
	TrackOperation(OpSet, func() {
		time.Sleep(5 * time.Millisecond)
	})

	TrackOperation(OpGet, func() {
		time.Sleep(2 * time.Millisecond)
	})

	TrackOperation(OpGet, func() {
		time.Sleep(3 * time.Millisecond)
	})

	TrackOperation(OpSetStatus, func() {
		time.Sleep(1 * time.Millisecond)
	})

	TrackOperation(OpSnapshot, func() {
		time.Sleep(10 * time.Millisecond)
	})

	// Record lock contention
	RecordLockContention(2 * time.Millisecond)
	RecordLockContention(3 * time.Millisecond)

	// Generate report
	report := GenerateMetricsReport()

	// Check that the report contains expected sections
	sections := []string{
		"=== Workflow System Metrics Report ===",
		"== Data Operations ==",
		"== Node Status Operations ==",
		"== Node Output Operations ==",
		"== Lock Operations ==",
		"== Other Operations ==",
		"== Lock Contention Stats ==",
		"== Performance Hotspots ==",
		"== Recommendations ==",
	}

	for _, section := range sections {
		if !strings.Contains(report, section) {
			t.Errorf("Report should contain section '%s'", section)
		}
	}

	// Check that the report contains operation stats
	operations := []string{
		"set:", "get:", "set_status:", "snapshot:",
	}

	for _, op := range operations {
		if !strings.Contains(report, op) {
			t.Errorf("Report should contain operation '%s'", op)
		}
	}

	// Check that the report contains contention stats
	if !strings.Contains(report, "Contention Count: 2") {
		t.Error("Report should contain contention count")
	}

	// Check that the report contains hotspots
	if !strings.Contains(report, "Top Operations by Total Time:") {
		t.Error("Report should contain hotspots by total time")
	}

	if !strings.Contains(report, "Top Operations by Average Time:") {
		t.Error("Report should contain hotspots by average time")
	}
}

func TestFormatOperationStats(t *testing.T) {
	// Create some stats
	stats := map[OperationType]OperationStats{
		OpSet: {
			Count:       2,
			TotalTimeNs: 10000000,
			MinTimeNs:   4000000,
			MaxTimeNs:   6000000,
			AvgTimeNs:   5000000,
			Active:      0,
		},
	}

	// Format the stats
	var sb strings.Builder
	ops := []OperationType{OpSet}
	formatOperationStats(&sb, ops, stats)

	result := sb.String()

	// Check that the result contains expected information
	expectedStrings := []string{
		"set:",
		"Count: 2",
		"Min Time:",
		"Max Time:",
		"Avg Time:",
		"Active: 0",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(result, s) {
			t.Errorf("formatOperationStats result should contain '%s'", s)
		}
	}

	// Test with empty stats
	sb.Reset()
	formatOperationStats(&sb, []OperationType{OpDelete}, stats)
	if sb.Len() != 0 {
		t.Error("formatOperationStats should not write anything for non-existent stats")
	}
}

func TestIdentifyHotspots(t *testing.T) {
	// Create some stats
	stats := map[OperationType]OperationStats{
		OpSet: {
			Count:       10,
			TotalTimeNs: 100000000,
			AvgTimeNs:   10000000,
		},
		OpGet: {
			Count:       20,
			TotalTimeNs: 40000000,
			AvgTimeNs:   2000000,
		},
		OpDelete: {
			Count:       5,
			TotalTimeNs: 25000000,
			AvgTimeNs:   5000000,
		},
	}

	// Identify hotspots
	var sb strings.Builder
	identifyHotspots(&sb, stats)

	result := sb.String()

	// Check that the result contains expected information
	if !strings.Contains(result, "Top Operations by Total Time:") {
		t.Error("identifyHotspots result should contain 'Top Operations by Total Time:'")
	}

	if !strings.Contains(result, "Top Operations by Average Time:") {
		t.Error("identifyHotspots result should contain 'Top Operations by Average Time:'")
	}

	// The first operation by total time should be OpSet
	if !strings.Contains(result, "set: 100.00 ms") {
		t.Error("OpSet should be the top operation by total time")
	}

	// The first operation by average time should be OpSet
	if !strings.Contains(result, "set: 10.00 ms/call") {
		t.Error("OpSet should be the top operation by average time")
	}

	// Test with empty stats
	sb.Reset()
	identifyHotspots(&sb, map[OperationType]OperationStats{})
	if sb.Len() != 0 {
		t.Error("identifyHotspots should not write anything for empty stats")
	}
}

func TestGenerateRecommendations(t *testing.T) {
	// Create some stats with high contention
	stats := map[OperationType]OperationStats{
		OpSnapshot: {
			Count:       10,
			TotalTimeNs: 20000000,
			AvgTimeNs:   2000000, // 2ms, above the 1ms threshold
		},
		OpLoadSnapshot: {
			Count:       5,
			TotalTimeNs: 10000000,
			AvgTimeNs:   2000000, // 2ms, above the 1ms threshold
		},
		OpIsNodeRunnable: {
			Count:       100,
			TotalTimeNs: 20000000,
			AvgTimeNs:   200000, // 200µs, above the 100µs threshold
		},
	}

	contentionStats := LockContentionStats{
		Count:       10,
		TotalTimeNs: 1000000,
		MaxTimeNs:   200000,
		AvgTimeNs:   100000, // 100µs, above the 50µs threshold
	}

	// Generate recommendations
	var sb strings.Builder
	generateRecommendations(&sb, stats, contentionStats)

	result := sb.String()

	// Check that the result contains expected recommendations
	expectedRecommendations := []string{
		"High lock contention detected",
		"Snapshot operations are expensive",
		"LoadSnapshot operations are expensive",
		"Dependency resolution is expensive",
	}

	for _, rec := range expectedRecommendations {
		if !strings.Contains(result, rec) {
			t.Errorf("generateRecommendations result should contain '%s'", rec)
		}
	}

	// Test with no issues
	sb.Reset()
	noIssueStats := map[OperationType]OperationStats{
		OpSet: {
			Count:       10,
			TotalTimeNs: 1000000,
			AvgTimeNs:   100000,
		},
	}

	noContentionStats := LockContentionStats{
		Count:       1,
		TotalTimeNs: 10000,
		MaxTimeNs:   10000,
		AvgTimeNs:   10000, // 10µs, below the 50µs threshold
	}

	generateRecommendations(&sb, noIssueStats, noContentionStats)

	result = sb.String()
	if !strings.Contains(result, "No specific optimizations recommended") {
		t.Error("generateRecommendations should indicate no optimizations when there are no issues")
	}
}

func TestMinInt(t *testing.T) {
	testCases := []struct {
		a, b, expected int
	}{
		{5, 10, 5},
		{10, 5, 5},
		{0, 0, 0},
		{-5, 5, -5},
		{5, -5, -5},
	}

	for _, tc := range testCases {
		result := minInt(tc.a, tc.b)
		if result != tc.expected {
			t.Errorf("minInt(%d, %d) = %d, expected %d", tc.a, tc.b, result, tc.expected)
		}
	}
}
