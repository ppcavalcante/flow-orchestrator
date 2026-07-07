---------------------- MODULE MCM11ChoiceMerge ----------------------
(* M11 OR-join model-check harness (phase 44). Topology: a single ChoiceNode c *)
(* with three branches, each an entry -> interior chain; a MergeNode m OR-joins  *)
(* the INTERIORS {bAi, bBi} (branch C is a leaf, NOT joined). m depends on       *)
(* bAi, bBi AND c (the DEPMODEL structural Choice-dep — EXCLUDED from the count). *)
(* Interiors make the cause-aware Bypass propagation (a bypassed entry -> its     *)
(* bypassed interior) a REAL action (not just the Choice's direct mark), so       *)
(* BypassedNeverRuns + the separator's failure propagation are non-vacuous.       *)
(*                                                                            *)
(*   c --> bA (FailSet) --> bAi --\                                             *)
(*     --> bB ------------> bBi ---+--> m (MergeTails = {bAi, bBi})             *)
(*     --> bC (leaf)               /                                            *)
(*     ------------ Choice-dep ---/                                            *)
(*                                                                            *)
(* The nondet branch pick exercises ALL three cases in one exhaustive config:  *)
(*   - takes bA -> bA runs & FAILS -> bAi SKIPPED (fail cascade) -> m has a      *)
(*     skip-cause tail -> m SKIPPED (fail-fast; failure BLOCKS — the separator). *)
(*   - takes bB -> bA/bAi & bC bypassed -> m over {bAi:bypassed, bBi:done} -> 1  *)
(*     taken -> m FIRES (bypass SATISFIES — the separator's other half).        *)
(*   - takes bC -> bA/bAi & bB/bBi bypassed -> m over both-bypassed -> 0 taken   *)
(*     -> m BYPASSED (the all-bypassed / anti-vacuity case).                    *)
(* Crash-free (MaxCrashes=0); the crash x choice scenario is MCM11CrashChoice.  *)
EXTENDS FiniteSets, Naturals

CONSTANTS c, bA, bAi, bB, bBi, bC, m, pick, cfail, ContinueOnError, FailSet, MaxConc,
          MaxCrashes, Suspendable, TimerNodes, MaxTick

MCNodes == {c, bA, bAi, bB, bBi, bC, m}
MCDeps  == { <<c,bA>>, <<c,bB>>, <<c,bC>>, <<bA,bAi>>, <<bB,bBi>>,
             <<bAi,m>>, <<bBi,m>>, <<c,m>> }
MCFireAt == [n \in MCNodes |-> 0]

MCChoiceNodes    == {c}
MCChoiceFailSet  == cfail   \* {} = c routes (per `pick`); {c} = c FAILS to route (44-F1 route)
MCMergeNodes     == {m}
MCChoiceBranches == [x \in {c} |-> {bA, bB, bC}]
\* `pick` (a cfg constant) is the branch c takes; explore all routes by running
\* per pick in {bA, bB, bC}. bA -> fail-blocks; bB -> bypass-satisfies; bC -> all-bypassed.
MCChosenBranch   == [x \in {c} |-> pick]
MCMergeTails     == [x \in {m} |-> {bAi, bBi}]

MCSagaNodes    == {}
MCCompFailSet  == {}
MCSagaTrigger  == "none"

VARIABLES status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount,
          mailbox, delivered, applied, recorded, rollingBack, triggerCause

INSTANCE M10DurableExecutor WITH Nodes <- MCNodes, Deps <- MCDeps, FireAt <- MCFireAt,
    ChoiceNodes <- MCChoiceNodes, ChoiceFailSet <- MCChoiceFailSet, MergeNodes <- MCMergeNodes,
    ChoiceBranches <- MCChoiceBranches, ChosenBranch <- MCChosenBranch, MergeTails <- MCMergeTails,
    SagaNodes <- MCSagaNodes, CompFailSet <- MCCompFailSet, SagaTrigger <- MCSagaTrigger
=============================================================================
