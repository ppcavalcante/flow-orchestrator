# Receipt A — input-aware registered DAG factories (E-01 completion)

**Re:** `openai-workflow/docs/plans/security-workflow-architecture/engine-input-aware-factory-handoff.md` §14.A
**Status: E-01 COMPLETE (rev 5).** The requested capability, the two prerequisite lifecycle corrections, and
the full §14.A acceptance set (A1–A10) landed on `main`, TDD, and additive — backward-compatible **with one
declared exception**: a parent re-drive that reuses an existing deterministic child ID with a *different*
registered type is now refused (`ErrValidation`), including legacy-only registrations (see §2 "Deviations" and
`## Input-aware factories` in the dispatch guide + CHANGELOG **Compatibility notes**). Ordinary same-type legacy
replay is unchanged. This is the **A** receipt (E-01 completion); the two additional engine requests are
delivered separately as **B1** (public submission inspection/discovery) and **B2** (targeted host-local
admission), each with its own receipt.

> **Rev 5 — §16 recheck response (final subject `ef51e67`).** The §16 recheck of `5496a20` confirmed R1 CLOSED
> and R2/R3/R5/A9 closed, but the exact-head CI was red on formatting + one flaky test, plus receipt-truth items.
> All fixed at `ef51e67`:
> - **gofmt** on `workflow_store_sqlite_eread.go` (the R3 comment reword left a list abutting a paragraph) — the
>   CI Test jobs stopped here before their race tests. Formatter run; `gofmt`/`golangci-lint` now clean.
> - **`TestQueueAdversarial_TwoWorkersReclaimParkedChild_ExactlyOneResumes`** (Coverage job) — a pre-existing
>   40ms real-clock flake that could not tell fencing from a legitimate sequential reclaim. Rewritten with a
>   frozen FakeClock so exactly one worker holds a live claim during the race (deterministic 20×/10×-race).
> - **A8 retry** — replaced the helper-call version with a REAL trigger-based checkpoint fault THROUGH `RunNext`
>   (attempts 1..2 requeue, 3 dead-letters, no 4th, at-least-once); corrected the misleading "only way" comment.
> - **A3** — honestly scoped (worker B's reclaim is real; worker A's frontier is staged, the standard technique).
> - Receipt truth: removed the duplicated "independent-review" block and the stale "impossible" claim, reconciled
>   the W13 mapping (all four sub-cases are real dispatch), qualified the compatibility headline with the declared
>   exception, and corrected B2's host-facing entry-point count.
>
> **Rev 4 — §15 recheck response.** The full-delivery recheck of `b4fba05` (§15) kept both defects closed and
> confirmed A1–A8 runtime behavior, but asked us to correct several witnesses that STAGED intermediate state
> rather than driving the real transition, and retracted one false claim. All addressed at `2246b97`:
> - **A2** — the earlier "not constructible" claim was **wrong** and is **retracted**: a valid JSON object with
>   a `json.Number("1e1000")` value is accepted by the constructor but rejected by float64 `seedInput`, so the
>   real "constructor accepts, seed rejects, parent converges on failure" case IS constructible and is now a
>   witness (`..._constructor_accepts_object_but_seed_rejects_float64_overflow`).
> - **A3** — now sets a **genuinely different worker default** and proves the durable input mode wins over it.
> - **A6** — now an **actual worker losing its lease during `RunNext`** (a construction barrier + a real reclaim
>   through `RunNext`), whose stale checkpoint is fenced (`ErrFencedOut`); no manual `ClaimNext`/`build`/`Execute`.
> - **A8** — a **real ctx drain** (cancel mid-`RunNext` → left claimed → reclaim resumes) and the **real bounded
>   infra-retry state machine** (exactly `maxAttempts` drives, then dead-letter), not a staged journal.
> - **A9** — the different-type refusal is now in the dispatch guide and the CHANGELOG (below), and the
>   legacy-replay witness is labeled LEGACY-PATH (§3). This is same-model self-review, not independent certification.
>
> **Rev 3 — §14 E-01 completion.** Following the §13 recheck (both reproduced defects CLOSED on `53cfb98`),
> the remaining §9 acceptance obligations now have DIRECT new-path witnesses:
> - **A1–A8** — eight new-path acceptance witnesses (`dispatch_input_aware_acceptance_test.go`, commit
>   `bfa1a92`). See §3 for the per-witness new-path-vs-shared mapping and §7 for the A1–A10 dispositions.
> - **A9** — the compatibility broadening is declared (below, and in `CHANGELOG.md` / `docs/guides/dispatch.md`).
> - **A10** — final evidence + commands + module version in §3/§6/§7.
> - The A2 "seed rejection" sub-case is now a real witness (see the Rev-4 note above and §7.A2); the earlier
>   "not constructible" claim is retracted. This is same-model self-review, not independent certification.

