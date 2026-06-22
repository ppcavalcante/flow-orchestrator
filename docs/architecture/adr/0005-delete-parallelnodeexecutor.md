# 0005. Delete the standalone ParallelNodeExecutor

## Status

Accepted (milestone M3, 2026-06-15).

## Context

`ParallelNodeExecutor` / `NewParallelNodeExecutor` / `ExecuteNodes` formed a standalone
parallel executor that honored `ExecutionConfig` — but it had **zero production callers**
(its only 5 callers were tests and benchmarks). It appears to predate the inline executor
that `DAG.Execute` actually uses. Once `MaxConcurrency` is wired into the real path
(ADR 0004), its only distinct behavior (honoring the config) becomes the real path's behavior.

## Decision

**Delete all three symbols.** Migrate the 5 bench/test callers to drive `DAG.Execute` with
a configured `ExecutionConfig` — which is the thing actually worth benchmarking now.

## Consequences

- Removes pure redundancy; one execution path, configured the same way everywhere.
- **Breaking** for any out-of-tree caller (there were none in-tree beyond tests/benchmarks).
  Migration note in the CHANGELOG: use `ExecutionConfig.MaxConcurrency` via `WithExecutionConfig`.
- Benchmarks now measure the real path.

## Alternatives Considered

- **Unexport instead of delete** — leaves dead code with no caller; deletion is cleaner once
  the live path honors the config.
- **Keep it public** — gives consumers two ways to do the same thing, the opposite of the
  surface-truthfulness goal.

## References

- `pkg/workflow/parallel_execution.go`; bench callers in `internal/workflow/benchmark/`.
- Paired with ADR 0004.
