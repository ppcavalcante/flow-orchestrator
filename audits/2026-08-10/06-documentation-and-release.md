# Documentation and release audit

## 1. Release lineage

Published latest is `v0.21.0-alpha` at `4108150`. Current `anvil-m1` does not descend from that tag. The release is a separate commit above merge-base `bee7258`; the development line contains/replays implementation work but lost release-only metadata.

Current branch state includes:

- `pkg/workflow.Version = "0.20.0-alpha"`;
- CHANGELOG ending at 0.20;
- README correctly describing 0.21 latest and explaining development marker may lag;
- several getting-started docs still pinning 0.20;
- M23 source/docs targeting 0.22.

Allowing `Version` to lag intentionally does not solve the release-history problem. The 0.21 CHANGELOG entry is absent from the active line.

**Recommendation:** release directly from trunk or merge every release commit back immediately. Generate version surfaces from one manifest and fail CI on divergence.

## 2. Public documentation defects

### D-01 — README teaches an invalid durable construction path

Under “Fluent Builder API,” README does:

```go
builder := NewWorkflowBuilder().WithStore(store)
...
dag, _ := builder.Build()
err := dag.Execute(...)
```

`Build` deliberately rejects a store-configured builder and returns nil DAG. Correct durable path is `FromBuilder`.

This snippet parses, so doctest passes.

**Severity:** high onboarding failure.

### D-02 — Contributor toolchain is wrong

CONTRIBUTING says Go 1.24 “matches go.mod and CI.” Actual module floor is 1.25.0 and CI tests 1.25.0/1.25.11. The short contributing doc repeats 1.24.

It also tells contributors to run `go test ./... -race` with no timeout, which current local suite can exceed.

### D-03 — Security claims lint zero while lint reports 22

`SECURITY.md` says configured golangci-lint reports zero. Current clean-cache result is 22.

Security policy also says automated dependency scanning is roadmap work while CI already runs govulncheck. Update both directions.

### D-04 — Architecture docs show removed fields

Known stale references include `StartNodes`, `EndNodes`, and old Workflow/DAG field shapes. Current code removed these during sealing.

### D-05 — Getting-started versions are stale

Installation, quickstart, first workflow, and getting-started index still identify 0.20 as latest or pin 0.20 while README/security identify 0.21.

### D-06 — Store interchangeability is overstated

Docs often say backends are interchangeable while complex value/output types differ. “All three stores” is context-dependent: sometimes it means durable stores, sometimes the old pre-SQLite set, and sometimes excludes InMemory without saying so.

### D-07 — Thread-safety is overstated

README/docs claim broad thread safety. The actual contract has method-level caveats, aliases, callback deadlocks, config races, and platform limits.

### D-08 — “Formally verified” is too broad

Use “formally modeled/model-checked algorithms” and link the honest scope. TLA+ does not mechanically prove the Go source refines the model.

### D-09 — “A changed graph is rejected” is too broad

STABILITY says changed graph is rejected. Only persisted node names absent from current DAG are rejected today. Added nodes/edges/actions/boundaries are not.

### D-10 — Boundary semantics are internally inconsistent

Docs/code say exactly precedence/order; J1 says order alone is insufficient; BAR requires Completed; M24 plans endorsement provenance. Public docs need one progression and exact release scope.

### D-11 — Reserved engine keys are not inventoried

Consumers cannot know all engine-written keys. `__boundaries__` is unexported and can overwrite consumer data. Publish a reserved-key contract only as a short-term bridge; architectural fix is separate metadata.

### D-12 — WorkflowData Clone contract is false

Source comments say deep copy and InMemoryStore says external modification is prevented. Nested values are shared.

## 3. Documentation-system assessment

### Strengths

- docs are extensive;
- persistence threat/durability contract is unusually candid;
- STABILITY separates API and data compatibility;
- ADRs preserve decisions;
- doctest has anti-vacuity and executes complete programs;
- examples now run rather than only compile.

### Weaknesses

- many snippets are parse-only;
- API signatures are manually copied;
- docs are edited late in milestone;
- `.planning` contains corrections not reflected in shipped docs;
- source comments contain more current truth than architecture guides;
- version and release metadata are manually repeated;
- some prose is so long that contradictions appear within one comment block.

## 4. Production comments

Approximately 42% of production lines in `pkg/workflow` are comment-only. Many are valuable explanations of crash windows and invariants. Many also include:

- milestone/phase IDs;
- reviewer disputes;
- previous false statements;
- mutation transcripts;
- count histories;
- process instructions.

This creates three costs:

1. readers cannot identify the current contract quickly;
2. historical claims rot beside code;
3. code review must verify a large prose system on every edit.

**Rule:** source comments should answer “why must this code be this way now?” Historical review chronology belongs in ADR/audit/finding documents.

## 5. Documentation repair plan

### Immediate

- fix README WithStore example;
- update Go requirement and timeout commands;
- remove false lint-zero claim or make it true;
- reconcile 0.21/0.22 version surfaces;
- update removed architecture fields;
- document current boundary scope and status-forgery residual;
- document value-fidelity matrix and platform matrix.

### Automation

- generate API reference from `go doc`/source;
- compile examples in external modules;
- maintain one version manifest;
- check README/install/CHANGELOG/version consistency;
- check symbols mentioned in architecture docs;
- link-check all Markdown;
- generate store capability/fidelity matrix;
- generate supported-Go matrix from go.mod/CI.

### Versioned truth

Separate docs into:

- latest released (`v0.21`);
- current development (`main`/M23);
- migration notes.

A current-development API should not silently appear under latest-release installation instructions.

## 6. Release engineering

### What is good

- actions are SHA-pinned;
- least-privilege permissions are set;
- release provenance/SBOM jobs exist;
- patched Go release toolchain is pinned;
- release CI runs examples, race, fuzz, lint, coverage, and govulncheck on the old release tree.

### What must improve

- current branch must run remote CI before phase/release closure;
- mutation job needs timeout and separate workflow status;
- release and development history must converge;
- current coverage target must gain timeout;
- current lint must be zero;
- version consistency check promised in planning must be implemented;
- release should consume immutable final SHA evidence, not a moving shared tree;
- accepted high findings need public release-note disclosure or closure.

## 7. Recommended public positioning

A precise description would be:

> Flow Orchestrator is a pre-1.0 embedded Go library for static-DAG durable execution. It checkpoints per-node state at level barriers and resumes without replaying workflow code. Optional file and SQLite stores provide durability; SQLite additionally provides leases, fencing, dispatch, scheduling, and query APIs. Core algorithms have model-checked TLA+ specifications and implementation property tests. Actions remain at-least-once and must make external effects idempotent.

This remains compelling without overclaiming recursive sealing, exactly-once business effects, universal thread safety, or formal proof of the Go implementation.
