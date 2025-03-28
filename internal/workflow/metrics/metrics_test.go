package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestNewMetricsCollector(t *testing.T) {
	collector := NewMetricsCollector()

	if collector == nil {
		t.Fatal("NewMetricsCollector returned nil")
	}

	if collector.config == nil {
		t.Error("MetricsCollector should have a non-nil config")
	}

	// Check that maps are initialized
	if collector.operationCounts == nil {
		t.Error("operationCounts map should be initialized")
	}

	if collector.operationTimes == nil {
		t.Error("operationTimes map should be initialized")
	}

	if collector.operationTimesMin == nil {
		t.Error("operationTimesMin map should be initialized")
	}

	if collector.operationTimesMax == nil {
		t.Error("operationTimesMax map should be initialized")
	}

	if collector.activeOperations == nil {
		t.Error("activeOperations map should be initialized")
	}

	// Check that lock contention fields are initialized
	if collector.lockContentionCount == nil {
		t.Error("lockContentionCount should be initialized")
	}

	if collector.lockContentionTime == nil {
		t.Error("lockContentionTime should be initialized")
	}

	if collector.lockContentionTimeMax == nil {
		t.Error("lockContentionTimeMax should be initialized")
	}
}

func TestNewMetricsCollectorWithConfig(t *testing.T) {
	// Test with nil config
	collector := NewMetricsCollectorWithConfig(nil)
	if collector.config == nil {
		t.Error("MetricsCollector should have a non-nil config even when nil is passed")
	}

	// Test with custom config
	customConfig := &MetricsConfig{
		Enabled:                 true,
		SamplingRate:            0.5,
		EnableOperationTiming:   true,
		EnableLockContention:    false,
		EnableMemoryTracking:    true,
		SlowOperationThreshold:  5 * time.Millisecond,
		HighContentionThreshold: 200,
	}

	collector = NewMetricsCollectorWithConfig(customConfig)
	if collector.config != customConfig {
		t.Error("MetricsCollector should use the provided config")
	}

	// Check that all operation types have counters initialized
	operations := []OperationType{
		OpSet, OpGet, OpDelete,
		OpSetStatus, OpGetStatus,
		OpSetOutput, OpGetOutput,
		OpIsNodeRunnable,
		OpSnapshot, OpLoadSnapshot,
		OpLockAcquire, OpLockRelease,
		OpRLockAcquire, OpRLockRelease,
	}

	for _, op := range operations {
		if collector.operationCounts[op] == nil {
			t.Errorf("Counter for operation %s should be initialized", op)
		}
		if collector.operationTimes[op] == nil {
			t.Errorf("Timer for operation %s should be initialized", op)
		}
		if collector.operationTimesMin[op] == nil {
			t.Errorf("Min timer for operation %s should be initialized", op)
		}
		if collector.operationTimesMax[op] == nil {
			t.Errorf("Max timer for operation %s should be initialized", op)
		}
		if collector.activeOperations[op] == nil {
			t.Errorf("Active operations counter for %s should be initialized", op)
		}
	}
}

func TestGetConfig(t *testing.T) {
	config := DefaultMetricsConfig()
	collector := NewMetricsCollectorWithConfig(config)

	returnedConfig := collector.GetConfig()
	if returnedConfig != config {
		t.Error("GetConfig should return the collector's config")
	}
}

func TestUpdateConfig(t *testing.T) {
	collector := NewMetricsCollector()
	originalConfig := collector.GetConfig()

	newConfig := &MetricsConfig{
		Enabled:                 false,
		SamplingRate:            0.1,
		EnableOperationTiming:   false,
		EnableLockContention:    false,
		EnableMemoryTracking:    false,
		SlowOperationThreshold:  100 * time.Millisecond,
		HighContentionThreshold: 500,
	}

	collector.UpdateConfig(newConfig)

	if collector.GetConfig() != newConfig {
		t.Error("UpdateConfig should update the collector's config")
	}

	if collector.GetConfig() == originalConfig {
		t.Error("UpdateConfig should replace the original config")
	}
}

