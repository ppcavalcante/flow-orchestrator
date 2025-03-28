package workflow

import (
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow/metrics"
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

	if config.MaxInternStringLength != 128 {
		t.Errorf("Expected MaxInternStringLength to be 128, got %d", config.MaxInternStringLength)
	}

	if config.InternStringCapacity != 128 {
		t.Errorf("Expected InternStringCapacity to be 128, got %d", config.InternStringCapacity)
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

	// Other values should be the same as default
	if config.MaxInternStringLength != 128 {
		t.Errorf("Expected MaxInternStringLength to be 128, got %d", config.MaxInternStringLength)
	}

	if config.InternStringCapacity != 128 {
		t.Errorf("Expected InternStringCapacity to be 128, got %d", config.InternStringCapacity)
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

	// Other values should be the same as default
	if config.MaxInternStringLength != 128 {
		t.Errorf("Expected MaxInternStringLength to be 128, got %d", config.MaxInternStringLength)
	}

	if config.InternStringCapacity != 128 {
		t.Errorf("Expected InternStringCapacity to be 128, got %d", config.InternStringCapacity)
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

	// Low memory config has different string interning settings
	if config.MaxInternStringLength != 64 {
		t.Errorf("Expected MaxInternStringLength to be 64, got %d", config.MaxInternStringLength)
	}

	if config.InternStringCapacity != expectedNodes*2 {
		t.Errorf("Expected InternStringCapacity to be %d, got %d", expectedNodes*2, config.InternStringCapacity)
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

	// Other values should be the same as default
	if config.MaxInternStringLength != 128 {
		t.Errorf("Expected MaxInternStringLength to be 128, got %d", config.MaxInternStringLength)
	}

	if config.InternStringCapacity != 128 {
		t.Errorf("Expected InternStringCapacity to be 128, got %d", config.InternStringCapacity)
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

	if updatedConfig.MaxInternStringLength != config.MaxInternStringLength {
		t.Errorf("Expected MaxInternStringLength to be %d, got %d", config.MaxInternStringLength, updatedConfig.MaxInternStringLength)
	}

	if updatedConfig.InternStringCapacity != config.InternStringCapacity {
		t.Errorf("Expected InternStringCapacity to be %d, got %d", config.InternStringCapacity, updatedConfig.InternStringCapacity)
	}
}
