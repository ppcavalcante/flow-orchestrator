-------------------------- MODULE M20Caps --------------------------
(***************************************************************************)
(* M20 ph102 — the concurrency-cap safety arm (CapNeverExceeded).           *)
(*                                                                          *)
(* Models N workers competing to claim runs of a capped type. The           *)
(* load-bearing safety claim (DEC-P98-COUNT-IN-TXN / DEC-M20-D3): the        *)
(* RUNNING count (claimed AND NOT parked) of a type NEVER exceeds cap(type), *)
(* because the count read and the claim happen ATOMICALLY inside ONE         *)
(* BEGIN IMMEDIATE txn (the row-COUNT is the arbiter, never a fenced/derived *)
(* counter — [[engineer-fencing-extends-to-derived-state]]).                 *)
(*                                                                          *)
(* TLC serializes actions == the SQLite write-lock. The seed-break is the    *)
(* classic TOCTOU: a NON-atomic count-then-claim.                            *)
(*                                                                          *)
(*   CapAtomic = TRUE  : the count check and the increment are ONE atomic     *)
(*     action -> CapNeverExceeded holds.                                     *)
(*   CapAtomic = FALSE : SEED-THE-BREAK -- the count read (Decide) and the    *)
(*     increment (Commit) are split. Two workers both read running < cap,     *)
(*     then both Commit -> running = cap + 1 -> CapNeverExceeded Falsifies.   *)
(*     (A fenced counter that lags the durable rows has exactly this shape.)  *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Workers,     \* set of worker process ids
    Cap,         \* the per-type running cap (state-space cap; >= 1)
    CapAtomic    \* TRUE = real (atomic count+claim); FALSE = seed-the-break (TOCTOU) -> must Falsify

ASSUME Cap \in Nat /\ Cap >= 1
ASSUME CapAtomic \in BOOLEAN

VARIABLES
    running,     \* running: the count of claimed-AND-not-parked runs of the (single modeled) type
    wstate       \* wstate[w]: "idle" | "decided" (read count, not yet committed) | "run"

vars == <<running, wstate>>

Init ==
    /\ running = 0
    /\ wstate  = [w \in Workers |-> "idle"]

\* Claim: a worker claims a run inside its BEGIN IMMEDIATE txn. The guard `running < Cap` is the
\* COUNT read (over durable claimed-not-parked rows). TLC serializes Claim == the write-lock.
\*   CapAtomic = TRUE : count-check AND increment are ONE atomic step -> running never exceeds Cap.
\*   CapAtomic = FALSE: only DECIDE here (read count, no increment); the increment is a separate
\*     Commit -> two workers both decide under running < Cap, then both commit -> overshoot.
Claim(w) ==
    /\ wstate[w] = "idle"
    /\ running < Cap
    /\ IF CapAtomic
         THEN /\ running' = running + 1
              /\ wstate'  = [wstate EXCEPT ![w] = "run"]
         ELSE /\ wstate'  = [wstate EXCEPT ![w] = "decided"]   \* TOCTOU: decided on a stale count
              /\ UNCHANGED running

\* Commit (seed-break only): the deferred increment lands, based on the STALE decision -- it does
\* NOT re-check running against Cap, so a second decided worker overshoots.
Commit(w) ==
    /\ ~CapAtomic
    /\ wstate[w] = "decided"
    /\ running' = running + 1
    /\ wstate'  = [wstate EXCEPT ![w] = "run"]

\* Done: a run finishes and releases its slot (keeps the model live / bounds running).
Done(w) ==
    /\ wstate[w] = "run"
    /\ running > 0
    /\ running' = running - 1
    /\ wstate'  = [wstate EXCEPT ![w] = "idle"]

Next ==
    \/ \E w \in Workers : Claim(w)
    \/ \E w \in Workers : Commit(w)
    \/ \E w \in Workers : Done(w)
    \/ (\A w \in Workers : wstate[w] = "idle") /\ running = 0 /\ UNCHANGED vars  \* terminal stutter

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ running \in 0..(Cap + Cardinality(Workers))
    /\ wstate  \in [Workers -> {"idle", "decided", "run"}]

\* THE biting invariant (DEC-P98-COUNT-IN-TXN): the running count never exceeds the cap. With
\* CapAtomic = TRUE it holds; the seed-break (CapAtomic = FALSE, non-atomic count-then-claim) lets
\* two workers both claim under a stale count -> running = Cap + 1 -> Falsifies. This proves the
\* atomic COUNT-in-txn (not a lagging fenced counter) is load-bearing.
CapNeverExceeded ==
    running <= Cap

=============================================================================
