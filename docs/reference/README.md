# Reference Documentation

This section provides detailed reference documentation for Flow Orchestrator, including API documentation, configuration options, and example applications.

## Contents

- [API Reference](./api-reference.md)
- [Configuration Options](./configuration.md)  
- [Examples](./examples.md)

## API Reference

The [API Reference](./api-reference.md) provides comprehensive documentation for the public API of Flow Orchestrator, including:

- Core Types
- Interfaces
- Functions
- Constants

## Configuration Options

The [Configuration Options](./configuration.md) document details all configuration options available in Flow Orchestrator, including:

- Workflow Options
- Middleware Options
- Persistence Options
- Memory Optimization Options
- Concurrency Options
- Observer Options

## Examples

The [Examples](./examples.md) document provides an overview of the example applications included with Flow Orchestrator, demonstrating:

- Simple workflow creation and execution
- API integration
- Error handling strategies
- Common DAG patterns
- Advanced features and optimizations

## Type Definitions

### Core Types

#### Workflow

The top-level container for a workflow execution:

```go
type Workflow struct {
    DAG        *DAG
    WorkflowID string
    Store      WorkflowStore
}
```

#### DAG (Directed Acyclic Graph)

Represents the structure of a workflow:

```go
type DAG struct {
    Nodes      map[string]*Node
    StartNodes []*Node
    EndNodes   []*Node
    Name       string
    CycleNodes []string
}
```

#### Node

A single unit of work in a workflow:

```go
type Node struct {
    Name       string
    Action     Action
    DependsOn  []*Node
    RetryCount int
    Timeout    time.Duration
}
```

#### Action

Interface for executable work:

```go
type Action interface {
    Execute(ctx context.Context, data *WorkflowData) error
}
```

#### WorkflowData

Shared data store for workflow execution:

```go
type WorkflowData struct {
    ID string
    // Internal fields omitted
}
```

#### WorkflowStore

Interface for workflow persistence:

```go
type WorkflowStore interface {
    Save(data *WorkflowData) error
    Load(workflowID string) (*WorkflowData, error)
    ListWorkflows() ([]string, error)
    Delete(workflowID string) error
}
```

#### Middleware

Function type for middleware:

```go
type Middleware func(Action) Action
```

### Status Types

#### NodeStatus

Possible status values for a workflow node:

```go
type NodeStatus string

const (
    Pending   NodeStatus = "pending"
    Running   NodeStatus = "running"
    Completed NodeStatus = "completed"
    Failed    NodeStatus = "failed"
    Skipped   NodeStatus = "skipped"
)
```

## Builder API

The WorkflowBuilder provides a fluent interface for defining workflows:

```go
// Create a builder
builder := workflow.NewWorkflowBuilder().
    WithWorkflowID("order-processing")

// Add nodes
builder.AddStartNode("validate-order").
    WithAction(validateOrderAction)

builder.AddNode("process-payment").
    WithAction(processPaymentAction).
    DependsOn("validate-order")

// Build the DAG
dag, err := builder.Build()
```

## Interfaces

Flow Orchestrator defines several key interfaces that can be implemented by users:

### Action Interface

```go
type Action interface {
    Execute(ctx context.Context, data *WorkflowData) error
}
```

### WorkflowStore Interface

```go
type WorkflowStore interface {
    Save(data *WorkflowData) error
    Load(workflowID string) (*WorkflowData, error)
    ListWorkflows() ([]string, error)
    Delete(workflowID string) error
}
```

## Version Information

Flow Orchestrator follows [Semantic Versioning](https://semver.org/). The current version information is available via:

```go
import "github.com/pparaujo/flow-orchestrator/pkg/workflow"

// Get version string
version := workflow.Version

// Get detailed version info
versionInfo := workflow.VersionInfo
fmt.Printf("Version: %d.%d.%d", 
    versionInfo.Major, 
    versionInfo.Minor, 
    versionInfo.Patch)
```

## Constants

Flow Orchestrator defines several constants that are available to users:

```go
// Node status constants
const (
    Pending   NodeStatus = "pending"
    Running   NodeStatus = "running"
    Completed NodeStatus = "completed"
    Failed    NodeStatus = "failed"
    Skipped   NodeStatus = "skipped"
)

// Default configuration values
const (
    DefaultArenaBlockSize = 64 * 1024 // 64KB
    DefaultParallelism    = 4         // Default parallel workers
)
```

## Further Reading

- [Getting Started](../getting-started/) - Learn the basics of using Flow Orchestrator
- [Guides](../guides/) - Detailed guides on specific features and use cases
- [Architecture](../architecture/) - Understand the internal design of Flow Orchestrator 