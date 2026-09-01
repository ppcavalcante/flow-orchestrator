<!--
Canonical, TRACKED release finding-ledger (CUR-008).
Frozen candidate: 4904f6b (v0.22.4-alpha-5-g4904f6b), pkg/workflow.Version = 0.22.4-alpha.
This file POINTS to the tracked audit evidence under audits/; it deliberately does not
re-copy per-finding detail (a hand-copied canonical file rots — see the note at the end).
-->

# Release finding-ledger (canonical, tracked)

**Purpose.** This is the minimal, strict, **tracked** disposition of the project's release-audit
findings, reproducible from a clean clone. It exists because the canonical active-state ledger
(`.planning/STATE.md`) is **gitignored** — a clean clone never receives it, so public audit/release
readers could not reproduce closure assertions, and its closure labels conflated *fixed / partial /
deferred / accepted / superseded* under one word ("remediated"). That is finding **CUR-008**; this
file is its fix. The bulky planning archive (`.planning/`, `AUD-056`) remains out of scope and
explicitly accepted at alpha — only the minimal canonical ledger is brought into tracked content.

**Scope.** The two release-audit rounds:

- `audits/2026-08-10/` — the 71-finding audit, `AUD-001` … `AUD-071` (register: `07-findings-register.md`).
- `audits/2026-08-11/` — the release-readiness follow-up, `CUR-001` … `CUR-008`, **plus** a strict
  per-finding re-verification of all 71 priors at `e215f47` (`01-prior-finding-verification.md`,
  `04-findings-register.md`).

Both rounds are now tracked in this repository (this commit). Per-finding evidence lives in those
registers; this ledger is the strict roll-up and the record of what changed since `e215f47`.

**Frozen candidate.** `4904f6b` — `v0.22.4-alpha-5-g4904f6b` — `pkg/workflow.Version = 0.22.4-alpha`.
Accumulating toward `v0.23.0-alpha`. The release preflight (`scripts/release/preflight.sh`, wired
into `.github/workflows/release.yml` — `CUR-007`) binds tag ↔ source version ↔ changelog ↔ prior-tag
ancestry at tag time; this ledger is the finding-disposition half of the same "freeze the candidate"
requirement.

---

## Strict status of the 71-finding round (`AUD-001` … `AUD-071`)

The authoritative strict re-verification is tracked at
`audits/2026-08-11/01-prior-finding-verification.md`, taken at `e215f47`. Updated for HEAD `4904f6b`
with **every delta backed by evidence at HEAD, not by a summary claim** (per `DEC-M23-DISPOSITION-CONTROL`):

| Status | @ e215f47 | @ HEAD 4904f6b | Meaning |
|---|---:|---:|---|
| Fixed | 42 | **54** | Original defect / stated contract closed in code + tests |
| Partial | 12 | **5** | A narrower mitigation landed; a material residual remains (each accepted at alpha) |
| Open | 14 | **9** | Still observable, or no closure evidence |
| Superseded | 3 | 3 | Replaced by a later decision/gate, not repaired |
| **Total** | **71** | **71** | Every prior ID reviewed |

**Twelve priors moved to Fixed since `e215f47`** — two independently, seven because a release-blocking
`CUR` that subsumed them is now verified closed at HEAD (executed tests, see the crosswalk), and three
because *this* commit closes `CUR-008`:

