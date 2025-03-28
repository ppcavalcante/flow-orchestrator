# Flow Orchestrator Scripts

This directory contains scripts used for testing, profiling, and maintaining the Flow Orchestrator project.

## Directory Structure

```
scripts/
├── testing/         # Scripts for running tests
├── profiling/       # Scripts for performance benchmarking and visualization
└── tools/           # Utility scripts for development
```

## Testing Scripts 

### `testing/run_tests.sh`

A comprehensive testing script that:
- Builds the project
- Runs unit tests
- Optionally generates code coverage reports
- Cleans up temporary files after testing

**Usage:**
```bash
# Run basic tests
./scripts/testing/run_tests.sh

# Run tests with coverage reporting
./scripts/testing/run_tests.sh -coverage

# Generate FlatBuffers code and run tests
./scripts/testing/run_tests.sh -flatbuffers
```

## Profiling Scripts

### `profiling/run_benchmarks.sh`

Runs comprehensive benchmark tests across all aspects of the workflow system:
- **Real-world scenario benchmarks**: Tests complete workflow patterns that mimic real use cases
- **Scalability benchmarks**: Tests performance with different node counts, worker pool sizes, and DAG topologies
- **Error handling benchmarks**: Tests performance of retry mechanisms, recovery patterns, and failure handling
- **Storage benchmarks**: Tests and compares performance of different persistence backends
- **Core component benchmarks**: Tests individual components like workflow data operations, DAG operations, etc.

Results are saved to the `benchmark_results` directory, including:
- HTML benchmark summary with charts
- Raw benchmark results
- CPU profiles
- Memory profiles
- Trace files
- Allocation reports
- Performance recommendations

**Usage:**
```bash
./scripts/profiling/run_benchmarks.sh
```

**Outputs:**
```
benchmark_results/
├── benchmark_summary.md    # Markdown summary with tables and recommendations
├── *-results.txt           # Raw benchmark result files
├── profiles/               # Raw profile data
│   ├── *-cpu.prof          # CPU profiles
│   ├── *-mem.prof          # Memory profiles
│   └── *-trace.out         # Execution traces
└── reports/                # Detailed analysis reports
    ├── *-cpu.txt           # CPU hotspot reports
    ├── *-mem-allocs.txt    # Memory allocation reports
    └── *-mem-objects.txt   # Object allocation reports
```

### `profiling/visualize_profiles.sh`

Generates visualizations from profile data:
- SVG call graphs
- Interactive flame graphs
- PNG dot graphs
- Text-based reports

Requires the `graphviz` package to be installed.

**Usage:**
```bash
./scripts/profiling/visualize_profiles.sh
```

## Tool Scripts

### `tools/check_flatbuffers.sh`

Validates that the FlatBuffers environment is properly set up:
- Checks if the FlatBuffers compiler (`flatc`) is installed
- Validates the schema file
- Ensures output directories exist

**Usage:**
```bash
./scripts/tools/check_flatbuffers.sh
```

## Using Scripts with Make

The scripts are integrated with Make for easier usage:

```bash
# Run tests
make test

# Run tests with coverage
make test-coverage

# Run benchmarks
make run-benchmarks

# Visualize profile data
make visualize-profiles

# Check FlatBuffers installation
make check-flatbuffers

# Generate FlatBuffers code (runs check-flatbuffers first)
make generate-fb
``` 