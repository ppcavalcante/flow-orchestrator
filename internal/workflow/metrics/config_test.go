package metrics

import (
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
)

func TestDefaultMetricsConfig(t *testing.T) {
	config := DefaultMetricsConfig()

	if !config.Enabled {
		t.Error("DefaultMetricsConfig should be enabled")
	}

	if config.SamplingRate != 1.0 {
		t.Errorf("DefaultMetricsConfig SamplingRate expected 1.0, got %f", config.SamplingRate)
	}

	if !config.EnableOperationTiming {
		t.Error("DefaultMetricsConfig EnableOperationTiming should be true")
	}

	if !config.EnableLockContention {
		t.Error("DefaultMetricsConfig EnableLockContention should be true")
	}

	if config.EnableMemoryTracking {
		t.Error("DefaultMetricsConfig EnableMemoryTracking should be false")
	}

	if config.SlowOperationThreshold != 1*time.Millisecond {
		t.Errorf("DefaultMetricsConfig SlowOperationThreshold expected 1ms, got %v", config.SlowOperationThreshold)
	}

	if config.HighContentionThreshold != 100 {
		t.Errorf("DefaultMetricsConfig HighContentionThreshold expected 100, got %d", config.HighContentionThreshold)
	}
}

func TestProductionMetricsConfig(t *testing.T) {
	config := ProductionMetricsConfig()

	if !config.Enabled {
		t.Error("ProductionMetricsConfig should be enabled")
	}

	if config.SamplingRate != 0.01 {
		t.Errorf("ProductionMetricsConfig SamplingRate expected 0.01, got %f", config.SamplingRate)
	}

	if !config.EnableOperationTiming {
		t.Error("ProductionMetricsConfig EnableOperationTiming should be true")
	}

	if !config.EnableLockContention {
		t.Error("ProductionMetricsConfig EnableLockContention should be true")
	}

	if config.EnableMemoryTracking {
		t.Error("ProductionMetricsConfig EnableMemoryTracking should be false")
	}

	if config.SlowOperationThreshold != 10*time.Millisecond {
		t.Errorf("ProductionMetricsConfig SlowOperationThreshold expected 10ms, got %v", config.SlowOperationThreshold)
	}

	if config.HighContentionThreshold != 1000 {
		t.Errorf("ProductionMetricsConfig HighContentionThreshold expected 1000, got %d", config.HighContentionThreshold)
	}
}

func TestDisabledMetricsConfig(t *testing.T) {
	config := DisabledMetricsConfig()

	if config.Enabled {
		t.Error("DisabledMetricsConfig should be disabled")
	}
}

func TestShouldCollect(t *testing.T) {
	// Test with enabled = false
	config := &MetricsConfig{Enabled: false}
	if config.ShouldCollect() {
		t.Error("ShouldCollect should return false when Enabled is false")
	}

	// Test with sampling rate = 1.0
	config = &MetricsConfig{Enabled: true, SamplingRate: 1.0}
	if !config.ShouldCollect() {
		t.Error("ShouldCollect should return true when SamplingRate is 1.0")
	}

	// Test with sampling rate between 0 and 1
	// Mock the random function to return a predictable value
	originalRandFunc := utils.SecureRandomFloat64
	defer func() {
		utils.SecureRandomFloat64 = originalRandFunc
	}()

	// Mock to return 0.4
	utils.SecureRandomFloat64 = func() float64 { return 0.4 }

	config = &MetricsConfig{Enabled: true, SamplingRate: 0.5}
	if !config.ShouldCollect() {
		t.Error("ShouldCollect should return true when random value (0.4) < SamplingRate (0.5)")
	}

	config = &MetricsConfig{Enabled: true, SamplingRate: 0.3}
	if config.ShouldCollect() {
		t.Error("ShouldCollect should return false when random value (0.4) >= SamplingRate (0.3)")
	}
}

func TestShouldCollectSpecificMetrics(t *testing.T) {
	// Create a config that always collects (no sampling)
	config := &MetricsConfig{
		Enabled:               true,
		SamplingRate:          1.0,
		EnableOperationTiming: true,
		EnableLockContention:  false,
		EnableMemoryTracking:  true,
	}

	if !config.ShouldCollectOperationTiming() {
		t.Error("ShouldCollectOperationTiming should return true when enabled")
	}

	if config.ShouldCollectLockContention() {
		t.Error("ShouldCollectLockContention should return false when disabled")
	}

	if !config.ShouldCollectMemoryTracking() {
		t.Error("ShouldCollectMemoryTracking should return true when enabled")
	}

	// Test with disabled config
	config.Enabled = false

	if config.ShouldCollectOperationTiming() {
		t.Error("ShouldCollectOperationTiming should return false when config is disabled")
	}

	if config.ShouldCollectLockContention() {
		t.Error("ShouldCollectLockContention should return false when config is disabled")
	}

	if config.ShouldCollectMemoryTracking() {
		t.Error("ShouldCollectMemoryTracking should return false when config is disabled")
	}
}

func TestWithMethods(t *testing.T) {
	original := DefaultMetricsConfig()

	// Test WithSamplingRate
	modified := original.WithSamplingRate(0.5)
	if modified.SamplingRate != 0.5 {
		t.Errorf("WithSamplingRate expected 0.5, got %f", modified.SamplingRate)
	}
	// Original should be unchanged
	if original.SamplingRate != 1.0 {
		t.Errorf("Original config should be unchanged, expected 1.0, got %f", original.SamplingRate)
	}

	// Test WithOperationTiming
	modified = original.WithOperationTiming(false)
	if modified.EnableOperationTiming {
		t.Error("WithOperationTiming(false) should disable operation timing")
	}

	// Test WithLockContention
	modified = original.WithLockContention(false)
	if modified.EnableLockContention {
		t.Error("WithLockContention(false) should disable lock contention")
	}

	// Test WithMemoryTracking
	modified = original.WithMemoryTracking(true)
	if !modified.EnableMemoryTracking {
		t.Error("WithMemoryTracking(true) should enable memory tracking")
	}
}
