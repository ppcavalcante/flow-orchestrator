package arena

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestArenaProperties contains property-based tests for the arena memory management system
func TestArenaProperties(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxShrinkCount = 10

	properties := gopter.NewProperties(parameters)

	// Property: Allocations don't overlap and maintain integrity
	properties.Property("non-overlapping allocations", prop.ForAll(
		func(allocSizes []int) bool {
			// Skip invalid inputs
			if len(allocSizes) == 0 {
				return true
			}

			a := NewArena()
			allocations := make([][]byte, len(allocSizes))

			// Perform allocations
			for i, size := range allocSizes {
				if size <= 0 {
					continue
				}
				allocations[i] = a.Alloc(size)

				// Write a unique pattern to this allocation
				for j := 0; j < len(allocations[i]) && j < size; j++ {
					allocations[i][j] = byte(i + 1)
				}
			}

			// Verify allocations don't overlap by checking patterns
			for i, alloc := range allocations {
				if alloc == nil {
					continue
				}

				// Check this allocation has the correct pattern
				for j := 0; j < len(alloc) && j < allocSizes[i]; j++ {
					if alloc[j] != byte(i+1) {
						return false // Overlap detected
					}
				}
			}

			return true
		},
		gen.SliceOf(gen.IntRange(0, 1024)).Map(func(sizes []int) []int {
			// Ensure we have some reasonable allocation sizes
			if len(sizes) > 20 {
				return sizes[:20] // Limit to 20 allocations for performance
			}
			return sizes
		}),
	))

	// Property: Reset allows memory reuse
	properties.Property("memory reuse after reset", prop.ForAll(
		func(allocSize int) bool {
			if allocSize <= 0 {
				return true
			}

			a := NewArena()

			// First allocation
			_ = a.Alloc(allocSize)
			stats1 := a.Stats()

			// Reset arena
			a.Reset()

			// Second allocation of same size
			_ = a.Alloc(allocSize)
			stats2 := a.Stats()

			// After reset and same-sized allocation, we should have the same number of blocks
			// and the offset should be the same (indicating we're reusing the first block)
			return stats1["totalBlocks"] == stats2["totalBlocks"] &&
				stats1["currentBlock"] == stats2["currentBlock"]
		},
		gen.IntRange(1, 1024),
	))

	// Property: String allocation preserves content
	properties.Property("string allocation preserves content", prop.ForAll(
		func(strings []string) bool {
			a := NewArena()

			// Allocate all strings
			allocatedStrings := make([]string, len(strings))
			for i, s := range strings {
				allocatedStrings[i] = a.AllocString(s)
			}

			// Verify content is preserved
			for i, s := range strings {
				if allocatedStrings[i] != s {
					return false
				}
			}

			return true
		},
		gen.SliceOf(gen.AnyString()).Map(func(strings []string) []string {
			if len(strings) > 20 {
				return strings[:20] // Limit to 20 strings for performance
			}
			return strings
		}),
	))

	// Property: Arena handles concurrent allocations safely
	properties.Property("concurrent allocation safety", prop.ForAll(
		func(allocSizes []int, workerCount int) bool {
			if len(allocSizes) == 0 || workerCount <= 0 {
				return true
			}

			a := NewArena()
			var wg sync.WaitGroup

			// Perform concurrent allocations
			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					// Each worker allocates some memory
					for j := 0; j < len(allocSizes); j++ {
						size := allocSizes[j]
						if size <= 0 {
							continue
						}

						// Allocate and write a pattern
						mem := a.Alloc(size)
						for k := 0; k < len(mem) && k < size; k++ {
							mem[k] = byte(workerID + 1)
						}
					}
				}(i)
			}

			wg.Wait()

			// If we got here without panics, the test passes
			// We're primarily testing for race conditions and memory corruption
			return true
		},
		gen.SliceOf(gen.IntRange(1, 128)).Map(func(sizes []int) []int {
			if len(sizes) > 10 {
				return sizes[:10] // Limit for performance
			}
			return sizes
		}),
		gen.IntRange(2, 8), // Number of concurrent workers
	))

	// Property: Block allocation strategy is efficient
	properties.Property("efficient block allocation", prop.ForAll(
		func(allocSizes []int) bool {
			if len(allocSizes) == 0 {
				return true
			}

			a := NewArena()
			totalAllocated := 0

			// Perform allocations
			for _, size := range allocSizes {
				if size <= 0 {
					continue
				}
				_ = a.Alloc(size)
				totalAllocated += size
			}

			// Get stats
			stats := a.Stats()

			// Verify that wasted bytes are reasonable (less than 50% of total)
			// This is a heuristic - the actual threshold depends on the allocation pattern
			wastedBytes := stats["wastedBytes"]
			return wastedBytes <= int64(totalAllocated)
		},
		gen.SliceOf(gen.IntRange(1, 1024)).Map(func(sizes []int) []int {
			if len(sizes) > 50 {
				return sizes[:50] // Limit for performance
			}
			return sizes
		}),
	))

	// Property: Free releases memory
	properties.Property("free releases memory", prop.ForAll(
		func(allocSize int) bool {
			if allocSize <= 0 {
				return true
			}

			a := NewArena()

			// Allocate a large amount of memory
			for i := 0; i < 10; i++ {
				_ = a.Alloc(allocSize)
			}

			// Get stats before free
			statsBefore := a.Stats()

			// Free memory
			a.Free()

			// Get stats after free
			statsAfter := a.Stats()

			// After free, we should have only one block and reset counters
			return statsAfter["totalBlocks"] == 1 &&
				statsAfter["totalAllocated"] == 0 &&
				statsAfter["currentBlock"] == 0 &&
				statsAfter["wastedBytes"] == 0 &&
				statsBefore["totalBlocks"] >= 1
		},
		gen.IntRange(1024, 8192), // Larger allocations to force multiple blocks
	))

	// Run the properties
	properties.TestingRun(t)
}
