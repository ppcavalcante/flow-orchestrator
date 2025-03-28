package benchmark

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
	"github.com/pparaujo/flow-orchestrator/internal/workflow/memory"
)

// BenchmarkNodeCreation compares the performance of creating nodes with and without pooling
func BenchmarkNodeCreation(b *testing.B) {
	b.Run("WithoutPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			node := workflow.NewNode("node", &testAction{})
			node.WithRetries(3)
			node.WithTimeout(time.Second)

			// Use the node to prevent compiler optimizations
			if node.Name == "" {
				b.Fatal("Node name should not be empty")
			}
		}
	})

	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			node := memory.CreateNode("node", &testAction{})
			node.RetryCount = 3
			node.Timeout = time.Second

			// Use the node to prevent compiler optimizations
			if node.Name == "" {
				b.Fatal("Node name should not be empty")
			}

			// Return node to pool
			memory.PutNode(node)
		}
	})
}

// BenchmarkNodeWithDependencies compares the performance of creating nodes with dependencies
func BenchmarkNodeWithDependencies(b *testing.B) {
	b.Run("WithoutPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// Create parent nodes
			parent1 := workflow.NewNode("parent1", &testAction{})
			parent2 := workflow.NewNode("parent2", &testAction{})

			// Create child node with dependencies
			child := workflow.NewNode("child", &testAction{})
			child.AddDependencies(parent1, parent2)

			// Use the nodes to prevent compiler optimizations
			if len(child.DependsOn) != 2 {
				b.Fatal("Child should have 2 dependencies")
			}
		}
	})

	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// Create parent nodes
			parent1 := memory.CreateNode("parent1", &testAction{})
			parent2 := memory.CreateNode("parent2", &testAction{})

			// Create child node with dependencies
			child := memory.CreateNodeWithDependencies("child", &testAction{}, parent1, parent2)

			// Use the nodes to prevent compiler optimizations
			if len(child.DependsOn) != 2 {
				b.Fatal("Child should have 2 dependencies")
			}

			// Return nodes to pool
			memory.PutNode(parent1)
			memory.PutNode(parent2)
			memory.PutNode(child)
		}
	})
}

// BenchmarkBufferUsage compares the performance of using buffers with and without pooling
func BenchmarkBufferUsage(b *testing.B) {
	data := []byte("Hello, World! This is a test of buffer pooling.")

	b.Run("WithoutPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, 0, 64)
			buf = append(buf, data...)

			// Use the buffer to prevent compiler optimizations
			if len(buf) != len(data) {
				b.Fatal("Buffer length mismatch")
			}
		}
	})

	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := memory.GetBuffer(64)
			*buf = append(*buf, data...)

			// Use the buffer to prevent compiler optimizations
			if len(*buf) != len(data) {
				b.Fatal("Buffer length mismatch")
			}

			// Return buffer to pool
			memory.PutBuffer(buf)
		}
	})
}

// BenchmarkStringInterning compares the performance of string interning with and without batch processing
func BenchmarkStringInterning(b *testing.B) {
	// Create test strings
	testStrings := []string{
		"node1", "node2", "node3", "node4", "node5",
		"pending", "running", "completed", "failed", "skipped",
		"workflow1", "workflow2", "workflow3", "workflow4", "workflow5",
	}

	b.Run("IndividualIntern", func(b *testing.B) {
		b.ReportAllocs()
		interner := workflow.NewStringInterner()

		for i := 0; i < b.N; i++ {
			for _, s := range testStrings {
				interned := interner.Intern(s)

				// Use the interned string to prevent compiler optimizations
				if interned != s {
					b.Fatal("Interned string should match original")
				}
			}
		}
	})

	b.Run("BatchIntern", func(b *testing.B) {
		b.ReportAllocs()
		interner := workflow.NewStringInterner()

		for i := 0; i < b.N; i++ {
			internedStrings := interner.InternBatch(testStrings)

			// Use the interned strings to prevent compiler optimizations
			if len(internedStrings) != len(testStrings) {
				b.Fatal("Interned strings count mismatch")
			}
		}
	})
}

// BenchmarkMemoryPooling tests memory allocation patterns
func BenchmarkMemoryPooling(b *testing.B) {
	// Test allocation of small objects
	b.Run("SmallObjects", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// Allocate small objects
			objs := make([][]byte, 100)
			for j := 0; j < 100; j++ {
				objs[j] = make([]byte, 64)
			}
			// Use the objects to prevent compiler optimizations
			for j := 0; j < 100; j++ {
				objs[j][0] = byte(j)
			}
		}
	})

	// Test allocation of large objects
	b.Run("LargeObjects", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// Allocate large objects
			objs := make([][]byte, 10)
			for j := 0; j < 10; j++ {
				objs[j] = make([]byte, 4096)
			}
			// Use the objects to prevent compiler optimizations
			for j := 0; j < 10; j++ {
				objs[j][0] = byte(j)
			}
		}
	})

	// Test object pooling
	b.Run("ObjectPooling", func(b *testing.B) {
		b.ReportAllocs()
		// Create a pool of objects
		pool := sync.Pool{
			New: func() interface{} {
				return make([]byte, 1024)
			},
		}

		for i := 0; i < b.N; i++ {
			// Get objects from pool
			objs := make([][]byte, 100)
			for j := 0; j < 100; j++ {
				objs[j] = pool.Get().([]byte)
			}
			// Use the objects
			for j := 0; j < 100; j++ {
				objs[j][0] = byte(j)
			}
			// Return objects to pool
			for j := 0; j < 100; j++ {
				pool.Put(objs[j])
			}
		}
	})
}

// BenchmarkConcurrentExecution compares the performance of executing tasks with and without concurrency
func BenchmarkConcurrentExecution(b *testing.B) {
	// Create test tasks
	taskCount := 100
	tasks := make([]func(), taskCount)
	for i := 0; i < taskCount; i++ {
		tasks[i] = func() {
			// Simulate some work
			time.Sleep(10 * time.Microsecond)
		}
	}

	b.Run("Sequential", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, task := range tasks {
				task()
			}
		}
	})

	b.Run("Concurrent", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			wg.Add(len(tasks))

			for _, task := range tasks {
				task := task // Capture for closure
				go func() {
					defer wg.Done()
					task()
				}()
			}

			wg.Wait()
		}
	})

	b.Run("LimitedConcurrency", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			wg.Add(len(tasks))

			// Create a semaphore to limit concurrency
			maxConcurrency := 4
			semaphore := make(chan struct{}, maxConcurrency)

			for _, task := range tasks {
				task := task // Capture for closure
				go func() {
					defer wg.Done()

					// Acquire semaphore
					semaphore <- struct{}{}
					defer func() { <-semaphore }()

					task()
				}()
			}

			wg.Wait()
		}
	})
}

// testAction is a simple action implementation for testing
type testAction struct{}

func (a *testAction) Execute(ctx context.Context, data *workflow.WorkflowData) error {
	return nil
}
