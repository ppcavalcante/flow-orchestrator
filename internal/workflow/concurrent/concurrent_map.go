package concurrent

import (
	"encoding/json"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// Ensure ConcurrentMap implements ConcurrentMapI
var _ ConcurrentMapI = (*ConcurrentMap)(nil)

// ShardCount is the number of shards in the concurrent map
// This should be a power of 2 for efficient hashing
const ShardCount = 32

// ConcurrentMap is a thread-safe map implementation that uses sharding to reduce lock contention.
// It divides the map into multiple shards, each with its own lock, significantly reducing
// contention compared to a single lock for the entire map.
type ConcurrentMap struct {
	shards    [ShardCount]*mapShard
	hasher    func(string) uint32
	itemCount int64 // Track actual item count atomically
}

// mapShard represents a single shard of the concurrent map
type mapShard struct {
	items map[string]interface{}
	mu    sync.RWMutex
}

// NewConcurrentMap creates a new concurrent map
func NewConcurrentMap() *ConcurrentMap {
	cm := &ConcurrentMap{
		hasher: fnvHash,
	}

	for i := 0; i < ShardCount; i++ {
		cm.shards[i] = &mapShard{
			items: make(map[string]interface{}),
		}
	}

	return cm
}

// NewConcurrentMapWithCapacity creates a new concurrent map with a given capacity hint
func NewConcurrentMapWithCapacity(capacity int) *ConcurrentMap {
	cm := &ConcurrentMap{
		hasher: fnvHash,
	}

	// Distribute capacity across shards
	shardCapacity := capacity / ShardCount
	if shardCapacity < 1 {
		shardCapacity = 1
	}

	for i := 0; i < ShardCount; i++ {
		cm.shards[i] = &mapShard{
			items: make(map[string]interface{}, shardCapacity),
		}
	}

	return cm
}

// getShard returns the shard for a given key
func (m *ConcurrentMap) getShard(key string) *mapShard {
	hash := m.hasher(key)
	return m.shards[hash%ShardCount]
}

// fnvHash is a hash function for strings using FNV-1a
func fnvHash(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // Ignore error as fnv.New32a().Write never returns an error
	return h.Sum32()
}

// Set sets the given value under the specified key
func (m *ConcurrentMap) Set(key string, value interface{}) {
	// Get the shard
	shard := m.getShard(key)
	shard.mu.Lock()

	// Check if this is a new key
	_, exists := shard.items[key]

	// Set the value
	shard.items[key] = value
	shard.mu.Unlock()

	// Update count if this is a new key
	if !exists {
		atomic.AddInt64(&m.itemCount, 1)
	}
}

// Get retrieves an element from the map
func (m *ConcurrentMap) Get(key string) (interface{}, bool) {
	// Get the shard
	shard := m.getShard(key)
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	// Get the item from the shard
	val, ok := shard.items[key]
	return val, ok
}

// Count returns the number of elements in the map
func (m *ConcurrentMap) Count() int {
	return int(atomic.LoadInt64(&m.itemCount))
}

// Delete removes an element from the map
func (m *ConcurrentMap) Delete(key string) {
	// Get the shard
	shard := m.getShard(key)
	shard.mu.Lock()

	// Check if the key exists
	_, exists := shard.items[key]

	// Delete the key
	delete(shard.items, key)
	shard.mu.Unlock()

	// Update count if needed
	if exists {
		atomic.AddInt64(&m.itemCount, -1)
	}
}

// Has checks if a key exists in the map
func (m *ConcurrentMap) Has(key string) bool {
	// Get the shard
	shard := m.getShard(key)
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	// Check if the key exists
	_, exists := shard.items[key]
	return exists
}

// Keys returns all keys in the map
func (m *ConcurrentMap) Keys() []string {
	// Pre-allocate the slice based on count
	count := int(atomic.LoadInt64(&m.itemCount))
	keys := make([]string, 0, count)

	// Iterate through each shard
	for i := 0; i < ShardCount; i++ {
		shard := m.shards[i]
		shard.mu.RLock()

		// Append keys from this shard
		for key := range shard.items {
			keys = append(keys, key)
		}

		shard.mu.RUnlock()
	}

	return keys
}

// Items returns a copy of all items in the map
func (m *ConcurrentMap) Items() map[string]interface{} {
	// Pre-allocate the map based on count
	count := int(atomic.LoadInt64(&m.itemCount))
	items := make(map[string]interface{}, count)

	// Iterate through each shard
	for i := 0; i < ShardCount; i++ {
		shard := m.shards[i]
		shard.mu.RLock()

		// Copy items from this shard
		for key, value := range shard.items {
			items[key] = value
		}

		shard.mu.RUnlock()
	}

	return items
}

// ForEach executes a function for each item in the map
func (m *ConcurrentMap) ForEach(fn func(key string, value interface{})) {
	for i := 0; i < ShardCount; i++ {
		shard := m.shards[i]
		shard.mu.RLock()
		for k, v := range shard.items {
			fn(k, v)
		}
		shard.mu.RUnlock()
	}
}

// Clear removes all items from the map
func (m *ConcurrentMap) Clear() {
	// Iterate through each shard
	for i := 0; i < ShardCount; i++ {
		shard := m.shards[i]
		shard.mu.Lock()

		// Create a new map (more efficient than deleting all keys)
		shard.items = make(map[string]interface{})

		shard.mu.Unlock()
	}

	// Reset count
	atomic.StoreInt64(&m.itemCount, 0)
}

// MarshalJSON implements the json.Marshaler interface
func (m *ConcurrentMap) MarshalJSON() ([]byte, error) {
	// Use Items() to get a copy of all items
	items := m.Items()
	return json.Marshal(items)
}
