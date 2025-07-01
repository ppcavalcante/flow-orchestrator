package metrics

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
)

// OperationType represents different operations that we want to track
type OperationType string

// Define operation types
const (
	// Data Operations
	OpSet    OperationType = "set"
	OpGet    OperationType = "get"
	OpDelete OperationType = "delete"

	// Node Status Operations
	OpSetStatus OperationType = "set_status"
	OpGetStatus OperationType = "get_status"

	// Node Output Operations
	OpSetOutput OperationType = "set_output"
	OpGetOutput OperationType = "get_output"

	// Dependency Resolution
	OpIsNodeRunnable OperationType = "is_node_runnable"

	// Serialization
	OpSnapshot     OperationType = "snapshot"
	OpLoadSnapshot OperationType = "load_snapshot"

	// Lock Operations
	OpLockAcquire  OperationType = "lock_acquire"
	OpLockRelease  OperationType = "lock_release"
	OpRLockAcquire OperationType = "rlock_acquire"
	OpRLockRelease OperationType = "rlock_release"

	// Test Operations
	OpTest  OperationType = "test_op"
	OpTest1 OperationType = "op1"
	OpTest2 OperationType = "op2"
	OpTest3 OperationType = "op3"
)

// MetricsCollector collects metrics for the workflow system
type MetricsCollector struct {
	// Configuration
	config *MetricsConfig

	// Operation counters
	operationCounts map[OperationType]*int64

	// Operation timing
	operationTimes    map[OperationType]*int64 // in nanoseconds
	operationTimesMin map[OperationType]*int64 // in nanoseconds
	operationTimesMax map[OperationType]*int64 // in nanoseconds

	// Lock contention
	lockContentionCount   *int64 // number of times a lock acquisition was delayed
	lockContentionTime    *int64 // total time spent waiting for locks (ns)
	lockContentionTimeMax *int64 // max time spent waiting for a lock (ns)

	// Active operations
	activeOperations map[OperationType]*int64

	// For statistics collection
	mu sync.RWMutex
}

// NewMetricsCollector creates a new metrics collector with the default configuration
func NewMetricsCollector() *MetricsCollector {
	return NewMetricsCollectorWithConfig(DefaultMetricsConfig())
}

// NewMetricsCollectorWithConfig creates a new metrics collector with the specified configuration
func NewMetricsCollectorWithConfig(config *MetricsConfig) *MetricsCollector {
	if config == nil {
		config = DefaultMetricsConfig()
	}

	m := &MetricsCollector{
		config:                config,
		operationCounts:       make(map[OperationType]*int64),
		operationTimes:        make(map[OperationType]*int64),
		operationTimesMin:     make(map[OperationType]*int64),
		operationTimesMax:     make(map[OperationType]*int64),
		lockContentionCount:   new(int64),
		lockContentionTime:    new(int64),
		lockContentionTimeMax: new(int64),
		activeOperations:      make(map[OperationType]*int64),
	}

	// Initialize counters for all operation types
	for _, op := range []OperationType{
		OpSet, OpGet, OpDelete,
		OpSetStatus, OpGetStatus,
		OpSetOutput, OpGetOutput,
		OpIsNodeRunnable,
		OpSnapshot, OpLoadSnapshot,
		OpLockAcquire, OpLockRelease,
		OpRLockAcquire, OpRLockRelease,
		OpTest, OpTest1, OpTest2, OpTest3,
	} {
		m.operationCounts[op] = new(int64)
		m.operationTimes[op] = new(int64)
		m.operationTimesMin[op] = new(int64)
		atomic.StoreInt64(m.operationTimesMin[op], int64(^uint64(0)>>1)) // Initialize to max int64
		m.operationTimesMax[op] = new(int64)
		m.activeOperations[op] = new(int64)
	}

	return m
}

// GetConfig returns the current metrics configuration
func (m *MetricsCollector) GetConfig() *MetricsConfig {
	return m.config
}

