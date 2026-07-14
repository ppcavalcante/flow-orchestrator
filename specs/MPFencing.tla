-------------------------- MODULE MPFencing --------------------------
(***************************************************************************)
(* M16 ph77 — the SHIPPED multi-process fencing formal arm (promoted from    *)
(* the ph73 spike sketch, structure unchanged — it was already faithful).    *)
(*                                                                          *)
(* The machine-checked M16 safety proof. Models N worker processes competing  *)
(* for ONE workflow via a durable lease   *)
(* row + a monotonic fencing token, with the fencing CAS riding the checkpoint *)
(* txn. The load-bearing safety claim (DEC-M16-D3): a stale-token writer's     *)
(* checkpoint is REJECTED, so at most one TOKEN-CURRENT writer ever commits.   *)
(*                                                                          *)
(* Key abstraction (DEC-M16-D3, the inversion): the wall-clock lease expiry is *)
(* NOT modeled as time. It is a NONDETERMINISTIC "LeaseLapse" event (a paused  *)
(* worker is indistinguishable from a dead one), which is exactly the honest    *)
(* liveness heuristic. Safety rests on the TOKEN, never the clock — so the      *)
(* model has no clock at all, and the safety invariant holds regardless of WHEN *)
(* lapse fires. This is what makes the arm small + exhaustible.                 *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Procs,        \* set of worker process ids
    MaxToken,     \* bound the fencing token (state-space cap)
    Fencing       \* TRUE = the real model; FALSE = the SEED-THE-BREAK (CAS removed) -> must Falsify

ASSUME MaxToken \in Nat /\ MaxToken >= 1
ASSUME Fencing \in BOOLEAN

\* One workflow with a fixed set of LEVELS (the decomposed journal's PK is (workflow, level);
\* a re-claimer re-writes levels). Small so the arm stays exhaustible.
Levels == {0, 1}

VARIABLES
    leaseOwner,   \* the process currently holding the lease, or "none"
    leaseToken,   \* the current (highest issued) fencing token on the lease row
    procToken,    \* procToken[p]: the token p believes it holds (0 = p holds no claim)
    journal,      \* journal[lvl]: the token under which level lvl's row was LAST written (0 = unwritten).
                  \* This mirrors the real decomposed store: one row per level, overwritten by UPSERT.
    maxWritten,   \* GHOST: maxWritten[lvl] = the highest token that has EVER written level lvl. The
                  \* safety oracle compares journal (what survived) against this (the newest writer).
    leaseLapsed   \* TRUE once the current owner's lease has (nondeterministically) lapsed,
                  \* making the workflow re-claimable. Reset on a fresh claim.

vars == <<leaseOwner, leaseToken, procToken, journal, maxWritten, leaseLapsed>>

Init ==
    /\ leaseOwner  = "none"
    /\ leaseToken  = 0
    /\ procToken   = [p \in Procs |-> 0]
    /\ journal     = [lvl \in Levels |-> 0]
    /\ maxWritten  = [lvl \in Levels |-> 0]
    /\ leaseLapsed = FALSE

\* Claim: a process claims the workflow when it is unowned OR the lease has lapsed. Bumps the
\* durable token monotonically (the fencing token) and records it as the claimant's token.
\* This is the INSERT-or-CAS in one IMMEDIATE txn — exactly one process transitions per step
\* (TLC serializes actions), modeling the write-lock serialization.
Claim(p) ==
    /\ leaseToken < MaxToken                 \* bound the search
    /\ (leaseOwner = "none" \/ leaseLapsed)  \* claimable: unowned or lapsed (the liveness gate)
    /\ leaseOwner'  = p
    /\ leaseToken'  = leaseToken + 1
    /\ procToken'   = [procToken EXCEPT ![p] = leaseToken + 1]
    /\ leaseLapsed' = FALSE                   \* a fresh claim resets the lapse flag
    /\ UNCHANGED <<journal, maxWritten>>

\* LeaseLapse: the NONDETERMINISTIC liveness event — the current owner is now considered
\* dead/slow (a paused-but-alive worker is indistinguishable). Makes the workflow re-claimable.
\* It does NOT touch procToken: the (possibly still-alive) owner STILL believes it holds its
\* token — that is the zombie condition the fencing must defend against.
LeaseLapse ==
    /\ leaseOwner # "none"
    /\ ~leaseLapsed
    /\ leaseLapsed' = TRUE
    /\ UNCHANGED <<leaseOwner, leaseToken, procToken, journal, maxWritten>>

\* Checkpoint: a process attempts to commit a journal write under the token it BELIEVES it
\* holds (procToken[p]). The FENCING CAS: with Fencing = TRUE the write lands ONLY if the
\* process's token is STILL the current lease token (procToken[p] = leaseToken) — a stale
\* zombie (procToken[p] < leaseToken after someone re-claimed) is REJECTED. With Fencing =
\* FALSE the CAS is removed: any process that ever held a token can land a write -> the
\* stale write lands -> the safety invariant Falsifies (the seed-the-break).
Checkpoint(p, lvl) ==
    /\ procToken[p] > 0                        \* p has claimed at some point
    \* The FENCING CAS (in the checkpoint's IMMEDIATE txn): the write lands only if p's token
    \* is STILL current. Removing it (Fencing = FALSE) lets a stale zombie overwrite a level a
    \* newer owner already wrote -> the safety invariant Falsifies (the seed-the-break).
    /\ (Fencing => procToken[p] = leaseToken)
    /\ journal' = [journal EXCEPT ![lvl] = procToken[p]]
    \* The ghost records the highest token that ever wrote this level (for the safety oracle).
    /\ maxWritten' = [maxWritten EXCEPT ![lvl] = IF procToken[p] > maxWritten[lvl] THEN procToken[p] ELSE maxWritten[lvl]]
    /\ UNCHANGED <<leaseOwner, leaseToken, procToken, leaseLapsed>>

Next ==
    \/ \E p \in Procs : Claim(p)
    \/ \E p \in Procs, lvl \in Levels : Checkpoint(p, lvl)
    \/ LeaseLapse
    \/ (leaseToken >= MaxToken /\ UNCHANGED vars)   \* terminal stutter at the token cap

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ leaseOwner \in (Procs \union {"none"})
    /\ leaseToken \in 0..MaxToken
    /\ procToken \in [Procs -> 0..MaxToken]
    /\ journal    \in [Levels -> 0..MaxToken]
    /\ maxWritten \in [Levels -> 0..MaxToken]
    /\ leaseLapsed \in BOOLEAN

\* THE biting invariant (DEC-M16-D7 restated over the decomposed journal): NO STALE OVERWRITE.
\* For every level, the SURVIVING journal row's token equals the HIGHEST token that ever wrote
\* that level — i.e. a stale (fenced-out) zombie's lower-token write NEVER overwrote a newer
\* owner's row. This is the faithful "at most one token-current writer" for the row-decomposed
\* store: a legit historical write (token < current) is fine as long as it did not CLOBBER a
\* newer one. With Fencing = TRUE it holds; the seed-break (Fencing = FALSE) lets a stale zombie
\* overwrite a re-claimer's level -> journal[lvl] < maxWritten[lvl] -> Falsifies.
NoStaleOverwrite ==
    \A lvl \in Levels : journal[lvl] = maxWritten[lvl]

=============================================================================
