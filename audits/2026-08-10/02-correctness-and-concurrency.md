# Correctness and concurrency audit

## 1. Executor semantics

### What is strong

- Nine statuses are modeled explicitly, including `Waiting`, `Bypassed`, `Compensated`, and `CompensationFailed`.
- Dependency resolution is centralized in `depResolved`, skip cause in `isSkipCause`, and blocked classification in `classifyBlockedStatus`.
- Cancellation is intentionally given precedence over incidental node errors.
- A park drains the level, checkpoints first, forces sync where needed, then returns `ErrSuspended`.
- Fail-fast aggregates concurrent same-level failures rather than silently retaining only one.
- Continue-on-error is observable through status and does not cancel siblings.
- The BAR oracle now correctly treats both compensation terminal states as evidence of prior completion; this fixed an earlier review finding.

### C-01 — MaxConcurrency does not bound goroutine creation

**Measured defect. High availability/performance.**

The loop calls `go func` for every runnable node and only then acquires the semaphore. A 5,000-node level produced 5,002 goroutines with 16 actions active.

The `DefaultMaxConcurrency` comment explicitly claims the bound prevents “one goroutine per node” explosion. The implementation does exactly that.

**Fix:** use a bounded worker pool or acquire before spawning. Acquiring in the producer loop is simplest but changes cancellation responsiveness; a ready-channel worker pool is cleaner and matches fan-out.

### C-02 — Whole-level barriers amplify stragglers

**Architectural limit. Medium.**

Independent work in later levels waits for unrelated slow nodes. This is correct under current checkpoint semantics but harms latency and throughput.

**Fix:** retain until representative benchmark evidence exists; then design a bounded dependency-ready queue and a frontier checkpoint model. Do not silently change durability granularity.

### C-03 — Action panics kill the process

**Measured/recorded defect. Critical availability.**

Actions execute in worker goroutines without recovery. A caller cannot recover around `DAG.Execute`. Ordinary user panic may be an explicit policy choice, but built-in constructors accept nil operands that predictably panic:

- `NewCompositeAction(nil)`;
- `NewRetryableAction(nil, ...)`;
- `NewMapAction(..., nil)`;
- `NewValidationAction(..., nil, ...)`;
- typed-nil Action values.

Boundary-role validation recognizes these forms, but ordinary nodes remain exposed.

**Fix choices:**

1. always convert panic to a typed node failure, preserving stack;
2. offer an explicit `PanicPolicy` with safe default;
3. at minimum reject all built-in nil/typed-nil forms at Build.

Crashing the embedding process should not be the accidental default for library-supplied invalid constructors.

## 2. Graph construction and sealing

### C-04 — Build mutates reusable builder state

**Executed defect. High correctness/API.**

Choice and merge edges are appended directly to retained dependency slices. Repeated Build duplicates them.

Potential consequences:

- topology changes based on Build count;
- duplicate dependencies alter diagnostics/counts and may change merge behavior;
- registry factories or tests that reuse a builder do not produce identical definitions;
- the graph digest planned for phase 121 would be unstable.

**Fix:** build from cloned dependency slices; never write generated edges to `NodeBuilder`. Alternatively freeze a builder after first Build and return a typed error on reuse. Pure Build is preferable.

### C-05 — `*DAG` satisfies `Action`

**Known executed defect. High availability/composition.**

A built DAG can be passed to `WithAction`, bypassing `AddSubWorkflow` depth, cycle, suspension, and identity checks. A depth-5,000 witness builds and executes.

**Fix:** make compiled graph execution use a method signature that does not satisfy `Action`, or reject `*DAG` and wrapped DAG actions during Build.

### C-06 — Exported DAG value copies can be permanently wedged

**Executed defect. High availability.**

`DAG` contains `sync.RWMutex`. A value copy taken while the original lock is held inherits locked state with no owner/unlocker. The copy retains `built=true` and is admitted, then hangs forever in `Validate`.

The repository’s tagged test intentionally reproduces this and returns green. `copylocks` is not part of the default `go test` vet subset and reflection can evade it.

**Fix:** make the exported object a small pointer handle to an immutable internal core. A `noCopy` marker improves vet diagnostics but does not fix runtime behavior.

### C-07 — Graph identity is a node-name subset check

**Source-proven gap. High durable correctness.**

Resume rejects persisted statuses for removed nodes. It does not reject:

- added nodes;
- edge changes;
- action changes;
- changed retry/timeout/continue policy;
- compensation changes;
- boundary changes;
- suspendability or dynamic primitive changes.

The STABILITY text saying “a changed graph is rejected” is too broad.

**Fix:** durable definition digest plus explicit consumer semantic version.

### C-08 — DAG config can race execution

**Source-proven risk. Medium.**

`WithExecutionConfig` and `WithTracerProvider` write `d.config` without the DAG mutex. `Execute` reads it repeatedly. `Workflow.DAG()` returns the live pointer.

**Fix:** move config to Runner; otherwise lock and snapshot config at Execute entry, and document setters as pre-run-only.

## 3. Boundary model

### What is right

- Names resolve at Build.
- Root-anchored dominance is deterministic and reports a concrete offending path.
- boundary declarations are cloned from the builder.
- verifier/sink mutable CompositeAction values are snapshotted.
- opaque/empty built-in action kinds are refused.
- the oracle explicitly reports unavailable M24 clause-2 coverage.
- the phase-119 J2 clause was not shipped after bounded search showed it would reject legitimate declarations without excluding a demonstrated hazard.

### C-09 — Boundary meaning contradicts J1

