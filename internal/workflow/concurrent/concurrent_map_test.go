package concurrent

import (
	"sync"
	"testing"
)

func TestNewConcurrentMap(t *testing.T) {
	cm := NewConcurrentMap()

	if cm == nil {
		t.Fatal("NewConcurrentMap() returned nil")
	}

	if cm.hasher == nil {
		t.Error("ConcurrentMap.hasher should not be nil")
	}

	for i := 0; i < ShardCount; i++ {
		if cm.shards[i] == nil {
			t.Errorf("ConcurrentMap.shards[%d] should not be nil", i)
		}

		if cm.shards[i].items == nil {
			t.Errorf("ConcurrentMap.shards[%d].items should not be nil", i)
		}
	}
}

func TestNewConcurrentMapWithCapacity(t *testing.T) {
	capacity := 32
	cm := NewConcurrentMapWithCapacity(capacity)

	if cm == nil {
		t.Fatal("NewConcurrentMapWithCapacity() returned nil")
	}

	if cm.hasher == nil {
		t.Error("ConcurrentMap.hasher should not be nil")
	}

	for i := 0; i < ShardCount; i++ {
		if cm.shards[i] == nil {
			t.Errorf("ConcurrentMap.shards[%d] should not be nil", i)
		}

		if cm.shards[i].items == nil {
			t.Errorf("ConcurrentMap.shards[%d].items should not be nil", i)
		}
	}
}

func TestConcurrentMapSetGet(t *testing.T) {
	cm := NewConcurrentMap()

	// Test Set and Get
	key := "testKey"
	value := "testValue"

	cm.Set(key, value)

	// Test Get with existing key
	retrievedValue, ok := cm.Get(key)
	if !ok {
		t.Errorf("Get(%q) returned ok=false, want true", key)
	}

	if retrievedValue != value {
		t.Errorf("Get(%q) = %v, want %v", key, retrievedValue, value)
	}

	// Test Get with non-existent key
	_, ok = cm.Get("nonExistentKey")
	if ok {
		t.Error("Get(nonExistentKey) returned ok=true, want false")
	}
}

func TestConcurrentMapHas(t *testing.T) {
	cm := NewConcurrentMap()

	key := "testKey"
	value := "testValue"

	// Test Has with non-existent key
	if cm.Has(key) {
		t.Errorf("Has(%q) = true, want false for non-existent key", key)
	}

	// Add the key and test again
	cm.Set(key, value)

	if !cm.Has(key) {
		t.Errorf("Has(%q) = false, want true for existing key", key)
	}
}

func TestConcurrentMapDelete(t *testing.T) {
	cm := NewConcurrentMap()

	key := "testKey"
	value := "testValue"

	// Add a key
	cm.Set(key, value)

	// Verify it exists
	if !cm.Has(key) {
		t.Fatalf("Key %q should exist after Set", key)
	}

	// Delete the key
	cm.Delete(key)

	// Verify it no longer exists
	if cm.Has(key) {
		t.Errorf("Key %q should not exist after Delete", key)
	}
}

func TestConcurrentMapCount(t *testing.T) {
	cm := NewConcurrentMap()

	// Initial count should be 0
	if count := cm.Count(); count != 0 {
		t.Errorf("Initial Count() = %d, want 0", count)
	}

	// Add some items
	cm.Set("key1", "value1")
	cm.Set("key2", "value2")
	cm.Set("key3", "value3")

	// Count should be 3
	if count := cm.Count(); count != 3 {
		t.Errorf("Count() after adding 3 items = %d, want 3", count)
	}

	// Delete an item
	cm.Delete("key2")

	// Count should be 2
	if count := cm.Count(); count != 2 {
		t.Errorf("Count() after deleting 1 item = %d, want 2", count)
	}
}

func TestConcurrentMapKeys(t *testing.T) {
	cm := NewConcurrentMap()

	// Add some items
	cm.Set("key1", "value1")
	cm.Set("key2", "value2")
	cm.Set("key3", "value3")

	// Get the keys
	keys := cm.Keys()

	// Check that we have the expected number of keys
	if len(keys) != 3 {
		t.Errorf("Keys() returned %d keys, want 3", len(keys))
	}

	// Check that all expected keys are present
	expectedKeys := map[string]bool{
		"key1": true,
		"key2": true,
		"key3": true,
	}

	for _, key := range keys {
		if !expectedKeys[key] {
			t.Errorf("Keys() returned unexpected key: %q", key)
		}
		// Mark this key as found
		delete(expectedKeys, key)
	}

	// Check that all expected keys were found
	if len(expectedKeys) != 0 {
		t.Errorf("Keys() did not return all expected keys. Missing: %v", expectedKeys)
	}
}

