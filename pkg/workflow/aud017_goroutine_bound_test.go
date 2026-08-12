package workflow

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-017 / C-01: executeNodesInLevel must bound the number of LIVE goroutines by
// maxConcurrency, not by the level width. The pre-fix code spawned one goroutine
// per runnable node and acquired the semaphore INSIDE each, so a 5,000-node level
// created ~5,000 parked goroutines while only maxConcurrency actions ran. The
// DefaultMaxConcurrency doc claims the bound prevents exactly that.
func TestAUD017_LevelGoroutinesBoundedByMaxConcurrency(t *testing.T) {
	const (
		n              = 200
		maxConcurrency = 2
	)

	started := make(chan struct{}, n)
	release := make(chan struct{})

	level := make([]*Node, n)
	for i := 0; i < n; i++ {
		level[i] = newNode(
			// distinct names; all independent (no deps) => all launch-eligible at once
			"n"+string(rune('A'+i%26))+string(rune('0'+i/26)),
			ActionFunc(func(ctx context.Context, data *WorkflowData) error {
				started <- struct{}{}
				<-release // block so the slot stays occupied while we sample
				return nil
			}),
		)
	}

	data := NewWorkflowData("aud017")

	// Let the runtime settle, then take a baseline goroutine count.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		executeNodesInLevel(context.Background(), level, data, maxConcurrency, resolveTracer(nil))
	}()

	// Wait until maxConcurrency actions are actually running (slots full).
	for i := 0; i < maxConcurrency; i++ {
		<-started
	}
	// Give any (buggy) surplus goroutines a beat to have been spawned.
	time.Sleep(50 * time.Millisecond)

	delta := runtime.NumGoroutine() - baseline

	// With the fix, delta ~= maxConcurrency + a small constant (producer, waiter).
	// Pre-fix, delta ~= n. A generous ceiling well below n discriminates cleanly.
	require.Lessf(t, delta, n/4,
		"live goroutines grew by %d for a %d-node level at maxConcurrency=%d; "+
			"MaxConcurrency must bound goroutine count, not just active actions", delta, n, maxConcurrency)

	close(release)
	wg.Wait()

	// Every node must still have completed.
	for _, node := range level {
		st, _ := data.GetNodeStatus(node.name)
		require.Equal(t, Completed, st, "node %s should complete", node.name)
	}
}
