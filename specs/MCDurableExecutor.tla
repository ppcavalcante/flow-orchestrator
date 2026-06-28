---------------------- MODULE MCDurableExecutor ----------------------
(* Model-checking harness for DurableExecutor: supplies the concrete DAG    *)
(* (Nodes + Deps, tuple syntax the .cfg cannot express) and forwards the    *)
(* remaining constants (ContinueOnError, FailSet, MaxConc, MaxCrashes) from  *)
(* the .cfg by name.                                                         *)
(*                                                                          *)
(* Concrete instance: diamond  n1 -> {n2, n3} -> n4 (same as MCExecutor).   *)
EXTENDS FiniteSets, Naturals

CONSTANTS n1, n2, n3, n4, ContinueOnError, FailSet, MaxConc, MaxCrashes

MCNodes == {n1, n2, n3, n4}
MCDeps  == { <<n1,n2>>, <<n1,n3>>, <<n2,n4>>, <<n3,n4>> }

VARIABLES status, halted, journal, exec, up, crashes

INSTANCE DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps

------------------------------------------------------------------------
(* STATUS-CONVERGENCE — the STATUS arm of resume-equivalence, asserted         *)
(* DIRECTLY (not by composition) so it has its own teeth.                       *)
(*                                                                            *)
(* The precise claim is "crash+recover introduces NO new terminal state": every *)
(* settled (up, fully drained) state reached WITH crashes is one a NO-CRASH run  *)
(* could also reach. ValidFinal(s) characterises exactly the no-crash finals of  *)
(* the diamond n1 -> {n2,n3} -> n4 from the scenario constants.                  *)
(*                                                                            *)
(* NOTE — this is NOT a single unique vector. Writing it as an equality first    *)
(* surfaced a real subtlety (and is why a bite-proof matters): under a HARD       *)
(* failure of n2, its LEVEL-SIBLING n3 has a genuinely NON-deterministic final —  *)
(* "done" if n3 finished before n2's failure halted scheduling, or "pending" if   *)
(* the halt pre-empted n3's start. This race exists in the base Executor model    *)
(* too (Start is a per-node action gated on ~halted; an independent pending node   *)
(* legitimately rests — DEC-CHUNK3), and is faithful to the executor abstraction. *)
(* So convergence is to the SET of valid no-crash finals, and crash/recover must   *)
(* not escape it. (When n2 does not hard-fail — Clean / continue-on-error — the    *)
(* final IS unique: ValidFinal pins every node to one status.)                     *)
HardN2 == n2 \in FailSet /\ n2 \notin ContinueOnError   \* n2 is a hard (halting) failure

ValidFinal(s) ==
    /\ s[n1] = "done"                                       \* runs first; never fails in these configs
    /\ s[n2] = (IF n2 \in FailSet THEN "failed" ELSE "done")
    /\ s[n4] = (IF HardN2 THEN "skipped" ELSE "done")       \* skip-caused by a hard-failed n2, else completes
    /\ (IF HardN2 THEN s[n3] \in {"done","pending"}         \* the within-level fail-fast race
                  ELSE s[n3] = "done")

(* In ANY settled state — reached with OR without crashes — the per-node status   *)
(* is a valid no-crash final. A recovery that reaches a status NO no-crash run     *)
(* could (e.g. sweeps the un-run n3 to Skipped, leaves a clean-config node         *)
(* pending, or mis-derives halted onto a wrong frontier) escapes ValidFinal at a    *)
(* settled point and FALSIFIES this directly — independent of ExecFidelity          *)
(* (which catches fabricated done, not wrong-skip) and NoDoubleCommit. This is      *)
(* the status arm of "crash+recover converges to a state a no-crash run reaches".   *)
StatusConvergence == Settled => ValidFinal(status)
=============================================================================
