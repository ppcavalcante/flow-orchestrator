# Verification, CI, and process audit

## 1. Verification portfolio

The project’s assurance investment is exceptional for an alpha:

- 1,230 test functions across `pkg`/`internal`;
- 70 benchmarks;
- 8 fuzz entry points;
- gopter properties against the real executor;
- tagged adversarial copy tests;
- subprocess and process-kill tests;
- store corruption/fidelity/differential suites;
- 19 TLA+ modules and 38 TLC configs;
- mutation/bite narratives and break configs.

This catches real failures. The project’s own history proves that instrument review is necessary: many green checks were vacuous, incomplete, misordered, or failed for the wrong reason.

## 2. Current gate status

| Gate | Current audit result | Assessment |
|---|---|---|
| Build | Pass | Good |
| Vet | Pass | Good for default corpus; tagged copy file intentionally fails copylocks |
| Gofmt/diff check | Pass | Good |
| Pure-Go/cross-build | Pass | Compile portability only |
| Targeted phase119/BAR | Pass | Does not establish full package gate |
| Tagged adversarial | Pass by reproducing known deadlock | Must not be read as absence of defect |
| Lint | **Fail: 22** | Current CI would fail |
| Govulncheck local | **Fail: 2 called stdlib vulns** | Patched release toolchain should differ |
| Full race final SHA | Not freshly run by this audit | Earlier samples exist; known flaky |
| TLC | Not run automatically | Existing manual evidence only |
| Remote M23 CI | None | Current branch unproven remotely |

## 3. CI defects

### V-01 — Coverage target is unprotected and CI invokes it

**High release-gate defect.**

`test-coverage-focused` runs `go test -race -coverprofile=...` without `-timeout`. Local current suite exceeds Go’s default 10-minute timeout. The CI coverage job runs `make test-coverage-focused` exactly.

The ordinary race job’s `-timeout 30m` does not protect the coverage job. The reviewer briefing’s claim that CI is safe is therefore incorrect.

A killed run exits non-zero, so Make should stop, but it may leave a partial profile and reassuring package/coverage lines. More importantly, the coverage gate cannot be assumed runnable on current code.

**Fix:** one Make variable used by every race invocation:

```make
GO_TEST_TIMEOUT ?= 30m
RACE_TEST = go test -race -timeout $(GO_TEST_TIMEOUT)
```

Use it in all targets and CI. Remove partial profiles before each run and only move a temp profile into place after test success.

### V-02 — Package gate is nondeterministic

Two distinct known flakes:

1. `NoDelayRetryMiddleware/Context_cancellation`: a 5ms deadline may expire before the first action call under load, so expected count 1 becomes 0.
2. `TestSQLiteDelta_WallClockIsON`: performance ratio assertions fluctuate under shared load.

A single green is a sample, not a stable commit property.

**Fix first flake:** synchronize cancellation after action entry or use a pre-cancelled context and expect zero invocations, depending the intended property.

**Fix second:** remove wall-clock ratios from normal correctness tests. Use benchmarks/benchstat or structural operation-count instrumentation. Its own comment is contradictory: it discusses a 2.0 ceiling while code uses 1.5 and claims a ~1.19 ratio breaches 1.5.

### V-03 — Mutation job has no workflow timeout

The informational job ran six hours and was cancelled, causing a `main` workflow to show cancelled despite blocking jobs succeeding.

**Fix:** schedule nightly/release, set `timeout-minutes`, shard scopes, upload survivor report, and keep its status separate from blocking CI.

### V-04 — Formal models are not continuously checked

No Make/CI target runs TLC. Specs are valuable but can rot relative to code and tools. The local default Java is a stub.

**Fix:** pin TLA tools jar checksum/version; add small-model PR checks and full scheduled capstones. Publish state counts and config list as artifacts.

### V-05 — Doctest parses most snippets rather than type-checking them

Complete programs compile/run; snippets only need one parser wrapping strategy to parse. The broken README example using `WithStore(...).Build()` is syntactically valid and passes.

**Fix:** classify more snippets into compilable external modules, or maintain type-checkable assembled examples. Generate API signatures from source.

### V-06 — Lint baseline is described inconsistently

Planning previously treated 16 findings as accepted baseline while security policy says zero. Current count is 22. A red gate that is knowingly ignored is not a gate.

**Fix:** either fix/suppress with reasons and enforce zero, or explicitly configure a checked baseline with no new findings. Zero is preferable at this size.

### V-07 — Coverage artifact is stale

Tracked `coverage-focused.txt` reports 90% from M21-era commit `bee7258`, not current M23. A tracked generated profile looks current by proximity.

**Fix:** do not track ephemeral coverage, or stamp commit/toolchain in a generated report and regenerate only during release.

## 4. Phase 119 assessment

