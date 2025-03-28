package concurrent

import (
	"sync"
	"testing"
)

func TestNewReadMap(t *testing.T) {
	rm := NewReadMap()

	if rm == nil {
		t.Fatal("NewReadMap() returned nil")
	}

	if rm.value.Load() == nil {
		t.Error("ReadMap.value should not be nil")
	}

	// Check that the internal map is initialized
	if rm.value.Load().data == nil {
		t.Error("ReadMap.value.data should not be nil")
	}
}

func TestNewReadMapWithCapacity(t *testing.T) {
	capacity := 32
	rm := NewReadMapWithCapacity(capacity)

	if rm == nil {
		t.Fatal("NewReadMapWithCapacity() returned nil")
	}

	if rm.value.Load() == nil {
		t.Error("ReadMap.value should not be nil")
	}

	// Check that the internal map is initialized
	if rm.value.Load().data == nil {
		t.Error("ReadMap.value.data should not be nil")
	}
}

func TestReadMapSetGet(t *testing.T) {
	rm := NewReadMap()

	// Test Set and Get
	key := "testKey"
	value := "testValue"

	rm.Set(key, value)

	// Test Get with existing key
	retrievedValue, ok := rm.Get(key)
	if !ok {
		t.Errorf("Get(%q) returned ok=false, want true", key)
	}

	if retrievedValue != value {
		t.Errorf("Get(%q) = %v, want %v", key, retrievedValue, value)
	}

	// Test Get with non-existent key
	_, ok = rm.Get("nonExistentKey")
	if ok {
		t.Error("Get(nonExistentKey) returned ok=true, want false")
	}
}

func TestReadMapHas(t *testing.T) {
	rm := NewReadMap()

	key := "testKey"
	value := "testValue"

	// Test Has with non-existent key
	if rm.Has(key) {
		t.Errorf("Has(%q) = true, want false for non-existent key", key)
	}

	// Add the key and test again
	rm.Set(key, value)

	if !rm.Has(key) {
		t.Errorf("Has(%q) = false, want true for existing key", key)
	}
}

func TestReadMapDelete(t *testing.T) {
	rm := NewReadMap()

	key := "testKey"
	value := "testValue"

	// Add a key
	rm.Set(key, value)

	// Verify it exists
	if !rm.Has(key) {
		t.Fatalf("Key %q should exist after Set", key)
	}

	// Delete the key
	rm.Delete(key)

	// Verify it no longer exists
	if rm.Has(key) {
		t.Errorf("Key %q should not exist after Delete", key)
	}
}

func TestReadMapCount(t *testing.T) {
	rm := NewReadMap()

	// Initial count should be 0
	if count := rm.Count(); count != 0 {
		t.Errorf("Initial Count() = %d, want 0", count)
	}

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Count should be 3
	if count := rm.Count(); count != 3 {
		t.Errorf("Count() after adding 3 items = %d, want 3", count)
	}

	// Delete an item
	rm.Delete("key2")

	// Count should be 2
	if count := rm.Count(); count != 2 {
		t.Errorf("Count() after deleting 1 item = %d, want 2", count)
	}
}

func TestReadMapKeys(t *testing.T) {
	rm := NewReadMap()

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Get the keys
	keys := rm.Keys()

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

func TestReadMapItems(t *testing.T) {
	rm := NewReadMap()

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Get all items
	items := rm.Items()

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

func TestReadMapForEach(t *testing.T) {
	rm := NewReadMap()

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Use ForEach to collect items
	items := make(map[string]interface{})
	rm.ForEach(func(key string, value interface{}) {
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

func TestReadMapClear(t *testing.T) {
	rm := NewReadMap()

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Verify items were added
	if count := rm.Count(); count != 3 {
		t.Fatalf("Count() after adding 3 items = %d, want 3", count)
	}

	// Clear the map
	rm.Clear()

	// Verify the map is empty
	if count := rm.Count(); count != 0 {
		t.Errorf("Count() after Clear() = %d, want 0", count)
	}

	// Verify no keys remain
	if keys := rm.Keys(); len(keys) != 0 {
		t.Errorf("Keys() after Clear() returned %d keys, want 0", len(keys))
	}
}

func TestReadMapSnapshot(t *testing.T) {
	rm := NewReadMap()

	// Add some items
	rm.Set("key1", "value1")
	rm.Set("key2", "value2")
	rm.Set("key3", "value3")

	// Take a snapshot
	snapshot := rm.Snapshot()

	// Check that the snapshot has the expected items
	if len(snapshot) != 3 {
		t.Errorf("Snapshot() returned %d items, want 3", len(snapshot))
	}

	// Check that all expected items are present in the snapshot
	expectedItems := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	for key, value := range snapshot {
		expectedValue, ok := expectedItems[key]
		if !ok {
			t.Errorf("Snapshot() returned unexpected key: %q", key)
			continue
		}

		if value != expectedValue {
			t.Errorf("Snapshot()[%q] = %v, want %v", key, value, expectedValue)
		}

		// Mark this item as found
		delete(expectedItems, key)
	}

	// Check that all expected items were found
	if len(expectedItems) != 0 {
		t.Errorf("Snapshot() did not return all expected items. Missing: %v", expectedItems)
	}

	// Modify the original map
	rm.Set("key4", "value4")
	rm.Delete("key1")

	// Check that the snapshot is unchanged
	if len(snapshot) != 3 {
		t.Errorf("Snapshot should still have 3 items after modifying the original map, got %d", len(snapshot))
	}

	if _, ok := snapshot["key1"]; !ok {
		t.Error("Snapshot should still contain key1 after it was deleted from the original map")
	}

	if _, ok := snapshot["key4"]; ok {
		t.Error("Snapshot should not contain key4 which was added to the original map after the snapshot was taken")
	}
}

func TestReadMapConcurrency(t *testing.T) {
	rm := NewReadMap()

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
				rm.Set(key, value)

				// Get
				val, ok := rm.Get(key)
				if !ok || val != value {
					t.Errorf("Concurrent Get(%q) = %v, %v; want %v, true", key, val, ok, value)
				}

				// Has
				if !rm.Has(key) {
					t.Errorf("Concurrent Has(%q) = false, want true", key)
				}

				// For some keys, also test delete
				if j%3 == 0 {
					rm.Delete(key)
					if rm.Has(key) {
						t.Errorf("Concurrent Has(%q) after Delete = true, want false", key)
					}
				}
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
}
