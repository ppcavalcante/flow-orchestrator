# 0018. Sub-workflow composition & approvals — a phased-hybrid exec-model, completion-signal WAKE, and the SQLite signal mailbox

## Status

Accepted (milestone M19 "Composition", 2026-07). Records the locked composition design
(`COMP-SW` / `COMP-APV` / `COMP-INV`; `GATE-M19-EXEC-MODEL` — the exec-model **design-options**
gate, user-ratified 2026-07-17): let a workflow node **spawn and await a child workflow**, and let
a node **park for a human approve/reject decision**. Additive and opt-in over the M17 dispatch
queue + the M10 durable-suspend mechanism; **the executor is byte-unchanged** (`Execute` / `dag.go`
untouched across all of M19 — the composition nodes are ordinary suspendable/blocking `Action`s
threaded on ctx, and the new `Workflow` fields are opt-in).

## Context

Through M18 a workflow was a single DAG: it could suspend and resume (M10), dispatch onto a durable
queue (M17), and be cancelled mid-run (M18) — but a node could not **spawn another whole workflow
and wait for its result**, and there was no first-class **approval gate**. Composition is the
platform-program step that de-risks M21 dynamic fan-out: a parent node that spawns + awaits a child
is the primitive fan-out is built on.

Three forces shaped the design:

1. **One-writer-per-workflow must survive.** The M16 per-`WorkflowID` drive lease is
   **non-reentrant**. A parent and child are therefore **distinct workflows** with distinct IDs and
   distinct journals — a child is never "part of" the parent's journal. A child whose deterministic
   ID collides with an ancestor already on the drive stack would self-deadlock on the lease, so that
   must be refused *before* the lease is acquired.

2. **A child may or may not be able to run in-process.** A definition-value, non-suspendable child
   can be run **inline** (block the parent goroutine on `child.Execute`) with zero infra. A child
   that **parks** (contains an approval / signal-wait) cannot be run inline — the parent goroutine
   would block forever on a park. Such a child, and a child referenced only by **type** (resolved at
   runtime from a `Registry`), must be **dispatched** onto the queue and awaited via a park/wake.

3. **Approvals need a durable mailbox on the production store.** The approval gate is a signal-wait
   (M10). Through M18 only InMemory / FlatBuffers / JSON implemented `SignalStore` — the SQLite
   production store had no mailbox table, so approvals (and the queue-path wake) could not run
   durably on SQLite. That gap had to be closed as its own foundation.

## Decision

### 1. Exec-model — Option C, a phased hybrid (`GATE-M19-EXEC-MODEL`)

The embedder picks the dispatch mode **explicitly** via three distinct builder methods; the model is
**not** an automatic router. The build-time closure-scan *enforces* the boundary (an inline child
that contains a suspendable node fails `Build`), but it does not silently re-route.

| Builder method | Child form | Runs | Awaits by |
|---|---|---|---|
| `AddSubWorkflow(name, child *DAG)` | definition-value, **non-suspendable** | **inline** (blocks on `child.Execute`) | blocking |
| `AddSubWorkflowParked(name, child *DAG)` | definition-value | **out-of-band** (host/producer runs it) | **park → wake** |
| `AddSubWorkflowQueued(name, childType string)` | **type-ref** (resolved via `Registry`), may be suspendable | **queue** (a `Pool` worker claims + runs) | **park → wake** |

- **Inline** is the zero-infra default. The child's whole spawn-closure is **scanned at build**
  (`scanChildInlineSafe`, recursive, `*DAG`-identity-visited so a by-value cycle/diamond terminates)
  for any suspendable node; one anywhere → `ErrSubWorkflowSuspendableChild` at `Build`. This is what
  guarantees the inline block always terminates.
- **Queue** is the explicit opt-in for a type-ref and/or suspendable child. It structurally requires
  a multi-process `*SQLiteStore` + a worker `Pool` + a `Registry` (injected at `Execute`; the DAG
  carries only the type **string**, keeping the workflow pure data). The parent's mailbox address
  rides the **trusted control columns** of the `work_queue` row — never the input BLOB
  (`DEC-P94-PARENT-ADDRESS-COLUMN`), so attacker-controlled input cannot forge a wake target.

