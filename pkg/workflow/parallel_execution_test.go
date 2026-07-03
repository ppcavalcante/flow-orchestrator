package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParallelExecution(t *testing.T) {
	t.Run("DefaultExecutionConfig", func(t *testing.T) {
		config := DefaultConfig()
		if config.MaxConcurrency != DefaultMaxConcurrency {
			t.Errorf("Expected default MaxConcurrency to be %d, got %d", DefaultMaxConcurrency, config.MaxConcurrency)
		}
	})

	t.Run("ParallelExecutionOfIndependentNodes", func(t *testing.T) {
		// Create a DAG
		dag := NewDAG("test-dag")

		// Track execution order
		var mu sync.Mutex
		executionTimes := make([]time.Time, 0)
		nodeNames := make([]string, 0)

		// Create nodes with different sleep durations
		node1 := NewNode("slow", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			executionTimes = append(executionTimes, time.Now())
			nodeNames = append(nodeNames, "slow")
			mu.Unlock()
			return nil
		}))

		node2 := NewNode("fast", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			executionTimes = append(executionTimes, time.Now())
			nodeNames = append(nodeNames, "fast")
			mu.Unlock()
			return nil
		}))

		// Add nodes to the DAG
		mustAddNode(t, dag, node1)
		mustAddNode(t, dag, node2)
		// Create workflow data
		data := NewWorkflowData("test-dag")

		// Execute the DAG
		err := dag.Execute(context.Background(), data)
		if err != nil {
			t.Errorf("DAG execution failed: %v", err)
		}

		// Check that both nodes executed
		if len(executionTimes) != 2 {
			t.Errorf("Expected 2 node executions, got %d", len(executionTimes))
		}

		// Check node statuses - they should all be completed
		if status, exists := data.GetNodeStatus("slow"); !exists {
			t.Errorf("Slow node status not found")
		} else if status != Completed {
			t.Errorf("Expected slow node status to be Completed, got %v", status)
		}
		if status, exists := data.GetNodeStatus("fast"); !exists {
			t.Errorf("Fast node status not found")
		} else if status != Completed {
			t.Errorf("Expected fast node status to be Completed, got %v", status)
		}

		// Note: The order of execution might vary depending on the implementation,
		// so we're simply checking that all nodes have executed successfully
	})

	t.Run("NodeFailureInParallelExecution", func(t *testing.T) {
		// Create a DAG
		dag := NewDAG("test-fail")

		// Create a failing node
		failingNode := NewNode("failing", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return fmt.Errorf("intentional failure")
		}))

		// Add node to the DAG
		mustAddNode(t, dag, failingNode)
		// Create workflow data
		data := NewWorkflowData("test-fail")

		// Execute the DAG
		err := dag.Execute(context.Background(), data)
		if err == nil {
			t.Errorf("Expected DAG execution to fail")
		}
	})

	t.Run("ContextCancellationInParallelExecution", func(t *testing.T) {
		// Create a context with cancellation
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create a DAG
		dag := NewDAG("test-cancel")

		// Create a node that checks for cancellation
		node := NewNode("long-running", ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}))

		// Add node to the DAG
		mustAddNode(t, dag, node)
		// Create workflow data
		data := NewWorkflowData("test-cancel")

		// Cancel the context after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		// Execute the DAG
		err := dag.Execute(ctx, data)

		// We expect an error that contains "context canceled"
		if err == nil {
			t.Errorf("Expected execution to fail due to context cancellation")
		} else if !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("Expected error to contain 'context canceled', got: %v", err)
		}
	})
}

type testAction struct {
	executed bool
	delay    time.Duration
	err      error
}

func (a *testAction) Execute(ctx context.Context, data *WorkflowData) error {
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
			a.executed = true
			return a.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.executed = true
	return a.err
}

func TestParallelExecutionWithDefaultConfig(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes
	action1 := &testAction{}
	action2 := &testAction{delay: 100 * time.Millisecond}
	action3 := &testAction{}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)
	node3 := NewNode("node3", action3)

	// Test parallel execution via the live level-executor; no failures on the
	// happy path.
	nodes := []*Node{node1, node2, node3}
	failures, _ := executeNodesInLevel(context.Background(), nodes, data, DefaultConfig().MaxConcurrency, resolveTracer(nil))
	require.Empty(t, failures)

	// Verify all nodes were executed
	assert.True(t, action1.executed)
	assert.True(t, action2.executed)
	assert.True(t, action3.executed)

	// Verify node statuses
	status1, _ := data.GetNodeStatus("node1")
	status2, _ := data.GetNodeStatus("node2")
	status3, _ := data.GetNodeStatus("node3")

	assert.Equal(t, Completed, status1)
	assert.Equal(t, Completed, status2)
	assert.Equal(t, Completed, status3)
}

