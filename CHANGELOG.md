# Changelog

All notable changes to Flow Orchestrator will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.0-alpha]

**M9 — Durable Execution Core (Tier 1: crash-resume).** A process can now crash mid-run
and resume from where it left off, re-running only the work that had not completed. The
whole milestone is **ADDITIVE — there are NO breaking changes** (in contrast to the
breaking `0.8.0-alpha`): the new durability is an *optional* `Checkpointer` interface a
store may implement, a new `IdempotencyKey` helper, and internal hardening of the file
writes. Existing code, the `WorkflowStore` interface, and `DAG.Execute`'s signature are
unchanged; a store that does not implement `Checkpointer` behaves exactly as before. The
in-code version constant is `0.9.0-alpha`; every tag is a pre-release, so `go get @latest`
resolves to it. See [STABILITY.md](STABILITY.md) and
[docs/guides/persistence.md](docs/guides/persistence.md).

### Added (0.9.0-alpha — M9)
- **Durable crash-resume via an optional `Checkpointer` interface.** A `WorkflowStore` MAY
  implement `Checkpointer { SaveCheckpoint(*WorkflowData) error }`. When it does,
  `Workflow.Execute` flushes the run's state at each completed level barrier (the
  per-level checkpoint, wired through `ExecutionConfig` — `DAG.Execute`'s signature is
  unchanged). The three built-in stores implement it: `JSONFileStore` /
  `FlatBuffersStore` checkpoint atomically; `InMemoryStore` checkpoints into its map. A
  store that does NOT implement `Checkpointer` keeps the prior save-at-boundaries-only
  behavior with zero overhead.
- **Resume = re-run `Execute` with the same `WorkflowID` + store + DAG.** Nodes the journal
  records `Completed` are skipped and their outputs rehydrated; every not-done node —
  including any node in flight at the crash — re-runs. No new `Resume()` method is needed.
- **Graph-identity guard.** If the persisted state references a node the current DAG no
  longer contains, resume is rejected with `ErrValidation` (the node-identity analog of
  workflow versioning) rather than silently mis-resuming a changed graph.
- **`IdempotencyKey(data *WorkflowData, nodeName string) string`.** A deterministic,
  replay-stable dedupe key for side-effecting actions, derived ONLY from
  `(WorkflowID, nodeName)` so it is byte-identical across a crash-resume re-run. Stable
  format (a compatibility contract): `hex(SHA-256( uint64-LE(len(workflowID)) ||
  workflowID || nodeName ))`. See the at-least-once contract below.

### Fixed (0.9.0-alpha — M9)
- **Atomic file writes (torn-write guard).** Both file stores previously wrote via a bare
  `os.WriteFile`, which a crash mid-write could leave torn/partial. `Save` and
  `SaveCheckpoint` now write via temp-file + `fsync` + atomic `rename` (with a parent-dir
  fsync), so a crash leaves either the prior file or the new file fully intact, never a
  mix. This is both the durability foundation for checkpointing and an independent fix to
  a latent corruption-on-crash bug.

### Contract (0.9.0-alpha — M9)
- **At-least-once on any non-completed node — side effects MUST be idempotent.** On resume,
  any node that had not reached `Completed` when the process died — *including a node that
  was in flight* — re-runs, because the crash can land after a side effect but before that
  node's completion was checkpointed. This is the same guarantee Temporal / DBOS / Restate
  impose; it is a contract, not a bug. Make side-effecting actions idempotent (use
  `IdempotencyKey` to drive downstream dedupe). Documented in
  [STABILITY.md](STABILITY.md) and the persistence guide.

### Verified (0.9.0-alpha — M9)
- **Crash-resume serialization fidelity (int64).** A gopter property over the real
  checkpoint `Save → reload` path asserts every value's exact type and magnitude survives
  (the int64 killers — `MaxInt64` / `MinInt64` / the 2^53 boundary — ride every
  iteration), guarding the durability multiplier of the v0.7.1 int64-via-float64 saga.
  Mutation-proven (reverting the `UseNumber` codec falsifies it).