**Child identity is deterministic:** `childID = "sub:" + sha256(len-prefixed parentID ‖ nodeName)`.
Resume-stable (a re-drive finds the same child) and **idempotent-spawn** by construction — re-driving
an already-terminal child journal does not re-run its actions, so spawn count stays 1 across any
parent re-drive or crash-mid-child window.

### 2. The result contract — a child **data key**, not a node output (`DEC-P91-RESULT-DATAKEY`)

`WithResult(parentKey, childDataKey)` declares that the child's result is the child's **data key**
`childDataKey`, copied into parent data under `parentKey` on child success. **The child must
`Set(childDataKey, ...)` its result.** A data key is read (not a node output) because data keys carry
the store's **typed columns** — so a **scalar** result (int64 via `value_long`, plus string/bool/
float) round-trips **type-faithfully on all three durable stores** (an int64 reloads as an int64).
Node *outputs* reload as a raw JSON string on FlatBuffers and SQLite, which would corrupt an int64
result into a string on two of three stores. The contract is **uniform** across inline, parked, and
queue paths (all three read the same data key).

**Scope caveat (`F-P91-1`):** this backend-uniformity covers the scalar types the stores type-column.
A **complex** result (map / slice / nil) reloads typed on InMemory but as a JSON string on FB/SQLite
— the same pre-existing store-wide property that governs *every* complex data value, not a
sub-workflow-specific behavior. Declare a **scalar** result key when backend-uniformity matters.

A result-key collision with a foreign pre-existing parent value is a loud
`ErrSubWorkflowResultKeyCollision` at run time (compared with `reflect.DeepEqual`, total over any
value type) — never a silent last-writer-wins. A prior spawn of the *same* node writing the *same*
value is the idempotent re-apply and is allowed.

### 3. The failure contract — child-fail → parent-node-fail → **parent-scoped** compensation

A child that terminalizes **failed** fails the **parent node** (INV-01 fail-fast, the same terminal
contract the approval reject uses). The parent's own M12 saga compensation then runs over **the
parent's nodes only** — a child's internal rollback is the child's own concern (the child owns its
journal + saga). The terminal verdict is **coe-aware**: a `ContinueOnError` child node that is
`Failed` is *not* a run failure, and a saga-rollback node (`Compensated` / `CompensationFailed`)
**is** a failure even with no `Failed` node present (`childRunFailed`, `F-P92-01` — a cancel/deadline
rollback can leave `{Compensated, CompensationFailed, Completed}` and no `Failed`; treating a rollback
node as failure keeps the parked verdict identical to the inline path, which surfaces the `*SagaError`).

### 4. Approvals — a thin suspendable node, fail-fast reject, defensive decode (ph90)

`AddApproval(name)` parks the run (`Waiting`) until an `ApprovalDecision` payload is delivered to the
mailbox **under the signal name equal to the node name** (`ApproveSignal` / `RejectSignal` derive the
same name — they cannot drift), then acts: **approve** → idempotent apply (persist the raw payload for
audit) + converge; **reject** → fail fast with an `*ApprovalRejectedError` (INV-01, no downstream
runs). Requires a `SignalStore` (else `ErrWaitRequiresSignalStore` — a loud failure, never a
forever-park).

`ApprovalRejectedError` deliberately has **no `Unwrap`** (`DEC-P90-REJECT-ERR-NO-UNWRAP`): a rejection
wraps no underlying cause, and exposing one would make it a new poison source into any `errors.Is/As`
classifier over `DAG.Execute`'s return (the `ExecutionError.Unwrap` poison-precedence trap).
`errors.As(err, &ApprovalRejectedError{})` is the correct terminal-fail classification.

