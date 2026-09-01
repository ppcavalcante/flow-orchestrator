# Architecture and execution audit

## 1. Product thesis

The core design choice is coherent:

> Workflow topology is a declared static DAG. Durability is per-node journal/checkpoint state, not deterministic replay of workflow code.

This buys:

- ordinary Go actions;
- no workflow-code replay restrictions;
- no mandatory server or broker;
- file or SQLite durability;
- a graph whose topology can be validated and model-checked.

It costs:

- no arbitrary runtime control-flow construction;
- explicit primitives for choice, fan-out, waits, and composition;
- weaker history than an event-sourced replay engine;
- migration/versioning obligations when a persisted workflow definition changes;
- level-barrier latency and checkpoint granularity.

The phrase “no determinism tax” is accurate if narrowly defined as “no deterministic-replay programming restrictions.” It should not be used to imply zero runtime overhead or deterministic action results. Actions are ordinary consumer code and may use time, randomness, I/O, goroutines, and nondeterministic map iteration.

## 2. Main components

### Definition layer

`WorkflowBuilder` and `NodeBuilder` collect nodes, dependencies, action policy, choice edges, merge edges, boundaries, stores, and execution configuration.

`build()` is the central mint:

1. create a new DAG;
2. fold generated choice/merge dependencies;
3. create Nodes;
4. wire dependencies;
5. cycle-check;
6. validate reconvergence and append structural choice→merge dependencies;
7. clone/validate boundary declarations;
8. stamp `built=true`.

This single-mint idea is right. Its current implementation is not pure because step 2 mutates the reusable builder.

### Compiled graph layer

`DAG` contains nodes, config, feature flags, boundary declarations, provenance stamp, and an embedded `sync.RWMutex`.

Graph topology is externally sealed after M23:

- node fields are private;
- topology mutators are private;
- dependency access is defensive;
- a zero/unbuilt DAG is refused at execution.

The remaining problem is representation: `DAG` is still an exported value type containing a copy-sensitive mutex and a live mutable config.

### Runtime state layer

`WorkflowData` combines four distinct categories behind one mutex and one public object:

1. consumer data (`data`);
2. engine statuses (`nodeStatus`);
3. outputs (`outputs`);
4. engine continuation/saga state (`waits`, rollback marker, trigger cause, delta capture).

This was convenient for early implementation and serialization. It is now the project’s main architectural constraint: consumer actions receive engine-control mutators and engine metadata shares consumer key space.

### Executor layer

`DAG.Execute`:

- checks the build stamp;
- validates cycles;
- projects boundaries;
- computes topological levels;
- initializes statuses;
- executes each level sequentially;
- executes level nodes concurrently;
- resolves fail-fast, continue-on-error, choice/merge, park, cancellation, and checkpoints.

The executor is straightforward enough to reason about, and its status semantics are unusually well documented. The critical scalability limitation is that scheduling is level-barrier-based and goroutine-per-node.

### Durable workflow layer

`Workflow.Execute` serializes drives using a Locker and delegates to `executeLocked`:

- load or initialize state;
- check limited graph identity;
- validate/stamp-check;
- resume rollback or execute forward;
- inject checkpointer, sync, signal, clock, registry, and depth capabilities via context;
- save terminal state;
- acknowledge consumed signals after durable state.

This layering is strong. Context-scoped callbacks avoid shared mutable DAG callback fields and are a good design choice.

### Persistence layer

Four stores exist:

- InMemoryStore;
- JSONFileStore;
- FlatBuffersStore;
- SQLiteStore.

The file stores are full-snapshot persistence. SQLite decomposes state into rows and supports incremental checkpoints, leases, work queue, schedules, caps, query/read model, signals, and dispatch metrics.

### Dispatch and scheduling layer

- `Registry` maps data type strings to DAG factories carrying code.
- `RunNext` claims, rebuilds, seeds, executes, and terminalizes one work item.
- `Pool` creates a store per worker to preserve store-local fencing token isolation.
- `SchedulePoller` scans and fires due schedules.

