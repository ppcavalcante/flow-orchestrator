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

// A human approves — deliver + resume in one call.
sig := workflow.ApproveSignal("sign-off", "alice@example.com", "LGTM", "decision-1")
if err := wf.DeliverAndResume(ctx, sig); err != nil {
    // A reject surfaces as *ApprovalRejectedError; classify with errors.As.
    var rej *workflow.ApprovalRejectedError
    if errors.As(err, &rej) {
        log.Printf("rejected by %s: %s", rej.Approver, rej.Comment)
    }
}
```

- The approve payload is **persisted for audit** (approver + comment) and surfaced as the node output.
- `sigID` (the last arg) is a host-supplied dedupe key — re-delivering the same ID is idempotent.
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
| `AddSubWorkflowParked(name, child *DAG)` | definition-value | **out-of-band** | park → wake |
| `AddSubWorkflowQueued(name, childType)` | **type-ref**, may be suspendable | **queue** (`Pool`) | park → wake |

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
child.AddNode("compute").WithAction(func(ctx context.Context, d *workflow.WorkflowData) error {
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
park). Route such a child to the queue path instead. Do **not** also call `WithAction`.

### Queue sub-workflow (`AddSubWorkflowQueued`) — type-ref / suspendable children

The explicit opt-in for a child referenced by **type** and/or one that **parks** (e.g. a child with
its own approval). The parent node enqueues the child onto the M17 work queue (carrying the parent's
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
    b.AddApproval("analyst-sign-off")           // suspendable child → must be the queue path
    b.AddNode("score").WithAction(score).DependsOn("analyst-sign-off")
    return b.Build()                             // Build() returns (*DAG, error); the child Sets its result key
})

parent := workflow.NewWorkflowBuilder()
parent.AddSubWorkflowQueued("risk", "risk-check").
    WithInput(map[string]any{"applicant": "acme"}). // seeds the child's data keys
    WithResult("risk_score", "score_out")

parent.WithStore(sqliteStore).WithWorkflowID("loan-9")
wf, _ := workflow.FromBuilder(parent) // FromBuilder → *Workflow (Build() returns *DAG)
wf.Registry = reg                     // inject the type→DAG map (CODE)
// Run wf under a Pool; the parent parks at "risk" and wakes when the child terminalizes.
```

`WithInput(map)` seeds the child's data keys (JSON-encoded into the queue row's input). It is valid
**only** on a queued node — inline/parked children read the parent data directly.

### Parked sub-workflow (`AddSubWorkflowParked`)

The definition-value child runs **out-of-band** (a host/producer runs it) and the parent parks until
a completion signal wakes it. Use this when you run the child yourself and signal completion with
`SubWorkflowCompletionSignal(nodeName, sigID)`. Most embedders want `AddSubWorkflowQueued` (the queue
producer emits the completion signal for you); `AddSubWorkflowParked` is the lower-level seam.

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
