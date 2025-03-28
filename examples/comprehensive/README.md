# Comprehensive Workflow System Example

This example demonstrates the full capabilities of the workflow system, including:

- Arena-based memory management for improved performance
- Metrics collection and reporting
- Multiple workflow patterns and use cases
- Error handling and recovery strategies
- Performance profiling options

## Overview

The example includes three different workflow scenarios:

1. **E-Commerce Checkout Workflow**: Simulates a typical e-commerce checkout process with cart validation, inventory checking, payment processing, and order creation.

2. **Data Processing Workflow**: Demonstrates a data processing pipeline with extraction, validation, transformation, aggregation, and report generation.

3. **Error Handling Workflow**: Shows how to handle errors, implement retries, and manage timeouts in workflows.

## Features Demonstrated

- **Arena-based Memory Management**: Efficient memory allocation using memory arenas
- **String Interning**: Reduced memory usage through string deduplication
- **Metrics Collection**: Performance tracking for operations
- **Configurable Worker Pool**: Parallel execution of workflow nodes
- **Error Handling**: Retry mechanisms and timeout handling
- **Profiling**: Optional CPU and memory profiling

## Configuration Options

The example allows you to configure various aspects of the workflow system:

- `UseArena`: Enable/disable arena-based memory management
- `ArenaBlockSize`: Configure the size of memory blocks in the arena
- `EnableMetrics`: Enable/disable metrics collection
- `MetricsSamplingRate`: Set the sampling rate for metrics
- `WorkerCount`: Configure the number of worker goroutines
- `EnableProfiling`: Enable/disable CPU and memory profiling
- `ProfileOutputDir`: Set the directory for profiling output

## Running the Example

To run the example:

```bash
cd examples/comprehensive
go run main.go
```

## Expected Output

The example will run all three workflows in sequence and display:

1. Configuration settings
2. Execution progress for each workflow
3. Results from each workflow
4. Memory usage statistics (if arena is enabled)
5. Performance metrics (if metrics are enabled)

## Modifying the Example

You can modify the configuration in the `main()` function to experiment with different settings:

```go
config := Config{
    UseArena:            true,
    ArenaBlockSize:      8192,
    EnableMetrics:       true,
    MetricsSamplingRate: 0.5,
    WorkerCount:         runtime.NumCPU(),
    EnableProfiling:     false,
    ProfileOutputDir:    "./profiles",
}
```

Try changing these values to see how they affect performance and memory usage. 