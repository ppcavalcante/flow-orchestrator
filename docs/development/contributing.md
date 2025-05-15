# Contributing to Flow Orchestrator

Thank you for your interest in contributing to Flow Orchestrator! This document provides guidelines and instructions for contributing to the project.

## Code of Conduct

Please read and follow our [Code of Conduct](../../CODE_OF_CONDUCT.md).

## How to Contribute

### Reporting Bugs

1. Ensure the bug has not already been reported by searching on GitHub under [Issues](https://github.com/pparaujo/flow-orchestrator/issues)
2. If you're unable to find an open issue addressing the problem, [open a new one](https://github.com/pparaujo/flow-orchestrator/issues/new)
3. Include a clear title and description, as much relevant information as possible, and a code sample or an executable test case demonstrating the expected behavior that is not occurring

### Suggesting Enhancements

1. [Open a new issue](https://github.com/pparaujo/flow-orchestrator/issues/new)
2. Provide a clear title and description of the enhancement
3. Include any relevant examples or use cases

### Pull Requests

1. Fork the repository
2. Create a new branch from `develop`: `git checkout -b feature/your-feature-name develop`
3. Make your changes
4. Ensure your code follows the project's style guidelines
5. Ensure all tests pass: `go test ./...`
6. Update documentation as needed
7. Commit your changes following [conventional commit](https://www.conventionalcommits.org/) format
8. Push to your branch
9. Open a pull request against the `develop` branch

## Development Workflow

Please refer to our [Git Workflow](../gw.md) documentation for details on our branching strategy and release process.

## Code Guidelines

### Go Code Style

1. Follow the standard Go code style
2. Use `gofmt` to format your code
3. Ensure your code passes `go vet` and `golint`
4. Add appropriate comments for exported functions, types, and constants

### Documentation

1. Update documentation for any changes to the public API
2. Document new features, configuration options, and behavior changes
3. Include examples for new functionality

### Testing

1. Write tests for all new features and bug fixes
2. Ensure all tests pass before submitting a pull request
3. Aim for high test coverage, especially for critical components
4. See our [Test Coverage Strategy](../test_coverage_strategy.md) for details

## Getting Help

If you need help with contributing, please:

1. Check the [documentation](https://github.com/pparaujo/flow-orchestrator/docs)
2. Ask in the [issue tracker](https://github.com/pparaujo/flow-orchestrator/issues)

## License

By contributing to Flow Orchestrator, you agree that your contributions will be licensed under the project's [MIT License](../../LICENSE). 