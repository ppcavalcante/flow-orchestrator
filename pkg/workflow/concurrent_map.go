package workflow

import (
	internalconcurrent "github.com/ppcavalcante/flow-orchestrator/internal/workflow/concurrent"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/concurrent"
)

// ConcurrentMap is a thread-safe map implementation.
// It provides a simple wrapper around the concurrent.Map type.
type ConcurrentMap struct {
	m *concurrent.Map
}

// NewConcurrentMap creates a new concurrent map.
func NewConcurrentMap() *ConcurrentMap {
	return &ConcurrentMap{
		m: concurrent.NewMap(),
	}
}

// NewConcurrentMapWithCapacity creates a new concurrent map with the specified capacity.
func NewConcurrentMapWithCapacity(capacity int) *ConcurrentMap {
	return &ConcurrentMap{
		m: concurrent.NewMapWithCapacity(capacity),
	}
}

// Set sets a value in the map.
func (m *ConcurrentMap) Set(key string, value interface{}) {
	m.m.Set(key, value)
}

// Get gets a value from the map.
func (m *ConcurrentMap) Get(key string) (interface{}, bool) {
	return m.m.Get(key)
}

// Delete deletes a key from the map.
func (m *ConcurrentMap) Delete(key string) {
	m.m.Delete(key)
}

// Len returns the number of items in the map.
func (m *ConcurrentMap) Len() int {
	return m.m.Len()
}

// Keys returns a slice of all keys in the map.
func (m *ConcurrentMap) Keys() []string {
	return m.m.Keys()
}

// ForEach executes a function for each key-value pair in the map.
func (m *ConcurrentMap) ForEach(fn func(key string, value interface{})) {
	m.m.ForEach(fn)
}

// Clear removes all items from the map.
func (m *ConcurrentMap) Clear() {
	m.m.Clear()
}

// Has checks if a key exists in the map.
func (m *ConcurrentMap) Has(key string) bool {
	_, exists := m.Get(key)
	return exists
}

// Count returns the number of items in the map.
func (m *ConcurrentMap) Count() int {
	return m.Len()
}

// Items returns a copy of all items in the map.
func (m *ConcurrentMap) Items() map[string]interface{} {
	result := make(map[string]interface{})
	m.ForEach(func(key string, value interface{}) {
		result[key] = value
	})
	return result
}

// ConcurrentMapI defines the interface for concurrent maps.
type ConcurrentMapI interface {
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

// MapType defines the type of concurrent map to create.
type MapType int

const (
	// MapTypeStandard is a standard concurrent map.
	MapTypeStandard MapType = iota
	// MapTypeSharded is an alias for MapTypeStandard (for backward compatibility)
	MapTypeSharded = MapTypeStandard
	// MapTypeReadOptimized is optimized for read-heavy workloads.
	MapTypeReadOptimized
	// MapTypeWriteOptimized is optimized for write-heavy workloads.
	MapTypeWriteOptimized
)

// MapConfig defines configuration options for creating concurrent maps.
type MapConfig struct {
	// Initial capacity hint
	Capacity int

	// Type of map to create
	Type MapType
}

// DefaultMapConfig returns the default map configuration.
func DefaultMapConfig() MapConfig {
	return MapConfig{
		Capacity: 0,
		Type:     MapTypeStandard,
	}
}

// ReadOptimizedMapConfig returns a configuration for read-optimized maps.
func ReadOptimizedMapConfig(capacity int) MapConfig {
	return MapConfig{
		Capacity: capacity,
		Type:     MapTypeReadOptimized,
	}
}

// ReadOptimizedMap wraps the internal read-optimized map (copy-on-write)
// for read-heavy workloads where lock-free reads are beneficial.
type ReadOptimizedMap struct {
	m *internalconcurrent.ReadMap
}

// NewReadMap creates a new read-optimized map using copy-on-write semantics.
func NewReadMap() ConcurrentMapI {
	return &ReadOptimizedMap{m: internalconcurrent.NewReadMap()}
}

// NewReadMapWithCapacity creates a new read-optimized map with the specified capacity.
func NewReadMapWithCapacity(capacity int) ConcurrentMapI {
	return &ReadOptimizedMap{m: internalconcurrent.NewReadMapWithCapacity(capacity)}
}

// Set stores a key-value pair in the map.
func (r *ReadOptimizedMap) Set(key string, value interface{}) { r.m.Set(key, value) }

// Get retrieves a value by key, returning the value and whether it exists.
func (r *ReadOptimizedMap) Get(key string) (interface{}, bool) { return r.m.Get(key) }

// Delete removes a key from the map.
func (r *ReadOptimizedMap) Delete(key string) { r.m.Delete(key) }

// Clear removes all entries from the map.
func (r *ReadOptimizedMap) Clear() { r.m.Clear() }

// Has returns true if the key exists in the map.
func (r *ReadOptimizedMap) Has(key string) bool { return r.m.Has(key) }

// Count returns the number of entries in the map.
func (r *ReadOptimizedMap) Count() int { return r.m.Count() }

// Keys returns all keys in the map.
func (r *ReadOptimizedMap) Keys() []string { return r.m.Keys() }

// Items returns a copy of all key-value pairs in the map.
func (r *ReadOptimizedMap) Items() map[string]interface{} { return r.m.Items() }

// ForEach iterates over all key-value pairs, calling fn for each entry.
func (r *ReadOptimizedMap) ForEach(fn func(key string, value interface{})) { r.m.ForEach(fn) }

// NewConcurrentMapFromConfig creates the appropriate concurrent map based on the config.
func NewConcurrentMapFromConfig(config MapConfig) ConcurrentMapI {
	switch config.Type {
	case MapTypeReadOptimized:
		return NewReadMapWithCapacity(config.Capacity)
	default:
		return NewConcurrentMapWithCapacity(config.Capacity)
	}
}
