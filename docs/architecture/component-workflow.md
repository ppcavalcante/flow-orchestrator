# Component: Workflow Engine

The Workflow Engine is the central component of Flow Orchestrator responsible for orchestrating the execution of workflows. This document provides a detailed overview of its architecture, components, and behaviors.

## Overview

The Workflow Engine ties together all other components of the system to provide:

1. Workflow execution management
2. State persistence
3. Error handling and recovery
4. Observability
5. Performance optimization

## Architecture

The Workflow Engine has a layered architecture:

```
+-----------------------------------+
|           Workflow API            |
+-----------------------------------+
|                                   |
|           Workflow Engine         |
|                                   |
+-----------------------------------+
|               |                   |
v               v                   v
+----------+ +--------+ +----------------------+
|   DAG    | | Action | |       Store          |
| Executor | | Runner | | (persistence layer)  |
+----------+ +--------+ +----------------------+
```

### Key Components

#### Workflow

The `Workflow` struct is the top-level container:

```go
type Workflow struct {
    DAG        *DAG           // Workflow structure
    WorkflowID string         // Unique identifier
    Store      WorkflowStore  // Persistence layer
    Options    WorkflowOptions // Configuration options
}
```

#### WorkflowEngine

The engine handles the execution lifecycle:

```go
type WorkflowEngine struct {
    // Internal fields
    executor    *DAGExecutor
    actionRunner *ActionRunner
    observer    WorkflowObserver
    metrics     MetricsCollector
}
```

#### DAGExecutor

The `DAGExecutor` is responsible for:

- Topological sorting of the DAG
- Level-based parallel execution
- Dependency tracking
- Node status management

#### ActionRunner

The `ActionRunner` handles:

- Action execution
- Retry logic
- Timeout management
- Worker pool management for parallel execution

#### WorkflowStore

The persistence layer interface:

```go
type WorkflowStore interface {
    Save(data *WorkflowData) error
    Load(workflowID string) (*WorkflowData, error)
    ListWorkflows() ([]string, error)
    Delete(workflowID string) error
}
```

## Execution Flow

The workflow execution process follows these steps:

1. **Initialization**:
   - Load previous state if available
   - Initialize node statuses
   - Set up observers and metrics

2. **Validation**:
   - Verify DAG structure
   - Check for cycles
   - Validate node configuration

3. **Preparation**:
   - Topologically sort the DAG
   - Group nodes into execution levels
   - Allocate memory for execution

4. **Execution**:
   - Process nodes level by level
   - Execute nodes in parallel where possible
   - Track node status and outputs
   - Persist state at key points

5. **Completion**:
   - Finalize workflow state
   - Persist final state
   - Collect and report metrics
   - Notify observers

## Persistence

The Workflow Engine integrates with the persistence layer to:

- Load previous workflow state when resuming
- Save workflow state at critical points:
  - Before node execution (for potential recovery)
  - After node execution (to capture results)
  - When workflow completes or fails

## Error Handling

The engine implements several error handling strategies:

1. **Node-Level Retry**:
   - Configurable retry attempts
   - Exponential backoff
   - Error classification for retry decisions

2. **Workflow Recovery**:
   - Resume from persistent state
   - Skip completed nodes
   - Continue from failure points

3. **Graceful Shutdown**:
   - Handle context cancellation
   - Persist state on shutdown
   - Allow for clean resumption

## Concurrency Management

The engine manages concurrent execution through:

1. **Worker Pool**:
   - Configurable number of workers
   - Work stealing for load balancing
   - Prioritization of critical path nodes

2. **Lock-Free Data Structures**:
   - Atomic operations where possible
   - Fine-grained locking for shared state
   - Read-write locks for read-heavy data

3. **Controlled Parallelism**:
   - Level-based execution for dependency satisfaction
   - Adaptive concurrency based on resource availability
   - Backpressure mechanisms

## Observability

The Workflow Engine provides observability through:

1. **Metrics**:
   - Workflow duration
   - Node execution times
   - Success/failure rates
   - Retry statistics

2. **Observers**:
   - Node status change hooks
   - Workflow lifecycle events
   - Error reporting

3. **Logging**:
   - Structured logs
   - Configurable verbosity
   - Context correlation

## Memory Management

The engine employs several memory optimization techniques:

1. **Arena Allocation**:
   - Pool of memory arenas
   - Batch allocation for related objects
   - Reduced GC pressure

2. **Object Pooling**:
   - Reuse of common objects
   - Specialized pools for high-churn objects
   - Pre-allocation of known-size collections

3. **Efficient Data Structures**:
   - Custom map implementations for specific use cases
   - Compact representations for common data
   - String interning

## Configuration Options

The Workflow Engine is configurable through `WorkflowOptions`:

```go
type WorkflowOptions struct {
    MaxConcurrency      int           // Maximum concurrent node executions
    DefaultRetryCount   int           // Default retry attempts for nodes
    DefaultRetryDelay   time.Duration // Default delay between retries
    PersistenceInterval time.Duration // How often to save state
    EnableObservability bool          // Enable metrics and observers
    ArenaSize           int           // Size of memory arenas
    LogLevel            LogLevel      // Logging verbosity
}
```

## Integration Points

The Workflow Engine provides several integration points:

1. **Public API**:
   - `Execute` method for starting workflow execution
   - `Cancel` method for graceful cancellation
   - `GetStatus` method for workflow status

2. **Extension Interfaces**:
   - `WorkflowObserver` for custom monitoring
   - `MetricsCollector` for custom metrics
   - `ErrorHandler` for custom error handling

3. **Customization Options**:
   - Custom `WorkflowStore` implementations
   - Custom `Action` implementations
   - Middleware for cross-cutting concerns

## Performance Considerations

The engine is optimized for:

1. **Low Latency**:
   - Minimal overhead per action
   - Efficient scheduling
   - Memory locality

2. **High Throughput**:
   - Parallel execution where possible
   - Batched persistence operations
   - Optimized state tracking

3. **Resource Efficiency**:
   - Minimal memory footprint
   - Controlled concurrency
   - Efficient I/O usage

## Usage Examples

### Basic Execution

```go
// Create a workflow
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "order-processing",
    Store:      store,
}

// Execute the workflow
err := workflow.Execute(context.Background())
```

### With Custom Options

```go
// Create options
options := workflow.WorkflowOptions{
    MaxConcurrency:    10,
    DefaultRetryCount: 3,
    LogLevel:          workflow.LogLevelDebug,
}

// Create a workflow with options
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "order-processing",
    Store:      store,
    Options:    options,
}

// Execute the workflow
err := workflow.Execute(context.Background())
```

### With Observer

```go
// Create an observer
observer := &workflow.DefaultObserver{
    OnNodeStatusChange: func(nodeID string, status workflow.NodeStatus) {
        fmt.Printf("Node %s changed status to %s\n", nodeID, status)
    },
    OnWorkflowComplete: func(data *workflow.WorkflowData) {
        fmt.Printf("Workflow %s completed\n", data.ID)
    },
}

// Create the workflow engine with the observer
engine := workflow.NewWorkflowEngine(
    workflow.WithObserver(observer),
)

// Execute using the engine
err := engine.Execute(dag, workflowID, store)
```

## Conclusion

The Workflow Engine component is the orchestration heart of Flow Orchestrator. It provides a robust, efficient, and extensible foundation for executing complex workflows with rich error handling, persistence, and observability.

For more information on related components, see:

- [DAG Execution Model](./dag-execution.md)
- [Middleware System](../guides/middleware.md)
- [Persistence Layer](../guides/persistence.md) 