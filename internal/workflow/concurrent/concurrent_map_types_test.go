package concurrent

import (
	"testing"
)

func TestDefaultMapConfig(t *testing.T) {
	config := DefaultMapConfig()

	if config.Capacity != 16 {
		t.Errorf("DefaultMapConfig.Capacity = %d, want 16", config.Capacity)
	}

	if config.Type != MapTypeSharded {
		t.Errorf("DefaultMapConfig.Type = %v, want MapTypeSharded", config.Type)
	}
}

func TestReadOptimizedMapConfig(t *testing.T) {
	capacity := 32
	config := ReadOptimizedMapConfig(capacity)

	if config.Capacity != capacity {
		t.Errorf("ReadOptimizedMapConfig.Capacity = %d, want %d", config.Capacity, capacity)
	}

	if config.Type != MapTypeReadOptimized {
		t.Errorf("ReadOptimizedMapConfig.Type = %v, want MapTypeReadOptimized", config.Type)
	}
}

func TestNewConcurrentMapFromConfig(t *testing.T) {
	// Test with sharded map config
	shardedConfig := MapConfig{
		Capacity: 16,
		Type:     MapTypeSharded,
	}

	shardedMap := NewConcurrentMapFromConfig(shardedConfig)
	_, isConcurrentMap := shardedMap.(*ConcurrentMap)
	if !isConcurrentMap {
		t.Errorf("NewConcurrentMapFromConfig with MapTypeSharded should return *ConcurrentMap, got %T", shardedMap)
	}

	// Test with read-optimized map config
	readOptimizedConfig := MapConfig{
		Capacity: 16,
		Type:     MapTypeReadOptimized,
	}

	readOptimizedMap := NewConcurrentMapFromConfig(readOptimizedConfig)
	_, isReadMap := readOptimizedMap.(*ReadMap)
	if !isReadMap {
		t.Errorf("NewConcurrentMapFromConfig with MapTypeReadOptimized should return *ReadMap, got %T", readOptimizedMap)
	}
}
