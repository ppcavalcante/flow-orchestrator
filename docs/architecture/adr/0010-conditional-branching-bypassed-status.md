# 0010. Conditional branching — the `Bypassed` status, structured `ChoiceNode`/`MergeNode`, and the cause-aware gate

## Status

Accepted (milestone v0.11-M11, 2026-07-05). Records the locked M11 design (`DEC-M11`):
true workflow-level branching — a `ChoiceNode` routes to exactly one branch and a
`MergeNode` OR-joins them. Realizes the `ChoiceNode` feature that was cut from M10 and
deferred here (`DEC-M10-P38-DEFER`, [ADR-0009](0009-durable-continuations-waiting-status.md)).

## Context

Through M10 the engine was a **strict-AND DAG executor**: a node launches once *all* its
dependencies resolve, and status was two-valued at the block — `Skipped` (a dependency
failed or was itself skipped) versus `Pending` (never reached). Branching could only be
faked *inside* actions ("run every node, decide what it does"), because there was no way
for a not-taken branch to genuinely not run.

True branching needs two things the strict-AND model did not have:

1. **A not-taken branch that does not run, and is not a failure.** Reusing `Skipped` for
   this was the obvious shortcut — and it corrupts a load-bearing signal. `Skipped` carries
   the failure-diagnostics meaning "an upstream you needed failed/was-skipped"
   (`DEC-CHUNK3`); a clean not-taken branch is *not* that. Overloading `Skipped` would make
   "why didn't this node run?" un-answerable from status alone.
2. **An OR-join.** The common "converge after a choice" shape needs a node that fires when
   **one** of several mutually-exclusive branches ran — the exact inverse of strict-AND.
   This is precisely why `ChoiceNode` was refused in M10: a merge below a choice is *always*
   `Skipped` under a strict-AND gate, so the gate itself had to be redesigned, not patched.

The constraint that shaped the design is the project moat: an **embeddable, no-server,
no-DB** engine with a **formally verified** core and **no determinism tax** (the workflow is
static DAG *data*, never replayed code). Branching had to stay **static and declared** (no
runtime graph mutation) so it remains exhaustively model-checkable, and it had to add **no
new dependency** and **no cost to the non-branching hot path**.

## Decision

**Branching is a static, declared, structured structure over a single cause-aware launch
gate.** Four locked elements:

1. **A 7th, terminal `NodeStatus`: `Bypassed`.** A not-taken branch of a `ChoiceNode` (and
   its whole reachable subgraph) is recorded `Bypassed` — **terminal** (never runs, never
   re-armed) and **not a failure** (never appears in an `ExecutionError`). It is a *distinct*
   status, not a `Skipped` overload, precisely to preserve the `Skipped` failure-diagnostics
   meaning. The FlatBuffers wire schema gains value 6 (additive; follows the M10 `Waiting`
   wire-5 precedent; old buffers never carry it), round-tripping through all three stores.

2. **The launch gate becomes cause-aware — one classifier, shared.** A node that cannot
   launch this pass is classified by the cause across its **full** dependency set
   (`classifyBlockedStatus`): a failure/skip-cause dep (non-coe `Failed` or `Skipped`) →
   `Skipped`; a purely `Bypassed` blocking cause → `Bypassed`; a non-terminal
   (`Pending`/`Running`/`Waiting`) dep → leave for a later pass (a `Waiting` sibling is
   **not** a terminal cause — M10 suspend semantics preserved). The **diamond rule**
   (`DEC-M11-P41-DIAMOND`): a node with both a `Bypassed` dep and a surviving *taken*
   (`Completed`, or coe-`Failed`) ancestor is `Skipped`, not `Bypassed` — the taken path
   wins. This one classifier is shared by the in-level launch gate **and** the post-halt
   sweep (`markSkippedFrom`), consulting the **same** `depResolved`/`isSkipCause` predicates
   the launch decision uses, so run and skip/bypass classification **cannot drift**. The
   whole gate redesign landed in phase 41 with the OR-join arm present-but-inert, so phase
   42 was purely additive — the redesign happened **once**.

3. **Structured `ChoiceNode` + `MergeNode` (not per-node trigger rules).** A `ChoiceNode` is
   a declared node whose action **is** the routing decision: it evaluates `When(pred,
   target)` arms in **declared order, first match wins** (`DEC-M11-FIRSTMATCH`), activates
   that one branch, and marks every other branch entry `Bypassed`; the choice itself is
   always `Completed` (it makes a decision — never `Bypassed`). No match with an `Otherwise`
   takes the default; **no match without an `Otherwise` is a typed error**
   (`ErrNoBranchMatched`) — a routing dead-end is surfaced, never a silent hang. A
   `MergeNode` is the OR-join: it fires iff **≥1 taken branch-tail** `Completed` (a
   `Bypassed` tail is satisfied, not blocking), and is itself `Bypassed` when every branch
   was bypassed (bypass composes downward). The taken count ranges over the recorded `From`
   tails only — the structural choice-dependency is **excluded** so it cannot vacuously
   inflate the count (`DEC-M11-DEPMODEL`, anti-vacuity). Predicates are `func(*WorkflowData)
   bool`, **data-only**, reading only guaranteed-run-ancestor/seed keys (an absent key reads
   zero and falls through — never panics).

4. **Strict reconvergence validation at `Build()`.** Only **structured, single-`ChoiceNode`,
   local** OR-joins are expressible (`D-P42-STRICT`). `Build()` returns a typed error for
   every unstructured shape: `ErrUnstructuredMerge` (a non-merge node reconverging branches,
   a cross-`ChoiceNode` merge, or an empty-branch `Choice → merge` tail), `ErrSharedBranch`
   (a branch entry owned by two choices), `ErrDanglingMerge` (a tail under no choice). This
   strictness is **load-bearing for the runtime semantics**: because a choice takes exactly
   one branch and cross-choice merges are forbidden, a merge's `Failed` predecessor is always
   the sole taken branch failing → fail-fast (INV-01) is sound. Errors surface at build,
   never at runtime.

