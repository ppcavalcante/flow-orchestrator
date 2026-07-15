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

**Best for**: Production use, high-performance needs. For deep durable workflows,
see [Durability modes: Strict vs Batched](#durability-modes-strict-vs-batched)
(`WithDurabilityMode(Batched(K))` group-commit).

### SQLite Store (decomposed, row-based)

Added in **M15**. A `SQLiteStore` persists the run **decomposed into rows** — one row per
node instead of one blob per run — backed by pure-Go `modernc.org/sqlite` (no cgo:
`CGO_ENABLED=0` is preserved):

```go
// Create a SQLite store (path to a .db file; the parent dir is created if absent)
store, err := workflow.NewSQLiteStore("./workflow.db")
if err != nil {
    log.Fatalf("Failed to create store: %v", err)
}
defer store.Close() // SQLiteStore holds a DB handle; close it at shutdown
```

**Best for**: **deep durable workflows** (the row decomposition is the structural fix for
the deep-durable `O(N²)` re-serialize tail — see [Deep-durable cost](#deep-durable-cost--and-the-sqlite-structural-fix)),
and **durable visibility** (indexed "which runs have a `Waiting`/`Failed` node" queries —
see [Indexed visibility](#indexed-visibility-workflowquery)). See
[SQLite store details](#sqlite-store-details-decomposed-row-based) below and
[ADR-0014](../architecture/adr/0014-decomposed-sqlite-store.md).

> **Single-process by default; multi-process is opt-in (M16).** Out of the box `SQLiteStore`
> uses a single-writer connection and an in-process lease — do not point two processes at the
> same `.db` file. To run **competing consumers** (a pool of worker processes sharing one
> `.db`), opt in with `NewSQLiteStore(path, workflow.WithMultiProcess())` and drive via
> `WithMultiProcessLocker(ownerID)` — see [Multi-process safety](#multi-process-safety-competing-consumers)
> below. The default single-process path is byte-for-byte unchanged.

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
wf := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "order-processing",
    Store:      store,
}

// Execute the workflow with persistence
err = wf.Execute(context.Background())
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
    wf := &workflow.Workflow{
        DAG:        dag,
        WorkflowID: workflowID,
        Store:      store,
    }
    
    // Resume the workflow
    return wf.Execute(context.Background())
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
checkpoint). All four built-in stores implement it: the file stores
(`JSONFileStore`, `FlatBuffersStore`) and the row-based `SQLiteStore` write atomically;
`InMemoryStore` checkpoints into its map (useful for tests, but not durable across
process death).

A store that does **not** implement `Checkpointer` keeps the prior behavior
exactly — state is saved only at run boundaries — with zero overhead.

> **Cost of durability scales with depth (v0.12.0 measured).** Each completed level
> barrier triggers a checkpoint that atomically writes the **whole** workflow state, so
> the checkpointing cost grows with the number of levels (the DAG's *depth*) and the
> state size. In the default **`Strict`** mode this is cheap for typical shallow/wide
> DAGs, but material for very deep ones: a **1000-level** durable workflow measured
> around **~10.6s**, dominated not by bytes but by one `fsync` per level (~10ms fixed).
> Durable execution is **not free at depth** — prefer shallow/wide graphs when you can.
>
> **`Batched(K)` (added v0.13.0) removes the per-level `fsync` cost for deep runs:** it
> fsyncs a full snapshot only every `K`th checkpoint (group commit), taking the same
> 1000-level run from ~10.6s (`Strict`) to **~0.25s at `Batched(64)` — ≈43-48×**
> (hardware-varying). See
> [Durability modes](#durability-modes-strict-vs-batched) below. A residual
> `O(N²)`-serialize tail only bites past ~3k sequential levels (pathological for a
> width-parallel engine); a delta/WAL is a possible post-1.0 option if deeper scale is
> needed.

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

### Durability modes: Strict vs Batched

`FlatBuffersStore` supports two checkpoint-flush cadences, selected at construction
with the additive **`WithDurabilityMode`** option (added v0.13.0):

```go
// Strict (the default) — one fsync per completed level. Bit-identical to the
// pre-v0.13 durable contract; omit the option entirely for this behavior.
store, err := workflow.NewFlatBuffersStore(dir) // == WithDurabilityMode(workflow.Strict())

// Batched(K) — group commit: fsync a full snapshot only every Kth checkpoint.
// Removes the per-level fsync cost for deep runs (see the depth-cost note above).
store, err := workflow.NewFlatBuffersStore(dir, workflow.WithDurabilityMode(workflow.Batched(64)))
```

**The tradeoff (choose deliberately — this is a durability contract):**

| | `Strict` (default) | `Batched(K)` |
|---|---|---|
| fsync cadence | every completed level | every `K`th checkpoint |
| deep-run cost | ~10.6s at 1000 levels | ~0.25s at `Batched(64)` (≈43-48×) |
| power-loss loss window | **zero** completed levels | **up to `K`** completed levels |

- **`Batched(K)` weakens power-loss durability by design:** a crash can lose up to
  `K` levels of completed-but-not-yet-fsynced progress. Those levels **re-run** on
  resume — safe because the [at-least-once idempotency contract](#the-at-least-once-contract--side-effects-must-be-idempotent)
  already requires side effects to be idempotent, so re-running ≤`K` levels is
  harmless when you honor that contract. `Batched` is **not** a torn-file risk: the
  only on-disk writes are complete, fsync'd snapshots — a power loss never finds a
  partial file.
- **Floors stay durable regardless of mode:** a **suspend** (`WaitForSignal` /
  `WaitForCondition` park) and a **run completion** force an immediate `fsync` even
  under `Batched`, so a parked or finished run is always durable on disk.
- **`Strict` is the default and the frozen contract** — if you do not opt into
  `Batched`, durability is exactly as it was before v0.13.0 (byte-identical on disk).
- **Only `FlatBuffersStore` batches.** `JSONFileStore` is always full-snapshot,
  `Strict`-only (it is not the deep-durable performance path).

#### SQLite durability modes (`WithSQLiteDurability`, M15)

`SQLiteStore` has the same two-mode contract, selected with its own option type
(`WithSQLiteDurability` — the FB `WithDurabilityMode` is FB-typed and cannot be reused):

```go
// Strict (the default) — every checkpoint commit is power-loss-durable.
store, err := workflow.NewSQLiteStore("./workflow.db") // == WithSQLiteDurability(SQLiteStrict())

// Batched(K) — group commit: a durable flush only every Kth checkpoint. ≤K-level loss window.
store, err := workflow.NewSQLiteStore("./workflow.db",
    workflow.WithSQLiteDurability(workflow.SQLiteBatched(64)))
```

The mapping to SQLite's actual durability model:

| | `SQLiteStrict()` (default) | `SQLiteBatched(k)` |
|---|---|---|
| pragmas | `synchronous=FULL` + `fullfsync=1` (+ WAL) | `synchronous=NORMAL` + `fullfsync=1` (+ WAL) |
| durable boundary | every commit | every `k`th checkpoint via `wal_checkpoint(TRUNCATE)` |
| power-loss loss window | **zero** completed levels | **up to `k`** completed levels |

- **`SQLiteBatched(k)` weakens power-loss durability by design** — a crash loses only the WAL
  frames since the last checkpoint, bounded to **≤`k`** levels. Those levels **re-run**
  idempotently on resume (the same at-least-once contract). `k` is clamped to `≥1`.
- **The suspend/completion floor holds in both modes** — `Sync()` (the `Syncer` capability)
  forces a durable `wal_checkpoint(TRUNCATE)`, so a parked or finished run is power-loss-durable
  regardless of the `Batched` cadence. In `Strict` every commit is already durable, so `Sync`
  is a cheap no-op.
- **darwin needs `F_FULLFSYNC`.** macOS's `fsync` deliberately does *not* flush the drive
  cache, so both modes set `fullfsync=1` to make every fsync (and the checkpoint boundary) a
  real `F_FULLFSYNC` drive flush. On Linux, `synchronous=FULL` uses the platform's native
  power-loss primitive (`fdatasync`); `fullfsync` is a no-op there. The crash-*correctness* is
  platform-portable (CI-gated on Linux); only the fsync *timing* number is darwin-specific.

> **Honest note on the `O(N²)` shape:** the SQLite `Batched(K)` amortization is over the
> **fsync** cost, exactly like the FB group-commit. It does **not** change the deep-durable
> wall-clock *shape* on its own — the `O(N)` forward drive comes from the
> [`IncrementalCheckpointer`](#deep-durable-cost--and-the-sqlite-structural-fix) fast path, not
> from the durability mode. Do not read `Batched` as "makes deep runs `O(N)`".

**Durability contract (SQLite, honest):** `SQLiteStore` provides **exactly-once state
PERSISTENCE and at-least-once side EFFECTS** (idempotency-keyed) — *never* unqualified
exactly-once. A committed checkpoint persists the run's state exactly once; a resumed re-run
of a not-yet-checkpointed level re-executes its side effects (which must be idempotent).

### Saga rollback is crash-safe — and also at-least-once

Added in **v0.12.0**, a saga rollback (see the
[Saga / Compensation pattern](./workflow-patterns.md#saga--compensation-durable-rollback))
runs on the same durable seam. When a run fails and compensations are declared, the engine
persists a durable `rolling_back` marker (and the trigger cause) **before** compensating,
then checkpoints after each reverse level. So a crash **during** rollback resumes straight
back into the rollback drive — never re-running the forward DAG — and finishes the
compensations that had not yet completed.

That makes rollback **at-least-once too**: a compensation for a node still `Completed` at
the crash re-runs on resume, so — exactly like forward side effects — **compensations must
be idempotent**. Read the stable dedup handle inside a compensation with
`CompensationIdempotencyKey(ctx)` (it returns the same `IdempotencyKey(data, nodeName)`,
byte-identical across the resume) and drive downstream dedup with it.

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
b.WithStore(store) // must implement Checkpointer
wf, err := workflow.FromBuilder(b) // FromBuilder returns *Workflow (Build() returns *DAG)
if err != nil {
    log.Fatal(err)
}

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

## SQLite store details (decomposed, row-based)

Added in **M15**, `SQLiteStore` is a third first-class `WorkflowStore` that stores the run
**decomposed into rows** rather than as one blob per file (the FB/JSON model). It is fully
**additive** — it implements the same frozen `WorkflowStore` base, plus the optional
`Checkpointer`, `Syncer`, and the two M15 optional interfaces below. (It does **not**
implement `SignalStore`, so `WaitForSignal` needs one of the other stores — see
[Durable continuations](#wait-for-a-signal-addwaitforsignal--human-in-the-loop).) Backed by
pure-Go `modernc.org/sqlite` (no cgo — `CGO_ENABLED=0` is preserved).

### Schema (per-node rows)

The durable unit is a **row**, not a blob:

| Table | One row per | Holds |
|---|---|---|
| `workflows` | run | run-level scalars: `rolling_back`, `trigger_cause`, `updated_at`. **No `status` column.** |
| `nodes` | node | `status` (a `NodeStatus` string) + `output` |
| `data_kv` | data key | typed KV (a `kind` discriminator preserves the Go type; `int64` on an INTEGER-affinity column) |
| `waits` | parked node | durable timer `fireAt` (M10) |

Two things to know about the model — both are honesty invariants, not implementation trivia:

- **Run status is DERIVED per-node — there is no `workflows.status` column.** A run's
  "status" is a *derivation* over its per-node statuses, not a stored run-level field. The
  visibility queries below derive it from the `nodes` rows.
- **An output-only node stores a `''` (empty) sentinel status, not `Pending`.** A node that
  has an output but no status entry (the blob store's "no status" state) is persisted with an
  empty-string status so `Load` reconstructs *exactly* that state — the decomposed store never
  synthesizes a phantom `Pending`.

`SQLiteStore.Load` reconstructs a `WorkflowData` whose `Snapshot()` is **byte-identical** to
the same data saved through the FB/JSON path (verified by a round-trip oracle and a gopter
property substitution).

### Deep-durable cost — and the SQLite structural fix

The FB/JSON stores checkpoint the **whole** run per level, so a deep DAG re-serializes all
`N` nodes at each of its `N` levels — an `O(N²)` serialize tail (see the
[FlatBuffers depth-cost note](#durability--idempotency-crash-resume)). `SQLiteStore` fixes
this **structurally**: because the durable unit is a row, a checkpoint can `UPSERT` only the
nodes that changed.

The fast path rides the optional **`IncrementalCheckpointer`** interface:

```go
// IncrementalCheckpointer — the executor passes the per-level changed-set; the store
// re-reads and UPSERTs only those keys. O(Δ) compute AND writes = a genuine O(N) forward
// drive (vs the O(N²) full-scan of a plain Checkpointer). Type-asserted, additive.
type IncrementalCheckpointer interface {
    SaveDeltaCheckpoint(changed ChangeSet, d *WorkflowData) error
}

type ChangeSet struct {
    Nodes    []string
    DataKeys []string
    WaitKeys []string
}
```

When the store implements `IncrementalCheckpointer`, `Workflow.Execute` drives it with the
per-level changed-set → a measured **`O(N)` forward drive** (`O(Δ)` compute + writes per
level, rather than re-scanning all `N`). Even without it, the decomposed store's
delta-free `Checkpointer` fallback is already a large absolute win over the M14 blob store
(the committed deep benchmark: a deep-4000 run ≈1.86s vs the M14 FlatBuffers snapshot tail's
≈44.8s at the same depth — ~24× faster, ~1000× less I/O), but that fallback path keeps the
`O(N²)` compute shape. A store that does **not** implement `IncrementalCheckpointer` falls
back to the full `SaveCheckpoint` unchanged.

> **Honest bound:** the `O(N)` win is delivered by the **incremental interface**, not by
> decomposition alone. The plain `SaveCheckpoint` fallback still scans all `N` nodes per level
> (it has no changed-set to work from), so it retains the `O(N²)` compute shape — only the
> `SaveDeltaCheckpoint` forward path is true `O(N)`.

### Indexed visibility (`WorkflowQuery`)

`SQLiteStore` answers durable-visibility questions from **indexes**, not the composition-only
`ListWorkflows` → `Load`-each → decode fallback (which deserializes every run):

```go
// WorkflowQuery — additive optional indexed visibility. Honest primitives over the real
// (derived, per-node) data model; the caller composes run-level buckets. Type-asserted.
type WorkflowQuery interface {
    // ListByNodeStatus returns the DISTINCT workflow IDs with at least one node in status.
    // since>0 additionally filters to updated_at >= since (unix-nanos); since<=0 = no filter.
    ListByNodeStatus(status NodeStatus, since int64) ([]string, error)
    // ListRollingBack returns the workflow IDs currently rolling back (M12), optionally
    // filtered by since (updated_at >= since).
    ListRollingBack(since int64) ([]string, error)
}
```

The design is deliberately **honest primitives** (not contested run-level buckets): "which
runs are waiting" is `ListByNodeStatus(workflow.Waiting)`, "which have a failed node" is
`ListByNodeStatus(workflow.Failed)`; run-level compositions like "terminally failed"
(`Failed ∧ ¬RollingBack`) or "completed" are the **caller's** composition from these
primitives + `ListRollingBack`. `ListByNodeStatus` rejects an unknown status as corrupt input
(`ErrValidation`). Two covering indexes back these queries (`idx_nodes_status`,
`idx_workflows_rolling_back`); the `since` recency filter is a residual on the rows those
indexes already fetch (no separate `updated_at` index — that would be write-amplification for
no read gain).

### SQLite durability modes

`SQLiteStore` has its own durability knob, the analogue of the FlatBuffers
`WithDurabilityMode` — see [Durability modes](#durability-modes-strict-vs-batched) below for
the full `Strict` vs `Batched(K)` contract, which applies to SQLite via
`WithSQLiteDurability(SQLiteStrict() | SQLiteBatched(k))`.

## Multi-process safety (competing consumers)

Added in **M16**. By default `SQLiteStore` is single-process (above). Opt into cross-process
mode and N worker processes can share **one `.db` file** as **competing consumers**: each
claims a distinct workflow, drives it, and safely hands off after a crash — with **no two live
workers ever corrupting one run's journal**. It is fully additive and engages only when you opt
in; the single-process path is untouched. See
[ADR-0015](../architecture/adr/0015-multi-process-safety-leases-fencing.md).

> **Building a job queue on this?** M17 adds an opt-in **work-dispatch** layer on top of this
> fencing model — a durable `work_queue` + `ClaimNext` + a type→factory registry + a worker
> `Pool` that turns the store into a zero-infra distributed job queue. See the
> [Work dispatch guide](dispatch.md). The rest of this section is the fencing *mechanism* dispatch
> is built on.

```go
// Open the store in multi-process mode (per worker process).
store, err := workflow.NewSQLiteStore("./shared.db",
    workflow.WithMultiProcess(),          // enable cross-process leases + fencing
    workflow.WithLeaseTTL(60*time.Second), // optional; default 30s (see sizing below)
)
if err != nil { log.Fatal(err) }
defer store.Close()

dag, err := builder.Build() // ... your DAG ...
if err != nil { log.Fatal(err) }
wf := &workflow.Workflow{
    DAG:        dag,
    WorkflowID: "order-123",
    Store:      store, // the SAME *SQLiteStore instance opened above
}
wf.WithMultiProcessLocker("worker-" + hostID) // claim/fence this workflow for this owner

err = wf.Execute(ctx)
switch {
case errors.Is(err, workflow.ErrClaimLost):
    // A live lease is held by another worker — this consumer didn't win the workflow.
    // Move on to the next one; do NOT retry this workflow.
case errors.Is(err, workflow.ErrFencedOut):
    // We were SUPERSEDED mid-run (a re-claim bumped the token). Do NOT retry — a
    // successor owns this workflow now. Abort cleanly (no partial write landed).
case errors.Is(err, workflow.ErrBusy):
    // Transient write-lock contention past busy_timeout. Safe to RETRY.
case err != nil:
    // other error
}
```

### How it works — leases for liveness, fencing tokens for safety

The model deliberately splits the two halves of the hazard:

- **A lease carries *liveness*** — a `leases(workflow_id, owner_id, expiry, fencing_token)`
  row with an `expiry` says *when* a stalled/dead worker's workflow becomes re-claimable. The
  TTL is a **timing heuristic, never a safety input**: getting it wrong only costs a redo.
- **A monotonic fencing token carries *safety*** — every `Claim` returns a fresh, strictly
  increasing `FencingToken` (a counter, **never** a timestamp — wall-clock is never trusted for
  safety). On **every** checkpoint write, inside the same `BEGIN IMMEDIATE` transaction as the
  row write, the store re-reads the durable token and **rejects the write if the token this
  process holds is stale** (a re-claim bumped it). A superseded (zombie) write returns
  `ErrFencedOut` and is rolled back — it never lands, no matter how long the zombie slept past
  its lease. This compare-and-swap is the keystone: a lease *alone* is unsafe (a worker can
  pause past its lease, get re-claimed, wake, and write); fencing is what makes that write fail.

The oracle is **per-level**: a committed row written under an *older* token is legal durable
state — re-claim resumes *from* it. Only a stale *overwrite of an already-advanced level* is
rejected. That is why re-claim-after-death resumes from the **last committed frontier**, not a
reset to empty: worker A claims (token N), commits level 0, dies; worker B re-claims (token
N+1) and resumes from level-0's committed state, running to completion with the journal
exactly-once.

Renewal is free: the lease `expiry` is bumped **inside the checkpoint transaction**, so a
worker making progress renews automatically with no background heartbeat. The executor wiring
is a thin `Locker` adapter — `Acquire` = `Claim`, `release` = `Release` — so the executor core
is unchanged.

### The honesty contract — exactly-once PERSISTENCE, at-least-once EFFECTS

**Exactly-once state *persistence* across processes; at-least-once *effects*.** Fencing
guarantees the durable journal is written exactly once per level even under competing consumers.
It does **not** make side effects exactly-once: a worker can complete a node's side effect, then
die before its checkpoint commits, and the re-claimer re-runs that node. **Side effects must be
idempotent** — the same at-least-once discipline the crash-resume model already requires (see
[The at-least-once contract](#the-at-least-once-contract--side-effects-must-be-idempotent)).
Never read this as unqualified "exactly-once".

### Same-instance invariant (a footgun to know)

The fencing token this process holds lives in the **store instance's in-memory state**, and the
checkpoint compare-and-swap reads it off `Workflow.Store`. So the store the lock claims through
and the store the drive checkpoints through **must be the identical `*SQLiteStore` instance** —
not two handles on the same file. `WithMultiProcessLocker(ownerID)` **derives** the locker from
`w.Store`, making the mismatch **impossible** — it is the sole public MP entry point, so a
caller cannot hand it a different store instance. (It panics if `w.Store` is not a
`WithMultiProcess` SQLiteStore — fail-loud, never a silently-unfenced drive.) The footgun is
structurally removed from the public API.

### Sizing `WithLeaseTTL`

**Size the TTL above your longest expected single-level compute** (default 30s). A single level
whose compute exceeds the TTL *without* an intervening checkpoint may have its lease lapse, get
re-claimed, and its in-flight work **fenced and redone** — never double-committed. That is a
**liveness** cost (a wasted redo), not a safety failure: the fencing CAS guarantees the
superseded work never corrupts the journal. Undersizing wastes work; it never loses or
duplicates durable state.

### Error taxonomy — retry vs abort

| Sentinel | Meaning | Recovery |
|---|---|---|
| `ErrClaimLost` | A `Claim` you didn't win — a live lease is held by another owner. | Don't run this workflow; move to the next. |
| `ErrFencedOut` | **Superseded** — a re-claim owns this workflow now. | **Do NOT retry.** Abort cleanly (no partial write landed). |
| `ErrBusy` | **Transient** — a write lock was contended past `busy_timeout` (`SQLITE_BUSY`). | Safe to **retry**. |

`ErrFencedOut` (abort) and `ErrBusy` (retry) are kept `errors.Is`-distinct so the two recovery
paths never collapse.

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
