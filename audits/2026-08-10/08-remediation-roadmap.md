# Remediation roadmap

## Guiding principle

Do not add another feature until the project can answer, from one immutable SHA:

- what version this is;
- what the public contract is;
- what current CI actually proves;
- which findings block release;
- which graph definition a durable journal belongs to;
- which state actions are allowed to mutate.

## Stage 0 — freeze and establish one truth (1–3 days)

1. Freeze feature additions to M23.
2. Create an integration worktree/branch from current HEAD.
3. Stop shared-worktree commits; each owner gets a branch/worktree.
4. Build a short release-blocker issue list from `07-findings-register.md`.
5. Reconcile release ancestry:
   - merge/replay the 0.21 release metadata;
   - restore CHANGELOG 0.21;
   - decide 0.22 development version convention.
6. Replace active planning state with a generated concise page:
   - current SHA;
   - phase 119 fix tail;
   - last accepted gates;
   - current blockers;
   - next owner/action.
7. Mark phase 119 open until final independent review and QA pass.

**Exit:** one immutable integration SHA and no contradictory current-state marker.

## Stage 1 — repair the release gate (2–5 days)

### Deterministic tests

- Replace the 5ms middleware race with synchronized cancellation.
- Remove `TestSQLiteDelta_WallClockIsON` from ordinary correctness suite; retain benchmark/structural guard.
- Run repeated package samples after fixes to confirm no known flakes.

### Timeouts and profiles

- Create shared Make test variables.
- Apply explicit timeout to every race target.
- Write coverage to temp path; rename only on successful test completion.
- Delete stale tracked coverage profile or stamp it as release artifact.

### Static/security gates

- Fix 22 lint findings or add precise justified suppressions.
- Add `toolchain go1.25.11` or newer patched version.
- Keep Go 1.25.0 compatibility arm, but run release/govuln on patched toolchain.
- Update CONTRIBUTING and SECURITY.

### Long-running tiers

- add mutation `timeout-minutes` and move to nightly/release workflow;
- add small TLC CI and full scheduled TLC;
- keep tagged adversarial check blocking, but label output as “known defect reproduced” until fixed.

**Exit:** local and remote CI green on immutable SHA, with no single-run flake caveat.

## Stage 2 — close runtime safety blockers (1–2 weeks)

### Panic safety

- validate built-in action operands and typed nils;
- add external subprocess tests through `DAG.Execute`;
- choose panic policy;
- convert public configuration panics to errors.

### DAG representation

Replace copied mutex value with safe handle/core:

```go
type DAG struct { core *dagCore }
type dagCore struct {
    // immutable topology
}
```

Runtime config belongs elsewhere. Add a test that all legal value copies remain usable during concurrent drives.

### Static level worker pool

- add wide-level goroutine-count test;
- implement bounded worker queue;
- retain active-action MaxConcurrency property;
- rerun race/property tests;
- update model wording to distinguish action concurrency from goroutine count.

### Builder purity

- clone declared dependencies;
- fold generated edges into local definition state;
- test repeated Build equality for ordinary, choice, merge, boundary, fan-out, and FromBuilder paths.

### DAG-as-Action

- reject the type immediately;
- add wrapped/composite cases;
- longer-term rename compiled graph execution method.

**Exit:** no public input can panic/hang through known built-in/copy/nesting classes; wide levels are goroutine-bounded.

## Stage 3 — finish M23 honestly (1–2 weeks)

### Boundary semantics decision

Choose one:

1. **Precedence boundary:** V occurs before S. Remove J1 as semantically unnecessary.
2. **Successful verification gate (recommended):** V reached successful completion before S. Rename/docs/errors accordingly.
3. **Authorization gate:** defer public guarantee until M24 endorsement exists.

Do not call all three “boundary.”

### Repair phase-119 evidence

- assert every generated accepted DAG executes without error;
- rerun J2 corpus;
- retain explicit nested-choice bound;
- fix lint/gci/staticcheck in phase files;
- run independent code review on full final range;
- run QA after review on frozen SHA;
- update SUMMARY only by new superseding gate records.

### Definition identity

Persist graph digest and consumer definition version. Include boundary declaration identity. Add mismatch tests for:

- added/removed node;
- edge change;
- action version change;
- retry/timeout/CoE change;
- compensation change;
- boundary change;
- suspendability/dynamic primitive change.

### Seal ceiling

Commit runnable demonstrations for fan-out, child DAG, choice, and compensation scope. Public docs state the exact ceiling.

