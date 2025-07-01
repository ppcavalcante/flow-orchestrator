package benchmark

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestDeadlockDetection runs a series of tests with timeouts to identify where deadlocks might occur
func TestDeadlockDetection(t *testing.T) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create data
	data := workflow.NewWorkflowData("test")

	// Run tests in sequence with deadlock detection
	testFuncs := []struct {
		name string
		fn   func()
	}{
		{"Set", func() { data.Set("key1", "value1") }},
		{"Get", func() { data.Get("key1") }},
		{"SetNodeStatus", func() { data.SetNodeStatus("node1", workflow.Running) }},
		{"GetNodeStatus", func() { data.GetNodeStatus("node1") }},
		{"SetOutput", func() { data.SetOutput("node1", "output") }},
		{"GetOutput", func() { data.GetOutput("node1") }},
		{"IsNodeRunnable", func() {
			// Create a test node
			node := workflow.NewNode("node2", nil)
			data.IsNodeRunnable(node.Name)
		}},
		{"Snapshot", func() {
			snapshot, _ := data.Snapshot()
			_ = snapshot // Use snapshot to avoid unused variable warning
		}},
		{"LoadSnapshot", func() {
			// Create a JSON snapshot
			jsonData := []byte(`{"id":"test","data":{"key1":"value1"},"nodeStatus":{},"outputs":{}}`)
			data.LoadSnapshot(jsonData)
		}},
	}

	// Run tests with a timeout
	for _, tc := range testFuncs {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan bool, 1)

			go func() {
				tc.fn()
				done <- true
			}()

			select {
			case <-done:
				// Test passed
			case <-ctx.Done():
				// Deadlock detected
				buf := make([]byte, 10000)
				n := runtime.Stack(buf, true)
				t.Fatalf("Test %s timed out, likely deadlock.\nGoroutine stack dump:\n%s", tc.name, buf[:n])
			case <-time.After(2 * time.Second):
				// Timeout
				t.Fatalf("Test %s took too long, possible performance issue", tc.name)
			}
		})
	}
}

// TestConcurrentAccess tests for race conditions or deadlocks under concurrent access
func TestConcurrentAccess(t *testing.T) {
	data := workflow.NewWorkflowData("test")

	// Test concurrent reads and writes
	t.Run("ReadWriteContention", func(t *testing.T) {
		done := make(chan bool)
		timeout := time.After(5 * time.Second)

		// Start writers
		for i := 0; i < 10; i++ {
			go func(i int) {
				for j := 0; j < 100; j++ {
					key := fmt.Sprintf("key%d", i*100+j)
					data.Set(key, j)
					data.SetNodeStatus(fmt.Sprintf("node%d", i*100+j), workflow.Running)
					data.SetOutput(fmt.Sprintf("node%d", i*100+j), j)
				}
				done <- true
			}(i)
		}

		// Start readers
		for i := 0; i < 10; i++ {
			go func(i int) {
				for j := 0; j < 100; j++ {
					key := fmt.Sprintf("key%d", i*100+j)
					data.Get(key)
					data.GetNodeStatus(fmt.Sprintf("node%d", i*100+j))
					data.GetOutput(fmt.Sprintf("node%d", i*100+j))
				}
				done <- true
			}(i)
		}

		// Wait for all goroutines or timeout
		for i := 0; i < 20; i++ {
			select {
			case <-done:
				// One goroutine finished
			case <-timeout:
				buf := make([]byte, 10000)
				n := runtime.Stack(buf, true)
				t.Fatalf("Concurrent test timed out, likely deadlock.\nGoroutine stack dump:\n%s", buf[:n])
			}
		}
	})

	// Test snapshot under contention
	t.Run("SnapshotContention", func(t *testing.T) {
		done := make(chan bool)
		timeout := time.After(5 * time.Second)

		// Start operations while taking snapshots
		for i := 0; i < 5; i++ {
			go func(i int) {
				for j := 0; j < 20; j++ {
					// Mix of operations
					key := fmt.Sprintf("key%d", i*20+j)
					data.Set(key, j)
					data.Get(key)
					data.SetNodeStatus(fmt.Sprintf("node%d", i*20+j), workflow.Running)
					data.GetNodeStatus(fmt.Sprintf("node%d", i*20+j))
				}
				done <- true
			}(i)
		}

		// Start snapshotting
		for i := 0; i < 3; i++ {
			go func() {
				for j := 0; j < 5; j++ {
					snapshot, err := data.Snapshot()
					if err != nil {
						t.Errorf("Failed to create snapshot: %v", err)
						continue
					}
					// Create a new data and load the snapshot
					newData := workflow.NewWorkflowData("test2")
					newData.LoadSnapshot(snapshot)
				}
				done <- true
			}()
		}

		// Wait for all goroutines or timeout
		for i := 0; i < 8; i++ {
			select {
			case <-done:
				// One goroutine finished
			case <-timeout:
				buf := make([]byte, 10000)
				n := runtime.Stack(buf, true)
				t.Fatalf("Snapshot contention test timed out, likely deadlock.\nGoroutine stack dump:\n%s", buf[:n])
			}
		}
	})
}
