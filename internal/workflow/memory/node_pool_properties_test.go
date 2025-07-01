package memory

import (
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestNodePoolProperties contains property-based tests for the NodePool
func TestNodePoolProperties(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxShrinkCount = 10

	properties := gopter.NewProperties(parameters)

	// Property: Get returns a clean node
	properties.Property("get returns clean node", prop.ForAll(
		func(name string, retryCount int) bool {
			pool := NewNodePool(10)

			// Get a node from the pool
			node := pool.Get()

			// Node should be empty (zero values)
			if node.Name != "" || node.Action != nil || len(node.DependsOn) != 0 || node.RetryCount != 0 || node.Timeout != 0 {
				return false
			}

			// Set some values
			node.Name = name
			node.RetryCount = retryCount

			// Put back in pool
			pool.Put(node)

			// Get another node
			node2 := pool.Get()

			// Node should be clean (zero values)
			return node2.Name == "" && node2.Action == nil && len(node2.DependsOn) == 0 && node2.RetryCount == 0 && node2.Timeout == 0
		},
		gen.AlphaString(),
		gen.IntRange(0, 10),
	))

	// Property: Pool reuses nodes
	properties.Property("pool reuses nodes", prop.ForAll(
		func(count int) bool {
			if count <= 0 {
				return true
			}

			pool := NewNodePool(count)

			// Get initial stats
			initialGets, initialPuts, _, _ := pool.GetStats()

			// Get and put nodes multiple times
			for i := 0; i < count; i++ {
				node := pool.Get()
				node.Name = "node-" + string(rune('A'+i%26))
				pool.Put(node)
			}

			// Get final stats
			finalGets, finalPuts, _, _ := pool.GetStats()

			// We should have more gets and puts than initially
			return finalGets > initialGets && finalPuts > initialPuts
		},
		gen.IntRange(1, 100),
	))

	// Property: Concurrent usage is thread-safe
	properties.Property("concurrent usage is thread-safe", prop.ForAll(
		func(workerCount int, operationsPerWorker int) bool {
			if workerCount <= 0 || operationsPerWorker <= 0 {
				return true
			}

			pool := NewNodePool(workerCount * operationsPerWorker)

			var wg sync.WaitGroup

			// Perform concurrent operations
			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()

					for j := 0; j < operationsPerWorker; j++ {
						// Get a node
						node := pool.Get()

						// Set some values
						node.Name = "worker-" + string(rune('A'+workerID))
						node.RetryCount = j

						// Put back in pool
						pool.Put(node)
					}
				}(i)
			}

			wg.Wait()

			// Get stats
			gets, puts, _, _ := pool.GetStats()

			// We should have at least as many gets and puts as operations
			totalOps := workerCount * operationsPerWorker
			return gets >= totalOps && puts >= totalOps
		},
		gen.IntRange(2, 10),  // Number of workers
		gen.IntRange(1, 100), // Operations per worker
	))

	// Property: Node dependencies are properly reset
	properties.Property("node dependencies are properly reset", prop.ForAll(
		func(depCount int) bool {
			if depCount <= 0 {
				return true
			}

			pool := NewNodePool(depCount + 1)

			// Create a node with dependencies
			node := pool.Get()

			// Create dependency nodes
			deps := make([]*workflow.Node, depCount)
			for i := 0; i < depCount; i++ {
				deps[i] = pool.Get()
				deps[i].Name = "dep-" + string(rune('A'+i))
			}

			// Add dependencies to the node
			node.DependsOn = append(node.DependsOn, deps...)

			// Verify dependencies were added
			if len(node.DependsOn) != depCount {
				return false
			}

			// Put node back in pool
			pool.Put(node)

			// Get node again
			node2 := pool.Get()

			// Dependencies should be reset (empty slice)
			return len(node2.DependsOn) == 0
		},
		gen.IntRange(1, 10),
	))

	// Property: Global pool functions work correctly
	properties.Property("global pool functions work correctly", prop.ForAll(
		func(name string) bool {
			// Get a node from the global pool
			node := GetNode()

			// Node should be clean
			if node.Name != "" || node.Action != nil || len(node.DependsOn) != 0 || node.RetryCount != 0 || node.Timeout != 0 {
				return false
			}

			// Set a name
			node.Name = name

			// Put back in pool
			PutNode(node)

			// Get another node
			node2 := GetNode()

			// Node should be clean
			return node2.Name == "" && node2.Action == nil && len(node2.DependsOn) == 0 && node2.RetryCount == 0 && node2.Timeout == 0
		},
		gen.AlphaString(),
	))

	// Run the properties
	properties.TestingRun(t)
}
