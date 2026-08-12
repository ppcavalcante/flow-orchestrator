# Sub-workflows & approvals (composition)

Added in **M19**. Composition is an **opt-in** layer that lets a workflow node **spawn and await a
child workflow**, and lets a node **park for a human approve/reject decision**. It is built over the
M10 durable-suspend mechanism and the M17 dispatch queue; the executor is unchanged — a sub-workflow
node and an approval node are ordinary `Action`s.

Two seams:

- **Approval gate** — `AddApproval(name)`: park until an approve/reject decision arrives, then
  converge or fail fast.
- **Sub-workflow spawn/await** — `AddSubWorkflow` / `AddSubWorkflowParked` / `AddSubWorkflowQueued`:
  a node spawns a child workflow and awaits its result.

A parent and a child are **distinct workflows** — distinct IDs, distinct journals, distinct sagas
(one-writer-per-workflow is preserved). The child owns its own durable state.

See [ADR-0018](../architecture/adr/0018-sub-workflow-composition-and-approvals.md).

---

## Approval gate

`AddApproval(name)` parks the run (`Waiting`) until an `ApprovalDecision` is delivered to the
workflow's durable mailbox, then acts: **approve** → converge (downstream runs); **reject** → the node
fails fast with an `*ApprovalRejectedError` and **no downstream node runs** (INV-01). It requires a
store that implements `SignalStore` (InMemory / FlatBuffers / JSON, and — since M19 — **SQLite**);
without one the node returns `ErrWaitRequiresSignalStore` (a loud failure, never a forever-park).

The decision signal's **name is the node name** — `ApproveSignal` / `RejectSignal` derive it, so a
host's delivery can never drift from the node. Because the name *is* the decision signal, it must be
**non-empty**: `AddApproval("")` fails loud at `Build` with `ErrValidation` (M22) — a bare `""` name
would build a node no host could ever satisfy (there is no empty decision signal to target).

```go
b := workflow.NewWorkflowBuilder()
b.AddNode("prepare").WithAction(prepareRelease)
b.AddApproval("sign-off").DependsOn("prepare")
b.AddNode("deploy").WithAction(deploy).DependsOn("sign-off")

// FromBuilder returns a store-backed *Workflow (b.Build() returns a bare *DAG).
b.WithStore(store).WithWorkflowID("release-42")
wf, _ := workflow.FromBuilder(b)

// First drive parks at "sign-off" (returns ErrSuspended).
if err := wf.Execute(ctx); !errors.Is(err, workflow.ErrSuspended) { /* ... */ }

// A human approves — deliver + resume in one call. The decision carries a
// correlation nonce (AUD-025) binding it to THIS approval park; obtain it from the
// workflow. A decision with a wrong/absent nonce is inert (the node keeps waiting).
nonce := wf.ApprovalNonce("sign-off")
sig := workflow.ApproveSignal("sign-off", "alice@example.com", "LGTM", "decision-1", nonce)
if err := wf.DeliverAndResume(ctx, sig); err != nil {
    // A reject surfaces as *ApprovalRejectedError; classify with errors.As.
    var rej *workflow.ApprovalRejectedError
    if errors.As(err, &rej) {
        log.Printf("rejected by %s: %s", rej.Approver, rej.Comment)
    }
}
```

- The approve payload is **persisted for audit** (approver + comment) and surfaced as the node output.
- The **correlation nonce** (AUD-025) — `wf.ApprovalNonce(node)`, or the pure `workflow.ApprovalNonce(workflowID, node, definitionDigest)` — binds a decision to a specific park. It is a freshness/correlation token for honest hosts, **not** a secret: it makes a stale/stray/mis-correlated decision inert, but does not defend against an attacker who controls the store (authenticate the decision before delivering it — see AUD-069). For a **queued sub-workflow** child, compute it with the child's deterministic ID and the child DAG's `DefinitionDigest()`.
- `sigID` is a host-supplied dedupe key — re-delivering the same ID is idempotent.
- A **missing `Approved` field decodes as `false`** — a fail-safe reject, never a phantom approve.
- Do **not** also call `WithAction` on an approval node — the action is set directly, and retry /
  timeout are not meaningful on a park.

---

## Sub-workflow spawn/await — three dispatch modes

The embedder picks the dispatch mode **explicitly**. There is no automatic router; instead the choice
is a builder method, and the build-time closure-scan *enforces* the inline-safety boundary.

