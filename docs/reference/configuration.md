# Configuration Options

This document provides a comprehensive reference for all configuration options available in Flow Orchestrator. These options allow you to customize the behavior of various components to suit your specific needs.

## Workflow Options

The `WorkflowOptions` structure provides configuration options for workflow execution:

```go
type WorkflowOptions struct {
    // MaxConcurrency controls the maximum number of nodes that can execute concurrently
    MaxConcurrency int
    
    // DefaultRetryCount sets the default number of retries for nodes that fail
    DefaultRetryCount int
    
    // DefaultRetryDelay sets the default delay between retry attempts
    DefaultRetryDelay time.Duration
    
    // PersistenceInterval controls how often workflow state is persisted during execution
    PersistenceInterval time.Duration
    
    // EnableObservability enables metrics collection and observer callbacks
    EnableObservability bool
    
    // ArenaSize sets the size of memory arenas for optimized allocation
    ArenaSize int
    
    // EnableArenaAllocator enables the use of arena memory allocation
    EnableArenaAllocator bool
    
    // EnableStringInterning enables string interning for memory optimization
    EnableStringInterning bool
    
    // EnableBufferPooling enables buffer pooling for reduced allocations
    EnableBufferPooling bool
    
    // EnableNodePooling enables node pooling for reduced allocations
    EnableNodePooling bool
    
    // LogLevel sets the verbosity of workflow logging
    LogLevel LogLevel
}
```

### Default Values

```go
var DefaultWorkflowOptions = WorkflowOptions{
    MaxConcurrency:      runtime.NumCPU(),
    DefaultRetryCount:   0,
    DefaultRetryDelay:   time.Second,
    PersistenceInterval: 5 * time.Second,
    EnableObservability: true,
    ArenaSize:           64 * 1024, // 64KB
    EnableArenaAllocator: false,
    EnableStringInterning: false,
    EnableBufferPooling: false,
    EnableNodePooling: false,
    LogLevel:           LogLevelInfo,
}
```

### Using Workflow Options

```go
// Create custom options
options := workflow.WorkflowOptions{
    MaxConcurrency:    8,
    DefaultRetryCount: 3,
    DefaultRetryDelay: 2 * time.Second,
    LogLevel:          workflow.LogLevelDebug,
}

// Create a workflow with the options
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "my-workflow",
    Store:      store,
    Options:    options,
}
```

## Middleware Options

### Logging Middleware Options

```go
// Create logging middleware with custom options
loggingMiddleware := workflow.LoggingMiddleware(
    workflow.WithLogger(customLogger),
    workflow.WithLogLevel(workflow.LogLevelDebug),
    workflow.WithLogPrefix("workflow"),
)
```

Available options:
- `WithLogger(logger)`: Sets a custom logger
- `WithLogLevel(level)`: Sets the logging level
- `WithLogPrefix(prefix)`: Sets a prefix for log messages

### Retry Middleware Options

```go
// Create retry middleware with custom options
retryMiddleware := workflow.RetryMiddleware(
    3,              // Max retries
    time.Second,    // Base delay
    workflow.WithExponentialBackoff(2.0),
    workflow.WithMaxDelay(30 * time.Second),
    workflow.WithRetryableErrors([]string{"connection_error", "timeout_error"}),
)
```

Available options:
- `WithExponentialBackoff(factor)`: Sets exponential backoff factor
- `WithMaxDelay(duration)`: Sets maximum delay between retries
- `WithRetryableErrors(errors)`: Sets specific error types to retry
- `WithJitter(factor)`: Adds random jitter to retry delays

### Metrics Middleware Options

```go
// Create metrics middleware with custom options
metricsMiddleware := workflow.MetricsMiddleware(
    workflow.WithMetricsRegistry(customRegistry),
    workflow.WithMetricsPrefix("myapp"),
    workflow.WithMetricsSampleRate(0.1),
)
```

Available options:
- `WithMetricsRegistry(registry)`: Sets a custom metrics registry
- `WithMetricsPrefix(prefix)`: Sets a prefix for metric names
- `WithMetricsSampleRate(rate)`: Sets sampling rate for metrics

## Persistence Options

### JSON File Store Options

```go
// Create a JSON file store with custom options
store, err := workflow.NewJSONFileStore(
    "./workflow_data",
    workflow.WithJSONPrettyPrint(true),
    workflow.WithJSONFilePermissions(0600),
)
```