func TestUpdateConfigComprehensive(t *testing.T) {
	collector := NewMetricsCollector()

	// Initial state - should have default config
	initialConfig := collector.GetConfig()
	if !initialConfig.Enabled {
		t.Error("Default config should be enabled")
	}

	// Test updating with nil config (should not change anything)
	collector.UpdateConfig(nil)
	if collector.config != initialConfig {
		t.Error("Updating with nil config should not change the config")
	}

	// Test updating with disabled config
	disabledConfig := DisabledMetricsConfig()
	collector.UpdateConfig(disabledConfig)

	// Check that config was updated
	if collector.config.Enabled {
		t.Error("Config should be disabled after update")
	}
	if collector.config.SamplingRate != disabledConfig.SamplingRate {
		t.Errorf("SamplingRate not updated correctly, got %f, expected %f",
			collector.config.SamplingRate, disabledConfig.SamplingRate)
	}

	// Test updating with production config
	prodConfig := ProductionMetricsConfig()
	collector.UpdateConfig(prodConfig)

	// Check that config was updated
	if !collector.config.Enabled {
		t.Error("Config should be enabled after update to production config")
	}
	if !collector.config.EnableOperationTiming {
		t.Error("EnableOperationTiming should be true in production config")
	}
	if !collector.config.EnableLockContention {
		t.Error("EnableLockContention should be true in production config")
	}
	if collector.config.SamplingRate != prodConfig.SamplingRate {
		t.Errorf("SamplingRate not updated correctly, got %f, expected %f",
			collector.config.SamplingRate, prodConfig.SamplingRate)
	}
}

func TestTrackOperation(t *testing.T) {
	// Create a collector with 100% sampling rate
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	collector := NewMetricsCollectorWithConfig(config)

	// Initial count should be 0
	initialCount := *collector.operationCounts[OpGet]
	if initialCount != 0 {
		t.Errorf("Initial count should be 0, got %d", initialCount)
	}

	// Track an operation
	var result int
	collector.TrackOperation(OpGet, func() {
		// Simulate some work
		time.Sleep(1 * time.Millisecond)
		result = 42
	})

	// Count should be incremented
	newCount := *collector.operationCounts[OpGet]
	if newCount != initialCount+1 {
		t.Errorf("Count should be incremented by 1, got %d", newCount)
	}

	// Operation time should be recorded
	opTime := *collector.operationTimes[OpGet]
	if opTime <= 0 {
		t.Errorf("Operation time should be positive, got %d", opTime)
	}

	// Min time should be set
	minTime := *collector.operationTimesMin[OpGet]
	if minTime <= 0 {
		t.Errorf("Min time should be positive, got %d", minTime)
	}

	// Max time should be set
	maxTime := *collector.operationTimesMax[OpGet]
	if maxTime <= 0 {
		t.Errorf("Max time should be positive, got %d", maxTime)
	}

	// Result should be correct
	if result != 42 {
		t.Errorf("Operation result should be 42, got %d", result)
	}

	// Test with disabled config
	config.Enabled = false
	collector.UpdateConfig(config)

	// Reset counters
	*collector.operationCounts[OpGet] = 0
	*collector.operationTimes[OpGet] = 0

	collector.TrackOperation(OpGet, func() {
		// Simulate some work
		time.Sleep(1 * time.Millisecond)
	})

	// Count should not be incremented when disabled
	newCount = *collector.operationCounts[OpGet]
	if newCount != 0 {
		t.Errorf("Count should not be incremented when disabled, got %d", newCount)
	}

	// Operation time should not be recorded when disabled
	opTime = *collector.operationTimes[OpGet]
	if opTime != 0 {
		t.Errorf("Operation time should not be recorded when disabled, got %d", opTime)
	}
}

