# 0022. Sealed graph + complete mediation — an opaque Node/DAG/Workflow surface, a structural definition digest, and engine-reserved keys a consumer cannot forge

## Status

Accepted (milestone M23 "Sealed Graph + Complete Mediation", v0.22.0-alpha, 2026-08). Records the
locked design for **sealing the public graph surface** and **completing runtime mediation** so a
consumer can neither mutate a validated graph through a leaked handle nor overwrite the engine's own
bookkeeping — plus the independent-audit remediation pass that hardened the surrounding contracts.
All changes are **additive/defensive**: the static-DAG executor stays **behavior-compatible for
valid graphs** (0-diff on the valid path); the changes only *refuse earlier and louder* what was
previously reachable.

## Context

Through M21 the `Node` / `DAG` / `Workflow` types exposed their internals as **exported struct
fields** and carried **post-`Build` mutators** (e.g. `(*DAG).AddDependency`, per-`Node` setters). A
consumer holding a `*Node` from `GetNode`/`GetLevels`, or a `*DAG` from a built workflow, could
therefore reach past the builder and mutate an **already-validated** graph — adding an edge that
`build()`'s reconvergence check would have refused, flipping a policy field, or swapping an action.
`Validate()` does not re-run the full `build()` checks (the reconvergence check runs only in
`build()`), so a post-build mutation could install a violation no gate would catch. This is a
correctness-and-trust hole directly under the 1.0 static-DAG moat.

Two adjacent gaps compounded it:

1. **Resume only checked node *existence*, not *definition*.** `checkGraphIdentity` verified that
   every persisted node still exists in the DAG, but a graph whose **topology, per-node policy,
   compensation, boundary, action kind, or suspendability** changed between crash and resume would
   still resume — silently running against a definition the journal was not written for.
2. **A consumer action could overwrite engine-reserved state.** `WorkflowData` is the shared bag; a
   consumer `Set` on an engine-reserved (`__`-prefixed) key could corrupt the executor's own
   bookkeeping (journals, dispositions, correlation state).

The milestone also folded in an **independent-audit remediation pass** — a set of pre-1.0 API and
runtime fixes surfaced by an external audit tranche (see CHANGELOG `[Unreleased]`).

## Decision

**Seal the exported graph surface and complete mediation, enforced structurally.**

1. **`Node` / `DAG` / `Workflow` become opaque handles (SEAL-01/02/06).** Every graph field is
   unexported; the post-`Build` mutators are **deleted** from the public surface. Reads move to
   accessors:
   - `Node`: `Name()`, `GetDependencies()`, `HasDependency(name)` — a `*Node` from
     `GetNode`/`GetLevels` is read-only, changing nothing through that handle.
   - `DAG`: `Name()` (now a method), `GetNode`, `GetLevels`, `DefinitionDigest`, `Validate`,
     `Execute` — `StartNodes`/`EndNodes` are deleted; the graph is opaque.
   - `Workflow`: the graph is read via `w.DAG()`; the exported **config** fields remain
     (`WorkflowID`, `Store`, `MaxSubWorkflowDepth`, `Clock`, `Locker`, `RollbackTimeout`,
     `MetricsConfig`).
   - The **edge set is sealed too** (SEAL-06): `(*DAG).AddDependency` and `(*Workflow).AddDependency`
     are gone from the public surface, so no out-of-package caller can add an edge to an
     already-validated graph.
   The seal is **enforced by a parser-driven surface census** (`pkg/workflow/surface_census_test.go`)
   — a test that reads the actual exported surface and fails if a field or mutator reappears, so the
   seal cannot silently regress.

2. **A structural definition digest — `DAG.DefinitionDigest()` (AUD-010).** A digest over the graph
   *definition*: topology, per-node policy/compensation, boundary, action kind, and suspendability.
   Resume now rejects a **changed graph definition**, not only a removed node — closing the
   "resume against a different graph" hole.

3. **Complete per-node action mediation via a sealed per-node action view.** Each node's action runs
   against a **sealed view** of `WorkflowData`; a consumer action can no longer overwrite
   engine-reserved (`__`-prefixed) keys — a `Set` on a reserved key through the sealed view is
   refused and recorded as a seal violation (AUD-018). The engine writes reserved keys through a
   private `setReserved` path the consumer cannot reach.

4. **A build-time boundary verifier-dominance check — `WithBoundary(doer, verifier, sink)`.** Declares
   the precedence property `Precedence(verifier, sink)` scoped to control flow: on **every** route
   the executor can take through the built graph, the verifier precedes the sink. `Build` **refuses**
   a topology that can reach the sink without passing the verifier — the violation is caught at build,
   not discovered at run time.

