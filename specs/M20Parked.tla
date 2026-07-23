-------------------------- MODULE M20Parked --------------------------
(***************************************************************************)
(* M20 ph102 — the parked-exempt liveness arm (NoCapWedge).                 *)
(*                                                                          *)
(* Models K sub-workflow PARENTS, each of which claims a running slot, then  *)
(* PARKS awaiting a CHILD sub-workflow that must itself claim a slot to run.  *)
(* The load-bearing liveness claim (DEC-M20-D1 / DEC-P98-PARKED-COLUMN): a    *)
(* parked child is claimed-but-NOT-a-running-slot, so it is EXEMPT from the   *)
(* concurrency cap. Because parked parents do not consume slots, their        *)
(* children can always claim -> the system never wedges (deadlock-free).      *)
(*                                                                          *)
(* The check is TLC deadlock detection (CHECK_DEADLOCK, on by default):       *)
(*   ParkedExempt = TRUE  : a parked parent RELEASES its slot from the cap    *)
(*     referent -> its child claims, runs, completes, the parent resumes ->   *)
(*     every path reaches AllDone (a legit terminal stutter). NO deadlock.    *)
(*   ParkedExempt = FALSE : SEED-THE-BREAK -- a parked parent still COUNTS    *)
(*     against the cap -> with the cap full of parked parents, no child can   *)
(*     claim a slot, and no parent can resume (its child is stuck pending) -> *)
(*     a genuine DEADLOCK (the K-parked-parent wedge red-team BLOCKER-1 the    *)
(*     parked-column dissolved) -> TLC reports "Deadlock reached".            *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Parents,       \* set of sub-workflow parent ids (each has exactly one child)
    Cap,           \* the running-slot cap (>= 1)
    ParkedExempt   \* TRUE = real (parked child is cap-exempt); FALSE = seed-the-break -> must wedge

ASSUME Cap \in Nat /\ Cap >= 1
ASSUME ParkedExempt \in BOOLEAN

VARIABLES
    running,   \* count of claimed-AND-NOT-parked slots (the cap referent)
    pstate,    \* pstate[p]: "pending" | "running" | "parked" | "done"
    cstate     \* cstate[p]: "absent" | "pending" | "running" | "done"  (parent p's child)

vars == <<running, pstate, cstate>>

Init ==
    /\ running = 0
    /\ pstate  = [p \in Parents |-> "pending"]
    /\ cstate  = [p \in Parents |-> "absent"]

\* ParentClaim: a parent claims a running slot (the cap gates it, DEC-P98-COUNT-IN-TXN).
ParentClaim(p) ==
    /\ pstate[p] = "pending"
    /\ running < Cap
    /\ running' = running + 1
    /\ pstate'  = [pstate EXCEPT ![p] = "running"]
    /\ UNCHANGED cstate

\* ParentPark: the running parent spawns its child and PARKS awaiting it (the M19 suspend seam).
\* ParkedExempt = TRUE  : the parked row is claimed-AND-parked -> EXEMPT -> its slot is released
\*   from the cap referent (running decrements). This is DEC-P98-PARKED-COLUMN.
\* ParkedExempt = FALSE : the parked parent still counts as a running slot (the seed-the-break) ->
\*   it holds the slot while awaiting its child -> the wedge.
ParentPark(p) ==
    /\ pstate[p] = "running"
    /\ pstate'  = [pstate EXCEPT ![p] = "parked"]
    /\ cstate'  = [cstate EXCEPT ![p] = "pending"]      \* the child becomes claimable
    /\ running' = IF ParkedExempt THEN running - 1 ELSE running

\* ChildClaim: the child claims a running slot -- gated by the SAME cap.
ChildClaim(p) ==
    /\ cstate[p] = "pending"
    /\ running < Cap
    /\ running' = running + 1
    /\ cstate'  = [cstate EXCEPT ![p] = "running"]
    /\ UNCHANGED pstate

\* ChildDone: the child finishes and releases its slot.
ChildDone(p) ==
    /\ cstate[p] = "running"
    /\ running' = running - 1
    /\ cstate'  = [cstate EXCEPT ![p] = "done"]
    /\ UNCHANGED pstate

\* ParentResume: the parent's WAKE fires once its child is done (M19 completion-signal) -> it
\* completes. (The parked row is already off the cap referent when ParkedExempt; no re-claim needed
\* to model the wedge, whose crux is the CHILD getting a slot.)
ParentResume(p) ==
    /\ pstate[p] = "parked"
    /\ cstate[p] = "done"
    /\ pstate'  = [pstate EXCEPT ![p] = "done"]
    /\ UNCHANGED <<running, cstate>>

AllDone == \A p \in Parents : pstate[p] = "done"

Next ==
    \/ \E p \in Parents : ParentClaim(p)
    \/ \E p \in Parents : ParentPark(p)
    \/ \E p \in Parents : ChildClaim(p)
    \/ \E p \in Parents : ChildDone(p)
    \/ \E p \in Parents : ParentResume(p)
    \/ (AllDone /\ UNCHANGED vars)        \* the ONLY legit terminal stutter -- a wedge is NOT AllDone

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ running \in 0..(2 * Cardinality(Parents))
    /\ pstate  \in [Parents -> {"pending", "running", "parked", "done"}]
    /\ cstate  \in [Parents -> {"absent", "pending", "running", "done"}]

\* NoCapWedge is checked via TLC DEADLOCK DETECTION (CHECK_DEADLOCK, on by default): with
\* ParkedExempt = TRUE every path reaches AllDone (deadlock-free); the seed-break (ParkedExempt =
\* FALSE) reaches a state where a parked parent holds the last slot, its child cannot claim, and no
\* parent can resume -> no action enabled, not AllDone -> TLC reports "Deadlock reached". A companion
\* safety invariant for clarity: whenever a child is stuck pending under a full cap, at least one
\* slot-holder is NOT a parked parent (i.e. progress is always possible) -- see NoWedge below.
NoWedge ==
    \* If every slot is held AND some child is still pending, then NOT all slot-holders are parked
    \* parents (else nothing can advance). With ParkedExempt the parked parents never hold slots, so
    \* this holds; the seed-break reaches the all-parked-hold-the-cap wedge.
    (running >= Cap /\ \E p \in Parents : cstate[p] = "pending")
      => (\E p \in Parents : pstate[p] = "running" \/ cstate[p] = "running")

=============================================================================
