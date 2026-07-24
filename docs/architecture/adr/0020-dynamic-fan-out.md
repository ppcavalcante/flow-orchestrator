# 0020. Dynamic fan-out — one journaled node that expands once and drives N bounded branches

## Status

Accepted (milestone M21 "Dynamic Fan-out", 2026-07). Records the locked design for **runtime-N fan-out**:
a workflow maps a branch action over N items **discovered at run time** (N unknown at `Build()`) → N
parallel branches → fan-in / aggregate — crash-safely, with **no determinism/replay tax**, **no central
server**, preserving **one-writer-per-workflow** and the **static-DAG** moat, **additively**. The public
`Execute` / `dag.go` / `parallel_execution.go` behavior is byte-unchanged (the only executor change is an
additive `withMaxConcurrency` set-site at `dag.go` that non-fan-out nodes never read). Target v0.20.0-alpha.

## Context

The platform program needs `map`-over-runtime-N before 1.0: fan out N branches where N is only known once
the workflow is running (e.g. "one branch per row a query returned"). Four hazards shape the design:

1. **The replay tax.** If the expander that resolves N re-runs on resume and returns a *different* N, the
   resumed graph no longer matches the crashed one — the moat's whole "workflow-is-DATA, no replay" claim
   breaks. N must be resolved **exactly once** and survive a crash as data.
2. **The static-DAG moat.** `checkGraphIdentity` (`workflow.go:587`) verifies the persisted journal on
   resume. Materializing N nodes into the frozen `DAG.Nodes` at run time (Option D below) breaches the
   static-DAG invariant and forces the formal capstone to be re-proven over a mutable graph.
3. **The unbounded-N DoS.** An expander returning millions of items must fail **loud and cheap**, never
   spawn millions of children or silently truncate.
4. **Partial failure.** With N branches, "one failed" needs a policy: fail the whole node (like a single
   child) vs complete with a {succeeded, failed} partition — and a partial failure must **not** silently
   cross into the parent's M12 compensation.

## Decision

**A fan-out is a SINGLE ordinary DAG node** (`fanOutAction`, `pkg/workflow/fanout.go`) whose `Execute`:

1. **Gates** on a durable `Checkpointer` store (else `ErrFanOutRequiresCheckpointer`, loud + early) —
   expansion-once has no durable `{N}` without one.
2. **Expands once.** It reads the reserved journal key first: present ⇒ this is a resume, decode the
   journaled items, **never re-run the expander**. Absent ⇒ run the expander ONCE, journal `{N + per-item
   keys}` as a JSON string, and **flush durably BEFORE branch 1** (the level-barrier flush at `dag.go` is
   too late — a crash between "expander returned" and "branch 1" would lose N).
3. **Caps the width** (`WithMaxWidth`, default 1024) — enforced AFTER N is resolved but BEFORE any branch
   or child ID exists; exceeding → loud `ErrFanOutMaxWidth` (the DoS guard).
4. **Drives N branches** in the node's **own `MaxConcurrency`-bounded goroutine pool** (read from ctx via
   the `withMaxConcurrency` seam), under a cancellable sub-context. Each branch is a child workflow under
   a **deterministic ID** `subFanOutChildID(parentID, nodeName, index)` — so crash-after-branch-k
   idempotency is the same per-child resume-idempotency as `subWorkflowAction` (a completed child is a
   no-op on resume), N-wide.
5. **Fans in** as the tail of the same `Execute` — aggregating `baseKey[i]` in **discovery order**
   (index-addressed, not completion order), per the fan-in policy.

**Fan-in is the tail of one `Execute`, not a separate scheduler concern.** The level executor
(`GetLevels()`, `dag.go:214`) ranges only the frozen `d.Nodes` — a journal entry is not a `*Node`, gets no
level/launch. That capability does not exist and is not being built.

### Why Option A (in-workflow journal expansion), not the alternatives

The load-bearing grounded fact: **`checkGraphIdentity` is ASYMMETRIC** — it verifies every *persisted* node
still EXISTS in the DAG (persisted ⊆ DAG); it does **not** require every DAG node to carry a journal status.
So expansion carried as journaled **data** that the node fans over satisfies the resume identity check — it
does **not** breach the static-DAG invariant. That is the insight that makes Option A moat-safe.

- **A — In-workflow journal expansion (chosen).** Expander runs once → `{N + items}` journaled as durable
  DATA → the one node drives N inline branches in its own bounded pool → fan-in reads N results in
  discovery order. Recovery reads the journal, re-runs no user code. **Keeps all six moat legs, runs on any
  `Checkpointer` (InMemory/JSON/SQLite), `dag.go`/`Execute` 0-diff.**
- **B — Child-run-per-item over the M17 queue + M19 composition (deferred to M22).** True cross-process
  N-parallelism, but (1) forces the MP-SQLite store (loses InMemory/JSON for fan-out) and (2) the await-K
  join is genuinely net-new (every await today is hardwired to exactly ONE child). Justified only if true
  cross-process parallelism at large N becomes a *stated* requirement.
