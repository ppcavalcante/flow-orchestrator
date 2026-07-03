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

## Trust & Safety

The persistence layer has a defined, deliberately bounded trust model. Read it before
loading state that any other process or user can influence. This guide covers the worked
detail; the **authoritative one-statement contract is in
[`STABILITY.md`](../../STABILITY.md#trust--safety--the-persistence-contract)** — read that for
the per-store summary, what is guaranteed, and the honest ceiling.

- **Persistence files and workflow IDs are caller-controlled.** The store reads and
  writes files under the `baseDir` you supply, named by the workflow ID you supply.
  You own that directory and those IDs — they are in your trust boundary, and the library
  does not authenticate, sign, or structurally verify what it loads.
- **Both load paths reject oversized input *atomically with the read* — no stat-then-read
  race.** Every `Load` (both stores) and the `WorkflowData.LoadFromJSON` escape hatch
  read through an `io.LimitReader(cap+1)` and reject
  anything over the size cap (64 MiB) as `ErrCorruptData`. There is no separate `os.Stat`, so
  the file cannot grow between a size check and the read — the reader simply never consumes
  more than `cap+1` bytes (reading one past the limit distinguishes "exactly at cap," accepted,
  from "over cap," rejected). The decoded element count is also capped: a JSON section
  (`data`/`nodeStatus`/`outputs`) or a FlatBuffers vector over ~1M entries is rejected before
  the maps are populated, so a small-on-disk-but-huge-decoded document cannot drive an
  unbounded allocation.
- **`FlatBuffersStore.Load` rejects malformed input *before* structural traversal —
  it does not merely recover from a panic after the fact.** FlatBuffers accessors index
  into the file's own offsets with no bounds checking, so a corrupt file would otherwise
  crash the process. A layered bounds guard runs ahead of the decode: the atomic size cap
  above, a root-offset and minimum-length sanity check, and per-element count caps before each
  load loop. Anything that fails is rejected as `ErrCorruptData` with `data == nil`,
  deterministically and without relying on a panic. The M1 `recover()` remains as a residual
  backstop for deep-offset cases the cheap pre-walk cannot reach. The result: `Load` will not
  panic and will not perform an unbounded allocation on hostile input. (The JSON path gets the
  no-panic property for free — `encoding/json` returns an error rather than panicking on
  malformed input — so the JSON guards are the size and element-count caps above.)
- **The guard checks *structure*, not *meaning* — this is the honest residual.** Go's
  FlatBuffers runtime ships no `flatbuffers.Verifier`, so this is a hand-rolled bounds
  guard, not a full structural verifier. Even with the guard, a *well-formed* file can
  still (a) allocate up to the size cap — bounded, but not free; (b) carry
  semantically-hostile-but-structurally-valid content the guard cannot detect; and (c)
  drive `WorkflowData` into any schema-permitted state. Concretely, a near-complete
  truncation of a valid file can still decode its in-range scalar fields and load as
  in-bounds data rather than being rejected, because FlatBuffers places the root and vtable
  near the front of the buffer. The JSON path likewise loads any schema-permitted state a
  valid (sub-cap) document encodes. The contract promises **"won't panic / won't
  unbounded-alloc,"** *not* "won't load malicious-but-valid data." See ADR-0008 for the
  decision and its residual.
- **`workflowID` is validated as a single safe path segment; traversal IDs are
  rejected.** Every store entry point (Save/Load/Delete on both the FlatBuffers and JSON
  stores) rejects an ID that is empty, contains a path separator, is non-local
  (`..`, absolute paths, volume names), or does not survive a `filepath.Base`
  round-trip. An ID like `../../etc/passwd` is refused with an error, not joined onto
  `baseDir`.
- **The engine is NOT hardened against a determined attacker feeding crafted files.**
  The guarantees above prevent crashes and path traversal; they do not make the
  deserializer adversarial-proof. There is no network, process-exec, or injection surface in
  the persistence path — the only attack surface a file presents is what it decodes into your
  own workflow state. If persistence input can cross a trust boundary, validate and sandbox it
  before loading — do not point a store at a directory an untrusted party can write to and
  assume safety.

See *Persistence fidelity* below for the type-faithfulness limits of a save/load
round-trip.

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

## Durability & Idempotency (crash-resume)

A workflow run can survive a process crash and resume from where it left off,
re-running only the work that had not yet completed. This is **crash-resume**:
durable execution built on a per-node *result journal* mapped onto the static DAG.

### How it works

A `WorkflowStore` MAY additionally implement the optional `Checkpointer`
interface:

```go
type Checkpointer interface {
    // SaveCheckpoint atomically and durably persists the current workflow state.
    SaveCheckpoint(data *WorkflowData) error
}
```

When the store a `Workflow` is given implements `Checkpointer`, `Workflow.Execute`
flushes the run's state to the store **at each completed level barrier** (a
checkpoint). The three built-in stores all implement it: the file stores
(`JSONFileStore`, `FlatBuffersStore`) write atomically; `InMemoryStore` checkpoints
into its map (useful for tests, but not durable across process death).

