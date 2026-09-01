# API, security, and operability audit

## 1. Public API assessment

The public package has over one hundred top-level exported declaration lines before counting all methods. It combines several product layers that should not necessarily freeze together.

### What is good

- builders make the common DAG path readable;
- store capability interfaces are additive;
- typed error sentinels use `errors.Is`;
- typed generic keys are optional rather than mandatory;
- constructors generally use functional options;
- child/fan-out ID helpers expose stable framing contracts consumers need;
- pre-1.0 policy explicitly permits cleanup.

### A-01 — Import path bakes repository layout into public API

Consumers import `github.com/.../pkg/workflow`. In Go, `pkg` is usually an internal repository layout convention, not a semantic package path.

**Recommendation:** before 1.0, consider module-root package or `/workflow`. If retained, decide deliberately; moving later is a breaking import migration.

### A-02 — `WithAction(interface{})` defeats type safety

It accepts several unrelated forms, includes an undocumented legacy adapter, and must store an error for later Build. Go’s type system can express the supported surface directly.

**Recommendation:** `WithAction(Action)` and optionally `WithActionFunc(func(... ) error)`. Remove legacy callback.

### A-03 — Public signatures expose unexported types

Examples include `choiceBuilder`, `mergeBuilder`, and `fanOutExpander`. Chaining works, but consumers cannot name all types for wrappers, fields, interfaces, or documentation.

**Recommendation:** export stable builder interfaces/types or return `*NodeBuilder` where possible.

### A-04 — `TopologicalSort` has the wrong shape

It returns `[][]*Node` but always returns a single flat inner slice; `GetLevels` is the grouped API. This has already produced documentation confusion.

**Recommendation:** return `[]*Node`, or unexport it.

### A-05 — Configuration styles are inconsistent

Some settings live on builder, some on exported Workflow fields, some have fluent setters, and some comments reference nonexistent builder methods (`WithMetrics`). `FromBuilder` carries only a subset of conceptual runtime configuration.

**Recommendation:** make Runner options the single execution-environment configuration surface.

### A-06 — Panic/error policy is inconsistent

The library alternates among typed validation errors, delayed Build errors, and panics for ordinary misuse. Public nil and MP-store mismatch examples demonstrate this.

**Recommendation:** publish a panic policy. Public API should return errors for caller-controlled configuration; panic should mean internal invariant violation only.

### A-07 — Thread-safety contract is not per type/method

README says “Thread-Safe,” while:

- config setters race execution;
- nested data values are aliases;
- metrics reads have a documented race window;
- callback iteration can deadlock;
- direct DAG drives can interleave on shared data;
- non-Unix file mailbox locking is process-unsafe.

**Recommendation:** add a concurrency section to every major type’s docs: safe concurrent operations, pre-run-only mutation, ownership, callback reentrancy, and cross-process scope.

## 2. Security model

### What is appropriately scoped

The project is an embedded library with no listener by default. Its direct attack surface is mainly:

- persisted bytes/database rows;
- workflow/signal/schedule IDs;
- host-supplied action/data values;
- resource exhaustion;
- multi-process coordination.

The docs correctly say persisted data is not authenticated and the caller must protect the directory/DB.

### S-01 — Complete mediation is not yet true operationally

Actions can write statuses, outputs, engine keys, snapshots, rollback flags, and boundary projection data. M23’s guarantee is structural/control-flow only. Security terminology such as “complete mediation” invites a stronger interpretation than shipped behavior.

**Recommendation:** until M24 lands, use “structural precedence validation” publicly. Reserve “complete mediation” for a system where all relevant mutation paths are actually policy-controlled.

### S-02 — Approval authorization is under-specified

Approval is currently a named signal/wait primitive, not an authorization protocol. Missing elements include:

- freshness;
- principal identity;
- request correlation/nonce;
- policy version;
- cryptographic or host-authenticated provenance.

**Recommendation:** document Approval as orchestration only. Provide hooks/data model for host-authenticated endorsements rather than imply the engine authenticates approvers.

