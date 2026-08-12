# Reference Documentation

This section provides detailed reference documentation for Flow Orchestrator, including API documentation, configuration options, and example applications.

## Contents

- [API Reference](./api-reference.md)
- [Configuration Options](./configuration.md)  
- [Examples](./examples.md)
- [Platform Support & Store Capability Matrix](./platform-support.md)

## API Reference

The [API Reference](./api-reference.md) provides comprehensive documentation for the public API of Flow Orchestrator, including:

- Core Types
- Interfaces
- Functions
- Constants

## Configuration Options

The [Configuration Options](./configuration.md) document details all configuration options available in Flow Orchestrator, including:

- Workflow Data configuration
- Middleware
- Persistence (stores)
- Memory optimization (arena, interning)
- Concurrency (`ExecutionConfig`)
- Metrics &amp; observability

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

The top-level container for a workflow execution. As of M23 (SEAL-01) the graph
is no longer an exported field; read it via the `w.DAG()` accessor. The exported
configuration fields remain:

```go
type Workflow struct {
    WorkflowID          string
    Store               WorkflowStore
    MaxSubWorkflowDepth int
    Clock               Clock
    Locker              Locker
    RollbackTimeout     time.Duration
    MetricsConfig       *metrics.Config
    // unexported fields (the graph, etc.)
}

// Read the graph:
func (w *Workflow) DAG() *DAG
```

#### DAG (Directed Acyclic Graph)

Represents the structure of a workflow. As of M23 (SEAL-01/06) `DAG` is an
**opaque handle** — every field is unexported. `Name` is now a method;
`StartNodes`/`EndNodes` were deleted. Read the graph through its accessors:

```go
type DAG struct {
    // unexported fields
}

func (d *DAG) Name() string
func (d *DAG) GetNode(name string) (*Node, bool)
func (d *DAG) GetLevels() [][]*Node
func (d *DAG) DefinitionDigest() string
func (d *DAG) Validate() error
func (d *DAG) Execute(ctx context.Context, data *WorkflowData) error
```

#### Node

A single unit of work in a workflow. As of M23 (SEAL-01/02) a `*Node` is an
**opaque handle** — every field is unexported and the post-Build mutators were
deleted, so a `*Node` obtained from `GetNode`/`GetLevels` is read-only. Read it
through its accessors:

```go
type Node struct {
    // unexported fields
}

func (n *Node) Name() string
func (n *Node) GetDependencies() []*Node
func (n *Node) HasDependency(nodeName string) bool
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
    Waiting   NodeStatus = "waiting"  // parked on an external event (timer/signal); non-terminal, non-failing (added v0.10.0)
    Bypassed  NodeStatus = "bypassed" // not-taken branch of a ChoiceNode; terminal, not a failure (added v0.11.0)
    Compensated        NodeStatus = "compensated"         // Completed node undone by its compensation in a saga rollback; terminal (added v0.12.0)
    CompensationFailed NodeStatus = "compensation_failed" // Completed node whose compensation was attempted and failed; terminal (added v0.12.0)
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
import "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"

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
// Node status constants (pkg/workflow/node.go)
const (
    Pending   NodeStatus = "pending"
    Running   NodeStatus = "running"
    Completed NodeStatus = "completed"
    Failed    NodeStatus = "failed"
    Skipped   NodeStatus = "skipped"
    Waiting   NodeStatus = "waiting"  // parked on an external event; non-terminal (added v0.10.0)
    Bypassed  NodeStatus = "bypassed" // not-taken branch of a ChoiceNode; terminal, not a failure (added v0.11.0)
    Compensated        NodeStatus = "compensated"         // Completed node undone by its compensation in a saga rollback; terminal (added v0.12.0)
    CompensationFailed NodeStatus = "compensation_failed" // Completed node whose compensation was attempted and failed; terminal (added v0.12.0)
)

// Default per-level execution concurrency (pkg/workflow/parallel_execution.go)
const DefaultMaxConcurrency = 16
```

## Further Reading

- [Getting Started](../getting-started/) - Learn the basics of using Flow Orchestrator
- [Guides](../guides/) - Detailed guides on specific features and use cases
- [Architecture](../architecture/) - Understand the internal design of Flow Orchestrator 