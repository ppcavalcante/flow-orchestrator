# Example Applications

Flow Orchestrator ships a deliberate 13-example suite that progresses from a three-node graph to a
crash-durable, multi-worker, human-in-the-loop pipeline. Every example is **runnable** and **tested**
— each directory ships a smoke test that actually executes it and asserts the real durable effect, so
an example cannot silently rot.

The authoritative map is [`examples/README.md`](../../examples/README.md); this page mirrors it. The
numbering is a learning order, not a dependency order — jump to whichever capability you need.

**Run one:**
```bash
GOTOOLCHAIN=local go run ./examples/01-hello-dag
```

**Run (test) them all** — each example's smoke test executes its core:
```bash
GOTOOLCHAIN=local go test ./examples/...
```

> `12-observability` is a **separate Go module** (its own `go.mod`, because it pulls in the
> OpenTelemetry SDK). Run it from inside its directory: `cd examples/12-observability && go run .`

## Fundamentals

### 01 — Hello DAG
- **Location**: `examples/01-hello-dag`
- **Shows**: nodes, dependencies, `WorkflowData`, typed `Action` / `ActionFunc`.
- **Run**: `GOTOOLCHAIN=local go run ./examples/01-hello-dag`

### 02 — Errors and retries
- **Location**: `examples/02-errors-and-retries`
- **Shows**: `ContinueOnError`, `WithRetries`, capped backoff + jitter, the error taxonomy.
- **Run**: `GOTOOLCHAIN=local go run ./examples/02-errors-and-retries`

## The moat — durability

### 03 — Durable crash-resume
- **Location**: `examples/03-durable-crash-resume`
- **Shows**: a SQLite-backed run where the process is **killed mid-run** and resumes exactly-once,
  replaying no completed work — the core thesis, *workflow is data, not replay*. `main()` re-execs and
  self-kills a child to stage the crash.
- **Run**: `GOTOOLCHAIN=local go run ./examples/03-durable-crash-resume`

## Rich control flow

### 04 — Choice and merge
- **Location**: `examples/04-choice-and-merge`
- **Shows**: conditional branching, the `Bypassed` status, re-converging paths via a merge.
- **Run**: `GOTOOLCHAIN=local go run ./examples/04-choice-and-merge`

### 05 — Dynamic fan-out
- **Location**: `examples/05-dynamic-fanout`
- **Shows**: `AddFanOut` over `N` items discovered at runtime, `WithMaxWidth`, partial collection, and
  crash-resume rebuilding the exact branches.
- **Run**: `GOTOOLCHAIN=local go run ./examples/05-dynamic-fanout`

### 06 — Saga compensation
- **Location**: `examples/06-saga-compensation`
- **Shows**: durable reverse-order rollback on failure, `Compensated` / `CompensationFailed`.
- **Run**: `GOTOOLCHAIN=local go run ./examples/06-saga-compensation`

## Coordination & humans-in-the-loop

### 07 — Signals, timers, approvals
- **Location**: `examples/07-signals-timers-approvals`
- **Shows**: `AddApproval` + `ApproveSignal` (with the correlation nonce), a durable
  first-of(signal, timer).
- **Run**: `GOTOOLCHAIN=local go run ./examples/07-signals-timers-approvals`

### 08 — Sub-workflows
- **Location**: `examples/08-sub-workflows`
- **Shows**: composing a child graph, parked children, depth bounds.
- **Run**: `GOTOOLCHAIN=local go run ./examples/08-sub-workflows`

## Distribution & governance

### 09 — Competing consumers
- **Location**: `examples/09-competing-consumers`
- **Shows**: the dispatch model (`RunNext` / `RunWorker`), leases + fencing, several workers draining
  one queue; at-least-once external effects vs the exactly-once journal.
- **Run**: `GOTOOLCHAIN=local go run ./examples/09-competing-consumers`

### 10 — Scheduling and caps
- **Location**: `examples/10-scheduling-and-caps`
- **Shows**: durable cron / interval / one-shot schedules via a `DueTimers` / `Tick` poller, plus
  cross-process concurrency caps.
- **Run**: `GOTOOLCHAIN=local go run ./examples/10-scheduling-and-caps`

### 11 — Governance boundary
- **Location**: `examples/11-governance-boundary`
- **Shows**: the M23 `WithBoundary` verifier-dominance check — it **refuses at Build** any topology
  reaching the sink without passing the verifier.
- **Run**: `GOTOOLCHAIN=local go run ./examples/11-governance-boundary`

## Ops & capstone

### 12 — Observability
- **Location**: `examples/12-observability` (its own Go module)
- **Shows**: exporting the in-memory metrics to OpenTelemetry via the API-only bridge. Metrics default
  to **disabled** — the example calls `metrics.NewConfig().WithEnabled(true)`. See the
  [Observability guide](../guides/observability.md) for the instrument inventory and OTLP wiring.
- **Run**: `cd examples/12-observability && GOTOOLCHAIN=local go run .`

### ★ Capstone — document pipeline
- **Location**: `examples/capstone-document-pipeline`
- **Shows**: a realistic app combining fan-out, choice/merge, a sub-workflow, and an approval,
  published on SQLite with durable multi-worker dispatch — the full capability set in one place.
- **Run**: `GOTOOLCHAIN=local go run ./examples/capstone-document-pipeline`

## Adapting examples for your use case

The examples are starting points: copy the closest one, extract a specific pattern, or combine
capabilities from several. Each ships its own `README.md` with a focused walkthrough.

## Additional resources

- See the [Getting Started](../getting-started/) section for basic usage.
- Explore the [Guides](../guides/) section for detailed feature docs.
- Check the [API Reference](./api-reference.md) for the full API.
