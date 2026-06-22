# 0004. Wire MaxConcurrency end-to-end; default 16, bounded

## Status

Accepted (milestone M3, 2026-06-15). Supersedes the M1 documentation of `MaxConcurrency` as a phantom knob.

## Context

`ExecutionConfig.MaxConcurrency` was a "phantom knob": `DAG.Execute` ran a hardcoded
`chan struct{}, 16` semaphore and never consulted the config. A redundant
`ParallelNodeExecutor` honored the knob but had zero production callers. There was also a
3-way default conflict: `DefaultConfig` said 4, the dead executor said 4, the live path
hardcoded 16. Configurable concurrency is table-stakes for enterprise production sizing.

## Decision

**Wire `MaxConcurrency` end-to-end.** Hold an `ExecutionConfig` on the `DAG` (set to
`DefaultConfig()` in the constructors); add `WithExecutionConfig` hooks on both
`WorkflowBuilder` and `DAG`; pass the limit into `executeNodesInLevel`, replacing the
hardcoded 16. Resolve the default conflict by bumping `DefaultConfig().MaxConcurrency`
**4 → 16** so the wired default preserves the *effective* behavior (the live path already
ran 16). A non-positive value coerces to the bounded default of 16 — concurrency is
**never unbounded** (one goroutine per node on a large level is a goroutine-explosion / DoS
hazard).

## Consequences

- The knob is real: callers can size per-level concurrency through the builder or DAG.
- **Behavior change, not a removal**: `DefaultConfig` default 4→16; effective concurrency is
  unchanged because the live path was already 16. Recorded in the CHANGELOG.
- Concurrency is always bounded; there is no unbounded mode by design.

## Alternatives Considered

- **Keep `DefaultConfig`'s existing 4** — silently drops effective concurrency 16→4, a perf
  regression masquerading as cleanup.
- **`<=0` means unbounded** — goroutine-explosion DoS risk on large levels.
- **Remove the knob, keep fixed 16** — gives up the configurable concurrency enterprises require.

## References

- `pkg/workflow/parallel_execution.go`, `pkg/workflow/dag.go`, `pkg/workflow/builder.go`
- Paired with ADR 0005 (delete the now-redundant `ParallelNodeExecutor`).
