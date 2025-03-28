// Package metrics provides functionality for collecting and reporting performance metrics
// for workflow execution.
package metrics

import (
	"time"

	internal "github.com/pparaujo/flow-orchestrator/internal/workflow/metrics"
)

// Config represents the configuration for metrics collection
type Config struct {
	internalConfig *internal.MetricsConfig
}

// NewConfig creates a new metrics configuration with default values
func NewConfig() *Config {
	return &Config{
		internalConfig: internal.DefaultMetricsConfig(),
	}
}

// DisabledMetricsConfig returns a configuration with metrics collection disabled
func DisabledMetricsConfig() *Config {
	return &Config{
		internalConfig: internal.DisabledMetricsConfig(),
	}
}

// ProductionConfig returns a configuration suitable for production use
func ProductionConfig() *Config {
	return &Config{
		internalConfig: internal.ProductionMetricsConfig(),
	}
}

// WithEnabled sets whether metrics collection is enabled
func (c *Config) WithEnabled(enabled bool) *Config {
	c.internalConfig.Enabled = enabled
	return c
}

// WithSamplingRate sets the sampling rate for metrics collection
func (c *Config) WithSamplingRate(rate float64) *Config {
	c.internalConfig.SamplingRate = rate
	return c
}

// WithOperationTiming enables or disables operation timing metrics
func (c *Config) WithOperationTiming(enabled bool) *Config {
	c.internalConfig.EnableOperationTiming = enabled
	return c
}

// WithLockContention enables or disables lock contention metrics
func (c *Config) WithLockContention(enabled bool) *Config {
	c.internalConfig.EnableLockContention = enabled
	return c
}

// WithMemoryTracking enables or disables memory tracking metrics
func (c *Config) WithMemoryTracking(enabled bool) *Config {
	c.internalConfig.EnableMemoryTracking = enabled
	return c
}

// WithSlowOperationThreshold sets the threshold for reporting slow operations
func (c *Config) WithSlowOperationThreshold(threshold time.Duration) *Config {
	c.internalConfig.SlowOperationThreshold = threshold
	return c
}

// WithHighContentionThreshold sets the threshold for reporting high contention
func (c *Config) WithHighContentionThreshold(threshold int) *Config {
	c.internalConfig.HighContentionThreshold = threshold
	return c
}

// Apply applies this configuration to the default metrics collector
func (c *Config) Apply() {
	if c != nil && c.internalConfig != nil {
		defaultCollector.mu.Lock()
		defer defaultCollector.mu.Unlock()
		defaultCollector.collector.UpdateConfig(c.internalConfig)
	}
}

// GetInternalConfig returns the internal metrics configuration
func (c *Config) GetInternalConfig() *internal.MetricsConfig {
	return c.internalConfig
}

// ConfigFromLiteral creates a Config from a struct literal with Enabled and SamplingRate fields.
// This function is maintained for backward compatibility.
func ConfigFromLiteral(enabled bool, samplingRate float64) *Config {
	config := NewConfig()
	config.WithEnabled(enabled)
	config.WithSamplingRate(samplingRate)
	return config
}
