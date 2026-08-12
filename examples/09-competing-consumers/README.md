# 09 — Competing Consumers (dispatch, leases, fencing)

The dispatch model: **the workflow is DATA on a shared durable queue**, and N interchangeable
workers drain it. Each enqueued item's effect is applied **exactly once** across the whole
fleet — not because each worker is careful, but because the effect is idempotent and the
coordination is structural.

## What it shows

- A **SQLite work queue** (`NewSQLiteStore(path, WithMultiProcess())`) shared by a fleet.
- A registered **`DAGFactory`** keyed by a work type (`"process-order"`) — the unit of work.
- Several items **enqueued**, then **multiple concurrent workers** (goroutines, each with its
  own store handle) draining the one queue via `RunNext`.
- **Idempotent exactly-once effect**: every order's `charge` is applied once (keyed on the
  order id), proven against a shared side-effect ledger independent of the engine's journal.
- **Leases + fencing**: a slow/dead worker's claim lapses and a sibling **re-claims** it
  (liveness); the reclaim issues a strictly greater monotonic **fencing token**, and the
  superseded worker's late checkpoint is **fenced out** (safety), so the journal is never
  corrupted by a stale writer.

## The honest contract: at-least-once execution, idempotent exactly-once effect

Durable dispatch is **at-least-once execution**. If a worker is descheduled long enough for
its lease to lapse (aggravated under `-race`, which slows drives ~10x), a sibling reclaims and
re-runs the action whose Completed status was not yet checkpointed. The engine's own **journal**
is exactly-once, but an **external** effect that is not part of the transactional checkpoint
can run more than once. So an exactly-once *outcome* comes from making the effect **idempotent**
— here, charging is keyed on the order id, so a re-execution is a no-op on the effect.

The demo and test therefore assert the two facts the contract distinguishes: the applied
**effect** is exactly-once (each order charged, none lost, none phantom), while **execution
attempts** are at-least-once (>= the order count; a reclaim legitimately re-runs an action).
This is the lesson: **give durable effects an idempotency key.**

## How to run

```bash
GOTOOLCHAIN=local go run ./examples/09-competing-consumers
GOTOOLCHAIN=local go test ./examples/09-competing-consumers/ -count=1
```

## Key API calls

| Call | Role |
|---|---|
| `workflow.NewSQLiteStore(path, WithMultiProcess(), WithLeaseTTL(ttl))` | shared multi-process queue store, per-worker handle |
| `workflow.NewRegistry()` / `reg.Register(typ, DAGFactory)` | map a work type to its graph |
| `store.Enqueue(workflowID, typ, jsonInput)` | put an item on the queue |
| `workflow.RunNext(ctx, store, reg, ownerID)` | atomically claim + drive the next item |
| `store.Claim(ctx, workflowID, ownerID)` → `FencingToken` | acquire / re-claim a lapsed lease |
| `store.Renew(workflowID, token)` → `ErrFencedOut` | a superseded token is rejected |

## Expected output

```
enqueued 12 orders onto the shared queue
worker-0 drove 2 orders
worker-1 drove 0 orders
worker-2 drove 1 orders
worker-3 drove 9 orders
fleet performed 12 drives across 4 workers for 12 orders

result: 12 orders, effect-applied-exactly-once=12 lost=0; execution-attempts=12 (>= orders: at-least-once)
```

The per-worker split is nondeterministic (whoever claims first wins) — that is the point of
interchangeable consumers. Under heavy load (or `-race`) `execution-attempts` may exceed the
order count as reclaims re-run actions; the idempotent effect stays exactly-once regardless.
