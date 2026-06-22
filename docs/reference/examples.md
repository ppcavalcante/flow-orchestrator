# Example Applications

Flow Orchestrator includes several example applications to demonstrate its features and capabilities. This document provides an overview of these examples and how to use them.

## Available Examples

### Simple Workflow

**Location**: `examples/new_simple`

A basic example demonstrating the core workflow concepts. It creates a simple workflow with a few dependent nodes and executes it.

**Key Features Demonstrated**:
- Basic workflow creation
- Action implementation
- Dependency definition
- Workflow execution

**How to Run**:
```bash
cd examples/new_simple
go run main.go
```

### API Workflow

**Location**: `examples/api_workflow`

A command-line example that models an **API-orchestration pipeline** as a
workflow: fetch user data, process it, send it to an analytics endpoint, and save
it to a database. The external services are **mock clients** (no real network
calls, no HTTP server) so the example is self-contained and deterministic.
Configuration flags let you simulate failures and slow responses to exercise the
error-handling and retry paths.

**Key Features Demonstrated**:
- Modeling a multi-step service pipeline as a DAG
- Passing data between nodes via `WorkflowData`
- Error handling and failure-injection (the simulate-* flags)

**How to Run**:
```bash
cd examples/api_workflow
go run main.go
```

### Error Handling

**Location**: `examples/error_handling`

Examples of different error handling strategies in workflows. It demonstrates how to handle errors at different levels, including node retries, error recovery, and workflow-level error handling.

**Key Features Demonstrated**:
- Node retry configuration
- Error recovery strategies
- Conditional execution based on errors
- Custom error handling middleware

**How to Run**:
```bash
cd examples/error_handling
go run main.go
```

### DAG Patterns

**Location**: `examples/dag_patterns`

Demonstrates different patterns for constructing DAGs (Directed Acyclic Graphs) for various workflow scenarios.

**Key Features Demonstrated**:
- Sequential workflows
- Parallel execution
- Fan-out/fan-in patterns
- Conditional branches
- Dynamic DAG construction

**How to Run**:
```bash
cd examples/dag_patterns
go run main.go
```

### Comprehensive Example

**Location**: `examples/comprehensive`

A complete example showcasing all features of the workflow system. This is the most complete example that demonstrates the full capabilities of Flow Orchestrator.

**Key Features Demonstrated**:
- Complex workflow definition
- Custom middleware
- Persistence configuration
- Advanced error handling
- Performance optimization
- Observability features

**How to Run**:
```bash
cd examples/comprehensive
go run main.go
```

### Observability (OpenTelemetry)

**Location**: `examples/observability`

A runnable, self-contained example of wiring the engine's metrics to a real
OpenTelemetry SDK via the API-only bridge. It is a **separate Go module** (its own
`go.mod` with a `replace` directive back to the library) so the OTel SDK never
enters the library's own dependency graph. It uses a deterministic, network-free
pipeline (a `ManualReader` feeding a `stdoutmetric` exporter), so it is safe to
run in CI. See the [Observability guide](../guides/observability.md) for the
instrument inventory and the production (OTLP) wiring.

**Key Features Demonstrated**:
- Host-owned OTel SDK / `MeterProvider` (the library stays API-only)
- Enabling the library's metrics via `WithMetricsConfig`
- Bridging the collector with `metrics.NewOTelBridge`
- Manual collection + export of the `flow_orchestrator.operation.*` instruments

**How to Run**:
```bash
cd examples/observability
go run .
```

## Adapting Examples for Your Use Case

The examples are designed to be starting points for your own applications. Here's how you can adapt them:

1. **Copy and Modify**: Copy the relevant example to your project and modify it to suit your needs
2. **Extract Patterns**: Extract specific patterns or techniques from the examples
3. **Combine Features**: Combine features from different examples

## Example Workflow Patterns

The examples demonstrate several common workflow patterns:

### Sequential Workflow

Nodes execute in a strict sequence:
```
A → B → C → D
```

### Parallel Workflow

Multiple branches execute in parallel:
```
    ┌→ B →┐
A → ├→ C →┤ → E
    └→ D →┘
```

### Conditional Workflow

Execution path depends on conditions:
```
    ┌→ B1 →┐
A → ├      ┤ → D
    └→ B2 →┘
```

### Error Recovery Workflow

Includes fallback paths for error recovery:
```
    ┌→ B → C →┐
A →  │         ├→ E
    └→ D[fallback] →┘
```

## Additional Resources

- See the [Getting Started](../getting-started/) section for basic usage
- Explore the [Guides](../guides/) section for more detailed information on specific features
- Check the [API Reference](./api-reference.md) for detailed API documentation 