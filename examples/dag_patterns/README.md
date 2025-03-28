# DAG Patterns Example

This example demonstrates different Directed Acyclic Graph (DAG) patterns that can be implemented using the workflow system. It showcases the flexibility of the system in handling various workflow topologies.

## Overview

The example includes five different DAG patterns:

1. **Linear Pattern**: A simple sequential workflow where each node depends on the previous node.
   ```
   A → B → C → D
   ```

2. **Fan-Out Pattern**: A workflow where one node fans out to multiple parallel nodes.
   ```
   A → [B, C, D]
   ```

3. **Fan-In Pattern**: A workflow where multiple parallel nodes converge to a single node.
   ```
   [A, B, C] → D
   ```

4. **Diamond Pattern**: A workflow that combines fan-out and fan-in patterns.
   ```
   A → [B, C] → D
   ```

5. **Complex Pattern**: A more realistic workflow with multiple branches and dependencies.
   ```
   A → B → C → [D, E, F] → [G, H] → I → J → K
   ```

## Features Demonstrated

- **Parallel Execution**: Automatic execution of independent nodes in parallel
- **Dependency Management**: Proper handling of node dependencies
- **Arena-based Memory**: Efficient memory management using arenas
- **Execution Tracking**: Tracking of node execution times and statuses

## Running the Example

To run the example with a specific pattern:

```bash
cd examples/dag_patterns
go run main.go <pattern>
```

Where `<pattern>` is one of: `linear`, `fan_out`, `fan_in`, `diamond`, or `complex`.

If no pattern is specified, the linear pattern will be used by default.

## Example Output

The example will display:

1. Configuration settings
2. Pattern description
3. Execution progress for each node
4. Execution time for each node
5. Overall workflow execution time
6. Node statuses and completion timestamps
7. Memory usage statistics (if arena is enabled)

## Modifying the Example

You can modify the configuration in the `main()` function to experiment with different settings:

```go
config := Config{
    UseArena:       true,
    ArenaBlockSize: 8192,
    WorkerCount:    runtime.NumCPU(),
    Pattern:        pattern,
}
```

Try changing these values to see how they affect performance and memory usage. 