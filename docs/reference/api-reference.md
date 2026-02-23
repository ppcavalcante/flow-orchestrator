# API Reference

This document provides a comprehensive reference for the public API of Flow Orchestrator. It covers the core types, interfaces, and functions available in the `github.com/ppcavalcante/flow-orchestrator/pkg/workflow` package.

## Core Types

### Workflow

The `Workflow` type represents a complete workflow execution unit:

```go
type Workflow struct {
    DAG        *DAG           // The workflow structure
    WorkflowID string         // Unique identifier
    Store      WorkflowStore  // Optional persistence layer
}

// Execute runs the workflow
func (w *Workflow) Execute(ctx context.Context) error
```

### DAG (Directed Acyclic Graph)

The `DAG` type represents the structure of a workflow:

```go
type DAG struct {
    // Internal fields omitted
}

// NewDAG creates a new DAG
func NewDAG(name string) *DAG

// AddNode adds a node to the DAG
func (d *DAG) AddNode(node *Node) error

// AddDependency adds a dependency between nodes
func (d *DAG) AddDependency(from, to string) error

// Validate checks the DAG for cycles and other issues
func (d *DAG) Validate() error

// Execute runs the DAG with the provided context and data
func (d *DAG) Execute(ctx context.Context, data *WorkflowData) error
```

### Node

The `Node` type represents a single unit of work:

```go
type Node struct {
    Name       string         // Unique identifier
    Action     Action         // The executable work
    DependsOn  []*Node        // Dependencies
    RetryCount int            // Number of retry attempts
    Timeout    time.Duration  // Maximum execution time
}

// NewNode creates a new node
func NewNode(name string, action Action) *Node

// WithRetries sets the retry count
func (n *Node) WithRetries(count int) *Node

// WithTimeout sets the execution timeout
func (n *Node) WithTimeout(timeout time.Duration) *Node
```

### Action

The `Action` interface represents executable work:

```go
type Action interface {
    Execute(ctx context.Context, data *WorkflowData) error
}

// ActionFunc allows functions to be used as Actions
type ActionFunc func(ctx context.Context, data *WorkflowData) error

// Execute implements the Action interface
func (f ActionFunc) Execute(ctx context.Context, data *WorkflowData) error
```

### WorkflowData

The `WorkflowData` type provides thread-safe data storage:

```go
type WorkflowData struct {
    ID string  // Workflow identifier
    // Internal fields omitted
}

// NewWorkflowData creates a new WorkflowData instance
func NewWorkflowData(id string) *WorkflowData

// Get retrieves a value
func (d *WorkflowData) Get(key string) (interface{}, bool)

// Set stores a value
func (d *WorkflowData) Set(key string, value interface{})

// GetString retrieves a string value
func (d *WorkflowData) GetString(key string) (string, bool)

// GetInt retrieves an int value
func (d *WorkflowData) GetInt(key string) (int, bool)

// GetBool retrieves a bool value
func (d *WorkflowData) GetBool(key string) (bool, bool)

// GetFloat64 retrieves a float64 value
func (d *WorkflowData) GetFloat64(key string) (float64, bool)

// GetOutput retrieves a node output
func (d *WorkflowData) GetOutput(nodeName string) (interface{}, bool)

// SetOutput stores a node output
func (d *WorkflowData) SetOutput(nodeName string, output interface{})

// GetNodeStatus retrieves a node's status
func (d *WorkflowData) GetNodeStatus(nodeName string) (NodeStatus, bool)

// SetNodeStatus sets a node's status
func (d *WorkflowData) SetNodeStatus(nodeName string, status NodeStatus)
```

### WorkflowBuilder

The `WorkflowBuilder` provides a fluent API for workflow construction:

```go
type WorkflowBuilder struct {
    // Internal fields omitted
}

// NewWorkflowBuilder creates a new builder
func NewWorkflowBuilder() *WorkflowBuilder

// WithWorkflowID sets the workflow ID
func (b *WorkflowBuilder) WithWorkflowID(id string) *WorkflowBuilder

// WithStore sets the persistence store
func (b *WorkflowBuilder) WithStore(store WorkflowStore) *WorkflowBuilder

// WithStateStore is an alias for WithStore (deprecated)
func (b *WorkflowBuilder) WithStateStore(store WorkflowStore) *WorkflowBuilder

// AddStartNode adds a start node (no dependencies)
func (b *WorkflowBuilder) AddStartNode(name string) *NodeBuilder

// AddNode adds a node
func (b *WorkflowBuilder) AddNode(name string) *NodeBuilder

// Build creates the final DAG
func (b *WorkflowBuilder) Build() (*DAG, error)
```

