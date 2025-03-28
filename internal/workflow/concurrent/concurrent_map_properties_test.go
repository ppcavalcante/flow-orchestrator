package concurrent

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestConcurrentMapProperties contains property-based tests for the ConcurrentMap
func TestConcurrentMapProperties(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxShrinkCount = 10

	properties := gopter.NewProperties(parameters)

	// Property: Basic operations work correctly
	properties.Property("basic operations work correctly", prop.ForAll(
		func(keyValues map[string]int) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Verify count
			if cm.Count() != len(keyValues) {
				return false
			}

			// Verify all keys exist and have correct values
			for k, v := range keyValues {
				val, exists := cm.Get(k)
				if !exists {
					return false
				}

				// Check value
				if val != v {
					return false
				}

				// Check Has method
				if !cm.Has(k) {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			// Limit map size for performance
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
	))

	// Property: Delete removes items correctly
	properties.Property("delete removes items correctly", prop.ForAll(
		func(keyValues map[string]int, deleteKeys []string) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Delete specified keys
			keysDeleted := 0
			for _, k := range deleteKeys {
				if _, exists := keyValues[k]; exists {
					cm.Delete(k)
					keysDeleted++
				}
			}

			// Verify count
			expectedCount := len(keyValues) - keysDeleted
			if cm.Count() != expectedCount {
				return false
			}

			// Verify deleted keys don't exist
			for _, k := range deleteKeys {
				if _, exists := keyValues[k]; exists {
					if cm.Has(k) {
						return false
					}
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
		gen.SliceOf(gen.AlphaString()).Map(func(keys []string) []string {
			if len(keys) > 10 {
				return keys[:10] // Limit for performance
			}
			return keys
		}),
	))

	// Property: Keys returns all keys
	properties.Property("keys returns all keys", prop.ForAll(
		func(keyValues map[string]int) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Get keys
			keys := cm.Keys()

			// Verify all keys are returned
			if len(keys) != len(keyValues) {
				return false
			}

			// Check each key exists in the original map
			for _, k := range keys {
				if _, exists := keyValues[k]; !exists {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
	))

	// Property: Items returns all items
	properties.Property("items returns all items", prop.ForAll(
		func(keyValues map[string]int) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Get items
			items := cm.Items()

			// Verify all items are returned
			if len(items) != len(keyValues) {
				return false
			}

			// Check each item has the correct value
			for k, v := range keyValues {
				itemVal, exists := items[k]
				if !exists {
					return false
				}

				if itemVal != v {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
	))

	// Property: Clear removes all items
	properties.Property("clear removes all items", prop.ForAll(
		func(keyValues map[string]int) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Clear the map
			cm.Clear()

			// Verify count is 0
			if cm.Count() != 0 {
				return false
			}

			// Verify no keys exist
			for k := range keyValues {
				if cm.Has(k) {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
	))

	// Property: Concurrent operations are thread-safe
	properties.Property("concurrent operations are thread-safe", prop.ForAll(
		func(keyValues map[string]int, workerCount int) bool {
			if len(keyValues) == 0 || workerCount <= 0 {
				return true
			}

			cm := NewConcurrentMap()
			var wg sync.WaitGroup

			// Convert map to slices for easier concurrent access
			keys := make([]string, 0, len(keyValues))
			values := make([]int, 0, len(keyValues))

			for k, v := range keyValues {
				keys = append(keys, k)
				values = append(values, v)
			}

			// First set all values to ensure they exist
			for j, k := range keys {
				v := values[j]
				cm.Set(k, v)
			}

			// Perform concurrent operations
			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					// Each worker performs operations on all keys
					for j, k := range keys {
						v := values[j]

						// Determine operation based on worker ID and key index
						opType := (workerID + j) % 3

						switch opType {
						case 0: // Set
							cm.Set(k, v)
						case 1: // Get
							_, _ = cm.Get(k)
						case 2: // Has
							_ = cm.Has(k)
						}
					}
				}(i)
			}

			wg.Wait()

			// Verify final state
			// All keys should exist with their original values
			for j, k := range keys {
				v := values[j]

				val, exists := cm.Get(k)
				if !exists {
					return false
				}

				if val != v {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 10 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 10 {
						break
					}
				}
				return result
			}
			return m
		}),
		gen.IntRange(2, 8), // Number of concurrent workers
	))

	// Property: ForEach iterates over all items
	properties.Property("forEach iterates over all items", prop.ForAll(
		func(keyValues map[string]int) bool {
			if len(keyValues) == 0 {
				return true
			}

			cm := NewConcurrentMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				cm.Set(k, v)
			}

			// Use ForEach to collect items
			collected := make(map[string]int)
			cm.ForEach(func(k string, v interface{}) {
				collected[k] = v.(int)
			})

			// Verify all items were collected
			if len(collected) != len(keyValues) {
				return false
			}

			// Check each item has the correct value
			for k, v := range keyValues {
				collectedVal, exists := collected[k]
				if !exists {
					return false
				}

				if collectedVal != v {
					return false
				}
			}

			return true
		},
		gen.MapOf(
			gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
			gen.Int(),
		).Map(func(m map[string]int) map[string]int {
			if len(m) > 20 {
				result := make(map[string]int)
				count := 0
				for k, v := range m {
					result[k] = v
					count++
					if count >= 20 {
						break
					}
				}
				return result
			}
			return m
		}),
	))

	// Run the properties
	properties.TestingRun(t)
}
