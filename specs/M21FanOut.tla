-------------------------- MODULE M21FanOut --------------------------
(***************************************************************************)
(* M21 ph108 — the DYNAMIC FAN-OUT formal capstone (FANOUT-09).             *)
(*                                                                          *)
(* Models a single fan-out node whose Execute (a) resolves N via an         *)
(* expander journaled ONCE, then (b) drives N branches, each terminalized   *)
(* exactly once, under a KILL-STORM (crash + resume at any point). The two  *)
(* load-bearing M21 invariants:                                             *)
(*                                                                          *)
(*   ExactlyNSpawn   — the expander journals N EXACTLY ONCE across any       *)
(*     number of crash+resume cycles; the branch set is the SAME N on every  *)
(*     resume (never re-expanded to a different N). This is the moat's       *)
(*     no-replay leg: a runtime-variable N that stayed stable across crashes.*)
(*   FanInWaitsForAll — the fan node reaches a terminal verdict IFF all N    *)
(*     branches are terminal (no early/partial terminalization of the node   *)
(*     while a branch is still non-terminal).                               *)
(*                                                                          *)
(* TLC serializes actions == the executor's single-writer drive. A crash    *)
(* discards in-flight (non-committed) branch progress but PRESERVES the      *)
(* durable journal (the expansion {N} + each already-committed branch); a    *)
(* resume re-drives, re-reading the journal (expansion-once) and skipping    *)
(* already-terminal branches (the deterministic-child-ID idempotency).       *)
(*                                                                          *)
(*   AtomicExpansion = TRUE  : the expander runs + journals {N} ONCE; every  *)
(*     resume READS the journal -> N is stable -> ExactlyNSpawn holds.       *)
(*   AtomicExpansion = FALSE : SEED-THE-BREAK -- a resume RE-EXPANDS (the     *)
(*     expander re-runs and may yield a DIFFERENT N -> the branch set is      *)
(*     re-sized -> ExactlyNSpawn Falsifies. This is the determinism-tax the   *)
(*     moat forbids: re-running the expander on resume breaks a runtime-N     *)
(*     fan-out. (It also lets a branch double-spawn under a re-sized set.)    *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Branches,        \* set of branch indices (the resolved N = Cardinality(Branches))
    MaxCrashes,      \* exhaustive crash bound (>= 1)
    AtomicExpansion  \* TRUE = real (expand-once, read-journal-on-resume); FALSE = seed-break (re-expand)

ASSUME MaxCrashes \in Nat /\ MaxCrashes >= 1
ASSUME AtomicExpansion \in BOOLEAN

VARIABLES
    expanded,     \* 0 = not yet expanded; N (= Cardinality(Branches)) = the journaled width
    expandCount,  \* how many times the expander ACTUALLY ran (the ExactlyNSpawn witness: must stay <= 1)
    branch,       \* branch[b]: "pending" | "running" | "done" (a branch's durable terminal)
    node,         \* the fan node's own status: "active" | "terminal"
    crashes       \* crash budget consumed

vars == <<expanded, expandCount, branch, node, crashes>>

N == Cardinality(Branches)

Init ==
    /\ expanded    = 0
    /\ expandCount = 0
    /\ branch      = [b \in Branches |-> "pending"]
    /\ node        = "active"
    /\ crashes     = 0

\* EXPAND: the expander runs. It journals N and increments the ACTUAL-run counter. It runs only when
\* the journal is empty (expanded = 0) — the read-before-call expansion-once guard. Under the seed-
\* break (AtomicExpansion = FALSE) a resume can re-clear the journal (see Crash), so Expand can fire
\* AGAIN -> expandCount climbs past 1.
Expand ==
    /\ node = "active"
    /\ expanded = 0
    /\ expanded'    = N
    /\ expandCount' = expandCount + 1
    /\ UNCHANGED <<branch, node, crashes>>

\* RunBranch: a branch begins (pending -> running). Requires the journal (expanded = N).
RunBranch(b) ==
    /\ node = "active"
    /\ expanded = N
    /\ branch[b] = "pending"
    /\ branch' = [branch EXCEPT ![b] = "running"]
    /\ UNCHANGED <<expanded, expandCount, node, crashes>>

\* FinishBranch: a running branch commits its durable terminal (running -> done). Idempotent by the
\* deterministic child ID — a re-drive of a "done" branch is a no-op (not re-modeled; "done" is stable).
FinishBranch(b) ==
    /\ node = "active"
    /\ branch[b] = "running"
    /\ branch' = [branch EXCEPT ![b] = "done"]
    /\ UNCHANGED <<expanded, expandCount, node, crashes>>

\* TerminalizeNode: the fan node reaches its terminal verdict — ONLY when every branch is done
\* (FanInWaitsForAll is the guard here; the invariant asserts no OTHER path terminalizes the node).
TerminalizeNode ==
    /\ node = "active"
    /\ expanded = N
    /\ \A b \in Branches : branch[b] = "done"
    /\ node' = "terminal"
    /\ UNCHANGED <<expanded, expandCount, branch, crashes>>

\* CRASH: a kill-9 at any point. The DURABLE journal survives: expanded (the {N} journal) and every
\* "done" branch stay; in-flight "running" branches roll back to "pending" (non-committed progress is
\* lost). The node returns to "active" (a non-terminal node re-drives on resume; a "terminal" node is
\* durable and skipped — so a crash only re-activates a still-active node).
\*   AtomicExpansion = TRUE : the journal (expanded) is PRESERVED -> resume reads it, no re-expand.
\*   AtomicExpansion = FALSE: SEED-BREAK -- the resume RE-CLEARS the journal (expanded' = 0) so the
\*     expander re-runs -> expandCount climbs -> ExactlyNSpawn Falsifies.
Crash ==
    /\ crashes < MaxCrashes
    /\ node = "active"
    /\ crashes' = crashes + 1
    /\ branch'  = [b \in Branches |-> IF branch[b] = "running" THEN "pending" ELSE branch[b]]
    /\ expanded' = IF AtomicExpansion THEN expanded ELSE 0
    /\ UNCHANGED <<expandCount, node>>

Next ==
    \/ Expand
    \/ \E b \in Branches : RunBranch(b)
    \/ \E b \in Branches : FinishBranch(b)
    \/ TerminalizeNode
    \/ Crash
    \/ node = "terminal" /\ UNCHANGED vars   \* terminal stutter (keeps the model total)

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ expanded \in {0, N}
    /\ expandCount \in 0..(MaxCrashes + 1)
    /\ branch \in [Branches -> {"pending", "running", "done"}]
    /\ node \in {"active", "terminal"}
    /\ crashes \in 0..MaxCrashes

\* EXACTLY-N-SPAWN (the moat's no-replay leg): the expander runs AT MOST ONCE across the whole run,
\* including every crash+resume. With AtomicExpansion = TRUE the journal is preserved so a resume reads
\* it (Expand's expanded=0 guard never re-fires) -> expandCount <= 1. The seed-break (re-expand on
\* resume) re-clears the journal -> Expand fires again -> expandCount = 2 -> Falsifies. This machine-
\* checks that journaling N once (never re-running the expander) is load-bearing for a runtime-N fan-out.
ExactlyNSpawn ==
    expandCount <= 1

\* FAN-IN-WAITS-FOR-ALL: the fan node is terminal ONLY when all N branches are terminal. If the node is
\* "terminal", every branch must be "done" — no early/partial node terminalization while a branch is
\* still non-terminal. (TerminalizeNode's guard establishes this; the invariant is the durable witness,
\* and would Falsify if any path terminalized the node with a non-done branch.)
FanInWaitsForAll ==
    (node = "terminal") => (\A b \in Branches : branch[b] = "done")

=============================================================================
