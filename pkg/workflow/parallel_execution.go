package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ExecutionConfig holds configuration options for DAG execution
type ExecutionConfig struct {
	MaxConcurrency int  // Maximum number of concurrent executions
	PreserveOrder  bool // Whether to preserve execution order in results
}

// DefaultConfig returns default execution configuration
func DefaultConfig() ExecutionConfig {
	return ExecutionConfig{
		MaxConcurrency: 4,
		PreserveOrder:  true,
	}
}

// ParallelNodeExecutor executes nodes in parallel
type ParallelNodeExecutor struct {
	config ExecutionConfig
}

// NewParallelNodeExecutor creates a new executor with the given config
func NewParallelNodeExecutor(config ExecutionConfig) *ParallelNodeExecutor {
	return &ParallelNodeExecutor{
		config: config,
	}
}

// ExecuteNodes executes multiple nodes in parallel
func (e *ParallelNodeExecutor) ExecuteNodes(ctx context.Context, nodes []*Node, data *WorkflowData) error {
	// Create error channel and wait group
	errChan := make(chan error, len(nodes))
	var wg sync.WaitGroup

	// Create semaphore to limit concurrency
	maxConcurrency := e.config.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 4 // Default to 4 concurrent executions
	}
	semaphore := make(chan struct{}, maxConcurrency)

	// Execute each node in parallel
	for _, node := range nodes {
		// Skip nodes that are already completed
		if status, _ := data.GetNodeStatus(node.Name); status == Completed {
			continue
		}

		// Check dependencies
		dependenciesComplete := true
		for _, dep := range node.DependsOn {
			if status, _ := data.GetNodeStatus(dep.Name); status != Completed {
				dependenciesComplete = false
				errChan <- fmt.Errorf("dependency %s of node %s is not completed", dep.Name, node.Name)
				break
			}
		}

		if !dependenciesComplete {
			continue
		}

		// Launch goroutine for node execution
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()

			// Acquire semaphore to limit concurrency
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Execute the node
			startTime := time.Now()
			err := n.Execute(ctx, data)

			if err != nil {
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

// ExecuteNodesInLevel executes all nodes in a level in parallel
func ExecuteNodesInLevel(ctx context.Context, level []*Node, data *WorkflowData) error {
	// Create error channel and wait group
	errChan := make(chan error, len(level))
	var wg sync.WaitGroup

	// Execute each node in this level in parallel
	for _, node := range level {
		// Skip nodes that are already completed
		if status, _ := data.GetNodeStatus(node.Name); status == Completed {
			continue
		}

		// Check dependencies
		dependenciesComplete := true
		for _, dep := range node.DependsOn {
			if status, _ := data.GetNodeStatus(dep.Name); status != Completed {
				dependenciesComplete = false
				errChan <- fmt.Errorf("dependency %s of node %s is not completed", dep.Name, node.Name)
				break
			}
		}

		if !dependenciesComplete {
			continue
		}

		// Launch goroutine for node execution
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()

			// Execute the node
			startTime := time.Now()
			err := n.Execute(ctx, data)

			if err != nil {
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
