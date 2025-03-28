package concurrent

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestReadMapProperties contains property-based tests for the ReadMap
func TestReadMapProperties(t *testing.T) {
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

			rm := NewReadMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				rm.Set(k, v)
			}

			// Verify all keys exist and have correct values
			for k, v := range keyValues {
				val, exists := rm.Get(k)
				if !exists {
					return false
				}

				// Check value
				if val != v {
					return false
				}

				// Check Has method
				if !rm.Has(k) {
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

			rm := NewReadMap()

			// Set all key-value pairs
			for k, v := range keyValues {
				rm.Set(k, v)
			}

			// Delete specified keys
			for _, k := range deleteKeys {
				if _, exists := keyValues[k]; exists {
					rm.Delete(k)
				}
			}

			// Verify deleted keys don't exist
			for _, k := range deleteKeys {
				if _, exists := keyValues[k]; exists {
					if rm.Has(k) {
						return false
					}
				}
			}

			// Verify non-deleted keys still exist
			for k, v := range keyValues {
				isDeleted := false
				for _, dk := range deleteKeys {
					if k == dk {
						isDeleted = true
						break
					}
				}

				if !isDeleted {
					val, exists := rm.Get(k)
					if !exists || val != v {
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

	// Property: Concurrent operations are thread-safe
	properties.Property("concurrent operations are thread-safe", prop.ForAll(
		func(keyValues map[string]int, workerCount int) bool {
			if len(keyValues) == 0 || workerCount <= 0 {
				return true
			}

			rm := NewReadMap()
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
				rm.Set(k, v)
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
							rm.Set(k, v)
						case 1: // Get
							_, _ = rm.Get(k)
						case 2: // Has
							_ = rm.Has(k)
						}
					}
				}(i)
			}

			wg.Wait()

			// Verify final state
			// All keys should exist with their original values
			for j, k := range keys {
				v := values[j]

				val, exists := rm.Get(k)
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

	// Property: High contention operations don't cause issues
	properties.Property("high contention operations", prop.ForAll(
		func(keyCount int, operationsPerKey int, workerCount int) bool {
			if keyCount <= 0 || operationsPerKey <= 0 || workerCount <= 0 {
				return true
			}

			// Limit parameters for performance
			if keyCount > 10 {
				keyCount = 10
			}
			if operationsPerKey > 100 {
				operationsPerKey = 100
			}
			if workerCount > 8 {
				workerCount = 8
			}

			rm := NewReadMap()
			var wg sync.WaitGroup

			// Generate keys
			keys := make([]string, keyCount)
			for i := 0; i < keyCount; i++ {
				keys[i] = "key-" + string(rune('A'+i))
			}

			// Perform concurrent operations with high contention
			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					for j := 0; j < operationsPerKey; j++ {
						for k, key := range keys {
							// All workers operate on the same keys
							value := workerID*1000 + j*10 + k

							// Mix of operations
							opType := (workerID + j + k) % 3

							switch opType {
							case 0: // Set
								rm.Set(key, value)
							case 1: // Get
								_, _ = rm.Get(key)
							case 2: // Set after Get
								_, _ = rm.Get(key)
								rm.Set(key, value)
							}
						}
					}
				}(i)
			}

			wg.Wait()

			// Verify all keys exist
			for _, key := range keys {
				if !rm.Has(key) {
					return false
				}
			}

			return true
		},
		gen.IntRange(1, 10),  // keyCount
		gen.IntRange(1, 100), // operationsPerKey
		gen.IntRange(2, 8),   // workerCount
	))

	// Property: Compare with ConcurrentMap for correctness
	properties.Property("compare with concurrent map", prop.ForAll(
		func(keyValues map[string]int, operations []int) bool {
			if len(keyValues) == 0 || len(operations) == 0 {
				return true
			}

			rm := NewReadMap()
			cm := NewConcurrentMap()

			// Convert map to slices for easier access
			keys := make([]string, 0, len(keyValues))
			values := make([]int, 0, len(keyValues))

			for k, v := range keyValues {
				keys = append(keys, k)
				values = append(values, v)
			}

			// Perform the same operations on both maps
			for _, op := range operations {
				if len(keys) == 0 {
					continue
				}

				// Select a key based on the operation value
				keyIndex := op % len(keys)
				if keyIndex < 0 {
					keyIndex = -keyIndex
				}

				key := keys[keyIndex]
				value := values[keyIndex]

				// Determine operation type
				opType := op % 3

				switch opType {
				case 0: // Set
					rm.Set(key, value)
					cm.Set(key, value)
				case 1: // Get
					rmVal, rmExists := rm.Get(key)
					cmVal, cmExists := cm.Get(key)

					// Both maps should return the same result
					if rmExists != cmExists {
						return false
					}

					if rmExists && cmExists && rmVal != cmVal {
						return false
					}
				case 2: // Delete
					rm.Delete(key)
					cm.Delete(key)
				}
			}

			// Verify both maps have the same state
			for _, key := range keys {
				rmVal, rmExists := rm.Get(key)
				cmVal, cmExists := cm.Get(key)

				if rmExists != cmExists {
					return false
				}

				if rmExists && cmExists && rmVal != cmVal {
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
		gen.SliceOf(gen.Int()).Map(func(ops []int) []int {
			if len(ops) > 50 {
				return ops[:50] // Limit for performance
			}
			return ops
		}),
	))

	// Run the properties
	properties.TestingRun(t)
}
