# Verification, CI, documentation, and release audit

## 1. Verification portfolio

Flow Orchestrator’s assurance portfolio is unusually broad for an alpha Go library:

- ordinary and race package tests;
- adversarial tests tied to audit IDs;
- property tests and bounded fuzzing;
- mutation testing;
- focused coverage thresholds;
- deterministic performance/allocation ceilings;
- pinned lint and vulnerability tools;
- FlatBuffers generation checks;
- eight TLA+ capstones with recorded expected state counts;
- documentation parsing/complete-program tests;
- executable examples.

The breadth is real. The primary weakness is not missing test categories: several green categories prove less than their labels imply, while the final race/package gate is red because a safety-improving example cleanup did not refresh its checked-in writer-set oracle.

## 2. Local gate assessment

| Gate | Result on audited content | Assessment |
|---|---|---|
| `go build ./...` | Pass | Normal build is green. |
| `CGO_ENABLED=0 go build ./...` | Pass | Pure-Go host build is green. |
| `CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...` | Pass | Documented cross-build moat is green. |
| `go vet ./...` | Pass | No vet diagnostics. |
| pinned golangci-lint v2.12.2 | Pass, 0 issues | Prior lint-baseline mismatch is closed. |
| `go mod verify` + tidy-diff | Pass | Module checksums and manifest are consistent. |
| govulncheck v1.4.0 | Pass for reachable code | Zero reachable vulnerabilities; one imported package and two required modules have advisories outside reached symbols. |
| generated FlatBuffers diff | Pass | Regeneration does not change committed bindings. |
| Go formatting scan | Pass | No files reported by gofmt. |
| targeted prior-audit regressions | Pass | Named boundary, topology, nonce, nested-DAG, and compensation tests pass. |
| former flakes, 20 repetitions | Pass | Both named prior flakes stayed green across 20 runs. |
| determinism-tax ceiling | Pass | Non-race ceiling passes with `GOGC=800`. |
| `go test ./internal/doctest` | Pass | Green but insufficient: it accepts a stale exported signature in the architecture guide. |
| `scripts/formal/run_tlc.sh` | Pass, 8/8 | Pinned capstones match expected state counts. |
| `make examples` | Pass | The rebuilt capability-progression suite executed end to end. |
| `go test -race -timeout 30m ./...` | **Fail** | After 1,087.13 seconds, VB-09 rejected a stale golden that still expects six removed `examples/new_simple` writer sites. |
| targeted `TestVB09_TerminalWriterSetMatchesTheGolden` | **Fail** | Non-race reproduction fails on the same six expected removals. |

The exact full-race and targeted-failure evidence is in `00-evidence-and-method.md`.

## 3. CI assessment

### Strong controls

The normal CI workflow has meaningful blocking gates:

- Go floor and patched Go matrix arms;
- generated FlatBuffers bindings;
- normal, pure-Go, and Windows/ARM64 builds;
- vet and gofmt;
- full `-race` suite with a 30-minute ceiling;
- a separate non-race determinism-tax guard;
- pinned govulncheck and golangci-lint;
- focused coverage generation plus threshold enforcement;
- bounded FlatBuffers fuzzing;
- pinned TLA+ capstones;
- executable examples;
- scheduled, bounded, explicitly non-blocking mutation testing.

This is a strong design: heavy or noisy evidence is separated from deterministic blockers, and tools are mostly pinned.

### Example gate repaired during the audit

At `d570940`, the old `new_simple` failed sealed action mediation. Commits `f172c80` and `e215f47` replaced the suite with 13 numbered capability examples plus a capstone. Final `make examples` passed and exercised crash/resume, retries, branching, fan-out, saga, signals/approvals, sub-workflows, competing consumers, scheduling/caps, governance, observability, and the capstone.

CI now uses `set -euo pipefail` and a 300-second per-example timeout, closing the prior masked-mid-loop failure. Make and CI still maintain separate loops—Make clears known durable paths while CI relies on a fresh checkout—so consolidating them would reduce drift, but no current example-execution failure was reproduced.

### Blocking suite is stale after the example repair

The same cleanup deliberately removed six action-side status/output writers from `examples/new_simple`, but the checked-in VB-09 terminal-writer golden still requires those sites. The local command equivalent to branch CI’s blocking race step fails even though the underlying mediation change is safer. No remote `anvil-m1` run was available; that CI step is expected to fail on the same source **[INFERENCE]**. Refreshing the reviewed golden—not weakening the deriver or restoring the writers—is required before CI can be green.