// UpdateConfig updates the metrics configuration
func (m *MetricsCollector) UpdateConfig(config *MetricsConfig) {
	if config == nil {
		return
	}
	m.config = config
}

// TrackOperation tracks the execution time of an operation
func (m *MetricsCollector) TrackOperation(op OperationType, f func()) {
	// Skip if metrics are disabled
	if m == nil || !m.config.Enabled {
		f()
		return
	}

	// Skip based on sampling rate
	if m.config.SamplingRate < 1.0 && utils.SecureRandomFloat64() > m.config.SamplingRate {
		f()
		return
	}

	// Initialize counter for this operation type if it doesn't exist
	m.mu.Lock()
	if _, exists := m.operationCounts[op]; !exists {
		m.operationCounts[op] = new(int64)
		m.operationTimes[op] = new(int64)
		m.operationTimesMin[op] = new(int64)
		atomic.StoreInt64(m.operationTimesMin[op], int64(^uint64(0)>>1)) // Initialize to max int64
		m.operationTimesMax[op] = new(int64)
		m.activeOperations[op] = new(int64)
	}

	// Get the counters under the lock to ensure they exist
	opCount := m.operationCounts[op]
	opTimes := m.operationTimes[op]
	opTimesMin := m.operationTimesMin[op]
	opTimesMax := m.operationTimesMax[op]
	activeOps := m.activeOperations[op]
	m.mu.Unlock()

	// Track active operations with panic recovery
	atomic.AddInt64(activeOps, 1)
	defer func() {
		atomic.AddInt64(activeOps, -1)
		if r := recover(); r != nil {
			panic(r) // Re-panic after decrementing
		}
	}()

	// Track operation time
	start := time.Now()
	f()
	duration := time.Since(start)
	durationNs := duration.Nanoseconds()

	// Update operation stats
	atomic.AddInt64(opCount, 1)
	atomic.AddInt64(opTimes, durationNs)

	// Update min/max times
	for {
		currentMin := atomic.LoadInt64(opTimesMin)
		if durationNs >= currentMin || atomic.CompareAndSwapInt64(opTimesMin, currentMin, durationNs) {
			break
		}
	}

	for {
		currentMax := atomic.LoadInt64(opTimesMax)
		if durationNs <= currentMax || atomic.CompareAndSwapInt64(opTimesMax, currentMax, durationNs) {
			break
		}
	}
}

// StartOperation starts tracking an operation
func (m *MetricsCollector) StartOperation(op OperationType) time.Time {
	// Skip if metrics are disabled
	if m == nil || !m.config.Enabled {
		return time.Now()
	}

	// Initialize counter for this operation type if it doesn't exist
	m.mu.Lock()
	if _, exists := m.operationCounts[op]; !exists {
		m.operationCounts[op] = new(int64)
		m.operationTimes[op] = new(int64)
		m.operationTimesMin[op] = new(int64)
		atomic.StoreInt64(m.operationTimesMin[op], int64(^uint64(0)>>1)) // Initialize to max int64
		m.operationTimesMax[op] = new(int64)
		m.activeOperations[op] = new(int64)
	}
	m.mu.Unlock()

	atomic.AddInt64(m.activeOperations[op], 1)
	return time.Now()
}

// EndOperation ends tracking an operation and returns the duration
func (m *MetricsCollector) EndOperation(op OperationType, start time.Time) time.Duration {
	end := time.Now()
	duration := end.Sub(start)

	// Skip if metrics are disabled
	if m == nil || !m.config.Enabled {
		return duration
	}

	// Ensure we always decrement active operations, even if there's a panic
	defer func() {
		atomic.AddInt64(m.activeOperations[op], -1)
		if r := recover(); r != nil {
			panic(r) // Re-panic after decrementing
		}
	}()

	m.RecordOperationTiming(op, duration)
	return duration
}