func TestConcurrentMapItems(t *testing.T) {
	cm := NewConcurrentMap()

	// Add some items
	cm.Set("key1", "value1")
	cm.Set("key2", "value2")
	cm.Set("key3", "value3")

	// Get all items
	items := cm.Items()

	// Check that we have the expected number of items
	if len(items) != 3 {
		t.Errorf("Items() returned %d items, want 3", len(items))
	}

	// Check that all expected items are present
	expectedItems := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	for key, value := range items {
		expectedValue, ok := expectedItems[key]
		if !ok {
			t.Errorf("Items() returned unexpected key: %q", key)
			continue
		}

		if value != expectedValue {
			t.Errorf("Items()[%q] = %v, want %v", key, value, expectedValue)
		}

		// Mark this item as found
		delete(expectedItems, key)
	}

	// Check that all expected items were found
	if len(expectedItems) != 0 {
		t.Errorf("Items() did not return all expected items. Missing: %v", expectedItems)
	}
}

func TestConcurrentMapForEach(t *testing.T) {
	cm := NewConcurrentMap()

	// Add some items
	cm.Set("key1", "value1")
	cm.Set("key2", "value2")
	cm.Set("key3", "value3")

	// Use ForEach to collect items
	items := make(map[string]interface{})
	cm.ForEach(func(key string, value interface{}) {
		items[key] = value
	})

	// Check that we have the expected number of items
	if len(items) != 3 {
		t.Errorf("ForEach collected %d items, want 3", len(items))
	}

	// Check that all expected items are present
	expectedItems := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	for key, value := range items {
		expectedValue, ok := expectedItems[key]
		if !ok {
			t.Errorf("ForEach collected unexpected key: %q", key)
			continue
		}

		if value != expectedValue {
			t.Errorf("ForEach collected items[%q] = %v, want %v", key, value, expectedValue)
		}

		// Mark this item as found
		delete(expectedItems, key)
	}

	// Check that all expected items were found
	if len(expectedItems) != 0 {
		t.Errorf("ForEach did not collect all expected items. Missing: %v", expectedItems)
	}
}

func TestConcurrentMapClear(t *testing.T) {
	cm := NewConcurrentMap()

	// Add some items
	cm.Set("key1", "value1")
	cm.Set("key2", "value2")
	cm.Set("key3", "value3")

	// Verify items were added
	if count := cm.Count(); count != 3 {
		t.Fatalf("Count() after adding 3 items = %d, want 3", count)
	}

	// Clear the map
	cm.Clear()

	// Verify the map is empty
	if count := cm.Count(); count != 0 {
		t.Errorf("Count() after Clear() = %d, want 0", count)
	}

	// Verify no keys remain
	if keys := cm.Keys(); len(keys) != 0 {
		t.Errorf("Keys() after Clear() returned %d keys, want 0", len(keys))
	}
}

func TestConcurrentMapConcurrency(t *testing.T) {
	cm := NewConcurrentMap()

	// Number of goroutines
	numGoroutines := 10
	// Operations per goroutine
	opsPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Launch goroutines that perform concurrent operations
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Each goroutine performs a series of operations
			for j := 0; j < opsPerGoroutine; j++ {
				key := "key" + string(rune('A'+id)) + string(rune('0'+j%10))
				value := "value" + string(rune('A'+id)) + string(rune('0'+j%10))

				// Set
				cm.Set(key, value)

				// Get
				val, ok := cm.Get(key)
				if !ok || val != value {
					t.Errorf("Concurrent Get(%q) = %v, %v; want %v, true", key, val, ok, value)
				}

				// Has
				if !cm.Has(key) {
					t.Errorf("Concurrent Has(%q) = false, want true", key)
				}

				// For some keys, also test delete
				if j%3 == 0 {
					cm.Delete(key)
					if cm.Has(key) {
						t.Errorf("Concurrent Has(%q) after Delete = true, want false", key)
					}
				}
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
}

func TestFnvHash(t *testing.T) {
	// Test that the hash function returns consistent results
	key := "testKey"
	hash1 := fnvHash(key)
	hash2 := fnvHash(key)

	if hash1 != hash2 {
		t.Errorf("fnvHash(%q) returned inconsistent results: %d and %d", key, hash1, hash2)
	}

	// Test that different keys produce different hashes (not guaranteed, but likely)
	key2 := "anotherKey"
	hash3 := fnvHash(key2)

	if hash1 == hash3 {
		t.Logf("fnvHash produced the same hash for different keys: %q and %q", key, key2)
	}
}

func TestConcurrentMapGetShard(t *testing.T) {
	cm := NewConcurrentMap()

	// Test that getShard returns a non-nil shard
	key := "testKey"
	shard := cm.getShard(key)

	if shard == nil {
		t.Errorf("getShard(%q) returned nil", key)
	}

	// Test that the same key always maps to the same shard
	shard2 := cm.getShard(key)
	if shard != shard2 {
		t.Errorf("getShard(%q) returned inconsistent results", key)
	}
}
