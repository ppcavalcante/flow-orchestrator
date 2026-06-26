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

// WithExecutionConfig sets the per-level concurrency config (added v0.3.0)
func (d *DAG) WithExecutionConfig(config ExecutionConfig) *DAG
```

#### ExecutionConfig

Controls per-level concurrency of `DAG.Execute` (wired end-to-end as of v0.3.0):

```go
type ExecutionConfig struct {
    MaxConcurrency int // Max nodes executed concurrently per level (<=0 -> DefaultMaxConcurrency)
}

const DefaultMaxConcurrency = 16

func DefaultConfig() ExecutionConfig

// Also settable on the builder:
func (b *WorkflowBuilder) WithExecutionConfig(config ExecutionConfig) *WorkflowBuilder
```

### Node

The `Node` type represents a single unit of work:

```go
type Node struct {
    Name            string         // Unique identifier
    Action          Action         // The executable work
    DependsOn       []*Node        // Dependencies
    RetryCount      int            // Number of retry attempts
    Timeout         time.Duration  // Maximum execution time
    ContinueOnError bool           // If true, a failure of this node does not fail the workflow (added v0.7.0)
}

// NewNode creates a new node
func NewNode(name string, action Action) *Node

// WithRetries sets the retry count
func (n *Node) WithRetries(count int) *Node

// WithTimeout sets the execution timeout
func (n *Node) WithTimeout(timeout time.Duration) *Node

// WithContinueOnError marks the node so that its failure does not fail the
// workflow: the node is recorded as Failed and the rest of the DAG continues
// (added v0.7.0). See "Failure Semantics" below.
func (n *Node) WithContinueOnError() *Node
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

// GetInt retrieves an int value (platform int; on 32-bit builds use GetInt64 for values > MaxInt32)
func (d *WorkflowData) GetInt(key string) (int, bool)

// GetInt64 retrieves an integer value as int64 (portable across architectures)
func (d *WorkflowData) GetInt64(key string) (int64, bool)

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

### Typed-Key Data API (added v0.7.0)

An **additive, type-safe** layer over the string-keyed `Set`/`Get`. A `Key[T]`
carries the value type alongside its string name, so a producer and consumer that
share a `Key` are checked at compile time. These are **package-level generic
functions** (Go does not allow type parameters on methods), so call them as
`workflow.Set(d, key, v)` / `workflow.Get(d, key)`.

The values are stored in the same underlying map as the string API (the typed
layer bridges it with a `v.(T)` assertion), so the two APIs fully interoperate: a
value written with `Set[T]` is readable via `WorkflowData.Get(k.Name())`, and vice
versa. The typed layer holds no state and adds no locking — thread-safety is
inherited from `WorkflowData`.

```go
// Key is a typed handle to a value in a WorkflowData. Declare it once and share
// it between the producing and consuming nodes.
type Key[T any] struct { /* unexported field */ }

// NewKey returns a Key addressing name and carrying value type T.
func NewKey[T any](name string) Key[T]

// Name returns the underlying string key (lets you reach the value via the string API).
func (k Key[T]) Name() string

// Set stores v under the typed key k in d (writes through the string store).
func Set[T any](d *WorkflowData, k Key[T], v T)

// Get returns (value, true) only when a value is present at k's name AND its
// dynamic type is T; otherwise (zero, false). A stored zero value reports
// (zero, true), so an absent key is distinguishable from a stored zero. Never panics.
func Get[T any](d *WorkflowData, k Key[T]) (T, bool)

// GetOr returns the stored value, or def when the key is absent or holds a value
// of a different type.
func GetOr[T any](d *WorkflowData, k Key[T], def T) T
```

Example:

