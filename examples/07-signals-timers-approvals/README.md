# 07 — Signals, Timers & Approvals

Human-in-the-loop and durable waiting: a run **parks** on an external event (a human
decision or a clock) and resumes later — possibly in another process — replaying no
completed work.

## What it shows

- **`AddApproval("gate")`** — a decision gate. When reached the run **parks** (`Waiting`)
  until an approve/reject decision is delivered to the durable mailbox. `Execute` returns
  **`ErrSuspended`** — a *success* arm (the checkpoint is durably flushed), branched with
  `errors.Is(err, ErrSuspended)`, never treated as a failure.
- **The AUD-025 correlation nonce** — releasing the gate requires the nonce that correlates
  a decision to *this* park. A decision with the **wrong (or absent) nonce is inert**: the
  run stays parked. This is the security-relevant behavior — a stale or forged approval can
  neither approve nor reject. The nonce is derived from the live workflow
  (`(*Workflow).ApprovalNonce`) or, for a store-only driver, from the store
  (`ApprovalNonceFromStore`); the two agree.
- **`AddWaitForSignalTimeout("await", "payment", timeout)`** — a durable
  **first-of(signal, timer)**. The run parks until *either* the named signal arrives *or*
  an absolute deadline (frozen at first encounter, so it's durable-remaining across a crash)
  passes. Whichever comes first wins. A `FakeClock` drives durable time deterministically —
  advancing it past the deadline fires the timer instantly, the way a real "3h later" resume
  would.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/07-signals-timers-approvals
GOTOOLCHAIN=local go test ./examples/07-signals-timers-approvals/ -count=1
```

## Key API

| Call | Role |
|---|---|
| `WorkflowBuilder.AddApproval(name)` | declare a parking approval gate |
| `Workflow.Execute` → `ErrSuspended` | the three-outcome park contract |
| `(*Workflow).ApprovalNonce(node)` | correlation nonce from a live workflow |
| `ApprovalNonceFromStore(store, id, node)` | same nonce from the store alone (dispatcher path) |
| `ApproveSignal(node, approver, comment, sigID, nonce)` | the decision payload |
| `Workflow.DeliverAndResume(ctx, sig)` | enqueue a decision then drive |
| `WorkflowBuilder.AddWaitForSignalTimeout(name, signal, timeout)` | durable first-of(signal, timer) |
| `WorkflowBuilder.WithClock` / `NewFakeClock` / `FakeClock.Advance` | deterministic durable time |

## Expected output

```
approval: run parked, "gate" is Waiting
approval: wrong-nonce decision was INERT — still parked
publish: release published (gate was approved)
approval: resumed to completion, published=true

settle: run converged past the first-of wait
first-of: signal arm won, await output="paid-in-full"
settle: run converged past the first-of wait
first-of: timer arm won (no signal), await output="true" (the timeout sentinel)
```
