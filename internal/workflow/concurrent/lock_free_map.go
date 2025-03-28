package concurrent

import (
	"sync"
	"sync/atomic"
)

// ReadMap is an implementation of a map optimized for read-heavy workloads
// It uses a copy-on-write approach to ensure reads never block.
type ReadMap struct {
	// Atomic pointer to the current read-only map
	// This allows lock-free reads while ensuring safe updates
	value atomic.Pointer[readMapData]
	mu    sync.Mutex // Used only for writes
}

// readMapData holds the actual map data
type readMapData struct {
	data map[string]interface{}
}

// NewReadMap creates a new read-optimized map
func NewReadMap() *ReadMap {
	m := &ReadMap{}
	initialData := &readMapData{
		data: make(map[string]interface{}),
	}
	m.value.Store(initialData)
	return m
}

// NewReadMapWithCapacity creates a new read-optimized map with a specified capacity
func NewReadMapWithCapacity(capacity int) *ReadMap {
	m := &ReadMap{}
	initialData := &readMapData{
		data: make(map[string]interface{}, capacity),
	}
	m.value.Store(initialData)
	return m
}

// Get retrieves a value
// This operation is completely lock-free
func (m *ReadMap) Get(key string) (interface{}, bool) {
	// Atomic load of the current map
	currentData := m.value.Load()
	// Access the map (safe since this map is read-only)
	val, ok := currentData.data[key]
	return val, ok
}

// Has checks if a key exists in the map
// This operation is completely lock-free
func (m *ReadMap) Has(key string) bool {
	currentData := m.value.Load()
	_, ok := currentData.data[key]
	return ok
}

// Set adds or updates a value
// This operation creates a new copy of the map for thread safety
func (m *ReadMap) Set(key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get current data
	currentData := m.value.Load()

	// Create a new map with the current data plus the new item
	newData := &readMapData{
		data: make(map[string]interface{}, len(currentData.data)+1),
	}

	// Copy all current items
	for k, v := range currentData.data {
		newData.data[k] = v
	}

	// Add the new item
	newData.data[key] = value

	// Atomically replace the current map with the new one
	m.value.Store(newData)
}

// Delete removes an item from the map
func (m *ReadMap) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get current data
	currentData := m.value.Load()

	// If the key doesn't exist, nothing to do
	if _, exists := currentData.data[key]; !exists {
		return
	}

	// Create a new map without the item
	newData := &readMapData{
		data: make(map[string]interface{}, len(currentData.data)),
	}

	// Copy all current items except the one to delete
	for k, v := range currentData.data {
		if k != key {
			newData.data[k] = v
		}
	}

	// Atomically replace the current map with the new one
	m.value.Store(newData)
}

// Count returns the number of items in the map
func (m *ReadMap) Count() int {
	currentData := m.value.Load()
	return len(currentData.data)
}

// Keys returns all keys in the map
func (m *ReadMap) Keys() []string {
	currentData := m.value.Load()
	keys := make([]string, 0, len(currentData.data))

	for k := range currentData.data {
		keys = append(keys, k)
	}

	return keys
}

// Items returns a copy of all items in the map
func (m *ReadMap) Items() map[string]interface{} {
	currentData := m.value.Load()
	result := make(map[string]interface{}, len(currentData.data))

	for k, v := range currentData.data {
		result[k] = v
	}

	return result
}

// Clear removes all items from the map
func (m *ReadMap) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create an empty map and store it
	newData := &readMapData{
		data: make(map[string]interface{}),
	}
	m.value.Store(newData)
}

// Snapshot returns a snapshot of the map for atomic operations
// This allows you to perform multiple reads from the same state
func (m *ReadMap) Snapshot() map[string]interface{} {
	// Simply return the current data (which is already a snapshot)
	currentData := m.value.Load()
	return currentData.data
}

// ForEach executes a function for each item in the map
func (m *ReadMap) ForEach(fn func(key string, value interface{})) {
	// Get current snapshot of the map
	currentData := m.value.Load()

	// Iterate through the snapshot (which is read-only)
	for k, v := range currentData.data {
		fn(k, v)
	}
}
