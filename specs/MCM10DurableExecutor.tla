---------------------- MODULE MCM10DurableExecutor ----------------------
(* Model-checking harness for M10DurableExecutor: supplies the concrete DAG  *)
(* (Nodes + Deps, tuple syntax the .cfg cannot express) and forwards the     *)
(* remaining constants from the .cfg by name.                                *)
(*                                                                          *)
(* Concrete instance: diamond  n1 -> {n2, n3} -> n4 (same as MCDurable-      *)
(* Executor), with n2 the declared SUSPENSION node. So: n1 runs; n2 parks    *)
(* (waiting) until its event fires; its sibling n3 completes independently;  *)
(* the join n4 (depends on n2 AND n3) waits for n2 to wake, then runs. This  *)
(* exercises dependent-waits-on-parked, sibling-proceeds, and converge-on-   *)
(* wake in one DAG.                                                          *)
EXTENDS FiniteSets, Naturals

CONSTANTS n1, n2, n3, n4, ContinueOnError, FailSet, MaxConc, MaxCrashes,
          Suspendable, TimerNodes, MaxTick

MCNodes == {n1, n2, n3, n4}
MCDeps  == { <<n1,n2>>, <<n1,n3>>, <<n2,n4>>, <<n3,n4>> }

(* The timer due-time function (tuple syntax the .cfg cannot express): n2 is the   *)
(* declared TIMER node, due at logical tick 2. FireAt[n2] = 2 (>= 1) FORCES n2 to   *)
(* park first and the clock to actually advance to fire it — so the Tick-fairness   *)
(* liveness has real work to do (a FireAt of 0 would let FireTimer fire at clock=0   *)
(* without any Tick, hollowing the bite). FireAt = MaxTick keeps the clock space    *)
(* minimal (0..2). Non-timer nodes get 0 (unused — FireTimer is gated on            *)
(* n in TimerNodes). *)
MCFireAt == [n \in MCNodes |-> IF n = n2 THEN 2 ELSE 0]

VARIABLES status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount,
          mailbox, delivered, applied, recorded

\* M11 OR-join topology is EMPTY for the M10 diamond config — the OR-join arm is
\* inert, so this config re-runs the extended spec byte-behaviour-unchanged
\* (DEC-M11-P44-PRESERVE: the preservation proof is this exact re-run staying green
\* at ~14,380 states).
MCChoiceNodes    == {}
MCChoiceFailSet  == {}
MCMergeNodes     == {}
MCChoiceBranches == [c \in {} |-> {}]
MCChosenBranch   == [c \in {} |-> n1]
MCMergeTails     == [m \in {} |-> {}]

INSTANCE M10DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps, FireAt <- MCFireAt,
    ChoiceNodes <- MCChoiceNodes, ChoiceFailSet <- MCChoiceFailSet, MergeNodes <- MCMergeNodes,
    ChoiceBranches <- MCChoiceBranches, ChosenBranch <- MCChosenBranch, MergeTails <- MCMergeTails
=============================================================================
