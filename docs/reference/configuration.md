# Configuration Options

This document provides a reference for the configuration options available in Flow Orchestrator.

## Execution Configuration

The `ExecutionConfig` structure controls parallel execution behavior:

```go
type ExecutionConfig struct {
    MaxConcurrency int  // Maximum number of concurrent node executions
    PreserveOrder  bool // Whether to preserve execution order in results
}

// Default configuration
func DefaultConfig() ExecutionConfig {
    return ExecutionConfig{
        MaxConcurrency: 4,
        PreserveOrder:  true,
    }
}
```

### Usage

```go
config := workflow.ExecutionConfig{
    MaxConcurrency: 8,
    PreserveOrder:  true,
}
executor := workflow.NewParallelNodeExecutor(config)
```

## WorkflowData Configuration

The `WorkflowDataConfig` structure controls capacity hints and optimization settings for `WorkflowData`:

```go
type WorkflowDataConfig struct {
    ExpectedNodes        int // Capacity hint for node count
    ExpectedData         int // Capacity hint for data entries
    MaxInternStringLength int // Max string length for interning
    InternStringCapacity  int // Initial capacity for interned strings
    MetricsConfig        *metrics.Config // Metrics configuration
}
```

### Presets

```go
// Default configuration
config := workflow.DefaultWorkflowDataConfig()

// Optimized for read-heavy workloads
config := workflow.ReadOptimizedWorkflowDataConfig(expectedNodes)

// Optimized for high concurrency
config := workflow.HighConcurrencyWorkflowDataConfig(expectedNodes)

// Optimized for low memory usage (disables metrics, aggressive interning)
config := workflow.LowMemoryWorkflowDataConfig(expectedNodes)

// Production preset (1% sampling rate for metrics)
config := workflow.ProductionWorkflowDataConfig(expectedNodes)
```

### Custom Configuration

```go
config := workflow.DefaultWorkflowDataConfig()
config.ExpectedNodes = 100
config.ExpectedData = 200

// Override metrics
config = config.WithMetricsConfig(metrics.ProductionConfig())

// Create WorkflowData with config
data := workflow.NewWorkflowDataWithConfig("my-workflow", config)
```

## Metrics Configuration

The `metrics.Config` type (in `pkg/workflow/metrics`) controls metrics collection:

```go
import "github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"

// Create with defaults (enabled, 100% sampling)
config := metrics.NewConfig()

// Disabled metrics
config := metrics.DisabledMetricsConfig()

// Production preset (1% sampling)
config := metrics.ProductionConfig()
```

### Fluent Configuration

```go
config := metrics.NewConfig().
    WithEnabled(true).
    WithSamplingRate(0.01).          // 1% sampling
    WithOperationTiming(true).       // Track operation durations
    WithLockContention(true).        // Track lock contention
    WithMemoryTracking(false).       // Disable memory tracking
    WithSlowOperationThreshold(100 * time.Millisecond).
    WithHighContentionThreshold(10)

// Apply to default collector
config.Apply()
```

## Middleware Configuration

Middleware is configured via function parameters (not option structs):

```go
// Logging — no configuration, uses standard log package
workflow.LoggingMiddleware()

// Retry — configure retries and backoff duration (includes jitter)
workflow.RetryMiddleware(3, 500*time.Millisecond)

// Timeout — configure timeout duration
workflow.TimeoutMiddleware(5 * time.Second)

// Metrics — no configuration
workflow.MetricsMiddleware()

// Validation — provide a validator function
workflow.ValidationMiddleware(func(data *workflow.WorkflowData) error {
    if _, ok := data.GetString("required_field"); !ok {
        return fmt.Errorf("required_field is missing")
    }
    return nil
})

// Conditional retry — configure retries, backoff, and error predicate
workflow.ConditionalRetryMiddleware(3, time.Second, func(err error) bool {
    return !errors.Is(err, workflow.ErrInvalidInput)
})

// No-delay retry — for testing/benchmarks
workflow.NoDelayRetryMiddleware(3)           // quiet mode
workflow.NoDelayRetryMiddleware(3, true)     // verbose mode
```

## Persistence Configuration

Store constructors take a base directory path:

```go
// In-memory store (no configuration needed)
store := workflow.NewInMemoryStore()

// JSON file store (deprecated — use FlatBuffersStore)
store, err := workflow.NewJSONFileStore("./workflow_data")

// FlatBuffers store (recommended for production)
store, err := workflow.NewFlatBuffersStore("./workflow_data")
```

All stores implement the `WorkflowStore` interface:

```go
type WorkflowStore interface {
    Save(data *WorkflowData) error
    Load(workflowID string) (*WorkflowData, error)
    ListWorkflows() ([]string, error)
    Delete(workflowID string) error
}
```

### Using a Store with the Builder

```go
builder := workflow.NewWorkflowBuilder().
    WithWorkflowID("my-workflow").
    WithStore(store) // or WithStateStore(store) for backward compatibility
```
