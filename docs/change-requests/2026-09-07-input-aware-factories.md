# Receipt A — input-aware registered DAG factories (E-01 completion)

**Re:** `openai-workflow/docs/plans/security-workflow-architecture/engine-input-aware-factory-handoff.md` §14.A
**Status: E-01 COMPLETE (rev 3).** The requested capability, the two prerequisite lifecycle corrections, and
the full §14.A acceptance set (A1–A10) landed on `main`, TDD, additive and backward-compatible. This is the
**A** receipt (E-01 completion); the two additional engine requests are delivered separately as **B1** (public
submission inspection/discovery) and **B2** (targeted host-local admission), each with its own receipt.

> **Rev 3 — §14 E-01 completion.** Following the §13 recheck (both reproduced defects CLOSED on `53cfb98`),
> the remaining §9 acceptance obligations now have DIRECT new-path witnesses:
> - **A1–A8** — eight new-path acceptance witnesses (`dispatch_input_aware_acceptance_test.go`, commit
>   `bfa1a92`). See §3 for the per-witness new-path-vs-shared mapping and §7 for the A1–A10 dispositions.
> - **A9** — the compatibility broadening is declared (below, and in `CHANGELOG.md` / `docs/guides/dispatch.md`).
> - **A10** — final evidence + commands + module version in §3/§6/§7.
> - The genuinely-impossible sub-case (a *parent-awaited* input-aware child reaching a **seedInput rejection**)
>   is called out with its precise structural reason in §7.A2 — not silently waived. This is same-model
>   self-review, not independent certification.

> **Rev 2 — independent-review response.** Your validation of candidate `b874471` (whose producer CI
> passed green: run `34133736256`) reproduced two real defects and flagged two witness-quality gaps.
> All fixed in `53cfb98`, each reproduced with a failing test first:

> **Rev 2 — independent-review response.** Your validation of candidate `b874471` (whose producer CI
> passed green: run `34133736256`) reproduced two real defects and flagged two witness-quality gaps.
> All fixed in `53cfb98`, each reproduced with a failing test first:
> 1. **Queued-child TYPE conflict now refused** — the C9 guard compared durable input but not durable
>    type; it now reads and enforces both (`queueChildTypeInput`). Test:
>    `TestInputAware_QueuedChildTypeConflict_Refused`.
> 2. **Failure-recording faults now surfaced** — `failClaimedItem` no longer discards the `MarkFailed`
>    error; a store rejection is joined with the cause (preserving its `ErrIO`/`ErrBusy` class), §5A.
>    Test: `TestInputAware_FactoryFailure_RecordingFaultSurfaced`.
> 3. **W2 fan-out-width witness added** (`TestInputAware_W2_FanOutWidthBound`) — distinct from W1's
>    concurrency; over-bound fails loud (`ErrFanOutMaxWidth`), never truncates.
> 4. **W1 reliability** — replaced the `Eventually(active==want)` poll (could time out on a loaded
>    runner: failed without `-race`, passed with) with a deterministic start-barrier; 30× clean.
>
> The two "additional application needs" (public E-read original-type/input lookup; targeted by-ID
> admission through caps/fencing) are correctly **outside** the E-01 request — the latter is an
> explicit engine non-goal (§8) — and remain consumer-owned follow-ons, not defects here.

## 1. Commit / version / public API