### Release workflow is narrower than branch CI

The tag-triggered release workflow regenerates FlatBuffers, runs `go test -v ./...`, generates an SBOM, creates the release, and invokes SLSA provenance. The targeted package test fails on the audited source, so the encompassing release test step is expected to fail for the same reason **[INFERENCE]**. After that defect is fixed, the workflow still does not itself require the branch CI result. Its `go test ./...` includes the existing, insufficient `internal/doctest`; it does not add semantic signature/inventory documentation checks or repeat lint, race, formal, examples, coverage, version consistency, or ancestry checks.

Because the normal CI workflow triggers only for pushes to named branches and pull requests, a tag push can enter the release workflow without a mechanically bound green branch-gate artifact. SLSA accurately attests the built subject; it does not prove the tag matches source version or that the intended quality gates ran.

Remediation: make release depend on a reusable, SHA-bound gate workflow or repeat all release blockers at the tag SHA before creating a GitHub Release.

## 4. Documentation assessment

### Improvements since the prior audit

- durability and at-least-once semantics are candid;
- approval nonce is correctly scoped as correlation/freshness, not authorization;
- formal-method language is narrower and generally honest;
- platform/process trust boundaries are explicit;
- graph identity is described as structural digesting;
- store limits and operational queue APIs are extensively documented;
- version/toolchain instructions are more consistent.

### Remaining contradictions

1. `TopologicalSort` implementation and API reference now return `[]*Node`, but `docs/architecture/dag-execution.md` still publishes `[][]*Node` and the removed one-group behavior.
2. `CHANGELOG.md` says `WorkflowData.Clone` now deep-copies nested maps/slices, but an external mutation probe disproves that for nested slices reached through slices, typed maps, and pointers.
3. The product README at repository-relative `README.md:316`—not the audit bundle README—says “all three built-in stores” while four concrete store implementations exist; the text never defines whether SQLite is intentionally outside that category, leaving interchangeability scope ambiguous.
4. Broad thread-safety language still lacks one generated type/method matrix.
5. Many production comments preserve review-round history and proof narration rather than only current invariants, raising stale-claim cost.

### Doctest limitation

The doctest suite passes with the current stale architecture signature. Parser acceptance is not declaration equivalence. The next documentation gate should extract package-qualified declarations and compare them through `go/types`, or generate the reference directly.

## 5. Release lineage and identity

Observed repository points:

```text
current audited HEAD: e215f47a6d9841ac0cadbca1b9a0bf79551cf58e
v0.21.0-alpha tag:    4108150...
merge base:           bee7258...
```

Current source therefore does not descend from the published release commit. The development tree contains/replays the implementation lineage but not the release-only commit as an ancestor.

During development, keeping `workflow.Version = 0.21.0-alpha` while changes remain `[Unreleased]` is defensible. It becomes unsafe at tag time because no automated preflight compares tag, source version, `VersionInfo`, changelog section, and ancestry. Default closure requires the next candidate to descend from the prior published tag; an intentional replacement lineage requires a tracked policy decision and explicit accepted/superseded reclassification of `AUD-009`.

Remote workflow history also has no run for local `anvil-m1`; the visible latest public runs are the July 24 release/main runs. Local success is not remote release evidence.

## 6. Planning and process assessment

### Strengths

- decisions, residuals, and failed reviews are preserved rather than silently erased;
- many claims distinguish measured, source-proven, and model-bounded evidence;
- canonical state now explicitly overrides historical phase artifacts;
- remediation commits are narrowly named and regression-heavy.

### Weaknesses

- `.planning` is ignored but contains the claimed canonical release decision;
- 115,822 planning Markdown lines and roughly 154 MiB make independent review expensive;
- closure language conflates fixed, deferred, accepted, and superseded states;
- no public issue ledger represents remaining debt;
- audits continue against moving worktrees rather than frozen candidate commits;
- production code contains about 10,075 comment-only lines out of 24,796 lines, increasing maintenance surface.

## 7. Release verdict

**NO-GO for the next alpha tag from `e215f47`.**

The code-quality baseline is strong enough to justify targeted remediation rather than redesign. The release is blocked by measured and source-proven backend fidelity, public panic, clone isolation, a stale blocking test oracle, and release-identity gaps. Medium documentation and evidence-process debt should be fixed in the same freeze because each has already hidden or overstated a concrete defect.