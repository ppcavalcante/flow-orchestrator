# Installation Guide

This guide provides detailed instructions for installing Flow Orchestrator in various environments.

## Prerequisites

Flow Orchestrator requires:

- Go 1.24 or higher
- A working Go development environment

You can check your Go version with:

```bash
go version
```

## Installation Methods

### Using Go Modules (Recommended)

The recommended way to install Flow Orchestrator is using Go modules:

```bash
# Add to your project
go get github.com/ppcavalcante/flow-orchestrator@latest
```

In your `go.mod` file, you'll see a line like:

```
require github.com/ppcavalcante/flow-orchestrator v0.7.4-alpha
```

> **Versioning:** the project is **alpha** — every published tag is a pre-release and there is
> **no stable (`v1`+) release**. The latest is **`v0.7.4-alpha`** (the M1–M7 work: typed-int
> fidelity, untrusted-input bounds, CI gates, OpenTelemetry export, typed-key data +
> continue-on-error + formal verification). Because there is no stable tag, `go get @latest`
> resolves to the highest pre-release — currently **`v0.7.4-alpha`** — so the command above is
> correct. Pinning the exact version
> (`go get github.com/ppcavalcante/flow-orchestrator@v0.7.4-alpha`) is optional but recommended
> for reproducibility, and the API may change between alpha minors. The in-code version
> (`pkg/workflow.Version`) reads `0.7.4-alpha`. See
> [CHANGELOG.md](../../CHANGELOG.md) and [STABILITY.md](../../STABILITY.md).

### Using Traditional GOPATH

If you're using GOPATH without modules:

```bash
go get -u github.com/ppcavalcante/flow-orchestrator
```

### Building from Source

If you want to build from source:

```bash
# Clone the repository
git clone https://github.com/ppcavalcante/flow-orchestrator.git

# Navigate to the project directory
cd flow-orchestrator

# Build the project
make build

# Run tests
make test
```

## Import in Your Code

Once installed, you can import Flow Orchestrator in your Go code:

```go
import (
    "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)
```

## Verifying Installation

Create a simple program to verify your installation:

```go
package main

import (
    "fmt"
    
    "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

func main() {
    fmt.Printf("Flow Orchestrator version: %s\n", workflow.Version)
}
```

Save this to a file named `verify.go` and run it:

```bash
go run verify.go
```

You should see output indicating the version of Flow Orchestrator.

## Development Setup

For development work on Flow Orchestrator itself, we recommend:

1. Fork the repository on GitHub
2. Clone your fork locally
3. Set up the development environment:

```bash
# Fetch Go module dependencies (also done automatically by build/test)
go mod download

# Generate FlatBuffers code (requires flatc)
make generate-fb

# Run tests
make test

# Run linter
make lint
```

### Development Tools

Flow Orchestrator development requires:

- [golangci-lint](https://golangci-lint.run/usage/install/) for linting
- [FlatBuffers compiler](https://github.com/google/flatbuffers/releases) for generating FlatBuffers code

On macOS, you can install these tools using Homebrew:

```bash
brew install golangci-lint flatbuffers
```

## Dependency Management

Flow Orchestrator has minimal dependencies, but they are managed using Go modules. To update dependencies:

```bash
go get -u ./...
go mod tidy
```

## Troubleshooting

### Common Issues

**Issue**: Unable to find package "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"

**Solution**: Ensure your Go modules are properly set up and try running `go mod tidy`.

**Issue**: Build errors related to FlatBuffers

**Solution**: Run `make generate-fb` to generate the required FlatBuffers code.

**Issue**: Version conflicts with other libraries

**Solution**: Use Go modules' version constraints to resolve conflicts:

```
require (
    github.com/ppcavalcante/flow-orchestrator v0.7.4-alpha
    github.com/conflicting/package v1.2.3 // indirect
)

replace github.com/conflicting/package => github.com/conflicting/package v1.2.4
```

### Getting Help

If you encounter issues with installation, please:

1. Check the [examples directory](../../examples/) for working code examples
2. Review the [Troubleshooting Guide](../development/troubleshooting.md) for common issues
3. Check for [known issues](https://github.com/ppcavalcante/flow-orchestrator/issues) on GitHub

## Next Steps

Now that you have installed Flow Orchestrator, you can:

- Follow the [Quickstart Guide](./quickstart.md) to get started quickly
- Read [Basic Concepts](./basic-concepts.md) to understand the key concepts
- Try the [Your First Workflow](./first-workflow.md) tutorial for a hands-on experience 