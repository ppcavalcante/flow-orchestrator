# Formal specifications (TLA+)

Machine-checked models of flow-orchestrator's core algorithms — M7 Phase 22,
Layer 2 (the algorithm-design proof; Layer 1 is the gopter property suite in
`pkg/workflow/invariants_property_test.go`, which checks the Go *implementation*).

## `Executor.tla` — the DAG level-executor

Models `DAG.Execute` / `executeNodesInLevel` (`pkg/workflow/parallel_execution.go`):
node scheduling under a concurrency bound, dependency resolution including the
continue-on-error rule, and fail-fast halting.

**Properties checked (exhaustively, by TLC):**

| Property | Kind | Meaning |
|---|---|---|
| `ConcurrencyBound` | safety | never more than `MaxConc` nodes running at once |
| `DepsBeforeRun` | safety | a node runs only after every dependency is *resolved* (Completed, or a continue-on-error dep that Failed — `DEC-P21-depguard`) |
| `HardFailureHalts` | safety | any hard (non-coe) failure ⇒ scheduling halted ⇒ (with `Start` requiring `~halted`) no node starts after a hard failure: **failure-safety** |
| `SkippedSound` | safety | a node is `skipped` only if it has a terminal non-resolving dependency (a non-coe `failed` dep, or a `skipped` dep) — `DEC-CHUNK3-status` S1. A node whose deps all resolved, or an independent unreached node, is never skipped. |
| `Termination` | liveness | every behavior eventually reaches a **settled** fixed point (`<>[]Settled`) and stays there — no deadlock AND no refuse-to-schedule livelock. `Settled` = no node is `Stuck`, where Stuck = `running` (must Finish), or `pending` and eligible to start (deps resolved, `~halted` — a correct scheduler MUST start it), or `pending` with a skip-cause dep (must Skip). A legitimately-blocked pending node (halted/unresolved-deps AND no skip-cause — the independent-unreached case) is NOT Stuck and may rest, so the target is `Settled`, not all-nodes-terminal. Deliberately stronger than `<>[]nothing-enabled`, which a refuse-to-schedule bug satisfies vacuously. |

