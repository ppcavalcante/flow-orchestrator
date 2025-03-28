# API Reference

This document provides a comprehensive reference for the public API of Flow Orchestrator, located in the `pkg/workflow` package. All functionality needed to define and execute workflows is available through this public API.

> **Note**: Implementation details in the `internal/workflow` packages are not part of the public API and should not be imported directly.

## Import Path

```go
import "github.com/yourusername/flow-orchestrator/pkg/workflow"
```

## Core Types

### Workflow

The top-level container for a workflow execution:

```go
type Workflow struct {
    DAG        *DAG
    WorkflowID string
    Store      WorkflowStore
}
```

#### Methods

- `NewWorkflow(store WorkflowStore) *Workflow`: Creates a new workflow
- `AddNode(node *Node) error`: Adds a node to the workflow
- `AddDependency(from, to string) error`: Adds a dependency between nodes
- `WithWorkflowID(id string) *Workflow`: Sets the workflow ID
- `Execute(ctx context.Context) error`: Executes the workflow

### DAG

Represents a Directed Acyclic Graph structure for workflows:

```go
type DAG struct {
    Nodes      map[string]*Node
    StartNodes []*Node
    EndNodes   []*Node
    Name       string
}
```

#### Methods

- `NewDAG(name string) *DAG`: Creates a new DAG
- `AddNode(node *Node) error`: Adds a node to the DAG
- `AddDependency(fromNode, toNode string) error`: Adds a dependency between nodes
- `Validate() error`: Validates the DAG structure
- `Execute(ctx context.Context, data *WorkflowData) error`: Executes the DAG

## Table of Contents

