---------------------- MODULE MCM10FailResume ----------------------
(* Model-checking harness for the ph39 T5 plain-resume-of-a-hard-failed-run        *)
(* scenario (closes FIND-M10-P36-T2). A SEPARATE, small instance so it exhaustively *)
(* + cheaply exercises exactly the failure-resume path, while MCM10DurableExecutor  *)
(* (FailSet = {}) certifies the suspend/signal/timer invariants on the diamond.     *)
(*                                                                                  *)
(* Topology: nF (hard FailSet root) -> nD (its dependent); nT (an INDEPENDENT timer *)
(* root, no dep on nF). The scenario: nF hard-fails (fail-fast halts THIS attempt); *)
(* nD is a dependent of a non-coe Failed node so it Skips; nT is genuinely           *)
(* independent. A Crash + Recover (halted RESETS to FALSE — Option A) starts a fresh *)
(* attempt where nF stays Failed (terminal, NoResurrection), nD stays Skipped, and   *)
(* the INDEPENDENT overdue timer nT completes — the exact Go behavior                *)
(* (parallel_execution.go:88: a loaded-Failed node is terminal/skipped, not a fresh  *)
(* fail-fast trigger; an independent node runs; Execute returns nil). DEC-M10-P39-T5.*)
EXTENDS FiniteSets, Naturals

CONSTANTS nF, nD, nT, ContinueOnError, FailSet, MaxConc, MaxCrashes,
          Suspendable, TimerNodes, MaxTick

MCNodes == {nF, nD, nT}
MCDeps  == { <<nF, nD>> }   \* nD depends on the failing node; nT is independent

(* nT is the independent timer, due at logical tick 2 (so it must park first and the *)
(* clock must advance to fire it — the Tick-fairness has real work). Non-timer nodes *)
(* get 0 (unused — FireTimer is gated on n in TimerNodes). *)
MCFireAt == [n \in MCNodes |-> IF n = nT THEN 2 ELSE 0]

VARIABLES status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount,
          mailbox, delivered, applied, recorded, rollingBack, triggerCause,
          spawned, childTerminal

\* M11 OR-join + M12 saga arms are EMPTY/inert for this M10 fail-resume config, so
\* they re-run the extended spec byte-behaviour-unchanged (DEC-M11-P44-PRESERVE /
\* M12 preservation) — the fail-resume scenario is unaffected by the later arms.
\* These bindings were missing (the wrapper went stale when M10DurableExecutor gained
\* the M11/M12 constants+variables), which is why it failed to parse (task #103).
MCChoiceNodes    == {}
MCChoiceFailSet  == {}
MCMergeNodes     == {}
MCChoiceBranches == [c \in {} |-> {}]
MCChosenBranch   == [c \in {} |-> nF]
MCMergeTails     == [m \in {} |-> {}]
MCSagaNodes    == {}
MCCompFailSet  == {}
MCSagaTrigger  == "none"
\* M19 sub-workflow arm empty -> inert -> re-runs byte-behaviour-unchanged (DEC-P96-BASE).
MCSubWorkflowNodes == {}
MCChildFailSet     == {}
MCMaxDepth         == 3
MCNodeDepth        == [n \in MCNodes |-> 0]

INSTANCE M10DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps, FireAt <- MCFireAt,
    ChoiceNodes <- MCChoiceNodes, ChoiceFailSet <- MCChoiceFailSet, MergeNodes <- MCMergeNodes,
    ChoiceBranches <- MCChoiceBranches, ChosenBranch <- MCChosenBranch, MergeTails <- MCMergeTails,
    SagaNodes <- MCSagaNodes, CompFailSet <- MCCompFailSet, SagaTrigger <- MCSagaTrigger,
    SubWorkflowNodes <- MCSubWorkflowNodes, ChildFailSet <- MCChildFailSet,
    MaxDepth <- MCMaxDepth, NodeDepth <- MCNodeDepth
=============================================================================
