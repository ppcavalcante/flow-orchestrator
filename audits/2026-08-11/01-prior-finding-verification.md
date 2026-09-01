# Prior finding verification

## 1. Interpretation

This is a strict check of every `AUD-001` through `AUD-071` entry in the 2026-08-10 register. Implementation/documentation statuses are evaluated against committed `e215f47a6d9841ac0cadbca1b9a0bf79551cf58e`; planning, worktree, issue, tag, and remote-workflow entries use the separately scoped 2026-08-11 repository observations recorded in `00-evidence-and-method.md`.

Statuses mean:

- **Fixed** — the measured defect or stated contract is closed in current code, tests, and/or authoritative documentation.
- **Partial** — a narrower mitigation landed, but at least one material part of the finding or promised resolution remains.
- **Open** — the finding remains observable, or no implementation evidence establishes closure.
- **Superseded** — the exact historical artifact was not repaired, but a later explicit decision or independent gate replaced it. This is not counted as “fixed.”

The strict result is **42 fixed, 12 partial, 14 open, and 3 superseded**. Therefore the repository statement that all 71 findings were “remediated” is true only under a broader bookkeeping definition that includes deferral, acceptance, and supersession. It is not true if “remediated” means “the original defect is gone.”

## 2. Per-finding result

| ID | Status | Current evidence / residual |
|---|---|---|
| AUD-001 | Partial | Built-in nil, typed-nil, and nil-operand actions are rejected at Build. The package still has no uniform public panic policy; `DAG.Execute(ctx, nil)` still panics. |
| AUD-002 | Fixed | Built DAGs are consumed by pointer and the copied-lock public execution path is gone. |
| AUD-003 | Fixed | Empty multi-process owner IDs are rejected and adversarial tests cover the collapse case. |
| AUD-004 | Fixed | Current pinned golangci-lint run reports zero findings. |
| AUD-005 | Fixed | The two package flakes were rewritten/isolated; the named tests passed 20 consecutive repetitions in this audit. |
| AUD-006 | Fixed | Race targets carry explicit timeouts and coverage output is written atomically. |
| AUD-007 | Superseded | The phase-119 instrument was not repaired to a final independent pass. M23 closure instead used a different goal-backward verifier and records the old instrument as retired/partial. |
| AUD-008 | Fixed | Boundary validation is root-anchored and the OR-join bypass case is covered by regression tests. |
| AUD-009 | Open | Current source still does not descend from the published `v0.21.0-alpha` tag; release-only ancestry remains divergent. |
| AUD-010 | Partial | Structural definition digesting now covers topology, policy, compensation kind, boundaries, and action kind. It deliberately excludes action body/version, so semantically changed code can retain the same digest. |
| AUD-011 | Fixed | Passing a compiled `*DAG` through `WithAction` is rejected; composition must use the guarded sub-workflow path. |
| AUD-012 | Fixed | Nested DAG cycle detection is recursive and regression-tested. |
| AUD-013 | Partial | `WorkflowData.Clone` recursively copies `map[string]any` branches, but each `[]any` container is only shallow-copied; nested slice elements, typed maps, and pointers still alias. The exported “deep clone” contract remains false. |
| AUD-014 | Fixed | `InMemoryStore.Save` clones then canonicalizes and `Load` clones; current store-isolation/canonical-fidelity tests pass. This does not make direct `WorkflowData.Clone` fully deep. |
| AUD-015 | Fixed | JSON store payload identity is checked against the lookup key. |
| AUD-016 | Fixed | Unknown status values fail closed across store decoders. |
| AUD-017 | Fixed | The executor acquires capacity before spawning; bounded-goroutine regression tests pass. |
| AUD-018 | Partial | Reserved engine-key collision rejection and sealed action views mitigate consumer overwrite, but engine metadata still shares the generic data map. The planned physical data-plane/journal split has not occurred. |
| AUD-019 | Fixed | Forward consumer actions receive a sealed per-node view; attempts to mutate engine journal state are rejected and recorded as node failures. |
| AUD-020 | Fixed | Boundary J1 semantics were aligned to dominance plus terminal-success conditions. |
| AUD-021 | Fixed | The phase-119 J2 sweep now preserves execution failures. |
| AUD-022 | Fixed | The BAR/subsumption instrument now fails closed rather than reporting a false pass. |
| AUD-023 | Fixed | Exact bounded TLA+ commands and expected state counts are documented; all eight capstones passed in this audit. |
| AUD-024 | Fixed | Supported/unsupported platform and process boundaries are explicitly documented. |
| AUD-025 | Partial | Approval correlation nonce and store-only derivation landed; SQLite, JSON, and in-memory signals carry delivery time. FlatBuffers delivery still drops `EnqueuedAt`, proven by an external probe. |
| AUD-026 | Fixed | A canonical cross-store value contract and conformance tests now exist. |
| AUD-027 | Fixed | Metrics configuration survives durable save/load under the documented contract. |
| AUD-028 | Fixed | Nil/nil Load results are rejected on the audited runtime paths. |
| AUD-029 | Fixed | Public store failures are classified into the documented error taxonomy on the audited paths. |
| AUD-030 | Fixed | Same-owner concurrency is composed with process-local locking. |
| AUD-031 | Partial | Named constructor/configuration panic cases were converted to typed errors, but uniform exported-entry validation is incomplete; nil execution data and typed-nil stores still panic. |
| AUD-032 | Fixed | DAG execution configuration is snapshotted under lock and the audited setter/execute race is closed. |
| AUD-033 | Fixed | In-process locking honors context cancellation. |
| AUD-034 | Fixed | Pool worker errors are surfaced through the configured observer/health path. |
| AUD-035 | Partial | Pool observability is materially improved, but `SchedulePoller` still has no equivalent terminal error observer and returns nil on cancellation by policy. |
| AUD-036 | Fixed | Status decoding is exhaustive and unknown values are corrupt data. |
| AUD-037 | Fixed | Error aggregation uses bounded/non-nil slices and the audited nil-slot failure mode is closed. |
| AUD-038 | Partial | Several type-specific race contracts and locks were added, but public documentation still overgeneralizes “thread-safe” and there is no complete generated per-type matrix. |
| AUD-039 | Partial | `TopologicalSort` now honestly returns `[]*Node` and the API reference matches; `docs/architecture/dag-execution.md` still publishes the removed `[][]*Node` signature and one-group behavior. |
| AUD-040 | Fixed | Exported fluent builder APIs no longer expose the audited unexported return types. |
| AUD-041 | Fixed | Typed action entry points exist; the legacy polymorphic adapter is explicitly marked. |
| AUD-042 | Open | No smaller stable `Runner` facade was introduced; workflow configuration remains split across fields, builders, and fluent options. |
| AUD-043 | Open | The public import path remains `github.com/ppcavalcante/flow-orchestrator/pkg/workflow`. |
| AUD-044 | Fixed | Experimental/reserved APIs are explicitly labeled and inventoried. |
| AUD-045 | Open | The load-dependent wall-clock test is opt-in, but no CI structural complexity guard replaced it. |
| AUD-046 | Fixed | CI now runs the pinned TLA+ capstones. |
| AUD-047 | Fixed | Primary public claims were narrowed from source-level “formally verified” to modeled/model-checked algorithms with scope. |
| AUD-048 | Partial | The doctest suite catches more stale samples and currently passes, but stale type-correctness claims still parse and pass; current `TopologicalSort` docs prove the gap. |
| AUD-049 | Fixed | Previously untracked adversarial suites are now in the repository and run by normal package discovery. |
| AUD-050 | Fixed | Mutation testing has a 40-minute workflow timeout and is explicitly informational/non-blocking. |
| AUD-051 | Fixed | CI and contributor toolchain pins are patched; current govulncheck found no reachable vulnerability. |
| AUD-052 | Fixed | Contributor/release documentation now matches the Go floor and pinned CI patch line. |
| AUD-053 | Fixed | Public graph-identity wording now says structural definition digest rather than node-name subset. |
| AUD-054 | Partial | A store matrix exists and many overclaims were corrected, but the product README still says “all three built-in stores” without explaining SQLite’s category/interchangeability among the four concrete implementations. |
| AUD-055 | Partial | `STATE.md` is identified as canonical and carries a current decision, but it is hand-maintained and still contradicts strict closure evidence and older artifacts. |
| AUD-056 | Open | Planning remains extremely large: 115,822 Markdown lines in `.planning` at this snapshot. |
| AUD-057 | Open | `.planning` remains ignored while canonical release state lives there; clean clones do not receive that evidence. |
| AUD-058 | Open | No open GitHub issues represent the remaining product/process debt. |
| AUD-059 | Open | The audit worktree was concurrently modified again: `d570940`, `f172c80`, and `e215f47` landed after the initial snapshot. The audit re-anchored and reran affected gates, but the process remains non-immutable. |
| AUD-060 | Open | Production remains comment-heavy: about 10,075 comment-only lines out of 24,796 production Go lines (40.6%). |
| AUD-061 | Superseded | The phase-119 instrument is explicitly retired/partial and replaced for M23 closure, not simplified into the originally requested smaller oracle. |
| AUD-062 | Open | No generated source/API/test census replaces hand-maintained counts. |
| AUD-063 | Fixed | The false CI timeout statement in reviewer guidance was corrected. |
| AUD-064 | Superseded | Historical phase-119 “COMPLETE” artifacts remain, but canonical state explicitly overrides them rather than rewriting history. |
| AUD-065 | Open | No repository-wide semantic link/signature validation gate was established; stale API signatures remain public. |
| AUD-066 | Open | Whole-level execution barriers remain an explicit architectural ceiling. |
| AUD-067 | Open | Production-readiness work remains and the project correctly retains alpha status. |
| AUD-068 | Fixed | Optional graph-definition size budgets exist at Build and are tested. |
| AUD-069 | Fixed | Approval is explicitly documented as orchestration/correlation, not authentication or authorization. |
| AUD-070 | Fixed | Definition type mismatch and migration-hook behavior are implemented and covered. |
| AUD-071 | Open | Workspace hygiene remains unresolved: untracked launcher binaries are still present and `.planning` occupies roughly 154 MiB. |

## 3. Bottom line

The remediation wave substantially improved runtime correctness: 42 strict closures include the highest-risk owner/fencing, graph sealing, goroutine bounding, store identity, decoder fail-closed behavior, formal CI, lint, toolchain, and action-mediation items. Panic-prone built-in actions were materially mitigated, but remain part of Partial `AUD-001` because the exported panic policy is not uniform.

It did **not** close every prior finding. The most important technical residuals are FlatBuffers signal freshness, semantic action-version identity, incomplete Clone depth, inconsistent public nil handling, API/documentation drift, and the unchanged whole-level scheduler. The largest process residual is that canonical evidence remains in an ignored, very large planning tree and closure labels include accepted/deferred work.