| Builder method | Child | Runs | Await |
|---|---|---|---|
| `AddSubWorkflow(name, child *DAG)` | definition-value, **non-suspendable** | **inline** (blocks) | blocking |
| `AddSubWorkflowParked(name, child *DAG)` | definition-value (a verdict **classifier**, never executed), **may be suspendable** | **out-of-band** (the host runs it) | park → wake |
| `AddSubWorkflowQueued(name, childType)` | **type-ref**, may be suspendable | **queue** (`Pool`) | park → wake |

A **suspendable** child is accepted on **either** the parked or the queued path — only *inline* refuses
one. Choose between them on the **store**: parked needs a `SignalStore`; queued needs a multi-process
`*SQLiteStore` + `Pool` + `Registry`.

### The result contract (all three modes)

`WithResult(parentKey, childDataKey)` declares the child's result: **the child must
`Set(childDataKey, result)`**, and on child success the value is copied into parent data under
`parentKey`.

> ⚠️ **Read the child's DATA key, not a node output.** A **scalar** result (`int64` via `value_long`,
> plus string / bool / float) round-trips **type-faithfully on all three durable stores** (an `int64`
> reloads *as* an `int64`). A **complex** result (map / slice / nil) is *not* backend-uniform — it
> reloads typed on InMemory but as a JSON string on FlatBuffers/SQLite, exactly as any other complex
> data value does. **Declare a scalar result key when backend-uniformity matters.**

A result-key collision with a foreign pre-existing parent value is a loud
`ErrSubWorkflowResultKeyCollision` at run time — never a silent overwrite. (A prior spawn of the same
node writing the same value is the idempotent re-apply and is allowed.)

### The failure contract

A child that terminalizes **failed** fails the **parent node** (fail-fast). The parent's own M12 saga
compensation then runs over **the parent's nodes only** — the child's internal rollback is the child's
own concern. The verdict is coe-aware (a `ContinueOnError` child node that failed is not a run failure;
a saga-rollback node **is**).

### Inline sub-workflow (`AddSubWorkflow`)

Runs the definition-value child in-process under a deterministic child ID (`f(parentID, nodeName)`)
and **blocks** on it. Requires a `Store` (else `ErrSubWorkflowRequiresStore`). The spawn is
**idempotent** — a re-drive after the child completed does not re-run it.

```go
// child DAG — must be non-suspendable, and must Set the declared result key.
child := workflow.NewWorkflowBuilder()
child.AddNode("compute").WithActionFunc(func(ctx context.Context, d *workflow.WorkflowData) error {
    d.Set("total", int64(1250)) // scalar → value_long-faithful on every store
    return nil
})
childDAG, _ := child.Build() // Build() returns *DAG

parent := workflow.NewWorkflowBuilder()
parent.AddSubWorkflow("price", childDAG).WithResult("order_total", "total")
parent.AddNode("charge").WithAction(charge).DependsOn("price")
```

The child's whole spawn-closure is **scanned at build**: a suspendable node anywhere in it fails
`Build` with `ErrSubWorkflowSuspendableChild` (an inline child blocks the parent, so it can never
park). Route such a child to **either** the queued or the parked path — both accept a suspendable
child; choose on the store (`*SQLiteStore` + `Pool` + `Registry` vs a `SignalStore`). Do **not** also
call `WithAction`.

### Queue sub-workflow (`AddSubWorkflowQueued`) — type-ref children, engine-dispatched

The opt-in for a child referenced by **type**, and one of the two routes for a child that **parks**
(e.g. a child with its own approval — `AddSubWorkflowParked` takes one too, when the host runs it
rather than the engine). The parent node enqueues the child onto the M17 work queue (carrying the parent's
mailbox address in the trusted control columns), **parks** (`Waiting`), and a `Pool` worker claims +
runs the child. On child-terminal a completion signal wakes the parent, which reads the result data
key and renders the verdict.

It structurally requires a multi-process `*SQLiteStore` + a worker `Pool` + a **`Registry`** (the
`type → DAG` map). The DAG carries only the child **type string** (pure data); the Registry (the CODE)
is injected on the `Workflow` at `Execute`.

