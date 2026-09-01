# Findings register

## Legend

- **Critical** — fatal behavior reachable during otherwise valid operation, irrecoverable corruption of core workflow state, or violation of a core fencing/mediation safety property; blocks release. Loss of ancillary metadata with a fail-closed workaround is High.
- **High** — material correctness, availability, security, release-identity, or declared blocking-gate failure; normally blocks release. Invalid-input host panics are High unless reachable from a valid drive.
- **Medium** — important API, non-blocking verification, operability, documentation, or maintainability debt. A Medium may still block publication when the affected documentation/evidence is part of the release contract.
- **Low** — bounded inconsistency or cleanup.
- **Measured** — reproduced with an executable command/probe.
- **Source-proven** — established by direct control/data-flow or CI configuration inspection.
- **Repository-proven** — established by Git object, worktree, issue, or artifact state.

## Current findings

| ID | Severity | Evidence | Finding | Release disposition |
|---|---|---|---|---|
| CUR-001 | High | Measured + Source-proven | FlatBuffers signals discard `EnqueuedAt`, breaking the advertised backend-independent freshness contract | Block next tag |
| CUR-002 | High | Measured + Source-proven | Exported execution/store inputs can still panic on nil or typed-nil values | Block next tag |
| CUR-003 | High | Measured + Source-proven | `WorkflowData.Clone` still aliases nested slices, typed maps, and pointers while docs claim a deep clone | Block next tag unless the public contract is explicitly narrowed |
| CUR-005 | Medium | Measured + Source-proven | Doctest passes a stale architecture signature; the product README leaves the intended store category/interchangeability subset ambiguous | Block next tag until public docs match the API/store matrix |
| CUR-006 | High | Measured + Source-proven | Example cleanup removed six unmediated writer sites but left the blocking VB-09 golden expecting them; the targeted non-race test and full race suite fail | Block next tag until the expected removals are reviewed, the golden is refreshed, and the full race suite passes |
| CUR-007 | High | Source-proven + Repository-proven | Release workflow does not bind tag, source version, changelog, prior-release ancestry, and blocking-gate evidence to one candidate SHA | Block next tag until all identity/SHA-bound gate checks precede release and the candidate descends from the previous published tag, unless a tracked replacement-lineage decision explicitly reclassifies `AUD-009` |
| CUR-008 | Medium | Source-proven + Repository-proven | Canonical release state is ignored and uses non-strict closure labels | Block next tag until the minimal release ledger is tracked and the candidate is frozen; planning-size cleanup may remain accepted |

`CUR-004` is intentionally unused: the candidate finding about action-body semantic identity was reclassified after checking the documented structural-only `DefinitionDigest` contract. The stable identifier gap preserves audit traceability; the limitation is an accepted alpha residual, mapped from partial `AUD-010` below.

## Strict status of the 2026-08-10 register

| Status | Count | Meaning |
|---|---:|---|
| Fixed | 42 | Original defect/contract closed |
| Partial | 12 | Narrower mitigation landed; material residual remains |
| Open | 14 | Still observable or no closure evidence |
| Superseded | 3 | Replaced by a later decision/gate, not repaired |
| **Total** | **71** | Every prior ID was reviewed |

The prior open/partial items mapped to current release blockers are `AUD-001`, `AUD-009`, `AUD-013`, `AUD-025`, `AUD-031`, `AUD-039`, `AUD-048`, `AUD-054`, `AUD-055`, `AUD-057`, `AUD-059`, and `AUD-065`. Debt that can remain explicitly accepted at alpha includes `AUD-010`, `AUD-042`, `AUD-043`, `AUD-045`, `AUD-056`, `AUD-058`, `AUD-060`, `AUD-062`, `AUD-066`, `AUD-067`, and `AUD-071`.

### Disposition of every non-fixed prior item

| Prior IDs | Disposition |
|---|---|
| `AUD-025` | Release blocker subsumed by `CUR-001`. |
| `AUD-001`, `AUD-031` | Release blockers subsumed by `CUR-002`. |
| `AUD-013` | Release blocker subsumed by `CUR-003`. |
| `AUD-010` | Explicitly accepted alpha architectural risk: the documented structural digest omits action-body semantic identity; hosts own action-upgrade deployment/migration discipline. Reconsider an explicit action version before a stronger stability tier. |
| `AUD-039`, `AUD-048`, `AUD-054`, `AUD-065` | Documentation truth/freshness blockers subsumed by `CUR-005`. |
| `AUD-009` | Release identity blocker subsumed by `CUR-007`. |
| `AUD-055`, `AUD-057`, `AUD-059` | Minimal tracked-ledger/frozen-candidate blockers subsumed by `CUR-008`. |
| `AUD-018`, `AUD-035`, `AUD-038` | Explicitly accepted alpha residuals: no physical metadata split, no schedule-poller terminal observer, and no complete generated concurrency matrix. |
| `AUD-042`, `AUD-043`, `AUD-045`, `AUD-056`, `AUD-058`, `AUD-060`, `AUD-062`, `AUD-066`, `AUD-067`, `AUD-071` | Explicitly accepted alpha API/architecture/process debt; retain in the public debt ledger. |
| `AUD-007`, `AUD-061`, `AUD-064` | Superseded historical instrument/artifact findings; no current implementation action, but preserve their replacement decision. |

## Positive controls

The current tree passed or demonstrated the following material controls during this audit:

- project build and vet;
- pinned golangci-lint with zero findings;
- module checksum verification and tidy-diff check;
- govulncheck with zero reachable vulnerabilities;
- all eight pinned TLA+ capstones;
- determinism-tax performance ceiling;
- targeted prior-audit regression suite;
- repeated former-flake tests (20 runs);
- generated-code freshness and Go formatting checks;
- rebuilt end-to-end example suite.

The final full race suite failed after 1,087.13 seconds in `TestVB09_TerminalWriterSetMatchesTheGolden`; a targeted non-race run reproduced the same stale-golden failure. Exact command evidence is in `00-evidence-and-method.md`.