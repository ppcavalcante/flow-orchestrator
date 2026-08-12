# Component: Workflow Engine

The workflow execution subsystem — the `Workflow`/`DAG` types and `DAG.Execute` — orchestrates the execution of workflows. ("Workflow Engine" here is a conceptual name for that subsystem, not a Go type.) This document provides a detailed overview of its architecture, components, and behaviors.

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
    WorkflowID string        // Unique identifier
    Store      WorkflowStore // Persistence layer
    // ... plus Clock, Locker, MetricsConfig, RollbackTimeout, MaxSubWorkflowDepth.
    // The graph itself is UNEXPORTED (M23 SEAL-06) — read it with the DAG() accessor.
}

func (w *Workflow) DAG() *DAG
```

**`FromBuilder(builder)` is the only exported path to a runnable `*Workflow`** — a
directly-constructed one carries no validated graph. Run it with
`(*Workflow).Execute(ctx)`.

#### Execution path

There is no separate `WorkflowEngine` type — execution lives on the `DAG`:

- `(*DAG).Validate()` checks structure and detects cycles. It does **not** compute
  start/end nodes or execution levels: `DAG.StartNodes`/`EndNodes` were deleted in
  v0.22.0 (M23 ph117), and levels are derived on demand by `GetLevels()`, which
  recomputes the dependency-free set (`len(node.dependsOn) == 0`) on every call.
- `(*DAG).Execute(ctx, data)` runs the workflow level by level. Within each
  level, nodes are run in parallel bounded by `ExecutionConfig.MaxConcurrency`
  (the internal `executeNodesInLevel` helper, default 16).
- Per-node retry/timeout behavior is supplied by the node's action and any
  middleware wrapping it (`RetryMiddleware`, `TimeoutMiddleware`), not a
  dedicated runner type.

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

A store MAY additionally implement the optional `Checkpointer` interface to opt
into durable mid-run checkpointing (M9 crash-resume):

```go
type Checkpointer interface {
    // SaveCheckpoint atomically and durably persists the current workflow state.
    SaveCheckpoint(data *WorkflowData) error
}
```

When the configured store implements `Checkpointer`, `Workflow.Execute` flushes
the run's state at each completed level barrier, so a crash mid-run resumes from
the last checkpoint instead of restarting. All **four** built-in stores implement it
— `InMemoryStore`, `JSONFileStore`, `FlatBuffersStore` and `SQLiteStore` (the file
stores via atomic temp+fsync+rename). See the
[Persistence guide → Durability & Idempotency](../guides/persistence.md#durability--idempotency-crash-resume).

## Execution Flow

The workflow execution process follows these steps:

1. **Initialization**:
   - Load previous state if available
   - Initialize node statuses
   - Initialize the metrics collector (if enabled)

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
   - Record per-operation metrics

## Persistence

The Workflow Engine integrates with the persistence layer to:

- Load previous workflow state when resuming
- Save workflow state at critical points:
  - **At each completed level barrier** — checkpointing is level-granular, **not
    per-node**: nodes within a level run in parallel and the flush happens once the
    whole level drains. A crash mid-level re-runs that level's nodes on resume, which
    is why actions must be idempotent (see
    [DAG execution → checkpointing](dag-execution.md#checkpointing)).
  - When the workflow completes, fails, or parks

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

1. **Bounded per-level parallelism**:
   - Each runnable node in a level runs in its own goroutine
   - A buffered-channel semaphore caps in-flight goroutines at
     `ExecutionConfig.MaxConcurrency` (default 16) — a fixed configured bound, not
     a worker pool, work-stealing, or adaptive/backpressure scheduler
   - `executeNodesInLevel` (`parallel_execution.go`) implements this

2. **Shared-state safety**:
   - `WorkflowData` uses a `sync.RWMutex` (read-heavy reads take the read lock)
   - The metrics config is read lock-free via `atomic.Pointer`

3. **Level-based ordering**:
   - Nodes execute level by level so every dependency completes before its
     dependents start

## Observability

The Workflow Engine provides observability through:

1. **Metrics**:
   - Workflow duration
   - Node execution times
   - Success/failure rates
   - Retry statistics
   - Exportable to OpenTelemetry via the API-only bridge (host owns the SDK/exporter) —
     see the [Observability guide](../guides/observability.md)

2. **Node status**:
   - Per-node status — **nine** states tracked on the `WorkflowData`:
     Pending/Running/Completed/Failed/Skipped, plus `Waiting` (v0.10.0),
     `Bypassed` (v0.11.0), and `Compensated`/`CompensationFailed` (v0.12.0)
     (`Waiting` — a node parked on an external
     event; non-terminal and non-failing)

3. **Logging**:
   - `LoggingMiddleware` logs action start/completion and errors via the standard
     log package

(There is no observer/hook subsystem — observation is via metrics, node status,
and logging middleware.)

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

Configuration is split across a few focused types rather than a single options
struct:

```go
// Per-level execution concurrency (pkg/workflow/parallel_execution.go).
type ExecutionConfig struct {
    MaxConcurrency int                    // Max nodes executed concurrently per level (<=0 -> DefaultMaxConcurrency, 16)
    TracerProvider trace.TracerProvider   // OTel span-per-node provider; nil disables tracing (API-only, host owns the SDK)
}

