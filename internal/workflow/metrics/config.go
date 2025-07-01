package metrics

import (
	"time"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
)

// MetricsConfig defines configuration options for metrics collection
type MetricsConfig struct {
	// Whether metrics collection is enabled
	Enabled bool

	// Sampling rate (0.0 to 1.0) - collect metrics for this percentage of operations
	SamplingRate float64

	// Enable specific metric types
	EnableOperationTiming bool
	EnableLockContention  bool
	EnableMemoryTracking  bool

	// Thresholds for reporting
	SlowOperationThreshold  time.Duration
	HighContentionThreshold int
}

// DefaultMetricsConfig returns a default configuration for metrics collection
func DefaultMetricsConfig() *MetricsConfig {
	return &MetricsConfig{
		Enabled:                 true,
		SamplingRate:            1.0,
		EnableOperationTiming:   true,
		EnableLockContention:    true,
		EnableMemoryTracking:    false,
		SlowOperationThreshold:  1 * time.Millisecond,
		HighContentionThreshold: 100,
	}
}

// ProductionMetricsConfig returns a configuration optimized for production use
// with minimal overhead (1% sampling rate)
func ProductionMetricsConfig() *MetricsConfig {
	return &MetricsConfig{
		Enabled:                 true,
		SamplingRate:            0.01, // 1% sampling
		EnableOperationTiming:   true,
		EnableLockContention:    true,
		EnableMemoryTracking:    false,
		SlowOperationThreshold:  10 * time.Millisecond,
		HighContentionThreshold: 1000,
	}
}

// DisabledMetricsConfig returns a configuration with all metrics disabled
func DisabledMetricsConfig() *MetricsConfig {
	return &MetricsConfig{
		Enabled: false,
	}
}

// ShouldCollect determines whether metrics should be collected for a given operation
// based on the configuration settings
func (c *MetricsConfig) ShouldCollect() bool {
	if !c.Enabled {
		return false
	}

	if c.SamplingRate >= 1.0 {
		return true
	}

	return utils.SecureRandomFloat64() < c.SamplingRate
}

// ShouldCollectOperationTiming determines whether operation timing metrics should be collected
func (c *MetricsConfig) ShouldCollectOperationTiming() bool {
	return c.ShouldCollect() && c.EnableOperationTiming
}

// ShouldCollectLockContention determines whether lock contention metrics should be collected
func (c *MetricsConfig) ShouldCollectLockContention() bool {
	return c.ShouldCollect() && c.EnableLockContention
}

// ShouldCollectMemoryTracking determines whether memory tracking metrics should be collected
func (c *MetricsConfig) ShouldCollectMemoryTracking() bool {
	return c.ShouldCollect() && c.EnableMemoryTracking
}

// WithSamplingRate returns a copy of the config with the specified sampling rate
func (c *MetricsConfig) WithSamplingRate(rate float64) *MetricsConfig {
	config := *c
	config.SamplingRate = rate
	return &config
}

// WithOperationTiming returns a copy of the config with operation timing enabled/disabled
func (c *MetricsConfig) WithOperationTiming(enabled bool) *MetricsConfig {
	config := *c
	config.EnableOperationTiming = enabled
	return &config
}

// WithLockContention returns a copy of the config with lock contention tracking enabled/disabled
func (c *MetricsConfig) WithLockContention(enabled bool) *MetricsConfig {
	config := *c
	config.EnableLockContention = enabled
	return &config
}

// WithMemoryTracking returns a copy of the config with memory tracking enabled/disabled
func (c *MetricsConfig) WithMemoryTracking(enabled bool) *MetricsConfig {
	config := *c
	config.EnableMemoryTracking = enabled
	return &config
}