// RecordOperationTiming records the timing for an operation
func (m *MetricsCollector) RecordOperationTiming(op OperationType, duration time.Duration) {
	// Skip metrics collection if disabled or not sampled
	if !m.config.ShouldCollectOperationTiming() {
		return
	}

	ns := duration.Nanoseconds()

	// Get the counters with proper synchronization
	m.mu.RLock()
	opCount, countExists := m.operationCounts[op]
	opTimes, exists := m.operationTimes[op]
	opTimesMin, minExists := m.operationTimesMin[op]
	opTimesMax, maxExists := m.operationTimesMax[op]
	m.mu.RUnlock()

	// If any of the counters don't exist, we need to create them
	if !exists || !minExists || !maxExists || !countExists {
		m.mu.Lock()
		// Check again in case another goroutine created them while we were waiting
		if _, exists := m.operationTimes[op]; !exists {
			m.operationTimes[op] = new(int64)
			m.operationTimesMin[op] = new(int64)
			atomic.StoreInt64(m.operationTimesMin[op], int64(^uint64(0)>>1)) // Initialize to max int64
			m.operationTimesMax[op] = new(int64)
		}
		if _, exists := m.operationCounts[op]; !exists {
			m.operationCounts[op] = new(int64)
		}
		opCount = m.operationCounts[op]
		opTimes = m.operationTimes[op]
		opTimesMin = m.operationTimesMin[op]
		opTimesMax = m.operationTimesMax[op]
		m.mu.Unlock()
	}

	// Increment operation count
	atomic.AddInt64(opCount, 1)

	// Update total time
	atomic.AddInt64(opTimes, ns)

	// Update min time (using CAS to ensure atomic min)
	for {
		current := atomic.LoadInt64(opTimesMin)
		if ns >= current || atomic.CompareAndSwapInt64(opTimesMin, current, ns) {
			break
		}
	}

	// Update max time (using CAS to ensure atomic max)
	for {
		current := atomic.LoadInt64(opTimesMax)
		if ns <= current || atomic.CompareAndSwapInt64(opTimesMax, current, ns) {
			break
		}
	}
}

// RecordLockContention records lock contention if lock contention metrics are enabled
func (m *MetricsCollector) RecordLockContention(duration time.Duration) {
	// Skip metrics collection if disabled or not sampled
	if !m.config.ShouldCollectLockContention() {
		return
	}

	atomic.AddInt64(m.lockContentionCount, 1)
	ns := duration.Nanoseconds()
	atomic.AddInt64(m.lockContentionTime, ns)

	// Update max contention time (using CAS to ensure atomic max)
	for {
		current := atomic.LoadInt64(m.lockContentionTimeMax)
		if ns <= current || atomic.CompareAndSwapInt64(m.lockContentionTimeMax, current, ns) {
			break
		}
	}
}

// OperationStats contains statistics for an operation
type OperationStats struct {
	Count       int64
	TotalTimeNs int64
	MinTimeNs   int64
	MaxTimeNs   int64
	AvgTimeNs   int64
	Active      int64
}

// LockContentionStats contains statistics for lock contention
type LockContentionStats struct {
	Count       int64
	TotalTimeNs int64
	MaxTimeNs   int64
	AvgTimeNs   int64
}

// GetOperationStats returns statistics for a specific operation type
func (m *MetricsCollector) GetOperationStats(op OperationType) OperationStats {
	if m == nil || !m.config.Enabled {
		return OperationStats{}
	}

	// First check if the operation exists
	m.mu.RLock()
	_, exists := m.operationCounts[op]
	if !exists {
		m.mu.RUnlock()
		return OperationStats{}
	}

	// Get all values under the same lock to ensure consistency
	count := atomic.LoadInt64(m.operationCounts[op])
	totalTime := atomic.LoadInt64(m.operationTimes[op])
	minTime := atomic.LoadInt64(m.operationTimesMin[op])
	maxTime := atomic.LoadInt64(m.operationTimesMax[op])
	active := atomic.LoadInt64(m.activeOperations[op])
	m.mu.RUnlock()

	// If no operations have been recorded, return empty stats
	if count == 0 {
		return OperationStats{}
	}

	// If minTime is still at initialization value, set it to 0
	if minTime == int64(^uint64(0)>>1) {
		minTime = 0
	}

	return OperationStats{
		Count:       count,
		TotalTimeNs: totalTime,
		MinTimeNs:   minTime,
		MaxTimeNs:   maxTime,
		AvgTimeNs:   totalTime / count,
		Active:      active,
	}
}