// WorkflowData capacity hints + metrics (pkg/workflow/workflow_data_config.go).
type WorkflowDataConfig struct {
    ExpectedNodes int
    ExpectedData  int
    MetricsConfig *metrics.Config
}
```

- **Concurrency** is set with `WorkflowBuilder.WithExecutionConfig(cfg)` or
  `DAG.WithExecutionConfig(cfg)`; `DefaultConfig()` uses `MaxConcurrency = 16`.
- **Retries / timeouts** are configured **per node** on the builder
  (`WithRetries`, `WithTimeout`) or via middleware (`RetryMiddleware`,
  `TimeoutMiddleware`), not via a global option.
- **Continue-on-error** is set **per node** with `WithContinueOnError()`: that
  node's failure is recorded as `Failed` but does not fail the workflow (the
  default is fail-fast). See [DAG Execution → failure handling](dag-execution.md).
- **Metrics** are configured with `metrics.Config` (see the
  [Configuration reference](../reference/configuration.md)) and attached via
  `WorkflowDataConfig.WithMetricsConfig`; export to OpenTelemetry is covered in
  the [Observability guide](../guides/observability.md).
- **Persistence** is chosen by which `WorkflowStore` you wire in
  (`WithStore`), not a persistence-interval option.

## Integration Points

The engine provides several integration points:

1. **Public API**:
   - `(*DAG).Execute(ctx, data)` / `(*Workflow).Execute(ctx)` to run a workflow
   - cancellation is via the standard `context.Context` passed to `Execute`
   - node status is tracked on the `WorkflowData` (e.g. `GetNodeStatus`)

2. **Customization points**:
   - custom `WorkflowStore` implementations (persistence backend)
   - custom `Action` implementations (the `Action` interface / `ActionFunc`)
   - `Middleware` for cross-cutting concerns (logging, retry, timeout,
     validation, metrics)
   - `metrics.Config` for metrics collection, exportable to OpenTelemetry via
     the metrics bridge

## Performance Considerations

The engine is optimized for:

1. **Low Latency**:
   - Minimal overhead per action
   - Efficient scheduling
   - Memory locality

2. **High Throughput**:
   - Level-wise parallel execution under a bounded concurrency limit
   - Binary (FlatBuffers) serialization for persistence
   - Optimized state tracking

3. **Resource Efficiency**:
   - Minimal memory footprint
   - Controlled concurrency
   - Efficient I/O usage

## Usage Examples

### Basic Execution

```go
// Create a workflow. The store goes on the builder; FromBuilder is the only exported
// path to a runnable *Workflow (M23 SEAL-06 sealed the DAG field).
builder.WithWorkflowID("order-processing").WithStore(store)

wf, err := workflow.FromBuilder(builder)
if err != nil {
    log.Fatalf("failed to build workflow: %v", err)
}

// Execute the workflow
err := wf.Execute(context.Background())
```

### With custom concurrency

Concurrency is configured with an `ExecutionConfig` on the builder (or directly
on the DAG):

```go
wf, err := workflow.FromBuilder(workflow.NewWorkflowBuilder().
    WithWorkflowID("order-processing").
    WithStore(store).
    WithExecutionConfig(workflow.ExecutionConfig{MaxConcurrency: 10}))
if err != nil {
    log.Fatal(err)
}

err = wf.Execute(context.Background())
```

> **`WithStore` reaches execution only via `FromBuilder`.** `Build()` returns a
> store-less `*DAG` (a DAG carries no store by design), so a store set with
> `WithStore` rides to execution only through `FromBuilder` (which builds a
> `*Workflow` carrying the store). Since **v0.13.0 (REM-04)**,
> `builder.WithStore(s).Build()` **returns an error** rather than silently running
> non-durable — the guard directs you to `FromBuilder`.

### With metrics enabled

Metrics are configured on the `WorkflowData` via `WorkflowDataConfig`:

```go
cfg := workflow.DefaultWorkflowDataConfig().
    WithMetricsConfig(metrics.NewConfig())
data := workflow.NewWorkflowDataWithConfig("order-processing", cfg)

err := dag.Execute(context.Background(), data)
```

To export those metrics to OpenTelemetry, see the
[Observability guide](../guides/observability.md).

## Conclusion

The Workflow Engine component is the orchestration heart of Flow Orchestrator. It provides a robust, efficient, and extensible foundation for executing complex workflows with rich error handling, persistence, and observability.

For more information on related components, see:

- [DAG Execution Model](./dag-execution.md)
- [Middleware System](../guides/middleware.md)
- [Persistence Layer](../guides/persistence.md) 