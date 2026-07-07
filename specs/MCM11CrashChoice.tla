---------------------- MODULE MCM11CrashChoice ----------------------
(* M11 crash x choice bounded-exhaustive harness (phase 44, DEC-M11-P44-        *)
(* CRASHCHOICE — REQUIRED, not conditional). The pre-committed bounded scenario: *)
(* a single ChoiceNode c, TWO branches {bA, bB}, a MergeNode m over both, under  *)
(* ONE crash (MaxCrashes=1). FailSet={bA}. No timers/signals (Suspendable={}) to  *)
(* keep the crash x choice product exhaustively checkable.                       *)
(*                                                                            *)
(*   c --> bA (FailSet) --\                                                     *)
(*     --> bB -------------+--> m (MergeTails = {bA, bB})                       *)
(*     ------ Choice-dep --/                                                    *)
(*                                                                            *)
(* Proves the OR-join composes with crash-resume: a bypassed branch STAYS       *)
(* bypassed after Recover (the decision is durable in the journal), and a taken  *)
(* tail's failure still BLOCKS the merge (fail-fast) post-crash — the separator  *)
(* holds ACROSS a crash. (taken=bA -> bA fails -> m SKIPPED; taken=bB -> bA      *)
(* bypassed, bB done -> m FIRES.) *)
EXTENDS FiniteSets, Naturals

CONSTANTS c, bA, bB, m, pick, cfail, ContinueOnError, FailSet, MaxConc, MaxCrashes,
          Suspendable, TimerNodes, MaxTick

MCNodes == {c, bA, bB, m}
MCDeps  == { <<c,bA>>, <<c,bB>>, <<bA,m>>, <<bB,m>>, <<c,m>> }
MCFireAt == [n \in MCNodes |-> 0]

MCChoiceNodes    == {c}
MCChoiceFailSet  == cfail   \* {} = c routes; {c} = c fails to route across a crash
MCMergeNodes     == {m}
MCChoiceBranches == [x \in {c} |-> {bA, bB}]
\* Deterministic route (durable across the crash — CHOICE-02). Run per pick in
\* {bA, bB}: bA -> a taken-tail failure blocks m across a crash; bB -> m fires,
\* bA stays bypassed across a crash.
MCChosenBranch   == [x \in {c} |-> pick]
MCMergeTails     == [x \in {m} |-> {bA, bB}]

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
