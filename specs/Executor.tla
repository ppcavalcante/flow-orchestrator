-------------------------- MODULE Executor --------------------------
(***************************************************************************)
(* Formal model of the flow-orchestrator DAG level-executor (M7 Phase 22, *)
(* Layer 2). Models the algorithm in pkg/workflow:                         *)
(*   - DAG.Execute runs nodes level-wise; within scheduling, at most       *)
(*     MaxConc nodes run concurrently (the per-level semaphore, and        *)
(*     because levels are wg.Wait-sequenced the global peak = max per-level *)
(*     peak, so a single global bound faithfully models the observable     *)
(*     concurrency).                                                        *)
(*   - A node may start only when every dependency is RESOLVED: Completed, *)
(*     OR (ContinueOnError dependency that Failed)  [DEC-P21-depguard].     *)
(*   - A hard (non-continue-on-error) node failure HALTS scheduling        *)
(*     (fail-fast): no further node starts. A continue-on-error failure    *)
(*     does not halt.                                                       *)
(*                                                                          *)
(* TLC checks, over a small concrete DAG instance (see Executor.cfg):       *)
(*   SAFETY:                                                                 *)
(*     ConcurrencyBound  - never more than MaxConc nodes running            *)
(*     DepsBeforeRun     - a node that ran had all deps resolved first      *)
(*     HardFailureHalts  - any hard failure implies scheduling is halted    *)
(*                         (=> with Start requiring ~halted, no node starts *)
(*                          after a hard failure: failure-safety)           *)
(*   LIVENESS:                                                              *)
(*     Termination       - every node eventually reaches a terminal state   *)
(*                         (no deadlock; the scheduler always makes progress)*)
(***************************************************************************)
EXTENDS FiniteSets, Naturals

CONSTANTS
    Nodes,          \* set of node ids
    Deps,           \* dependency relation: <<d, n>> in Deps  <=>  n depends on d
    ContinueOnError,\* subset of Nodes flagged continue-on-error
    FailSet,        \* subset of Nodes whose action fails when it runs
    MaxConc         \* max concurrent running nodes (>= 1)

VARIABLES
    status,         \* status[n] in {"pending","running","done","failed","skipped"}
    halted          \* TRUE once a hard (non-coe) node has failed

vars == <<status, halted>>

Terminal == {"done", "failed", "skipped"}

AllTerminal == \A n \in Nodes : status[n] \in Terminal

DepsOf(n) == { d \in Nodes : <<d, n>> \in Deps }

Running == { n \in Nodes : status[n] = "running" }

(* A dependency d of n is resolved iff it completed, or it is a            *)
(* continue-on-error node that failed (DEC-P21-depguard).                  *)
Resolved(d) == \/ status[d] = "done"
               \/ (d \in ContinueOnError /\ status[d] = "failed")

DepsResolved(n) == \A d \in DepsOf(n) : Resolved(d)

(* A dependency d is a terminal NON-RESOLVING skip-cause for its dependents *)
(* (DEC-CHUNK3-status, S1): a Failed dep that is NOT continue-on-error (the *)
(* coe case is resolved above), or a dep that is itself Skipped. A          *)
(* pending/running dep is non-resolving but not terminal, so not a cause.   *)
SkipCause(d) == \/ status[d] = "skipped"
                \/ (status[d] = "failed" /\ d \notin ContinueOnError)

(* n has at least one dependency that is a terminal skip-cause.             *)
HasSkipCauseDep(n) == \E d \in DepsOf(n) : SkipCause(d)

TypeOK ==
    /\ status \in [Nodes -> {"pending","running","done","failed","skipped"}]
    /\ halted \in BOOLEAN

Init ==
    /\ status = [n \in Nodes |-> "pending"]
    /\ halted = FALSE

(* Start n: schedulable only when pending, not halted, deps resolved, and  *)
(* the concurrency bound permits one more.                                  *)
Start(n) ==
    /\ status[n] = "pending"
    /\ ~halted
    /\ DepsResolved(n)
    /\ Cardinality(Running) < MaxConc
    /\ status' = [status EXCEPT ![n] = "running"]
    /\ UNCHANGED halted

(* Finish n: a running node terminates. If it is in FailSet it fails (and   *)
(* halts scheduling iff it is a hard node); otherwise it completes.         *)
Finish(n) ==
    /\ status[n] = "running"
    /\ IF n \in FailSet
         THEN /\ status' = [status EXCEPT ![n] = "failed"]
              /\ halted' = (halted \/ (n \notin ContinueOnError))
         ELSE /\ status' = [status EXCEPT ![n] = "done"]
              /\ UNCHANGED halted

(* Skip n (DEC-CHUNK3-status, S1 — NARROW): a pending node becomes          *)
(* "skipped" iff it has a dependency in a terminal non-resolving state (a    *)
(* non-coe Failed dep, or an already-Skipped dep). This is transitive: a     *)
(* node skipped here makes HasSkipCauseDep true for its own dependents.      *)
(* Independent pending nodes (no skip-cause dep) are NOT skipped — once the   *)
(* run has stopped they simply remain "pending" (mirrors Execute returning   *)
(* while an unrelated, unreached node is left Pending, not Skipped).         *)
(* Skip does not gate on `halted`: a transitive skip can also occur in a      *)
(* continue-on-error run where an upstream became Skipped without a hard      *)
(* halt; the guard is the skip-cause dependency itself.                       *)
Skip(n) ==
    /\ status[n] = "pending"
    /\ HasSkipCauseDep(n)
    /\ status' = [status EXCEPT ![n] = "skipped"]
    /\ UNCHANGED halted