5. **An approval correlation nonce — `ApprovalNonce` + `ApprovalNonceFromStore` (AUD-025).** A pure
   function of `(workflowID, nodeName, DefinitionDigest)` (the id and name length-prefixed before
   hashing, the digest appended fixed-length — the same collision guard `SubWorkflowChildID` uses). A
   host attaches it to the decision it delivers; the engine consumes the decision **only if the
   attached nonce matches**, binding an approval to a specific workflow, node, and graph definition.
   Recompute it, do not store it.

6. **The independent-audit remediation pass** (pre-1.0, additive/loud):
   - **Typed `WithAction` / `WithActionFunc` (AUD-041)** — `WithAction` now takes a typed `Action`;
     `WithActionFunc` is the bare-func sibling. A bare func no longer silently satisfies `WithAction`.
   - **Ctx-aware `Claim` (AUD-034)** — `ClaimStore.Claim` takes a `context.Context` (Renew/Release
     stay ctx-free).
   - **Exported `ChoiceBuilder` / `MergeBuilder` / `FanOutExpander` (AUD-040)** — the previously
     unexported choice/merge builders and the fan-out expander type are named on the public surface.
   - **`WithDefinitionBudget` (AUD-068)** — an explicit size ceiling on the definition
     (`MaxNodes` / `MaxEdges` / `MaxWidth`; 0 = unlimited), refused at `Build`.
   - Plus the runtime-safety fixes in the same tranche: deep `WorkflowData.Clone` (AUD-013/014),
     goroutine-bounded level execution (AUD-017), nil/typed-nil action rejection at `Build`
     (AUD-001), a compiled `*DAG` passed to `WithAction` rejected at `Build` (AUD-011), and empty
     multi-process `ownerID` rejection (AUD-003).

## Consequences

- **The static-DAG moat is now enforced against a leaked handle, not just by convention.** A
  validated graph cannot be mutated from outside the package; the surface census keeps the seal from
  regressing. The executor is 0-diff for valid graphs — the seal and the digest only refuse invalid
  or drifted inputs earlier and louder.
- **Resume is definition-faithful.** A run only resumes against the graph definition its journal was
  written for; a topology/policy/compensation/boundary/action-kind/suspendability change is rejected
  on resume rather than silently honored.
- **Engine bookkeeping is unforgeable from a consumer action.** Reserved keys are the engine's; a
  consumer write to one is refused, so a consumer cannot corrupt journals/dispositions/correlation
  state through the shared `WorkflowData`.
- **The approval nonce is a freshness/correlation token, not a secret (honest ceiling).** It binds a
  decision to `(workflowID, node, definition-digest)` and defeats a **stale or cross-wired** approval
  — a decision minted for one node/graph cannot be replayed against another. It is **not** an
  authentication secret: it is a pure, recomputable function of public inputs, so an attacker who
  **controls the store** can recompute and forge it. That is consistent with the M9 trust model,
  under which the store is an input TCB — a store-controlling attacker is already inside the trust
  boundary. The nonce raises the bar against accident and misrouting, not against a compromised store.
- **A pre-1.0 API break, taken deliberately while the surface is still `-alpha`.** Sealing the fields
  and retyping `WithAction` are source-breaking for a consumer that read a field or passed a bare
  func; both have a mechanical migration (accessor method / `WithActionFunc` or `workflow.ActionFunc`).
  Taking the break now keeps the 1.0 frozen surface honest.

## Alternatives Considered

- **Keep the fields exported and rely on documentation ("do not mutate after Build").** Rejected — a
  convention the compiler does not enforce is not a moat; the leaked-handle mutation path stays open
  and `Validate()` does not catch the resulting violation.
- **Make `Validate()` equivalent to `build()` instead of sealing.** Rejected — it would require every
  consumer to re-`Validate()` after every possible mutation, and a consumer that forgets still ships a
  broken graph. Sealing removes the mutation path entirely; `Validate()` stays a cheap cycle check.
- **A digest over node existence only (the pre-M23 `checkGraphIdentity`).** Rejected as insufficient —
  it misses topology/policy/action-kind drift. The structural `DefinitionDigest` covers the full
  definition.
- **A signed/HMAC approval token instead of a recomputable nonce.** Rejected for M23 — a real secret
  would need a key-management story the embeddable, zero-infra library does not own, and under the M9
  trust model a store-controlling attacker defeats it anyway. The correlation nonce is the honest fit
  for the actual threat (accident + misrouting), stated with its ceiling.

## References

- CHANGELOG `[Unreleased]` — the M23 sealed-graph + audit-remediation tranche.
- [ADR-0009](0009-durable-continuations-waiting-status.md) — the durable-execution / suspend model the
  definition digest protects on resume.
- [ADR-0018](0018-sub-workflow-composition-and-approvals.md) — the approval signal path the correlation
  nonce binds.
- [ADR-0020](0020-dynamic-fan-out.md) — the static-DAG moat this seal enforces against a leaked handle.
- The M9 durable-execution threat model (store as an input TCB) — the trust boundary the nonce's
  ceiling is stated against.
- [API reference](../../reference/api-reference.md) · [Reference README](../../reference/README.md).
