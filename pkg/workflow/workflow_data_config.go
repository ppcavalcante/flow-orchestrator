package workflow

import (
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// WorkflowDataConfig defines the configuration for WorkflowData
type WorkflowDataConfig struct {
	// Expected capacity hints
	ExpectedNodes int
	ExpectedData  int

	// Metrics configuration
	MetricsConfig *metrics.Config
}

// DefaultWorkflowDataConfig creates a default configuration for WorkflowData
func DefaultWorkflowDataConfig() WorkflowDataConfig {
	return WorkflowDataConfig{
		// Reasonable defaults
		ExpectedNodes: 16,
		ExpectedData:  32,

		// Default metrics configuration
		MetricsConfig: metrics.NewConfig(),
	}
}

// ReadOptimizedWorkflowDataConfig creates a configuration optimized for read-heavy workloads
func ReadOptimizedWorkflowDataConfig(expectedNodes int) WorkflowDataConfig {
	config := DefaultWorkflowDataConfig()

	// Set expected capacity
	config.ExpectedNodes = expectedNodes
	config.ExpectedData = expectedNodes * 2 // Assume ~2 data items per node

	return config
}

// HighConcurrencyWorkflowDataConfig creates a configuration optimized for high concurrency
func HighConcurrencyWorkflowDataConfig(expectedNodes int) WorkflowDataConfig {
	config := DefaultWorkflowDataConfig()

	// Set expected capacity
	config.ExpectedNodes = expectedNodes
	config.ExpectedData = expectedNodes * 2 // Assume ~2 data items per node

	return config
}

// LowMemoryWorkflowDataConfig creates a configuration optimized for memory efficiency
func LowMemoryWorkflowDataConfig(expectedNodes int) WorkflowDataConfig {
	config := DefaultWorkflowDataConfig()

	// Set expected capacity
	config.ExpectedNodes = expectedNodes
	config.ExpectedData = expectedNodes

	// Disable metrics to save memory
	config.MetricsConfig = metrics.DisabledMetricsConfig()

	return config
}

// ProductionWorkflowDataConfig creates a configuration optimized for production use
func ProductionWorkflowDataConfig(expectedNodes int) WorkflowDataConfig {
	config := DefaultWorkflowDataConfig()

	// Set expected capacity
	config.ExpectedNodes = expectedNodes
	config.ExpectedData = expectedNodes * 2 // Assume ~2 data items per node

	// Use production metrics configuration (1% sampling)
	config.MetricsConfig = metrics.ProductionConfig()

	return config
}

// WithMetricsConfig returns a copy of the configuration with the specified metrics configuration
func (c WorkflowDataConfig) WithMetricsConfig(metricsConfig *metrics.Config) WorkflowDataConfig {
	c.MetricsConfig = metricsConfig
	return c
}
