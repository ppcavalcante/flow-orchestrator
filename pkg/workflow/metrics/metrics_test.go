package metrics

import (
	"testing"
	"time"
)

const (
	TestOp  OperationType = "test_op"
	TestOp1 OperationType = "op1"
	TestOp2 OperationType = "op2"
	TestOp3 OperationType = "op3"
)

func TestMetricsCollector(t *testing.T) {
	t.Run("BasicOperations", func(t *testing.T) {
		collector := NewMetricsCollector()

		// Test enable/disable
		if !collector.IsEnabled() {
			t.Error("Metrics collector should be enabled by default")
		}
		collector.Disable()
		if collector.IsEnabled() {
			t.Error("Metrics collector should be disabled")
		}
		collector.Enable()
		if !collector.IsEnabled() {
			t.Error("Metrics collector should be enabled")
		}

		// Test sampling rate
		if rate := collector.GetSamplingRate(); rate != 1.0 {
			t.Errorf("Default sampling rate should be 1.0, got %v", rate)
		}

		// Test operation tracking with fixed sampling rate
		sleepTime := 5 * time.Millisecond
		collector.TrackOperation(TestOp, func() {
			time.Sleep(sleepTime)
		})

		stats, exists := collector.GetOperationStats(TestOp)
		if !exists {
			t.Error("Expected operation stats to exist")
		}
		if stats.Count != 1 {
			t.Errorf("Expected 1 operation, got %d", stats.Count)
		}
		if stats.TotalTimeNs == 0 {
			t.Error("Expected non-zero operation time")
		}

		// Now test with modified sampling rate
		collector.WithSamplingRate(0.5)
		if rate := collector.GetSamplingRate(); rate != 0.5 {
			t.Errorf("Sampling rate should be 0.5, got %v", rate)
		}
	})

	t.Run("ConfigurationAndReset", func(t *testing.T) {
		collector := NewMetricsCollector()

		// Use sampling rate 1.0 to ensure consistent tracking
		collector.WithSamplingRate(1.0)

		// Verify configuration
		if !collector.IsEnabled() {
			t.Error("Collector should be enabled")
		}
		if rate := collector.GetSamplingRate(); rate != 1.0 {
			t.Errorf("Sampling rate should be 1.0, got %v", rate)
		}

		// Track some operations
		for i := 0; i < 10; i++ {
			collector.TrackOperation(TestOp, func() {
				time.Sleep(time.Millisecond)
			})
		}

		// Get stats before reset
		stats, exists := collector.GetOperationStats(TestOp)
		if !exists {
			t.Error("Expected operation stats to exist")
		}
		if stats.Count == 0 {
			t.Error("Expected non-zero operation count before reset")
		}

		// Reset and verify
		collector.Reset()
		stats, exists = collector.GetOperationStats(TestOp)
		if exists && stats.Count > 0 {
			t.Errorf("Expected zero operation count after reset, got %d", stats.Count)
		}
	})

	t.Run("AllOperationsStats", func(t *testing.T) {
		collector := NewMetricsCollector()
		collector.Reset()

		// Track multiple operations
		operations := []OperationType{TestOp1, TestOp2, TestOp3}
		for _, op := range operations {
			collector.TrackOperation(op, func() {
				time.Sleep(time.Millisecond)
			})
		}

		// Get all stats
		allStats := collector.GetAllOperationStats()

		// Verify our test operations were tracked
		for _, op := range operations {
			stats, ok := allStats[op]
			if !ok {
				t.Errorf("Missing stats for operation %s", op)
			}
			if stats.Count != 1 {
				t.Errorf("Expected 1 operation for %s, got %d", op, stats.Count)
			}
		}
	})

	t.Run("SamplingBehavior", func(t *testing.T) {
		collector := NewMetricsCollector()

		// Test with different sampling rates
		testCases := []struct {
			name         string
			samplingRate float64
			operations   int
			minExpected  int64 // minimum number of operations we expect to be tracked
			maxExpected  int64 // maximum number of operations we expect to be tracked
		}{
			{
				name:         "Full sampling",
				samplingRate: 1.0,
				operations:   10,
				minExpected:  10,
				maxExpected:  10,
			},
			{
				name:         "No sampling",
				samplingRate: 0.0,
				operations:   10,
				minExpected:  0,
				maxExpected:  0,
			},
			{
				name:         "50% sampling",
				samplingRate: 0.5,
				operations:   1000, // More operations for better statistical distribution
				minExpected:  400,  // Allow for some statistical variance (expect 40-60%)
				maxExpected:  600,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				collector.Reset()
				collector.WithSamplingRate(tc.samplingRate)

				// Track operations
				for i := 0; i < tc.operations; i++ {
					collector.TrackOperation(TestOp, func() {
						time.Sleep(time.Microsecond)
					})
				}

				// Verify tracking
				stats, exists := collector.GetOperationStats(TestOp)
				if tc.maxExpected > 0 {
					if !exists {
						t.Error("Expected operation stats to exist")
					}
					if stats.Count < tc.minExpected || stats.Count > tc.maxExpected {
						t.Errorf("Expected between %d and %d operations to be tracked, got %d",
							tc.minExpected, tc.maxExpected, stats.Count)
					}
				} else if exists && stats.Count > 0 {
					t.Errorf("Expected no operations to be tracked, got %d", stats.Count)
				}
			})
		}
	})

}
