# Troubleshooting Guide

This guide covers common issues you might encounter when developing with Flow Orchestrator and their solutions.

## Common Issues

### Workflow Execution Problems

#### Workflow Hangs or Never Completes

**Symptoms**: A workflow starts executing but never completes or appears to hang.

**Possible Causes and Solutions**:

1. **Deadlock in the DAG**: Check for circular dependencies between nodes.
   ```bash
   # Validate your DAG
   err := dag.Validate()
   if err != nil {
       log.Fatalf("DAG validation failed: %v", err)
   }
   ```

2. **Context Without Timeout**: Ensure you've provided a context with timeout.
   ```go
   ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
   defer cancel()
   err := dag.Execute(ctx, data)
   ```

3. **Blocking Action**: An action may be blocked on I/O or waiting for a resource.
   - Add more detailed logging to your actions
   - Set appropriate timeouts for I/O operations

#### Workflow Fails with Errors

**Symptoms**: Workflow execution returns an error or fails unexpectedly.

**Possible Causes and Solutions**:

1. **Node Execution Error**: Check the error message and node status.
   ```go
   if err != nil {
       // Check which node failed
       for _, nodeName := range dag.GetNodeNames() {
           status, _ := data.GetNodeStatus(nodeName)
           log.Printf("Node %s: %s", nodeName, status)
       }
   }
   ```

2. **Dependency Not Met**: A node's dependencies were not satisfied.
   - Ensure all dependencies are correctly defined
   - Check that dependent nodes are completing successfully

### Memory and Performance Issues

#### High Memory Usage

**Symptoms**: Application consumes excessive memory during workflow execution.

**Possible Causes and Solutions**:

1. **Large Workflow Data**: Check if you're storing large amounts of data in the workflow.
   - Only store necessary data in the WorkflowData
   - Consider using references instead of copying large objects

2. **Memory Leaks**: Check for potential memory leaks.
   - Ensure you're releasing resources properly
   - Use the arena allocator for temporary objects

#### Performance Degradation

**Symptoms**: Workflows execute more slowly than expected.

**Possible Causes and Solutions**:

1. **Insufficient Concurrency**: Check your concurrency settings.
   ```go
   // Set maximum concurrency
   workflow.Options.MaxConcurrency = runtime.NumCPU()
   ```

2. **Inefficient Actions**: Actions may be inefficient.
   - Profile your application to identify bottlenecks
   - Optimize expensive operations

### Dependency Issues

#### Missing Dependencies

**Symptoms**: Compilation errors mentioning missing packages.

**Possible Causes and Solutions**:

1. **Go Modules Issues**: Ensure your Go modules are properly set up.
   ```bash
   # Tidy up dependencies
   go mod tidy
   
   # Update dependencies
   go get -u ./...
   ```

2. **Indirect Dependencies**: Check for indirect dependencies.
   ```bash
   # Check module dependencies
   go mod graph
   ```

## Debugging Techniques

### Enable Debug Logging

```go
// Set log level to debug
options := workflow.WorkflowOptions{
    LogLevel: workflow.LogLevelDebug,
}

// Create a workflow with the options
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "debug-workflow",
    Options:    options,
}
```

### Use Middleware for Debugging

```go
// Add logging middleware
stack := workflow.NewMiddlewareStack()
stack.Use(workflow.LoggingMiddleware(
    workflow.WithLogLevel(workflow.LogLevelDebug),
))

// Apply middleware to actions
action = stack.Apply(action)
```

### Visualize the DAG

```go
// Print the DAG structure
dag.PrintGraph()

// Or create a DOT representation for visualization
dot := dag.ToDOT()
fmt.Println(dot)
```

## When to Seek Help

If you've tried the solutions above and are still experiencing issues:

1. Check the [GitHub Issues](https://github.com/ppcavalcante/flow-orchestrator/issues) to see if others have encountered similar problems
2. Search the documentation for relevant information
3. Create a new issue with:
   - A clear description of the problem
   - Steps to reproduce
   - Expected vs. actual behavior
   - Relevant logs and error messages
   - Your environment details (Go version, OS, etc.) 