func TestTrackOperationComprehensive(t *testing.T) {
	collector := NewMetricsCollector()

	// Test with disabled config
	disabledConfig := DisabledMetricsConfig()
	collector.UpdateConfig(disabledConfig)

	// Track operation with disabled metrics
	operationCalled := false
	collector.TrackOperation(OpSet, func() {
		operationCalled = true
	})

	// Operation should still be called even if metrics are disabled
	if !operationCalled {
		t.Error("Operation should be called even if metrics are disabled")
	}

	// No metrics should be recorded
	stats := collector.GetOperationStats(OpSet)
	if stats.Count != 0 {
		t.Errorf("No metrics should be recorded when disabled, got count: %d", stats.Count)
	}

	// Now enable metrics with operation timing
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	collector.UpdateConfig(config)

	// Track operation with enabled metrics
	collector.TrackOperation(OpSet, func() {
		// Simulate some work
		time.Sleep(5 * time.Millisecond)
	})

	// Metrics should be recorded
	stats = collector.GetOperationStats(OpSet)
	if stats.Count != 1 {
		t.Errorf("Expected count 1, got %d", stats.Count)
	}
	if stats.TotalTimeNs <= 0 {
		t.Errorf("Expected positive total time, got %d", stats.TotalTimeNs)
	}

	// Reset active operations counter to ensure clean state
	atomic.StoreInt64(collector.activeOperations[OpSet], 0)

	// Test with a panicking operation
	panicRecovered := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicRecovered = true
			}
		}()

		collector.TrackOperation(OpSet, func() {
			panic("test panic")
		})
	}()

	// The panic should have been recovered in our test function
	if !panicRecovered {
		t.Error("Expected panic to be propagated")
	}

	// Active operations should be properly decremented even after panic
	active := atomic.LoadInt64(collector.activeOperations[OpSet])
	if active != 0 {
		t.Errorf("Active operations should be 0 after panic, got %d", active)
	}
}

func TestStartEndOperation(t *testing.T) {
	collector := NewMetricsCollector()

	// Start an operation
	start := collector.StartOperation(OpSet)

	// Sleep to simulate work
	time.Sleep(1 * time.Millisecond)

	// End the operation
	duration := collector.EndOperation(OpSet, start)

	// Duration should be positive
	if duration <= 0 {
		t.Errorf("Duration should be positive, got %v", duration)
	}

	// Active operations should be 0
	active := *collector.activeOperations[OpSet]
	if active != 0 {
		t.Errorf("Active operations should be 0 after ending, got %d", active)
	}
}

func TestRecordOperationTiming(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	collector.UpdateConfig(config)

	// Verify the configuration is properly set
	if !collector.config.ShouldCollectOperationTiming() {
		t.Fatal("Operation timing collection should be enabled")
	}

	// Record a timing
	duration := 5 * time.Millisecond
	collector.RecordOperationTiming(OpDelete, duration)

	// Count should be incremented
	count := atomic.LoadInt64(collector.operationCounts[OpDelete])
	if count != 1 {
		t.Errorf("Count should be 1, got %d", count)
	}

	// Total time should be recorded
	totalTime := atomic.LoadInt64(collector.operationTimes[OpDelete])
	if totalTime != duration.Nanoseconds() {
		t.Errorf("Total time should be %d, got %d", duration.Nanoseconds(), totalTime)
	}

	// Min time should be set
	minTime := atomic.LoadInt64(collector.operationTimesMin[OpDelete])
	if minTime != duration.Nanoseconds() {
		t.Errorf("Min time should be %d, got %d", duration.Nanoseconds(), minTime)
	}

	// Max time should be set
	maxTime := atomic.LoadInt64(collector.operationTimesMax[OpDelete])
	if maxTime != duration.Nanoseconds() {
		t.Errorf("Max time should be %d, got %d", duration.Nanoseconds(), maxTime)
	}

	// Record a shorter timing
	shorterDuration := 2 * time.Millisecond
	collector.RecordOperationTiming(OpDelete, shorterDuration)

	// Min time should be updated
	minTime = atomic.LoadInt64(collector.operationTimesMin[OpDelete])
	if minTime != shorterDuration.Nanoseconds() {
		t.Errorf("Min time should be updated to %d, got %d", shorterDuration.Nanoseconds(), minTime)
	}

	// Record a longer timing
	longerDuration := 10 * time.Millisecond
	collector.RecordOperationTiming(OpDelete, longerDuration)

	// Max time should be updated
	maxTime = atomic.LoadInt64(collector.operationTimesMax[OpDelete])
	if maxTime != longerDuration.Nanoseconds() {
		t.Errorf("Max time should be updated to %d, got %d", longerDuration.Nanoseconds(), maxTime)
	}
}

