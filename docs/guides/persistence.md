# Persistence Layer

Flow Orchestrator's persistence layer allows workflow state to be saved and restored, enabling durable, resumable workflows across application restarts.

## Core Concepts

### WorkflowData

The `WorkflowData` structure stores all workflow state:
- Workflow identifier
- Node statuses
- Custom workflow data (key-value pairs)
- Node outputs
- Metadata

### WorkflowStore Interface

All storage implementations implement this interface:

```go
type WorkflowStore interface {
    // Save persists workflow data to storage
    Save(data *WorkflowData) error
    
    // Load retrieves workflow data from storage by ID
    Load(workflowID string) (*WorkflowData, error)
    
    // ListWorkflows returns all saved workflow IDs
    ListWorkflows() ([]string, error)
    
    // Delete removes workflow data from storage
    Delete(workflowID string) error
}
```

## Built-in Storage Options

### In-Memory Store

For ephemeral workflows without persistence needs:

```go
// Create an in-memory store
store := workflow.NewInMemoryStore()
```

**Best for**: Development, testing, ephemeral workflows

### JSON File Store

Persists as human-readable JSON files:

```go
// Create a JSON file store
store, err := workflow.NewJSONFileStore("./workflow_data")
if err != nil {
    log.Fatalf("Failed to create store: %v", err)
}
```

**Best for**: Simple applications, debugging, development

### FlatBuffers Store

High-performance binary serialization:

```go
// Create a FlatBuffers store
store, err := workflow.NewFlatBuffersStore("./workflow_data")
if err != nil {
    log.Fatalf("Failed to create store: %v", err)
}
```

**Best for**: Production use, high-performance needs

## Using Persistence

### Basic Usage

```go
// Build your workflow DAG
dag, err := builder.Build()
if err != nil {
    log.Fatalf("Failed to build workflow: %v", err)
}

// Create a persistence store
store, err := workflow.NewJSONFileStore("./workflow_data")
if err != nil {
    log.Fatalf("Failed to create store: %v", err)
}

// Create a workflow with the DAG and store
workflow := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "order-processing",
    Store:      store,
}

// Execute the workflow with persistence
err = workflow.Execute(context.Background())
```

### Resuming Workflows

```go
func resumeWorkflow(workflowID string) error {
    // Create a store
    store, err := workflow.NewJSONFileStore("./workflow_data")
    if err != nil {
        return fmt.Errorf("failed to create store: %w", err)
    }
    
    // Check if workflow state exists
    _, err = store.Load(workflowID)
    if err != nil {
        return fmt.Errorf("no existing workflow state: %w", err)
    }
    
    // Rebuild the workflow DAG (same structure as before)
    dag := rebuildWorkflowDAG()
    
    // Create a workflow with the existing state
    workflow := &workflow.Workflow{
        DAG:        dag,
        WorkflowID: workflowID,
        Store:      store,
    }
    
    // Resume the workflow
    return workflow.Execute(context.Background())
}
```

## Serialization Considerations

### Supported Data Types

Ensure data stored in WorkflowData can be serialized:

**Recommended**:
- Basic types (string, int, float, bool)
- Maps with string keys
- Arrays and slices
- Structs (field visibility depends on format)
- Time values (as strings for JSON)

**Avoid**:
- Channels
- Functions
- Complex pointers
- Unexported struct fields (for JSON)

### Custom Type Handling

For custom types, implement serialization helpers:

```go
// Store a custom type
func storeOrderStatus(data *workflow.WorkflowData, status OrderStatus) error {
    statusJSON, err := json.Marshal(status)
    if err != nil {
        return err
    }
    data.Set("order_status", string(statusJSON))
    return nil
}

// Retrieve a custom type
func getOrderStatus(data *workflow.WorkflowData) (OrderStatus, error) {
    var status OrderStatus
    
    statusJSON, ok := data.GetString("order_status")
    if !ok {
        return status, fmt.Errorf("order status not found")
    }
    
    err := json.Unmarshal([]byte(statusJSON), &status)
    return status, err
}
```

## Creating Custom Storage

Implement the WorkflowStore interface for custom storage:

```go
type MyCustomStore struct {
    // Your store implementation details
}

func (s *MyCustomStore) Save(data *workflow.WorkflowData) error {
    // Serialize and store workflow data
    return nil
}

func (s *MyCustomStore) Load(workflowID string) (*workflow.WorkflowData, error) {
    // Load and deserialize workflow data
    return nil, nil
}

func (s *MyCustomStore) ListWorkflows() ([]string, error) {
    // Return all workflow IDs in storage
    return nil, nil
}

func (s *MyCustomStore) Delete(workflowID string) error {
    // Remove workflow data from storage
    return nil
}
```

## Performance Considerations

1. **Batch Updates**: Minimize save operations
2. **Use Binary Formats**: FlatBuffers provides better performance than JSON
3. **Select Appropriate Storage**: Match storage to access patterns
4. **Consider Caching**: Add a caching layer for frequent access
5. **Compress Data**: For large workflows, consider compression

## Best Practices

1. **Use Appropriate Storage**: Choose storage based on requirements
2. **Plan for Recovery**: Implement error handling around storage
3. **Version Your Data**: Include version info for migrations
4. **Test Recovery**: Regularly test workflow resumption
5. **Clean Up Old Data**: Implement TTL for completed workflows
6. **Define Serialization Strategy**: Have a clear approach for complex types

## Conclusion

Flow Orchestrator's persistence layer enables durable, resumable workflows. By choosing the right storage implementation and following serialization best practices, you can create reliable workflow applications that maintain state across application restarts.

For more on using persistence with complex workflow patterns, see the [Workflow Patterns](./workflow-patterns.md) and [Error Handling](./error-handling.md) guides. 