A store that does **not** implement `Checkpointer` keeps the prior behavior
exactly — state is saved only at run boundaries — with zero overhead.

**Resume is just re-running `Execute`** with the same `WorkflowID`, the same store,
and the same DAG (the `resumeWorkflow` example above already does this). On resume:

- nodes the journal records `Completed` are **skipped**, and their outputs are
  rehydrated from the journal;
- every other node — including any node that was *in flight* when the crash hit —
  **re-runs**;
- if the persisted state references a node the current DAG no longer contains, the
  resume is **rejected** (a graph-identity guard), rather than silently
  mis-resuming a changed graph.

Checkpoint writes are atomic (temp file + fsync + rename): a crash mid-write leaves
either the prior checkpoint or the new one fully intact, never a torn file.

### The at-least-once contract — side effects MUST be idempotent

> **A node that had not reached `Completed` when the crash occurred — including a
> node that was in flight — RE-RUNS on resume.** The crash can land *after* a node
> performed a side effect but *before* its completion was checkpointed, so that
> side effect happens **at least once, possibly more than once**.

This is a contract, not a bug: it is the same at-least-once guarantee Temporal,
DBOS, and Restate all impose, and it cannot be designed away without a
same-transaction database. **Any action with an external side effect (charging a
card, sending an email, calling a non-idempotent API) MUST be made idempotent** so
a re-run is harmless.

The library provides a replay-stable key to drive downstream deduplication:

```go
func IdempotencyKey(data *workflow.WorkflowData, nodeName string) string
```

`IdempotencyKey` returns a deterministic key derived **only** from
`(WorkflowID, nodeName)`. It is byte-identical across a crash-resume re-run — the
original attempt and every resume present the *same* key — so an action can pass it
to a downstream system as an idempotency key and the downstream collapses the
re-execution into one logical operation:

```go
node := workflow.NewNode("charge-card", workflow.ActionFunc(
    func(ctx context.Context, data *workflow.WorkflowData) error {
        key := workflow.IdempotencyKey(data, "charge-card")
        // Pass key to the payment API as its idempotency key; on a resume
        // re-run the same key arrives and the charge is not duplicated.
        return paymentAPI.Charge(ctx, key, amount)
    }))
```

The key deliberately does **not** fold in any retry attempt or timestamp: a resume
re-run is the *same logical attempt*, not a new one. (In-run retries are a separate
concern handled by `RetryableAction`.)

**Stable format (a compatibility contract):** the key is the lowercase hex encoding
of `SHA-256( uint64-LE(len(workflowID)) || workflowID || nodeName )` — 64 hex
characters. The length-frame on the workflow ID makes the field boundary
unambiguous (so `("ab","c")` and `("a","bc")` cannot collide). Downstream systems
may recompute this, so the construction will not change across versions without a
deliberate, documented break.

## Durable Continuations (suspend & resume)

Added in **v0.10.0**, a workflow can **suspend** on an external event and **resume**
later, built on the same crash-resume seam above — "suspend is a crash you chose". A
declared *suspension node* parks in the non-terminal `Waiting` status, the run
checkpoints (carrying `Waiting`), and `Workflow.Execute` returns `ErrSuspended`
(distinguish it with `errors.Is(err, workflow.ErrSuspended)`) instead of `nil`. The
process may then exit. Waking is just re-entering the executor; the parked node
re-runs and either re-parks or converges.

Requirements: suspension needs a `Checkpointer` store (otherwise
`ErrSuspendRequiresCheckpointer`); wait-for-signal additionally needs a store that
implements `SignalStore` (otherwise `ErrWaitRequiresSignalStore`). All three built-in
stores satisfy both.

### Durable timers (`AddTimer`)

A timer node parks until an **absolute** due-time — `clock.Now() + d`, frozen and
persisted at the first encounter, so it survives crash and suspend. An overdue timer
(the process was down past the due-time) fires immediately on the next resume. A
durable timer is a **lower bound** on a wake-up, not a hard real-time deadline.

```go
b := workflow.NewWorkflowBuilder().WithWorkflowID("order-42")
b.AddNode("place-order").WithAction(placeOrder)
b.AddTimer("wait-1h", time.Hour).DependsOn("place-order")
b.AddNode("send-reminder").WithAction(sendReminder).DependsOn("wait-1h")
wf, _ := b.Build()
wf.Store = store // must implement Checkpointer

// First drive: the timer arms + parks, Execute returns ErrSuspended.
if err := wf.Execute(ctx); err != nil && !errors.Is(err, workflow.ErrSuspended) {
    log.Fatal(err)
}

// Later — the host drives waking on its own schedule. Tick fires any timer due at now.
fired, err := wf.Tick(ctx, time.Now())
// fired == true means a resume ran; err == nil means the run completed,
// ErrSuspended means other nodes are still parked.
```

