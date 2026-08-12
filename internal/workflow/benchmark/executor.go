package benchmark

import (
	"context"
	"fmt"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// ExecutorI is an interface for workflow executors
type ExecutorI interface {
	Execute(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) (interface{}, error)
	ExecuteAsync(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) *Future
	Submit(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) error
}

// Future represents the result of an asynchronous execution
type Future struct {
	done   chan struct{}
	result interface{}
	err    error
}

// Get waits for the future to complete and returns the result
func (f *Future) Get() (interface{}, error) {
	<-f.done
	return f.result, f.err
}

// IsDone returns true if the future has completed
func (f *Future) IsDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// NewFuture creates a new future
func NewFuture() *Future {
	return &Future{
		done: make(chan struct{}),
	}
}

// Complete completes the future with the given result and error
func (f *Future) Complete(result interface{}, err error) {
	f.result = result
	f.err = err
	close(f.done)
}

// Config represents the configuration for an executor
type Config struct {
	MaxParallelism         int
	MaxConcurrentWorkflows int
	NodeExecutionTimeout   time.Duration
	QueueSize              int
	QueueTimeout           time.Duration
}

// StandardExecutor is a basic executor implementation
type StandardExecutor struct{}

// NewExecutor creates a new standard executor
func NewExecutor() *StandardExecutor {
	return &StandardExecutor{}
}

// Execute executes a workflow
func (e *StandardExecutor) Execute(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) (interface{}, error) {
	// M23 SEAL-01/02: this harness drove node actions directly off Node.Action and
	// ranged over DAG.Nodes, so it re-implemented the executor instead of measuring
	// it — no topological order (map iteration), no status writes, no retries, no
	// timeouts. Both fields are sealed now, and the harness drives the real engine.
	// NOTE: this changes what the benchmark measures; it is not comparable to a
	// pre-M23 figure from this type.
	if err := dag.Execute(ctx, data); err != nil {
		return nil, err
	}
	return data, nil
}

// ExecuteAsync executes a workflow asynchronously
func (e *StandardExecutor) ExecuteAsync(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) *Future {
	future := NewFuture()
	go func() {
		result, err := e.Execute(ctx, dag, data)
		future.Complete(result, err)
	}()
	return future
}

// Submit submits a workflow for execution
func (e *StandardExecutor) Submit(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) error {
	_, err := e.Execute(ctx, dag, data)
	return err
}

// OptimizedExecutor is an executor with improved concurrency
type OptimizedExecutor struct {
	config Config
}

// NewExecutorWithConfig creates a new optimized executor with the given configuration
func NewExecutorWithConfig(config *Config) *OptimizedExecutor {
	return &OptimizedExecutor{
		config: *config,
	}
}

// Execute executes a workflow respecting DAG dependency levels with parallel execution within each level
func (e *OptimizedExecutor) Execute(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) (interface{}, error) {
	// M23 SEAL-01: this harness re-implemented level-wise parallel execution by
	// driving n.Action directly — a second, divergent copy of the engine's own
	// scheduler that silently skipped statuses, retries and timeouts. Node.Action
	// is sealed, and the harness drives the real engine.
	// NOTE: this changes what the benchmark measures; it is not comparable to a
	// pre-M23 figure from this type.
	if err := dag.Execute(ctx, data); err != nil {
		return nil, err
	}
	return data, nil
}

// ExecuteAsync executes a workflow asynchronously
func (e *OptimizedExecutor) ExecuteAsync(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) *Future {
	future := NewFuture()
	go func() {
		result, err := e.Execute(ctx, dag, data)
		future.Complete(result, err)
	}()
	return future
}

// ErrExecutorOverloaded is returned when the executor is overloaded
var ErrExecutorOverloaded = fmt.Errorf("executor overloaded")

// Submit submits a workflow for execution with backpressure
func (e *OptimizedExecutor) Submit(ctx context.Context, dag *workflow.DAG, data *workflow.WorkflowData) error {
	// Simulate backpressure
	select {
	case <-time.After(e.config.QueueTimeout):
		return ErrExecutorOverloaded
	default:
		_, err := e.Execute(ctx, dag, data)
		return err
	}
}