```go
var UserID = workflow.NewKey[int]("user_id")

// producer node
workflow.Set(data, UserID, 123)

// consumer node — compile-time type-safe, no assertion at the call site
id, ok := workflow.Get(data, UserID) // id is int
n := workflow.GetOr(data, UserID, -1)
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

// WithAction sets the node's action (accepts an Action or a compatible func)
func (b *NodeBuilder) WithAction(action interface{}) *NodeBuilder

// WithRetries sets the retry count
func (b *NodeBuilder) WithRetries(count int) *NodeBuilder

// WithTimeout sets the execution timeout
func (b *NodeBuilder) WithTimeout(timeout time.Duration) *NodeBuilder

// WithContinueOnError marks the node continue-on-error (added v0.7.0).
// See "Failure Semantics" below.
func (b *NodeBuilder) WithContinueOnError() *NodeBuilder

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

// NewJSONFileStore creates a JSON file-based store (human-readable, recovery-friendly;
// use FlatBuffersStore for the faster binary format)
func NewJSONFileStore(baseDir string) (*JSONFileStore, error)

// NewFlatBuffersStore creates a FlatBuffers-based store
func NewFlatBuffersStore(baseDir string) (*FlatBuffersStore, error)
```

## Node Status

```go
type NodeStatus string

const (
    Pending   NodeStatus = "pending"   // initial state; also a node never reached
    Running   NodeStatus = "running"
    Completed NodeStatus = "completed"
    Failed    NodeStatus = "failed"     // the node's action returned an error
    Skipped   NodeStatus = "skipped"    // a non-resolving dependency (Failed non-coe, or Skipped) blocked it
)
```

## Failure Semantics

`DAG.Execute` runs levels in sequence. How a node failure is handled depends on
whether the node is marked continue-on-error:

