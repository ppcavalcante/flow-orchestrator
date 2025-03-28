package workflow

import (
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewConcurrentMap(t *testing.T) {
	m := NewConcurrentMap()
	if m == nil {
		t.Fatal("NewConcurrentMap returned nil")
	}
	if m.m == nil {
		t.Fatal("Internal map is nil")
	}
}

func TestNewConcurrentMapWithCapacity(t *testing.T) {
	capacity := 10
	m := NewConcurrentMapWithCapacity(capacity)
	if m == nil {
		t.Fatal("NewConcurrentMapWithCapacity returned nil")
	}
	if m.m == nil {
		t.Fatal("Internal map is nil")
	}
}

func TestConcurrentMapSetGet(t *testing.T) {
	m := NewConcurrentMap()

	// Test setting and getting values
	m.Set("key1", "value1")
	m.Set("key2", 42)

	// Test getting existing values
	val1, exists := m.Get("key1")
	if !exists {
		t.Error("Get didn't find key1")
	}
	if val1 != "value1" {
		t.Errorf("Expected 'value1', got %v", val1)
	}

	val2, exists := m.Get("key2")
	if !exists {
		t.Error("Get didn't find key2")
	}
	if val2 != 42 {
		t.Errorf("Expected 42, got %v", val2)
	}

	// Test getting non-existent value
	_, exists = m.Get("nonexistent")
	if exists {
		t.Error("Get found a non-existent key")
	}
}

func TestConcurrentMapDelete(t *testing.T) {
	m := NewConcurrentMap()

	// Add some items
	m.Set("key1", "value1")
	m.Set("key2", "value2")

	// Verify items exist
	if _, exists := m.Get("key1"); !exists {
		t.Error("key1 should exist before deletion")
	}

	// Delete an item
	m.Delete("key1")

	// Verify item was deleted
	if _, exists := m.Get("key1"); exists {
		t.Error("key1 should not exist after deletion")
	}

	// Verify other item still exists
	if _, exists := m.Get("key2"); !exists {
		t.Error("key2 should still exist")
	}

	// Delete non-existent item (should not panic)
	m.Delete("nonexistent")
}

func TestConcurrentMapLen(t *testing.T) {
	m := NewConcurrentMap()

	// Empty map should have length 0
	if m.Len() != 0 {
		t.Errorf("Expected length 0, got %d", m.Len())
	}

	// Add items and check length
	m.Set("key1", "value1")
	if m.Len() != 1 {
		t.Errorf("Expected length 1, got %d", m.Len())
	}

	m.Set("key2", "value2")
	if m.Len() != 2 {
		t.Errorf("Expected length 2, got %d", m.Len())
	}

	// Delete item and check length
	m.Delete("key1")
	if m.Len() != 1 {
		t.Errorf("Expected length 1 after deletion, got %d", m.Len())
	}
}

func TestConcurrentMapKeys(t *testing.T) {
	m := NewConcurrentMap()

	// Empty map should return empty slice
	keys := m.Keys()
	if len(keys) != 0 {
		t.Errorf("Expected empty keys slice, got %v", keys)
	}

	// Add items
	m.Set("key1", "value1")
	m.Set("key2", "value2")
	m.Set("key3", "value3")

	// Get keys and sort them for deterministic comparison
	keys = m.Keys()
	sort.Strings(keys)

	// Check keys
	expected := []string{"key1", "key2", "key3"}
	if len(keys) != len(expected) {
		t.Errorf("Expected %d keys, got %d", len(expected), len(keys))
	}

	for i, key := range expected {
		if keys[i] != key {
			t.Errorf("Expected key %s at index %d, got %s", key, i, keys[i])
		}
	}
}

func TestConcurrentMapForEach(t *testing.T) {
	m := NewConcurrentMap()

	// Add items
	m.Set("key1", "value1")
	m.Set("key2", "value2")

	// Use ForEach to collect items
	items := make(map[string]interface{})
	m.ForEach(func(key string, value interface{}) {
		items[key] = value
	})

	// Check collected items
	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}

	if items["key1"] != "value1" {
		t.Errorf("Expected value1 for key1, got %v", items["key1"])
	}

	if items["key2"] != "value2" {
		t.Errorf("Expected value2 for key2, got %v", items["key2"])
	}
}

func TestConcurrentMapClear(t *testing.T) {
	m := NewConcurrentMap()

	// Add items
	m.Set("key1", "value1")
	m.Set("key2", "value2")

	// Clear the map
	m.Clear()

	// Check length
	if m.Len() != 0 {
		t.Errorf("Expected length 0 after Clear, got %d", m.Len())
	}

	// Check that items are gone
	if _, exists := m.Get("key1"); exists {
		t.Error("key1 should not exist after Clear")
	}

	if _, exists := m.Get("key2"); exists {
		t.Error("key2 should not exist after Clear")
	}
}