`MCExecutor.tla` is the model-checking harness: it supplies the concrete diamond
DAG `n1 → {n2,n3} → n4` (tuple syntax that TLC `.cfg` files can't express) and
forwards the remaining constants from the config.

## Scenarios

- **`Executor.cfg`** — continue-on-error CONTINUES: `n2` is continue-on-error and
  fails; `n4` still runs because the coe-failed dep is *resolved*. Exercises the
  coe-unblock arm of `DEC-P21-depguard`.
- **`ExecutorHardFail.cfg`** — hard failure HALTS: `n2` is a normal node and fails;
  `n4` (its dependent) never starts and is **skipped** (it has a non-resolving
  failed dep — the S1 `Skip` rule). Exercises `HardFailureHalts` non-vacuously +
  failure-safety + `SkippedSound`.
- **`ExecutorConc1.cfg`** — `MaxConc=1` (< the diamond's width 2). Makes the
  `ConcurrencyBound` check (and its mutation) non-vacuous; no failures.

## Running TLC

Requires Java (17+) and the official TLA+ tools jar:

```sh
curl -fsSL -o /tmp/tla2tools.jar \
  https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar

cd specs
java -cp /tmp/tla2tools.jar tlc2.TLC -config Executor.cfg          MCExecutor.tla
java -cp /tmp/tla2tools.jar tlc2.TLC -config ExecutorHardFail.cfg  MCExecutor.tla
java -cp /tmp/tla2tools.jar tlc2.TLC -config ExecutorConc1.cfg     MCExecutor.tla
```

**Verified result (all three configs):** `Model checking completed. No error has been
found.` — all five safety invariants (incl. `SkippedSound`) + `Termination`
(`<>[]Settled`) hold over the complete state space. (`tla2tools.jar` is intentionally
not vendored; fetch it as above.)

## Why this is not theater — and what it does NOT prove

A proof that cannot fail is worthless. Each invariant was **mutation-tested** —
the spec was deliberately broken and TLC confirmed to *catch* it:

| Mutation | Result |
|---|---|
| remove `Cardinality(Running) < MaxConc` from `Start` — **checked at `MaxConc=1` (`ExecutorConc1.cfg`)** | `ConcurrencyBound is violated` ✓ |
| remove `DepsResolved(n)` from `Start` | `DepsBeforeRun is violated` ✓ |
| hard failure no longer sets `halted` | `HardFailureHalts is violated` ✓ |
| revert `Skip` to S2 (drop the `HasSkipCauseDep(n)` guard — skip ANY pending node once halted) | `SkippedSound is violated` ✓ (an independent pending node with no failed/skipped dep gets wrongly skipped) |
| **stuck scheduler:** `Start` can never fire (a runnable pending node hangs forever) | `Termination`: `Temporal properties were violated` ✓ (with `-deadlock off`; with default deadlock-checking it surfaces as `Deadlock reached`). Proves the `<>[]Settled` liveness is NOT vacuous — a refuse-to-schedule executor is caught, not relabeled as "done". |
| **stalled node:** a `running` node can never `Finish` | `Termination` violated / `Deadlock reached` ✓ (a Stuck `running` node never settles) |

> Note: the `ConcurrencyBound` mutation is **vacuous when `MaxConc` ≥ the max level
> width** (the bound can't be exceeded, so TLC reports no error even with the guard
> removed). `ExecutorConc1.cfg` (MaxConc=1, diamond width 2) makes it bite — the
> guard-removed spec there gives `ConcurrencyBound is violated`. (Same vacuous-pass
> lesson as the Layer-1 overlap-window fix; surfaced by qa cross-review.) The other
> two mutations are structural and bite at any `MaxConc`.

**Honest scope — two complementary layers, neither sufficient alone:**

- **Layer 2 (this TLA+ model)** proves the **algorithm design/logic** is correct for
  *all* interleavings (exhaustive over the instance): the dependency-resolution rule
  (`DEC-P21-depguard`), the fail-fast halt logic, the scheduling discipline.
  **It assumes the scheduler enforces its guards** — it models "a node starts only
  when `Running < MaxConc`", it does **not** model the Go `chan` semaphore's
  acquire/release. So a *mechanism* bug (e.g. the real semaphore releasing early)
  is **out of scope for TLC** — only Layer 1 catches that. Faithfulness of model↔code
  is human-reviewed, not extracted.
- **Layer 1 (gopter suite, `pkg/workflow/invariants_property_test.go`)** runs the
  **real** `DAG.Execute` — actual semaphore, goroutines, map — over random DAGs, and
  is mutation-tested against the real code. Demonstrated: breaking the real semaphore
  (`make(chan struct{}, len(level))`) drove peak in-flight 8→60 and the
  concurrency property fired. It is *sampling* (random small DAGs), not exhaustive.

**Together:** design proved exhaustively (Layer 2) + implementation checked on the
real mechanism and shown falsifiable (Layer 1). **Not covered by either:** the Go
memory model beyond what `-race` samples; very large DAGs; and the model↔code
faithfulness step (reviewed, not mechanically guaranteed — that ceiling needs
deductive proof, which DEC-M7-verify deliberately did not take on).

## `DurableExecutor.tla` — M9 crash-resume (durable execution core)

Refines the base `Executor` model with a **durable result journal** and process
**crash / recover**, modelling what M9 (chunks 1–3) actually built in
`pkg/workflow`: a per-level checkpoint flushed at the level barrier
(`Workflow.Execute` wires the `Checkpointer` callback; `JSON`/`FB SaveCheckpoint`
write atomically), a crash that abandons in-memory state at any point, and resume =
re-running `Execute` with the same `WorkflowID`+`Store` (completed nodes skipped and
rehydrated, the not-done frontier — incl. any node in flight at the crash — re-run).

`Executor.tla` is left **byte-unchanged** (DESIGN-M9 §8 sanctions a separate
module); the base safety invariants are **retained and re-checked** under
crash/recover, proving durability does not break the scheduling discipline. New
variables: `journal` (durable persisted status), `exec` (per-node execution tally),
`up` (process alive), `crashes` (bounded by `MaxCrashes`). New actions: `Checkpoint`
(persist the snapshot at a quiescent `Running={}` barrier — a sound *superset* of
the code's per-level barriers, since a finer checkpoint only shrinks the resume
frontier, never changes the converged result), `Crash` (`up→FALSE`, in-memory
abandoned, at any point), `Recover` (`status := journal`, `halted` re-derived).

**Properties checked (exhaustively, by TLC):**

| Property | Kind | Meaning |
|---|---|---|
| `ConcurrencyBound`, `DepsBeforeRun`, `HardFailureHalts`, `SkippedSound` | safety | the base scheduling invariants, **re-verified under crash/recover** (durability preserves them) |
| `ExecFidelity` | safety | the **output arm of resume-equivalence**: a node reported `done`/`failed` must have *actually executed* (`exec[n] ≥ 1`). A crash+recover may change *which* run produced a result (the at-least-once frontier) but may never **fabricate** a result no run produced — exactly what a phantom checkpoint (recording a node done before it ran) would do. |
| `StatusConvergence` | safety | the **status arm of resume-equivalence** (defined in `MCDurableExecutor.tla`): every *settled* state reached with crashes is a status vector a NO-CRASH run could also reach (`Settled ⇒ ValidFinal(status)`). Crash/recover introduces **no new terminal state** — it cannot leave a node in a status the un-crashed run never produces. Asserted directly (not by composition) and bite-proven. NOT a single unique vector — see the within-level race note below. |
| `NoDoubleCommit` | safety | a node durably recorded `done` is never reverted or re-executed: `journal[n]="done" ⇒ status[n]="done"`, so it is skipped on resume (`Start` needs `pending`) and never runs again. (The at-least-once duplicate concerns nodes *not yet* journaled done.) |
| `Termination` | liveness | `<>[]Settled` **including after a crash**: a down process must `Recover` (weak fairness) and then drain. `Settled` requires `up`, so a behavior that crashes and never recovers is never settled. |

> **Within-level fail-fast race (why `StatusConvergence` is a SET membership, not an
> equality).** Writing the status arm first as `status = Expected` (a unique vector)
> surfaced a real subtlety — the value of bite-proving a "follows-by-composition"
> claim. Under a HARD failure of `n2`, its level-sibling `n3` has a genuinely
> NON-deterministic terminal status: `done` if `n3` finished before `n2`'s failure
> halted scheduling, or `pending` if the halt pre-empted `n3`'s start. This race is
> present in the base `Executor` model too (`Start` is per-node, gated on `~halted`;
> an independent pending node legitimately rests — `DEC-CHUNK3`) and is faithful to
> the executor abstraction. So the no-crash final is a *set* of valid vectors, and
> resume-equivalence is "crash adds no new final", i.e. `Settled ⇒ ValidFinal(status)`.
> When `n2` does not hard-fail (Clean / continue-on-error), `ValidFinal` pins every
> node to one status (the unique final).

### Durable scenarios

- **`DurableExecutorClean.cfg`** — no failures; a single crash at any point. The
  purest resume-equivalence case (crash → recover → converge to all-`done`); the
  phantom-completion mutation bites here most clearly.
- **`DurableExecutor.cfg`** — continue-on-error (`n2` coe-fails) + crash.
- **`DurableExecutorHardFail.cfg`** — hard failure (`n2` fails, halts) + crash;
  `halted` is re-derived from the reloaded journal so the failure stays observed
  and `n4` stays skipped across recover.

### Running TLC (durable)

```sh
cd specs
java -cp /tmp/tla2tools.jar tlc2.TLC -config DurableExecutorClean.cfg    MCDurableExecutor.tla
java -cp /tmp/tla2tools.jar tlc2.TLC -config DurableExecutor.cfg         MCDurableExecutor.tla
java -cp /tmp/tla2tools.jar tlc2.TLC -config DurableExecutorHardFail.cfg MCDurableExecutor.tla
```

**Verified result (`MaxCrashes=1`, single crash at every reachable point):**
`No error has been found.` — Clean 426 distinct states, coe 426, HardFail 318.

### Why this is not theater — durable mutation table

Each NEW property was mutation-tested; TLC confirmed it *catches* the deliberate break:

| Mutation | Result |
|---|---|
| `Recover` fabricates `done` for un-run (`journal=pending`) nodes — resume skips a node that never ran | `ExecFidelity is violated` ✓ (counterexample: Crash → Recover yields `status[n]="done"` with `exec[n]=0`; the no-crash run would have `exec ≥ 1`) |
| `Recover` resets all nodes to `pending` (drops the skip-completed restore) — a checkpointed-`done` node re-runs | `NoDoubleCommit is violated` ✓ (`journal[n]="done"` while `status[n]≠"done"` after recover) |
| `Recover` mis-derives `halted` (forces `TRUE`) — wrong-frontier recovery | `StatusConvergence is violated` ✓ (Clean config: Crash → Recover with `halted=TRUE` leaves un-run nodes resting `pending` forever — a terminal state no no-crash run reaches. `ExecFidelity`/`SkippedSound`/`HardFailureHalts` stay *silent*, so this isolates the status arm's teeth.) |
| drop `WF_vars(Recover)` — a crashed process may never recover | `Termination`: `Temporal properties were violated` ✓ (counterexample: Crash → `up=FALSE` → **Stuttering** forever, never `Settled`). Proves the durable liveness is not vacuous. |

> Redundant teeth (informative): the phantom-*journal* variant (`Checkpoint` records a
> `pending` node as `done`) also bites — caught by `NoDoubleCommit` at checkpoint time
> (`journal="done"` while `status="pending"`); and `Recover` sweeping an un-run node to
> `skipped` bites both `SkippedSound` (no skip-cause) and `StatusConvergence`. The four
> mutations above are chosen to ISOLATE each new invariant's independent teeth.

**Honest scope (same as Layer 2 above):** the durable model proves the crash-resume
*algorithm* — checkpoint/crash/recover logic, the at-least-once frontier, the
memoization (skip-completed) guarantee — for all interleavings with up to
`MaxCrashes` crashes at every reachable point. It assumes the model faithfully
mirrors the code's per-level flush (human-reviewed; the `Running={}` barrier is a
sound superset). It does **not** model the atomic file write itself (temp+fsync+
rename — that is the Go-side `TestWriteFileAtomic_*` suite) nor int64 serialization
fidelity (the Go-side `TestCheckpointFidelity_*` property). `MaxCrashes=1` checks
single-crash resume exhaustively; raising it checks crash-during-recovery.

**Context cancellation (DEC-CHUNK6) — Layer 1 only, deliberately not in the TLA+
model.** The model's universe is `Start`/`Finish`/`Skip` with no `ctx` and no
cancel action: cancellation is a *cooperative Go-context* concern (a node MAY or
MAY NOT observe cancel, depending on whether its action selects on `ctx.Done()`),
which TLA's atomic-action model represents poorly and which would not earn its
keep as a modeled transition. The cancellation contract — *cancellation always
wins (Execute returns the wrapped `ctx` error, never an `*ExecutionError`), no
`Cancelled` status, and no Skip sweep on the cancel path (unreached/downstream
stay `Pending`)* — is therefore verified by the **Layer-1 gopter property
`TestCancellationProperty` (`pkg/workflow/cancel_semantics_test.go`)** over the
real `DAG.Execute`, mutation-proven to bite (reverting cancel-wins, or re-running
the Skip sweep on cancel, both falsify it), plus example-based tests. It is **out
of scope for the TLA+ model by design**, not an omission.