- **C — Hybrid A default + B opt-in.** Builds and verifies TWO fan-out mechanisms in one milestone (scope
  blowout, union TLA+ burden). Rejected for M21.
- **D — Runtime DAG mutation (inject N nodes into `DAG.Nodes`).** Literally breaches static-DAG; the
  capstone must be re-proven over a mutable graph. The wrong rung — A gets the same result without mutating
  the frozen graph.

M21 grafts **B's indexed child-ID scheme** `(parentID, nodeName, itemIndex)` onto A (`subFanOutChildID` folds
the itemIndex as a fixed-width 8-byte-LE field into the same length-framed sha256 as `subWorkflowChildID`),
keeping B a clean M22 follow-on.

### The item + result fidelity contracts (load-bearing)

- **Item typing.** The expansion is journaled as a JSON string (a `[]interface{}` does not round-trip
  store-uniformly — SQLite reloads a complex value as a JSON string). A branch reads its item under
  `FanOutItemKey`, decoded with `UseNumber()`: a JSON number arrives as **`json.Number`** (int64-faithful,
  full range — call `.Int64()`/`.Float64()`). A **default** decode into `interface{}` yields `float64` and
  **corrupts an int64 item above 2^53** (a large ID / nanos timestamp) — the same first-CI-run fidelity bug,
  on the item axis.
- **Result typing.** A succeeded branch's declared result is written TYPED under `baseKey[i]` (an int64
  reloads as an int64 on all three stores — value_long-faithful scalar, NOT a `[]interface{}` aggregate),
  plus `baseKey.__count__` = N.

### Fan-in policy

- **FailFast (default).** The first branch failure fails the fan node and cancels in-flight / un-started
  siblings (the branch sub-context is cancelled). The surfaced error prefers the first **non-cancellation**
  error over a cancelled sibling (a cancelled sibling carries `context.Canceled` — the fail-fast side
  effect, not the root cause).
- **CollectPartial (opt-in, `WithCollectPartial`).** All N branches run to completion (no sibling
  cancellation); the node **Completes** (not Failed) even with k failures, exposing a partition:
  `baseKey.__count__` = N, `baseKey.__failed__` = the failed indices (store-uniform JSON string),
  `baseKey[i]` = the typed result for a succeeded i (absent for a failed i). A partial failure does **not**
  fail the node → no `ExecutionError` → a parent-level M12 `WithCompensation` rollback is **not** triggered
  (containment — the failed-branch isolation precedent from M19).

An **external** parent-ctx cancel is distinct from FailFast: under both policies it propagates (the node
stays non-terminal), so an interrupted run is never mis-recorded as a Completed partition.

## Verification — the M21 core claim is proven three ways

The claim — *no-replay crash-resume of runtime-N fan-out* — is machine-checked at the capstone (ph108,
sealed @ `c48c39e`):

1. **TLA+ `specs/M21FanOut.tla`** — TLC exhaustive (87 distinct states): **ExactlyNSpawn** (N children from
   the journaled expansion, exactly-once each under crash) + **FanInWaitsForAll** (aggregate fires iff all N
   branches terminal). `M21FanOutBreak.cfg` FALSIFIES ExactlyNSpawn (re-expand-on-resume) — bite-proven.
2. **Genuine 2-process kill-9 (`TestFanOutKill_2Proc`, non-short)** — real SIGKILL cycles, 6 branch effect
   rows each exactly-once, non-vacuous (~25s of kills vs ~0.3s clean). The true write→kill→read an
   in-process store-seed cannot reach.
3. **Gopter** over real `DAG.Execute` — non-vacuity independently re-confirmed (a seeded PENDING node so
   `Execute` re-enters and reads the journal, distinguishing journal-read from re-expand).

## Consequences

- **Additive and moat-preserving.** `Execute`/`dag.go`/`parallel_execution.go` public behavior byte-stable;
  1.0 stays earnable. Runs on any `Checkpointer`.
- **Single-level, single-process.** A fan-out branch itself fanning out is an explicit **non-goal** for
  v0.20.0-alpha (bounds the width×depth DoS surface + keeps the capstone tractable). True cross-process
  fan-out is deferred to M22 (Option B, opt-in, SQLite-only).
- **Known residue (documented, harmless).** A resumed all-terminal Failed run returns nil (the journal
  shows Failed); a fan-out failure may leave orphan cancelled-sibling child journals (dead data).

## Related

- [ADR-0018](0018-sub-workflow-composition-and-approvals.md) — the sub-workflow child-run + deterministic
  child-ID pattern this generalizes 1→N.
- [ADR-0019](0019-scheduling-and-concurrency-caps.md) — the M20 loud-typed-cap discipline (`ErrFanOutMaxWidth`
  mirrors it).
- [Fan-out guide](../../guides/fanout.md) · [API reference](../../reference/api-reference.md#dynamic-fan-out-added-m21).
