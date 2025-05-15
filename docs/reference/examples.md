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

An example showing how to integrate the workflow system with a REST API. It creates a simple HTTP server that accepts workflow requests and executes them.

**Key Features Demonstrated**:
- HTTP API integration
- Asynchronous workflow execution
- Status reporting via API
- Error handling in API context

**How to Run**:
```bash
cd examples/api_workflow
go run main.go
```

**API Endpoints**:
- `POST /workflows` - Create and start a new workflow
- `GET /workflows/{id}` - Get workflow status
- `GET /workflows` - List all workflows

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