Time is read through an injectable `Clock`. Inject a `FakeClock` to test a
"fire after 3h" flow instantly and deterministically — no real sleeping:

```go
clk := workflow.NewFakeClock(time.Now())
wf.WithClock(clk)
wf.Execute(ctx)            // arms wait-1h, parks
clk.Advance(time.Hour)     // no real time passes
wf.Tick(ctx, clk.Now())    // the timer is now due -> fires -> run converges
```

### Wait for a signal (`AddWaitForSignal`) — human-in-the-loop

A wait-for-signal node parks until a named `Signal` is delivered to the workflow's
**durable mailbox**. Delivery is decoupled from any running process: `DeliverSignal`
succeeds even when nothing is running and even before the instance exists (the signal
is buffered), and it is idempotent by `Signal.ID`.

```go
// The workflow: wait for an approval before shipping.
b.AddWaitForSignal("await-approval", "approval").DependsOn("submit")
b.AddNode("ship").WithAction(ship).DependsOn("await-approval")
// ... first Execute parks at await-approval and returns ErrSuspended ...

// Elsewhere (an HTTP handler, days later): deliver the decision and drive the run.
sig := workflow.Signal{ID: "approval-req-99", Name: "approval", Payload: map[string]any{"ok": true}}
err := wf.DeliverAndResume(ctx, sig) // enqueue then Execute in one call
// (or wf.DeliverSignal(sig) to enqueue only, and wake later with Execute)
```

The consuming node applies the payload **idempotently** and also exposes it as the
node's output (readable by dependents via `data.GetOutput("await-approval")`).
Consuming is ordered take → apply → `Completed` → checkpoint → **ack**, so a crash
before the checkpoint re-runs the node and re-applies the same byte-identical write.
Ack consumed signals promptly: a mailbox holds at most 2^20 un-acked entries.

### Wait for a condition (`AddWaitForCondition`)

Parks while a predicate over the workflow data is false, re-evaluating on each wake
(a host re-drive). Useful when the readiness signal is state another node writes:

```go
b.AddWaitForCondition("await-funded", func(d *workflow.WorkflowData) bool {
    bal, ok := workflow.Get(d, balanceKey)
    return ok && bal >= 100
}).DependsOn("open-account")
```

### Driving many workflows / concurrency

Waking is host-driven — there is **no mandatory background goroutine**. Within one
process, concurrent drives of the *same* `WorkflowID` (a timer `Tick` and a signal
`DeliverAndResume` arriving at once) are serialized by a `Locker` lease (default
in-process, `NewInProcessLocker`); different `WorkflowID`s run independently.
Cross-*process* serialization for one `WorkflowID` remains the host's responsibility.

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

### Persistence fidelity

Integers round-trip faithfully. Both the FlatBuffers store and the JSON store persist
and restore integer values as `int64` with no silent loss — a value of any magnitude
that fits in `int64` is returned intact. (The FlatBuffers store widens its on-disk
integer field via an additive `value_long` column; older `.fb` files written before this
change are still read correctly through a fallback.) On load the FlatBuffers store reads
`value_long` first and only falls back to the legacy field when `value_long` is unset; a
foreign writer that set the legacy field while leaving `value_long` zero would have its
legacy value ignored — every value written by this library sets `value_long`, so
in-library save/load is unaffected.

Read integers back through the typed getters:

- `GetInt64(key) (int64, bool)` is the portable accessor — it returns the full `int64`
  on every architecture, including 32-bit builds. Use it whenever a value may exceed
  `math.MaxInt32`.
- `GetInt(key) (int, bool)` returns the platform `int`. On 64-bit builds this carries any
  stored integer faithfully. On 32-bit builds `int` is 32 bits, so a stored value outside
  the int32 range cannot be represented through `GetInt` — use `GetInt64` for those.

Some values are still **not** a lossless round-trip — read this before relying on stored data:

- **Complex values (maps, slices, structs) are JSON-stringified and read back as a
  `string`.** Their original Go type is not recovered; on load you get a JSON string,
  which you deserialize yourself (see *Custom Type Handling* below).

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
2. **Match the format to the need**: `FlatBuffersStore` is the faster binary
   format for high-throughput or large state; `JSONFileStore` is a fully
   supported store whose human-readable files suit debugging and interoperating
   with external tools that read the JSON directly. Both are first-class.
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