**Exit:** M23 contract is singular, graph-bound, independently verified, and publicly describable without M24 language.

## Stage 4 — M24 data-plane separation (2–4 weeks)

### Do not build a view over the current flat object without separating state

Required fields:

- consumer data;
- consumer outputs, if outputs are meant as user plane;
- engine statuses;
- waits/rollback/trigger cause;
- internal metadata/envelopes.

The phase-120 premises correctly discovered that `outputs` was missing from earlier door populations. Keep construction, alias, composite literal, transitive wrapper, and compensation paths in the analysis.

### Per-node view

Actions receive a `*WorkflowData`-compatible view only because current Action signature requires it. Internally, make the object a facade delegating to shared backing state with node policy. Eventually prefer an interface in a breaking pre-1.0 cut.

### Engine-private metadata

Move:

- current level;
- boundary envelope;
- fan-out expansion journal;
- timeout disposition;
- internal endorsement data;

out of consumer key namespace.

### Authorization policy

- only engine writes statuses;
- only authorized node writes endorsement;
- sink reads verified endorsement generation;
- resume carries endorsement durably;
- old signals cannot authorize a new generation;
- compensation policy is separately defined.

### Store fidelity

Adopt canonical `Value`. Provide migration for existing JSON-string fallback.

**Exit:** consumer actions cannot mutate engine journal or collide with internal keys; policy is enforceable at all forward and compensation doors.

## Stage 5 — dispatch and operability hardening (1–2 weeks)

1. Validate non-empty owner in Claim/locker paths.
2. Compose in-process and durable locks.
3. Make Claim context-aware.
4. Pool validates MP mode and distinct store instances at startup.
5. Classify runNext errors; stop on permanent config failures.
6. Add observer/health interface to Pool and SchedulePoller.
7. Surface consecutive DB errors and last-success timestamps.
8. Add Signal delivery time, generation/correlation, and approval freshness examples.
9. Publish platform support matrix; fix or restrict Windows file mailboxes.

**Exit:** a misconfigured worker cannot look healthy while doing nothing, and approval/signal freshness is implementable.

## Stage 6 — pre-1.0 API cut (2–4 weeks)

### Remove/fix

- legacy action callback;
- `WithAction(interface{})`;
- unexported types in public signatures;
- `TopologicalSort() [][]*Node`;
- no-op `WithCatchupOnce`;
- public configuration panics;
- unsupported exported infrastructure;
- possibly `/pkg/workflow` import path.

### Introduce

- immutable `CompiledDAG`;
- `Runner`;
- definition digest/version;
- consumer DataView;
- explicit operational observer;
- typed OwnerID;
- store capability/fidelity matrix;
- migration hooks.

### Thread-safety contract

Document each type and test race behavior under supported concurrent use.

**Exit:** surface is coherent enough to soak for 1.0 without freezing accidental shapes.

## Stage 7 — docs/release closure

1. Separate released and development docs.
2. Generate API reference.
3. Compile external examples/snippets.
4. Fix all known architecture drift.
5. Publish threat model, store fidelity matrix, platform matrix, panic policy, and graph migration policy.
6. Generate version surfaces from one source.
7. Run release suite against exact tag candidate.
8. Publish accepted residuals as issues/release notes.
9. Merge release commit back into development immediately.

## Proposed success metrics

Avoid hand-copied counts in planning. Generate these on each gate:

- lint findings = 0;
- current remote CI run linked by SHA;
- all race commands carry explicit timeout;
- no known flaky test under repeated samples;
- max goroutines for width N bounded by O(MaxConcurrency), not O(N);
- repeated Build produces identical graph digest;
- every store passes one fidelity/identity/status contract suite;
- metrics-enabled resume remains enabled on every store;
- invalid/empty owner is rejected everywhere;
- no consumer action can call engine journal mutators;
- docs version/API checks green;
- active planning state generated and short.

## Priority summary

| Priority | Outcome |
|---|---|
| P0 | Trustworthy gate and one release truth |
| P1 | No host panic/hang, no MP identity collapse, pure Build |
| P2 | Singular boundary contract and durable graph identity |
| P3 | Engine/user state separation and canonical values |
| P4 | Observable dispatch/scheduling and fresh approvals |
| P5 | Coherent pre-1.0 API and versioned docs |

The project should resist adding more orchestration primitives until P0–P3 are complete. Each new primitive currently multiplies proof obligations across statuses, stores, resume, compensation, boundaries, dynamic expansion, and planning instruments.
