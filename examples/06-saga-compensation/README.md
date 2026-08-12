# 06 · Saga compensation

Durable, reverse-order rollback: a chain of forward steps each declares a compensation (its
undo). When a later step fails, the engine rolls the run back — running the compensations of
the already-completed steps in **reverse-topological order** — and each undone step ends in
the terminal `Compensated` status. Two scenarios run: one where every compensation succeeds,
one where a compensation itself fails.

## What it shows

- **`WithCompensationFunc(fn)`** — attach an undo to a forward step. A step with no
  compensation is a rollback no-op.
- **A hard failure triggers the saga** — `finalize` always fails; because it is not
  continue-on-error, the run fail-fasts and rolls back the three completed compensable steps
  above it. `finalize` itself never Completed, so it is never compensated (it ends `Failed`).
- **Reverse-topological order** — a step is undone *after* its dependents: the rollback runs
  `issue-ticket`, then `charge-card`, then `reserve-seat`. The order is recorded durably into
  the run's `WorkflowData`, so it is read back from the store.
- **`Compensated` vs `CompensationFailed`** — scenario B fails `charge-card`'s compensation:
  that step ends `CompensationFailed` (an honestly un-undone effect), while its neighbours
  still end `Compensated`.
- **The outcome types** — a *clean* rollback (every compensation succeeded) returns the
  trigger cause wrapped in `ErrRolledBack`, **not** a `*SagaError`. A *partial* rollback
  returns a `*SagaError` partitioning the Completed nodes into `Compensated` /
  `FailedToCompensate` / `Skipped`.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/06-saga-compensation
GOTOOLCHAIN=local go test ./examples/06-saga-compensation/ -count=1
```

## Key API calls

```go
b.AddNode("charge-card").
    WithActionFunc(fn).
    WithCompensationFunc(undo).
    DependsOn("reserve-seat")

execErr := wf.Execute(ctx)

var sagaErr *workflow.SagaError
if errors.As(execErr, &sagaErr) {
    // partial: sagaErr.Compensated, sagaErr.FailedToCompensate (each a workflow.NodeError)
} else if errors.Is(execErr, workflow.ErrRolledBack) {
    // clean rollback — every compensation succeeded
}

data.GetNodeStatus("charge-card") // Compensated, or CompensationFailed if its undo failed
```

## Expected output

Scenario A rolls back cleanly: `issue-ticket`, `charge-card`, `reserve-seat` are undone in
that order, the outcome is a clean rollback, and the recorded compensation order is
`issue-ticket,charge-card,reserve-seat`. Scenario B fails `charge-card`'s compensation: the
outcome is a partial rollback with `failed-to-compensate=[charge-card]`, the other two steps
still Compensated, and the durable order records only the successful undos
(`issue-ticket,reserve-seat`).
