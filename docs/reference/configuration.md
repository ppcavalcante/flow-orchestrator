# Configuration Options

This document provides a reference for the configuration options available in Flow Orchestrator.

## Execution Configuration

`ExecutionConfig` controls the per-level concurrency of `DAG.Execute`. As of v0.3.0 it is
**wired end-to-end**: the value you set is the limit the live execution path applies.

```go
type ExecutionConfig struct {
    MaxConcurrency int // Max nodes executed concurrently per level (<=0 -> DefaultMaxConcurrency)
}

// DefaultMaxConcurrency is the bounded default used when MaxConcurrency is unset/non-positive.
const DefaultMaxConcurrency = 16

// Default configuration
func DefaultConfig() ExecutionConfig {
    return ExecutionConfig{
        MaxConcurrency: DefaultMaxConcurrency, // 16
    }
}
```

A non-positive `MaxConcurrency` coerces to `DefaultMaxConcurrency` (16) — concurrency is
**never unbounded** (that would spawn one goroutine per node, a goroutine-explosion hazard
on large levels). Set the config via the builder or directly on a DAG:

```go
// On the builder:
dag, _ := workflow.NewWorkflowBuilder().
    WithExecutionConfig(workflow.ExecutionConfig{MaxConcurrency: 8}).
    Build()

// Or on an existing DAG:
dag.WithExecutionConfig(workflow.ExecutionConfig{MaxConcurrency: 8})
```

> **Changed in v0.3.0:** `DefaultConfig().MaxConcurrency` was bumped 4→16 so the wired
> default preserves the previous *effective* behavior (the live path was already running 16).
> The `PreserveOrder` field and the standalone `ParallelNodeExecutor` / `NewParallelNodeExecutor`
> (an unused parallel-executor that pre-dated the wired path) were **removed** — use
> `ExecutionConfig.MaxConcurrency` through the builder/DAG hooks above.

## WorkflowData Configuration

The `WorkflowDataConfig` structure controls capacity hints and optimization settings for `WorkflowData`:

```go
type WorkflowDataConfig struct {
    ExpectedNodes int             // Capacity hint for node count
    ExpectedData  int             // Capacity hint for data entries
    MetricsConfig *metrics.Config // Metrics configuration
}
```

> **Changed in v0.3.0:** the `MaxInternStringLength` and `InternStringCapacity` fields were
> **removed**. They were inert (written by the presets but never read by the interner), so
> their removal is not a behavior change.

### Presets

```go
// Default configuration
config := workflow.DefaultWorkflowDataConfig()

// Optimized for low memory usage (sizes maps tightly, disables metrics)
config := workflow.LowMemoryWorkflowDataConfig(expectedNodes)
```

> **Changed in the pre-1.0 hardening pass:** the `ReadOptimized`, `HighConcurrency`,
> and `Production` presets were removed. `ReadOptimized` and `HighConcurrency` were
> byte-identical (both just `ExpectedData = expectedNodes * 2` with default metrics);
> `Production` only differed by 1% metrics sampling, which is moot now that metrics
> default to OFF. Use `DefaultWorkflowDataConfig()` and set `ExpectedNodes` /
> `ExpectedData` directly (see Custom Configuration below), or `LowMemoryWorkflowDataConfig`.

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

### Exporting to OpenTelemetry

Once collection is enabled, the recorded metrics can be exported to any
OpenTelemetry backend via `metrics.NewOTelBridge(collector, meterProvider)`. The
library is **API-only** — the host owns the SDK, exporter, and OTLP endpoint. See
the [Observability guide](../guides/observability.md) for the instrument
inventory, cardinality contract, and host-wiring how-to.

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

// JSON file store (human-readable; good for debugging and external tools)
store, err := workflow.NewJSONFileStore("./workflow_data")

// FlatBuffers store (faster binary format for high-throughput/large state)
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
    WithStore(store)
```
