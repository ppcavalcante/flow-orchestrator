---------------------- MODULE DurableExecutor ----------------------
(***************************************************************************)
(* Formal model of M9 crash-resume (Tier 1: durable execution core).       *)
(* Refines the base Executor model (specs/Executor.tla) with a durable      *)
(* result journal and process crash/recover, modelling what the code        *)
(* actually does (pkg/workflow, M9 chunks 1-3):                             *)
(*                                                                          *)
(*   - DAG.Execute runs nodes level-wise; at each completed LEVEL BARRIER    *)
(*     (wg.Wait, no node running) the Checkpointer flushes the whole         *)
(*     WorkflowData snapshot durably (workflow.go wires the per-level        *)
(*     checkpoint callback; JSON/FB SaveCheckpoint write atomically).        *)
(*   - A process Crash discards in-memory state at ANY point; only the last  *)
(*     checkpoint survives.                                                  *)
(*   - Recover = re-run Execute with the same WorkflowID+Store: it loads the *)
(*     last checkpoint, so nodes the journal records terminal are SKIPPED    *)
(*     (their outputs rehydrated) and every not-done node — including any    *)
(*     node in flight at the crash — RE-RUNS (the at-least-once contract,    *)
(*     DECISION 5).                                                          *)
(*                                                                          *)
(* This module leaves Executor.tla byte-unchanged (DESIGN-M9 §8 sanctions a  *)
(* separate module). The base scheduling helpers are mirrored here so the    *)
(* durable model is self-contained; the base safety invariants are RETAINED  *)
(* and re-checked under crash/recover, proving durability does not break the  *)
(* scheduling discipline. The NEW claims are ExecFidelity (the output arm of  *)
(* resume-equivalence) and NoDoubleCommit; both are mutation-proven (README). *)
(***************************************************************************)
EXTENDS FiniteSets, Naturals

CONSTANTS
    Nodes,           \* set of node ids
    Deps,            \* dependency relation: <<d, n>> in Deps  <=>  n depends on d
    ContinueOnError, \* subset of Nodes flagged continue-on-error
    FailSet,         \* subset of Nodes whose action fails when it runs
    MaxConc,         \* max concurrent running nodes (>= 1)
    MaxCrashes       \* max number of process crashes to explore (>= 0)

VARIABLES
    status,   \* status[n]: in-memory status in {pending,running,done,failed,skipped}
    halted,   \* TRUE once a hard (non-coe) node has failed (in memory)
    journal,  \* journal[n]: the DURABLE persisted status (the result journal)
    exec,     \* exec[n]: how many times n's action has actually executed
    up,       \* process alive? FALSE between Crash and Recover
    crashes   \* number of crashes so far (bounds the search)

vars == <<status, halted, journal, exec, up, crashes>>

MaxRuns == MaxCrashes + 1   \* a node runs at most once per (crash-reverted) attempt

------------------------------------------------------------------------
(* Base scheduling helpers — mirror Executor.tla (single source of truth   *)
(* for the NON-durable layer is Executor.tla; copied here so this module is  *)
(* self-contained and its diff is independent).                             *)

Terminal == {"done", "failed", "skipped"}

DepsOf(n) == { d \in Nodes : <<d, n>> \in Deps }

Running == { n \in Nodes : status[n] = "running" }

Resolved(d) == \/ status[d] = "done"
               \/ (d \in ContinueOnError /\ status[d] = "failed")

DepsResolved(n) == \A d \in DepsOf(n) : Resolved(d)

SkipCause(d) == \/ status[d] = "skipped"
                \/ (status[d] = "failed" /\ d \notin ContinueOnError)

HasSkipCauseDep(n) == \E d \in DepsOf(n) : SkipCause(d)

(* A hard (non-coe) failure exists in an arbitrary status map s. Used to     *)
(* re-derive `halted` on Recover from the reloaded journal.                  *)
HardFailedIn(s) == \E n \in Nodes : s[n] = "failed" /\ n \notin ContinueOnError

------------------------------------------------------------------------

TypeOK ==
    /\ status  \in [Nodes -> {"pending","running","done","failed","skipped"}]
    /\ halted  \in BOOLEAN
    /\ journal \in [Nodes -> {"pending","running","done","failed","skipped"}]
    /\ exec    \in [Nodes -> 0..MaxRuns]
    /\ up      \in BOOLEAN
    /\ crashes \in 0..MaxCrashes