func TestConcurrentMapHas(t *testing.T) {
	m := NewConcurrentMap()

	// Check non-existent key
	if m.Has("key1") {
		t.Error("Has should return false for non-existent key")
	}

	// Add item
	m.Set("key1", "value1")

	// Check existing key
	if !m.Has("key1") {
		t.Error("Has should return true for existing key")
	}

	// Delete item
	m.Delete("key1")

	// Check deleted key
	if m.Has("key1") {
		t.Error("Has should return false for deleted key")
	}
}

func TestConcurrentMapCount(t *testing.T) {
	m := NewConcurrentMap()

	// Empty map should have count 0
	if m.Count() != 0 {
		t.Errorf("Expected count 0, got %d", m.Count())
	}

	// Add items and check count
	m.Set("key1", "value1")
	if m.Count() != 1 {
		t.Errorf("Expected count 1, got %d", m.Count())
	}

	m.Set("key2", "value2")
	if m.Count() != 2 {
		t.Errorf("Expected count 2, got %d", m.Count())
	}
}

func TestConcurrentMapItems(t *testing.T) {
	m := NewConcurrentMap()

	// Empty map should return empty map
	items := m.Items()
	if len(items) != 0 {
		t.Errorf("Expected empty items map, got %v", items)
	}

	// Add items
	m.Set("key1", "value1")
	m.Set("key2", 42)

	// Get items
	items = m.Items()

	// Check items
	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}

	if items["key1"] != "value1" {
		t.Errorf("Expected value1 for key1, got %v", items["key1"])
	}

	if items["key2"] != 42 {
		t.Errorf("Expected 42 for key2, got %v", items["key2"])
	}
}

func TestConcurrentMapMethods(t *testing.T) {
	t.Log("Running TestConcurrentMapMethods")
	m := NewConcurrentMap()

	// Test Set and Get
	m.Set("key1", "value1")
	val, exists := m.Get("key1")
	if !exists {
		t.Error("Get didn't find key1")
	}
	if val != "value1" {
		t.Errorf("Expected 'value1', got %v", val)
	}

	// Test Delete
	m.Delete("key1")
	_, exists = m.Get("key1")
	if exists {
		t.Error("Key should have been deleted")
	}

	// Test Len
	m.Set("key2", "value2")
	m.Set("key3", "value3")
	if m.Len() != 2 {
		t.Errorf("Expected length 2, got %d", m.Len())
	}

	// Test Keys
	keys := m.Keys()
	if len(keys) != 2 {
		t.Errorf("Expected 2 keys, got %d", len(keys))
	}
	sort.Strings(keys)
	if keys[0] != "key2" || keys[1] != "key3" {
		t.Errorf("Unexpected keys: %v", keys)
	}

	// Test ForEach
	visited := make(map[string]interface{})
	m.ForEach(func(key string, value interface{}) {
		visited[key] = value
	})
	if len(visited) != 2 {
		t.Errorf("ForEach should have visited 2 items, visited %d", len(visited))
	}
	if visited["key2"] != "value2" || visited["key3"] != "value3" {
		t.Errorf("ForEach visited unexpected items: %v", visited)
	}

	// Test Clear
	m.Clear()
	if m.Len() != 0 {
		t.Errorf("Map should be empty after Clear, got length %d", m.Len())
	}

	// Test Has
	m.Set("key4", "value4")
	if !m.Has("key4") {
		t.Error("Has should return true for existing key")
	}
	if m.Has("nonexistent") {
		t.Error("Has should return false for nonexistent key")
	}

	// Test Count
	if m.Count() != 1 {
		t.Errorf("Expected count 1, got %d", m.Count())
	}

	// Test Items
	m.Set("key5", "value5")
	items := m.Items()
	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}
	if items["key4"] != "value4" || items["key5"] != "value5" {
		t.Errorf("Unexpected items: %v", items)
	}
}

func TestDefaultMapConfig(t *testing.T) {
	config := DefaultMapConfig()
	if config.Capacity != 0 {
		t.Errorf("Expected default capacity 0, got %d", config.Capacity)
	}
	if config.Type != MapTypeStandard {
		t.Errorf("Expected default type MapTypeStandard, got %v", config.Type)
	}
}

func TestReadOptimizedMapConfig(t *testing.T) {
	capacity := 100
	config := ReadOptimizedMapConfig(capacity)
	if config.Capacity != capacity {
		t.Errorf("Expected capacity %d, got %d", capacity, config.Capacity)
	}
	if config.Type != MapTypeReadOptimized {
		t.Errorf("Expected type MapTypeReadOptimized, got %v", config.Type)
	}
}

func TestNewReadMap(t *testing.T) {
	m := NewReadMap()
	if m == nil {
		t.Fatal("NewReadMap returned nil")
	}

	// Basic functionality test
	m.Set("key", "value")
	val, exists := m.Get("key")
	if !exists || val != "value" {
		t.Errorf("Expected 'value', got %v, exists: %v", val, exists)
	}
}