## Consequences

- **The moat holds.** Branching is static declared data, exhaustively model-checkable; no
  server, no DB, no new dependency, no replay. A workflow with no `ChoiceNode` behaves
  exactly as M10 — the OR-join role is gated on the `*mergeAction` marker, so strict-AND
  nodes pay nothing, and the launch hot path keeps its early-break on the first unresolved
  dep (the cause-aware full scan runs only off the launch path, when a node is already
  blocked).
- **All additive.** `Bypassed`, `AddChoice`/`When`/`Otherwise`, `AddMerge`/`From`, the
  reconvergence sentinels, and `ErrNoBranchMatched` are new symbols; no existing exported
  signature changed. FlatBuffers/JSON round-trip is a new additive enum value.
- **`Skipped` stays honest.** The failure-diagnostics meaning of `Skipped` is preserved
  because bypass has its own status — "why didn't this node run?" is answerable from status
  alone (`Bypassed` = not-taken branch; `Skipped` = an upstream you needed failed;
  `Pending` = never reached).
- **Verification extended, bite-proven.** The branching semantics are machine-checked in new
  TLA+ specs (`specs/MCM11ChoiceMerge.tla`, `specs/MCM11CrashChoice.tla`) that model the
  OR-join fire/bypass, cause-aware classification, and choice×crash composition, each
  mutation-proven to bite. gopter property suites over real `DAG.Execute` prove first-match,
  never-invoked not-taken branches, and bypassed-interiors-are-`Bypassed`-not-`Skipped`.
- **Honest scope / limits by design.** Only structured single-choice local OR-joins are
  expressible; unstructured (van der Aalst) reconvergence, cross-choice merges, and
  empty-branch merges are rejected at build (workaround for an empty branch: a pass-through
  node). No loops (a DAG has no cycles). Predicates must be data-only and read only
  guaranteed-run-ancestor/seed keys.

## Alternatives Considered

- **Reuse `Skipped` for not-taken branches.** Rejected: it corrupts the `Skipped`
  failure-diagnostics signal (`DEC-CHUNK3`) — a clean routing decision would be
  indistinguishable from an upstream failure cascade. A distinct `Bypassed` keeps both
  answerable.
- **Per-node trigger rules / arbitrary predicate-gated launch (unstructured OR-join).**
  Rejected: van der Aalst-style unstructured reconvergence is not locally decidable, breaks
  the exhaustive-verification moat, and makes the fail-fast interaction unsound (a merge
  could not tell which predecessor was the taken one). The structured `ChoiceNode` +
  single-choice `MergeNode` keeps every OR-join local and model-checkable.
- **Patch `ChoiceNode` onto the strict-AND gate without redesigning it (the M10 attempt).**
  Rejected/deferred as `DEC-M10-P38-DEFER`: a merge below a choice is *always* `Skipped`
  under strict-AND, so the shared gate had to be redesigned. M11 does that redesign once
  (phase 41), landing the OR-join arm inert so phase 42 is additive.
- **Runtime graph mutation / dynamic sub-DAG instantiation for branches.** Rejected: it
  imports dynamic topology the exhaustive model cannot enumerate. Branches are declared
  static subgraphs; routing only *selects* among them.
- **Auto-discover merge tails (`.Of(choiceName)`) instead of explicit `.From(tails...)`.**
  Rejected: explicit tails match the DAG's explicit-edge idiom and the `DEPMODEL`, and stay
  legible with multi-node and empty branches.
- **Silently drop an unmatched choice (no `Otherwise`).** Rejected: a routing dead-end is a
  real error — surfaced as `ErrNoBranchMatched`, never a silent hang or panic.

## References

- `pkg/workflow/node.go` (`Bypassed` status),
  `pkg/workflow/choice.go` (`AddChoice`/`When`/`Otherwise`, `ErrNoBranchMatched`,
  `choiceAction`), `pkg/workflow/merge.go` (`AddMerge`/`From`/`WithAction`, `mergeAction`)
- `pkg/workflow/reconvergence.go` (`validateReconvergence`, `ErrUnstructuredMerge`,
  `ErrSharedBranch`, `ErrDanglingMerge`)
- `pkg/workflow/parallel_execution.go` (`classifyBlockedStatus`, `dependentRole`,
  `depResolved`, `isSkipCause`, `isTerminalStatus`, the OR-join fire gate),
  `pkg/workflow/dag.go` (`markSkippedFrom`), `pkg/workflow/builder.go`
  (choice/merge edge folding)
- `internal/workflow/fb/workflow/NodeStatus.go` (`NodeStatusBypassed` wire value 6)
- `specs/MCM11ChoiceMerge.tla`, `specs/MCM11CrashChoice.tla` (+ `.cfg`,
  `run_m11_capstone.sh`) and [`specs/README.md`](../../../specs/README.md)
- [ADR-0009](0009-durable-continuations-waiting-status.md) (the `Waiting` status and the
  `DEC-M10-P38-DEFER` that deferred `ChoiceNode` to here)
- [DAG Execution → Node State Transitions](../dag-execution.md#node-state-transitions),
  [API reference → Conditional branching](../../reference/api-reference.md#conditional-branching),
  [Workflow patterns → True Conditional Branching](../../guides/workflow-patterns.md#true-conditional-branching-choicenode--mergenode)
