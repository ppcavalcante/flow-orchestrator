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

// WithMetricsConfig returns a copy of the configuration with the specified metrics configuration
func (c WorkflowDataConfig) WithMetricsConfig(metricsConfig *metrics.Config) WorkflowDataConfig {
	c.MetricsConfig = metricsConfig
	return c
}
