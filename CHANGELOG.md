# Changelog

All notable changes to Flow Orchestrator will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Records milestone state for M4, M5, M6, and M7. The in-code version constant is
`0.7.3-alpha` (the combined M6+M7 release; M5 set `0.5.0-alpha`, the 0.6 line was
never cut as a const, M7 close set `0.7.0-alpha`, then patches followed from the
first real CI runs on `main` — see `version.go`). Published tags:
`v0.7.0-alpha` … `v0.7.3-alpha`; **use `v0.7.3-alpha`** (the latest; every tag is a
pre-release so `go get @latest` resolves to it). (`v0.7.0-alpha`'s first CI run
surfaced the int64 JSON-fidelity bug fixed in `0.7.1-alpha`; `v0.7.1-alpha`'s run
surfaced a coverage gate failure fixed in `0.7.2-alpha`; `0.7.3-alpha` is a
dead-code + docs-accuracy cleanup.) See [STABILITY.md](STABILITY.md).

### Changed
- **Dead-code prune + docs accuracy (0.7.3-alpha).** Removed the orphaned
  `internal/workflow/concurrent` package (zero production importers after the
  0.7.2 `concurrent_map.go` deletion) and its coverage/property-test gate entries;
  swept the now-dangling references to it from the docs. Corrected the install/
  versioning docs: all tags are pre-releases (no stable `v1`+ release), so
  `go get @latest` resolves to the highest pre-release (`v0.7.3-alpha`) — the prior
  notes wrongly implied a stable `v0.1.0` and that `@latest` wouldn't select the
  current release. Removed the ancient `v0.1.0-alpha`/`v0.1.1-alpha` tags. No
  library-facing API or behavior change.
- **Test coverage gate cleared honestly (0.7.2-alpha).** The first CI run of
  `v0.7.1-alpha` failed the per-package coverage gate (`pkg/workflow` 88% vs the 90%
  threshold). Closed it the honest way rather than padding or lowering the bar:
  deleted the entirely-dead `pkg/workflow/concurrent_map.go` (an unexported,
  test-only wrapper over `internal/workflow/concurrent` with zero production
  callers) and added behavior-asserting tests for genuinely-public untested API
  (`WithExecutionConfig`, builder/DAG error paths, `GetFloat64`/`GetInt` conversions,
  the int64 JSON round-trip incl. `MaxInt64`, store `Delete`). `pkg/workflow` → 90%.

### Fixed
- **int64 JSON-persistence fidelity (0.7.1-alpha).** `WorkflowData.LoadFromJSON`
  (and `JSONFileStore` save/load) decoded JSON numbers through `interface{}` →
  `float64`, silently losing precision for int64 magnitudes above 2^53 (e.g.
  `MaxInt64` rounded to 2^63, then `int64(2^63)` overflowed — platform-defined, so
  it passed on arm64 and corrupted on amd64). Now decoded with
  `json.Decoder.UseNumber()` + `json.Number.Int64()`, preserving the full int64
  range exactly and matching the FlatBuffers `value_long` path. Surfaced by the
  first CI run of `TestCrossBackendParity` on amd64.
- **release workflow.** `release.yml` referenced the SLSA Go *builder* workflow with
  builder-style inputs (invalid → the workflow failed to parse). Switched to the
  generic SLSA generator attesting the released artifact (SBOM) by digest, which is
  the correct tool for a library (no built binary).

