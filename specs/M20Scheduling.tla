-------------------------- MODULE M20Scheduling --------------------------
(***************************************************************************)
(* M20 ph102 — the scheduling safety arm (NoDoubleFire).                     *)
(*                                                                          *)
(* Models N poller processes racing to fire ONE durable schedule onto the    *)
(* work_queue. The load-bearing safety claim (DEC-P100-RECHECK-IS-ARBITER,   *)
(* the ph100 empirical finding, now machine-checked): a due slot is enqueued *)
(* AT MOST ONCE across N racing pollers, because the fire's BEGIN IMMEDIATE   *)
(* txn re-reads next_fire (the in-txn re-check) and advances it ATOMICALLY    *)
(* with the enqueue. The arbiter is the serialized-txn + in-txn re-check +    *)
(* atomic advance -- NOT the fence (the fence is a separate one-writer /      *)
(* crash-reclaim liveness layer, modeled in M20Caps' NoCapWedge, not here).  *)
(*                                                                          *)
(* TLC serializes actions == the SQLite BEGIN IMMEDIATE write-lock: at most   *)
(* one Fire commits at a time. No wall clock is modeled -- `now` is an        *)
(* abstract monotonic Tick (a slot becoming due), exactly the MPFencing       *)
(* "no clock, safety independent of WHEN" discipline.                        *)
(*                                                                          *)
(*   Recheck = TRUE  : the real model (enqueue + advance are ONE atomic       *)
(*     action) -> NoDoubleFire holds.                                        *)
(*   Recheck = FALSE : SEED-THE-BREAK -- the advance is DEFERRED (non-atomic  *)
(*     with the enqueue, i.e. the fence-as-arbiter fallacy where next_fire    *)
(*     is not the in-txn arbiter). A second poller sees the SAME due          *)
(*     next_fire before it advances and enqueues the slot AGAIN ->            *)
(*     NoDoubleFire Falsifies. This is the ph100 double-fire, machine-checked.*)
(***************************************************************************)
EXTENDS Naturals

CONSTANTS
    Procs,       \* set of poller process ids
    MaxSlot,     \* bound the clock/slot index (state-space cap)
    Recheck      \* TRUE = real (atomic re-check+advance); FALSE = seed-the-break -> must Falsify

ASSUME MaxSlot \in Nat /\ MaxSlot >= 1
ASSUME Recheck \in BOOLEAN

VARIABLES
    now,          \* abstract current time; advances via Tick (a slot becomes due). No wall clock.
    nextFire,     \* the schedule's durable stored next-fire slot -- the arbiter state
    enqueued,     \* enqueued[s]: GHOST count of runs enqueued for slot s (the safety oracle)
    pendingAdv    \* seed-break only: a fire happened but its (deferred, non-atomic) advance has not settled

vars == <<now, nextFire, enqueued, pendingAdv>>

Slots  == 0..MaxSlot
Fires  == 0..(MaxSlot + 1)   \* nextFire may advance one past the last due slot, then no Fire is enabled

Init ==
    /\ now        = 0
    /\ nextFire   = 0             \* the first slot is due at time 0
    /\ enqueued   = [s \in Slots |-> 0]
    /\ pendingAdv = FALSE

\* Tick: abstract time advances -- the next stored slot becomes due. No wall clock is modeled
\* (like MPFencing's nondeterministic lapse: safety must hold regardless of WHEN a slot comes due).
Tick ==
    /\ now < MaxSlot
    /\ now' = now + 1
    /\ UNCHANGED <<nextFire, enqueued, pendingAdv>>

\* Fire: a poller fires the due schedule inside its BEGIN IMMEDIATE txn. Guard `nextFire <= now`
\* is the in-txn re-check: a poller whose scan is stale re-reads next_fire, sees it already
\* advanced past now, and does nothing. TLC serializes Fire == the write-lock (one commit at a time).
\*   Recheck = TRUE : the advance is ATOMIC with the enqueue -> the NEXT Fire re-reads the advanced
\*     next_fire (> now) -> not due -> never re-enqueues this slot.
\*   Recheck = FALSE: the advance is DEFERRED (pendingAdv) -> a second poller re-reads the SAME
\*     next_fire <= now before it advances -> enqueues the slot AGAIN -> double-fire.
Fire(p) ==
    /\ nextFire \in Slots
    /\ nextFire <= now                        \* due -- the in-txn re-check
    \* count enqueues per slot, capped at 2 (reaching 2 already Falsifies NoDoubleFire; the cap
    \* keeps the state space finite in the seed-break, where the deferred advance lets fires repeat).
    /\ enqueued' = [enqueued EXCEPT ![nextFire] = IF enqueued[nextFire] < 2 THEN enqueued[nextFire] + 1 ELSE 2]
    /\ IF Recheck
         THEN /\ nextFire'   = nextFire + 1    \* atomic advance-in-the-same-txn (the real arbiter)
              /\ pendingAdv'  = FALSE
         ELSE /\ nextFire'   = nextFire        \* advance deferred (non-atomic) -- the seed-the-break
              /\ pendingAdv'  = TRUE
    /\ UNCHANGED now

\* DeferredAdvance (seed-break only): the non-atomic advance eventually settles. In the window
\* before it does, other Fires double-enqueue the same slot.
DeferredAdvance ==
    /\ ~Recheck
    /\ pendingAdv
    /\ nextFire < MaxSlot + 1
    /\ nextFire' = nextFire + 1
    /\ pendingAdv' = FALSE
    /\ UNCHANGED <<now, enqueued>>

Next ==
    \/ \E p \in Procs : Fire(p)
    \/ Tick
    \/ DeferredAdvance
    \/ (now >= MaxSlot /\ nextFire > MaxSlot /\ ~pendingAdv /\ UNCHANGED vars)  \* terminal stutter

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ now       \in 0..MaxSlot
    /\ nextFire  \in Fires
    /\ enqueued  \in [Slots -> 0..2]   \* per-slot enqueue count, capped at 2 (see Fire)
    /\ pendingAdv \in BOOLEAN

\* THE biting invariant (DEC-P100-RECHECK-IS-ARBITER): a due slot is enqueued AT MOST ONCE across
\* all N racing pollers. With Recheck = TRUE it holds; the seed-break (Recheck = FALSE, deferred
\* non-atomic advance) lets a second poller re-enqueue the same slot -> enqueued[s] = 2 -> Falsifies.
NoDoubleFire ==
    \A s \in Slots : enqueued[s] <= 1

=============================================================================
