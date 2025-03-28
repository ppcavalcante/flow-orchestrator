# API Workflow Testing Guide

This document provides instructions for testing the API workflow system under various conditions to verify its robustness and functionality.

## Test Scenarios Overview

The `test_scenarios.sh` script runs through multiple scenarios designed to test different aspects of the workflow system:

1. **Basic Workflow Success**: Verifies that the workflow executes correctly with minimal failures.
2. **Fetch Retries**: Tests automatic retries when the fetch operation fails.
3. **API Retries**: Tests automatic retries when the API operation fails.
4. **DB Retries**: Tests automatic retries when the database operation fails.
5. **Process Failure**: Tests system behavior when a non-retryable operation fails.
6. **High Failure Rates**: Simulates an environment with multiple failures to test resilience.
7. **Slow API Simulation**: Tests the system's behavior with slow external dependencies.
8. **Workflow Resumption**: Tests the ability to resume a workflow from a saved state.
9. **Cascading Failures**: Tests how failures propagate through the workflow.
10. **Multiple Layer Failure Recovery**: Tests recovery when multiple components fail.
11. **Custom Worker Pool Configuration**: Tests execution with a custom worker pool size and queue size.
12. **Cancellation Test (Simulated)**: Instructions for testing workflow cancellation.

## Prerequisites

- Go 1.15 or higher
- Bash shell

## Running the Tests

1. Make the test script executable:
   ```bash
   chmod +x test_scenarios.sh
   ```

2. Run the test script from the `examples/api_workflow` directory:
   ```bash
   ./test_scenarios.sh
   ```

3. Each test scenario will run one after another, with a prompt to continue between tests.

## Interpreting Test Results

- ✅ Green output indicates successful operations
- ❌ Red output indicates failures
- 🔄 Retry operations are shown in detail when `--verbose-retries` is enabled

## Command-Line Flags

The API workflow example supports various flags to simulate different conditions:

| Flag | Description |
|------|-------------|
| `--fail-fetch` | Force the fetch operation to fail |
| `--fail-process` | Force the process operation to fail |
| `--fail-api` | Force the API operation to fail |
| `--fail-db` | Force the DB operation to fail |
| `--fetch-failure-rate=RATE` | Set the failure rate for fetch (0.0-1.0) |
| `--api-failure-rate=RATE` | Set the failure rate for API calls (0.0-1.0) |
| `--db-failure-rate=RATE` | Set the failure rate for DB operations (0.0-1.0) |
| `--verbose-retries` | Show detailed retry information |
| `--resume=FILE` | Resume from a state file |
| `--slow-api` | Simulate slow API responses |
| `--custom-pool` | Use custom worker pool configuration |
| `--workers=N` | Set number of workers in the pool (default 3) |
| `--queue-size=N` | Set maximum queue size (default 50) |

## Manual Testing Suggestions

Beyond the automatic tests, you may want to manually test:

1. **Custom Failure Combinations**: Combine different failure modes to test more complex scenarios
   ```bash
   go run main.go --fail-fetch --fail-api --verbose-retries
   ```

2. **Varying Failure Rates**: Test with different failure rates to see how the system handles varying levels of reliability
   ```bash
   go run main.go --fetch-failure-rate=0.7 --api-failure-rate=0.4 --verbose-retries
   ```

3. **Cancellation Testing**: Start a slow workflow and send an interrupt signal
   ```bash
   go run main.go --slow-api
   # Then press Ctrl+C during execution
   ```

4. **State Inspection**: Examine the state file contents after a run
   ```bash
   go run main.go
   cat api_workflow_state.json | jq .  # requires jq for pretty printing
   ```

5. **Custom Worker Pool Configuration**: Test with different worker pool configurations
   ```bash
   go run main.go --custom-pool --workers=5 --queue-size=100 --slow-api
   ```

## Expected Behavior

1. **Retries**: Operations should automatically retry according to the configured retry count for each node.
2. **State Persistence**: The workflow state should be persisted to the file after each topological level.
3. **Resumption**: When resuming from a state file, already completed nodes should be skipped.
4. **Error Handling**: Failures should be properly captured and reported.
5. **Parallel Execution**: Nodes within the same topological level should execute in parallel.
6. **Worker Pool Scaling**: With custom worker pool configuration, the system should handle parallel execution according to the specified number of workers.

## Troubleshooting

- If tests fail unexpectedly, check the error messages for details on what failed.
- If state files aren't being generated, ensure the workflow has write permissions in the current directory.
- For resumption testing, verify that the state file exists and contains valid data.

## Advanced Testing

For more advanced testing scenarios, consider:

1. Implementing a stress test with thousands of records
2. Testing with actual external API and database connections
3. Adding monitoring to observe resource usage under different conditions
4. Testing workflow interruption and resumption across machine reboots 