This is a coherent embedded control plane, but error surfacing and runtime factory validation are insufficient.

## 3. What the architecture gets right

### Static topology is explicit

The core topology is not hidden in control flow. This enables cycle checks, deterministic dependency structure, bounded formal models, and understandable resume state.

### Durability boundaries are explicit

The engine acknowledges that a crash after an external effect but before checkpoint can re-run an action. Stable idempotency keys and at-least-once wording are correct.

### Capability interfaces limit mandatory machinery

`Checkpointer`, `IncrementalCheckpointer`, `SignalStore`, `Syncer`, and claim/query interfaces keep richer behavior optional. A basic direct DAG does not require SQLite.

### Context-scoped runtime injection is safer than mutable shared config

Checkpoint callbacks, clocks, registries, signal stores, and depth values are carried through each drive rather than repeatedly written into a shared singleton.

### SQLite same-instance fencing insight is excellent

The code correctly identifies that the durable token and in-memory `tokenState` must live on the same store instance used for checkpoint CAS. The per-worker store design is based on a real safety invariant.

### Store hardening is layered

Path validation, bounded reads, element limits, JSON/value depth guards, atomic writes, and typed errors show mature defensive design.

## 4. Architectural ceilings

### Level barriers are fundamental, not incidental

A node in level N+1 cannot start until every node in level N finishes, even if its own dependencies finished much earlier. This causes straggler amplification and prevents pipeline parallelism.

Changing this safely requires more than swapping a queue:

- checkpoint semantics become frontier-based rather than level-based;
- fail-fast and cancellation interleavings widen;
- `Skipped`/`Bypassed` propagation changes timing;
- TLA+ models must be updated;
- performance gains need representative straggler benchmarks.

### Static topology does not eliminate semantic versioning

Closures and action objects are code. A graph with identical names but changed code or edges is semantically different. Current identity checking only notices persisted status names removed from the new graph.

### Dynamic primitives form an explicit verification ceiling

Fan-out branches, child DAGs, queue children, and choice bypass behavior are not equivalent to top-level sealed nodes. M23 correctly plans to demonstrate this ceiling, but public language must not imply the seal recursively validates all dynamic behavior.

### The package is becoming a platform

One package now contains definition, execution, four stores, metrics, signals, queues, schedules, caps, read model, and pools. Deployment remains lightweight; conceptual surface does not.

## 5. Recommended target architecture

### Immutable compiled definition

```go
type CompiledDAG struct {
    core   *immutableDAG
    digest DefinitionDigest
}

type Runner struct {
    Definition *CompiledDAG
    Store      WorkflowStore
    Clock      Clock
    Locker     Locker
    // execution configuration
}
```

Benefits:

- copying the public handle is safe;
- mutex/config no longer live inside definition value bytes;
- build output is immutable;
- definition identity has a natural home;
- runtime configuration cannot mutate topology.

### Separate consumer state from engine journal

```go
type DataView interface {
    Get(string) (Value, bool)
    Set(string, Value) error
    Delete(string) bool
}

type engineJournal struct {
    statuses map[string]NodeStatus
    waits    map[string]int64
    rollback rollbackState
    metadata map[string]Value
}
```

Actions should not receive `SetNodeStatus`, `SetRollingBack`, snapshot replacement, or engine metadata keys.

### Canonical value model

Choose one supported set across all stores: booleans, strings, int64, float64, bytes, and canonical JSON/raw value. Reject unsupported aliases at the data boundary or encode them explicitly. InMemoryStore should be able to run in durable-fidelity mode.

### Definition identity

Persist:

- topology digest;
- node policy digest;
- action capability/kind metadata;
- boundary declarations;
- consumer-supplied semantic version for closure code.

Resume should return a typed migration-required error on mismatch.

### Capability metadata

Concrete-type inspection does not compose well. Actions should expose declared immutable capabilities such as suspension, nesting, boundary eligibility, and definition ID. Built-ins and third parties can then compose those capabilities explicitly.
