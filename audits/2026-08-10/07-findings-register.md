# Findings register

## Legend

- **Critical:** process safety, durable corruption, or core safety property; blocks release.
- **High:** material correctness/security/availability/release failure; normally blocks release.
- **Medium:** important API, operability, test, or maintainability debt.
- **Low:** bounded inconsistency or cleanup.
- **Measured:** executed by this audit or repository adversarial evidence.
- **Source-proven:** direct control/data-flow inspection.
- **Inferred:** credible risk needing a discriminating test.

## P0 — release blockers

| ID | Severity | Evidence | Finding | Resolution |
|---|---|---|---|---|
| AUD-001 | Critical | Measured | Built-in nil/typed-nil actions can panic executor goroutines and kill host | Validate at construction/Build; define panic policy |
| AUD-002 | High | Measured | Exported DAG value copied during drive can inherit locked mutex and hang forever | Public handle to immutable pointer core; no mutex in copied value |
| AUD-003 | High | Measured | Empty MP owner lets independent stores share same live token | Reject empty owner; typed owner; compose local+durable lock |
| AUD-004 | High | Measured/source | Current lint gate has 22 findings; SECURITY says zero | Fix or justified narrow suppressions; enforce zero |
| AUD-005 | High | Source/measured history | Two package test flakes make one green a sample | Make cancellation deterministic; move wall-clock check out of unit gate |
| AUD-006 | High | Source | CI coverage invokes unprotected race target; current suite exceeds default locally | Central timeout variable; atomic profile generation |
| AUD-007 | High | Source | Phase-119 final SHA lacks independent passing review/QA; latest records fail older ranges | Freeze SHA; repair findings; fresh independent review then QA |
| AUD-008 | High | Source | J2 sweep ignores DAG.Execute errors but counts them as runs | Assert successful execution or classify/exclude errors |
| AUD-009 | High | Source/history | Current development line does not descend from 0.21 release and lost release metadata | Merge release lineage; generated version consistency |
| AUD-010 | High | Source | Graph identity does not cover topology/action/boundary definition | Durable definition digest + semantic version |
| AUD-011 | High | Measured/known | `*DAG` as Action bypasses sanctioned composition limits | Reject or redesign method/interface shape |

## P1 — correctness, security, and availability

| ID | Severity | Evidence | Finding | Resolution |
|---|---|---|---|---|
| AUD-012 | High | Measured | Build mutates builder and duplicates generated edges | Pure Build over cloned dependency state |
| AUD-013 | High | Measured | WorkflowData.Clone aliases nested values despite “deep copy” | Canonical deep clone or immutable-value contract |
| AUD-014 | High | Measured | InMemoryStore Save/Load does not prevent external nested mutation | Durable-fidelity clone/serialization and mutation tests |
| AUD-015 | High | Measured | JSON Load key A can return payload ID B and redirect later Save | Reject ID mismatch; key is authoritative |
| AUD-016 | High | Measured | Enabled workflow metrics become disabled on file-backed resume | Reattach Runner metrics config after Load; all-store tests |
| AUD-017 | High | Measured | Static level spawns one goroutine per node despite MaxConcurrency | Bounded worker pool/acquire-before-spawn |
| AUD-018 | High | Source | Engine metadata and consumer data share flat key namespace | Separate journal/metadata; interim reserved-key collision rejection |
| AUD-019 | Critical scope gap | Measured existing | Actions can forge node status and bypass operational verifier/sink meaning | Engine-private journal + per-node data view (M24) |
| AUD-020 | High | Measured/source | Boundary contract says precedence-only while J1/BAR require successful completion | Rename/redefine gate semantics consistently |
| AUD-021 | High | Measured | Pool silently loops on non-MP StoreFactory misconfiguration | Validate worker store on open and return fatal error |
| AUD-022 | High | Source | Pool distinct-store safety contract is prose-only | Reject duplicate store pointers or own construction |
| AUD-023 | High | Source/declared | Same-owner MP concurrent drives are re-entrant, not process-local serialized | Composite local mutex + durable claim |
| AUD-024 | High | Declared | Non-Unix file signal lock is no-op with known races | Implement/test platform lock or document Unix-only MP file signals |
| AUD-025 | High | Source/known | Signals have no delivery timestamp/freshness/correlation | Add metadata and approval generation/nonce policy |
| AUD-026 | High | Known | Complex values reload differently across stores | Canonical Value and cross-store fidelity contract |
| AUD-027 | High | Source | README durable builder example returns nil DAG/error | Correct to FromBuilder; compile external snippets |
| AUD-028 | High | Source | SECURITY lint-zero claim false | Update after making gate true |

## P2 — API, operability, and verification quality