> **Rev 2 — recheck response** (the consumer's own same-model recheck, not third-party certification). Your
> validation of candidate `b874471` (whose producer CI passed green: run `34133736256`) reproduced two real
> defects and flagged two witness-quality gaps. All fixed in `53cfb98`, each reproduced with a failing test first:
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

- **Final engine commit (E-01 complete, after the §16 recheck):** `ef51e67` on `main` (feature + fixes
  `53cfb98`; A1–A8 witnesses `bfa1a92`; §15 R4 real-transition corrections `2246b97`; §16 fixes — gofmt, the
  deterministic parked-child reclaim test, and the real trigger-based A8 retry — `ef51e67`; built on the
  handoff's inspected baseline `defd443`).
- **Canonical module version:** `v0.22.4-alpha.0.20260908053020-ef51e67df4db`.
- **Consume before the next tag:** `go get github.com/ppcavalcante/flow-orchestrator@ef51e67`
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
| W5 invalid/absent input + **early-fail/integrity** | constructor error (incl. wrapping `ErrIO`/`ErrBusy`) dead-lettered not requeued; nil DAG; malformed seed; absent-ok (W5). A2: input-aware child fails at worker → wake + parent resolves failure, zero child actions; **the real "constructor accepts object, float64 seed rejects" case → durable failure + parent convergence**; done-without-journal → `ErrCorruptData` | `TestInputAware_W5_InvalidAndAbsentInput`, `TestInputAware_A2_W5_EarlyFailureAndIntegrity` (3 sub-cases), `TestQueueChild_*WithoutJournal*`, `_WorkerFactoryFailure_WakesParent` | NEW-PATH |
| W6 checkpoint reclaim + **settings/progressed-KV** | reclaim rebuilds from durable input, completed work not re-invoked, progressed journal survives (W6); A3 adds worker-defaults-differ + progressed **data KV** survives (not reseeded) + config honored on rebuild | `TestInputAware_W6_ReclaimRebuildsFromDurableInput`, `TestInputAware_A3_W6_SettingsAndProgressedDataSurviveReclaim` | NEW-PATH |
| W7 completion reconciliation | complete journal reclaimed **through `RegisterWithInput`** → re-execute is a no-op (counters flat), queue reaches done | `TestInputAware_A4_W7_CompletionReconciliation` | NEW-PATH (was shared in rev 2) |
| W8 construction isolation + **cap/config isolation** | a second claim progresses while a constructor is held at a barrier, `Registry.Types` called inside (W8); A5 adds two store handles, per-type cap 1, config isolation with no leakage, capped candidate stays pending | `TestInputAware_W8_ConstructionHoldsNoLockOrTxn`, `TestInputAware_A5_W8_ConcurrentConfigIsolationAndCap` | NEW-PATH |
| W9 stale owner / lease loss | reclaim through the input-aware path bumps the token; the stale owner's late failure-disposition is fenced while the row is still claimed | `TestInputAware_A6_W9_LeaseLossOnNewPath` | NEW-PATH (was shared in rev 2) |
| W10 queued-child correctness + **child parks/resumes** | input selects child policy; parent honors queue outcome; conflicting re-drive refused (W10). A7: the input-aware **child itself** parks on a wait-for-signal and resumes through the input-aware path, progress preserved, parent resolves from the queue outcome | `TestInputAware_W10_QueuedChildInputSelectsPolicy`, `TestInputAware_A7_W10_InputAwareChildParksResumes` | NEW-PATH |
| W11 control-plane separation | forged parent/signal/depth payload keys cannot change engine-derived identity/depth | `TestInputAware_W11_ControlPlaneSeparation` | NEW-PATH |
| W12 static-inspection honesty | legacy cycle still detected; input-aware registry gets the unsupported-inspection refusal without invoking an invented-input factory | `TestInputAware_W12_CycleHelperRefusesInputAware` | NEW-PATH |
| W13 cancel/drain/retry | A8: operator cancel before any action (constructor not reached); a REAL ctx drain leaves committed progress claimed then reclaim resumes; the REAL infra-retry budget THROUGH `RunNext` (a SQLite trigger faults every node checkpoint → attempts 1..2 requeue, attempt 3 dead-letters, no 4th, at-least-once); constructor error wrapping `ErrBusy` stays terminal, not requeued | `TestInputAware_A8_W13_CancelDrainRetryOnNewPath` (4 sub-cases) | NEW-PATH (all four sub-cases drive real dispatch) |
| A9 legacy replay vs different-type refusal | same-type legacy replay parks unchanged; different-type reuse of the same child ID refused | `TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused` | LEGACY-PATH (a legacy-registration behavior test, not the input-aware path) |

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

Commands run against the final subject `ef51e67`:

```text
GOTOOLCHAIN=local go test ./pkg/workflow/ -run '^(TestInputAware_|TestB2Security_|TestHostLocal_|TestERead_|TestQueueChild_|TestQueuedChild_|TestRunNext|TestClaimNext|TestPool|TestCancel|TestCaps|TestAdversarial)' -race -count=1 -timeout=600s
  → ok  github.com/ppcavalcante/flow-orchestrator/pkg/workflow  36.287s   (no race report)
GOTOOLCHAIN=local go test ./pkg/workflow/ -count=1 -timeout=1800s          → ok (full suite; needs ≥900s — the suite exceeds 600s)
GOTOOLCHAIN=local go vet ./pkg/workflow/                                    → clean
golangci-lint run pkg/workflow/                                            → 0 issues on the new/changed files
GOTOOLCHAIN=local go test ./internal/doctest/                              → ok (fenced-Go + live-corpus doc guards)
```

- The `ef51e67` change adds the §16 fixes (gofmt, the deterministic parked-child reclaim test, the real
  trigger-based A8 retry) over the E-01 production code; the authoritative amd64 gate runs on the push. **Bind any CI receipt to its exact SHA** — do not attribute a
  doc-only successor's run to a code commit. (The earlier `0f8ae60` Coverage re-run passed; its first failure was
  a pre-existing unrelated flake, `TestCancel_WinsOverGenuineFailure`, tracked separately — not part of E-01.)
- Assurance level: **same-model fresh-context self-review**, not independent certification (any earlier
  "independent-review" phrasing in this receipt refers to the consumer's own same-model recheck, not third-party
  certification).

## 7. A1–A10 dispositions

| Item | Disposition | Evidence |
|---|---|---|
| A1 (W3 end-to-end decode + node reads seed) | **DONE** | `TestInputAware_A1_W3_NestedDecodeNodeReadsSeed` |
| A2 (W5 early failure + missing-data integrity) | **DONE** — §15 R4: added the real float64-seed-reject case; impossibility claim retracted (below) | `TestInputAware_A2_W5_EarlyFailureAndIntegrity` (3 sub-cases) (+ `TestQueueChild_*`) |
| A3 (W6 settings + progressed KV survive reclaim) | **DONE** — genuinely different worker default overridden by the durable input; worker B's reclaim+resume is REAL, worker A's committed frontier is honestly STAGED (§16 action 3; real drain/lease-loss are A6/A8) | `TestInputAware_A3_W6_SettingsAndProgressedDataSurviveReclaim` |
| A4 (W7 input-aware completion reconciliation) | **DONE** | `TestInputAware_A4_W7_CompletionReconciliation` |
| A5 (W8 concurrent config isolation + capped admission) | **DONE** | `TestInputAware_A5_W8_ConcurrentConfigIsolationAndCap` |
| A6 (W9 lease loss on the new path) | **DONE** — §15 R4: an actual worker losing its lease during `RunNext` (barrier + real reclaim); the stale checkpoint is fenced (`ErrFencedOut`) | `TestInputAware_A6_W9_LeaseLossOnNewPath` |
| A7 (W10 the input-aware child parks/resumes) | **DONE** | `TestInputAware_A7_W10_InputAwareChildParksResumes` |
| A8 (W13 cancel/drain/bounded-retry) | **DONE** — real ctx drain + the real infra-retry budget THROUGH `RunNext` (SQLite checkpoint-fault trigger: exactly `maxAttempts` drives, no 4th; §16 action 3) | `TestInputAware_A8_W13_CancelDrainRetryOnNewPath` (4 sub-cases) |
| A9 (declare the compatibility broadening) | **DONE** — now in the dispatch guide (`## Input-aware factories`) + `CHANGELOG.md` **Compatibility notes**, not only this receipt; witness labeled LEGACY-PATH | witness `TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused` |
| A10 (final mapping + commands + version) | **DONE** | §3 mapping, §6 commands, §1 version `v0.22.4-alpha.0.20260908053020-ef51e67df4db` |

**A2 — retraction of the earlier "not constructible" claim (§15 R4).** An earlier revision wrongly argued this
combination was structurally impossible on the reasoning that `seedInput` rejects only a *non-object* payload.
That is wrong: a valid JSON **object** can still fail float64 seeding. `WithInput(map[string]any{"huge":
json.Number("1e1000")})` marshals to a valid object the input-aware constructor accepts (it decodes into a struct
that ignores the overflowing key), but the worker's `seedInput` — which unmarshals into
`map[string]interface{}` (float64) — rejects `1e1000`. The witness
`TestInputAware_A2_..._constructor_accepts_object_but_seed_rejects_float64_overflow` drives exactly that: zero
child actions, no child journal, durable terminal failure, completion notification, and the parent converging on
failure. The impossibility claim is **retracted**; no row is optional.
