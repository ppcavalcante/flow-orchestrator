# Flow Orchestrator Guides

Welcome to the Flow Orchestrator Guides section. These guides provide in-depth information on specific features and use cases, helping you get the most out of Flow Orchestrator.

## Available Guides

### Core Features

- [**Middleware System**](./middleware.md) - Using and creating middleware for cross-cutting concerns
- [**Persistence Layer**](./persistence.md) - Saving and resuming workflow state
- [**Performance Optimization**](./performance-optimization.md) - Optimizing memory and processing performance

### Usage Patterns

- [**Workflow Patterns**](./workflow-patterns.md) - Common patterns and best practices for workflow design
- [**Error Handling**](./error-handling.md) - Strategies for handling errors in workflows
- [**Benchmarks**](./benchmarks.md) - Performance benchmarks and analysis

## Recommended Reading Order

If you're new to Flow Orchestrator, we recommend reading the guides in this order:

1. **Workflow Patterns** - Learn common workflow structures and how to implement them
2. **Middleware System** - Understand how to add behaviors like logging, retries, and timeouts
3. **Error Handling** - Learn strategies for dealing with failures
4. **Persistence Layer** - Understand how to make your workflows durable
5. **Performance Optimization** - Learn how to optimize workflows for high performance

## Most Common Tasks

Here are quick references for common tasks:

### Adding Retry Capability

```go
// Using middleware
action := workflow.RetryMiddleware(3, time.Second)(myAction)

// Using node configuration
builder.AddNode("my-node").
    WithAction(myAction).
    WithRetries(3)
```

### Persisting Workflow State

```go
// Create a store
store, err := workflow.NewFlatBuffersStore("./workflow_data")
if err != nil {
    log.Fatalf("Failed to create store: %v", err)
}

// Create workflow with the store
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "my-workflow",
    Store:      store,
}

// Execute with persistence
err = workflow.Execute(context.Background())
```

### Adding Logging

```go
// Create a logging middleware
loggingMiddleware := workflow.LoggingMiddleware()

// Apply to an action
action := loggingMiddleware(myAction)
```

### Creating a Composite Action

```go
// Create a composite action from multiple actions
compositeAction := workflow.CompositeAction([]workflow.Action{
    validateAction,
    processAction,
    saveAction,
})
```

### Conditional Execution

```go
// Create a conditional action
conditionalAction := func(ctx context.Context, data *workflow.WorkflowData) error {
    shouldExecute, _ := data.GetBool("should_execute")
    if shouldExecute {
        return executeAction.Execute(ctx, data)
    }
    return nil
}
```

## Advanced Topics

For more advanced usage, check out these sections:

- [Memory Management](./performance-optimization.md#memory-management) - Learn about the arena allocator and string interning
- [Concurrent Execution](./performance-optimization.md#concurrent-data-structures) - Optimize workflow execution with parallelism
- [Custom Middleware](./middleware.md#creating-custom-middleware) - Create your own middleware for specialized needs
- [Custom Storage](./persistence.md#creating-custom-storage) - Implement your own storage backends 