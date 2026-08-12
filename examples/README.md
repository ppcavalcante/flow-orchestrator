# Flow Orchestrator — Examples

A deliberate progression, from a three-node graph to a crash-durable, multi-worker,
human-in-the-loop pipeline. Every example is **runnable** (`go run ./examples/<dir>`) and
**tested** — each ships a smoke test that actually executes it and asserts the real effect, so
an example cannot silently rot the way a compile-only example can. Run them all with:

```bash
GOTOOLCHAIN=local go test ./examples/...
```

The numbering is a learning order, not a dependency order — jump to whichever capability you need.

## Fundamentals

| # | Example | Shows |
|---|---------|-------|
| 01 | [`01-hello-dag`](01-hello-dag/) | Nodes, dependencies, `WorkflowData`, typed `Action`/`ActionFunc` |
| 02 | [`02-errors-and-retries`](02-errors-and-retries/) | `ContinueOnError`, `WithRetries`, capped backoff + jitter, the error taxonomy |

## The moat — durability

| # | Example | Shows |
|---|---------|-------|
| 03 | [`03-durable-crash-resume`](03-durable-crash-resume/) | SQLite store; a process is **killed mid-run** and resumes exactly-once, replaying no completed work — the core thesis: *workflow is data, not replay* |

## Rich control flow

| # | Example | Shows |
|---|---------|-------|
| 04 | [`04-choice-and-merge`](04-choice-and-merge/) | Conditional branching, the `Bypassed` status, re-converging paths |
| 05 | [`05-dynamic-fanout`](05-dynamic-fanout/) | `AddFanOut` over `N` items discovered at runtime, `WithMaxWidth`, partial collection, crash-resume rebuilding the exact branches |
| 06 | [`06-saga-compensation`](06-saga-compensation/) | Durable reverse-order rollback on failure, `Compensated` / `CompensationFailed` |

## Coordination & humans-in-the-loop

| # | Example | Shows |
|---|---------|-------|
| 07 | [`07-signals-timers-approvals`](07-signals-timers-approvals/) | `AddApproval` + `ApproveSignal` (with the correlation nonce), a durable first-of(signal, timer) |
| 08 | [`08-sub-workflows`](08-sub-workflows/) | Composing a child graph, parked children, depth bounds |

## Distribution & governance

| # | Example | Shows |
|---|---------|-------|
| 09 | [`09-competing-consumers`](09-competing-consumers/) | The dispatch model (`RunNext`/`RunWorker`), leases + fencing, several workers draining one queue |
| 10 | [`10-scheduling-and-caps`](10-scheduling-and-caps/) | Durable cron / interval / one-shot schedules + cross-process concurrency caps |
| 11 | [`11-governance-boundary`](11-governance-boundary/) | M23 `WithBoundary` — a verifier-dominance check that **refuses at Build** any topology reaching the sink without passing the verifier |

## Ops & capstone

| # | Example | Shows |
|---|---------|-------|
| 12 | [`12-observability`](12-observability/) | Exporting the in-memory metrics to OpenTelemetry (its own Go module) |
| ★ | [`capstone-document-pipeline`](capstone-document-pipeline/) | A realistic app combining fan-out, choice, approval, a sub-workflow, and durable multi-worker dispatch — the full capability set in one place |

## Conventions

- Each directory is a `package main` in the root module (except `12-observability`, which has its
  own `go.mod` because it pulls in the OpenTelemetry SDK).
- `main()` reports **infrastructure** failure through a non-zero exit code; a *demonstrated* workflow
  outcome (a deliberate failure the example is teaching) is not treated as an error.
- Each example's `*_test.go` runs its core and asserts the durable effect. Examples that fork or kill
  subprocesses (`03`, `09`) guard the heavy path behind `testing.Short()`.
- Always run the toolchain with `GOTOOLCHAIN=local` in this repo.