// GetLockContentionStats returns lock contention stats
func (m *MetricsCollector) GetLockContentionStats() LockContentionStats {
	count := atomic.LoadInt64(m.lockContentionCount)
	total := atomic.LoadInt64(m.lockContentionTime)
	maxTime := atomic.LoadInt64(m.lockContentionTimeMax)

	// Calculate average
	var avg int64
	if count > 0 {
		avg = total / count
	}

	return LockContentionStats{
		Count:       count,
		TotalTimeNs: total,
		MaxTimeNs:   maxTime,
		AvgTimeNs:   avg,
	}
}

// GetAllOperationStats returns statistics for all operation types
func (m *MetricsCollector) GetAllOperationStats() map[OperationType]OperationStats {
	if m == nil || !m.config.Enabled {
		return nil
	}

	// Get a snapshot of all operation types under a read lock
	m.mu.RLock()
	operations := make([]OperationType, 0, len(m.operationCounts))
	for op := range m.operationCounts {
		operations = append(operations, op)
	}
	m.mu.RUnlock()

	// Now collect stats for each operation without holding the lock
	stats := make(map[OperationType]OperationStats)
	for _, op := range operations {
		stats[op] = m.GetOperationStats(op)
	}
	return stats
}

// Reset resets all metrics
func (m *MetricsCollector) Reset() {
	for op := range m.operationCounts {
		atomic.StoreInt64(m.operationCounts[op], 0)
		atomic.StoreInt64(m.operationTimes[op], 0)
		atomic.StoreInt64(m.operationTimesMin[op], int64(^uint64(0)>>1)) // Initialize to max int64
		atomic.StoreInt64(m.operationTimesMax[op], 0)
		atomic.StoreInt64(m.activeOperations[op], 0)
	}

	atomic.StoreInt64(m.lockContentionCount, 0)
	atomic.StoreInt64(m.lockContentionTime, 0)
	atomic.StoreInt64(m.lockContentionTimeMax, 0)
}

// Global metrics collector with default configuration
var defaultCollector = NewMetricsCollector()

// SetGlobalConfig updates the configuration for the default metrics collector
func SetGlobalConfig(config *MetricsConfig) {
	defaultCollector.UpdateConfig(config)
}

// GetGlobalConfig returns the current configuration for the default metrics collector
func GetGlobalConfig() *MetricsConfig {
	return defaultCollector.GetConfig()
}

// TrackOperation tracks an operation using the default collector
func TrackOperation(op OperationType, f func()) {
	defaultCollector.TrackOperation(op, f)
}

// RecordLockContention records lock contention using the default collector
func RecordLockContention(duration time.Duration) {
	defaultCollector.RecordLockContention(duration)
}

// GetOperationStats returns operation stats from the default collector
func GetOperationStats(op OperationType) OperationStats {
	return defaultCollector.GetOperationStats(op)
}

// GetLockContentionStats returns lock contention stats from the default collector
func GetLockContentionStats() LockContentionStats {
	return defaultCollector.GetLockContentionStats()
}

// GetAllOperationStats returns all operation stats from the default collector
func GetAllOperationStats() map[OperationType]OperationStats {
	return defaultCollector.GetAllOperationStats()
}

// Reset resets all metrics in the default collector
func Reset() {
	defaultCollector.Reset()
}