**Executed specification/API inconsistency. High.**

The contract repeatedly says a boundary is exactly `Precedence(V,S)` and nothing more: S does not occur before V. J1 then refuses a continue-on-error verifier because, although precedence still holds, the declaration “would constrain order alone.”

BAR-M23 actually requires V to have reached `Completed`, not merely to have occurred. Those are different properties:

- `Precedence`: V starts/runs before S;
- `SuccessfulGate`: V completes successfully before S;
- `Authorization`: S consumes an endorsement authored by V.

The current API name and docs state the first while validation is moving toward the second and M24 toward the third.

**Fix:** choose and name one contract. Recommended: define a verification gate explicitly as successful completion plus later endorsement policy. If `WithBoundary` remains precedence-only, J1 is an over-refusal and should be removed.

### C-10 — Boundary projection collides with user data

**Source-proven defect. High data integrity.**

`projectBoundaries` unconditionally writes `__boundaries__` into consumer `WorkflowData`. The key is unexported and no collision guard exists. A consumer value under that key is silently overwritten.

This is one of several engine metadata keys in the consumer namespace:

- `current_level_<dag>`;
- `__boundaries__`;
- `__fanout_items__:<node>`;
- `<node>.__timedOut__`;
- exported `__fanout_item__` in branch data.

Fan-out result keys have value-aware collision checks; boundary and current-level writes do not.

**Fix:** separate engine metadata from consumer data. Short-term, publish a reserved namespace and reject collisions before execution.

### C-11 — Runtime status forgery defeats operational interpretation

**Measured known gap. Critical authorization scope, deferred M24.**

Actions receive public `SetNodeStatus` and can mark a verifier/sink terminal. The executor skips terminal nodes. The control-flow sentence can remain literally true while the business sink is skipped or the verifier never runs.

**Fix:** actions must receive a data-only view; status/journal mutators become engine-private.

### C-12 — Phase-119 J2 sweep discards execution errors

**Source-proven instrumentation defect. High phase evidence.**

`boundary_vb07_test.go` does:

```go
_ = dag.Execute(...)
runs++
flagEvaluated[...]++
```

The lint gate flags it. A generated graph whose execution regresses to an error is still counted as “run” and contributes a clean no-witness verdict. The equality/floor machinery proves attempted control-flow reached the predicate, not that the behavioral execution completed as intended.

**Fix:** `require.NoError` for every generated accepted graph, or explicitly classify expected execution errors and exclude them from the population. Rename counters if they count attempts. Re-run independent review after this change.

## 4. WorkflowData

### C-13 — “Deep copy” is shallow

**Executed defect. High correctness.**

`Clone` allocates new top-level maps but copies `any` values directly. Nested maps, slices, pointers, and objects are aliased. Comments/tests call this a deep copy without mutation checks.

This breaks:

- InMemoryStore snapshot isolation;
- the promise that Save/Load prevents external modification;
- realistic parity with durable stores;
- race safety when an action mutates a nested object after checkpoint.

**Fix:** either implement a supported-value deep clone or explicitly define values as immutable/caller-owned and stop claiming isolation. For durable workflows, canonical serialization at Set/checkpoint is safer.

### C-14 — Iteration callbacks can deadlock

**Executed defect. Medium API.**

`ForEach`, `ForEachNodeStatus`, `ForEachOutput`, and `ForEachWait` invoke callbacks under `RLock`. A callback calling a writer deadlocks. `ForEachWait` documents it; others are inconsistent.

**Fix:** snapshot entries under lock, release, then invoke callbacks. This also prevents a slow consumer callback from blocking all writers.

### C-15 — Nested values are not thread-safe

**Contract gap. Medium.**

`Set`/`Get` synchronize map slots, not the contents of a map/slice/pointer stored as `any`. “Thread-Safe” in README is too broad.

**Fix:** document slot-level synchronization, adopt immutable/canonical values, or clone on access.

### C-16 — Public total-write and engine-control methods are mixed

`LoadSnapshot`, `LoadFromJSON`, `SetNodeStatus`, `SetRollingBack`, and `SetTriggerCause` are reachable by ordinary actions. M24’s mediation seam must cover outputs as well as data and must treat construction/composite literals, aliases, and transitive calls honestly.

### C-17 — Public IDs/config fields are unlocked

`WorkflowData.ID`, `Workflow.WorkflowID`, Store, Clock, Locker, rollback timeout, depth, and metrics config are directly mutable. This is not inherently wrong for pre-run configuration, but it invalidates package-wide thread-safety language.

## 5. Action API

### C-18 — Legacy action silently discards errors and results

**Executed defect. High API correctness.**

The undocumented legacy callback’s two return values are ignored and the adapter returns nil. An error placed in the second interface marks the node Completed.

**Fix:** remove before 1.0. `WithAction` should accept `Action`; provide a separately named typed helper for function conversion.

### C-19 — Nil handling is inconsistent

Some constructors return typed validation errors; others panic through nil pointer dereference. Public examples include `FromBuilder(nil)` and `RunNext` with nil registry/store.

**Fix:** establish one policy. Library configuration functions should generally return `ErrValidation`; reserve panic for impossible internal invariants.

### C-20 — Timeout middleware can hang forever

This is documented honestly: it starts the action in a goroutine and, after timeout, waits for it to finish. An action ignoring context blocks forever. The name can mislead users into expecting a hard timeout.

**Fix:** retain semantics but rename/document as cooperative timeout. Go cannot safely kill a goroutine; returning early would leak action work and permit concurrent state mutation.
