# Flow Orchestrator Documentation

Welcome to the Flow Orchestrator documentation. This comprehensive guide will help you understand, use, and extend Flow Orchestrator for your workflow automation needs.

## Documentation Sections

### [Getting Started](./getting-started/)
Begin your journey with Flow Orchestrator with quick installation and basic usage guides:
- [Quickstart Guide](./getting-started/quickstart.md)
- [Installation](./getting-started/installation.md)
- [Basic Concepts](./getting-started/basic-concepts.md)
- [Your First Workflow](./getting-started/first-workflow.md)

### [Architecture](./architecture/)
Understand the internal design and components of Flow Orchestrator:
- [Architecture Overview](./architecture/overview.md)
- [DAG Execution Model](./architecture/dag-execution.md)
- [Workflow Engine](./architecture/component-workflow.md)
- [Architecture Decision Records](./architecture/adr/)

### [Guides](./guides/)
Dive deeper into specific topics with our comprehensive guides:
- [Workflow Patterns](./guides/workflow-patterns.md)
- [Error Handling](./guides/error-handling.md)
- [Middleware System](./guides/middleware.md)
- [Persistence Layer](./guides/persistence.md)
- [Performance Optimization](./guides/performance-optimization.md)
- [Benchmarks](./guides/benchmarks.md)

### [Reference](./reference/)
Detailed reference documentation for APIs, configuration options, and more:
- [API Reference](./reference/api-reference.md)
- [Configuration Options](./reference/configuration.md)
- [Examples](./reference/examples.md)

### [Development](./development/)
Information for contributors and developers working on Flow Orchestrator:
- [Contribution Guide](./development/contributing.md)
- [Development Environment Setup](./development/dev-environment.md)
- [Git Workflow](./gw.md)
- [Release Process](./development/release-process.md)
- [Test Coverage Strategy](./development/test_coverage_strategy.md)
- [Supply Chain Security](./development/supply_chain_security.md)
- [Troubleshooting](./development/troubleshooting.md)

### Project Information
General information about the project:
- [Changelog](../CHANGELOG.md)

## Getting Help

If you have questions or need help, please:
1. Check the documentation in this repository
2. Look for examples in the `examples/` directory
3. Open an issue on GitHub if you find a bug or have a feature request

## Contributing

Contributions to Flow Orchestrator are welcome! Please see our [Contributing Guide](./development/contributing.md) for details on how to get involved.

## Key Features

Flow Orchestrator is a lightweight, high-performance workflow orchestration engine for Go applications that need reliable execution of complex processes. It provides:

- **High Performance**: Optimized for minimal allocations and maximum throughput
- **Thread-Safe**: Built for concurrent access with minimal lock contention
- **Persistent**: Save and resume workflows across application restarts
- **Observable**: Comprehensive metrics for monitoring and optimization
- **Extensible**: Designed to support multiple orchestration patterns
- **Embeddable**: Clean API for integration into any Go application
- **Rigorously Tested**: Property-based testing ensures correctness across a wide range of inputs

## Status

Flow Orchestrator is currently in alpha status. While the core functionality is implemented and tested, the API may change before the stable release.

## Example Usage

```go
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
    WithAction(processPaymentAction).
    DependsOn("validate-order")

// Build and execute the workflow
dag, err := builder.Build()
if err != nil {
    log.Fatalf("Failed to build workflow: %v", err)
}

// Execute the workflow
err = dag.Execute(context.Background(), workflowData)
```

For more detailed examples, check out the [examples directory](../examples/) in the repository. 