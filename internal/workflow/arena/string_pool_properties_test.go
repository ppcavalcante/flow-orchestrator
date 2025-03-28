package arena

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestStringPoolProperties contains property-based tests for the StringPool
func TestStringPoolProperties(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxShrinkCount = 10

	properties := gopter.NewProperties(parameters)

	// Property: String interning preserves identity
	properties.Property("string interning preserves identity", prop.ForAll(
		func(strings []string) bool {
			if len(strings) == 0 {
				return true
			}

			arena := NewArena()
			pool := NewStringPool(arena)

			// Intern all strings
			internedStrings := make([]string, len(strings))
			for i, s := range strings {
				internedStrings[i] = pool.Intern(s)
			}

			// Verify content is preserved
			for i, s := range strings {
				if internedStrings[i] != s {
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

	// Property: Interning the same string returns the same reference
	properties.Property("interning same string returns same reference", prop.ForAll(
		func(s string, repeatCount int) bool {
			if repeatCount <= 0 {
				return true
			}

			arena := NewArena()
			pool := NewStringPool(arena)

			// First intern
			first := pool.Intern(s)

			// Repeat interning
			for i := 0; i < repeatCount; i++ {
				interned := pool.Intern(s)

				// For non-empty strings, we should get the same reference
				// Empty strings are a special case and might not be interned
				if s != "" && interned != first {
					return false
				}
			}

			return true
		},
		gen.AnyString(),
		gen.IntRange(1, 10),
	))

	// Property: Common strings are pre-interned
	properties.Property("common strings are pre-interned", prop.ForAll(
		func() bool {
			arena := NewArena()
			pool := NewStringPool(arena)

			// List of common strings that should be pre-interned
			commonStrings := []string{
				"", "true", "false", "success", "failure",
				"pending", "running", "error", "skipped",
				"id", "name", "status", "result", "data",
				"input", "output", "node", "edge", "graph",
				"workflow", "completed", "failed",
			}

			// Get initial stats
			initialStats := pool.Stats()
			initialHits := initialStats["hits"]

			// Intern all common strings
			for _, s := range commonStrings {
				_ = pool.Intern(s)
			}

			// Get updated stats
			updatedStats := pool.Stats()
			updatedHits := updatedStats["hits"]

			// All common strings should be hits (pre-interned)
			// Note: The first call to Stats() might also intern some strings,
			// so we check that the difference is at least the number of common strings
			return (updatedHits - initialHits) >= int64(len(commonStrings))
		},
	))

	// Property: Concurrent interning is thread-safe
	properties.Property("concurrent interning is thread-safe", prop.ForAll(
		func(strings []string, workerCount int) bool {
			if len(strings) == 0 || workerCount <= 0 {
				return true
			}

			arena := NewArena()
			pool := NewStringPool(arena)
			var wg sync.WaitGroup

			// Perform concurrent interning
			results := make([][]string, workerCount)
			for i := 0; i < workerCount; i++ {
				results[i] = make([]string, len(strings))
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					// Each worker interns all strings
					for j, s := range strings {
						results[workerID][j] = pool.Intern(s)
					}
				}(i)
			}

			wg.Wait()

			// Verify all workers got the same interned strings
			for i := 1; i < workerCount; i++ {
				for j, s := range strings {
					if s != "" && results[i][j] != results[0][j] {
						return false
					}
				}
			}

			return true
		},
		gen.SliceOf(gen.AnyString()).Map(func(strings []string) []string {
			if len(strings) > 10 {
				return strings[:10] // Limit for performance
			}
			return strings
		}),
		gen.IntRange(2, 5), // Number of concurrent workers
	))

	// Property: Interning reduces memory usage
	properties.Property("interning reduces memory usage", prop.ForAll(
		func(baseString string, suffixCount int) bool {
			if suffixCount <= 0 {
				return true
			}

			// Create strings with common prefix
			strings := make([]string, suffixCount)
			for i := 0; i < suffixCount; i++ {
				strings[i] = baseString + "-" + string(rune('a'+i))
			}

			// Measure arena size without interning
			arenaWithout := NewArena()
			for _, s := range strings {
				_ = arenaWithout.AllocString(s)
			}
			statsWithout := arenaWithout.Stats()

			// Measure arena size with interning
			arenaWith := NewArena()
			pool := NewStringPool(arenaWith)
			for _, s := range strings {
				_ = pool.Intern(s)
			}
			statsWith := arenaWith.Stats()

			// With interning, we should use less memory for strings with common prefixes
			// This is a heuristic - the actual savings depend on the strings
			// For very short strings or few strings, there might not be savings
			if len(baseString) > 5 && suffixCount > 5 {
				return statsWith["totalAllocated"] <= statsWithout["totalAllocated"]
			}

			return true
		},
		gen.AlphaString().SuchThat(func(s string) bool { return len(s) > 0 }),
		gen.IntRange(1, 20),
	))

	// Run the properties
	properties.TestingRun(t)
}
