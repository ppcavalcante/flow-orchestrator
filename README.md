# Flow Orchestrator

[![Go Report Card](https://goreportcard.com/badge/github.com/pparaujo/flow-orchestrator)](https://goreportcard.com/report/github.com/pparaujo/flow-orchestrator)
[![Go Reference](https://pkg.go.dev/badge/github.com/pparaujo/flow-orchestrator.svg)](https://pkg.go.dev/github.com/pparaujo/flow-orchestrator)
[![GitHub tag](https://img.shields.io/github/v/tag/pparaujo/flow-orchestrator?include_prereleases&label=release)](https://github.com/pparaujo/flow-orchestrator/tags)
[![Build Status](https://github.com/pparaujo/flow-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/pparaujo/flow-orchestrator/actions)
[![Coverage Status](https://codecov.io/gh/pparaujo/flow-orchestrator/branch/main/graph/badge.svg?token=CRL74YL94M)](https://codecov.io/gh/pparaujo/flow-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A lightweight, high-performance workflow orchestration engine for Go applications that need reliable execution of complex processes.

## Status: Alpha Release

Flow Orchestrator is currently in **alpha status**. While the core functionality is implemented and tested, the API may change before the stable release. We welcome early adopters and feedback!

## Overview

Flow Orchestrator is a flexible workflow engine designed for embedding within Go applications. It allows you to define, execute, and monitor complex workflows with a clean, fluent API while handling parallelism, dependencies, error handling, and persistence automatically.

### Key Features

- **High Performance**: Optimized for minimal allocations and maximum throughput
- **Thread-Safe**: Built for concurrent access with minimal lock contention
- **Persistent**: Save and resume workflows across application restarts
- **Observable**: Comprehensive metrics for monitoring and optimization
- **Extensible**: Designed to support multiple orchestration patterns
- **Embeddable**: Clean API for integration into any Go application
- **Rigorously Tested**: Property-based testing ensures correctness across a wide range of inputs

## Quick Start

### Installation

```bash
go get github.com/pparaujo/flow-orchestrator@v0.1.0-alpha
```

### Providing Feedback

We welcome feedback on the API design, feature requests, and bug reports. Please open issues on GitHub for any problems you encounter or suggestions for improvement.

### Basic Usage

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"
    
    "github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

func main() {
    // Create a workflow builder
    builder := workflow.NewWorkflowBuilder().
        WithWorkflowID("order-processing")
    
    // Add workflow steps with the fluent builder pattern
    builder.AddStartNode("validate-order").
        WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
            log.Println("Validating order...")
            data.Set("order_valid", true)
            return nil
        })
    
    builder.AddNode("process-payment").
        WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
            log.Println("Processing payment...")
            data.Set("payment_id", "pmt_123456")
            return nil
        }).
        DependsOn("validate-order")
    
    builder.AddNode("update-inventory").
        WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
            log.Println("Updating inventory...")
            data.Set("inventory_updated", true)
            return nil
        }).
        DependsOn("process-payment")
    
    // Build and execute the workflow
    dag, err := builder.Build()
    if err != nil {
        log.Fatalf("Failed to build workflow: %v", err)
    }
    
    data := workflow.NewWorkflowData("order-123")
    data.Set("order_id", "ord_789012")
    
    // Execute the workflow with a timeout
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    err = dag.Execute(ctx, data)
    if err != nil {
        log.Fatalf("Workflow execution failed: %v", err)
    }
    
    // Retrieve results
    paymentID, _ := data.GetString("payment_id")
    fmt.Printf("Order processed successfully with payment ID: %s\n", paymentID)
}
```

## Core Concepts

### Fluent Builder API

Flow Orchestrator uses a fluent builder pattern to create workflows:

```go
// Create a workflow
workflow := workflow.NewWorkflowBuilder().
    WithWorkflowID("my-workflow").
    WithStateStore(store)

// Add nodes with dependencies
workflow.AddStartNode("start-node").
    WithAction(startAction).
    WithRetries(3)

workflow.AddNode("process-node").
    WithAction(processAction).
    WithTimeout(5 * time.Second).
    DependsOn("start-node")

workflow.AddNode("final-node").
    WithAction(finalAction).
    DependsOn("process-node")

// Build and execute
dag, _ := workflow.Build()
err := dag.Execute(context.Background(), workflowData)
```

For a detailed explanation of the DAG execution model, see our [DAG Execution Model](docs/dag_execution_model.md) documentation.

### WorkflowData

The central data store for workflow execution:

```go
// Create new workflow data
data := workflow.NewWorkflowData("my-workflow-id")

// Store and retrieve values
data.Set("user_id", 12345)
data.Set("is_active", true)

userID, _ := data.GetInt("user_id")
isActive, _ := data.GetBool("is_active")

// Get node outputs
output, exists := data.GetOutput("fetch_data")
```

### Middleware

Add cross-cutting concerns like logging, retries, and timeouts:

```go
// Create middleware stack
stack := workflow.NewMiddlewareStack()
stack.Use(workflow.LoggingMiddleware())
stack.Use(workflow.RetryMiddleware(3, 1*time.Second))
stack.Use(workflow.TimeoutMiddleware(30*time.Second))

// Apply middleware to an action
wrappedAction := stack.Apply(myAction)
```

For more information on the middleware system, see our [Middleware System](docs/middleware_system.md) documentation.

### Property-Based Testing

Flow Orchestrator uses property-based testing to verify that the workflow engine satisfies fundamental invariants across a wide range of inputs:

```go
// Example property test for dependency execution order
properties.Property("dependency execution order", prop.ForAll(
    func(nodeCount int, maxDependenciesPerNode int, seed int64) bool {
        // Create a workflow with random DAG structure
        builder := NewWorkflowBuilder()
        // ... test implementation ...
        
        // Verify execution order respects dependencies
        for i, nodeName := range executionOrder {
            for _, depName := range nodeDependencies[nodeName] {
                // Dependency must be executed before the node
                if depIndex >= i {
                    return false
                }
            }
        }
        
        return true
    },
    gen.IntRange(2, 10),      // nodeCount: between 2 and 10 nodes
    gen.IntRange(0, 3),       // maxDependenciesPerNode: between 0 and 3 dependencies
    gen.Int64(),              // seed for deterministic randomness
))
```

Key properties tested include:
- Dependency execution order (nodes only execute after dependencies complete)
- Deterministic execution (same inputs produce same outputs)
- Cancellation behavior (workflows can be properly cancelled)
- Cycle detection (DAG validation correctly identifies cycles)
- Workflow data operations (data store correctly handles values)
- Node status transitions (nodes transition through correct states)

For more information on our testing approach, see our [Test Coverage Strategy](docs/test_coverage_strategy.md) documentation.

## Architecture

Flow Orchestrator is designed with a modular architecture that separates concerns and enables extensibility. For a comprehensive overview of the system architecture, see our [Architecture Overview](docs/architecture_overview.md) documentation.

Key architectural components include:

- **Workflow Engine**: Core execution engine for DAG-based workflows
- **Persistence Layer**: Pluggable storage backends for workflow state
- **Middleware System**: Extensible middleware for cross-cutting concerns
- **Metrics & Observability**: Comprehensive metrics for monitoring

## Real-World Use Cases

### E-Commerce Order Processing

```go
orderWorkflow := workflow.NewWorkflowBuilder().
    WithWorkflowID("order-processing")

// Add nodes
orderWorkflow.AddStartNode("validate-order").
    WithAction(validateOrderAction)

orderWorkflow.AddNode("check-inventory").
    WithAction(checkInventoryAction).
    DependsOn("validate-order")

orderWorkflow.AddNode("process-payment").
    WithAction(processPaymentAction).
    DependsOn("validate-order")

orderWorkflow.AddNode("allocate-inventory").
    WithAction(allocateInventoryAction).
    DependsOn("check-inventory", "process-payment")

orderWorkflow.AddNode("generate-invoice").
    WithAction(generateInvoiceAction).
    DependsOn("allocate-inventory")

orderWorkflow.AddNode("schedule-shipment").
    WithAction(scheduleShipmentAction).
    DependsOn("allocate-inventory")

orderWorkflow.AddNode("send-confirmation").
    WithAction(sendConfirmationAction).
    DependsOn("generate-invoice", "schedule-shipment")

// Build and execute
dag, _ := orderWorkflow.Build()
err := dag.Execute(context.Background(), orderData)
```

### Data Processing Pipeline

```go
dataWorkflow := workflow.NewWorkflowBuilder().
    WithWorkflowID("data-processing")

// Add nodes
dataWorkflow.AddStartNode("fetch-data").
    WithAction(fetchDataAction)

dataWorkflow.AddNode("validate-data").
    WithAction(validateDataAction).
    DependsOn("fetch-data")

dataWorkflow.AddNode("transform-data").
    WithAction(transformDataAction).
    DependsOn("validate-data")

dataWorkflow.AddNode("enrich-data").
    WithAction(enrichDataAction).
    DependsOn("validate-data")

dataWorkflow.AddNode("aggregate-data").
    WithAction(aggregateDataAction).
    DependsOn("transform-data", "enrich-data")

dataWorkflow.AddNode("store-data").
    WithAction(storeDataAction).
    DependsOn("aggregate-data")

dataWorkflow.AddNode("generate-report").
    WithAction(generateReportAction).
    DependsOn("store-data")

// Build and execute
dag, _ := dataWorkflow.Build()
err := dag.Execute(context.Background(), dataProcessingData)
```

## Performance

Flow Orchestrator is designed for high performance, with careful attention to memory allocation, concurrency, and scalability. Our benchmark suite evaluates several critical aspects of the system's performance:

- **Core component performance**: DAG construction, workflow data operations, node status management
- **Memory optimization**: Arena allocation, string interning, memory pooling
- **Concurrency and scalability**: Parallel execution, worker pool efficiency
- **Real-world scenarios**: E-commerce, ETL processing, API orchestration

Key performance characteristics:

- DAG construction scales linearly with node count
- Parallel execution scales linearly with worker count up to 32 workers
- Minimal memory allocation for core operations
- Optimized for both small and large workflows

For detailed benchmark results and performance recommendations, see our [Benchmark Analysis](docs/benchmark_analysis.md) documentation.

## Examples

The repository includes several examples to help you get started:

### Simple Workflow

A basic example demonstrating the core workflow concepts:

```bash
cd examples/new_simple
go run main.go
```

### API Workflow

An example showing how to integrate the workflow system with a REST API:

```bash
cd examples/api_workflow
go run main.go
```

### Error Handling

Examples of different error handling strategies:

```bash
cd examples/error_handling
go run main.go
```

### Comprehensive Example

A complete example showcasing all features of the workflow system:

```bash
cd examples/comprehensive
go run main.go
```

## Versioning and Roadmap

### Versioning

Flow Orchestrator follows [Semantic Versioning](https://semver.org/):

- **Current Version**: v0.1.0 (Alpha)

During the alpha and beta phases, the API may change as we refine the design based on user feedback.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