### NodeBuilder

The `NodeBuilder` provides a fluent API for node configuration:

```go
type NodeBuilder struct {
    // Internal fields omitted
}

// WithAction sets the node's action
func (b *NodeBuilder) WithAction(action Action) *NodeBuilder

// WithRetries sets the retry count
func (b *NodeBuilder) WithRetries(count int) *NodeBuilder

// WithTimeout sets the execution timeout
func (b *NodeBuilder) WithTimeout(timeout time.Duration) *NodeBuilder

// DependsOn adds dependencies
func (b *NodeBuilder) DependsOn(nodeNames ...string) *NodeBuilder
```

## Middleware

### Middleware Type

```go
type Middleware func(Action) Action
```

### Built-in Middleware

```go
// LoggingMiddleware logs action execution
func LoggingMiddleware() Middleware

// RetryMiddleware retries failed actions
func RetryMiddleware(maxRetries int, backoff time.Duration) Middleware

// TimeoutMiddleware adds a timeout to actions
func TimeoutMiddleware(timeout time.Duration) Middleware

// MetricsMiddleware collects execution metrics
func MetricsMiddleware() Middleware

// ValidationMiddleware validates workflow data before executing
func ValidationMiddleware(validator func(*WorkflowData) error) Middleware

// ConditionalRetryMiddleware retries based on a predicate
func ConditionalRetryMiddleware(maxRetries int, backoff time.Duration, predicate func(error) bool) Middleware

// NoDelayRetryMiddleware retries immediately (useful for testing/benchmarks)
func NoDelayRetryMiddleware(maxRetries int, verbose ...bool) Middleware
```

### MiddlewareStack

```go
type MiddlewareStack struct {
    // Internal fields omitted
}

// NewMiddlewareStack creates a new stack
func NewMiddlewareStack() *MiddlewareStack

// Use adds middleware to the stack (returns stack for chaining)
func (s *MiddlewareStack) Use(m Middleware) *MiddlewareStack

// Apply applies all middleware to an action
func (s *MiddlewareStack) Apply(action Action) Action
```

## Persistence

### WorkflowStore Interface

```go
type WorkflowStore interface {
    // Save persists workflow data
    Save(data *WorkflowData) error
    
    // Load retrieves workflow data
    Load(workflowID string) (*WorkflowData, error)
    
    // ListWorkflows returns all workflow IDs
    ListWorkflows() ([]string, error)
    
    // Delete removes workflow data
    Delete(workflowID string) error
}
```

### Built-in Stores

```go
// NewInMemoryStore creates an in-memory store
func NewInMemoryStore() *InMemoryStore

// NewJSONFileStore creates a JSON file-based store (deprecated: use FlatBuffersStore)
func NewJSONFileStore(baseDir string) (*JSONFileStore, error)

// NewFlatBuffersStore creates a FlatBuffers-based store
func NewFlatBuffersStore(baseDir string) (*FlatBuffersStore, error)
```

## Node Status

```go
type NodeStatus string

const (
    Pending    NodeStatus = "pending"
    Running    NodeStatus = "running"
    Completed  NodeStatus = "completed"
    Failed     NodeStatus = "failed"
    Skipped    NodeStatus = "skipped"
    NotStarted NodeStatus = "not_started"
)
```

## Error Sentinels

```go
// ErrInputNotFound indicates a required input was not found
var ErrInputNotFound = errors.New("input not found")

// ErrInvalidInput indicates an input value is invalid
var ErrInvalidInput = errors.New("invalid input")

// ErrExecutionFailed indicates an action execution failed
var ErrExecutionFailed = errors.New("execution failed")
```

## Usage Examples

For detailed usage examples, refer to the [examples directory](../../examples/) in the repository and the [Getting Started](../getting-started/) guides. 