### S-03 — Resource limits are inconsistent

Strong limits exist for file bytes/elements/depth, fan-out width, concurrency caps, and sub-workflow depth. Gaps remain:

- static DAG width/goroutines;
- node count and edge count at Build;
- Pool size has no cap by policy;
- arbitrary action memory/CPU;
- schedule due-set scan size;
- number of workflow data keys before in-memory use.

**Recommendation:** define resource-budget ownership. Add optional Build node/edge/width caps and bounded scheduler batch sizes.

### S-04 — Local supported toolchain can be vulnerable

The release toolchain is patched, but contributor docs still say Go 1.24 and local Go 1.25.1 is vulnerable. The `go` directive is a language/module floor, not a safe default toolchain.

**Recommendation:** `go 1.25.0` plus `toolchain go1.25.11` (or newer patched release), keep CI floor arm for compatibility, and update contributor docs.

### S-05 — JSON payload identity can cross workflow boundaries

See P-02. Even within a trusted directory, accidental file placement can redirect final saves. The file key should define identity.

### S-06 — Multi-process safety depends on unvalidated identity/factory contracts

Empty owner, same-owner concurrency, and shared StoreFactory output can invalidate fencing assumptions without immediate errors.

**Recommendation:** move these assumptions into constructors and runtime checks.

## 3. Operability

### What is good

- query/read-model APIs exist;
- dispatch metrics and OTel bridges exist;
- typed work states and dead-letter behavior exist;
- cancellation is represented durably;
- deterministic IDs let operators locate child/branch journals;
- error messages often name workflow/node/path.

### O-01 — Long-running loops hide infrastructure health

Pool and SchedulePoller intentionally retry, but expose too little information to the embedding host. A service should be able to answer:

- Is the DB unreachable?
- Are claims permanently misconfigured?
- Are schedules repeatedly failing?
- Is one workflow poisoning every retry?
- Are worker goroutines alive but doing no useful work?

**Recommendation:** add a small operational observer interface:

```go
type RuntimeObserver interface {
    OnClaimError(error)
    OnWorkflowResult(WorkItem, error)
    OnScheduleError(scheduleID string, err error)
    OnWorkerState(workerID string, state WorkerState)
}
```

Keep no-op default. Also expose health snapshots and consecutive-error counters.

### O-02 — Pool Run error semantics are too narrow

It returns store-open errors only; persistent runtime failures look like clean operation. A context-cancelled shutdown returns nil, which is reasonable, but must not hide prior fatal configuration failure.

### O-03 — No built-in definition migration hook

Once graph digesting lands, users need a way to:

- reject;
- explicitly accept compatible additive change;
- transform persisted state;
- fork workflow version/ID.

A typed mismatch with no migration story will be safe but operationally painful.

### O-04 — Current-level engine metadata is low-value shared-state churn

Every level writes `current_level_<dag>` into user data and persists it. This increases delta/checkpoint writes and collides with user keys. The read model already has node status; current level can be derived or stored in engine metadata.

## 4. Formal/security wording

“Formally verified engine” is too strong. The repository has model-checked TLA+ abstractions plus implementation property tests. `specs/README.md` correctly states that model↔Go faithfulness is human-reviewed.

Recommended public phrase:

> “Core scheduling, durability, queue, saga, fan-out, and scheduling algorithms have model-checked TLA+ specifications, complemented by property tests against the Go implementation.”

That is still a differentiator and is more credible.

## 5. Stability policy

`STABILITY.md` says the exported intended-public packages are public, then says an exported symbol may not be supported if “plainly internal” and docs are authoritative. That ambiguity defeats a consumer’s ability to know what may be used.

**Recommendation:**

- every exported declaration in intended-public packages is supported unless explicitly marked `Experimental`/`Deprecated`;
- move infrastructure that is not supported to `internal`;
- publish a generated API manifest and diff it in CI;
- define data-format compatibility separately, as the project already tries to do.