func TestRecordLockContention(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableLockContention = true
	collector.UpdateConfig(config)

	// Verify the configuration is properly set
	if !collector.config.ShouldCollectLockContention() {
		t.Fatal("Lock contention collection should be enabled")
	}

	// Record contention
	duration := 5 * time.Millisecond
	collector.RecordLockContention(duration)

	// Count should be incremented
	count := atomic.LoadInt64(collector.lockContentionCount)
	if count != 1 {
		t.Errorf("Contention count should be 1, got %d", count)
	}

	// Total time should be recorded
	totalTime := atomic.LoadInt64(collector.lockContentionTime)
	if totalTime != duration.Nanoseconds() {
		t.Errorf("Total contention time should be %d, got %d", duration.Nanoseconds(), totalTime)
	}

	// Max time should be set
	maxTime := atomic.LoadInt64(collector.lockContentionTimeMax)
	if maxTime != duration.Nanoseconds() {
		t.Errorf("Max contention time should be %d, got %d", duration.Nanoseconds(), maxTime)
	}

	// Record a longer contention
	longerDuration := 10 * time.Millisecond
	collector.RecordLockContention(longerDuration)

	// Count should be incremented again
	count = atomic.LoadInt64(collector.lockContentionCount)
	if count != 2 {
		t.Errorf("Contention count should be 2, got %d", count)
	}

	// Total time should be updated
	totalTime = atomic.LoadInt64(collector.lockContentionTime)
	expectedTotal := duration.Nanoseconds() + longerDuration.Nanoseconds()
	if totalTime != expectedTotal {
		t.Errorf("Total contention time should be %d, got %d", expectedTotal, totalTime)
	}

	// Max time should be updated
	maxTime = atomic.LoadInt64(collector.lockContentionTimeMax)
	if maxTime != longerDuration.Nanoseconds() {
		t.Errorf("Max contention time should be updated to %d, got %d", longerDuration.Nanoseconds(), maxTime)
	}
}

func TestGetOperationStats(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	collector.UpdateConfig(config)

	// Verify the configuration is properly set
	if !collector.config.ShouldCollectOperationTiming() {
		t.Fatal("Operation timing collection should be enabled")
	}

	// Record some operations
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)
	collector.RecordOperationTiming(OpSet, 10*time.Millisecond)

	// Get stats
	stats := collector.GetOperationStats(OpSet)

	// Check stats
	if stats.Count != 2 {
		t.Errorf("Stats count should be 2, got %d", stats.Count)
	}

	expectedTotal := int64(15 * time.Millisecond)
	if stats.TotalTimeNs != expectedTotal {
		t.Errorf("Stats total time should be %d, got %d", expectedTotal, stats.TotalTimeNs)
	}

	expectedMin := int64(5 * time.Millisecond)
	if stats.MinTimeNs != expectedMin {
		t.Errorf("Stats min time should be %d, got %d", expectedMin, stats.MinTimeNs)
	}

	expectedMax := int64(10 * time.Millisecond)
	if stats.MaxTimeNs != expectedMax {
		t.Errorf("Stats max time should be %d, got %d", expectedMax, stats.MaxTimeNs)
	}

	// Only calculate average if count is not zero to avoid division by zero
	if stats.Count > 0 {
		expectedAvg := expectedTotal / stats.Count
		if stats.AvgTimeNs != expectedAvg {
			t.Errorf("Stats avg time should be %d, got %d", expectedAvg, stats.AvgTimeNs)
		}
	}
}