| Prior | e215f47 | HEAD | Closed by — evidence |
|---|---|---|---|
| `AUD-009` | Open | **Fixed** | `main` linearly descends from `v0.21.0-alpha` **and** `v0.22.4-alpha`; `CUR-007` preflight now enforces prior-tag ancestry at every tag. |
| `AUD-067` | Open | **Fixed** | `WithCatchupOnce` removed in `b383781`; no symbol remains in `pkg/workflow`. |
| `AUD-025` | Partial | **Fixed** | `CUR-001` — FB `Signal.enqueued_at` round-trips (`TestAUD025_SignalCarriesEnqueuedAt/FlatBuffers` PASS). |
| `AUD-001` | Partial | **Fixed** | `CUR-002` — `DAG.Execute(ctx,nil)` returns `ErrValidation`, not panic (`TestAUD001_*`, `TestCUR002_*` PASS). |
| `AUD-031` | Partial | **Fixed** | `CUR-002` — typed-nil store rejected with `ErrValidation` (`TestAUD031_*` PASS). |
| `AUD-013` | Partial | **Fixed (narrowed)** | `CUR-003` — canonical value algebra deep-copied cycle-safe; non-canonical values documented as retained-by-reference (`TestAUD013_*`, `TestCUR003_*` PASS). |
| `AUD-039` | Partial | **Fixed** | `CUR-005` — `dag-execution.md` publishes `TopologicalSort() ([]*Node, error)`, matching code. |
| `AUD-048` | Partial | **Fixed** | `CUR-005` — doctest compiles the assembled samples green, incl. the API-drift test. |
| `AUD-054` | Partial | **Fixed** | `CUR-005` — README now names all four built-in stores. |
| `AUD-055` | Partial | **Fixed** | `CUR-008` (this commit) — the canonical release ledger is now tracked and strict. |
| `AUD-057` | Open | **Fixed** | `CUR-008` (this commit) — the audit evidence + this ledger are tracked, so a clean clone receives the release evidence. |
| `AUD-059` | Open | **Fixed** | `CUR-008` (this commit) — this ledger freezes to an immutable candidate SHA (`4904f6b`). |

**One improved, not fully closed:** `AUD-065` (Open → **Partial**) — `CUR-005` fixed the specific stale
public signature, but no repository-wide signature-validation *gate* was built; the gate is an
accepted-alpha residual.

The residual `Partial`/`Open` set is entirely accepted-alpha debt (enumerated below), **except `AUD-043`**
(the pending pre-1.0 USER decision). **Zero release blockers remain.**

---

## Release-blocker crosswalk — the `CUR` follow-up round, at HEAD

The 2026-08-11 round rolled the still-live priors into eight `CUR` blockers (`CUR-004` intentionally
unused — the action-body-identity candidate was reclassified as accepted `AUD-010` residual). Strict
disposition **at HEAD `4904f6b`** (release blockers were "block next tag" on 2026-08-11; the line has
since shipped `v0.22.0-alpha` … `v0.22.4-alpha`):

