# API Workflow Example

This example demonstrates an API integration workflow using the flow-orchestrator library. It simulates a data processing pipeline that:

1. Fetches user data from a mock API
2. Processes the data
3. Sends the processed data to an analytics API 
4. Saves user statistics to a database

## Features Demonstrated

- DAG (Directed Acyclic Graph) structure for workflow definition
- State management and persistence (StateDelta pattern)
- Error handling and retries
- Parallel execution of independent nodes
- Workflow resumption after failures

## Getting Started

### Running the Basic Workflow

```bash
go run main.go
```

### Testing Different Scenarios

This example includes a comprehensive test suite to verify the workflow system's robustness:

```bash
# Make the test script executable
chmod +x test_scenarios.sh

# Run all test scenarios
./test_scenarios.sh
```

For details on all test scenarios and how to interpret the results, see [TESTING.md](TESTING.md).

## Available Flags

The example supports various command-line flags to simulate different conditions:

| Flag | Description |
|------|-------------|
| `--fail-fetch` | Force fetch action to fail |
| `--fail-process` | Force process action to fail |
| `--fail-api` | Force API action to fail |
| `--fail-db` | Force DB action to fail |
| `--fetch-failure-rate=RATE` | Set failure rate for fetch (0.0-1.0) |
| `--api-failure-rate=RATE` | Set failure rate for API calls (0.0-1.0) |
| `--db-failure-rate=RATE` | Set failure rate for DB operations (0.0-1.0) |
| `--verbose-retries` | Show detailed retry information |
| `--resume=FILE` | Resume from state file |
| `--slow-api` | Simulate slow API responses |
| `--custom-pool` | Use custom worker pool configuration |
| `--workers=N` | Set number of workers in the pool (default 3) |
| `--queue-size=N` | Set maximum queue size (default 50) |

## Workflow Structure

The workflow is structured as a DAG with the following nodes:

```
Level 1: fetch
Level 2: process
Level 3: send_to_api, save_to_db
Level 4: finalize
```

Each node has a specific retry policy:
- `fetch`: 2 retries
- `process`: 1 retry
- `send_to_api`: 3 retries
- `save_to_db`: 2 retries
- `finalize`: 0 retries

## State Management

The workflow uses a file-based state store that persists the workflow state after each topological level. This enables resumption after failures, making the workflow more robust.

## Visualizing the Workflow

Here's a visual representation of the workflow:

```
     ┌───────┐
     │ fetch │
     └───┬───┘
         │
         ▼
    ┌─────────┐
    │ process │
    └────┬────┘
         │
    ┌────┴──────┐
    │           │
    ▼           ▼
┌──────────┐ ┌───────────┐
│send_to_api│ │save_to_db│
└─────┬────┘ └────┬──────┘
      │           │
      └─────┬─────┘
            │
            ▼
       ┌──────────┐
       │ finalize │
       └──────────┘
```

## Example Output

A successful run will produce output similar to:

```
🚀 Starting API Workflow Example

📋 Test Configuration:
   - Fetch Failure Rate: 0.00 (Forced: false)
   - Process Failure: false
   - API Failure Rate: 0.00 (Forced: false)
   - DB Failure Rate: 0.00 (Forced: false)
   - Verbose Retries: false
   - Simulate Slow API: false
   - Resume From: 

📋 Workflow Structure:
   Level 1: fetch
   Level 2: process
   Level 3: send_to_api, save_to_db
   Level 4: finalize

▶️ Executing workflow...
🔍 Fetching user data from API...
✅ Fetched 3 users in 302 ms
⚙️ Processing user data...
✅ Processed 3 users in 0 ms
📤 Sending user activity to Analytics API...
✅ Sent to API with transaction ID: txn-12345 in 201 ms
💾 Saving user stats to database...
✅ Saved to database with record ID: 42 in 150 ms
🏁 Finalizing workflow...
🎉 Workflow completed in 653 ms

📊 Workflow Summary:
   - Users Processed: 3
   - Active Users: 2
   - API Transaction ID: txn-12345
   - DB Record ID: 42
   - Total Time: 653 ms

✨ Workflow completed successfully!
``` 