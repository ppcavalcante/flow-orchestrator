package workflow

import (
	internalconcurrent "github.com/ppcavalcante/flow-orchestrator/internal/workflow/concurrent"
)

// concurrentMap is a thread-safe map implementation.
// It provides a simple wrapper around the internal concurrent.Map type.
type concurrentMap struct {
	m *internalconcurrent.Map
}

// newConcurrentMap creates a new concurrent map.
func newConcurrentMap() *concurrentMap {
	return &concurrentMap{
		m: internalconcurrent.NewMap(),
	}
}

// newConcurrentMapWithCapacity creates a new concurrent map with the specified capacity.
func newConcurrentMapWithCapacity(capacity int) *concurrentMap {
	return &concurrentMap{
		m: internalconcurrent.NewMapWithCapacity(capacity),
	}
}

// Set sets a value in the map.
func (m *concurrentMap) Set(key string, value interface{}) {
	m.m.Set(key, value)
}

// Get gets a value from the map.
func (m *concurrentMap) Get(key string) (interface{}, bool) {
	return m.m.Get(key)
}

// Delete deletes a key from the map.
func (m *concurrentMap) Delete(key string) {
	m.m.Delete(key)
}

// Len returns the number of items in the map.
func (m *concurrentMap) Len() int {
	return m.m.Len()
}

// Keys returns a slice of all keys in the map.
func (m *concurrentMap) Keys() []string {
	return m.m.Keys()
}

// ForEach executes a function for each key-value pair in the map.
func (m *concurrentMap) ForEach(fn func(key string, value interface{})) {
	m.m.ForEach(fn)
}

// Clear removes all items from the map.
func (m *concurrentMap) Clear() {
	m.m.Clear()
}

// Has checks if a key exists in the map.
func (m *concurrentMap) Has(key string) bool {
	_, exists := m.Get(key)
	return exists
}

// Count returns the number of items in the map.
func (m *concurrentMap) Count() int {
	return m.Len()
}

// Items returns a copy of all items in the map.
func (m *concurrentMap) Items() map[string]interface{} {
	result := make(map[string]interface{})
	m.ForEach(func(key string, value interface{}) {
		result[key] = value
	})
	return result
}

// concurrentMapI defines the interface for concurrent maps.
type concurrentMapI interface {
	// Core operations
	Set(key string, value interface{})
	Get(key string) (interface{}, bool)
	Delete(key string)
	Clear()

	// Query operations
	Has(key string) bool
	Count() int
	Keys() []string
	Items() map[string]interface{}

	// Iteration
	ForEach(fn func(key string, value interface{}))
}

// mapType defines the type of concurrent map to create.
type mapType int

const (
	// mapTypeStandard is a standard concurrent map.
	mapTypeStandard mapType = iota
	// mapTypeReadOptimized is optimized for read-heavy workloads.
	mapTypeReadOptimized
)

// mapConfig defines configuration options for creating concurrent maps.
type mapConfig struct {
	// Initial capacity hint
	Capacity int

	// Type of map to create
	Type mapType
}

// defaultMapConfig returns the default map configuration.
func defaultMapConfig() mapConfig {
	return mapConfig{
		Capacity: 0,
		Type:     mapTypeStandard,
	}
}

// readOptimizedMapConfig returns a configuration for read-optimized maps.
func readOptimizedMapConfig(capacity int) mapConfig {
	return mapConfig{
		Capacity: capacity,
		Type:     mapTypeReadOptimized,
	}
}

// readOptimizedMap wraps the internal read-optimized map (copy-on-write)
// for read-heavy workloads where lock-free reads are beneficial.
type readOptimizedMap struct {
	m *internalconcurrent.ReadMap
}

// newReadMap creates a new read-optimized map using copy-on-write semantics.
func newReadMap() concurrentMapI {
	return &readOptimizedMap{m: internalconcurrent.NewReadMap()}
}

// newReadMapWithCapacity creates a new read-optimized map with the specified capacity.
func newReadMapWithCapacity(capacity int) concurrentMapI {
	return &readOptimizedMap{m: internalconcurrent.NewReadMapWithCapacity(capacity)}
}

// Set stores a key-value pair in the map.
func (r *readOptimizedMap) Set(key string, value interface{}) { r.m.Set(key, value) }

// Get retrieves a value by key, returning the value and whether it exists.
func (r *readOptimizedMap) Get(key string) (interface{}, bool) { return r.m.Get(key) }

// Delete removes a key from the map.
func (r *readOptimizedMap) Delete(key string) { r.m.Delete(key) }

// Clear removes all entries from the map.
func (r *readOptimizedMap) Clear() { r.m.Clear() }

// Has returns true if the key exists in the map.
func (r *readOptimizedMap) Has(key string) bool { return r.m.Has(key) }

// Count returns the number of entries in the map.
func (r *readOptimizedMap) Count() int { return r.m.Count() }

// Keys returns all keys in the map.
func (r *readOptimizedMap) Keys() []string { return r.m.Keys() }

// Items returns a copy of all key-value pairs in the map.
func (r *readOptimizedMap) Items() map[string]interface{} { return r.m.Items() }

// ForEach iterates over all key-value pairs, calling fn for each entry.
func (r *readOptimizedMap) ForEach(fn func(key string, value interface{})) { r.m.ForEach(fn) }

// newConcurrentMapFromConfig creates the appropriate concurrent map based on the config.
func newConcurrentMapFromConfig(config mapConfig) concurrentMapI {
	switch config.Type {
	case mapTypeReadOptimized:
		return newReadMapWithCapacity(config.Capacity)
	default:
		return newConcurrentMapWithCapacity(config.Capacity)
	}
}