- **TLA+ `DurableExecutor.tla` — machine-checked resume-equivalence.** A new durable model
  (the base `Executor.tla` left byte-unchanged) adds `Checkpoint` / `Crash` / `Recover` and
  proves, exhaustively across continue-on-error / hard-fail / clean scenarios: the retained
  base safety invariants under crash/recover, **`ExecFidelity`** (a node reported done/
  failed actually executed — the output arm of resume-equivalence), **`StatusConvergence`**
  (every settled state reached with crashes is one a no-crash run could reach — the status
  arm), **`NoDoubleCommit`** (a checkpointed-`done` node never re-runs), and `Termination`
  including post-crash recovery. Every new property is mutation-proven to bite. See
  [specs/README.md](specs/README.md).

## [0.8.0-alpha]

**M8 Phase B — pre-1.0 hardening & consolidation.** The post-v0.7.4 batch from the
deep-review roadmap: it sharpens the execution/error/status contracts for the eventual
1.0, shrinks the public surface, and adds span-per-node tracing. **Pre-1.0, this release
cuts decisively — there are no deprecation cycles or kept aliases** (see the BREAKING
sections). The in-code version constant is `0.8.0-alpha`; every tag is a pre-release, so
`go get @latest` resolves to it. See [STABILITY.md](STABILITY.md).

### BREAKING CHANGES — Removed (0.8.0-alpha — M8 Phase B)
- **`NodeStatus.NotStarted` dropped.** The status enum is now 5 states —
  `Pending` / `Running` / `Completed` / `Failed` / `Skipped`. `NotStarted` was vestigial
  (never written by the engine) and had no FlatBuffers wire slot, so it could not
  round-trip. `Pending` is now the real written initial state (see Added).
- **Process-global metrics facade removed.** Metrics are per-instance only. The
  package-level `Enable` / `Disable` / `TrackOperation` / `Reset` / `Apply` / `Report`
  (and the rest of the standalone facade) are gone, along with the dead lock-contention
  apparatus. There is no process-global metrics state. The per-instance
  `MetricsCollector` and `OTelBridge`/`NewOTelBridge` are unchanged.
- **Five dead aliases removed:** `WithStateStore`, `WithRetry`, `ListNodeStatuses`,
  `NewWorkflowFromBuilder`, `GetNodeByName`.
- **Duplicate config presets removed:** `ReadOptimized` and `HighConcurrency` (byte-identical
  duplicates) and `Production` (its only trait was 1% metrics sampling, moot now that metrics
  default OFF). Presets collapse to `Default` + `LowMemory`.
- **`pkg/workflow/arena` internalized** to `internal/` (zero external importers); the
  `GetArenaStats` / `ResetArena` accessors and the two arena constructors are removed.
- **`SaveToFlatBuffer` / `LoadFromFlatBuffer` shims removed** — their own deprecation
  markers admitted they used JSON, not FlatBuffers.

### BREAKING CHANGES — Changed (0.8.0-alpha — M8 Phase B)
- **`DAG.Execute` now returns an aggregated `*ExecutionError`.** On a fail-fast halt it
  reports *every* node that failed in the halting level via
  `ExecutionError{FailedNodes []NodeError}` (previously only the first failure was
  returned, silently dropping concurrent siblings). `Unwrap() []error` lets `errors.Is`
  reach each node's error and any sentinel it wraps. The `Error()` string carries only
  node names and the actions' own error strings — never `WorkflowData` values, keys, or
  paths. A pure continue-on-error run (no fail-fast failure) still returns `nil`; coe
  failures stay tolerated and observable via `GetNodeStatus`.
- **Cancellation now wins over node failures.** If the context is cancelled or times out,
  `Execute` returns the wrapped ctx error (`errors.Is(err, context.Canceled)` /
  `context.DeadlineExceeded`) — never an `*ExecutionError` — regardless of where the
  cancel landed or whether a genuine failure coexisted. Incidental cancel-induced node
  errors are dropped; genuine failures remain observable via status. No `Cancelled`
  status was added.

### Added (0.8.0-alpha — M8 Phase B)
- **Real `Skipped` status.** A node is marked `Skipped` iff it did not run AND at least
  one dependency is in a terminal non-resolving state (a non-continue-on-error dependency
  that `Failed`, or a dependency that was itself `Skipped`); the rule is transitive.
  Independent nodes that were simply never reached (e.g. a run cancelled or halted before
  them, with no failed/skipped dependency) stay `Pending`, not `Skipped`. `Skipped` is not
  a failure — it never appears in `ExecutionError`.