- **Default (fail-fast).** When a normal node's action returns an error, the
  node is recorded as `Failed`, the executor cancels its in-flight siblings in
  that level, and `DAG.Execute` halts **without running any later level**,
  returning an [`*ExecutionError`](#aggregate-execution-error) that aggregates the
  halting level's fail-fast failures. A `Failed` normal node blocks its dependents.
- **Continue-on-error.** When a node marked with `WithContinueOnError()` fails,
  it is recorded as `Failed` but the failure does **not** cancel siblings and does
  **not** fail the workflow. Execution continues; the node's dependents still run
  and observe its `Failed` status via `WorkflowData.GetNodeStatus`. A
  continue-on-error dependency that `Failed` is treated as *resolved* (it no
  longer blocks dependents); a normal `Failed` dependency, and any
  `Skipped`/`Running`/`Pending` dependency, still blocks.

**Status accounting.** Execute initializes every node to `Pending` at the start,
so node status is total over the DAG. A node that did **not** run because a
dependency was in a terminal non-resolving state — a non-continue-on-error
dependency that `Failed`, or a dependency that was itself `Skipped` — is marked
`Skipped` (transitively). A node that simply was never reached (the run halted
before it, and none of its dependencies failed or were skipped) stays `Pending`.
`Skipped` is **not** a failure: skipped nodes never appear in `ExecutionError`.

`DAG.Execute` returns `nil` if and only if every node that is **not**
continue-on-error succeeded; otherwise it returns an `*ExecutionError`. These
semantics are machine-checked: see [Verification](#verification).

```go
// Marked continue-on-error: a failure here does not halt the workflow.
builder.AddNode("optional-enrichment").
    WithAction(enrichAction).
    WithContinueOnError().
    DependsOn("load")

// Dependent runs even if optional-enrichment failed, and branches on its status.
builder.AddNode("finalize").
    WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
        if status, _ := data.GetNodeStatus("optional-enrichment"); status == workflow.Failed {
            // proceed without the optional enrichment
        }
        return finalizeAction.Execute(ctx, data)
    }).
    DependsOn("optional-enrichment")
```

## Verification

The execution semantics above are verified at two layers (milestone M7):

- **Layer 1 — property-based tests.** `pkg/workflow/invariants_property_test.go`
  is a [gopter](https://github.com/leanovate/gopter) suite that generates random
  DAGs and asserts the executor's invariants (topological order, peak concurrency
  within `MaxConcurrency`, run-once completeness, dependencies-before-run, and the
  continue-on-error / fail-fast failure semantics) against the **real**
  implementation. It runs as part of `go test ./...`.
- **Layer 2 — TLA+ formal model.** `specs/` contains a TLA+/PlusCal model of the
  level executor and concurrency semaphore (`Executor.tla` + `MCExecutor.tla`),
  TLC-checked for safety (concurrency bound, dependencies-before-run, fail-fast
  halting) and liveness (termination / deadlock-freedom). See
  [`specs/README.md`](../../specs/README.md) for the model, the scenarios, and the
  honest scope (design-exhaustive vs implementation-sampled).

## Error Sentinels

Flow Orchestrator exposes two **intentionally distinct** families of sentinel errors,
matched with `errors.Is`. They are **not aliased**: a workflow missing on disk
(`ErrNotFound`) is a different concept from a data key being absent within an action
(`ErrInputNotFound`).

### Action-execution domain (an `Action`'s runtime behavior)

```go
// ErrInputNotFound indicates a required input was not found
var ErrInputNotFound = errors.New("input not found")

// ErrInvalidInput indicates an input value is invalid
var ErrInvalidInput = errors.New("invalid input")

// ErrExecutionFailed indicates an action execution failed
var ErrExecutionFailed = errors.New("execution failed")
```

### Store / persistence domain (added v0.3.0)

Returned by `WorkflowStore` implementations and the validation guards that feed them.
Each is wrapped with `%w` at its origin, so the underlying detail remains available via
`errors.Unwrap`/`errors.As` while the category stays stable. `ErrCorruptData` keeps its
public message generic so it does not leak file paths or raw decode internals.

```go
// ErrNotFound: the requested workflow does not exist (no file/entry). Distinct
// from a permission/other I/O failure, which is ErrIO.
var ErrNotFound = errors.New("workflow not found")

// ErrValidation: invalid input rejected before any I/O (empty/unsafe ID, nil data).
var ErrValidation = errors.New("validation failed")

// ErrCorruptData: persisted data could not be decoded (malformed/truncated/
// version-skewed FlatBuffers or JSON). For FlatBuffers Load, this is returned
// when the layered bounds guard rejects the input (oversize file, out-of-range
// root offset, or over-cap element count) *before* the decode, as well as by the
// recover() backstop if a deeper malformation slips past the pre-check.
var ErrCorruptData = errors.New("corrupt workflow data")

// ErrIO: a transient/environmental I/O failure that is neither not-found nor
// corruption (permissions, full disk, unavailable directory).
var ErrIO = errors.New("workflow I/O error")
```

Branch with `errors.Is`:

```go
_, err := store.Load(workflowID)
switch {
case errors.Is(err, workflow.ErrNotFound):    // no such workflow
case errors.Is(err, workflow.ErrCorruptData): // file present but undecodable
case errors.Is(err, workflow.ErrIO):          // transient I/O — safe to retry
}
```

## Aggregate Execution Error

When a workflow fails, `DAG.Execute` (and `Workflow.Execute`) return an
`*ExecutionError` that aggregates **every** fail-fast node failure — not just the
first. When several nodes in a level fail concurrently, all of them are captured.

```go
// NodeError pairs a failed node's name with the error its action returned.
type NodeError struct {
    NodeName string
    Err      error
}

// ExecutionError aggregates the fail-fast failures of one execution.
type ExecutionError struct {
    FailedNodes []NodeError // sorted by NodeName, deterministic
}
```

- `Error()` is a **summary only**: the failure count, each failed node's name, and
  each node's own error string. It never includes `WorkflowData` values, inputs,
  file paths, or internal engine state — only what the action itself returned.
- `Unwrap() []error` exposes the per-node errors, so `errors.Is` reaches a sentinel
  an action wrapped (e.g. `ErrExecutionFailed`) and `errors.As` extracts the aggregate.

```go
err := dag.Execute(ctx, data)

var execErr *workflow.ExecutionError
if errors.As(err, &execErr) {
    for _, ne := range execErr.FailedNodes {
        log.Printf("node %q failed: %v", ne.NodeName, ne.Err)
    }
}
if errors.Is(err, workflow.ErrExecutionFailed) {
    // a failed node's action wrapped the ErrExecutionFailed sentinel
}
```

**Continue-on-error interaction:** a node marked `WithContinueOnError()` whose
action fails is **tolerated** — it is recorded `Failed` (observe via
`GetNodeStatus`) and does **not** appear in `ExecutionError`. A run whose only
failures are continue-on-error nodes returns `nil` from `Execute`.

## Usage Examples

For detailed usage examples, refer to the [examples directory](../../examples/) in the repository and the [Getting Started](../getting-started/) guides. 