func TestGetLockContentionStats(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableLockContention = true
	collector.UpdateConfig(config)

	// Verify the configuration is properly set
	if !collector.config.ShouldCollectLockContention() {
		t.Fatal("Lock contention collection should be enabled")
	}

	// Record some contentions
	collector.RecordLockContention(5 * time.Millisecond)
	collector.RecordLockContention(10 * time.Millisecond)

	// Get stats
	stats := collector.GetLockContentionStats()

	// Check stats
	if stats.Count != 2 {
		t.Errorf("Contention stats count should be 2, got %d", stats.Count)
	}

	expectedTotal := int64(15 * time.Millisecond)
	if stats.TotalTimeNs != expectedTotal {
		t.Errorf("Contention stats total time should be %d, got %d", expectedTotal, stats.TotalTimeNs)
	}

	expectedMax := int64(10 * time.Millisecond)
	if stats.MaxTimeNs != expectedMax {
		t.Errorf("Contention stats max time should be %d, got %d", expectedMax, stats.MaxTimeNs)
	}

	// Only calculate average if count is not zero to avoid division by zero
	if stats.Count > 0 {
		expectedAvg := expectedTotal / stats.Count
		if stats.AvgTimeNs != expectedAvg {
			t.Errorf("Contention stats avg time should be %d, got %d", expectedAvg, stats.AvgTimeNs)
		}
	}
}

func TestGetAllOperationStats(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	collector.UpdateConfig(config)

	// Verify the configuration is properly set
	if !collector.config.ShouldCollectOperationTiming() {
		t.Fatal("Operation timing collection should be enabled")
	}

	// Record some operations
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)
	collector.RecordOperationTiming(OpGet, 10*time.Millisecond)

	// Get all stats
	allStats := collector.GetAllOperationStats()

	// Check that both operations are in the map
	if _, ok := allStats[OpSet]; !ok {
		t.Error("OpSet should be in the stats map")
	}

	if _, ok := allStats[OpGet]; !ok {
		t.Error("OpGet should be in the stats map")
	}

	// Check stats for OpSet
	setStats := allStats[OpSet]
	if setStats.Count != 1 {
		t.Errorf("OpSet stats count should be 1, got %d", setStats.Count)
	}

	// Check stats for OpGet
	getStats := allStats[OpGet]
	if getStats.Count != 1 {
		t.Errorf("OpGet stats count should be 1, got %d", getStats.Count)
	}
}

func TestReset(t *testing.T) {
	collector := NewMetricsCollector()

	// Make sure the config is enabled
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	config.EnableLockContention = true
	collector.UpdateConfig(config)

	// Record some operations
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)
	collector.RecordLockContention(10 * time.Millisecond)

	// Reset
	collector.Reset()

	// Check that counters are reset
	count := *collector.operationCounts[OpSet]
	if count != 0 {
		t.Errorf("Operation count should be reset to 0, got %d", count)
	}

	totalTime := *collector.operationTimes[OpSet]
	if totalTime != 0 {
		t.Errorf("Operation time should be reset to 0, got %d", totalTime)
	}

	contentionCount := *collector.lockContentionCount
	if contentionCount != 0 {
		t.Errorf("Contention count should be reset to 0, got %d", contentionCount)
	}

	contentionTime := *collector.lockContentionTime
	if contentionTime != 0 {
		t.Errorf("Contention time should be reset to 0, got %d", contentionTime)
	}
}

// Test global functions
func TestGlobalFunctions(t *testing.T) {
	// Reset global state
	Reset()

	// Set global config
	config := ProductionMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = true
	config.EnableLockContention = true
	SetGlobalConfig(config)

	// Get global config
	returnedConfig := GetGlobalConfig()
	if returnedConfig != config {
		t.Error("GetGlobalConfig should return the config set by SetGlobalConfig")
	}

	// Track operation
	TrackOperation(OpSet, func() {
		// Do nothing
	})

	// Record contention
	RecordLockContention(5 * time.Millisecond)

	// Get operation stats
	stats := GetOperationStats(OpSet)
	if stats.Count != 1 {
		t.Errorf("OpSet stats count should be 1, got %d", stats.Count)
	}

	// Get contention stats
	contentionStats := GetLockContentionStats()
	if contentionStats.Count != 1 {
		t.Errorf("Contention stats count should be 1, got %d", contentionStats.Count)
	}

	// Get all operation stats
	allStats := GetAllOperationStats()
	if len(allStats) == 0 {
		t.Error("GetAllOperationStats should return non-empty map")
	}

	// Reset again
	Reset()

	// Stats should be reset
	stats = GetOperationStats(OpSet)
	if stats.Count != 0 {
		t.Errorf("OpSet stats count should be reset to 0, got %d", stats.Count)
	}
}