func TestParallelExecutionWithError(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes with one failing node
	action1 := &testAction{}
	action2 := &testAction{err: assert.AnError}
	action3 := &testAction{}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)
	node3 := NewNode("node3", action3)

	// Test parallel execution via the live level-executor. The failing node is
	// returned as a NodeError; the slice is non-empty.
	nodes := []*Node{node1, node2, node3}
	failures, _ := executeNodesInLevel(context.Background(), nodes, data, DefaultConfig().MaxConcurrency, resolveTracer(nil))
	require.NotEmpty(t, failures)
	var sawNode2 bool
	for _, ne := range failures {
		if ne.NodeName == "node2" {
			sawNode2 = true
		}
	}
	assert.True(t, sawNode2, "expected node2 among the reported failures, got %+v", failures)

	// Verify node statuses
	status2, _ := data.GetNodeStatus("node2")
	assert.Equal(t, Failed, status2)
}

func TestParallelExecutionWithContext(t *testing.T) {
	// Create test workflow data
	data := NewWorkflowData("test")

	// Create test nodes with delays longer than the timeout
	action1 := &testAction{delay: 1 * time.Second}
	action2 := &testAction{delay: 1 * time.Second}

	node1 := NewNode("node1", action1)
	node2 := NewNode("node2", action2)

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Test parallel execution with timeout via the live level-executor. The
	// timed-out nodes are reported as NodeErrors carrying the deadline error.
	nodes := []*Node{node1, node2}
	failures, _ := executeNodesInLevel(ctx, nodes, data, DefaultConfig().MaxConcurrency, resolveTracer(nil))
	require.NotEmpty(t, failures)
	var sawDeadline bool
	for _, ne := range failures {
		if strings.Contains(ne.Error(), "context deadline exceeded") {
			sawDeadline = true
		}
	}
	assert.True(t, sawDeadline, "expected a context-deadline failure, got %+v", failures)

	// Verify that actions were not completed
	assert.False(t, action1.executed)
	assert.False(t, action2.executed)
}

// TestMaxConcurrencyIsHonored is the API-01 acceptance test: it proves the
// configured MaxConcurrency actually bounds the number of nodes running
// simultaneously in a single level (DEC-M3-maxconcurrency). Each action tracks
// the peak in-flight count via an atomic CAS-max; for a one-level DAG of N
// independent nodes the observed peak must equal min(K, N) and never exceed K.
func TestMaxConcurrencyIsHonored(t *testing.T) {
	// buildSingleLevelDAG builds a DAG of n independent nodes (all at level 0)
	// whose actions track the peak number of concurrently-running actions.
	buildSingleLevelDAG := func(n int) (*DAG, *int64) {
		var inFlight int64
		var peak int64
		dag := NewDAG("maxconc-test")
		for i := 0; i < n; i++ {
			node := NewNode(fmt.Sprintf("node%d", i), ActionFunc(func(ctx context.Context, data *WorkflowData) error {
				cur := atomic.AddInt64(&inFlight, 1)
				// CAS-max: bump peak up to cur if cur is larger.
				for {
					old := atomic.LoadInt64(&peak)
					if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
						break
					}
				}
				// Hold the slot long enough for siblings to pile up.
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt64(&inFlight, -1)
				return nil
			}))
			require.NoError(t, dag.AddNode(node))
		}
		return dag, &peak
	}

	const n = 50

	t.Run("LimitBites_K4", func(t *testing.T) {
		dag, peak := buildSingleLevelDAG(n)
		dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 4})
		require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("k4")))
		got := atomic.LoadInt64(peak)
		assert.LessOrEqual(t, got, int64(4), "peak in-flight must not exceed the limit")
		assert.Equal(t, int64(4), got, "with N>>K the peak must reach exactly K")
	})

	t.Run("DefaultK16", func(t *testing.T) {
		dag, peak := buildSingleLevelDAG(n)
		// Leave the DAG's DefaultConfig() (MaxConcurrency = DefaultMaxConcurrency = 16).
		require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("k16")))
		got := atomic.LoadInt64(peak)
		assert.LessOrEqual(t, got, int64(DefaultMaxConcurrency))
		assert.Equal(t, int64(DefaultMaxConcurrency), got, "with N>>default the peak must reach the default 16")
	})

	t.Run("NonPositiveCoercesToDefault", func(t *testing.T) {
		dag, peak := buildSingleLevelDAG(n)
		dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 0}) // <=0 must coerce to DefaultMaxConcurrency, never unbounded
		require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("k0")))
		got := atomic.LoadInt64(peak)
		assert.LessOrEqual(t, got, int64(DefaultMaxConcurrency), "<=0 must be bounded, not unbounded")
		assert.Equal(t, int64(DefaultMaxConcurrency), got)
	})
}