<!-- CUR dispositions verified at HEAD 4904f6b by executed tests (read-only re-verification). -->
| ID | Sev | Subsumes | Finding | HEAD disposition (evidence tier) |
|---|---|---|---|---|
| CUR-001 | High | `AUD-025` | FlatBuffers signals discard `EnqueuedAt` (backend-independent freshness) | **Fixed** (executed) — `Signal.enqueued_at:long` in the FB schema; `encodeSignalFB`/`decodeSignalFB` round-trip it; `TestCUR001_FlatBuffersCodecPreservesEnqueuedAt` + `TestAUD025_…/FlatBuffers` PASS. |
| CUR-002 | High | `AUD-001`, `AUD-031` | Exported execution/store inputs can panic on nil / typed-nil | **Fixed** (executed) — `DAG.Execute` and the store paths guard nil/typed-nil, returning `ErrValidation`; `TestCUR002_*`, `TestAUD001_*`, `TestAUD031_*` PASS (each asserts `NotPanics` + `ErrorIs ErrValidation`). |
| CUR-003 | High | `AUD-013` | `WorkflowData.Clone` aliases nested reference values vs a "deep clone" doc | **Fixed — narrowed** (executed + structural) — the reported nested-slice aliasing is deep-fixed cycle-safe for the canonical algebra; non-canonical values (typed maps/pointers/custom structs) are explicitly documented as retained-by-reference (the register's sanctioned "contract explicitly narrowed" closure). `TestCUR003_*`, `TestAUD013_*` PASS. |
| CUR-005 | Med | `AUD-039`, `AUD-048`, `AUD-054`, `AUD-065` | Stale architecture signature in doctest; ambiguous README store category | **Fixed** (executed) — `dag-execution.md` publishes `TopologicalSort() ([]*Node, error)`; README names all four stores; full `internal/doctest` suite PASS incl. the API-drift test. (`AUD-065`'s broader signature-*gate* remains accepted-alpha.) |
| CUR-006 | High | — | Example cleanup left the blocking VB-09 golden stale; race suite failed | **Fixed** (executed, decisive) — `go test -run TestVB09_TerminalWriterSetMatchesTheGolden -count=1` → `ok, 3.403s`, exit 0. |
| CUR-007 | High | `AUD-009` | Release identity not bound to one candidate SHA | **Fixed** (`4904f6b`) — preflight wired into `release.yml`, gating tag↔version↔changelog↔ancestry; verified it bites (exit 1) on a mismatched tag. |
| CUR-008 | Med | `AUD-055`, `AUD-057`, `AUD-059` | Canonical release ledger ignored / non-strict labels | **Fixed** (this commit) — both audit rounds tracked; this strict evidence-linked ledger added; candidate frozen to `4904f6b`. |

---

## Explicitly accepted alpha debt (retained in the public debt ledger)

Not release blockers; carried openly at alpha per the 2026-08-11 register:

- **API / architecture / process:** `AUD-042` (no small `Runner` facade), `AUD-043` (`pkg/workflow`
  import-path layout — **the one remaining pre-1.0 USER decision**, tracked separately below),
  `AUD-045` (load-dependent wall-clock test is opt-in), `AUD-066` (whole-level execution barriers),
  `AUD-071` (workspace hygiene — untracked launcher scripts now gitignored).
- **Structural residuals:** `AUD-010` (structural `DefinitionDigest` omits action-body semantics;
  hosts own action-upgrade migration discipline), `AUD-018` (no physical metadata/journal split),
  `AUD-035` (no `SchedulePoller` terminal-error observer), `AUD-038` (no complete generated
  concurrency matrix).
- **Planning-size / bookkeeping (the `.planning` tree, `AUD-056` scope — accepted, out of CUR-008's
  minimal-ledger scope):** `AUD-058`, `AUD-060`, `AUD-062`.

## Superseded (replaced by a later decision/gate, not repaired — decision preserved)

- `AUD-007`, `AUD-061`, `AUD-064` — historical phase-119 instrument / status artifacts; the M23
  closure used a different goal-backward verifier and preserved these records rather than rewriting
  history.

## Still open at HEAD

- `AUD-043` — the `pkg/workflow` import-path layout. The **last pre-1.0 USER-owned decision**; a
  module-path change is a breaking change for the real external consumer (`openai-workflow`), so it
  is being decided deliberately rather than executed silently. Tracked in the M24 backlog.
- The other 13 non-fixed priors (5 `Partial` + 8 remaining `Open`) are the accepted-alpha items
  enumerated above; none is a release blocker.

---

## How to reproduce this from a clean clone

1. `git clone` → `audits/2026-08-10/` and `audits/2026-08-11/` are present (tracked as of this commit).
2. `audits/2026-08-11/01-prior-finding-verification.md` gives the strict per-`AUD` status at `e215f47`.
3. `audits/2026-08-11/04-findings-register.md` gives the `CUR` crosswalk and the accepted/superseded
   dispositions.
4. This ledger records the deltas since `e215f47` and the freeze point.

## Honest limit of this file (and the real fix)

This ledger is **hand-written**, so — exactly as `.planning/STATE.md` warns of itself — it is a *copy*
of facts that live elsewhere and it will drift on the same mechanism that produced `AUD-055`. It
mitigates that two ways: it **points to** the tracked registers instead of re-copying per-finding
prose, and it freezes to a named SHA. The durable fix is a **derived** ledger — statuses generated
from typed finding/`gate_result` records, regenerated at release and diffed against the committed copy
so any drift reds. That cannot be built until those typed records exist for the full finding set
(today they largely do not — `AUD-062`); generating a canonical file over an incomplete record set
produces a confident wrong answer, strictly worse than pointing at the evidence. Until then, this
tracked, evidence-linked, frozen ledger is the minimal honest form `CUR-008` requires.
