package benchmark

import (
	"fmt"
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
	"github.com/pparaujo/flow-orchestrator/internal/workflow/arena"
)

// BenchmarkArenaAllocation benchmarks memory allocation with and without arenas
func BenchmarkArenaAllocation(b *testing.B) {
	// Test different allocation sizes
	sizes := []int{8, 64, 256, 1024, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			// Benchmark standard allocation
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					mem := make([]byte, size)
					// Use the memory to prevent compiler optimizations
					mem[0] = byte(i)
				}
			})

			// Benchmark arena allocation
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()
				a := arena.NewArena()

				for i := 0; i < b.N; i++ {
					mem := a.Alloc(size)
					// Use the memory to prevent compiler optimizations
					mem[0] = byte(i)
				}
			})
		})
	}
}

// BenchmarkArenaStringAllocation benchmarks string allocation with and without arenas
func BenchmarkArenaStringAllocation(b *testing.B) {
	// Test different string lengths
	strings := []string{
		"",
		"short",
		"medium length string",
		"this is a longer string that should span multiple cache lines",
		"this is a very long string that should definitely span multiple cache lines and might require special handling in the arena implementation",
	}

	for i, s := range strings {
		name := fmt.Sprintf("Length_%d", len(s))
		if i == 0 {
			name = "Empty"
		}

		b.Run(name, func(b *testing.B) {
			// Benchmark standard string allocation
			b.Run("Standard", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					newStr := string([]byte(s))
					// Use the string to prevent compiler optimizations
					if len(newStr) != len(s) {
						b.Fatal("String length mismatch")
					}
				}
			})

			// Benchmark arena string allocation
			b.Run("Arena", func(b *testing.B) {
				b.ReportAllocs()
				a := arena.NewArena()

				for i := 0; i < b.N; i++ {
					newStr := a.AllocString(s)
					// Use the string to prevent compiler optimizations
					if len(newStr) != len(s) {
						b.Fatal("String length mismatch")
					}
				}
			})
		})
	}
}

// BenchmarkStringPoolIntern benchmarks string interning with different implementations
func BenchmarkStringPoolIntern(b *testing.B) {
	// Create test strings with a mix of common and unique strings
	commonStrings := []string{
		"node", "status", "pending", "running", "completed", "failed", "skipped",
		"workflow", "data", "input", "output", "error", "result", "id", "name",
	}

	// Generate test strings: mix of common strings and unique strings
	testStrings := make([]string, 0, 100)
	for i := 0; i < 50; i++ {
		// Add common strings
		testStrings = append(testStrings, commonStrings[i%len(commonStrings)])
		// Add unique strings
		testStrings = append(testStrings, fmt.Sprintf("unique_string_%d", i))
	}

	b.Run("GlobalStringInterner", func(b *testing.B) {
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := workflow.InternString(s)
				// Use the interned string to prevent compiler optimizations
				if len(interned) != len(s) {
					b.Fatal("String length mismatch")
				}
			}
		}
	})

	b.Run("ArenaStringPool", func(b *testing.B) {
		b.ReportAllocs()
		a := arena.NewArena()
		pool := arena.NewStringPool(a)

		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := pool.Intern(s)
				// Use the interned string to prevent compiler optimizations
				if len(interned) != len(s) {
					b.Fatal("String length mismatch")
				}
			}
		}
	})

	b.Run("GlobalStringInternerBatch", func(b *testing.B) {
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			internedStrings := workflow.InternStringBatch(testStrings)
			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count should match original")
			}
		}
	})

	b.Run("ArenaStringPoolBatch", func(b *testing.B) {
		b.ReportAllocs()
		a := arena.NewArena()
		pool := arena.NewStringPool(a)

		for i := 0; i < b.N; i++ {
			internedStrings := pool.InternBatch(testStrings)
			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count should match original")
			}
		}
	})
}

// BenchmarkArenaReset benchmarks arena reset vs. reallocation
func BenchmarkArenaReset(b *testing.B) {
	// Test different allocation counts
	counts := []int{10, 100, 1000}

	for _, count := range counts {
		b.Run(fmt.Sprintf("Count_%d", count), func(b *testing.B) {
			// Benchmark creating a new arena for each iteration
			b.Run("NewArena", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					a := arena.NewArena()

					// Allocate memory
					for j := 0; j < count; j++ {
						mem := a.Alloc(64)
						// Use the memory to prevent compiler optimizations
						mem[0] = byte(j)
					}
				}
			})

			// Benchmark reusing the same arena with reset
			b.Run("ResetArena", func(b *testing.B) {
				b.ReportAllocs()
				a := arena.NewArena()

				for i := 0; i < b.N; i++ {
					// Reset the arena
					a.Reset()

					// Allocate memory
					for j := 0; j < count; j++ {
						mem := a.Alloc(64)
						// Use the memory to prevent compiler optimizations
						mem[0] = byte(j)
					}
				}
			})
		})
	}
}