The decision decode is **defensive** (`decodeApprovalDecision`): a durable round-trip returns the
payload as a generic `map[string]any` (InMemory/JSON, `UseNumber`) or a raw JSON string (FlatBuffers
reload), never the typed struct. A missing `Approved` field defaults to `false` — a **fail-safe
reject**, never a phantom approve; a wrong-typed field is a typed `ErrValidation`.

### 5. WAKE — completion-signal → `DeliverAndResume`, **no scheduler** (`DEC-M19-WAKE`)

The parked/queue paths park (`Waiting`) while the child journal is non-terminal. On child-terminal a
**bare completion signal** (no payload) is delivered to the parent mailbox, and a host
`DeliverAndResume` re-drives the parent, which reads the child data key + renders the verdict. **There
is no background scheduler** — the completion signal + the host re-drive *is* the wake.

The completion **gate is the child journal being terminal**, not the mere presence of the signal
(`DEC-P92-SIGNAL-IS-TRIGGER`): the signal only triggers the re-drive; the journal (or, for a queue
child, the `work_queue` row — see below) is the authority. So a misrouted/spurious signal → the parent
re-parks, never a false wake. The signal ID is deterministic (`f(childID)`), so a re-delivered signal
is idempotent (one mailbox entry).

**Queue-child terminal authority (`DEC-P94-QUEUE-TERMINAL-AUTHORITY`, `F-P94-05`):** for a
queue-dispatched child the **`work_queue` row is the lifecycle record** and is consulted **first**. An
operator-cancelled queue child terminalizes the *row* (`cancelled`) but not the child's data journal —
a journal-only gate would re-park the woken parent forever. So: row `done` → read the result data key;
row `failed`/`cancelled` → fail the parent node; row `pending`/`claimed` → still parked. A manually-
signalled parked child (ph92) has no row → fall through to the journal gate unchanged.

On the producer side, a queue child that terminalizes (`done` / `failed` / `cancelled`) delivers the
completion signal **after** the disposition is durable (`deliverSubWorkflowCompletion`, signal-after-
terminal ordering, `F-92-04`); a `ErrSuspended` park is **not** a disposition — the row + lease are
left intact (attempts reset to 0, `F-P94-01`, because a park is durable progress not a failed attempt)
so a reclaim resumes the child, and **no** completion signal fires until the child actually
terminalizes.

### 6. The SQLite signal mailbox (ph93) — closing the approvals-on-SQLite gap

A new `signals` table on the SQLite schema makes `*SQLiteStore` implement `SignalStore`
(`DeliverSignal` / `TakeSignals` / `AckSignals`). It is a **separate table** from the WorkflowData
snapshot (mailbox-outside-snapshot) with **no FK** to `workflows` (early-signal buffering — a signal
may arrive before the workflow instance exists). `DeliverSignal` is **last-writer-wins** on a
re-delivered `sig_id` (`ON CONFLICT DO UPDATE` — a `DO NOTHING` would make SQLite first-writer-wins, a
conformance divergence from the other stores, self-caught at ph93). `TakeSignals` enforces the F37
mailbox cap **before** materializing rows (the mailbox is an external-writable channel, M9 threat
model) and scans the payload as `sql.NullString` so a single NULL row cannot brick every read
(availability poison-pill defense, `F-P93-ADV-1`). This closes the ph90 gap: **approvals and any
signal-wait now run durably on the SQLite production store**, not just InMemory/FB/JSON, and it
unblocks the queue-path wake.

### 7. The nesting-DoS ceiling (ph95) — always-on runtime bound + opt-in build-time check

`ErrSubWorkflowMaxDepth` refuses a spawn reached at nesting depth `d >= ceiling` (**default 8**,
`Workflow.MaxSubWorkflowDepth` override) on **both** spawn paths — loud, never a park, never a silent
cap. Depth = the number of ancestor drives on the stack; the queue path seeds the drive-stack size
from the carried `work_queue.depth` (built in one O(n) pass, `F-P95-01`; the carried depth is bounded
`<= 1024` on read to fail-safe a forged/bit-rotted row), so a type-ref chain `A→B→C…` is bounded by
the same ceiling as an inline chain even though each queue child runs in a fresh worker. **This runtime
ceiling is the load-bearing DoS backstop.**