- [Builder API](#builder-api) - The recommended way to create and configure workflows
- [Workflow Execution](#workflow-execution) - Running and managing workflows
- [Middleware](#middleware) - Extending workflow behavior
- [WorkflowData](#workflowdata) - Managing data during workflow execution
- [WorkflowStore](#workflowstore) - Persisting workflow state
- [Low-Level API Components](#low-level-api-components) - For advanced customization scenarios

## Builder API

The Builder API is the recommended way to create and configure workflows. It provides a fluent interface for defining workflows and their components.

### WorkflowBuilder

The `WorkflowBuilder` is the main entry point for creating workflows.

#### Creating a Builder

```go
// Create a new workflow builder
builder := workflow.NewWorkflowBuilder()

// Configure the builder
builder.WithWorkflowID("my-workflow")
       .WithStore(store)
```

#### Methods

##### `NewWorkflowBuilder() *WorkflowBuilder`

Creates a new workflow builder.

```go
builder := workflow.NewWorkflowBuilder()
```

##### `WithWorkflowID(id string) *WorkflowBuilder`

Sets the workflow ID.

```go
builder.WithWorkflowID("data-processing-workflow")
```

##### `WithStore(store WorkflowStore) *WorkflowBuilder`

Sets the workflow store for persistence.

```go
store := workflow.NewInMemoryStore()
builder.WithStore(store)
```

##### `AddNode(name string) *NodeBuilder`

Adds a node to the workflow and returns a node builder for further configuration.

```go
nodeBuilder := builder.AddNode("process-data")
```

##### `AddStartNode(name string) *NodeBuilder`

Adds a starting node to the workflow (no dependencies) and returns a node builder.

```go
nodeBuilder := builder.AddStartNode("fetch-data")
```

##### `Build() (*DAG, error)`

Builds the workflow DAG, validating the structure and detecting cycles.

```go
dag, err := builder.Build()
if err != nil {
    // Handle error
}
```

### NodeBuilder

The `NodeBuilder` allows configuring individual nodes in the workflow.

#### Methods

##### `WithAction(action interface{}) *NodeBuilder`

Sets the action for the node. The action can be an `Action` interface or a function with the signature `func(ctx context.Context, data *WorkflowData) error`.

```go
nodeBuilder.WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
    // Node implementation
    return nil
})
```

##### `WithRetries(count int) *NodeBuilder`

Sets the number of retries for the node.

```go
nodeBuilder.WithRetries(3)
```

##### `WithTimeout(timeout time.Duration) *NodeBuilder`

Sets a timeout for the node execution.

```go
nodeBuilder.WithTimeout(30 * time.Second)
```

##### `DependsOn(deps ...string) *NodeBuilder`

Specifies dependencies for this node by name.

```go
nodeBuilder.DependsOn("fetch-data", "validate-data")
```

### Complete Example

```go
// Create a workflow using the builder API
builder := workflow.NewWorkflowBuilder().
    WithWorkflowID("data-processing").
    WithStore(workflow.NewInMemoryStore())

// Add nodes with the fluent API
builder.AddStartNode("fetch").
    WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
        // Fetch data implementation
        return nil
    }).
    WithRetries(3).
    WithTimeout(10 * time.Second)

builder.AddNode("process").
    WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
        // Process data implementation
        return nil
    }).
    DependsOn("fetch").
    WithRetries(2)

builder.AddNode("save").
    WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
        // Save data implementation
        return nil
    }).
    DependsOn("process")

// Build the workflow
workflow, err := workflow.FromBuilder(builder)
if err != nil {
    // Handle error
}

// Execute the workflow
err = workflow.Execute(context.Background())
```

## Workflow Execution

### Workflow

The `Workflow` struct represents a complete workflow that can be executed.

#### Methods

##### `NewWorkflow(store WorkflowStore) *Workflow`

Creates a new workflow with the given workflow store.

```go
store := workflow.NewInMemoryStore()
wf := workflow.NewWorkflow(store)
```

##### `WithWorkflowID(id string) *Workflow`

Sets the workflow ID.

```go
wf.WithWorkflowID("my-workflow")
```

##### `Execute(ctx context.Context) error`

Executes the workflow.

```go
err := wf.Execute(context.Background())
```

##### `FromBuilder(builder *WorkflowBuilder) (*Workflow, error)`

Creates a workflow from a builder.

```go
workflow, err := workflow.FromBuilder(builder)
```

## Middleware

Middleware allows extending the behavior of workflow actions.

### Built-in Middleware

#### `LoggingMiddleware() Middleware`

Adds logging to actions.

```go
builder.AddNode("process").
    WithAction(workflow.LoggingMiddleware()(myAction))
```

#### `TimeoutMiddleware(timeout time.Duration) Middleware`

Adds a timeout to actions.

```go
builder.AddNode("process").
    WithAction(workflow.TimeoutMiddleware(5 * time.Second)(myAction))
```

#### `RetryMiddleware(maxRetries int, backoff time.Duration) Middleware`

Adds retry behavior to actions.

```go
builder.AddNode("process").
    WithAction(workflow.RetryMiddleware(3, 1 * time.Second)(myAction))
```

#### `MetricsMiddleware() Middleware`

Adds metrics collection to actions.

```go
builder.AddNode("process").
    WithAction(workflow.MetricsMiddleware()(myAction))
```

#### `ValidationMiddleware(validator func(*WorkflowData) error) Middleware`

Adds validation to actions.

```go
validator := func(data *workflow.WorkflowData) error {
    // Validation logic
    return nil
}
builder.AddNode("process").
    WithAction(workflow.ValidationMiddleware(validator)(myAction))
```

#### `ConditionalRetryMiddleware(maxRetries int, backoff time.Duration, predicate func(error) bool) Middleware`

Adds conditional retry behavior to actions.

```go
shouldRetry := func(err error) bool {
    // Retry logic
    return true
}
builder.AddNode("process").
    WithAction(workflow.ConditionalRetryMiddleware(3, 1 * time.Second, shouldRetry)(myAction))
```

### Creating Custom Middleware

```go
// Create a custom middleware
func CustomMiddleware() workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Pre-processing
            err := next.Execute(ctx, data)
            // Post-processing
            return err
        })
    }
}

// Use the custom middleware
builder.AddNode("process").
    WithAction(CustomMiddleware()(myAction))
```

## WorkflowData

`WorkflowData` manages data during workflow execution.

### Methods

#### `NewWorkflowData(id string) *WorkflowData`

Creates a new workflow data instance.

```go
data := workflow.NewWorkflowData("my-workflow")
```

#### `Set(key string, value interface{})`

Sets a value in the workflow data.

```go
data.Set("user", user)
```

#### `Get(key string) (interface{}, bool)`

Gets a value from the workflow data.

```go
value, exists := data.Get("user")
```

#### `GetString(key string) (string, bool)`

Gets a string value from the workflow data.

```go
value, exists := data.GetString("username")
```

#### `GetInt(key string) (int, bool)`

Gets an int value from the workflow data.

```go
value, exists := data.GetInt("count")
```

#### `GetFloat64(key string) (float64, bool)`

Gets a float64 value from the workflow data.

```go
value, exists := data.GetFloat64("price")
```

#### `GetBool(key string) (bool, bool)`

Gets a bool value from the workflow data.

```go
value, exists := data.GetBool("isValid")
```

#### `SetOutput(nodeName string, output interface{})`

Sets the output of a node.

```go
data.SetOutput("fetch", result)
```

#### `GetOutput(nodeName string) (interface{}, bool)`

Gets the output of a node.

```go
output, exists := data.GetOutput("fetch")
```

## WorkflowStore

`WorkflowStore` persists workflow state.

### Interface

```go
type WorkflowStore interface {
    // Save stores the workflow data
    Save(data *WorkflowData) error

    // Load retrieves workflow data by ID
    Load(workflowID string) (*WorkflowData, error)

    // ListWorkflows returns all workflow IDs
    ListWorkflows() ([]string, error)

    // Delete removes a workflow
    Delete(workflowID string) error
}
```

### Implementations

#### `NewInMemoryStore() *InMemoryStore`

Creates an in-memory workflow store.

```go
store := workflow.NewInMemoryStore()
```

#### `NewJSONFileStore(baseDir string) (*JSONFileStore, error)`

Creates a JSON file-based workflow store.

```go
store, err := workflow.NewJSONFileStore("./workflow_data")
```

#### `NewFlatBuffersStore(baseDir string) (*FlatBuffersStore, error)`

Creates a FlatBuffers-based workflow store for high-performance serialization.

```go
store, err := workflow.NewFlatBuffersStore("./workflow_data")
```

## Low-Level API Components

The following components are part of the public API but are typically accessed through the higher-level Builder API. They are provided for advanced customization scenarios.

### DAG (Directed Acyclic Graph)

The `DAG` struct represents the workflow as a directed acyclic graph.

### Node

The `Node` struct represents a single unit of work in the workflow.

### Action

The `Action` interface represents executable work.

```go
type Action interface {
    Execute(ctx context.Context, data *WorkflowData) error
}
```

#### Action Implementations

- `ActionFunc` - Function-based action
- `CompositeAction` - Combines multiple actions
- `ValidationAction` - Validates input data
- `MapAction` - Transforms data
- `RetryableAction` - Adds retry behavior

### Memory Management

Advanced memory management utilities for high-performance scenarios.

#### Arena Allocation

```go
data := workflow.NewWorkflowDataWithArena("my-workflow", config)
```

#### String Interning

```go
data := workflow.NewWorkflowDataWithConfig("my-workflow", config)
```

### Metrics

Metrics collection tools accessible through the public API.

```go
// Get a metrics configuration
config := workflow.DefaultMetricsConfig()
workflow.SetMetricsConfig(config)

// Get metrics
stats := workflow.GetOperationStats()
``` 