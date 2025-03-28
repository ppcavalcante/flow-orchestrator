package memory

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// TestBufferPoolProperties contains property-based tests for the BufferPool
func TestBufferPoolProperties(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxShrinkCount = 10

	properties := gopter.NewProperties(parameters)

	// Property: Buffers from pool have at least the requested capacity
	properties.Property("buffer capacity meets request", prop.ForAll(
		func(size int) bool {
			if size <= 0 {
				return true
			}

			pool := NewBufferPool()
			buf := pool.Get(size)
			defer pool.Put(buf)

			// Buffer should have at least the requested capacity
			if cap(*buf) < size {
				return false
			}

			// Buffer should be empty (length 0)
			if len(*buf) != 0 {
				return false
			}

			return true
		},
		gen.IntRange(0, 32768),
	))

	// Property: Reusing buffers works correctly
	properties.Property("buffer reuse works correctly", prop.ForAll(
		func(size int, reuseCount int) bool {
			if size <= 0 || reuseCount <= 0 {
				return true
			}

			pool := NewBufferPool()

			// Get initial stats
			initialStats := pool.GetStats()

			// Get and put buffer multiple times
			for i := 0; i < reuseCount; i++ {
				buf := pool.Get(size)

				// Write to buffer to ensure it works
				*buf = append(*buf, byte(i))

				// Return to pool
				pool.Put(buf)
			}

			// Get final stats
			finalStats := pool.GetStats()

			// We should have more gets than the initial count
			return finalStats["gets"] > initialStats["gets"] &&
				finalStats["puts"] > initialStats["puts"]
		},
		gen.IntRange(1, 1024),
		gen.IntRange(1, 10),
	))

	// Property: Concurrent buffer usage is thread-safe
	properties.Property("concurrent buffer usage is thread-safe", prop.ForAll(
		func(size int, workerCount int) bool {
			if size <= 0 || workerCount <= 0 {
				return true
			}

			pool := NewBufferPool()
			var wg sync.WaitGroup

			// Perform concurrent buffer operations
			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					// Each worker gets and puts a buffer
					buf := pool.Get(size)

					// Write to buffer
					*buf = append(*buf, byte(workerID))

					// Return buffer to pool
					pool.Put(buf)
				}(i)
			}

			wg.Wait()

			// If we got here without panics, the test passes
			// We're primarily testing for race conditions
			return true
		},
		gen.IntRange(1, 1024),
		gen.IntRange(2, 8), // Number of concurrent workers
	))

	// Property: WithBuffer function works correctly
	properties.Property("WithBuffer function works correctly", prop.ForAll(
		func(size int, value byte) bool {
			if size <= 0 {
				return true
			}

			// Use WithBuffer to perform an operation
			var result byte
			err := WithBuffer(size, func(buf *[]byte) error {
				// Buffer should have at least the requested capacity
				if cap(*buf) < size {
					return nil // Skip test if capacity is insufficient
				}

				// Write to buffer
				*buf = append(*buf, value)

				// Read from buffer
				result = (*buf)[0]

				return nil
			})

			// Operation should succeed and value should be preserved
			return err == nil && result == value
		},
		gen.IntRange(1, 1024),
		gen.UInt8(),
	))

	// Run the properties
	properties.TestingRun(t)
}
