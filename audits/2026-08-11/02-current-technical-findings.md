# Current technical findings

## 1. Current architecture assessment

The project remains a coherent embedded durable-workflow library: a sealed DAG definition drives a mutex-protected `WorkflowData` journal; `Workflow` adds load/checkpoint/save and per-workflow leasing; capability interfaces add signals, deltas, sync, dispatch, schedules, and queue/fencing without forcing them on direct DAG users.

The strongest current areas are:

- structural graph validation and sealing;
- bounded forward and fan-out goroutine creation;
- explicit at-least-once crash semantics;
- SQLite claim/fencing and operational query surfaces;
- defensive file parsing, size/depth bounds, and typed errors;
- sealed per-node action views that mediate consumer journal mutation;
- broad adversarial, property, race, and model-check coverage.

The following findings remain after the remediation wave.

## 2. Findings

`CUR-004` is intentionally unused. A candidate finding about action-body semantic identity was reclassified as an accepted limitation after confirming that the public contract deliberately defines `DefinitionDigest` as structural-only; the stable gap preserves audit traceability.

### CUR-001 — FlatBuffers signals discard the freshness timestamp

**High — measured durable correctness/security gap.**

`Signal.EnqueuedAt` is documented as a delivery-time freshness value every durable store returns. SQLite reads `signals.enqueued_at`; JSON serializes `enqueued_at`; in-memory stores the complete struct. FlatBuffers is different:

- `pkg/workflow/schema/workflow_data.fbs:49-53` defines only `id`, `name`, and `payload`;
- `encodeSignalFB` writes only those three fields (`signal_store.go:289-303`);
- `decodeSignalFB` reconstructs only those three fields (`signal_store.go:318-346`).

Executed external probe:

```text
flatbuffers signal deliver=<nil> take=<nil> count=1 enqueued-at=0
```

The regression test itself says “Every durable store populates it,” but its factory table covers only SQLite, JSONFile, and InMemory (`aud025_signal_freshness_test.go:16-55`). FlatBuffers is omitted.

Impact: a consumer using FlatBuffers cannot distinguish a freshly delivered signal from an old buffered one. For approvals, the nonce is a stable correlation value derived from workflow ID, node, and structural digest; it is not a time-bound authorization token. Losing delivery time therefore removes the advertised backend-independent stale-signal check.

Remediation: add an `int64 enqueued_at` field to the FlatBuffers `Signal` table, regenerate bindings, encode/decode it, and add FlatBuffers to the common freshness test. Preserve zero as the backward-compatible “unknown” value for pre-field entries; callers that require freshness must fail closed on zero.

### CUR-002 — Public nil handling still permits host-process panics

**High — measured availability/API defect.**

Two ordinary exported-input paths still panic instead of returning the package’s typed errors:

1. A valid built DAG accepts nil execution data and later dereferences it:

```text
nil DAG data panic=runtime error: invalid memory address or nil pointer dereference
```

`DAG.Execute` validates build state and structure but has no early `data == nil` guard (`dag.go:520-618`).

2. `ApprovalNonceFromStore` checks only `store == nil` (`approval_nonce.go:92-96`). A typed-nil pointer inside the `WorkflowStore` interface passes that test and panics on `store.Load`:

```text
typed-nil store panic=runtime error: invalid memory address or nil pointer dereference
```

The same typed-nil store, when embedded in a constructed `Workflow`, also panics during `Workflow.Execute`.

Impact: callers that validate configuration dynamically, decode optional dependencies, or expose host-controlled workflow requests can crash the embedding process instead of receiving `ErrValidation`. This is the same policy inconsistency the prior audit identified, now on a newly added helper as well.

Remediation: validate nil execution data before any DAG work; use the existing interface-aware nil detector for `WorkflowStore` inputs; validate required workflow fields/dependencies at construction and public drive boundaries. Add table-driven nil/typed-nil tests over exported entry points.

### CUR-003 — `WorkflowData.Clone` is not a deep clone

**High — measured API/data-isolation defect.**

The clone walker recursively copies `map[string]any` branches, but its `[]any` case copies only the immediate slice header/elements and does not recurse into those elements. Typed maps, pointers, and other composite values are also retained by reference (`workflow_data.go:1397-1466`). The probe’s exact slice shape was `[]any{[]any{"original"}}`: the outer container was copied, while the inner `[]any` remained shared.

External probe, mutating originals after `Clone`:

```text
clone nested-slice="mutated" typed-map="mutated" pointer="mutated"
```

This contradicts the exported source contract and `CHANGELOG.md:14-16`, which says Clone now deep-copies nested maps/slices. The current `InMemoryStore.Save` mitigation is stronger than direct Clone: it clones and then canonicalizes the isolated copy, so supported store values are protected under the canonical store contract. That does not repair callers of the exported Clone method.

Impact: callers can unintentionally share mutable subgraphs between supposedly isolated workflow snapshots. The type-dependent behavior is especially risky because common direct `map[string]any` cases pass while adjacent shapes alias.

Remediation: either implement a documented, cycle-safe deep copy for the exact supported value algebra, or narrow the API contract and reject unsupported mutable shapes. Do not attempt an unconstrained reflection clone without a clear policy for pointers, interfaces, unexported fields, identity cycles, and custom types.

### CUR-005 — Documentation verification passes stale public contracts

**Medium — measured documentation/API defect.**

At final `e215f47`, `docs/reference/api-reference.md:67-68` correctly publishes `TopologicalSort() ([]*Node, error)`. One public architecture page still teaches the removed nested result: `docs/architecture/dag-execution.md:203-206` describes the former one-group `[][]*Node` behavior.

