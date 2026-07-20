# API Reference

This document provides a comprehensive reference for the public API of Flow Orchestrator. It covers the core types, interfaces, and functions available in the `github.com/ppcavalcante/flow-orchestrator/pkg/workflow` package.

## Core Types

### Workflow

The `Workflow` type represents a complete workflow execution unit:

```go
type Workflow struct {
    DAG        *DAG            // The workflow structure
    WorkflowID string          // Unique identifier
    Store      WorkflowStore   // Optional persistence layer
    // MetricsConfig opts the internally-built WorkflowData into operation-metrics
    // collection (added v0.13.0 / REM-02). nil (default) = metrics off, zero-tax
    // hot path unchanged. Set e.g. metrics.NewConfig().WithEnabled(true).
    MetricsConfig *metrics.Config
}

// Execute runs the workflow
func (w *Workflow) Execute(ctx context.Context) error

// GetMetrics returns the collected operation metrics after Execute (non-nil even
// on the disabled default). Call after Execute returns; concurrent-with-Execute is a race.
func (w *Workflow) GetMetrics() *metrics.MetricsCollector
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

// WithStore sets the persistence store. The store reaches execution only via
// FromBuilder (which builds a *Workflow carrying the store); a bare Build() returns
// a store-less *DAG. Since v0.13.0 (REM-04), Build() with a store set returns an
// error — use FromBuilder for a persistent, crash-safe run.
func (b *WorkflowBuilder) WithStore(store WorkflowStore) *WorkflowBuilder

// AddStartNode adds a start node (no dependencies)
func (b *WorkflowBuilder) AddStartNode(name string) *NodeBuilder

// AddNode adds a node
func (b *WorkflowBuilder) AddNode(name string) *NodeBuilder

// Build creates the final DAG
func (b *WorkflowBuilder) Build() (*DAG, error)

// Feature-specific node builders (documented in their own sections):
//   AddTimer / AddWaitForSignal / AddWaitForCondition  — Durable continuations (v0.10.0)
//   AddChoice / AddMerge                               — Conditional branching (v0.11.0)
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

// WithCompensation sets the node's compensating action for saga rollback (added v0.12.0).
// See "Saga / Compensation" below.
func (b *NodeBuilder) WithCompensation(action interface{}) *NodeBuilder

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

// NOTE: there is no MetricsMiddleware (the no-op stub was removed in v0.13.0).
// Enable metrics via Workflow.MetricsConfig + read with Workflow.GetMetrics().

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

// NewFlatBuffersStore creates a FlatBuffers-based store. Variadic options configure
// the durability-flush mode (added v0.13.0); with no options the default is Strict
// (the pre-v0.13 durable contract). See "Durability modes" below.
func NewFlatBuffersStore(baseDir string, opts ...func(*FlatBuffersStore)) (*FlatBuffersStore, error)

// NewSQLiteStore creates a decomposed, row-based store backed by pure-Go
// modernc.org/sqlite (CGO_ENABLED=0 preserved). Added M15. Variadic options configure
// the durability mode (WithSQLiteDurability, default SQLiteStrict). Single-process by
// default (single-writer connection + in-process lease); opt into multi-process competing
// consumers with WithMultiProcess() (M16 — then it also implements ClaimStore). Holds a DB
// handle: call Close() at shutdown. Implements the frozen WorkflowStore base plus the
// optional Checkpointer / Syncer / IncrementalCheckpointer / WorkflowQuery interfaces (NOT
// SignalStore). See the SQLite store section below and ADR-0014/ADR-0015.
func NewSQLiteStore(path string, opts ...SQLiteOption) (*SQLiteStore, error)
```

### Durable crash-resume (added v0.9.0)

A store MAY additionally implement the optional `Checkpointer` interface to opt
into durable mid-run checkpointing. All four built-in stores implement it (InMemory,
JSON, FlatBuffers, and the M15 `SQLiteStore`).

```go
// Checkpointer is an optional interface a WorkflowStore may implement to enable
// durable mid-run checkpointing (crash-resume). When the store implements it,
// Workflow.Execute flushes the run's state at each completed level barrier; a
// store that does not keeps the prior save-at-boundaries behavior with zero
// overhead.
type Checkpointer interface {
    // SaveCheckpoint atomically and durably persists the current workflow state.
    SaveCheckpoint(data *WorkflowData) error
}
```

#### Durability modes (added v0.13.0)

