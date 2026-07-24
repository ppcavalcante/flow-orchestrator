# Getting Started with Flow Orchestrator

This section provides everything you need to start using Flow Orchestrator in your applications. Whether you're new to workflow engines or an experienced developer, these guides will help you get up and running quickly.

## Quick Navigation

- [**Quickstart Guide**](./quickstart.md): Get up and running with Flow Orchestrator in 5 minutes
- [**Installation**](./installation.md): Detailed installation instructions for different environments
- [**Basic Concepts**](./basic-concepts.md): Learn about the core concepts and terminology
- [**Your First Workflow**](./first-workflow.md): Step-by-step guide to creating your first workflow

## Recommended Learning Path

If you're new to Flow Orchestrator, we recommend following this learning path:

1. Start with the [Quickstart Guide](./quickstart.md) to get a feel for the library
2. Read [Basic Concepts](./basic-concepts.md) to understand the key concepts
3. Follow the [Your First Workflow](./first-workflow.md) tutorial for a hands-on experience
4. Explore the [Guides](../guides/) section for more advanced topics

## System Requirements

- Go 1.25 or higher
- A small set of runtime dependencies: `google/flatbuffers` (persistence) and
  `go.opentelemetry.io/otel/metric` (the optional metrics bridge API); plus
  test-only deps (`leanovate/gopter`, `stretchr/testify`, the OTel SDK). All are
  resolved automatically by `go get`.
- Works on all platforms supported by Go

## Version Compatibility

| Flow Orchestrator Version | Go Version |
|---------------------------|------------|
| `v0.20.0-alpha` (latest release tag; `@latest` resolves here) | 1.25+ |

All published tags are pre-releases; there is no stable (`v1`+) release yet.

## Support

If you encounter any issues, please check:

1. The [Guides](../guides/) section for detailed information on specific topics
2. The [API Reference](../reference/api-reference.md) for detailed API documentation
3. The [examples directory](../../examples/) for working code examples

## What's Next?

After getting started, you may want to explore:

- [Middleware System](../guides/middleware.md) to add cross-cutting concerns
- [Persistence Layer](../guides/persistence.md) to save and restore workflows
- [Workflow Patterns](../guides/workflow-patterns.md) for common patterns and best practices 