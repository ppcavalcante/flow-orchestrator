# Persistence, dispatch, signals, and scheduling audit

## 1. Persistence strengths

The persistence layer is one of the strongest parts of the project.

### File stores

Positive properties include:

- workflow ID path-segment validation;
- bounded reads using `LimitReader(cap+1)`;
- element-count ceilings;
- pre-marshal value-depth checks and post-marshal JSON-depth checks;
- atomic same-directory temp write, file sync, close, and rename;
- typed not-found/corrupt/I/O/validation errors in key paths;
- file mode preservation and symlink-replacement hardening;
- malformed FlatBuffers recovery net plus cheap structural bounds.

### SQLite

The decomposed SQLite implementation has real engineering depth:

- row-oriented state;
- incremental checkpoints;
- transaction classification;
- multi-process `BEGIN IMMEDIATE` arbitration;
- monotonically increasing fencing tokens;
- lease renewal/reclaim;
- queue, cancellation, caps, scheduling, and read model;
- corrupt-row status/kind validation;
- pure-Go `modernc.org/sqlite` and CGO-free cross-build.

### Durability semantics

The at-least-once frontier is explicit. Checkpoints occur at successful barriers; actions not durably Completed may rerun. Saga compensation is also correctly documented as idempotent/at-least-once.

## 2. Store correctness gaps

### P-01 — InMemoryStore does not isolate nested state

**Executed defect. High.**

`Save` and `Load` call shallow `Clone`. A nested map changed after Save changed the stored snapshot; mutating a nested map returned by Load also changed subsequent Loads.

This means InMemoryStore is not a faithful test substitute for durable stores and can race with caller mutation.

**Fix:** canonical clone of supported values, or serialize/deserialize in “durable fidelity” mode. At minimum, make the contract explicit and add alias-mutation tests.

### P-02 — JSON lookup key and payload ID can disagree

**Executed defect. High.**

`JSONFileStore.Load("A")` accepted a file whose snapshot ID was `B` and returned `WorkflowData.ID == "B"`.

Impact in `Workflow.executeLocked`:

1. workflow A asks Store.Load(A);
2. returned data says B;
3. actions run under mixed identity;
4. final Store.Save(data) writes using data ID B.

A misplaced, copied, or forged A file can redirect writes into another valid workflow ID. FlatBuffers initializes ID from the lookup key and does not have this exact behavior; SQLite is row-keyed.

**Fix:** lookup key is authoritative. Reject payload mismatch as `ErrCorruptData`, or omit duplicated ID from payload on a future format version.

### P-03 — Durable store value fidelity is inconsistent

**Known measured gap. High developer correctness.**

- InMemory preserves host Go values and aliases.
- JSON normalizes integer types and retains nested JSON structure.
- FlatBuffers and SQLite use scalar tables and JSON-string fallback for complex values.
- complex outputs may reload as strings instead of original maps/slices.

A workflow can pass against InMemory and fail or type-switch differently in production.

**Fix:** canonical public value contract and cross-store contract tests. Do not describe stores as interchangeable without qualifying value fidelity.

### P-04 — Unknown status handling differs by backend

**Source-proven gap. Medium/high.**

- SQLite rejects unknown statuses as corrupt.
- JSON accepts arbitrary status strings into `NodeStatus`.
- FlatBuffers maps unknown enum values to Pending.

Unknown/corrupt terminal state can therefore cause a node to rerun rather than fail closed.

The security docs acknowledge semantically forged data can load, but backend divergence remains dangerous.

**Fix:** define a shared strict `isKnownStatus` decode policy and use it in all durable stores. If leniency is needed for forward compatibility, version the status schema explicitly.

### P-05 — Metrics configuration disappears after durable Load

**Executed defect. High observability.**

A workflow with enabled `MetricsConfig` produced an enabled collector on first JSON run and a disabled collector on resume. The loaded `WorkflowData` replaces the preconfigured object and stores do not persist execution-environment metrics config.

InMemory happens to preserve config through Clone, so tests cover the backend that works and miss file/SQLite behavior.

**Fix:** after Load, rebuild/attach a collector from `Workflow.MetricsConfig`; metrics config belongs to the Runner, not persisted state. Add resume tests for every store.

### P-06 — `Load` nil/nil is treated as fresh state

**Source-proven contract gap. Medium.**

A third-party Store returning `(nil, nil)` is silently treated as no prior data. The interface does not state whether nil/nil is legal.

**Fix:** reject as a typed store contract violation. `ErrNotFound` is the explicit fresh-state signal.

### P-07 — Error taxonomy is not uniform at all edges

Examples:

- unsupported values can surface raw JSON marshal errors rather than `ErrValidation`;
- ListWorkflows and constructors often return unclassified filesystem errors;
- public `WorkflowData` file helpers wrap errors outside store taxonomy.

The core categories are good; documentation should say where they apply, and store implementations should use them consistently.

## 3. Multi-process and fencing

### P-08 — Empty owner ID defeats process distinction

**Executed safety defect. Critical/high.**

Two independent multi-process store instances using empty owner ID both acquired the same live lease with the same token. `claimLocked` treats equal owner strings as re-entrant.

`NewPool` validates non-empty owner prefixes, but direct `Claim` and `WithMultiProcessLocker` do not.

