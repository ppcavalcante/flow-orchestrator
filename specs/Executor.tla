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

(* Skip n: once halted, pending nodes can no longer start; model them       *)
(* reaching a terminal "skipped" state so the system terminates (mirrors    *)
(* Execute returning while remaining nodes never run).                      *)
Skip(n) ==
    /\ halted
    /\ status[n] = "pending"
    /\ status' = [status EXCEPT ![n] = "skipped"]
    /\ UNCHANGED halted

(* Once every node is terminal the executor has drained: model that as a    *)
(* stable fixed point (stutter) so completion is not mistaken for deadlock   *)
(* and Termination (<>[]AllTerminal) is a meaningful liveness property.      *)
Done == AllTerminal /\ UNCHANGED vars

Next == (\E n \in Nodes : Start(n) \/ Finish(n) \/ Skip(n)) \/ Done

(* Weak fairness on every node's transitions guarantees progress: any node  *)
(* continuously able to start/finish/skip eventually does, so the system    *)
(* cannot stall with work remaining.                                        *)
Fairness == \A n \in Nodes : WF_vars(Start(n) \/ Finish(n) \/ Skip(n))

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

Safety == TypeOK /\ ConcurrencyBound /\ DepsBeforeRun /\ HardFailureHalts

------------------------------------------------------------------------
(* LIVENESS *)

(* Every behavior eventually reaches a state where all nodes are terminal   *)
(* and stays there: no deadlock, the executor always drains.                *)
Termination == <>[]AllTerminal

=============================================================================