`Registry.ValidateNoTypeCycles()` is an **opt-in build-time** fail-fast on a directly-declared type-ref
spawn cycle (A queues B queues A) — see the two scope caveats in the guide (`F-P95-02`, `F-P95-04`,
`F-P95-05`). It is a *nicety on top of* the runtime ceiling, not the DoS guard.

## Consequences

- **Additive + opt-in + moat intact.** The executor (`Execute` / `dag.go`) is byte-unchanged; the new
  `Workflow.Registry` / `Workflow.MaxSubWorkflowDepth` fields and the `signals` / `work_queue.depth`
  migrations are additive. Single-DAG workflows are unaffected. 1.0 stays earnable.
- **Formally verified (`COMP-INV-FORMAL`).** `specs/MCM19Composition.tla` adds a sub-workflow-await arm
  (62,790-state exhaustive) — parent parks/wakes on child-terminal, no double-spawn, child-fail →
  parent-fail, crash-safe (MaxCrashes ≥ 1) — each new invariant bite-proven, plus gopter composition
  properties over the real `DAG.Execute`. `COMP-INV-ADDITIVE` (git-diff-empty on the executor public
  path) and `COMP-INV-STATIC-DAG` (parent DAG acyclic after build) asserted.
- **Honest limitations.** (a) The WAKE is host-driven — a lost completion signal degrades to a host
  re-drive of the parent (it re-checks the child journal), not a lost result; there is no automatic
  scheduler. (b) A per-workflow `MaxSubWorkflowDepth` override governs the **inline** path only; the
  queue path enforces the package default (`F-P95-02`). (c) `ValidateNoTypeCycles` catches only
  directly-declared top-level type-ref cycles, not cycles reachable through an inline wrapper or a
  runtime-computed type (`F-P95-04`).

## Alternatives considered

- **A single auto-routing `AddSubWorkflow`** that inspects the child and picks inline vs queue. Rejected
  — a type-ref child's suspendability is not statically knowable, and a silent re-route would hide the
  infra requirement (a queue child *needs* an MP store + Pool + Registry). Explicit builder methods make
  the requirement legible; the closure-scan still *enforces* the inline-safety boundary.
- **Carry the child result in the completion-signal payload.** Rejected — the signal would then have to
  round-trip the result through the mailbox (losing int64 fidelity, re-solving the type problem the data
  key already solves). The bare-trigger + data-key read keeps one result path (`DEC-P92-SIGNAL-IS-TRIGGER`
  + `DEC-P91-RESULT-DATAKEY`).
- **A background scheduler that polls child journals and auto-resumes parents.** Rejected — it would
  reintroduce infra the zero-infra thesis avoids; the host re-drive on completion signal is sufficient and
  keeps the engine scheduler-free.
- **Make the child part of the parent journal.** Rejected — it would violate one-writer-per-workflow (M16)
  and entangle the child's saga with the parent's. Distinct workflows with distinct IDs keeps both intact.

## Related

- [ADR-0009](0009-durable-continuations-waiting-status.md) — the `Waiting` status + durable-suspend
  mechanism the approval/parked-await nodes ride.
- [ADR-0011](0011-saga-compensation-durable-rollback.md) — the parent-scoped compensation a child-fail
  triggers, and the coe-aware verdict `childRunFailed` reproduces.
- [ADR-0016](0016-work-dispatch-queue-registry-pool.md) — the M17 work-queue / Registry / Pool the queue
  path dispatches onto.
- [ADR-0015](0015-multi-process-safety-leases-fencing.md) — the non-reentrant per-`WorkflowID` drive lease
  that forces distinct parent/child IDs + the ancestor-cycle guard.
- [Sub-workflows & approvals guide](../../guides/sub-workflows.md).
</content>
</invoke>