func TestNewReadMapWithCapacity(t *testing.T) {
	capacity := 10
	m := NewReadMapWithCapacity(capacity)
	if m == nil {
		t.Fatal("NewReadMapWithCapacity returned nil")
	}

	// Basic functionality test
	m.Set("key", "value")
	val, exists := m.Get("key")
	if !exists || val != "value" {
		t.Errorf("Expected 'value', got %v, exists: %v", val, exists)
	}
}

func TestNewConcurrentMapFromConfig(t *testing.T) {
	// Test with standard config
	config := DefaultMapConfig()
	m1 := NewConcurrentMapFromConfig(config)
	if m1 == nil {
		t.Fatal("NewConcurrentMapFromConfig with standard config returned nil")
	}

	// Test with read-optimized config
	config = ReadOptimizedMapConfig(10)
	m2 := NewConcurrentMapFromConfig(config)
	if m2 == nil {
		t.Fatal("NewConcurrentMapFromConfig with read-optimized config returned nil")
	}

	// Basic functionality test for both map types
	m1.Set("key", "value")
	val, exists := m1.Get("key")
	if !exists || val != "value" {
		t.Errorf("Standard map: Expected 'value', got %v, exists: %v", val, exists)
	}

	m2.Set("key", "value")
	val, exists = m2.Get("key")
	if !exists || val != "value" {
		t.Errorf("Read-optimized map: Expected 'value', got %v, exists: %v", val, exists)
	}
}

func TestConcurrentMapOperations(t *testing.T) {
	t.Run("Basic Operations", func(t *testing.T) {
		m := NewConcurrentMap()

		// Test Set and Get
		m.Set("key1", "value1")
		val, exists := m.Get("key1")
		assert.True(t, exists)
		assert.Equal(t, "value1", val)

		// Test non-existent key
		val, exists = m.Get("nonexistent")
		assert.False(t, exists)
		assert.Nil(t, val)

		// Test Delete
		m.Delete("key1")
		val, exists = m.Get("key1")
		assert.False(t, exists)
		assert.Nil(t, val)

		// Test Has
		m.Set("key2", "value2")
		assert.True(t, m.Has("key2"))
		assert.False(t, m.Has("nonexistent"))
	})

	t.Run("Concurrent Operations", func(t *testing.T) {
		m := NewConcurrentMap()
		var wg sync.WaitGroup
		numGoroutines := 100
		numOperations := 1000

		// Concurrent writes
		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func(id int) {
				defer wg.Done()
				for j := 0; j < numOperations; j++ {
					key := string(rune(id))
					m.Set(key, j)
				}
			}(i)
		}
		wg.Wait()

		// Verify final state
		assert.Equal(t, numGoroutines, m.Count())

		// Test Keys
		keys := m.Keys()
		assert.Equal(t, numGoroutines, len(keys))

		// Test Items
		items := m.Items()
		assert.Equal(t, numGoroutines, len(items))

		// Test ForEach
		count := 0
		m.ForEach(func(key string, value interface{}) {
			count++
			assert.NotNil(t, value)
		})
		assert.Equal(t, numGoroutines, count)

		// Test Clear
		m.Clear()
		assert.Equal(t, 0, m.Count())
	})

	t.Run("Map Configurations", func(t *testing.T) {
		// Test DefaultMapConfig
		config := DefaultMapConfig()
		assert.Equal(t, MapTypeStandard, config.Type)
		assert.Equal(t, 0, config.Capacity)

		// Test ReadOptimizedMapConfig
		config = ReadOptimizedMapConfig(32)
		assert.Equal(t, MapTypeReadOptimized, config.Type)
		assert.Equal(t, 32, config.Capacity)

		// Test NewConcurrentMapFromConfig
		m := NewConcurrentMapFromConfig(config)
		assert.NotNil(t, m)

		// Test operations with configured map
		m.Set("key", "value")
		val, exists := m.Get("key")
		assert.True(t, exists)
		assert.Equal(t, "value", val)
	})

	t.Run("Edge Cases", func(t *testing.T) {
		m := NewConcurrentMapWithCapacity(1)

		// Test empty string key
		m.Set("", "empty")
		val, exists := m.Get("")
		assert.True(t, exists)
		assert.Equal(t, "empty", val)

		// Test nil value
		m.Set("nil", nil)
		val, exists = m.Get("nil")
		assert.True(t, exists)
		assert.Nil(t, val)

		// Test overwrite
		m.Set("key", "value1")
		m.Set("key", "value2")
		val, exists = m.Get("key")
		assert.True(t, exists)
		assert.Equal(t, "value2", val)

		// Test delete non-existent
		m.Delete("nonexistent")
		assert.False(t, m.Has("nonexistent"))
	})
}