`FlatBuffersStore` supports two checkpoint-flush cadences, selected with the variadic
`WithDurabilityMode` option. Default `Strict` is bit-identical to the pre-v0.13 durable
contract; `Batched(K)` group-commits (fsync every `K`th checkpoint) for deep durable runs,
trading a ≤`K`-level power-loss window (the lost levels re-run, safe under the at-least-once
idempotency contract) for a large speedup. Only `FlatBuffersStore` batches. See the
[Persistence guide](../guides/persistence.md#durability-modes-strict-vs-batched).

```go
// WithDurabilityMode selects the flush cadence (default: Strict).
func WithDurabilityMode(opt DurabilityOption) func(*FlatBuffersStore)

func Strict() DurabilityOption    // one fsync per completed level (the default)
func Batched(k uint) DurabilityOption // group commit: fsync every kth checkpoint (k<1 == Strict)

// Syncer is an optional store capability: force an immediate durable flush.
// FlatBuffersStore implements it; suspend and completion floors call it so a
// parked/finished run is durable even under Batched.
type Syncer interface {
    Sync(workflowID string) error
}
```

#### SQLite store — decomposed, indexed (added M15)

`SQLiteStore` is a third first-class store that persists the run **decomposed into rows**
(one row per node) rather than as one blob per run. It is **fully additive** — it implements
the frozen `WorkflowStore` base plus the optional `Checkpointer` / `Syncer` interfaces above,
plus two M15 optional interfaces below. (It does **not** implement `SignalStore` —
`WaitForSignal` needs one of the other stores.) Backed by pure-Go
`modernc.org/sqlite` (no cgo). **Single-process by default; multi-process is opt-in** via
`WithMultiProcess()` (M16 — see [below](#multi-process-safety--competing-consumers-added-m16)).
It has its own durability option type
(`WithSQLiteDurability(SQLiteStrict() | SQLiteBatched(k))`), the analogue of
`WithDurabilityMode`. See the
[Persistence guide → SQLite store](../guides/persistence.md#sqlite-store-details-decomposed-row-based)
and [ADR-0014](../architecture/adr/0014-decomposed-sqlite-store.md).

```go
// SQLiteOption configures a SQLiteStore at construction (variadic on NewSQLiteStore).
type SQLiteOption func(*sqliteDurability)

func WithSQLiteDurability(opt SQLiteOption) SQLiteOption
func SQLiteStrict() SQLiteOption      // synchronous=FULL + fullfsync=1; per-commit durable (default)
func SQLiteBatched(k uint) SQLiteOption // group commit: durable flush every kth checkpoint (k clamped >=1)

// IncrementalCheckpointer — additive optional interface (M15). A store that implements it
// receives the executor's per-level changed-set and UPSERTs only those keys: O(Δ) compute
// AND writes = a genuine O(N) forward drive (the structural fix for the deep-durable O(N²)
// tail). Type-asserted; a store without it falls back to the full SaveCheckpoint unchanged.
// SQLiteStore implements it.
type IncrementalCheckpointer interface {
    SaveDeltaCheckpoint(changed ChangeSet, d *WorkflowData) error
}

// ChangeSet is the per-level touched-key set the executor passes to SaveDeltaCheckpoint.
// A reported key absent from the WorkflowData means it was deleted (⇒ the store DELETEs it).
type ChangeSet struct {
    Nodes    []string
    DataKeys []string
    WaitKeys []string
}

// WorkflowQuery — additive optional indexed visibility (M15). Honest primitives over the
// real per-node data model (run status is DERIVED — there is no run-level status column);
// the caller composes run-level buckets. Indexed (no per-run decode). SQLiteStore implements it.
type WorkflowQuery interface {
    // ListByNodeStatus returns the DISTINCT workflow IDs with >=1 node in status. since>0
    // additionally filters to updated_at >= since (unix-nanos); since<=0 = no recency filter.
    // Rejects an unknown status as ErrValidation.
    ListByNodeStatus(status NodeStatus, since int64) ([]string, error)
    // ListRollingBack returns the workflow IDs currently rolling back (M12), since-filterable.
    ListRollingBack(since int64) ([]string, error)
}
```

> **Durability honesty (SQLite):** `SQLiteStore` provides **exactly-once state PERSISTENCE
> and at-least-once side EFFECTS** (idempotency-keyed) — never unqualified exactly-once. Under
> `SQLiteBatched(k)` a power loss can lose **≤`k`** completed levels (bounded, re-run
> idempotently). The `O(N)` forward drive comes from `IncrementalCheckpointer`, not from the
> durability mode. See the Persistence guide for the full contract.

Resume is just re-running `Workflow.Execute` with the same `WorkflowID`, store, and
DAG: `Completed` nodes are skipped (outputs rehydrated), every non-completed node
re-runs, and a persisted node missing from the current DAG is rejected with
`ErrValidation` (graph-identity guard). Because non-completed nodes re-run,
execution is **at-least-once** — side-effecting actions must be idempotent.

```go
// IdempotencyKey returns a replay-stable dedupe key for one node of one workflow
// run, derived only from (WorkflowID, nodeName): byte-identical across a
// crash-resume re-run, so a downstream system can collapse the re-execution into
// one logical operation. Format (a stable contract):
//   hex(SHA-256( uint64-LE(len(workflowID)) || workflowID || nodeName ))  // 64 hex chars
func IdempotencyKey(data *WorkflowData, nodeName string) string
```

See the [Persistence guide → Durability & Idempotency](../guides/persistence.md#durability--idempotency-crash-resume)
for the worked detail and the at-least-once contract.

#### Multi-process safety — competing consumers (added M16)

Opt `SQLiteStore` into cross-process mode and N worker processes can share one `.db` file as
**competing consumers**: each claims a distinct workflow, drives it, and safely hands off after
a crash, with no two live workers ever corrupting one run's journal. Fully additive — the
`ClaimStore` interface is optional and type-asserted like `Checkpointer`, engaging only under
`WithMultiProcess()`. Safety rests on **monotonic fencing tokens** (a stale/zombie write is
rejected by a compare-and-swap inside the checkpoint transaction), with **leases** carrying
only liveness (a timing heuristic). See the
[Persistence guide → Multi-process safety](../guides/persistence.md#multi-process-safety-competing-consumers)
and [ADR-0015](../architecture/adr/0015-multi-process-safety-leases-fencing.md).

```go
// Opt into multi-process mode + optionally size the lease (default 30s).
func WithMultiProcess() SQLiteOption        // enable cross-process leases + fencing
func WithLeaseTTL(d time.Duration) SQLiteOption // size ABOVE your longest level (liveness only)

// FencingToken is the monotonic per-claim safety counter (NEVER a timestamp). 0 = no claim.
type FencingToken int64

// ClaimStore — additive optional cross-process interface (M16). A SQLiteStore opened
// WithMultiProcess implements it; type-asserted. The frozen WorkflowStore base is untouched.
type ClaimStore interface {
    // Claim acquires (or re-claims a lapsed) lease for workflowID on behalf of ownerID (an
    // opaque, stable, per-process identity), returning a fresh monotonic token. INSERT-or-CAS.
    // Returns ErrClaimLost if a LIVE lease is held by a different owner.
    Claim(workflowID, ownerID string) (FencingToken, error)
    // Renew extends the lease expiry under the held token. Returns ErrFencedOut if superseded
    // (the drive must abort). Checkpoints ALSO renew automatically inside their CAS txn.
    Renew(workflowID string, token FencingToken) error
    // Release drops the lease iff the caller still holds token (best-effort clean handoff).
    Release(workflowID string, token FencingToken) error
}

// WithMultiProcessLocker wires cross-process claim/fence into this workflow's drive, DERIVING
// the locker from w.Store (the safe, mismatch-proof way — the fencing token lives in that
// store instance). ownerID is a caller-supplied opaque process identity. Panics if w.Store is
// not a ClaimStore (a WithMultiProcess SQLiteStore). This is the ONLY public entry to MP mode:
// the raw store-explicit constructor is intentionally unexported so the instance-mismatch
// footgun (a different store instance makes fencing silently no-op) cannot reach a caller.
func (w *Workflow) WithMultiProcessLocker(ownerID string) *Workflow
```

> **Honesty (multi-process):** the guarantee is **exactly-once state PERSISTENCE across
> processes, at-least-once EFFECTS** — never unqualified exactly-once. Side effects must be
> idempotent (a worker can finish a node's effect, then die before its checkpoint commits, and
> the re-claimer re-runs it). Re-claim-after-death resumes from the **last committed frontier**,
> not a reset to empty. Size `WithLeaseTTL` above your longest level — an over-running level is
> fenced and redone (a liveness cost), never double-committed.

#### Work dispatch — queue, registry, pool (added M17)

An opt-in competing-consumers dispatch layer on the `WithMultiProcess()` store: a durable
`work_queue` table + atomic `ClaimNext`, a type→DAG-factory `Registry`, and a store-per-worker
`Pool` — a zero-infra distributed job queue. All additive; requires an mp store. See the
[Work dispatch guide](../guides/dispatch.md) and
[ADR-0016](../architecture/adr/0016-work-dispatch-queue-registry-pool.md).

```go
// The queue (on a WithMultiProcess SQLiteStore). All require mp mode.
func (s *SQLiteStore) Enqueue(workflowID, typ string, input []byte) (queued bool, err error)
func (s *SQLiteStore) ClaimNext(ownerID string, typeFilter ...string) (WorkItem, error) // ErrNoWork if none
func (s *SQLiteStore) MarkDone(workflowID string) (bool, error)     // claimed→done (CAS + token-guarded)
func (s *SQLiteStore) MarkFailed(workflowID string) (bool, error)   // claimed→failed
func (s *SQLiteStore) MarkForRetry(workflowID string, maxAttempts int) (requeued bool, err error) // claimed→pending if attempts<max
func (s *SQLiteStore) CancelPending(workflowID string) (bool, error) // pending→cancelled (rejects a claimed item)
func (s *SQLiteStore) ListPending(olderThan int64) ([]PendingItem, error) // stuck-work visibility, FIFO

// A claimed item; input is the opaque Enqueue payload; Token is the M16 fencing token.
type WorkItem struct { WorkflowID, Type string; Input []byte; Token FencingToken }
// A pending item for operator inspection (Attempts climbing = a poison item nearing dead-letter).
type PendingItem struct { WorkflowID, Type string; EnqueuedAt int64; Attempts int }

// The registry: DATA type → CODE factory (actions are non-serializable closures — the moat).
type DAGFactory func() (*DAG, error)
func NewRegistry() *Registry
func (r *Registry) Register(typ string, factory DAGFactory) error // fail-loud on empty/nil/duplicate
func (r *Registry) Types() []string

// Run one claimable item of a registered type (claim→rebuild→seed→Execute→terminalize).
// ran=false + nil → nothing claimable (back off); ran=true → an item was driven.
func RunNext(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID string) (ran bool, err error)

// The worker pool: N goroutines, each opening its OWN store (store-per-worker is a SAFETY
// requirement — per-worker tokenState isolation; a shared store un-fences superseded writes).
type StoreFactory func() (*SQLiteStore, error)
func NewPool(factory StoreFactory, reg *Registry, ownerID string, opts ...PoolOption) (*Pool, error)
func (p *Pool) Run(ctx context.Context) error // blocks until ctx cancelled AND every worker drains
func WithPoolSize(n int) PoolOption
func WithMaxAttempts(n int) PoolOption
func WithPollInterval(d time.Duration) PoolOption
```

> **Honesty (dispatch):** **exactly-once state PERSISTENCE, at-least-once INVOCATION** — the
> journal is written once (fencing), but an action body may run **>1** time on a reclaim, bounded
> by `maxAttempts` before dead-lettering. Actions must be idempotent. Reclaim resumes from the
> **last committed frontier** (a fresh run seeds input; a re-claim skips the re-seed). Graceful
> `Pool` drain leaves in-flight work `claimed` to lapse + reclaim — it does **not** lose it.
> **Version-skew limit:** all live workers must share a type's factory version (the resume guard is
> identity-only); a drifted resume is dead-lettered, not mis-run.

#### Dispatch operability — read-model, metrics, cancel (added M18)

Operator-facing read + control over the dispatch queue. All opt-in/additive; the read-model and
cancel require an mp store, and the executor is byte-unchanged. See the
[Dispatch guide → Operator observability](../guides/dispatch.md#operator-observability-added-m18) and
[ADR-0017](../architecture/adr/0017-cancel-of-running-workflow.md).

```go
// Read-model (a WithMultiProcess SQLiteStore implements Observability; type-assert like Checkpointer).
// Four methods are ONE atomic SELECT; WorkflowStatus composes TWO (dispatch row + node tally).
// Snapshot aggregates the aggregate views in one consistent read-txn (use it when parts must agree).
type Observability interface {
    QueueCounts(typ string) (map[string]int, error)          // per-state row counts ("" = whole queue)
    InFlight() ([]InFlightItem, error)                       // claimed items + lease owner + freshness
    StuckWork(olderThan int64, registeredTypes []string) ([]StuckItem, error) // wedged items + StuckReason
    WorkflowStatus(workflowID string) (*WorkflowStatus, error) // dispatch state + per-node journal tally
    WorkerHealth() ([]WorkerInfo, error)                      // per-owner held/live lease counts
}
func (s *SQLiteStore) Snapshot(olderThan int64, registeredTypes []string) (*QueueSnapshot, error) // BEGIN DEFERRED, mutually consistent
// Result types: InFlightItem{WorkflowID,Type,OwnerID string;Attempts int;EnqueuedAt,Expiry int64;LeaseLive bool}
//   StuckItem{WorkflowID,Type,State string;Attempts int;EnqueuedAt int64;Reason StuckReason}
//   WorkflowStatus{WorkflowID,State,OwnerID string;Queued bool;Attempts int;UpdatedAt int64;NodeCounts []NodeStatusCount}
//   WorkerInfo{OwnerID string;TotalHeld,LiveHeld int}; QueueSnapshot{Counts,InFlight,Stuck,Workers}
//   StuckReason ∈ {"unregistered_type","too_old_pending","lapsed_claimed"}

// Cancel a RUNNING (claimed, mid-Execute) workflow → terminal `cancelled`, NEVER resumed. Sets a durable
// cancel_requested INTENT flag (not a terminal flip); needs NO token (operator surface); idempotent.
// Delivery is COOPERATIVE: a 500ms watcher cancels the run ctx; Execute acts at its next level barrier
// (latency = next level barrier + <=500ms — NOT an instant kill).
func (s *SQLiteStore) CancelRunning(workflowID string) (requested bool, err error)

// Dispatch metrics (opt-in; nil = zero-cost). Event counters, DISTINCT from the M14 per-run metrics
// (Workflow.MetricsConfig / metrics.Collector). Reset on process restart (events, not durable state).
type DispatchMetrics struct{ /* reclaimAfterDeath / fenceRejections / supersededAborts / retriesAttempted / deadLetters */ }
func NewDispatchMetrics() *DispatchMetrics
func WithDispatchMetrics(m *DispatchMetrics) SQLiteOption
// getters: (*DispatchMetrics) ReclaimAfterDeath/FenceRejections/SupersededAborts/RetriesAttempted/DeadLetters() int64

// OTel bridge: the 5 event counters + live state gauges (read from the read-model), via the OTel API.
func NewOTelDispatchBridge(store Observability, m *DispatchMetrics, mp metric.MeterProvider) (*OTelDispatchBridge, error)
func (b *OTelDispatchBridge) Shutdown(ctx context.Context) error
```

> **Honesty (M18):** the read-model is a purely additive read side (write path byte-unchanged); each
> granular query is one atomic SELECT **except `WorkflowStatus` (two — its dispatch row + its node
> tally)**, `Snapshot` is mutually consistent (`BEGIN DEFERRED`). Cancel is
> **cooperative and best-effort** — bounded by the level barrier + the 500 ms poll, **not** an instant
> abort; a node already mid-level is not interrupted. Dispatch metrics **reset on restart** (they count
> what this process saw). Lease `LeaseLive`/`Expiry` is a **liveness** read, never a safety input.

## Durable continuations (added v0.10.0)

<a id="durable-continuations"></a>

Built on the crash-resume seam above, a workflow can **suspend** on an external event
and **resume** later. A node that must wait *parks* (status [`Waiting`](#node-status)),
the run drains to its level barrier, the checkpoint flushes, and `Workflow.Execute`
returns `ErrSuspended` — the process may then exit. Waking is just re-entering the
executor. There is no mandatory background service; the host drives waking on its own
schedule. Requires a `Checkpointer` store (durable timers/conditions) and additionally
a `SignalStore` (signals).

### Suspension sentinel and configuration errors

```go
// ErrSuspended is returned by Workflow.Execute when the run parked on an external
// event (a node is Waiting) rather than completing. It is NOT a failure — it means
// "suspended, re-enter to resume". Test with errors.Is(err, workflow.ErrSuspended).
var ErrSuspended error

// ErrSuspendRequiresCheckpointer is returned (a real failure) when a suspension node
// would park but the Store does not implement Checkpointer — a run cannot suspend
// with nowhere durable to persist the parked state.
var ErrSuspendRequiresCheckpointer error

// ErrWaitRequiresSignalStore is returned (a real failure) when a WaitForSignalNode is
// reached but the Store does not implement SignalStore.
var ErrWaitRequiresSignalStore error
```

### Durable timers

```go
// NewTimerNode builds a declared TimerNode: when reached it parks the run (Waiting)
// until an ABSOLUTE due-time (clock.Now()+d, frozen at the first encounter and
// persisted), then fires and converges. The due-time survives crash/suspend; an
// overdue timer fires immediately on the next resume/Tick. A durable timer is a
// LOWER BOUND on a wake-up, not a hard real-time deadline — the first encounter
// always parks, so even a zero/elapsed duration parks once then fires on the next
// resume.
func NewTimerNode(name string, d time.Duration) *Node

// (builder form) — retry/timeout are not meaningful on a timer; do not also WithAction.
func (b *WorkflowBuilder) AddTimer(name string, d time.Duration) *NodeBuilder

// Tick is the host-driven wake API: the host calls it on its own schedule with the
// current instant, and the engine fires any timer that is due at now. fired is true
// iff at least one timer was due and a resume was re-entered (it signals "a resume
// ran", not "the run completed" — key off err for the outcome: nil = completed,
// ErrSuspended = other timers still parked). There is NO mandatory background wake.
func (w *Workflow) Tick(ctx context.Context, now time.Time) (fired bool, err error)

// DueTimers returns the names of armed timers whose persisted fireAt is at or before
// now — a read-only inspection a host loop uses to decide whether to Tick. A
// terminally-failed run reports no due timers (it is never auto-resurrected).
func (w *Workflow) DueTimers(now time.Time) ([]string, error)
```

Time is read through an injectable `Clock` so durable-time is testable without real
sleeping:

```go
// Clock is the single source of "now" for durable-timer logic.
type Clock interface{ Now() time.Time }

func SystemClock() Clock            // production wall clock (the default)
type FakeClock struct{ /* ... */ } // deterministic, test-advanced clock
func NewFakeClock(t time.Time) *FakeClock
func (c *FakeClock) Now() time.Time
func (c *FakeClock) Advance(d time.Duration) // move forward (negative = NTP skew)
func (c *FakeClock) Set(t time.Time)

// Inject a Clock on the Workflow (nil = SystemClock):
func (w *Workflow) WithClock(c Clock) *Workflow
```

### Wait-for-signal and wait-for-condition

```go
// NewWaitForSignalNode builds a declared node: when reached it parks the run
// (Waiting) until a Signal named signalName is delivered to the workflow's durable
// mailbox, then applies the payload idempotently (also surfaced as the node's output)
// and converges. Requires a Store implementing SignalStore.
func NewWaitForSignalNode(name, signalName string) *Node
func (b *WorkflowBuilder) AddWaitForSignal(name, signalName string) *NodeBuilder

// NewWaitForConditionNode ("await") parks while predicate(data) is false,
// re-evaluating on each wake, and converges when it flips.
func NewWaitForConditionNode(name string, predicate func(*WorkflowData) bool) *Node
func (b *WorkflowBuilder) AddWaitForCondition(name string, predicate func(*WorkflowData) bool) *NodeBuilder
```

### Signal delivery

```go
// Signal is one durable mailbox entry. ID is a host-supplied stable,
// unique-per-logical-event identifier (the inbound analog of IdempotencyKey):
// re-delivering the same ID is idempotent (one entry).
type Signal struct {
    ID      string // stable unique-per-logical-event id (host-supplied; dedupe key)
    Name    string // the signal name a WaitForSignalNode waits on
    Payload any    // arbitrary payload (JSON-encoded in the durable stores)
}

// DeliverSignal durably enqueues sig to this workflow's mailbox (enqueue-only). It
// succeeds with no process running and whether or not the instance exists yet
// (early-signal buffering). It does NOT drive the workflow.
func (w *Workflow) DeliverSignal(sig Signal) error

// DeliverAndResume durably enqueues sig and then drives the workflow in-process
// (enqueue then Execute) — the deliver-and-react convenience. The two steps are
// distinct: a crash between them just leaves the signal buffered for the next drive.
func (w *Workflow) DeliverAndResume(ctx context.Context, sig Signal) error
```

Signal delivery is **at-least-once** and consuming is **idempotent-apply**: the
consume ordering is take (non-destructive) → idempotent apply → node `Completed` →
checkpoint → **then** ack, so a crash before the checkpoint re-runs the node and
re-applies the same byte-identical write. Hosts must ack promptly; a mailbox holds at
most 2^20 un-acked entries (over-delivery beyond that is a host-contract violation,
rejected with `ErrCorruptData`).

### SignalStore interface

```go
// SignalStore is an OPTIONAL interface a WorkflowStore MAY implement (additive,
// type-asserted exactly like Checkpointer) to carry a durable signal mailbox. All
// three built-in stores implement it. The mailbox lives OUTSIDE the WorkflowData
// snapshot so an external deliverer's write can never clobber a running checkpoint.
type SignalStore interface {
    DeliverSignal(workflowID string, sig Signal) error   // idempotent by sig.ID; rejects empty ID
    TakeSignals(workflowID string) ([]Signal, error)     // non-destructive read
    AckSignals(workflowID string, ids []string) error    // after-durability drain; idempotent
}
```

### Drive serialization (Locker)

```go
// Locker serializes concurrent drives of the SAME WorkflowID within one process — a
// "drive" is the load→run→checkpoint→save→ack span. Concurrent Tick / Execute /
// DeliverAndResume calls for one WorkflowID take turns rather than racing that span;
// different WorkflowIDs are independent. Cross-PROCESS serialization is deferred to a
// future store lease and remains the host's responsibility until then.
type Locker interface {
    Acquire(ctx context.Context, workflowID string) (release func(), err error)
}

func NewInProcessLocker() Locker            // the default per-WorkflowID mutex locker
func (w *Workflow) WithLocker(l Locker) *Workflow // nil restores the process-wide default
```

See the [Persistence guide → Durable Continuations](../guides/persistence.md) for the
worked patterns (durable sleep, human-in-the-loop approvals).

## Conditional branching (added v0.11.0)

<a id="conditional-branching-added-v0110"></a>
<a id="conditional-branching"></a>

`ChoiceNode` and `MergeNode` add **true workflow-level branching** — one branch of a
choice runs, the rest are [`Bypassed`](#node-status), and a `MergeNode` OR-joins them.
The structure is static and declared (no dynamic graph mutation, no determinism tax);
the branching semantics are machine-checked in TLA+.

### ChoiceNode — `AddChoice`

```go
func (b *WorkflowBuilder) AddChoice(name string) *choiceBuilder
func (c *choiceBuilder) When(pred func(*WorkflowData) bool, target string) *choiceBuilder
func (c *choiceBuilder) Otherwise(target string) *choiceBuilder
func (c *choiceBuilder) DependsOn(deps ...string) *choiceBuilder
```

A `ChoiceNode` is a pure **routing decision**. When it runs it evaluates its `When` arms
in **declared order, first match wins**, activates that one branch's `target`, and marks
every other branch entry (and its reachable subgraph) `Bypassed`. It never blocks and is
itself always `Completed` (it makes a decision — it is never `Bypassed`).

- **Predicate:** `func(*WorkflowData) bool`, **data-only**. It may read only keys produced
  by a **guaranteed-run ancestor** or the seed data — reading an absent/not-yet-produced
  key returns the zero value and falls through to the next arm (never panics).
- **`Otherwise(target)`** is the default, taken when no `When` matches. **With no
  `Otherwise` and no match, the choice fails** with `ErrNoBranchMatched` (wrapped with the
  node name) — a routing dead-end is a typed error, not a silent hang. The downstream then
  cascades to `Skipped` (an upstream you needed failed), which is the honest cause.
- **`DependsOn`** wires the choice's **own** upstream (the nodes that must complete before
  the decision is made). Each `When`/`Otherwise` `target` is wired to depend on the choice
  automatically, independent of node-declaration order.
- A choice builder is deliberately distinct from `NodeBuilder`: no `WithAction` /
  `WithRetries` / `WithTimeout` — its action **is** the routing decision.

### MergeNode — `AddMerge`

```go
func (b *WorkflowBuilder) AddMerge(name string) *mergeBuilder
func (m *mergeBuilder) From(tails ...string) *mergeBuilder
func (m *mergeBuilder) WithAction(action interface{}) *mergeBuilder // Action or func(context.Context, *WorkflowData) error
func (m *mergeBuilder) DependsOn(deps ...string) *mergeBuilder
```

A `MergeNode` is the **OR-join** below a choice's branches. It **fires iff ≥1 taken
branch-tail resolved** — `Completed`, or a `WithContinueOnError()` tail that `Failed`
(both count as a taken path that ran); a `Bypassed` tail is satisfied, not blocking. If
**every** branch was bypassed, the merge is itself `Bypassed` (bypass composes downward). A
non-continue-on-error failure on the taken branch fails the run fail-fast. The taken count
ranges over the recorded `From` tails only — the structural choice-dependency is excluded,
so it cannot vacuously inflate the count.

- **`From(tails...)`** names the branch-tail predecessors to OR-join. May be called more
  than once (tails accumulate).
- **`WithAction`** optionally replaces the default **pass-through** join (which simply
  `Completed`s on fire; downstream reads the taken branch's output from `WorkflowData`).
- The merge depends on its `From` tails **and** the reconvergence-source `ChoiceNode`
  (wired automatically).

```go
wb.AddChoice("route").
    When(func(d *workflow.WorkflowData) bool { amt, _ := d.GetInt("amount"); return amt > 1000 }, "big").
    When(func(d *workflow.WorkflowData) bool { amt, _ := d.GetInt("amount"); return amt > 0 },    "small").
    Otherwise("zero")
// ... branch bodies, each ending at a tail node ...
wb.AddMerge("done").From("bigTail", "smallTail", "zero")
wb.AddNode("after").DependsOn("done")
```

### Reconvergence validation (structured OR-joins only)

Only **structured, single-`ChoiceNode`, local** OR-joins are expressible. `Build()`
returns a typed error for every unstructured shape (the strictness is load-bearing for the
runtime semantics and the exhaustive verification moat):

| Sentinel | Rejected shape |
|---|---|
| `ErrUnstructuredMerge` | a **non-`MergeNode`** reconverges two branches of the same choice (an implicit OR-join — only a `MergeNode` may sit at a reconvergence point); **or** a merge joins tails from **more than one** `ChoiceNode` (cross-Choice merge); **or** a merge joins a `ChoiceNode` tail directly (empty-branch merge — see below). |
| `ErrSharedBranch` | a single node is a branch entry of **two different** `ChoiceNode`s (ambiguous ownership). |
| `ErrDanglingMerge` | a `MergeNode` joins a tail that is under **no** `ChoiceNode`. |

**Not supported (by design):**
- **Unstructured (van der Aalst) OR-join** — arbitrary reconvergence is rejected; every
  OR-join must be a local, single-choice `MergeNode`.
- **Empty-branch merge** — a `Choice → merge` with no intervening node (the tail is the
  `ChoiceNode` itself) is rejected at `Build` (`DEC-M11-P42-EMPTYBRANCH`): a `ChoiceNode`
  is always `Completed`, so it carries no per-branch "was this branch taken?" signal.
  **Workaround:** put a pass-through node on the branch and merge from that.
- **Loops / cycles** — a DAG cannot contain cycles; branching does not add looping.

## Saga / Compensation (added v0.12.0)

<a id="saga--compensation-added-v0120"></a>
<a id="saga-compensation"></a>

A node can declare a **compensating action**; when the run fails, the engine rolls back by
invoking the compensations of `Completed` nodes in **reverse-topological order** to durably
undo their effects. The rollback is itself crash-safe (checkpointed per reverse level), and
the outcome is reported honestly via a typed `*SagaError`.

### Declaring a compensation — `WithCompensation`

```go
func (b *NodeBuilder) WithCompensation(action interface{}) *NodeBuilder
```

Sets the compensating action for a node (accepts an `Action` or a
`func(context.Context, *WorkflowData) error`, the same forms as `WithAction`; an
unsupported type is reported by `Build()`). A node with no compensation is a rollback
no-op. The compensation is exposed on `Node.Compensation`.

### Rollback trigger and scope

- **Triggers** iff `Execute` returns a hard `*ExecutionError` (fail-fast node failure)
  **or** the caller's context is canceled / deadline-exceeded — *and* the DAG declares at
  least one compensation. Does **not** trigger on `ErrSuspended`, a continue-on-error-only
  run (returns `nil`), a persistence/checkpoint error, or a validation/load error. A DAG
  with no compensation anywhere takes the exact pre-saga failure path (zero overhead).
- **Scope:** only a `Completed` node that declares a compensation is compensated, in
  reverse-topological order (within a level, concurrently, bounded by `MaxConcurrency`),
  under a **fresh context** (a caller-cancel triggers rollback but does not abort it). A
  `Bypassed` / `Skipped` / `Waiting` / `Failed` / never-run node is never compensated.
- **Bounded:** the whole reverse pass shares a deadline so a hung compensation cannot hang
  the run.

```go
func (w *Workflow) WithRollbackTimeout(d time.Duration) *Workflow // 0 => DefaultRollbackTimeout (5m); negative => unbounded
```

### At-least-once — compensations MUST be idempotent

Rollback is **at-least-once**: a crash mid-rollback re-runs the compensations of any node
still `Completed`, so a compensation can be invoked more than once. **Compensating actions
must be idempotent.** The engine supplies a stable dedup handle, read inside a compensation:

```go
func CompensationIdempotencyKey(ctx context.Context) (key string, ok bool)
```

The key is `IdempotencyKey(data, nodeName)` (derived only from workflow ID + node name, so
**byte-identical across a resume**); drive downstream deduplication with it so the
re-invocation is one logical undo.

### The outcome — `SagaError`

Rollback is **best-effort**: a compensation that fails (after the node's `WithRetries`
count) does not abort the pass — the node is marked `CompensationFailed`, every other
compensation still runs, and `Execute` returns a `*SagaError`:

```go
type SagaError struct {
    Cause              error       // the failure/cancel that triggered the rollback
    Compensated        []string    // compensation ran and succeeded (effect undone)
    FailedToCompensate []NodeError // compensation attempted and FAILED (effect NOT undone)
    Skipped            []string    // Completed but declared no compensation (nothing to undo)
}
func (e *SagaError) Unwrap() error // returns Cause — errors.As reaches BOTH the *SagaError and the *ExecutionError cause
```

A `*SagaError` is returned **only** when `FailedToCompensate` is non-empty. A rollback in
which every compensation succeeded returns the trigger cause **wrapped behind
`ErrRolledBack`** (since v0.13.0 / REM-03), not a `SagaError` — so `errors.Is(err,
ErrRolledBack)` distinguishes any rollback, and `errors.As(err, &execErr)` still reaches the
cause. A rolled-back run is **never** reported as success (`nil`); if the trigger cause
cannot be reconstructed after a crash, the bare `ErrRolledBack` sentinel surfaces.

The compensation/abort semantics — reverse-topological order, every-Completed-compensated-once,
crash-safe rollback, and the honest partition — are machine-checked in TLA+ (exhaustive under
crashes), with zero determinism tax. See
[ADR-0011](../architecture/adr/0011-saga-compensation-durable-rollback.md) and the
[Saga / Compensation pattern](../guides/workflow-patterns.md#saga--compensation-durable-rollback).

## Composition — sub-workflows & approvals (added M19)

Opt-in nodes that spawn/await a child workflow, and an approval gate. Parent and child are
**distinct workflows** (distinct IDs/journals/sagas; one-writer preserved). The dispatch mode is an
**explicit** builder choice — there is no auto-router; the build-time closure-scan *enforces* the
inline-safety boundary. See [ADR-0018](../architecture/adr/0018-sub-workflow-composition-and-approvals.md)
and the [Sub-workflows & approvals guide](../guides/sub-workflows.md).

```go
// Approval gate — park until an approve/reject decision (signal NAME == node name).
func (b *WorkflowBuilder) AddApproval(name string) *NodeBuilder
func ApproveSignal(node, approver, comment, sigID string) Signal // Approved=true
func RejectSignal(node, approver, comment, sigID string)  Signal // Approved=false → fail-fast
type ApprovalDecision struct{ Approved bool; Approver, Comment string }
type ApprovalRejectedError struct{ Node, Approver, Comment string } // NO Unwrap; classify via errors.As

// Sub-workflow spawn/await — three explicit dispatch modes.
func (b *WorkflowBuilder) AddSubWorkflow(name string, child *DAG) *NodeBuilder       // inline, blocks; non-suspendable child
func (b *WorkflowBuilder) AddSubWorkflowParked(name string, child *DAG) *NodeBuilder // out-of-band, park→wake
func (b *WorkflowBuilder) AddSubWorkflowQueued(name, childType string) *NodeBuilder  // queue (Pool), type-ref/suspendable
func (n *NodeBuilder) WithResult(parentKey, childDataKey string) *NodeBuilder        // child must Set(childDataKey, result)
func (n *NodeBuilder) WithInput(kv map[string]any) *NodeBuilder                      // queued node only — seeds child data
func SubWorkflowCompletionSignal(nodeName, sigID string) Signal                      // bare trigger (no payload)

// Queue path structurally needs an MP *SQLiteStore + Pool + a Registry (type→DAG), injected at Execute:
type Workflow struct{ /* ... */ Registry *Registry; MaxSubWorkflowDepth int /* inline-path override; default 8 */ }

// Optional build-time cycle check — CALL ONCE at Registry assembly, BEFORE the first RunNext/Pool run.
func (r *Registry) ValidateNoTypeCycles() error
```

- **Result:** read the child's **data key** (not a node output) — a scalar (`int64`/string/bool/float)
  is store-uniform; a complex result reloads as a JSON string on FB/SQLite. Collision with a foreign
  parent value → `ErrSubWorkflowResultKeyCollision`.
- **Failure:** child-fail → parent-node fail → parent's own M12 compensation over **parent nodes only**;
  coe-aware (a rollback node counts as failure).
- **WAKE:** completion-signal → `DeliverAndResume`, **no scheduler**. Gate = child **terminal** (for a
  queue child the `work_queue` row is terminal authority), not signal presence.
- **Nesting:** `ErrSubWorkflowMaxDepth` (default 8) on **both** paths — the load-bearing DoS bound. A
  `MaxSubWorkflowDepth` override governs the **inline** path only (`F-P95-02`); the queue path uses the
  package default. `ValidateNoTypeCycles` is an opt-in build-time nicety over the runtime ceiling and is
  **the embedder's responsibility to call** (`F-P95-05`); it catches only directly-declared top-level
  type-ref cycles (`F-P95-04`).
- **Durability:** approvals + the queue wake ride the durable mailbox; **SQLite now implements
  `SignalStore`** (the `signals` table, M19). Supply a **non-empty, stable `sig.ID`** per event
  (`F-P93-SEC-1`).

## Node Status

```go
type NodeStatus string

const (
    Pending   NodeStatus = "pending"   // initial state; also a node never reached
    Running   NodeStatus = "running"
    Completed NodeStatus = "completed"
    Failed    NodeStatus = "failed"     // the node's action returned an error
    Skipped   NodeStatus = "skipped"    // a non-resolving dependency (Failed non-coe, or Skipped) blocked it
    Waiting   NodeStatus = "waiting"    // parked on an external event (timer/signal); NON-TERMINAL, NON-FAILING (added v0.10.0)
    Bypassed  NodeStatus = "bypassed"   // the not-taken branch of a ChoiceNode; TERMINAL, NOT a failure (added v0.11.0)
    Compensated        NodeStatus = "compensated"          // a Completed node durably undone by its compensation in a saga rollback; TERMINAL (added v0.12.0)
    CompensationFailed NodeStatus = "compensation_failed"  // a Completed node whose compensation was attempted and FAILED; TERMINAL; effect NOT undone (added v0.12.0)
)
```

> **`Waiting` (added v0.10.0) is non-terminal and non-failing.** A node parks in
> `Waiting` when it is blocked on an external event — a durable timer's due-time or
> a signal — rather than on an upstream node. A `Waiting` node never causes its
> dependents to be `Skipped`, never trips fail-fast, and is never counted as
> terminal. It drives `Workflow.Execute` to return [`ErrSuspended`](#durable-continuations-added-v0100)
> at the level barrier (the run is not done); re-entering `Execute` on resume
> re-runs the node, which re-parks or wakes. Treat it as runnable, like `Pending`,
> not done. ("Suspend is a crash you chose.")

> **`Bypassed` (added v0.11.0) is terminal and is not a failure.** A node is
> `Bypassed` when it is the **not-taken branch of a [`ChoiceNode`](#conditional-branching-added-v0110)** —
> the routing decision activated a *different* branch, so this node (and its whole
> subgraph) did not run. It is deliberately distinct from `Skipped`: `Skipped` carries
> the failure-diagnostics meaning "an upstream you needed failed/was-skipped", so a
> clean not-taken branch must not be labelled with it (that separation is machine-checked).
> `Bypassed` nodes never appear in an `ExecutionError`. **Diamond rule:** a node with a
> `Bypassed` dependency that **also** has a surviving taken/`Completed` (or
> continue-on-error `Failed`) ancestor is `Skipped`, not `Bypassed` — the taken path wins.

> **`Compensated` / `CompensationFailed` (added v0.12.0) are the saga-rollback terminals.**
> When a run rolls back (see [Saga / Compensation](#saga--compensation-added-v0120)), each
> `Completed` node with a compensation is compensated: `Compensated` if its compensating
> action succeeded (effect undone), `CompensationFailed` if it was attempted and failed
> (effect **not** undone — needs operator attention). Both are terminal and are reached only
> from `Completed`; neither is a forward failure, and neither appears in an `ExecutionError`
> (the partial-rollback outcome is a [`*SagaError`](#saga--compensation-added-v0120) instead).

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

The execution semantics above are verified at two layers (kept current through M10):

- **Layer 1 — property-based tests.** `pkg/workflow/invariants_property_test.go`
  is a [gopter](https://github.com/leanovate/gopter) suite that generates random
  DAGs and asserts the executor's invariants (topological order, peak concurrency
  within `MaxConcurrency`, run-once completeness, dependencies-before-run, and the
  continue-on-error / fail-fast failure semantics) against the **real**
  implementation. It runs as part of `go test ./...`.
- **Layer 2 — TLA+ formal model.** `specs/` contains a TLA+/PlusCal model of the
  level executor and concurrency semaphore (`Executor.tla` + `MCExecutor.tla`),
  TLC-checked for safety (concurrency bound, dependencies-before-run, fail-fast
  halting) and liveness (termination / deadlock-freedom). Milestone M9 adds
  `DurableExecutor.tla` (+ `MCDurableExecutor.tla`), which models crash-resume and
  proves **resume-equivalence** on both arms — `ExecFidelity` (a reported result
  must have actually executed; no phantom checkpoint) and `StatusConvergence` (a
  crash introduces no new terminal state). Milestone M10 adds the durable-continuation
  capstone `M10DurableExecutor.tla` (+ `MCM10DurableExecutor.tla`), which refines the
  model with the non-terminal `Waiting` status, `Suspend`/`Wake`/`FireTimer`/`SendSignal`
  actions, and a `WakeReady`-conditioned `Stuck` arm (the anti-vacuity device that keeps
  liveness from going hollow in the parked state). It is TLC-checked exhaustively at
  `MaxCrashes=1` (a crash at every reachable point) with all M9 safety invariants
  **retained** plus five new ones — `WaitingSound`, `NoDoubleFire`, `NoSignalLost`,
  `NoDoubleApply`, `SuspendPreservesJournal` (and `NoResurrection`) — and the
  `WokeOnlyWhenReady` temporal property; each was mutation-proven to bite. See
  [`specs/README.md`](../../specs/README.md) for the models, the scenarios, and the
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

### Multi-process / competing-consumers domain (added M16)

Returned by the cross-process `ClaimStore` path and the MP-mode `Execute` drive. All three are
`errors.Is`-reachable through the `%w`-wrapped drive return. The **retry-vs-abort** distinction
is load-bearing — `ErrFencedOut` (abort) and `ErrBusy` (retry) are deliberately kept distinct.

```go
// ErrFencedOut: this worker was SUPERSEDED — a re-claim bumped the fencing token, so a write
// under the stale token was rejected (the zombie write never lands). Do NOT retry: a successor
// owns this workflow now. Abort cleanly.
var ErrFencedOut = errors.New("fenced out: stale claim token")

// ErrClaimLost: a Claim the caller did not win — a LIVE lease is held by another owner. The
// competing consumer that lost the race simply does not run this workflow.
var ErrClaimLost = errors.New("claim lost: workflow owned by another worker")

// ErrBusy: TRANSIENT — a write txn could not acquire the SQLite write lock within busy_timeout
// (SQLITE_BUSY). Contention, not supersession — safe to RETRY. Kept errors.Is-distinct from
// ErrFencedOut so "abort" and "retry" never collapse.
var ErrBusy = errors.New("sqlite busy: write lock contended past busy_timeout")
```

Branch with `errors.Is`:

```go
err := wf.Execute(ctx) // wf.WithMultiProcessLocker(ownerID), store WithMultiProcess()
switch {
case errors.Is(err, workflow.ErrClaimLost):  // lost to a live owner — skip, don't retry
case errors.Is(err, workflow.ErrFencedOut):  // superseded mid-run — ABORT, don't retry
case errors.Is(err, workflow.ErrBusy):       // transient contention — safe to RETRY
}
```

### Work-dispatch domain (added M17)

```go
// ErrNoWork: ClaimNext found nothing claimable (empty queue, no type match, or the whole
// claimable set was contended away). NOT an error condition — a poller BACKS OFF (sleeps) on it,
// as opposed to ErrBusy/ErrIO (a transient fault it RETRIES). errors.Is-distinct from both.
var ErrNoWork = errors.New("no claimable work in the queue")
```

`RunNext` translates `ErrNoWork` into `ran=false, err=nil` (nothing to do), so a caller keys off
`ran`, not the sentinel; the `Pool` backs off internally. See the
[Work dispatch guide](../guides/dispatch.md#error-taxonomy).

### Composition domain (added M19)

```go
// Build-time (surfaced from Build): an inline AddSubWorkflow child (or a transitive descendant)
// contains a suspendable node — route it to AddSubWorkflowQueued instead.
var ErrSubWorkflowSuspendableChild = errors.New("inline sub-workflow child contains a suspendable node: ...")

// Run-time configuration (loud, never a silent/in-memory-only spawn):
var ErrSubWorkflowRequiresStore    = errors.New("workflow cannot spawn a sub-workflow: no parent store in scope")
var ErrSubWorkflowRequiresRegistry // ErrValidation-wrapped; a queue node reached with a nil Registry
var ErrWaitRequiresSignalStore     // an approval/parked-await reached with no SignalStore

// Run-time guards:
var ErrSubWorkflowCycle             // child ID collides with an ancestor on the drive stack (would self-deadlock)
var ErrSubWorkflowResultKeyCollision // declared result key collides with a foreign pre-existing parent value
var ErrSubWorkflowMaxDepth          // ErrValidation-wrapped; nesting depth ≥ ceiling (default 8) — the DoS bound
var ErrSubWorkflowTypeCycle         // ErrValidation-wrapped; ValidateNoTypeCycles found a declared A→B→A cycle

// Approval reject (classify with errors.As — NO Unwrap):
type ApprovalRejectedError struct{ Node, Approver, Comment string }
```

`ErrSubWorkflowMaxDepth`, `ErrSubWorkflowRequiresRegistry`, `ErrSubWorkflowTypeCycle`, and the
`AddSubWorkflowQueued`/`WithResult`/`WithInput` misuse errors are `ErrValidation`-wrapped, so
`errors.Is(err, ErrValidation)` is true for them. See the
[Sub-workflows & approvals guide](../guides/sub-workflows.md).

### Saga / rollback domain (added v0.12.0)

```go
// ErrRolledBack marks ANY rolled-back run (since v0.13.0 / REM-03). A clean
// rollback wraps its trigger cause as fmt.Errorf("%w: %w", ErrRolledBack, cause),
// so errors.Is(err, ErrRolledBack) is TRUE for every rollback (clean or the
// crash-un-journaled sentinel case) while errors.As(err, &execErr) still reaches
// the underlying *ExecutionError cause. It is the never-nil floor — a rolled-back
// run is NEVER reported as success.
var ErrRolledBack = errors.New("workflow rolled back")
```

> **Breaking (v0.13.0, REM-03):** before, a clean (fully-compensated) rollback returned
> the bare `*ExecutionError` cause — indistinguishable by error from a plain node failure.
> Now it wraps the cause behind `ErrRolledBack`, so a caller can detect a rollback with
> `errors.Is(err, ErrRolledBack)`. Code that did a bare `err.(*ExecutionError)` type-assert
> on the rollback path must switch to `errors.As(err, &ee)` (which still reaches the cause).
> A **partial** rollback still returns `*SagaError` (unchanged).

The partial-rollback outcome itself is the typed
[`*SagaError`](#saga--compensation-added-v0120) (returned only when ≥1 compensation
failed); it `Unwrap`s to the original trigger cause, so `errors.As` reaches both the
`*SagaError` and, e.g., the `*ExecutionError` that triggered the rollback.

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