```go
// The child is registered by type — the SAME registry the Pool workers use.
reg := workflow.NewRegistry()
reg.Register("risk-check", func() (*workflow.DAG, error) {
    b := workflow.NewWorkflowBuilder()
    b.AddApproval("analyst-sign-off")           // suspendable child → NOT inline; queued or parked
    b.AddNode("score").WithAction(score).DependsOn("analyst-sign-off")
    return b.Build()                             // Build() returns (*DAG, error); the child Sets its result key
})

parent := workflow.NewWorkflowBuilder()
parent.AddSubWorkflowQueued("risk", "risk-check").
    WithInput(map[string]any{"applicant": "acme"}). // seeds the child's data keys
    WithResult("risk_score", "score_out")

parent.WithStore(sqliteStore).WithWorkflowID("loan-9")
wf, _ := workflow.FromBuilder(parent) // FromBuilder → *Workflow (Build() returns *DAG)

// The Registry (CODE) belongs to the EXECUTION ENVIRONMENT, not to the workflow: it is
// passed to the dispatcher, which injects it into each drive it starts. `Workflow.Registry`
// was unexported in v0.22.0 (M23 SEAL-06) and has no setter — a *Workflow you construct
// yourself cannot carry one.
ran, _ := workflow.RunNext(ctx, sqliteStore, reg, "worker-1") // or drive a fleet with NewPool(factory, reg, ...)
// The parent parks at "risk" and wakes when the child terminalizes.
```

`WithInput(map)` seeds the child's data keys (JSON-encoded into the queue row's input). It is valid
**only** on a queued node.

### Parameterizing an inline child

**No child reads parent data, on any path.** Every child runs under its own WorkflowID with its own
journal — parent and child are distinct workflows — and an inline child's `WorkflowData` is built
**fresh** for the child ID, then loaded only from that child's own persisted state. Nothing is copied
in from the parent. Only the **queue** path has an input mechanism (`WithInput`).

`WithResult` moves exactly **one** value **into parent data**, on every path: it is a single
`(parentKey, childDataKey)` pair, **not** additive — a second call *overwrites* the first, and a child
that sets three data keys still lands one in the parent. That bounds the *parent-data channel* only; it
does not bound what the **caller** can see (for parked, the host owns the child's run and its store, so
it can read the rest back — see below).

Parameterize an inline child by **capturing the values in its actions' closures at DAG-construction
time** — build the child from a Go function taking the parameters:

```go
// The child's DECLARATION. Returns the BUILDER, not a built *DAG: the inline path
// needs a *DAG and the parked path needs a builder (FromBuilder takes a
// *WorkflowBuilder), so declare once and build at the point of use.
func reviewChildBuilder(applicant string) *workflow.WorkflowBuilder {
	cb := workflow.NewWorkflowBuilder()
	cb.AddStartNode("review").WithAction(workflow.ActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
		d.Set("verdict", "reviewed:"+applicant) // captured, NOT read from parent data
		return nil
	}))
	return cb
}

// AddSubWorkflow takes a *DAG, so the inline path builds it here.
func reviewChild(applicant string) (*workflow.DAG, error) {
	return reviewChildBuilder(applicant).Build()
}

child, err := reviewChild("acme")
if err != nil {
	return err
}
parent.AddSubWorkflow("review", child).WithResult("verdict", "verdict")
```

The cost is that an inline child DAG is a **value, not a template**: a different parameterization needs
a different child DAG, and an out-capture is bound to that one build-time DAG, shared by every run.
Where one child definition must serve many runtime-varying inputs **on the inline path**, that is what
the **queue path** (`AddSubWorkflowQueued` + `WithInput`) is for — it takes a child *type* and seeds the
data keys per run. The **parked** path solves it differently, and more cheaply **than the queue path** —
see below.

### Parameterizing a parked child — the child is a verdict classifier

`AddSubWorkflowParked` **never executes the child you pass.** The host runs the child; the `child`
argument exists only to classify the host's finished run.

> **The constraint.** The classifier must declare, as `ContinueOnError`, **every node name the host's
> run may leave in status `Failed` and expect to be tolerated.** Nothing else about it is read: not its
> edges, not its actions, not its node count, not nodes that succeed. A `Compensated` /
> `CompensationFailed` node is always a failure regardless, and the classifier is not consulted.

Diverge one axis at a time, host run held fixed at a tolerated failure:

| Diverged in the classifier | Effect |
|---|---|
| node **name** absent | **false failure** |
| name present, **`ContinueOnError` flag** absent | **false failure** |
| node count | inert |
| extra nodes the host never ran | inert |
| edges / ordering | inert |
| action identity (would error if invoked) | inert — never invoked |

A one-node stub naming the single coe-failable node correctly classifies a larger host run. The failure
mode is a **silent false failure**: the parent fails a run the host considered successful.

Because the host runs the child, it builds a **fresh child DAG per run** with per-run captures, so **one
parent definition serves many runtime inputs** — what the inline path cannot do. Run the child under the
deterministic ID, which is public API:

> **The host's child run MUST use the same store as the parent.** The parked node reads the child's
> journal through the *parent's* store, so a child run on a different store is invisible to it and the
> parent **re-parks on every wake, silently** — the node reads `ErrNotFound` and returns `ErrSuspended`,
> so there is no error and no timeout, and no number of re-drives converges it.

```go
// The host runs the child itself, under the deterministic ID the parent will look for,
// ON THE SAME STORE as the parent — the parked node reads the child journal through the
// parent's store, so a different store makes the child invisible and the parent
// re-parks on every wake — no error, and no re-drive converges it.
childID := workflow.SubWorkflowChildID(parentWorkflowID, "review") // stable contract — do not re-derive by hand

// A fresh, per-run child. NOTE this takes the BUILDER form (see the inline example
// above): FromBuilder takes a *WorkflowBuilder, and the store and ID go ON the builder.
// Do NOT call Build() first — Build refuses a store-configured builder, and a *DAG has
// no WithWorkflowID/WithStore.
cb := reviewChildBuilder("acme").WithWorkflowID(childID).WithStore(store)
hostRun, err := workflow.FromBuilder(cb)
if err != nil {
	return err
}
if err := hostRun.Execute(ctx); err != nil {
	return err
}

// Child is terminal — wake the parked parent with the completion signal.
sig := workflow.SubWorkflowCompletionSignal("review", "sig-1")
if err := parentWF.DeliverAndResume(ctx, sig); err != nil {
	return err
}
```

> **Do not reimplement the ID derivation.** `SubWorkflowChildID` is
> `SHA-256(uint64-LE(len(parentID)) || parentID || nodeName)`, hex, prefixed `sub:`. The 8-byte length
> prefix is a **collision guard**, not incidental framing — without it `("ab","c")` and `("a","bc")`
> would collide. It is a stable contract (the same commitment `IdempotencyKey` carries), so recompute it
> with this function rather than by hand. `FanOutChildID` is its fan-out counterpart.

The host can also reach the child's **full result set**, because it owns the run and the store — via
`store.Load(childID)`, or a closure it wrote into the child's actions. (`Workflow.Execute` returns only
an error; the run's `WorkflowData` is not exposed, so this is a store read or a capture, not a free
in-process handle.) So `WithResult`'s single-value limit bounds only what reaches *parent* data.

### Inline vs parked — the divergences

None of these are symmetric.

| | inline (`AddSubWorkflow`) | parked (`AddSubWorkflowParked`) |
|---|---|---|
| **Suspendable child** | refused at `Build` (`ErrSubWorkflowSuspendableChild`) | **accepted** — the host may park *and resume* the child, then wake the parent |
| **Store** | bare `WorkflowStore` | **requires `SignalStore`** |
| **Depth ceiling, ancestor-cycle guard, closure scan** | enforced | **none** — the host's responsibility |
| **Failure fidelity** | propagates the child's **actual error value**; `errors.Is` reaches its sentinel | verdict **reconstructed from node statuses** — the error value is **lost**, only the node name survives |

> **Parked is not uniformly the lighter path.** It is lighter than the *queue* path (a `SignalStore`
> rather than `*SQLiteStore` + `Pool` + `Registry`), but **heavier than inline**, which needs only a bare
> `WorkflowStore`.

> **Failure fidelity is the trap.** A consumer classifying child failures with `errors.Is` / `errors.As`
> on a sentinel gets a true positive on inline and a **silent false negative** on parked. Classify a
> parked child's failure by node name, or have the host record the reason itself.

### Parked sub-workflow (`AddSubWorkflowParked`)

