# Error Handling Example

This example demonstrates different error handling patterns that can be implemented using the workflow system. It showcases the system's capabilities for handling various types of errors and failure scenarios.

## Overview

The example includes five different error handling patterns:

1. **Retry Pattern**: Automatically retry failed operations with configurable retry counts.
   - Demonstrates how to handle transient failures by retrying operations.
   - Shows the difference between no retry, simple retry, and retry with backoff.

2. **Timeout Pattern**: Set timeouts for operations to prevent indefinite waiting.
   - Demonstrates how to handle operations that take too long.
   - Shows different timeout configurations and their effects.

3. **Conditional Error Pattern**: Handle errors differently based on input conditions.
   - Demonstrates how to handle errors based on specific conditions.
   - Shows how to validate inputs and handle different error scenarios.

4. **Fallback Pattern**: Provide fallback mechanisms when primary operations fail.
   - Demonstrates how to handle errors by providing alternative paths.
   - Shows how to implement primary and fallback operations.

5. **Circuit Breaker Pattern**: Prevent cascading failures by stopping operations after too many failures.
   - Demonstrates how to handle errors by breaking the circuit after a threshold.
   - Shows how to implement a circuit breaker to prevent further failures.

## Features Demonstrated

- **Retry Mechanisms**: Configurable retry counts for transient failures
- **Timeout Handling**: Setting timeouts to prevent indefinite waiting
- **Conditional Error Handling**: Handling errors based on specific conditions
- **Fallback Mechanisms**: Providing alternative paths when primary operations fail
- **Circuit Breaker Pattern**: Preventing cascading failures
- **Arena-based Memory**: Efficient memory management using arenas

## Running the Example

To run the example with a specific error handling pattern:

```bash
cd examples/error_handling
go run main.go <pattern>
```

Where `<pattern>` is one of: `retry`, `timeout`, `conditional`, `fallback`, or `circuit_breaker`.

If no pattern is specified, the retry pattern will be used by default.

## Example Output

The example will display:

1. Configuration settings
2. Pattern description
3. Execution progress for each node
4. Error handling behavior
5. Node statuses and execution results
6. Memory usage statistics (if arena is enabled)

## Modifying the Example

You can modify the configuration in the `main()` function to experiment with different settings:

```go
config := Config{
    UseArena:       true,
    ArenaBlockSize: 8192,
    WorkerCount:    runtime.NumCPU(),
    ErrorType:      errorType,
}
```

You can also modify the error handling parameters in each pattern function to see how they affect the behavior of the workflow. 