| ID | Severity | Evidence | Finding | Resolution |
|---|---|---|---|---|
| AUD-029 | Medium | Measured | ForEach callback writing same data deadlocks | Snapshot before callback; document reentrancy |
| AUD-030 | High | Measured | Legacy action adapter drops result and error, reports Completed | Remove legacy signature |
| AUD-031 | Medium | Measured | Public nil behavior panics (`FromBuilder`, `RunNext`) | Uniform typed validation |
| AUD-032 | Medium | Source | DAG config setters race Execute | Move config to Runner or snapshot under lock |
| AUD-033 | Medium | Source | In-process Locker ignores context while blocked | Channel/semaphore keyed lock honoring ctx |
| AUD-034 | Medium | Source | Claim uses Background and ignores caller context | Context-aware Claim API |
| AUD-035 | Medium | Source | Pool/SchedulePoller hide persistent runtime errors | Observer/health channel and error classification |
| AUD-036 | Medium | Source | Unknown NodeStatus handling differs by store | Shared strict decoder policy |
| AUD-037 | Medium | Source | Store Load nil/nil silently means fresh state | Treat as store contract violation |
| AUD-038 | Medium | Source | Thread-safe marketing exceeds actual ownership/concurrency contract | Per-type/method concurrency documentation |
| AUD-039 | Medium | Source | TopologicalSort returns one flat slice inside `[][]` | Return `[]*Node` or unexport |
| AUD-040 | Medium | Source | Public API uses unexported builder/expander types | Export stable interfaces/types or simplify returns |
| AUD-041 | Medium | Source | `WithAction(interface{})` is weakly typed | Typed Action/ActionFunc methods |
| AUD-042 | Medium | Source | Runtime config split across builder, fields, setters; comment mentions nonexistent WithMetrics | Central Runner options |
| AUD-043 | Medium | Source | `pkg/workflow` import path freezes layout artifact | Decide/migrate before 1.0 |
| AUD-044 | Medium | Source | `STABILITY.md` leaves exported-but-unsupported ambiguity | All exports supported or explicit Experimental marker |
| AUD-045 | Medium | Source | Wall-clock “O(N)” test comment/math contradict code and is flaky | Structural operation counts or benchmark trend |
| AUD-046 | Medium | Source | Formal models are not run in CI | Pinned TLC PR/nightly jobs |
| AUD-047 | Medium | Source | “Formally verified engine” overstates model↔Go assurance | Use model-checked-algorithm wording |
| AUD-048 | Medium | Source | Doctest parse floor misses semantic/API errors | Type-check/compile assembled external snippets |
| AUD-049 | Medium | Source | Tracked coverage profile is stale | Remove or commit-stamp generated report |
| AUD-050 | Medium | Source | Mutation job can run six hours and cancel workflow | Timeout, shard, schedule separately |
| AUD-051 | Medium | Measured local | Development toolchain has reachable patched stdlib vulnerabilities | `toolchain` directive and contributor update |
| AUD-052 | Medium | Source | CONTRIBUTING says Go 1.24 and recommends timeout-free race command | Update to module floor/patched default and Make targets |
| AUD-053 | Medium | Source | “Changed graph is rejected” docs overclaim subset identity check | Narrow docs until digest lands |
| AUD-054 | Medium | Source | JSON/FB/SQLite/InMemory called interchangeable despite value differences | Publish capability/fidelity matrix |

## P3 — planning and maintainability

| ID | Severity | Evidence | Finding | Resolution |
|---|---|---|---|---|
| AUD-055 | High process | Source | STATE/PROJECT/ROADMAP/briefing disagree on active phase/milestone | One generated canonical active ledger |
| AUD-056 | Medium | Measured | Planning is 114.9k lines, larger than product/test narrative capacity | Archive; active state <200 lines |
| AUD-057 | Medium | Source | `.planning` is ignored and unavailable in published history | Track safe subset or separate versioned planning repo |
| AUD-058 | Medium | Source | Zero public issues despite accepted high findings | Mirror consumer-relevant debt to issues |
| AUD-059 | Medium | Observed | Shared worktree changed during audit | One worktree/branch per seat; immutable SHA gates |
| AUD-060 | Medium | Measured | 42% of production lines are comment-only; source carries review chronology | Keep current invariants, move history to ADR/audit |
| AUD-061 | Medium | Source | Phase-119 test machinery is self-analyzing and proof-heavy | Smaller independent oracle/model; reduce textual guards |
| AUD-062 | Low | Measured | Briefing test count wrong at stamped SHA | Generate census instead of hand-writing |
| AUD-063 | Medium | Source | Briefing says CI safe while coverage job calls dead target | Correct briefing and gate |
| AUD-064 | Medium | Source | Summary says COMPLETE while authoritative review/QA chain is fail | Derive status from accepted gate records |
| AUD-065 | Low | Source | File references drift (`pkg/workflow/suspendable_routing_test.go`) | Symbol/path validation in planning tooling |

## P4 — bounded/deferred design debt

| ID | Severity | Evidence | Finding | Resolution |
|---|---|---|---|---|
| AUD-066 | Medium | Architectural | Level barriers prevent ready-node pipelining | Benchmark/model a ready queue after correctness work |
| AUD-067 | Medium | Source | `WithCatchupOnce` is public no-op | Remove until implemented or fail unsupported |
| AUD-068 | Medium | Source | Build has no optional node/edge/static-width resource caps | Add definition budget options |
| AUD-069 | Medium | Source | Approval is orchestration, not authenticated authorization | Reframe and add host endorsement hooks |
| AUD-070 | Medium | Source | No definition migration API accompanies planned digest | Typed mismatch + migration/version strategy |
| AUD-071 | Low | Workspace | ~149 MiB workspace vs 6.6 MiB tracked, including ignored binaries/worktrees | Dedicated bin/temp cleanup tooling |

## Recommended ownership

| Area | Primary owner |
|---|---|
| AUD-001..003, 008, 010..025 | engine/architecture + independent QA |
| AUD-004..007, 045..052 | CI/release owner |
| AUD-026, 036, 054 | persistence owner |
| AUD-027, 028, 053 | docs/release owner |
| AUD-029..044 | API owner before 1.0 |
| AUD-055..065 | architect/process owner |

## Findings that should become public issues now

At minimum: copied-DAG deadlock, panic policy, graph identity, store fidelity, JSON ID mismatch, metrics resume, static goroutine width, signal freshness, Windows file-mailbox limitation, and version/release divergence.