- **Final engine commit (E-01 complete):** `bfa1a92` on `main` (feature + fixes `53cfb98`; A1–A8 witnesses
  `bfa1a92`; built on the handoff's inspected baseline `defd443`).
- **Canonical module version:** `v0.22.4-alpha.0.20260907163418-bfa1a9258c38`.
- **Consume before the next tag:** `go get github.com/ppcavalcante/flow-orchestrator@bfa1a92`
  (or wait for the `v0.23.0-alpha` tag — this ships in it).
- **Final public API (matches the handoff's proposed shape):**

  ```go
  type InputDAGFactory func(input []byte) (*DAG, error)
  func (r *Registry) RegisterWithInput(typ string, factory InputDAGFactory) error
  ```

  The existing `type DAGFactory func() (*DAG, error)` and `Register` are unchanged; `Types`,
  `RunNext`, `Pool` keep their signatures.

## 2. Changed files

| Path | Work |
|---|---|
| `pkg/workflow/workflow_dispatch.go` | `InputDAGFactory` + `RegisterWithInput`; unified one-entry registry (`registryEntry.build`) with an input-ignoring adapter for `Register`; defensive input copy; `runNext` builds from `item.Input`; nil-DAG guard; `failClaimedItem` (terminalize+wake) for all construction/seed failures (BUG-1). |
| `pkg/workflow/subworkflow_queue.go` | queued-child built from its own input; durable-input authority + conflicting-redefinition refusal (C9); `ValidateNoTypeCycles` explicit input-aware refusal (C11). |
| `pkg/workflow/subworkflow_parked.go` | parked-await consults queue authority before a missing journal → resolves a terminal queue child without a journal (BUG-2 / C15). |
| `pkg/workflow/workflow_store_sqlite_workqueue.go` | `queueChildTypeInput` (private durable type+input read). |
| `pkg/workflow/*_test.go` (dispatch_input_aware, subworkflow_early_failure, +2 adapted) | witnesses + 2 tests adapted to the private registry representation. |
| `docs/guides/dispatch.md`, `CHANGELOG.md` | new-API docs + changelog. |

**Deviations from the contract (declared, A9).** One compatibility broadening beyond §5B:

> A parent re-drive attempting to reuse an existing deterministic child ID with a **different registered
> type** now refuses with `ErrValidation`, **including legacy-only registrations**. Ordinary same-type
> legacy replay remains supported and unchanged. Input-conflict enforcement remains input-aware-specific.
> This broadens the original handoff's compatibility scope; it is **not** unchanged behavior for
> different-type reuse of the same child ID.

This is the fix for reproduced defect E01-QA-1 (a type-only conflict was silently accepted). Witness:
`TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused` (same-type legacy replay parks unchanged;
different-type reuse refused). Otherwise no deviations: naming follows the handoff, the private
representation is one `registryEntry{build, wantsInput}` per type, input ownership is the recommended
defensive byte-copy on the input-aware path only, and no persisted-format migration is introduced.

## 3. Witness → test mapping (A10 — new-path vs shared regression; all real-SQLite dispatch)

Run: `GOTOOLCHAIN=local go test ./pkg/workflow/ -run '<name>' -count=1` (full suite:
`go test ./pkg/workflow`; race: `go test -race ./pkg/workflow`). **NEW-PATH** = a test that constructs and
drives through `RegisterWithInput`; **SHARED** = the mechanism is byte-identical to a legacy path and cited to
its existing green coverage. Every W1–W13 now has a **new-path** witness.

| Witness | Evidence | Test | Kind |
|---|---|---|---|
| W1 variable-graph concurrency | two payloads → distinct honored bounds via a deterministic start-barrier (bounded 150ms negative-observation window, stated honestly) | `TestInputAware_W1_VariableGraphConcurrency` | NEW-PATH |
| W2 input-selected fan-out width | at-bound runs every branch; over-bound fails loud (`ErrFanOutMaxWidth`), never truncates | `TestInputAware_W2_FanOutWidthBound` | NEW-PATH |
| W3 payload/seed identity + **nested decode + node reads seed** | byte-identity + mutation protection + durable-bytes unchanged (W3); constructor decodes nested JSON-string config and a **node reads the seeded >2^53 int after reload** (A1) | `TestInputAware_W3_PayloadSeedIdentity`, `TestInputAware_A1_W3_NestedDecodeNodeReadsSeed` | NEW-PATH |
| W4 legacy + mixed registry | legacy unchanged; cross-method dup refused; unregistered stays pending | `TestInputAware_W4_LegacyAndMixedRegistry` | NEW-PATH |
| W5 invalid/absent input + **early-fail/integrity** | constructor error (incl. wrapping `ErrIO`/`ErrBusy`) dead-lettered not requeued; nil DAG; malformed seed; absent-ok (W5). A2: input-aware child fails at worker → wake + parent resolves failure, zero child actions; done-without-journal → `ErrCorruptData` | `TestInputAware_W5_InvalidAndAbsentInput`, `TestInputAware_A2_W5_EarlyFailureAndIntegrity`, `TestQueueChild_*WithoutJournal*`, `_WorkerFactoryFailure_WakesParent` | NEW-PATH |
| W6 checkpoint reclaim + **settings/progressed-KV** | reclaim rebuilds from durable input, completed work not re-invoked, progressed journal survives (W6); A3 adds worker-defaults-differ + progressed **data KV** survives (not reseeded) + config honored on rebuild | `TestInputAware_W6_ReclaimRebuildsFromDurableInput`, `TestInputAware_A3_W6_SettingsAndProgressedDataSurviveReclaim` | NEW-PATH |
| W7 completion reconciliation | complete journal reclaimed **through `RegisterWithInput`** → re-execute is a no-op (counters flat), queue reaches done | `TestInputAware_A4_W7_CompletionReconciliation` | NEW-PATH (was shared in rev 2) |
| W8 construction isolation + **cap/config isolation** | a second claim progresses while a constructor is held at a barrier, `Registry.Types` called inside (W8); A5 adds two store handles, per-type cap 1, config isolation with no leakage, capped candidate stays pending | `TestInputAware_W8_ConstructionHoldsNoLockOrTxn`, `TestInputAware_A5_W8_ConcurrentConfigIsolationAndCap` | NEW-PATH |
| W9 stale owner / lease loss | reclaim through the input-aware path bumps the token; the stale owner's late failure-disposition is fenced while the row is still claimed | `TestInputAware_A6_W9_LeaseLossOnNewPath` | NEW-PATH (was shared in rev 2) |
| W10 queued-child correctness + **child parks/resumes** | input selects child policy; parent honors queue outcome; conflicting re-drive refused (W10). A7: the input-aware **child itself** parks on a wait-for-signal and resumes through the input-aware path, progress preserved, parent resolves from the queue outcome | `TestInputAware_W10_QueuedChildInputSelectsPolicy`, `TestInputAware_A7_W10_InputAwareChildParksResumes` | NEW-PATH |
| W11 control-plane separation | forged parent/signal/depth payload keys cannot change engine-derived identity/depth | `TestInputAware_W11_ControlPlaneSeparation` | NEW-PATH |
| W12 static-inspection honesty | legacy cycle still detected; input-aware registry gets the unsupported-inspection refusal without invoking an invented-input factory | `TestInputAware_W12_CycleHelperRefusesInputAware` | NEW-PATH |
| W13 cancel/drain/retry | A8: operator cancel before any action (constructor not reached); drain leaves committed progress claimed then reclaim resumes; constructor error wrapping `ErrBusy` stays terminal, not requeued | `TestInputAware_A8_W13_CancelDrainRetryOnNewPath` | NEW-PATH; genuine bare-infra retry budget = SHARED (`disposeExecErr` / `MarkForRetry` suites, registration-form-independent) |
| A9 legacy replay vs different-type refusal | same-type legacy replay parks unchanged; different-type reuse of the same child ID refused | `TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused` | NEW-PATH |

W1's negative concurrency-ceiling assertion uses a bounded 150ms observation window — it is not a timing-free
proof, only a strong bound; stated per the handoff's request.

## 4. Confirmations

- **Old factory compatibility:** yes — `Register`/`DAGFactory` and every existing `DAGFactory`
  example compile and behave unchanged (the input-ignoring adapter is `build(input) == factory()`).
- **No persisted-format migration:** yes — no schema/journal/queue/schedule change; `work_queue.input`
  already existed. Seeding fidelity/acceptance semantics unchanged.
- **Authoritative child input:** yes — a queued child is built from its own input; once its row
  exists the durable input is authoritative and a conflicting parent re-drive is refused, not
  silently reinterpreted. Control-plane columns (parent id/signal, depth) stay engine-set.
- **Static cycle-helper disposition:** `ValidateNoTypeCycles` returns an explicit `ErrValidation`
  unsupported-inspection error for a registry containing any input-aware factory (never a false
  cycle, never an invented-input call). Legacy-only registries are unchanged. The runtime depth
  ceiling (`ErrSubWorkflowMaxDepth`) still bounds every chain.

## 5. Known limitations (unchanged ceilings)

- **At-least-once execution, exactly-once persistence** across a crash/reclaim — a side-effecting
  node must be idempotent. Unchanged.
- **The structural `DefinitionDigest` does not promise semantic equality.** Passing the same input
  bytes across engine/factory versions does not prove closure behavior, fan-out width, or execution
  configuration are unchanged — the factory chooses the graph from the input, and a factory that
  reads changing environment/config can still drift. The engine cannot infer semantic equality from
  an arbitrary Go closure; the application owns effective-settings identity for reconstruction.
- **Version-skew (DEC-M17-VERSIONSKEW):** all live workers must share the factory version for a type;
  a drifted resume is dead-lettered, not mis-run.

## 6. Gate (A10 — final reproducible checks)

Commands run against the final subject `bfa1a92`:

```text
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^(TestInputAware_|TestQueueChild_|TestQueuedChild_|TestRunNext_ReconciliationSeam_)' -race -count=1 -timeout=180s
  → ok  github.com/ppcavalcante/flow-orchestrator/pkg/workflow  11.090s   (no race report)
GOTOOLCHAIN=local go vet ./pkg/workflow/                → clean
golangci-lint run pkg/workflow/                         → 0 issues on the new file
```

- Candidate `b874471` earlier cleared the full producer CI-amd64 gate green (run `34133736256`: `-race 30m`,
  coverage, govulncheck, formal models, lint, fuzz).
- The A1–A8 commit `bfa1a92` is test-only over unchanged production code; its authoritative amd64 gate runs on
  the push. **Bind any CI receipt to its exact SHA** — do not attribute a doc-only successor's run to a code
  commit. (The earlier `0f8ae60` Coverage re-run passed; its first failure was a pre-existing unrelated flake,
  `TestCancel_WinsOverGenuineFailure`, tracked separately — not part of E-01.)
- Assurance level: **same-model fresh-context self-review**, not independent certification.

## 7. A1–A10 dispositions

| Item | Disposition | Evidence |
|---|---|---|
| A1 (W3 end-to-end decode + node reads seed) | **DONE** | `TestInputAware_A1_W3_NestedDecodeNodeReadsSeed` |
| A2 (W5 early failure + missing-data integrity) | **DONE**, with one structural note below | `TestInputAware_A2_W5_EarlyFailureAndIntegrity` (+ `TestQueueChild_*`) |
| A3 (W6 settings + progressed KV survive reclaim) | **DONE** | `TestInputAware_A3_W6_SettingsAndProgressedDataSurviveReclaim` |
| A4 (W7 input-aware completion reconciliation) | **DONE** | `TestInputAware_A4_W7_CompletionReconciliation` |
| A5 (W8 concurrent config isolation + capped admission) | **DONE** | `TestInputAware_A5_W8_ConcurrentConfigIsolationAndCap` |
| A6 (W9 lease loss on the new path) | **DONE** | `TestInputAware_A6_W9_LeaseLossOnNewPath` |
| A7 (W10 the input-aware child parks/resumes) | **DONE** | `TestInputAware_A7_W10_InputAwareChildParksResumes` |
| A8 (W13 cancel/drain/bounded-retry) | **DONE** (bare-infra retry budget cited to shared coverage) | `TestInputAware_A8_W13_CancelDrainRetryOnNewPath` |
| A9 (declare the compatibility broadening) | **DONE** | this receipt §2 + `CHANGELOG.md` + `docs/guides/dispatch.md`; witness `TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused` |
| A10 (final mapping + commands + version) | **DONE** | §3 mapping, §6 commands, §1 version `v0.22.4-alpha.0.20260907163418-bfa1a9258c38` |

**A2 — the one structural limit (precise reason, not a waiver).** The exact single-child combination
"input-aware queued child whose *constructor accepts* the payload but whose *`seedInput` rejects* it, **and**
whose parent then converges on the child's failed queue outcome" is **not constructible**, for a structural
reason: `seedInput` rejects only a **non-object** JSON payload; a parent declares its child input as a JSON
**object** (`WithInput(map[string]any)`); and an input-aware child **enforces durable == declared input**, so a
parent can never present the non-object payload that a directly-enqueued seed-rejecting child would carry — the
re-drive would refuse on the input-conflict guard, not converge on the child's failure. The requirement is
therefore split across two witnesses that together cover its intent: the **seed-rejection** on the input-aware
path is proven at dispatch level by `TestInputAware_W5_InvalidAndAbsentInput` (`accept-any` constructor + a
`[1,2,3]` seed → terminal `failed`, no action, not retried); the **early-failure → parent-convergence** is proven
by `TestInputAware_A2_..._constructor_fails_at_worker_parent_resolves_failure`. No row silently became optional.
