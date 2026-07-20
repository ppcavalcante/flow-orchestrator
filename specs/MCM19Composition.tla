---------------------- MODULE MCM19Composition ----------------------
(* Model-checking harness for the M19 SUB-WORKFLOW-AWAIT arm (ph96 formal capstone). *)
(* Mirrors MCM12Saga: supplies the concrete DAG + the sub-workflow constants (the     *)
(* tuple/function syntax the .cfg cannot express) and forwards the rest from the .cfg. *)
(*                                                                                    *)
(* Concrete instance — three INDEPENDENT sub-workflow nodes hung off a root p, chosen  *)
(* so each of the four composition invariants is exercised NON-VACUOUSLY:              *)
(*   p        : an ordinary root that completes (the parent-of-all).                   *)
(*   swOk     : a SubWorkflowNode at depth 1 (< MaxDepth) whose child SUCCEEDS. It      *)
(*              Starts -> SpawnChild (spawned=1) -> Suspend (parks) -> ChildComplete    *)
(*              (succeeded, wakeReady) -> Wake -> Finish (done). Exercises the full      *)
(*              spawn/park/wake round-trip + NoDoubleSpawn (spawned reaches exactly 1,   *)
(*              incl. across a crash re-drive) + ParkWakeExactlyOnTerminal.             *)
(*   swFail   : a SubWorkflowNode at depth 1 whose child FAILS (swFail \in ChildFailSet).*)
(*              Its child terminal is "failed" -> Finish maps the parent node to        *)
(*              "failed" (INV-01). Exercises ChildFailParentFail non-vacuously.         *)
(*   swDeep   : a SubWorkflowNode at depth = MaxDepth (>= the ceiling) -> DepthRefuse    *)
(*              fires (loud terminal, never spawns). Exercises BoundedNesting.          *)
(*                                                                                    *)
(* All three sub-workflow nodes are declared Suspendable (they park) and NON-timer      *)
(* (their wake is the child-completion signal, not a clock). FailSet is DISJOINT from   *)
(* SubWorkflowNodes (a sub-workflow node fails ONLY via its child, never its own action).*)
(* MaxCrashes = 1 is the exhaustive config: the crash x spawn x park/wake composition —  *)
(* incl. the spawn-idempotency-across-resume that NoDoubleSpawn is the formal witness of. *)
EXTENDS FiniteSets, Naturals

CONSTANTS p, swOk, swFail, swDeep, ContinueOnError, FailSet, MaxConc, MaxCrashes,
          Suspendable, TimerNodes, MaxTick,
          SubWorkflowNodes, ChildFailSet, MaxDepth

MCNodes == {p, swOk, swFail, swDeep}
\* The three sub-workflow nodes each depend on p (p runs first, then they spawn in
\* parallel). No dependency among the sub-workflow nodes — they are independent spawns.
MCDeps  == { <<p,swOk>>, <<p,swFail>>, <<p,swDeep>> }

\* No timers — the sub-workflow wake is the child-completion signal (FireAt unused).
MCFireAt == [n \in MCNodes |-> 0]

\* Journal-determined nesting depth (durable-by-data, NOT nondet — the ph44 trap): the
\* root and the two below-ceiling nodes sit at depth 1; swDeep sits AT the ceiling so it
\* must DepthRefuse (BoundedNesting boundary). MaxDepth = 3 comes from the .cfg.
MCNodeDepth == [n \in MCNodes |-> IF n = swDeep THEN MaxDepth ELSE 1]

VARIABLES status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount,
          mailbox, delivered, applied, recorded, rollingBack, triggerCause,
          spawned, childTerminal

\* M11 OR-join arm EMPTY (inert).
MCChoiceNodes    == {}
MCChoiceFailSet  == {}
MCMergeNodes     == {}
MCChoiceBranches == [c \in {} |-> {}]
MCChosenBranch   == [c \in {} |-> p]
MCMergeTails     == [m \in {} |-> {}]
\* M12 saga arm EMPTY (inert) — the compensation-boundary is asserted in the gopter arm
\* (a child owns its own saga; the parent's M12 rollback never crosses into a child journal).
MCSagaNodes    == {}
MCCompFailSet  == {}
MCSagaTrigger  == "none"

INSTANCE M10DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps, FireAt <- MCFireAt,
    ChoiceNodes <- MCChoiceNodes, ChoiceFailSet <- MCChoiceFailSet, MergeNodes <- MCMergeNodes,
    ChoiceBranches <- MCChoiceBranches, ChosenBranch <- MCChosenBranch, MergeTails <- MCMergeTails,
    SagaNodes <- MCSagaNodes, CompFailSet <- MCCompFailSet, SagaTrigger <- MCSagaTrigger,
    NodeDepth <- MCNodeDepth
=============================================================================
