package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultMaxConcurrency is the default per-level concurrency limit used when
// ExecutionConfig.MaxConcurrency is unset or non-positive. Concurrency is
// always bounded — there is no "unbounded" mode — because an unbounded level
// would spawn one goroutine per node, a goroutine-explosion / DoS hazard on
// large levels.
const DefaultMaxConcurrency = 16

// ExecutionConfig holds configuration options for DAG execution.
type ExecutionConfig struct {
	MaxConcurrency int // Maximum number of nodes executed concurrently per level (<=0 -> DefaultMaxConcurrency)
}

// DefaultConfig returns the default execution configuration.
func DefaultConfig() ExecutionConfig {
	return ExecutionConfig{
		MaxConcurrency: DefaultMaxConcurrency,
	}
}

// executeNodesInLevel executes all nodes in a level in parallel, bounded by
// maxConcurrency (a non-positive value coerces to DefaultMaxConcurrency).
// If any node fails, the context is cancelled to stop sibling goroutines.
func executeNodesInLevel(ctx context.Context, level []*Node, data *WorkflowData, maxConcurrency int) error {
	// Create a cancellable context so we can stop siblings on first failure
	levelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create error channel and wait group
	errChan := make(chan error, len(level))
	var wg sync.WaitGroup

	// Semaphore to limit concurrency. A non-positive limit coerces to the
	// bounded default; concurrency is never unbounded.
	if maxConcurrency <= 0 {
		maxConcurrency = DefaultMaxConcurrency
	}
	semaphore := make(chan struct{}, maxConcurrency)

	// Execute each node in this level in parallel
	for _, node := range level {
		// Skip nodes that are already completed
		if status, _ := data.GetNodeStatus(node.Name); status == Completed {
			continue
		}

		// Check dependencies. A dependency is "resolved" (does not block this
		// node) when it Completed, OR (DEC-P21-depguard) when it is a
		// continue-on-error node that Failed: such a failure is observable to
		// this node via status but must not halt the workflow. A Failed dep
		// that is NOT continue-on-error still blocks (fail-fast), and
		// Skipped/Running/Pending deps still block as before.
		dependenciesComplete := true
		for _, dep := range node.DependsOn {
			status, _ := data.GetNodeStatus(dep.Name)
			if status == Completed {
				continue
			}
			if dep.ContinueOnError && status == Failed {
				continue
			}
			dependenciesComplete = false
			errChan <- fmt.Errorf("dependency %s of node %s is not completed", dep.Name, node.Name)
			break
		}

		if !dependenciesComplete {
			continue
		}

		// Launch goroutine for node execution
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Execute the node with the cancellable context
			startTime := time.Now()
			err := n.Execute(levelCtx, data)

			if err != nil {
				if n.ContinueOnError {
					// Continue-on-error: the node is already marked Failed by
					// n.Execute. Do NOT cancel siblings and do NOT push to the
					// fail-fast error channel — the workflow proceeds and
					// dependents observe the Failed status. (DEC-M7-failure)
					return
				}
				// Fail-fast (default): cancel sibling goroutines and report.
				cancel()
				errChan <- fmt.Errorf("node %s failed after %v: %w", n.Name, time.Since(startTime), err)
			}
		}(node)
	}

	// Wait for all nodes to complete
	wg.Wait()
	close(errChan)

	// Check for errors
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	return nil
}