(* A node can still make progress iff some transition is enabled for it.     *)
CanProgress(n) == Start(n) \/ Finish(n) \/ Skip(n)

(* Stuck(n): n is in a state that DEMANDS further progress and must not be    *)
(* allowed to rest there. This is the teeth of the liveness property — it is  *)
(* deliberately NOT "~ENABLED CanProgress" (which a refuse-to-schedule bug    *)
(* would satisfy vacuously). A node is Stuck iff:                             *)
(*   - it is "running" (it must eventually Finish); or                        *)
(*   - it is "pending" AND eligible to start (deps resolved, not halted) —    *)
(*     a correct scheduler MUST start it; or                                   *)
(*   - it is "pending" AND has a skip-cause dep — it MUST be Skipped.         *)
(* A pending node that is NOT Stuck is one that genuinely cannot proceed:     *)
(* halted (or deps unresolved) AND no skip-cause dep — the independent-       *)
(* unreached case that legitimately rests in "pending" (DEC-CHUNK3-status).   *)
Stuck(n) ==
    \/ status[n] = "running"
    \/ (status[n] = "pending" /\ DepsResolved(n) /\ ~halted)
    \/ (status[n] = "pending" /\ HasSkipCauseDep(n))

(* Settled: no node is Stuck — every node is either terminal or a legitimately *)
(* blocked pending node. This is the genuine rest condition.                   *)
Settled == \A n \in Nodes : ~Stuck(n)

(* Once settled the executor has drained: stutter so completion is not        *)
(* mistaken for deadlock and the liveness property below is meaningful. The    *)
(* stutter is gated on Settled (NOT on "nothing enabled"), so a scheduler that *)
(* refuses to start an eligible node is NOT permitted to stutter — it leaves a  *)
(* Stuck node and the liveness property below catches it.                      *)
Done == Settled /\ UNCHANGED vars

Next == (\E n \in Nodes : CanProgress(n)) \/ Done

(* Weak fairness on every node's transitions guarantees progress: any node  *)
(* continuously able to start/finish/skip eventually does, so the system    *)
(* cannot stall with work remaining.                                        *)
Fairness == \A n \in Nodes : WF_vars(CanProgress(n))

Spec == Init /\ [][Next]_vars /\ Fairness

------------------------------------------------------------------------
(* INVARIANTS (safety) *)

ConcurrencyBound == Cardinality(Running) =< MaxConc

(* Any node that has started (running or completed-successfully) had all of *)
(* its dependencies resolved. (A node reaches "failed" only via running, so *)
(* it too respected this at Start time; we assert on running/done which are *)
(* the post-conditions reachable only through Start.)                       *)
DepsBeforeRun ==
    \A n \in Nodes :
        (status[n] \in {"running","done"}) => DepsResolved(n)

(* Failure-safety: a hard (non-coe) failure always implies scheduling is    *)
(* halted. Together with Start requiring ~halted, this means no node starts  *)
(* after a hard failure -- the fail-fast guarantee.                          *)
HardFailureHalts ==
    \A n \in Nodes :
        (status[n] = "failed" /\ n \notin ContinueOnError) => halted

(* Skipped soundness (DEC-CHUNK3-status, S1): a node is "skipped" only if it *)
(* has a terminal non-resolving dependency. The contrapositive of the gopter *)
(* property's soundness arm — a node whose deps all resolved is never        *)
(* skipped, and an independent unreached node stays pending, not skipped.    *)
SkippedSound ==
    \A n \in Nodes :
        (status[n] = "skipped") => HasSkipCauseDep(n)

Safety == TypeOK /\ ConcurrencyBound /\ DepsBeforeRun /\ HardFailureHalts
          /\ SkippedSound

------------------------------------------------------------------------
(* LIVENESS *)

(* Every behavior eventually reaches a SETTLED fixed point and stays there:   *)
(* no deadlock AND no livelock/refusal-to-schedule. Settled asserts no node is *)
(* Stuck — every node is terminal, or a legitimately blocked pending node      *)
(* (halted/unresolved-deps AND no skip-cause). This has TEETH: a scheduler     *)
(* that refuses to start an eligible node, or never finishes a running node,    *)
(* leaves a Stuck node forever, so <>[]Settled FAILS with a counterexample. It *)
(* is deliberately stronger than "<>[]nothing-enabled" (which a refuse-to-     *)
(* schedule bug satisfies vacuously). Under S1 a Settled state may still hold   *)
(* pending nodes (independent, unreached, no skip-cause dep) — the faithful     *)
(* model of Execute returning with such a node left Pending; that is why the    *)
(* target is Settled, not all-nodes-terminal.                                   *)
Termination == <>[]Settled

=============================================================================