Init ==
    /\ status  = [n \in Nodes |-> "pending"]
    /\ halted  = FALSE
    /\ journal = [n \in Nodes |-> "pending"]  \* nothing persisted yet = restart-from-scratch snapshot
    /\ exec    = [n \in Nodes |-> 0]
    /\ up      = TRUE
    /\ crashes = 0

------------------------------------------------------------------------
(* RUN actions (require the process to be up). Identical scheduling logic to  *)
(* Executor.tla, with exec[] bookkeeping so a real execution is observable.   *)

(* Start n: schedulable only when pending, not halted, deps resolved, and    *)
(* the concurrency bound permits one more. (A journaled-done node has status  *)
(* "done" after Recover, so this pending-guard is exactly the resume          *)
(* skip-completed guard — see NoDoubleCommit.)                                *)
Start(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ ~halted
    /\ DepsResolved(n)
    /\ Cardinality(Running) < MaxConc
    /\ status' = [status EXCEPT ![n] = "running"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes>>

(* Finish n: a running node's action executes (exec++) and terminates. If it  *)
(* is in FailSet it fails (halting iff hard); otherwise it completes.         *)
Finish(n) ==
    /\ up
    /\ status[n] = "running"
    /\ exec' = [exec EXCEPT ![n] = exec[n] + 1]
    /\ IF n \in FailSet
         THEN /\ status' = [status EXCEPT ![n] = "failed"]
              /\ halted' = (halted \/ (n \notin ContinueOnError))
         ELSE /\ status' = [status EXCEPT ![n] = "done"]
              /\ UNCHANGED halted
    /\ UNCHANGED <<journal, up, crashes>>

(* Skip n (DEC-CHUNK3-status, S1): a pending node with a terminal             *)
(* non-resolving dependency is skipped. A skip does NOT execute the action,   *)
(* so exec is unchanged.                                                      *)
Skip(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ HasSkipCauseDep(n)
    /\ status' = [status EXCEPT ![n] = "skipped"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes>>

------------------------------------------------------------------------
(* DURABILITY actions. *)

(* Checkpoint: persist the current in-memory snapshot to the durable journal. *)
(* Enabled only at a quiescent barrier (Running = {}) — the faithful analog of *)
(* the code flushing at the per-level wg.Wait. The set of Running={} states is *)
(* a SOUND SUPERSET of the code's per-level barriers: a finer checkpoint only  *)
(* shrinks the re-run frontier on resume, never changes the converged result, *)
(* so resume-equivalence over this superset implies it for the code. The       *)
(* journal # status guard avoids stuttering once the snapshot is current.       *)
Checkpoint ==
    /\ up
    /\ Running = {}
    /\ journal # status
    /\ journal' = status
    /\ UNCHANGED <<status, halted, exec, up, crashes>>

(* Crash: the process dies at an ARBITRARY point (no barrier required — a       *)
(* crash can catch nodes in flight). In-memory state is abandoned; only the    *)
(* journal persists. exec is the real-world execution tally and is NOT reverted *)
(* (a side effect that already happened stays happened — that is exactly why    *)
(* resume must be idempotent). Bounded by MaxCrashes to keep the search finite. *)
Crash ==
    /\ up
    /\ crashes < MaxCrashes
    /\ up' = FALSE
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<status, halted, journal, exec>>

(* Recover: restart and reload the last durable checkpoint (re-running         *)
(* Execute with the same WorkflowID+Store). In-memory status becomes the        *)
(* journal; halted is re-derived from the reloaded snapshot. Journaled-terminal *)
(* nodes are thus restored terminal and will be skipped (Start needs pending);  *)
(* everything else is pending and re-runs — the at-least-once frontier.         *)
Recover ==
    /\ ~up
    /\ up' = TRUE
    /\ status' = journal
    /\ halted' = HardFailedIn(journal)
    /\ UNCHANGED <<journal, exec, crashes>>

------------------------------------------------------------------------
(* Liveness scaffolding. *)

CanProgress(n) == Start(n) \/ Finish(n) \/ Skip(n)

(* Stuck(n): a state that DEMANDS progress (the teeth of Termination — NOT     *)
(* "~ENABLED", which a refuse-to-act bug satisfies vacuously). Mirrors          *)
(* Executor.tla's Stuck. Only meaningful while up; a down process is handled    *)
(* by Settled requiring up.                                                      *)
Stuck(n) ==
    \/ status[n] = "running"
    \/ (status[n] = "pending" /\ DepsResolved(n) /\ ~halted)
    \/ (status[n] = "pending" /\ HasSkipCauseDep(n))

(* Settled: the process is up AND no node is Stuck. A crashed (down) process    *)
(* is deliberately NOT settled — it must Recover first — so a behavior that      *)
(* crashes and never recovers FAILS <>[]Settled (caught by WF(Recover)).         *)
Settled == up /\ (\A n \in Nodes : ~Stuck(n))

Done == Settled /\ UNCHANGED vars

Next == \/ \E n \in Nodes : CanProgress(n)
        \/ Checkpoint
        \/ Crash
        \/ Recover
        \/ Done

(* Weak fairness on every node's run transitions AND on Recover: a down         *)
(* process must come back up, and runnable/finishable nodes must progress.       *)
(* Crash and Checkpoint are deliberately UNFAIR: crashes are adversarial and     *)
(* optional, checkpointing is optional (durability is best-effort progress, not  *)
(* a liveness obligation). Crash is additionally bounded by MaxCrashes.          *)
Fairness == /\ \A n \in Nodes : WF_vars(CanProgress(n))
            /\ WF_vars(Recover)

Spec == Init /\ [][Next]_vars /\ Fairness

------------------------------------------------------------------------
(* SAFETY — retained from Executor.tla (re-checked under crash/recover). *)

ConcurrencyBound == Cardinality(Running) =< MaxConc

DepsBeforeRun ==
    \A n \in Nodes :
        (status[n] \in {"running","done"}) => DepsResolved(n)

HardFailureHalts ==
    \A n \in Nodes :
        (status[n] = "failed" /\ n \notin ContinueOnError) => halted

SkippedSound ==
    \A n \in Nodes :
        (status[n] = "skipped") => HasSkipCauseDep(n)

------------------------------------------------------------------------
(* SAFETY — NEW durable claims (DESIGN-M9 §8). *)

(* ExecFidelity — the OUTPUT arm of resume-equivalence. A node reported         *)
(* terminal-by-execution (done or failed) must have ACTUALLY executed its       *)
(* action at least once. A crash+recover may change WHICH run produced a         *)
(* node's result (the at-least-once frontier), but it may never FABRICATE a      *)
(* result no run produced. This is precisely what a phantom checkpoint — one     *)
(* that records a node `done` before it ran — would break: on recover that node  *)
(* is restored `done` with exec = 0, this invariant fails, and TLC prints the    *)
(* divergence (the no-crash run would have executed it). The STATUS arm of        *)
(* resume-equivalence — every node converges to the same terminal status as a     *)
(* no-crash run — is established by the retained safety invariants above holding   *)
(* under crash/recover together with Settled => all-terminal for these DAGs.       *)
ExecFidelity ==
    \A n \in Nodes :
        (status[n] \in {"done","failed"}) => exec[n] >= 1

(* NoDoubleCommit — a node durably recorded `done` is never reverted or          *)
(* re-executed. Once journal[n] = "done", the node stays `done` in memory across  *)
(* crash+recover (Recover restores status := journal) and a `done` node cannot     *)
(* Start (Start requires `pending`), so it is skipped on resume and never runs     *)
(* again. A mutation that re-runs a checkpointed-done node (e.g. Recover resets it  *)
(* to pending, or Start drops its pending guard) moves status[n] off "done" and     *)
(* falsifies this. (The at-least-once duplicate concerns nodes that were NOT yet     *)
(* journaled done — running/done-but-unflushed — which this invariant deliberately   *)
(* does not constrain.)                                                              *)
NoDoubleCommit ==
    \A n \in Nodes :
        (journal[n] = "done") => (status[n] = "done")

------------------------------------------------------------------------
(* LIVENESS — every behavior eventually reaches a settled fixed point and        *)
(* stays there, INCLUDING after a crash: a down process must recover (WF) and     *)
(* then drain. Has teeth: dropping WF(Recover) lets a behavior crash and never     *)
(* recover, leaving the system never-Settled, and <>[]Settled FAILS. (Same          *)
(* anti-vacuity discipline as Executor.tla: Settled is "no node Stuck", not          *)
(* "nothing enabled".)                                                               *)
Termination == <>[]Settled

=============================================================================
