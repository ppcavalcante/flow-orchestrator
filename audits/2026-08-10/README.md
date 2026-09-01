# Flow Orchestrator — independent deep audit

**Audit snapshot:** 2026-08-10  
**Code subject:** branch `anvil-m1`, commit `c9aef0c499f21f90a168190c75bdf310182cf001`  
**Published release:** `v0.21.0-alpha` at `4108150`  
**Starting map:** `.planning/REVIEWER-BRIEFING.md`

This directory is a new, independent audit bundle. The reviewer briefing was used as a map, not as evidence to repeat. Claims below were checked against source, executable probes, repository history, CI, documentation, and planning artifacts.

The bundle lives under `audits/`, plural. During this audit another seat added an uncommitted `audit/*` ignore rule, so the original singular directory was moved here to keep the requested deliverable visible to Git.

## Documents

1. [`00-evidence-and-method.md`](00-evidence-and-method.md) — snapshot, commands, probes, limitations, and corrections to the briefing.
2. [`01-architecture-and-execution.md`](01-architecture-and-execution.md) — system model, control flow, strengths, and architectural ceilings.
3. [`02-correctness-and-concurrency.md`](02-correctness-and-concurrency.md) — executor, graph sealing, boundaries, data state, panic, deadlock, races, and scale.
4. [`03-persistence-dispatch-and-scheduling.md`](03-persistence-dispatch-and-scheduling.md) — stores, crash semantics, SQLite fencing, queues, pools, signals, and scheduling.
5. [`04-api-security-and-operability.md`](04-api-security-and-operability.md) — public API, threat model, authorization, portability, and operator experience.
6. [`05-verification-ci-and-process.md`](05-verification-ci-and-process.md) — tests, TLA+, phase 119, CI, lint, coverage, planning, and evidence quality.
7. [`06-documentation-and-release.md`](06-documentation-and-release.md) — public-doc drift, release ancestry, stability promises, and examples.
8. [`07-findings-register.md`](07-findings-register.md) — prioritized, traceable findings with evidence class and proposed resolution.
9. [`08-remediation-roadmap.md`](08-remediation-roadmap.md) — staged plan and proposed target architecture.

## Executive judgment

Flow Orchestrator has a credible and unusually sophisticated durable-execution core. The project is strongest where it has spent the longest: per-level checkpoint/resume, store hardening, SQLite lease/fencing behavior, continuation primitives, and adversarial verification. The team is unusually candid about failed checks and bounded proofs.

The current development branch is nevertheless **not releasable**. This is not because the entire core is unsound. It is because the branch currently combines:

- open host-process panic paths;
- a consumer-reachable copied-mutex permanent deadlock;
- incomplete graph identity;
- a multi-process owner-identity safety footgun;
- red lint and vulnerable local toolchain evidence;
- a package gate with two known timing flakes;
- an unprotected coverage target that CI itself invokes;
- a phase-119 closure instrument that discards execution errors;
- release/version ancestry divergence;
- stale and contradictory public/planning documentation.

The project’s next challenge is no longer feature depth. It is **making runtime contracts, evidence, docs, planning, and release lineage agree**.

## What is especially right

- The static-DAG/no-replay product thesis is coherent and differentiated.
- At-least-once action semantics and idempotency obligations are documented honestly.
- File input bounds, traversal guards, atomic writes, typed store errors, and SQLite fencing are materially stronger than typical alpha code.
- Optional interfaces keep the basic embedded path separate from SQLite dispatch machinery.
- Fan-out uses a bounded worker pool rather than one goroutine per item.
- Tests include property, fuzz, subprocess, kill, corruption, differential, and model-mutation evidence.
- The planning history preserves corrections instead of rewriting failures into a clean narrative.

## What is most wrong

### 1. “Bounded concurrency” does not bound goroutines

`executeNodesInLevel` creates one goroutine per runnable node and acquires the semaphore inside each goroutine. A 5,000-node level produced **5,002 goroutines while only 16 actions had started**. The comment on `DefaultMaxConcurrency` says the bound prevents goroutine explosion; that statement is false.

### 2. A legal copied DAG can hang forever

The tagged adversarial gate reproduced a stamped DAG copy inheriting a held `sync.RWMutex`; `Validate` then blocks forever. The exported API reachability hunt observed **1 wedged copy in 3,000 attempts**. The test passes because it demonstrates the defect. This violates the project’s own “no input hangs” hard bar and is deferred rather than fixed.

### 3. In-memory snapshots do not isolate nested values

`WorkflowData.Clone` shallow-copies `data` and `outputs`, despite calling itself a deep copy. `InMemoryStore.Save` and `Load` therefore do not prevent external modification of map/slice/pointer values. Executed probes showed both mutation-after-save and mutation-after-load altering the stored snapshot.

### 4. Multi-process ownership can silently collapse

Two independent multi-process `SQLiteStore` instances claiming the same workflow with `ownerID == ""` both succeeded with token `1`. They were treated as the same re-entrant owner. `NewPool` rejects an empty owner, but `Claim` and `WithMultiProcessLocker` do not. Distinct-process safety currently depends on prose and caller discipline.

### 5. Current CI truth is red or unproven

- Clean-cache lint: **22 findings**.
- Local `govulncheck` on Go 1.25.1: **2 called stdlib vulnerabilities**.
- Two known nondeterministic tests weaken a single package-wide green.
- `test-coverage-focused` has no `-timeout`, while the suite exceeds the default locally; the CI coverage job calls that exact target.
- No remote CI run covers the current M23 branch.

### 6. User data and engine metadata share one unprotected namespace

The engine writes keys such as `current_level_<dag>`, `__boundaries__`, `__fanout_items__:<node>`, and `<node>.__timedOut__` into the same map consumers own. Some are hidden and some overwrite values without a collision guard. Actions can also write engine statuses directly. M24’s data-plane split is not optional polish; it is the architectural repair.

## Novel measured defects found in this audit

These were not merely copied from the briefing:

| Finding | Executed result |
|---|---|
| Builder reuse mutates definition | first `Build`: 1 generated dependency; second: 2 |
| `WorkflowData.Clone` aliases nested map | source mutation appeared in clone |
| InMemoryStore isolation is false | mutation after Save and after Load changed stored value |
| JSON store trusts payload ID over lookup key | `Load("A")` returned data with ID `"B"` |
| Enabled metrics disappear on file-backed resume | first run enabled=true; second run enabled=false |
| Legacy action silently loses error | returned `lost-error`; execution returned nil and status Completed |
| Boundary contract contradicts J1 behavior | structurally ordered CoE verifier refused because it “constrains order alone” |
| Public nil handling inconsistent | `FromBuilder(nil)` and `RunNext(..., nil registry, ...)` panic |
| Empty multi-process owner collapses identity | two stores both claimed token 1 successfully |
| Non-MP Pool factory is silently accepted | `Pool.Run` returned nil on cancellation after swallowing permanent ClaimNext configuration errors |
| Iteration callback can deadlock | `ForEach` callback calling `Set` did not complete |
| Static executor creates parked goroutine explosion | 5,000 nodes → 5,002 goroutines, 16 active actions |

## Release decision

**No-go for `v0.22.0-alpha` from the current tree.**

Minimum release conditions are in [`08-remediation-roadmap.md`](08-remediation-roadmap.md). In short: fix P0 runtime safety, make current CI genuinely runnable, close phase 119 on an immutable final SHA, reconcile release ancestry/versioning, and publish an honest boundary/data-plane scope.