- **`Pending` is now the written initial state.** Every node is set to `Pending` when
  `Execute` begins, so `GetNodeStatus` is total over the DAG (a never-reached node is
  observably `Pending` rather than absent from the map).
- **OpenTelemetry tracing — span per executed node.** Opt in with
  `WithTracerProvider(tp)` on `ExecutionConfig`, `DAG`, or `WorkflowBuilder`. Each executed
  node gets a span named after the node, a child of a parent `workflow.execute` span, with
  `node.status` / `node.retry_count` attributes and `RecordError` on failure. Skipped nodes
  get no span (a `workflow.skipped_count` parent attribute instead). API-only (the host
  owns the SDK), off by default (a nil provider resolves to a no-op tracer, zero overhead),
  and subject to the same no-leak discipline as `ExecutionError`.
- **Per-instance metrics isolation** — two `WorkflowData` instances never share metrics
  state (pinned by a discriminating regression test).
- **Mutation testing (go-gremlins) in CI** — a non-blocking, informational job scoped to
  the core executor/data files; see [docs/development/mutation_testing.md](docs/development/mutation_testing.md)
  for the accepted-survivor baseline and how to run it.

### Note (0.8.0-alpha — M8 Phase B)
- **`JSONFileStore` is un-deprecated** and is a first-class, supported store (it has
  out-of-tree consumers and its JSON is the better format for hand-recovery). Its
  `MigrateToFlatBuffers` path is now bounded (no unbounded `os.ReadFile`).

## [Unreleased]

Historical milestone record for M4–M8 (the detailed batch notes below predate the
`[0.8.0-alpha]` section above, which is the released summary of M8 Phase B). Version
lineage: M5 set `0.5.0-alpha`; the 0.6 line was never cut as a const; M7 close set
`0.7.0-alpha`; 0.7.1–0.7.3 patched the first-CI-run findings; `0.7.4-alpha` was the
M8 Tier-1 "polish & honesty" batch; **`0.8.0-alpha` is the M8 Tier-2 / Phase B batch**
(see `version.go` and the `[0.8.0-alpha]` section). Every tag is a pre-release, so
`go get @latest` resolves to the latest (`v0.8.0-alpha`). See [STABILITY.md](STABILITY.md).

### Changed (0.7.4-alpha — M8 Tier-1)
- **Metrics are now OFF by default + a metrics-free fast path on the data plane.**
  Observability is opt-in (matches the OTel "host owns it" philosophy); removes a
  measured ~3× per-op tax on `Set`/`Get`/`SetNodeStatus` when unused. Enable via the
  per-instance `MetricsConfig`.
- **Dropped string interning from the read path** — `Get*`/`Has*` no longer take the
  interner lock (Go maps key by value, so it bought nothing on reads); interning
  stays on the write path.
- **Symmetric bounded reads on the JSON load paths (closes M5-SEC-01).** `JSONFileStore.Load`
  and `WorkflowData.LoadFromJSON`/`LoadFromFlatBuffer` now read via
  `io.LimitReader(cap+1)` with a strict over-cap reject (`ErrCorruptData`) + an
  element-count cap — mirroring the FlatBuffers path, no `os.Stat` TOCTOU.
- **Coverage gate hardened** from a knife-edge `<90` to a hard-floor + per-package
  ratchet (catches silent erosion above the floor; absorbs jitter).
- **Explicit persistence trust contract** documented in STABILITY.md (per-store
  guarantees, caller trust boundary, honest availability ceiling).

### Fixed (0.7.4-alpha — M8 Tier-1)
- **Context cancellation now halts scheduling between levels** — a cancelled
  workflow no longer launches subsequent levels; `Execute` returns the ctx error.
- **`Workflow.Execute` no longer swallows `Store.Load` errors on resume** — a corrupt
  persisted state surfaces instead of silently starting fresh.

### Added (0.7.4-alpha — M8 Tier-1)
- Runnable godoc `Example_*` functions (build→Execute, string + typed `Key[T]` data,
  store round-trip) so pkg.go.dev shows compile-checked usage; `doc.go` rendering fixed.
- Test coverage for the FlatBuffers all-types Save→Load dispatch (previously only
  int64/string were exercised).

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

