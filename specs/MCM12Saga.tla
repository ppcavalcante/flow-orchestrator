---------------------- MODULE MCM12Saga ----------------------
(* Model-checking harness for the M12 SAGA compensation/abort arm (phase 50).   *)
(* Concrete instance: a DIAMOND saga  s -> {a, b} -> t  (the spike's carry —     *)
(* parallel compensable branches a,b reconverging at t, so the reverse-topo      *)
(* compensation of a and b is the real INTERLEAVING source).                     *)
(*                                                                              *)
(*   - s, a, b, t are all COMPENSABLE (SagaNodes = all four).                    *)
(*   - t is in FailSet: t's action FAILS -> a hard (non-coe) failure halts the   *)
(*     forward run -> TriggerRollback arms the durable rolling_back marker +      *)
(*     journals triggerCause = "failure".                                        *)
(*   - the rollback drives reverse-topologically: t is Failed (not Completed) so  *)
(*     it is NOT compensated (no effect to undo); a and b (Completed, dependent t *)
(*     no longer `done`) compensate in PARALLEL; then s (its dependents a,b now   *)
(*     compensated) compensates. Best-effort partition = compensated {a,b,s}.     *)
(*                                                                              *)
(* MaxConc = 2 lets a,b run — and compensate — concurrently (the interleaving).  *)
(* MaxCrashes = 1 is the exhaustive config (DEC-M12-P50-CRASHROLLBACK; the spike  *)
(* showed the chain closes at 47, the diamond is bounded likewise).              *)
EXTENDS FiniteSets, Naturals

CONSTANTS s, a, b, t, ContinueOnError, FailSet, MaxConc, MaxCrashes,
          Suspendable, TimerNodes, MaxTick, SagaNodes, CompFailSet, SagaTrigger

MCNodes == {s, a, b, t}
MCDeps  == { <<s,a>>, <<s,b>>, <<a,t>>, <<b,t>> }

\* No timers in the saga config — FireAt is unused (TimerNodes = {}).
MCFireAt == [n \in MCNodes |-> 0]

VARIABLES status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount,
          mailbox, delivered, applied, recorded, rollingBack, triggerCause

\* M11 OR-join arm EMPTY for the saga config (inert).
MCChoiceNodes    == {}
MCChoiceFailSet  == {}
MCMergeNodes     == {}
MCChoiceBranches == [c \in {} |-> {}]
MCChosenBranch   == [c \in {} |-> s]
MCMergeTails     == [m \in {} |-> {}]

INSTANCE M10DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps, FireAt <- MCFireAt,
    ChoiceNodes <- MCChoiceNodes, ChoiceFailSet <- MCChoiceFailSet, MergeNodes <- MCMergeNodes,
    ChoiceBranches <- MCChoiceBranches, ChosenBranch <- MCChosenBranch, MergeTails <- MCMergeTails
=============================================================================