Phase 119 is a valuable case study and a warning.

### What it achieved

- J1 refusal is public-API tested.
- J2 over-refusal was measured rather than shipped blindly.
- a bounded generator covers one/two-merge families and records its bound.
- VB-09 derives a current writer set and diffs a committed golden.
- an independent census reproduced the golden.
- declared blind spots were demonstrated, not merely documented.
- several anti-vacuity repairs were bite-tested.

### Why it is not closed

- `SUMMARY.md` says COMPLETE, but the last independent review and QA gates in artifacts are failures over older ranges.
- review R8 explicitly does not cover `047bb68`; current `c9aef0c` is later again.
- no final independent pass/QA pass covers the complete final range.
- lint is red on phase files.
- the J2 behavioral sweep discards `DAG.Execute` errors and still increments `runs`.
- package-wide evidence is known nondeterministic.
- during this audit, another uncommitted 196-line edit appeared in `boundary_vb07_test.go`, recording and repairing additional `119-F16` through `119-F19` instrument defects. That work is outside `c9aef0c`, has no final independent gate, and demonstrates that the subject was still moving.

### Process lesson

Nine review rounds finding successive blind cells does not mean the team should stop reviewing. It means the instrument architecture became too complex.

The phase test now contains generators, AST self-analysis, population ladders, family×verdict products, structural-empty exemptions, singular-site guards, control-flow warrant analysis, and large prose proofs. This is test software with its own proof obligations.

**Recommendation:** simplify properties to direct executable contracts where possible. For J2, consider a small independent model/oracle implementation with generated graph serialization and explicit successful-execution requirement, rather than continuing to make one Go test introspect its own source.

## 5. TLA+ assessment

### Strong points

- Models cover executor, durability, continuation, OR-join, saga, decomposition, fencing, queue, composition, schedules/caps, and fan-out.
- break configs/mutations establish that many invariants can fail.
- specs README states bounds and model/implementation separation honestly.
- liveness and anti-vacuity concerns are taken seriously.

### Limits

- TLC verifies model instances, not arbitrary Go implementation behavior.
- model-to-code faithfulness is manual.
- static level goroutine behavior is not modeled by action concurrency count.
- very large graphs and Go memory model behavior are outside scope.
- no CI prevents model rot.
- “formally verified engine” marketing exceeds this scope.

## 6. Planning/process assessment

### Strengths

- failed verdicts are preserved rather than overwritten;
- residuals and bounded claims are often explicit;
- independent reviewer disagreement is recorded;
- the team reads failure text, not only exit codes;
- shared-worktree hazards are recognized;
- evidence ownership and immutable-SHA intent are sophisticated.

### Problems

#### Planning is larger than the product

114,908 Markdown lines in `.planning` versus 24,271 production Go lines. `STATE.md` alone is 2,168 lines and contains multiple “authoritative” resume blocks.

#### Canonical state is contradictory

- frontmatter says phase 116, zero completed;
- body contains several superseding resume markers;
- current briefing says phase 119 fix tail;
- PROJECT says M21 is current;
- ROADMAP and phase artifacts have been repaired asynchronously;
- MILESTONES does not provide current full history despite being called authoritative in places.

#### Planning is gitignored

The project’s most important decisions, findings, and rationale are not shipped, reviewed on GitHub, or available to consumers. Code comments compensate by carrying process history, making production source an incident ledger.

#### No public issue traceability

GitHub has zero open issues while internal records contain numerous high findings. External contributors cannot discover accepted debt.

#### Shared worktree is still active

Documentation changed during this audit. Earlier phase reviews also ran against moving heads. Process guidance knows the risk but architecture still permits it.

### Recommended planning system

1. Active state under 200 lines.
2. One structured finding/decision ledger with stable IDs, status, owner, evidence SHA, and visibility.
3. Archive closed phase prose; do not append indefinitely.
4. Generate counts/tables from ledger.
5. One worktree per seat; merge through commits/PRs.
6. Public issues for consumer-relevant accepted debt.
7. Keep enduring invariants in source; move review chronology to audit artifacts.
8. Separate evidence artifacts from product requirements so failed instruments do not rewrite product semantics.

## 7. Recommended CI tiers

### Every PR

- build/vet/gofmt/lint;
- patched-toolchain govulncheck;
- normal unit/property suite with deterministic tests;
- race suite with explicit timeout;
- tagged adversarial script;
- external doctest compilation;
- API/version consistency;
- small TLC configs.

### Nightly

- bounded fuzzing for all fuzz functions;
- full TLA capstones;
- stress/multi-process suites;
- coverage with explicit timeout;
- flake repetition report.

### Release

- kill storms;
- mutation testing with timeout/sharding;
- all break configs;
- clean external-module probes;
- docs/link/version/API diff;
- SBOM/provenance/vulnerability report.