### Added
- **Typed-key data API (milestone M7)** — an additive, type-safe layer over the
  string-keyed `WorkflowData.Set`/`Get`: `Key[T]` (with `NewKey[T](name)` and
  `Name()`) plus the package-level generic functions `Set[T]`, `Get[T]`
  (comma-ok; returns `(zero, false)` on absent-or-type-mismatch, never panics),
  and `GetOr[T]`. Producer and consumer share a typed `Key`, so type mismatches
  are compile errors. Values live in the same underlying store as the string API,
  so the two fully interoperate; the typed layer adds no state and inherits
  `WorkflowData`'s locking. (Go forbids type-parameterized methods, hence
  package-level functions.) See
  [api-reference.md → Typed-Key Data API](docs/reference/api-reference.md#typed-key-data-api-added-v070).
- **Per-node continue-on-error (milestone M7)** — `Node.WithContinueOnError()` and
  `NodeBuilder.WithContinueOnError()`, plus the `Node.ContinueOnError` field. A
  node so marked still records `Failed` on failure, but the failure no longer
  cancels its siblings or fails the workflow; dependents run and observe its
  `Failed` status. The dependency guard treats a continue-on-error `Failed`
  dependency as *resolved* (`DEC-P21-depguard`); a normal `Failed` dependency
  still blocks fail-fast. `DAG.Execute` returns `nil` iff every
  non-continue-on-error node succeeded. The default (unset) behavior is unchanged.
  See [api-reference.md → Failure Semantics](docs/reference/api-reference.md#failure-semantics).
- **Formal & property-based verification of the executor (milestone M7)** —
  `pkg/workflow/invariants_property_test.go` is a gopter property suite over random
  DAGs (topological order, peak concurrency within `MaxConcurrency`, run-once
  completeness, dependencies-before-run, and the continue-on-error/fail-fast
  failure semantics), gated in `go test ./...`. `specs/` adds a TLA+/PlusCal model
  of the level executor (`Executor.tla` + `MCExecutor.tla` with three `.cfg`
  scenarios), TLC-checked for safety (concurrency bound, dependencies-before-run,
  fail-fast halting) and liveness (termination / deadlock-freedom). See
  [`specs/README.md`](specs/README.md) for the honest scope.
- **OpenTelemetry metrics bridge (milestone M6)** — `pkg/workflow/metrics`
  gains `OTelBridge` and `NewOTelBridge(c *MetricsCollector, mp metric.MeterProvider)`
  (plus `(*OTelBridge).Shutdown`), bridging the existing metrics to OpenTelemetry
  via the OTel metrics **API only** (no SDK in the library's non-test dependency
  graph; the host owns the SDK/exporter). It registers 6 observable instruments
  under `flow_orchestrator.operation.*`. A separate-module
  `examples/observability/` and the [Observability guide](docs/guides/observability.md)
  ship with it.
- **CI/release hardening (milestone M5)** — the curated `golangci-lint` v2 set now actually
  runs in CI (pinned **v2.12.2**, matching the `Makefile`; the prior 4-milestone "dark lint"
  came from a floated `latest` running v1 against a v2 config plus CI never triggering on the
  branch). CI now triggers on `anvil-m1`/`main` pushes and all PRs, runs a `[1.24.x, 1.25.11]`
  Go matrix (go.mod floor + patched dev toolchain), and gates on build, `go vet`, `gofmt`,
  `go test ./... -race`, and `govulncheck` (pinned `v1.4.0`, blocking on *called*
  vulnerabilities). GitHub Actions are SHA-pinned.
- **Contributor-experience files (milestone M5, REL-01)** — root `CONTRIBUTING.md`,
  `CODE_OF_CONDUCT.md` (Contributor Covenant), `.github/ISSUE_TEMPLATE/` (bug + feature),
  `.github/PULL_REQUEST_TEMPLATE.md`, and honest README maturity/coverage badges.
- **Layered bounds guard on `FlatBuffersStore.Load` (milestone M4)** — malformed, truncated, or
  absurd-element-count `.fb` input is rejected *before* the FlatBuffers decode (a file-size cap,
  a root-offset and minimum-length sanity pre-check, and per-element count caps), returning
  `ErrCorruptData` with `data == nil` and no panic. The M1 `recover()` remains as a residual
  backstop. Bounds are internal default constants (64 MiB file size, 1 Mi elements) — **no new
  public surface**. See [ADR-0008](docs/architecture/adr/0008-layered-bounds-guard-trust-reratify.md).
- `FuzzFlatBuffersStoreLoad` fuzz harness with a seeded malformed corpus and regression-seed
  replay wired into the normal test suite (M4).

### Changed
- **Version constant bumped to `0.5.0-alpha`** (M5; folds in M4's skipped bump). The
  authoritative release version remains the git tag, cut post-release; the constant is the
  in-code dev marker.
- **Install guidance is now honest** (M5, REL-02): `docs/getting-started/*` use a working
  install line plus a "Versioning" note. *(Historical: at M5 the latest tag was `v0.1.1-alpha`,
  which predated the M2–M5 hardening. That tag — and `v0.1.0-alpha` — were later deleted; the
  only published tags are now `v0.7.0/0.1/0.2-alpha`, and `@latest` resolves to `v0.7.2-alpha`.
  The install docs reflect the current state.)*
- **FlatBuffers `Load` size cap is now race-free** (M5, SEC-01): enforced atomically by reading
  through an `io.LimitReader(cap+1)` rather than `os.Stat`-then-`os.ReadFile`, removing the
  TOCTOU window. Posture and bounds unchanged.
- The persistence trust contract is **re-ratified, not re-opened** (M4): "malformed input is
  rejected before structural traversal," not merely recovered-from-panic. Posture unchanged
  (caller-controlled; ceiling = availability; NOT adversarial-proof). The semantic-forgery
  residual is disclosed explicitly — the guard checks structure, not meaning, so a
  well-formed-but-hostile file can still load. See `docs/guides/persistence.md` and ADR-0008.

## [0.3.0-alpha] - 2026-06-15

"API Truth & Surface Cleanup" (milestone M3). Aggressive pre-1.0 cleanup so the public
`pkg/workflow` surface tells the truth before later milestones harden, gate, and instrument
it. See [STABILITY.md](STABILITY.md) for the going-forward contract.

### Added
- `ExecutionConfig.MaxConcurrency` is now **wired end-to-end** into `DAG.Execute` — the
  per-level concurrency limit is configurable, with a bounded default of 16.
- `WorkflowBuilder.WithExecutionConfig(ExecutionConfig)` and `DAG.WithExecutionConfig(ExecutionConfig)`
  hooks to set the execution config.
- `DefaultMaxConcurrency` constant (16).
- Store/persistence error sentinels for `errors.Is` branching: `ErrNotFound`,
  `ErrValidation`, `ErrCorruptData`, `ErrIO` (wrapped with `%w`; `ErrCorruptData` keeps a
  generic public message so it does not leak file paths). Distinct from the existing
  action-execution sentinels (`ErrInputNotFound`/`ErrInvalidInput`/`ErrExecutionFailed`) —
  the two families are intentionally not aliased.

### Changed
- **BREAKING**: Moved internal packages out of `pkg/`: the generated FlatBuffers code
  (`pkg/workflow/fb/workflow` → `internal/workflow/fb/workflow`), the concurrent-map
  implementations (`pkg/workflow/concurrent` → `internal/workflow/concurrent`), and the
  misc helpers (`pkg/workflow/utils` → `internal/workflow/utils`).
  *Migration:* these were infrastructure/generated code with no intended external use; do
  not import them. Use the public `pkg/workflow` API.
- **BREAKING**: Unexported in-package-only helpers: `ExecuteNodesInLevel` →
  `executeNodesInLevel`; `StatusToFBStatus` (FB converter); the `ConcurrentMap`/
  `ReadOptimizedMap` family; the `StringInterner` family.
  *Migration:* these had no external callers; drive execution via `DAG.Execute` and the
  builder.
- **BREAKING**: `DefaultConfig().MaxConcurrency` default changed **4 → 16**. *Effective*
  concurrency is unchanged — the live execution path was already running a hardcoded 16; the
  default now matches it, and the value is honored end-to-end. A non-positive value coerces
  to 16; concurrency is never unbounded.

### Removed
- **BREAKING**: Deleted the unused standalone parallel executor:
  `ParallelNodeExecutor`, `NewParallelNodeExecutor`, and `ExecuteNodes`.
  *Migration:* use `ExecutionConfig.MaxConcurrency` via `WithExecutionConfig` on the builder
  or DAG — `DAG.Execute` now honors it directly.
- **BREAKING**: Removed inert `WorkflowDataConfig` fields `MaxInternStringLength` and
  `InternStringCapacity` (and the preset assignments to them). They were written by config
  presets but never read by the interner, so removal is **not a behavior change**.
  *Migration:* delete any assignments to these fields. (Configurable interning may return as
  an honest feature in a future release.)
- **BREAKING**: Removed the inert `ExecutionConfig.PreserveOrder` field (never wired).

### Fixed
- Documentation reconciled to the cleaned surface (`docs/reference/configuration.md`,
  `docs/reference/api-reference.md`, `docs/guides/performance-optimization.md`): the
  previously-documented phantom `MaxConcurrency` knob, the deleted `ParallelNodeExecutor`,
  the removed intern knobs, and the now-internal concurrent-map API.

## [0.2.0-alpha] - 2026-06-15

"Persistence Fidelity" (milestone M2). Integer round-trip fidelity for the FlatBuffers store,
plus the M1 correctness/security fixes that were never recorded in this file.

### Added
- `WorkflowData.GetInt64(key string) (int64, bool)` — a portable integer accessor that
  returns the full `int64` range on every architecture. Use it for values that may exceed
  `MaxInt32`; `GetInt` remains `(int, bool)` (platform-width) for backward compatibility.
- FlatBuffers schema gained an additive `value_long:long` field on `KeyValueInt`
  (regenerated FB code), enabling faithful `int64` storage.

### Fixed
- **Integer fidelity (FB store)**: the FlatBuffers store previously clamped integers to
  `int32` on save, silently corrupting values outside the `int32` range. Save now writes the
  full `int64` (`value_long`); Load reads `value_long` with a fallback to the legacy `value`
  for older files. The FlatBuffers store now matches the JSON store, which already round-tripped
  `int64` faithfully. (M2 fidelity contract: int64-widen-additive.)
- **`GetInt` documentation**: documented the 32-bit platform limit (values > `MaxInt32` are
  not representable as `int` on 32-bit builds — use `GetInt64`); this is a documented limit,
  not a silent bug.
- **(M1) Persistence crash hardening**: `FlatBuffersStore.Load` wraps decoding in `recover()`
  and returns a "corrupt workflow file" error instead of panicking on a malformed `.fb` file.
  (A full FlatBuffers verifier and size cap remain deferred.)
- **(M1) Path-traversal guard**: an unexported `validateWorkflowID` guard now protects all
  file-store entry points (save/load/list/delete), rejecting unsafe workflow IDs.

### Compatibility
- **Data-compatible**: older `.fb` files (with only the legacy `value` field) still load via
  the legacy fallback; the schema change is additive.

## [0.1.1-alpha] - 2025-07-01

### Changed
- **BREAKING**: Updated GitHub username from `pparaujo` to `ppcavalcante`
- **BREAKING**: Updated module path from `github.com/pparaujo/flow-orchestrator` to `github.com/ppcavalcante/flow-orchestrator`
- Updated all import statements throughout the codebase
- Updated documentation and configuration files to reflect new username

### Added
- Comprehensive documentation structure
- Development guides and references

## [0.1.0-alpha] - 2025-05-15

### Added
- Initial alpha release
- Core workflow engine
- DAG execution model
- WorkflowData management
- Basic middleware system
- Simple persistence layer
- Memory optimization components
- Concurrent data structures
- Workflow builder API
- Basic examples

### Known Issues
- API may change before the stable release
- Limited persistence options
- Documentation is being improved

[Unreleased]: https://github.com/ppcavalcante/flow-orchestrator/compare/v0.7.2-alpha...HEAD
[0.7.2-alpha]: https://github.com/ppcavalcante/flow-orchestrator/releases/tag/v0.7.2-alpha
[0.7.1-alpha]: https://github.com/ppcavalcante/flow-orchestrator/releases/tag/v0.7.1-alpha
[0.7.0-alpha]: https://github.com/ppcavalcante/flow-orchestrator/releases/tag/v0.7.0-alpha
[0.3.0-alpha]: https://github.com/ppcavalcante/flow-orchestrator/compare/v0.2.0-alpha...v0.3.0-alpha
[0.2.0-alpha]: https://github.com/ppcavalcante/flow-orchestrator/compare/v0.1.1-alpha...v0.2.0-alpha
[0.1.1-alpha]: https://github.com/ppcavalcante/flow-orchestrator/compare/v0.1.0-alpha...v0.1.1-alpha
[0.1.0-alpha]: https://github.com/ppcavalcante/flow-orchestrator/releases/tag/v0.1.0-alpha

<!--
Close-ritual note: this CHANGELOG is updated at every milestone close as part of the
documentation workstream. Each milestone records its Added/Changed/Removed/Fixed entries
with migration notes for any breaking change; breaks are batched into a minor release per
STABILITY.md. The git tags above are created at release time. The published tags are
`v0.7.0-alpha`, `v0.7.1-alpha`, `v0.7.2-alpha` (the old `v0.1.x-alpha` tags were deleted);
`@latest` resolves to `v0.7.2-alpha`. The v0.1.x/0.2.0/0.3.0 sections below are historical
milestone records — their compare links may 404 since those tags no longer exist.
-->