The definition-value child runs **out-of-band** (a host/producer runs it) and the parent parks until
a completion signal wakes it. Use this when you run the child yourself and signal completion with
`SubWorkflowCompletionSignal(nodeName, sigID)`. Most embedders want `AddSubWorkflowQueued` **when the
engine should dispatch the child** — the queue producer emits the completion signal for you.
`AddSubWorkflowParked` is the lower-level seam: you run the child, so you also signal completion, and in
exchange it needs only a `SignalStore` and lets one parent definition serve many runtime inputs (see
[Parameterizing a parked child](#parameterizing-a-parked-child--the-child-is-a-verdict-classifier)).

> **A parked dispatched run does not hold a running-slot, and does not bleed its retry budget.** When a
> queue-dispatched child (or any dispatched workflow) parks (`Waiting`), its queue row is marked `parked`:
> (1) the concurrency cap counts **running** slots only (`claimed AND NOT parked`), so K parked parents do
> **not** deadlock a cap-1 sub-workflow — a park frees the slot; and (2) its `attempts` counter is **reset**
> — a park is durable progress, not a failed attempt, so a long-parked child does not silently spend its
> transient-infra retry budget. The drive lease is left to **lapse on its TTL** (a released lease would fall
> out of the reclaim scan and strand the row); reclaim-to-resume latency is therefore TTL-bound — tune it
> with `WithLeaseTTL` on the store (sized above your longest level). This is the honest behavior: reclaim is
> not instant, it is lease-lapse-bound.

---

## The nesting ceiling (DoS bound)

A sub-workflow spawn reached at nesting **depth ≥ the ceiling** is refused with
`ErrSubWorkflowMaxDepth` — loud, never a park, never a silent cap. The default ceiling is **8**; both
spawn paths enforce it (the queue path carries the accumulated depth across the dispatch, so a
type-ref chain `A → B → C…` is bounded just like an inline chain). **This runtime ceiling is the
load-bearing DoS guarantee.**

> ⚠️ **`MaxSubWorkflowDepth` override scope (`F-P95-02`).** Setting `Workflow.MaxSubWorkflowDepth`
> raises/lowers the ceiling for the **inline** path only. On the **queue** path a child runs in a
> separate worker drive that does not carry this field — only the depth *count* crosses the dispatch —
> so a queue child enforces the **package default (8)** regardless of the override. If you are an
> operator hardening the ceiling below 8, note that a queue-dispatched chain still uses 8; change the
> package default if you need a uniform queue-path bound.

### Optional build-time cycle check — `ValidateNoTypeCycles`

`Registry.ValidateNoTypeCycles()` is an **opt-in** build-time fail-fast on a directly-declared
type-ref spawn cycle (type A queues type B queues type A → `ErrSubWorkflowTypeCycle`).

> ⭐ **Embedder contract (`F-P95-05`).** This is **your responsibility to call** — the library does
> **not** auto-invoke it (it owns no construction hook and will not add per-dispatch cost). Call it
> **once, at Registry assembly time — after registering all types and BEFORE the first `RunNext` /
> `Pool` run**:
>
> ```go
> reg := workflow.NewRegistry()
> // ... reg.Register(...) all types ...
> if err := reg.ValidateNoTypeCycles(); err != nil {
>     return err // catch a declared A→B→A cycle before dispatch
> }
> // ... now start the Pool / call RunNext ...
> ```
>
> "Rejected at build" is only reachable if you make this call. **Skipping it weakens fail-fast
> diagnostics but never the DoS bound** — the runtime depth ceiling always fires.

**Scope (`F-P95-04`):** the check extracts only the queue-sub-workflow edges from each factory's
**top-level** nodes. A cycle reachable only through a nested *inline* wrapper, or through a
runtime-computed child type (an opaque factory), is **not** caught here — the runtime depth ceiling is
the backstop that bounds every chain, declarable or not.

---

## Durability & signals

Approvals and the queue-path wake ride the durable **signal mailbox**. Since M19 the SQLite production
store implements `SignalStore` (the `signals` table), so approvals + signal-waits run durably on
SQLite, not just InMemory/FB/JSON.

> **Signal `sig.ID` caller-contract (`F-P93-SEC-1`).** A signal's `ID` is the host-supplied dedupe key
> and the mailbox's primary key — **supply a non-empty, stable ID** per logical decision/event.
> Re-delivering the **same** ID is idempotent (one mailbox entry, last-writer-wins on the payload); an
> empty ID is rejected. Do not derive an ID from untrusted external data in a way that lets one event
> collide with another's ID (that would let one delivery overwrite another's payload). The engine's own
> completion signals use a deterministic `f(childID)` ID for exactly this idempotency.

The wake is **host-driven** — there is no background scheduler. A completion signal + a
`DeliverAndResume` (or, on the queue path, the producer's automatic completion signal + a worker
re-drive) *is* the wake. A lost completion signal degrades to a host re-drive of the parent (it
re-checks the child journal), not a lost result.

---

## Related

- [ADR-0018](../architecture/adr/0018-sub-workflow-composition-and-approvals.md) — the composition design.
- [Work dispatch](dispatch.md) — the `Registry` / `Pool` / work-queue the queue path dispatches onto.
- [Persistence](persistence.md) — the SQLite store + multi-process safety the queue path requires.
- [Durable continuations (ADR-0009)](../architecture/adr/0009-durable-continuations-waiting-status.md) —
  the `Waiting` / suspend-resume mechanism approvals and parked-await ride.
</content>
