# Flow Orchestrator

[![Go Report Card](https://goreportcard.com/badge/github.com/ppcavalcante/flow-orchestrator)](https://goreportcard.com/report/github.com/ppcavalcante/flow-orchestrator)
[![Go Reference](https://pkg.go.dev/badge/github.com/ppcavalcante/flow-orchestrator.svg)](https://pkg.go.dev/github.com/ppcavalcante/flow-orchestrator)
[![GitHub tag](https://img.shields.io/github/v/tag/ppcavalcante/flow-orchestrator?include_prereleases&label=release)](https://github.com/ppcavalcante/flow-orchestrator/tags)
[![Build Status](https://github.com/ppcavalcante/flow-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/ppcavalcante/flow-orchestrator/actions)
[![codecov](https://codecov.io/gh/ppcavalcante/flow-orchestrator/branch/main/graph/badge.svg)](https://codecov.io/gh/ppcavalcante/flow-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Status: Alpha](https://img.shields.io/badge/status-alpha-orange.svg)](#status-alpha-release)

A lightweight, high-performance workflow orchestration engine for Go applications that need reliable execution of complex processes — an **embeddable, formally-verified durable DAG engine**: crash anywhere and resume from the last completed level, with **no server, no database required, and no determinism tax.**

## Status: Alpha Release

Flow Orchestrator is currently in **alpha status**. While the core functionality is implemented and tested, the API may change before the stable release. We welcome early adopters and feedback!

## Overview

Flow Orchestrator is a flexible workflow engine designed for embedding within Go applications. It allows you to define, execute, and monitor complex workflows with a clean, fluent API while handling parallelism, dependencies, error handling, and persistence automatically.

It is a **durable execution core**: a workflow that crashes mid-run can be resumed — restart with the same workflow ID and store, and execution picks up from the last completed level without re-running finished work. Because a workflow here is **data (a static DAG), not replayed code**, this durability carries **no determinism tax** (unlike replay-based engines) and the resume algorithm is **machine-checked in TLA+** — a niche no other Go engine holds: an embeddable, formally-verified durable DAG engine with no server and no DB required. See [Durable Crash-Resume](#durable-crash-resume).

### Key Features

- **High Performance**: Optimized for minimal allocations and maximum throughput
- **Thread-Safe**: Built for concurrent access with minimal lock contention
- **Durable Crash-Resume**: Opt-in mid-run checkpointing (the optional `Checkpointer` interface; nil = zero overhead) lets a workflow survive a process crash and resume from the last completed level — same workflow ID + store, no finished work re-run, no server or DB required. Resume-equivalence is machine-checked in TLA+ ([`DurableExecutor.tla`](specs/README.md))
- **Observable**: Comprehensive metrics for monitoring and optimization
- **Extensible**: Designed to support multiple orchestration patterns
- **Embeddable**: Clean API for integration into any Go application
- **Type-Safe Data**: Optional generic typed keys (`Key[T]`) over the shared data store for compile-time-checked producer/consumer contracts
- **Resilient**: Per-node continue-on-error lets selected steps fail without halting the workflow (default is fail-fast)
- **Rigorously Tested & Formally Modeled**: Property-based tests (gopter) over random DAGs plus a TLC-checked TLA+ model of the executor verify the core invariants

## Quick Start

### Installation

```bash
go get github.com/ppcavalcante/flow-orchestrator@latest
```

> **Versioning:** the project is **alpha** — every published tag is a pre-release, and there is
> **no stable (`v1`+) release**. The latest is **`v0.9.0-alpha`** (the M9 durable execution core:
> crash-resume via the optional `Checkpointer` interface, per-level checkpointing, the
> at-least-once contract + `IdempotencyKey`, atomic writes, and a TLA+-verified resume-equivalence
> model — on top of the M1–M8 work). Because there is no stable tag, `go get @latest`
> resolves to the highest pre-release — currently **`v0.9.0-alpha`** — so the command above is
> correct. Pinning the exact version (`@v0.9.0-alpha`) is optional but recommended for
> reproducibility, and the API may change between alpha minors (see [STABILITY.md](STABILITY.md)).
> The in-code version (`pkg/workflow.Version`) reads `0.9.0-alpha`. See
> [CHANGELOG.md](CHANGELOG.md).

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
    
    "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
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
    WithStore(store)

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

For a detailed explanation of the DAG execution model, see our [DAG Execution Model](docs/architecture/dag-execution.md) documentation.

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

For more information on the middleware system, see our [Middleware System](docs/guides/middleware.md) documentation.

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

The executor's core invariants — topological order, peak concurrency within
`MaxConcurrency`, run-once completeness, dependencies-before-run, and the
continue-on-error / fail-fast failure semantics — are covered by the gopter suite
in [`pkg/workflow/invariants_property_test.go`](pkg/workflow/invariants_property_test.go),
run as part of `go test ./...`.

### Formal verification

Beyond the property tests, the level executor is modeled in TLA+ under
[`specs/`](specs/README.md) and machine-checked with TLC for safety (concurrency
bound, dependencies-before-run, fail-fast halting) and liveness (termination /
deadlock-freedom). A second model, [`DurableExecutor.tla`](specs/README.md),
machine-checks the **crash-resume algorithm**: resume-equivalence on both arms —
`ExecFidelity` (a reported result must have actually executed; no phantom
checkpoint) and `StatusConvergence` (a crash introduces no new terminal state).
`specs/README.md` documents the models, the scenarios, and the honest scope
(design-exhaustive model vs implementation-sampled tests).

For more information on our testing approach, see our [Test Coverage Strategy](docs/development/test_coverage_strategy.md) documentation.

## Durable Crash-Resume

A workflow run can survive a process crash and resume from where it left off,
re-running only the work that had not yet completed — **durable execution** with no
server and no database required.

A `WorkflowStore` MAY implement the optional `Checkpointer` interface to opt in:

```go
type Checkpointer interface {
    // SaveCheckpoint atomically and durably persists the current workflow state.
    SaveCheckpoint(data *WorkflowData) error
}
```

When the store implements it, `Workflow.Execute` flushes the run's state **at each
completed level barrier** (atomically — temp file + fsync + rename). **Resume is just
re-running `Execute`** with the same workflow ID, store, and DAG: completed nodes are
skipped (their outputs rehydrated), every other node re-runs, and a persisted node
missing from the current DAG is rejected (a graph-identity guard) rather than
mis-resumed. A store that does not implement `Checkpointer` keeps the prior
save-at-boundaries behavior with **zero overhead**. All three built-in stores
(`InMemoryStore`, `JSONFileStore`, `FlatBuffersStore`) implement it.

**At-least-once contract.** A node that had not reached `Completed` when the crash
hit — including one that was in flight — re-runs on resume, so its side effect can
happen more than once. Side-effecting actions **must be idempotent**; the library
provides `IdempotencyKey(data, nodeName)`, a replay-stable key (derived only from
the workflow ID and node name, byte-identical across a resume) to drive downstream
deduplication. This is the same guarantee Temporal, DBOS, and Restate impose.

Because a workflow is **data (a static DAG), not replayed code**, crash-resume costs
**no determinism tax**, and the resume algorithm is machine-checked in TLA+.

See the [Persistence guide → Durability & Idempotency](docs/guides/persistence.md#durability--idempotency-crash-resume)
for the worked detail and the [API reference](docs/reference/api-reference.md#durable-crash-resume-added-v090)
for the durable surface.

## Architecture

Flow Orchestrator is designed with a modular architecture that separates concerns and enables extensibility. For a comprehensive overview of the system architecture, see our [Architecture Overview](docs/architecture/overview.md) documentation.

Key architectural components include:

- **Workflow Engine**: Core execution engine for DAG-based workflows
- **Persistence Layer**: Pluggable storage backends for workflow state, with optional durable mid-run checkpointing (`Checkpointer`) for crash-resume
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
- **Concurrency and scalability**: level-wise parallel execution under a bounded concurrency limit
- **Real-world scenarios**: E-commerce, ETL processing, API orchestration

Key performance characteristics:

- DAG construction scales linearly with node count
- Parallel execution scales linearly with worker count up to 32 workers
- Minimal memory allocation for core operations
- Optimized for both small and large workflows

For detailed benchmark results and performance recommendations, see our [Benchmark Analysis](docs/guides/benchmarks.md) documentation.

## Examples

The repository includes several examples to help you get started:

### Simple Workflow

A basic example demonstrating the core workflow concepts:

```bash
cd examples/new_simple
go run main.go
```

### API Workflow

A command-line example that models an API-orchestration pipeline (fetch → process → send → save) as a workflow, using mock service clients (no HTTP server):

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

- **Latest release**: `v0.9.0-alpha` (the highest published tag; the `pkg/workflow.Version` marker on `main` reads `0.9.0-alpha`). Every tag is a pre-release, so `go get @latest` resolves to this; see the Versioning note under [Installation](#installation).
- **Stable release**: none yet — the project is pre-1.0 alpha. The API may change between alpha minors (see [STABILITY.md](STABILITY.md)).

### Roadmap

- **Shipped (M9, durable execution core):** crash-resume via the optional
  `Checkpointer` interface — per-level checkpointing, resume from the last completed
  level, the at-least-once contract + `IdempotencyKey`, atomic writes, and a
  TLA+-machine-checked resume-equivalence model.
- **Future (not yet shipped):** Tier-2 *suspend-resume* — durable timers, signals,
  and waiting for external events. These require a re-entrant execution model and are
  a deliberate future direction, **not** part of the current release.

During the alpha and beta phases, the API may change as we refine the design based on user feedback.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
