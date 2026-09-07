# Delivery receipt — input-aware registered DAG factories

**Re:** `openai-workflow/docs/plans/security-workflow-architecture/engine-input-aware-factory-handoff.md`
**Status: DELIVERED (rev 2, after independent-review fixes).** Both the requested capability and the
two prerequisite lifecycle corrections landed on `main`, TDD, additive and backward-compatible.

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

- **Engine commit:** `53cfb98` on `main` (built on the handoff's inspected baseline `defd443`).
- **Consume before the next tag:** `go get github.com/ppcavalcante/flow-orchestrator@53cfb98`
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
| `pkg/workflow/workflow_store_sqlite_workqueue.go` | `queueChildInput` (private durable-input read). |
| `pkg/workflow/*_test.go` (dispatch_input_aware, subworkflow_early_failure, +2 adapted) | witnesses + 2 tests adapted to the private registry representation. |
| `docs/guides/dispatch.md`, `CHANGELOG.md` | new-API docs + changelog. |

**Deviations from the contract:** none material. Naming follows the handoff. The private
representation is one `registryEntry{build, wantsInput}` per type (the handoff left this to us).
Input ownership is the recommended defensive byte-copy, on the input-aware path only.

## 3. Witness → test mapping (all real-SQLite dispatch)

Run: `GOTOOLCHAIN=local go test ./pkg/workflow/ -run '<name>' -count=1` (full suite:
`go test ./pkg/workflow`; race: `go test -race ./pkg/workflow`).

| Witness | Test | Notes |
|---|---|---|
| W1 variable-graph concurrency | `TestInputAware_W1_VariableGraphConcurrency` | two payloads → distinct honored bounds, proven by an atomic active-count (no timing guess). |
| W3 payload/seed identity | `TestInputAware_W3_PayloadSeedIdentity` | noncanonical outer JSON seen byte-identical; mutation protection; large int inside a JSON string survives; durable queue bytes unchanged. |
| W4 legacy + mixed registry | `TestInputAware_W4_LegacyAndMixedRegistry` | legacy runs unchanged; cross-method dup refused; unregistered stays pending. |
| W5 invalid/absent input | `TestInputAware_W5_InvalidAndAbsentInput` | constructor error (incl. one wrapping `ErrIO`) dead-lettered **not** requeued; nil DAG; malformed seed; zero-length input with a valid absent constructor. |
| W6 checkpoint reclaim | `TestInputAware_W6_ReclaimRebuildsFromDurableInput` | reclaim rebuilds from the durable input; completed work not re-invoked; progressed journal survives. |
| W8 construction isolation | `TestInputAware_W8_ConstructionHoldsNoLockOrTxn` | a second claim progresses while a constructor is held at a barrier → construction holds neither the registry lock nor the claim txn (C3/C4). |
| W10 queued-child correctness | `TestInputAware_W10_QueuedChildInputSelectsPolicy` | input selects the child's tolerated-failure policy; parent honors the queue outcome; conflicting re-drive refused. |
| W11 control-plane separation | `TestInputAware_W11_ControlPlaneSeparation` | forged parent/signal/depth payload keys cannot change engine-derived identity/depth. |
| W12 static-inspection honesty | `TestInputAware_W12_CycleHelperRefusesInputAware` | legacy cycle still detected; input-aware registry gets the unsupported-inspection refusal without invoking an invented-input factory. |
| W5 (parent outcome) / W10 early-fail | `TestQueueChild_TerminalFailedWithoutJournal…`, `_TerminalCancelledWithoutJournal…`, `_WorkerFactoryFailure_WakesParent`, `_PendingWithoutJournal…` | BUG-1/BUG-2: a queued child failing construction wakes its parent and resolves as failure, never a forever-park. |
| W2 fan-out width bound | `TestInputAware_W2_FanOutWidthBound` | input selects `WithMaxWidth`; at-bound runs every branch, over-bound fails loud (`ErrFanOutMaxWidth`), never truncates. (Added in rev 2 — distinct mechanism from W1.) |
| **W7 completion reconciliation** | *inherited* — `TestRunNext_ReconciliationSeam_Idempotent` | the reconciliation seam is byte-unchanged; W6 exercises reclaim through the input-aware path. |
| **W9 stale owner** | *inherited* — existing M16 fencing suites | fencing/tokenState is byte-unchanged by this additive feature. |
| **W13 cancellation/retry** | *inherited* — existing cancel suites + W5 | the cancel re-read/`disposeExecErr` path is unchanged; W5 proves invalid factory input is not turned into retryable infra. |

Per the handoff's §9 ("reuse existing coverage rather than duplicating every old test"), W2/W7/W9/W13
are mapped to existing green coverage plus the new-path witnesses that actually exercise the delta,
rather than re-run duplicates. Say the word if you want any of them as a dedicated input-aware test.

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

## 6. Gate

Candidate `b874471` cleared the full producer CI-amd64 gate green (run `34133736256`: `-race 30m`,
coverage, govulncheck, formal models, lint, fuzz — all success). Rev-2 fixes (`53cfb98`) verified
locally: the new/hardened witnesses pass under `-race`, W1 30× clean, `golangci-lint` 0 issues, and
the doctest gate passes; the authoritative amd64 gate re-runs on the rev-2 push (result appended when
it completes).