func TestInstrumentedRWMutexRLockContention(t *testing.T) {
	// Create a metrics collector with lock contention enabled
	collector := NewMetricsCollector()
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.EnableLockContention = true
	collector.UpdateConfig(config)

	// Create an instrumented mutex with the same config
	mutex := NewInstrumentedRWMutexWithConfig("test-mutex", config)

	// Set up the global collector for the mutex to use
	oldCollector := defaultCollector
	defaultCollector = collector
	defer func() {
		defaultCollector = oldCollector
	}()

	// Acquire the write lock first to create contention
	mutex.Lock()

	// Try to acquire read lock in a goroutine (will block)
	done := make(chan struct{})
	go func() {
		// This will block until the write lock is released
		mutex.RLock()
		mutex.RUnlock()
		close(done)
	}()

	// Sleep to ensure the goroutine has time to attempt the read lock
	time.Sleep(10 * time.Millisecond)

	// Release the write lock, allowing the read lock to be acquired
	mutex.Unlock()

	// Wait for the read lock goroutine to complete
	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timed out waiting for RLock to complete")
	}

	// Check that lock contention was recorded
	stats := collector.GetLockContentionStats()
	if stats.Count < 1 {
		t.Errorf("Expected at least 1 lock contention, got %d", stats.Count)
	}
}

func TestRecordOperationTimingComprehensive(t *testing.T) {
	collector := NewMetricsCollector()

	// Test with disabled config
	disabledConfig := DisabledMetricsConfig()
	collector.UpdateConfig(disabledConfig)

	// Record operation timing with disabled metrics
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)

	// No metrics should be recorded when disabled
	stats := collector.GetOperationStats(OpSet)
	if stats.Count != 0 {
		t.Errorf("No metrics should be recorded when disabled, got count: %d", stats.Count)
	}

	// Now enable metrics but disable operation timing specifically
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableOperationTiming = false
	collector.UpdateConfig(config)

	// Record operation timing with operation timing disabled
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)

	// No metrics should be recorded when operation timing is disabled
	stats = collector.GetOperationStats(OpSet)
	if stats.Count != 0 {
		t.Errorf("No metrics should be recorded when operation timing is disabled, got count: %d", stats.Count)
	}

	// Now enable operation timing
	config.EnableOperationTiming = true
	collector.UpdateConfig(config)

	// Record operation timing with operation timing enabled
	collector.RecordOperationTiming(OpSet, 5*time.Millisecond)

	// Metrics should be recorded
	stats = collector.GetOperationStats(OpSet)
	if stats.Count != 1 {
		t.Errorf("Expected count 1, got %d", stats.Count)
	}

	// Test the case where the counters don't exist yet
	// We'll use a new operation type that hasn't been used before
	customOp := OperationType("custom_op")

	// Initialize the counters for the custom operation type
	collector.mu.Lock()
	collector.operationCounts[customOp] = new(int64)
	collector.operationTimes[customOp] = new(int64)
	collector.operationTimesMin[customOp] = new(int64)
	atomic.StoreInt64(collector.operationTimesMin[customOp], int64(^uint64(0)>>1)) // Initialize to max int64
	collector.operationTimesMax[customOp] = new(int64)
	collector.activeOperations[customOp] = new(int64)
	collector.mu.Unlock()

	// Record operation timing for the new operation type
	collector.RecordOperationTiming(customOp, 10*time.Millisecond)

	// Metrics should be recorded for the new operation type
	stats = collector.GetOperationStats(customOp)
	if stats.Count != 1 {
		t.Errorf("Expected count 1 for new operation type, got %d", stats.Count)
	}
	if stats.TotalTimeNs != int64(10*time.Millisecond) {
		t.Errorf("Expected total time %d, got %d", int64(10*time.Millisecond), stats.TotalTimeNs)
	}
}
