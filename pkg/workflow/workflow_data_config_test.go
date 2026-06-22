package workflow

import (
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

func TestDefaultWorkflowDataConfig(t *testing.T) {
	config := DefaultWorkflowDataConfig()

	// Check default values
	if config.ExpectedNodes != 16 {
		t.Errorf("Expected ExpectedNodes to be 16, got %d", config.ExpectedNodes)
	}

	if config.ExpectedData != 32 {
		t.Errorf("Expected ExpectedData to be 32, got %d", config.ExpectedData)
	}

	if config.MetricsConfig == nil {
		t.Error("Expected MetricsConfig to be non-nil")
	}
}

func TestReadOptimizedWorkflowDataConfig(t *testing.T) {
	expectedNodes := 100
	config := ReadOptimizedWorkflowDataConfig(expectedNodes)

	// Check values
	if config.ExpectedNodes != expectedNodes {
		t.Errorf("Expected ExpectedNodes to be %d, got %d", expectedNodes, config.ExpectedNodes)
	}

	if config.ExpectedData != expectedNodes*2 {
		t.Errorf("Expected ExpectedData to be %d, got %d", expectedNodes*2, config.ExpectedData)
	}

	if config.MetricsConfig == nil {
		t.Error("Expected MetricsConfig to be non-nil")
	}
}

func TestHighConcurrencyWorkflowDataConfig(t *testing.T) {
	expectedNodes := 200
	config := HighConcurrencyWorkflowDataConfig(expectedNodes)

	// Check values
	if config.ExpectedNodes != expectedNodes {
		t.Errorf("Expected ExpectedNodes to be %d, got %d", expectedNodes, config.ExpectedNodes)
	}

	if config.ExpectedData != expectedNodes*2 {
		t.Errorf("Expected ExpectedData to be %d, got %d", expectedNodes*2, config.ExpectedData)
	}

	if config.MetricsConfig == nil {
		t.Error("Expected MetricsConfig to be non-nil")
	}
}

func TestLowMemoryWorkflowDataConfig(t *testing.T) {
	expectedNodes := 50
	config := LowMemoryWorkflowDataConfig(expectedNodes)

	// Check values
	if config.ExpectedNodes != expectedNodes {
		t.Errorf("Expected ExpectedNodes to be %d, got %d", expectedNodes, config.ExpectedNodes)
	}

	if config.ExpectedData != expectedNodes {
		t.Errorf("Expected ExpectedData to be %d, got %d", expectedNodes, config.ExpectedData)
	}

	if config.MetricsConfig == nil {
		t.Error("Expected MetricsConfig to be non-nil")
	}
}

func TestProductionWorkflowDataConfig(t *testing.T) {
	expectedNodes := 1000
	config := ProductionWorkflowDataConfig(expectedNodes)

	// Check values
	if config.ExpectedNodes != expectedNodes {
		t.Errorf("Expected ExpectedNodes to be %d, got %d", expectedNodes, config.ExpectedNodes)
	}

	if config.ExpectedData != expectedNodes*2 {
		t.Errorf("Expected ExpectedData to be %d, got %d", expectedNodes*2, config.ExpectedData)
	}

	if config.MetricsConfig == nil {
		t.Error("Expected MetricsConfig to be non-nil")
	}
}

func TestWithMetricsConfig(t *testing.T) {
	// Start with default config
	config := DefaultWorkflowDataConfig()

	// Create a new metrics config
	newMetricsConfig := metrics.DisabledMetricsConfig()

	// Apply the new metrics config
	updatedConfig := config.WithMetricsConfig(newMetricsConfig)

	// Check that the metrics config was updated
	if updatedConfig.MetricsConfig != newMetricsConfig {
		t.Error("Expected MetricsConfig to be updated")
	}

	// Check that other values remain unchanged
	if updatedConfig.ExpectedNodes != config.ExpectedNodes {
		t.Errorf("Expected ExpectedNodes to be %d, got %d", config.ExpectedNodes, updatedConfig.ExpectedNodes)
	}

	if updatedConfig.ExpectedData != config.ExpectedData {
		t.Errorf("Expected ExpectedData to be %d, got %d", config.ExpectedData, updatedConfig.ExpectedData)
	}
}