Independently, the product README at repository-relative `README.md:316`—not this audit bundle’s `README.md`—says “all three built-in stores” while the repository exposes four concrete store implementations: InMemory, JSONFile, FlatBuffers, and SQLite. The wording may intend a narrower non-SQLite category, but it never defines that subset, leaving store categorization and interchangeability ambiguous.

`go test ./internal/doctest -count=1` passes despite the stale architecture declaration, demonstrating that the suite proves syntax and selected complete programs rather than equivalence of manually copied signatures. The store-categorization ambiguity is separately source-observed; it is not claimed as a doctest escape without a dedicated inventory assertion.

Impact: consumers following the architecture page reason about a result shape that no longer exists, and store guidance leaves backend coverage ambiguous. More broadly, a green doctest gate still overstates documentation freshness.

Remediation: generate or AST-check exported signature blocks against `go/types`/`go doc`; update the remaining stale TopologicalSort architecture section; reconcile every store-count/interchangeability claim against the four-implementation matrix; make stale package-qualified declarations and inventory claims fail the documentation gate.

### CUR-006 — Example cleanup left the mediation-writer golden stale

**High — measured blocking verification defect.**

The rebuilt example suite correctly removed six action-side `SetNodeStatus`/`SetOutput` sites from the deleted `examples/new_simple/main.go`. `TestVB09_TerminalWriterSetMatchesTheGolden` still expects those unmediated writer sites. Both the full race suite and a targeted non-race reproduction fail with:

```text
VB-09: the set of non-test sites writing a terminal NodeStatus or node output
outside executeNodesInLevel has CHANGED.
REMOVED:
  examples/new_simple/main.go fetchAction   setnodestatus / setoutput
  examples/new_simple/main.go processAction setnodestatus / setoutput
  examples/new_simple/main.go saveAction    setnodestatus / setoutput
```

This is a stale verification oracle, not a runtime mediation regression: removal of the six sites is the desired safety change. The full race gate demonstrably fails, and the targeted non-race package test fails on the same oracle. The encompassing plain `go test ./...` command is therefore expected to fail **[INFERENCE]**; CI declares the race suite blocking.

Remediation: review the six removals as expected, update the checked-in golden writer set through its documented derivation procedure, run the targeted VB-09 test, then rerun the complete race suite. Do not weaken the deriver or reintroduce example-side journal writes to make the old golden pass.

### CUR-007 — Release identity is not mechanically tied to source identity

**High — source-proven + repository-proven release-process gap.**

Current `HEAD` is `e215f47`; the published `v0.21.0-alpha` tag points at `4108150`. Their merge base is `bee7258`, so current source does not descend from the published release commit. `pkg/workflow.Version` intentionally remains `0.21.0-alpha` while M23 changes sit under `[Unreleased]`, which is acceptable during development.

The release workflow, however, extracts any pushed tag and creates a release without checking that:

- the tag version equals `workflow.Version` / `VersionInfo`;
- the changelog has a matching release section;
- the tag commit descends from the previous published release under the canonical lineage policy;
- the tree is clean and the release commit is the attested source decision;
- every release-blocking gate result belongs to that exact tag SHA before release creation.

Impact: a future tag can publish M23/M24 code while the runtime reports 0.21, or repeat the current detached-release ancestry. The SLSA attestation can correctly attest an artifact built from the wrong version decision; provenance does not supply semantic version consistency.

Remediation: add a release preflight that parses the tag, source version, and changelog and rejects mismatch; tag a reviewed immutable commit that descends from the previous published tag; make release creation consume a reusable gate result bound to that exact candidate SHA (or rerun every blocker at the tag SHA). If the project intentionally replaces rather than rejoins the published lineage, a tracked release ADR must explicitly supersede the ancestry rule and `AUD-009` must be reclassified as accepted/superseded before GO.

### CUR-008 — The canonical release decision is ignored and uses non-strict closure labels

**Medium — source-proven + repository-proven process/reproducibility gap.**

`.planning/STATE.md` declares itself canonical and claims all 71 prior findings remediated, but `.planning` is ignored and absent from a clean clone. Public audit and release readers therefore cannot reproduce several closure assertions. The directory’s 115,822 Markdown lines / roughly 154 MiB are separate accepted cleanup debt, not the release-blocking scope of this finding.

The worktree also changed repeatedly during this audit: `d570940`, `f172c80`, and `e215f47` landed after the initial `f617542` snapshot. The audit re-anchored and reran affected gates at `e215f47`, but the process repeats the prior moving-target condition.

Impact: release decisions depend on local ignored state; historical artifacts contradict one another; “all remediated” conflates fixed, partial, deferred, accepted, and superseded items.

Remediation: before tagging, move the minimal canonical release ledger into tracked content, generate it from machine-readable finding/status records, and audit an immutable commit/worktree. Reducing or archiving the bulky planning history is Stage 3 hygiene and may remain explicitly accepted at alpha.

## 3. Architectural residuals, not regressions

These are real current limits but not newly introduced defects:

- whole-level barriers delay ready nodes behind unrelated stragglers;
- `DefinitionDigest` is deliberately structural and does not identify action-body semantics; action upgrades therefore require host deployment/migration discipline unless a future explicit action-version field is adopted;
- the public package remains broad and configuration styles remain split;
- the `/pkg/workflow` import path is an awkward long-term public package identity;
- production code is unusually comment-heavy (about 40.6% comment-only lines), increasing review and stale-claim cost;
- approval correlation is not authorization; hosts must authenticate and authorize decisions before delivery;
- in-memory and local file-store safety boundaries remain process/host trust boundaries, not adversarial service isolation.