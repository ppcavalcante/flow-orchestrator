# Flow Orchestrator — current-state audit

**Audit date:** 2026-08-11  
**Code subject:** `e215f47a6d9841ac0cadbca1b9a0bf79551cf58e` on `anvil-m1`  
**Published release:** `v0.21.0-alpha` at `4108150`  
**Decision:** **NO-GO for the next alpha tag from this tree**

**Snapshot note:** code conclusions evaluate committed `e215f47`. Planning/worktree measurements and GitHub state are separate 2026-08-11 observations recorded in `00-evidence-and-method.md`. The audit re-anchored after `d570940`, `f172c80`, and `e215f47` landed, then reran affected gates; later uncommitted audit files are outside the code subject.

## 1. Answer to “are all prior fixes done?”

No, not under a strict defect-closure definition.

Every prior finding was checked. Of the 71 entries from the 2026-08-10 audit:

| Status | Count |
|---|---:|
| Fixed | 42 |
| Partial | 12 |
| Open | 14 |
| Superseded | 3 |

The remediation program is substantial and closed many of the highest-risk runtime defects. The `.planning/STATE.md` statement that all 71 were “remediated” uses a broader meaning that includes accepted, deferred, and superseded work. It should not be read as “all 71 defects are gone.” The complete row-by-row result is in `01-prior-finding-verification.md`.

## 2. Current state

The implementation is materially stronger than the 2026-08-10 snapshot:

- built-in nil/typed-nil actions are rejected at Build;
- DAG value-copy deadlocks and `*DAG` action bypasses are closed;
- forward execution and fan-out creation are goroutine-bounded;
- consumer actions receive sealed per-node views;
- structural definition digests protect declared graph-definition changes; action-body semantics remain deliberately out of scope and require host deployment/migration discipline;
- store identity, enum decoding, and value fidelity are much more uniform;
- SQLite queue/fencing, observability, and operational surfaces remain strong;
- lint, build, vet, module integrity, vulnerability, property/adversarial, and TLA+ controls are broad for an alpha; the race gate is broad but currently red on a stale checked-in writer-set oracle.

This is still not a release-complete tree. Five High findings remain: `CUR-001` FlatBuffers freshness loss, `CUR-002` public nil panics, `CUR-003` shallow Clone behavior, `CUR-006` a stale mediation-writer golden that breaks the blocking test/race gate, and `CUR-007` unbound release identity.

Two Medium findings also block publication truth: `CUR-005` a stale architecture signature plus ambiguous product-README store categorization that current doctest does not fully police, and `CUR-008` an ignored, non-strict canonical release ledger. For `CUR-008`, tracking the minimal strict ledger and freezing the candidate are blocking; reducing the full planning archive is separate accepted cleanup. The complete register follows.

## 3. Fresh findings

| ID | Severity | Summary |
|---|---|---|
| CUR-001 | High | FlatBuffers signals discard `EnqueuedAt` |
| CUR-002 | High | Exported nil/typed-nil inputs still panic |
| CUR-003 | High | `WorkflowData.Clone` still aliases mutable subgraphs |
| CUR-005 | Medium | Doctest passes a stale architecture signature; product README leaves store categorization ambiguous |
| CUR-006 | High | Example cleanup left the blocking mediation-writer golden stale |
| CUR-007 | High | Release tag/version/changelog/ancestry/gate evidence are not bound |
| CUR-008 | Medium | Canonical release evidence is ignored and uses non-strict closure labels |

`CUR-004` is intentionally unused. The action-semantic-identity candidate was reclassified as an accepted structural-digest limitation after checking the documented contract; identifiers were not renumbered so the audit trail remains stable.

Detailed evidence, impact, and fixes are in `02-current-technical-findings.md`.

## 4. Verification verdict

The normal code-quality substrate remains strong: build, vet, pinned lint, module integrity, generated-code freshness, Go formatting, govulncheck, targeted regressions, former-flake stress, the determinism-tax guard, the rebuilt example suite, and all eight formal capstones passed on the audited content. The final full race suite ran for 1,087.13 seconds and failed in `TestVB09_TerminalWriterSetMatchesTheGolden`; a targeted non-race reproduction failed the same way. The golden still expects six unmediated writer sites deliberately removed with `examples/new_simple`.

That stale oracle is a release-blocking verification defect even though the underlying removal is a safety improvement. The measured external probes and remaining source/repository findings independently prevent a green release verdict.

## 5. Bundle map

1. `00-evidence-and-method.md` — snapshot, commands, limitations, and observed outputs.
2. `01-prior-finding-verification.md` — strict status for all 71 prior findings.
3. `02-current-technical-findings.md` — present architecture, seven current findings, and bounded architectural residuals.
4. `03-verification-release-and-docs.md` — gates, CI, documentation, release lineage, and process assessment.
5. `04-findings-register.md` — prioritized register.
6. `05-remediation-roadmap.md` — release-ordered closure plan.

## 6. Minimum next-tag conditions

Before the next alpha tag:

- preserve `EnqueuedAt` through FlatBuffers and test all four stores;
- make ordinary nil/typed-nil misuse return typed errors;
- make the Clone contract true or explicitly narrow it;
- repair stale API docs and make signature drift machine-detectable;
- review the six expected writer-site removals, refresh the VB-09 golden, and rerun the complete race suite;
- enforce tag/source/changelog consistency, make the candidate descend from the prior published tag (or explicitly reclassify `AUD-009` through a tracked replacement-lineage decision), and bind every blocking-gate result to the same immutable candidate SHA before release creation;
- publish a tracked strict finding ledger rather than relying on ignored `.planning` state.