Available options:
- `WithJSONPrettyPrint(enabled)`: Enables pretty-printed JSON
- `WithJSONFilePermissions(perm)`: Sets file permissions
- `WithJSONBackupCount(count)`: Sets number of backups to keep

### FlatBuffers Store Options

```go
// Create a FlatBuffers store with custom options
store, err := workflow.NewFlatBuffersStore(
    "./workflow_data",
    workflow.WithCompression(true),
    workflow.WithCompressionLevel(9),
    workflow.WithFilePermissions(0600),
)
```

Available options:
- `WithCompression(enabled)`: Enables data compression
- `WithCompressionLevel(level)`: Sets compression level (1-9)
- `WithFilePermissions(perm)`: Sets file permissions
- `WithBackupCount(count)`: Sets number of backups to keep

## Memory Optimization Options

### Arena Options

```go
// Create an arena with custom options
arena := workflow.NewArena(
    workflow.WithArenaBlockSize(128 * 1024),
    workflow.WithArenaPreallocation(true),
)
```

Available options:
- `WithArenaBlockSize(size)`: Sets the arena block size
- `WithArenaPreallocation(enabled)`: Enables memory preallocation
- `WithArenaGrowthFactor(factor)`: Sets growth factor for arena blocks

### String Pool Options

```go
// Create a string pool with custom options
pool := workflow.NewStringPool(
    arena,
    workflow.WithStringCacheSize(10000),
    workflow.WithPreinternedStrings(commonStrings),
)
```

Available options:
- `WithStringCacheSize(size)`: Sets the size of the string cache
- `WithPreinternedStrings(strings)`: Pre-interns common strings
- `WithSyncMapBackend(enabled)`: Uses sync.Map instead of sharded map

## Concurrency Options

### Worker Pool Options

```go
// Create a worker pool with custom options
pool := workflow.NewWorkerPool(
    workflow.WithWorkerCount(8),
    workflow.WithQueueSize(100),
    workflow.WithWorkerIdleTimeout(30 * time.Second),
)
```

Available options:
- `WithWorkerCount(count)`: Sets the number of workers
- `WithQueueSize(size)`: Sets the task queue size
- `WithWorkerIdleTimeout(timeout)`: Sets worker idle timeout
- `WithPrioritization(enabled)`: Enables task prioritization

## Observer Options

```go
// Create an observer with custom options
observer := workflow.NewObserver(
    workflow.WithOnNodeStatusChange(statusChangeCallback),
    workflow.WithOnWorkflowComplete(workflowCompleteCallback),
    workflow.WithOnWorkflowError(workflowErrorCallback),
)
```

Available options:
- `WithOnNodeStatusChange(callback)`: Sets node status change callback
- `WithOnWorkflowComplete(callback)`: Sets workflow completion callback
- `WithOnWorkflowError(callback)`: Sets workflow error callback
- `WithOnNodeStart(callback)`: Sets node start callback
- `WithOnNodeComplete(callback)`: Sets node completion callback
- `WithOnNodeError(callback)`: Sets node error callback

## Environment Variables

Flow Orchestrator also respects the following environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `FLOW_ORCHESTRATOR_MAX_CONCURRENCY` | Maximum concurrent node executions | Number of CPU cores |
| `FLOW_ORCHESTRATOR_DEFAULT_RETRY_COUNT` | Default retry count for failed nodes | 0 |
| `FLOW_ORCHESTRATOR_DEFAULT_RETRY_DELAY` | Default delay between retries (in milliseconds) | 1000 |
| `FLOW_ORCHESTRATOR_PERSISTENCE_INTERVAL` | Interval for persisting workflow state (in milliseconds) | 5000 |
| `FLOW_ORCHESTRATOR_ENABLE_OBSERVABILITY` | Enable metrics collection and observer callbacks | true |
| `FLOW_ORCHESTRATOR_ARENA_SIZE` | Size of memory arenas in bytes | 65536 |
| `FLOW_ORCHESTRATOR_LOG_LEVEL` | Logging verbosity (debug, info, warn, error) | info |
| `FLOW_ORCHESTRATOR_ENABLE_ARENA_ALLOCATOR` | Enable arena memory allocation | false |
| `FLOW_ORCHESTRATOR_ENABLE_STRING_INTERNING` | Enable string interning | false | 