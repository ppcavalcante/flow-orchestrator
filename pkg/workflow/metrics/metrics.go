// Package metrics provides functionality for collecting and reporting performance metrics
// for workflow execution. This is a public API that delegates to the internal metrics system.
package metrics

import (
	"sync"

	internal "github.com/pparaujo/flow-orchestrator/internal/workflow/metrics"
)

// OperationType represents a type of operation being measured
type OperationType = internal.OperationType

// OperationStats holds statistics about an operation
type OperationStats = internal.OperationStats

// MetricsCollector collects and reports performance metrics
type MetricsCollector struct {
	collector         *internal.MetricsCollector
	mu                sync.RWMutex
	operationCounts   map[OperationType]*int64
	operationTimes    map[OperationType]*int64
	operationTimesMin map[OperationType]*int64
	operationTimesMax map[OperationType]*int64
	activeOperations  map[OperationType]*int64
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector() *MetricsCollector {
	internalConfig := internal.DefaultMetricsConfig()
	internalConfig.Enabled = true
	internalConfig.SamplingRate = 1.0
	internalConfig.EnableOperationTiming = true
	collector := internal.NewMetricsCollectorWithConfig(internalConfig)

	// Create the collector first
	m := &MetricsCollector{
		collector: collector,
	}

	// Reset to ensure clean state
	m.Reset()

	return m
}

// NewMetricsCollectorWithConfig creates a new metrics collector with the specified configuration
func NewMetricsCollectorWithConfig(config *internal.MetricsConfig) *MetricsCollector {
	collector := internal.NewMetricsCollectorWithConfig(config)

	// Create the collector first
	m := &MetricsCollector{
		collector: collector,
	}

	// Reset to ensure clean state
	m.Reset()

	return m
}

// WithSamplingRate sets the sampling rate for metrics collection
func (m *MetricsCollector) WithSamplingRate(rate float64) *MetricsCollector {
	m.mu.Lock()
	defer m.mu.Unlock()

	config := m.collector.GetConfig()
	config.SamplingRate = rate
	m.collector.UpdateConfig(config)

	return m
}

// Enable enables metrics collection
func (m *MetricsCollector) Enable() {
	m.mu.Lock()
	defer m.mu.Unlock()

	config := m.collector.GetConfig()
	config.Enabled = true
	m.collector.UpdateConfig(config)
}

// Disable disables metrics collection
func (m *MetricsCollector) Disable() {
	m.mu.Lock()
	defer m.mu.Unlock()

	config := m.collector.GetConfig()
	config.Enabled = false
	m.collector.UpdateConfig(config)
}

// IsEnabled returns whether metrics collection is enabled
func (m *MetricsCollector) IsEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config := m.collector.GetConfig()
	return config.Enabled
}

// GetSamplingRate returns the current sampling rate
func (m *MetricsCollector) GetSamplingRate() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config := m.collector.GetConfig()
	return config.SamplingRate
}

// GetConfig returns the current metrics configuration
func (m *MetricsCollector) GetConfig() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()

	internalConfig := m.collector.GetConfig()
	return &Config{
		internalConfig: internalConfig,
	}
}

// Reset clears all collected metrics
func (m *MetricsCollector) Reset() {
	m.collector.Reset()
	m.operationCounts = make(map[OperationType]*int64)
	m.operationTimes = make(map[OperationType]*int64)
	m.operationTimesMin = make(map[OperationType]*int64)
	m.operationTimesMax = make(map[OperationType]*int64)
	m.activeOperations = make(map[OperationType]*int64)
}

// TrackOperation tracks the execution time of an operation
func (m *MetricsCollector) TrackOperation(op OperationType, f func()) {
	// Get the config under a read lock
	m.mu.RLock()
	collector := m.collector
	config := collector.GetConfig()
	m.mu.RUnlock()

	// Skip if disabled
	if !config.Enabled {
		f()
		return
	}

	// Let the internal collector handle all the logic
	collector.TrackOperation(op, f)
}

// GetOperationStats returns statistics for a specific operation type
func (m *MetricsCollector) GetOperationStats(opType OperationType) (OperationStats, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := m.collector.GetOperationStats(opType)
	return stats, stats.Count > 0
}

// GetAllOperationStats returns statistics for all operation types
func (m *MetricsCollector) GetAllOperationStats() map[OperationType]OperationStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.collector.GetAllOperationStats()
}

// Default global metrics collector
var defaultCollector = NewMetricsCollector()

// TrackOperation tracks an operation using the default collector
func TrackOperation(opType OperationType, fn func()) {
	defaultCollector.TrackOperation(opType, fn)
}

// GetOperationStats gets stats from the default collector
func GetOperationStats(opType OperationType) (OperationStats, bool) {
	return defaultCollector.GetOperationStats(opType)
}

// GetAllOperationStats gets all stats from the default collector
func GetAllOperationStats() map[OperationType]OperationStats {
	return defaultCollector.GetAllOperationStats()
}

// Reset resets the default collector
func Reset() {
	defaultCollector.Reset()
}

// Enable enables the default collector
func Enable() {
	defaultCollector.Enable()
}

// Disable disables the default collector
func Disable() {
	defaultCollector.Disable()
}

// SetSamplingRate sets the sampling rate for the default collector
func SetSamplingRate(rate float64) {
	defaultCollector.WithSamplingRate(rate)
}

// IsEnabled returns whether the default collector is enabled
func IsEnabled() bool {
	return defaultCollector.IsEnabled()
}

// GetSamplingRate returns the sampling rate for the default collector
func GetSamplingRate() float64 {
	return defaultCollector.GetSamplingRate()
}

// DefaultConfig returns the default metrics configuration.
// This function is maintained for backward compatibility.
func DefaultConfig() *Config {
	return NewConfig()
}

// ApplyConfig applies the given configuration to the default metrics collector.
// This function is maintained for backward compatibility.
func ApplyConfig(config *Config) {
	if config != nil {
		config.Apply()
	}
}
