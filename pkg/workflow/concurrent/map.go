// Package concurrent provides concurrent data structures for the workflow package.
// This is a simplified public API for the internal concurrent system.
package concurrent

import (
	"sync"
)

// Map is a thread-safe map implementation.
type Map struct {
	mu   sync.RWMutex
	data map[string]interface{}
}

// NewMap creates a new concurrent map.
func NewMap() *Map {
	return &Map{
		data: make(map[string]interface{}),
	}
}

// NewMapWithCapacity creates a new concurrent map with the specified capacity.
func NewMapWithCapacity(capacity int) *Map {
	return &Map{
		data: make(map[string]interface{}, capacity),
	}
}

// Set sets a value in the map.
func (m *Map) Set(key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}

// Get gets a value from the map.
func (m *Map) Get(key string) (interface{}, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.data[key]
	return value, ok
}

// Delete deletes a key from the map.
func (m *Map) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
}

// Len returns the number of items in the map.
func (m *Map) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Keys returns all keys in the map.
func (m *Map) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}

	return keys
}

// ForEach executes the given function for each key-value pair in the map.
func (m *Map) ForEach(fn func(key string, value interface{})) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for k, v := range m.data {
		fn(k, v)
	}
}

// Clear removes all items from the map.
func (m *Map) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = make(map[string]interface{})
}