**Fix:** reject empty owner everywhere. Consider a typed `OwnerID` constructor. Document and test uniqueness expectations. The library cannot prove global uniqueness, but it can reject the most dangerous invalid value.

### P-09 — Same-owner concurrent drives are not composed with local locking

**Declared residual. High if misused.**

`WithMultiProcessLocker` replaces the in-process locker. Same `(workflowID, ownerID)` claims are re-entrant and do not serialize goroutines in one process. The comment tells the host to drive one at a time or compose a locker, but no public composition helper is provided.

**Fix:** the multi-process locker should internally acquire a process-local per-ID lock, then claim the durable lease. Correctness should be default, not an assembly exercise.

### P-10 — Claim ignores caller cancellation

`claimLocker.Acquire` ignores ctx; `SQLiteStore.Claim` uses `context.Background`. Busy timeout or DB stalls can outlive the workflow request.

**Fix:** context-aware Claim variant and propagation through Locker.

### P-11 — Public API panics on store mismatch

`WithMultiProcessLocker` panics when Store is not ClaimStore. This is ordinary configuration error.

**Fix:** return error, or construct the workflow through a validated option/builder that cannot represent mismatch.

## 4. Queue and Pool

### What is right

- Registry prevents silent duplicate overwrite.
- empty registry does not claim arbitrary work;
- claims are type-filtered;
- input seed is fresh-only to avoid wiping a reclaimed journal;
- per-worker stores preserve tokenState isolation;
- cancellation leaves in-flight work reclaimable instead of poisoning it;
- retry/dead-letter and parent wake-up paths are explicit.

### P-12 — Pool silently accepts a non-MP store factory

**Executed defect. High operability/correctness.**

The StoreFactory contract says each store MUST use multi-process mode. `runWorker` never validates it. `ClaimNext` returns permanent `ErrValidation`; the worker discards the error, sleeps, and repeats. `Pool.Run` eventually returns nil on cancellation.

A production worker can therefore be alive, healthy-looking, and process zero work forever.

**Fix:** immediately validate `store.dur.mp`; return a worker startup error. This is configuration, not transient contention.

### P-13 — Shared store instances are prohibited only by prose

The Pool safety argument requires each factory call to return a distinct `*SQLiteStore`. A factory returning the same pointer to every worker collapses tokenState isolation and causes multiple deferred Close calls.

**Fix:** Pool startup should track returned pointers and reject duplicates before work begins. Better: accept path/options and own store construction rather than accepting an unverifiable factory contract.

### P-14 — Runtime claim/store errors are swallowed

`runWorker` discards every `runNext` error. The intent is to continue after per-item failures and transient contention, but the implementation also hides permanent configuration and persistent infrastructure failures.

**Fix:** classify:

- handled item error: continue, emit event;
- `ErrBusy`: retry/backoff;
- permanent validation/config: stop worker, return error;
- persistent I/O: emit health event and trip after configurable threshold.

Add an error callback/channel and health counters.

### P-15 — Nil public dispatch dependencies panic

`RunNext` dereferences nil Registry/Store. Nearby constructors validate nils.

**Fix:** typed validation at public boundary.

## 5. Signals and waits

### Strong behavior

- mailbox is separate from snapshot, avoiding delivery/checkpoint overwrite;
- signal IDs are deduplicated;
- take/apply/checkpoint/ack ordering is explicit;
- ack failure leaves inert state rather than failing a durable completion;
- Unix file mailbox operations use a dedicated lock;
- payload size/decode paths are guarded.

### P-16 — No signal freshness metadata

**Known security/product gap. High for approvals.**

Public Signal has no enqueued/delivered timestamp. SQLite stores time but does not expose it. Old buffered signals can satisfy new waits/approvals.

**Fix:** include immutable delivery metadata and let a declaration specify minimum delivery time or correlation generation. An approval should usually bind to a request/nonce, not name alone.

### P-17 — Non-Unix file mailbox locking is a no-op

**Declared platform defect. High portability.**

The source documents over-cap, ack/redelivery, and delete/delivery races. Windows is only cross-compiled, not behavior-tested.

**Fix:** implement/test LockFileEx, or explicitly declare multi-process file mailboxes Unix-only and require SQLite elsewhere.

### P-18 — Engine disposition keys share user namespace

Signal-timeout writes `<node>.__timedOut__`. This is documented, but still collides with arbitrary user keys and is part of the broader data/journal separation problem.

## 6. Scheduling

### Strong behavior

- cron parser is dependency-free and fuzzed;
- schedules are durable;
- many pollers race through fenced fire;
- concurrency caps are transactionally enforced;
- polling is opt-in and embedded.

### P-19 — SchedulePoller suppresses all operational errors

Scan and fire errors are discarded and retried forever. `Run` returns only clean cancellation. A broken DB can look like an idle scheduler.

**Fix:** expose error events/metrics and classify permanent errors. Retrying is correct; invisibility is not.

### P-20 — `WithCatchupOnce` is public but intentionally a no-op

The docs now call it reserved, which is honest, but a no-op option in a public API remains confusing and expands the future compatibility surface.

**Fix:** remove before 1.0 until behavior exists, or return an explicit unsupported error at schedule creation.
