# Testing Scripts

This directory contains scripts for running tests and checking code coverage.

## Coverage Strategy

Flow Orchestrator follows a tiered approach to test coverage, with different thresholds for different package types:

| Package | Minimum Coverage | Priority | Notes |
|---------|-----------------|----------|-------|
| pkg/workflow | 90% | Critical | Public API - must be well-tested |
| internal/workflow/arena | 90% | High | Memory management is critical |
| internal/workflow/memory | 90% | High | Memory utilities are critical |
| internal/workflow/metrics | 70% | Medium | Metrics collection |
| internal/workflow/utils | 70% | Medium | Utility functions |
| internal/workflow/concurrent | 80% | Medium | Concurrency utilities |
| pkg/workflow/fb | 0% | Low | Generated code, no testing needed |
| internal/workflow/benchmark | 0% | Low | Benchmarks are tests themselves |
| examples/ | 0% | Low | Examples are for demonstration |

## Scripts

### check_coverage.sh

This script checks if each package meets its coverage threshold. It:

1. Runs tests for each package with coverage enabled
2. Extracts the coverage percentage
3. Compares it with the threshold
4. Reports success or failure

Usage:
```bash
./check_coverage.sh
```

### run_tests.sh

This script runs tests for the entire project with various options.

Usage:
```bash
# Run all tests
./run_tests.sh

# Run tests with race detection
./run_tests.sh --race

# Run tests with coverage
./run_tests.sh --coverage

# Run tests for a specific package
./run_tests.sh --package=pkg/workflow
```

### run_property_tests.sh

This script runs all property-based tests in the project. Property-based tests verify that core components satisfy fundamental invariants across a wide range of inputs.

Usage:
```bash
# Run all property tests
./run_property_tests.sh
```

The script runs property tests for:
- Workflow Engine (dependency execution order, deterministic execution, etc.)
- Memory Management (Arena, String Pool, Buffer Pool, Node Pool)
- Concurrent Data Structures (Concurrent Map, Read Map)

## Codecov Integration

We use Codecov to track coverage over time. Our configuration:

1. Ignores low-priority packages (examples, generated code, benchmarks)
2. Sets different coverage targets for different package priorities
3. Uses flags to categorize packages by priority level

The configuration is in the root `codecov.yml` file.

## Make Targets

Several make targets are available for working with coverage:

- `make test-coverage`: Run tests with coverage for all packages
- `make test-coverage-focused`: Run tests with coverage excluding examples and generated code
- `make codecov-coverage`: Generate coverage report specifically for codecov
- `make coverage-report`: Generate a focused coverage report
- `make check-coverage`: Check if coverage meets thresholds
- `make coverage-improvement`: Generate a report showing which files need coverage improvement
- `make property-tests`: Run all property-based tests 