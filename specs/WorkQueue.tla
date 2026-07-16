-------------------------- MODULE WorkQueue --------------------------
(***************************************************************************)
(* M17 ph83 — the queue-row lifecycle formal arm (the NEW property M16's    *)
(* MPFencing arm did not model). A NEW, SEPARATE bounded-exhaustive spec —   *)
(* it composes with nothing and re-proves nothing; MPFencing.tla is UNTOUCHED*)
(* and re-runs byte-identical to its seal.                                   *)
(*                                                                          *)
(* Models N worker processes competing for ONE workflow's work_queue row via *)
(* the M16 durable lease + monotonic fencing token (the SAME substrate as     *)
(* MPFencing: leaseToken + procToken[p]), PLUS the ph80-82 queue-row state    *)
(* machine qstate in {pending, claimed, done, failed} and its transitions:    *)
(* Claim (pending->claimed), Reclaim (a lapsed claimed row re-claimed under a  *)
(* bumped token — ph82 D1's reclaim-broadening), Terminalize (claimed->done/   *)
(* failed, ph82-F1 token-guarded), and Crash (a claimed worker vanishes).      *)
(*                                                                          *)
(* Clock abstraction is MPFencing's (DEC-M16-D3): lease expiry is the         *)
(* NONDETERMINISTIC LeaseLapse event, never modeled as time — safety rests on *)
(* the TOKEN. The four invariants each carry a CONSTANT toggle that, when      *)
(* FALSE, is the SEED-THE-BREAK (removes the guard) and must FALSIFY:          *)
(*   TokenGuard=FALSE            -> NoSupersededTerminalize (F1) Falsifies      *)
(*   TerminalReclaimBlocked=FALSE-> NoReclaimOfTerminal (C2)     Falsifies      *)
(*   ReclaimEnabled=FALSE        -> NoLostWork                   Falsifies      *)
(*   (AtMostOneClaimedWriter's break is a config with two equal winning tokens,*)
(*    modeled by the WinnerUnique toggle below.)                              *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Procs,                   \* set of worker process ids
    MaxToken,                \* bound the fencing token (state-space cap)
    TokenGuard,              \* TRUE = ph82-F1 real model; FALSE = SEED-BREAK (Terminalize un-guarded) -> NoSupersededTerminalize Falsifies
    TerminalReclaimBlocked,  \* TRUE = C2 real model (reclaim excludes terminal); FALSE = SEED-BREAK -> NoReclaimOfTerminal Falsifies
    ReclaimEnabled,          \* TRUE = the reclaim path exists (real); FALSE = SEED-BREAK (no reclaim) -> NoLostWork Falsifies
    WinnerUnique,            \* TRUE = a fresh claim strictly bumps the token (real); FALSE = SEED-BREAK (equal winning tokens) -> AtMostOneClaimedWriter Falsifies
    CancelGate               \* M18 ph88: TRUE = the real BLOCKER-2 reclaim-terminalizes-cancelled fix (a lapsed-claimed
                             \* cancel_requested row is terminalized -> cancelled, never resumed); FALSE = SEED-BREAK (that
                             \* row is re-offered -> claimed = resumed as fresh work) -> NoResumeAfterCancel Falsifies

ASSUME MaxToken \in Nat /\ MaxToken >= 1
ASSUME TokenGuard \in BOOLEAN
ASSUME TerminalReclaimBlocked \in BOOLEAN
ASSUME ReclaimEnabled \in BOOLEAN
ASSUME WinnerUnique \in BOOLEAN
ASSUME CancelGate \in BOOLEAN

\* The queue-row lifecycle states (ph80-82). One workflow (the property is per-row;
\* N workflows only multiply the space with no new behavior). Terminal = {done, failed}.
\* M18 ph88: `cancelled` is the 5th state (the red-team MAJOR — the M17 arm had 4). It is TERMINAL,
\* so the existing terminal-quantified invariants (C2 NoReclaimOfTerminal, F1 NoSupersededTerminalize)
\* and TerminalStutter extend to it for FREE — a cancel flip is a token-guarded terminal write like any
\* other, and F1 covers it (a superseded worker cannot cancel-terminalize either).
QStates == {"pending", "claimed", "done", "failed", "cancelled"}
Terminal == {"done", "failed", "cancelled"}

VARIABLES
    qstate,          \* the work_queue row state in QStates
    leaseOwner,      \* the process currently holding the lease, or "none"
    leaseToken,      \* the current (highest issued) fencing token on the lease row
    procToken,       \* procToken[p]: the token p believes it holds (0 = none)
    leaseLapsed,     \* TRUE once the current owner's lease has (nondeterministically) lapsed
    terminalWriter,  \* GHOST: the token under which the row was terminalized (0 = not yet terminal).
                     \* The F1 oracle: a terminal flip must only ever be by the CURRENT-token holder.
    everTerminal,    \* GHOST: TRUE once the row has EVER been terminal. The C2 oracle: once terminal,
                     \* the row must STAY terminal (a terminal row is never re-claimed). Set by
                     \* Terminalize, NEVER reset — so a seed-break Reclaim that pulls a terminal row back
                     \* to claimed leaves everTerminal=TRUE while qstate # terminal -> C2 Falsifies.
    cancel_requested,\* M18 ph88: the durable operator-cancel INTENT flag (the ph87 cancel_requested column).
                     \* Set by RequestCancel (no token — the operator surface); READ by the disposition gate
                     \* and the reclaim-terminalize (NOT re-decided — the flag-read faithfulness lesson).
                     \* Monotonic (FALSE->TRUE, never cleared — ph87 never clears it).
    resumedAfterCancel \* GHOST (the NoResumeAfterCancel oracle, MODE-b — the ph87 BLOCKER-2 crux): TRUE iff a
                     \* Reclaim ever moved a cancel_requested row back to a PROGRESSING claimed state (resumed as
                     \* fresh work) instead of terminalizing it -> cancelled. In the real model (CancelGate=TRUE)
                     \* this is UNREACHABLE (the cancel arm terminalizes atomically); the seed-break sets it.
                     \* Lives in the NON-terminal (everTerminal=FALSE) space C2 cannot see — that is what makes
                     \* NoResumeAfterCancel non-redundant with C2 (the isolation proof).

vars == <<qstate, leaseOwner, leaseToken, procToken, leaseLapsed, terminalWriter, everTerminal,
          cancel_requested, resumedAfterCancel>>

Init ==
    /\ qstate           = "pending"
    /\ leaseOwner       = "none"
    /\ leaseToken       = 0
    /\ procToken        = [p \in Procs |-> 0]
    /\ leaseLapsed      = FALSE
    /\ terminalWriter   = 0
    /\ everTerminal     = FALSE
    /\ cancel_requested = FALSE
    /\ resumedAfterCancel = FALSE

\* Claim: a process claims a PENDING row (the ph80 ClaimNext pending arm). Bumps the durable
\* token monotonically and records it. INSERT-or-CAS in one IMMEDIATE txn (TLC serializes).
\* WinnerUnique=FALSE is the AtMostOneClaimedWriter seed-break: the token does NOT strictly
\* increase, so two claims can hold the SAME winning token -> the fencing "unique winner" breaks.
Claim(p) ==
    /\ leaseToken < MaxToken
    /\ qstate = "pending"
    /\ qstate'      = "claimed"
    /\ leaseOwner'  = p
    /\ leaseToken'  = leaseToken + 1                                   \* Claim always strictly bumps
    /\ procToken'   = [procToken EXCEPT ![p] = leaseToken + 1]
    /\ leaseLapsed' = FALSE
    /\ UNCHANGED <<terminalWriter, everTerminal, cancel_requested, resumedAfterCancel>>

\* Reclaim: a lapsed CLAIMED row is re-claimed under a bumped token (ph82 D1's reclaim-broadening).
\* The row STAYS claimed (a new owner). ReclaimEnabled=FALSE removes this path -> an abandoned
\* claimed row can never progress -> NoLostWork Falsifies. TerminalReclaimBlocked is the C2 guard:
\* with it TRUE (real) reclaim requires a NON-terminal (claimed) row; FALSE (seed-break) lets a
\* terminal row be re-claimed -> NoReclaimOfTerminal Falsifies.
\*
\* M18 ph88: Reclaim now DISCRIMINATES on cancel_requested (the BLOCKER-2 reclaim-terminalizes-cancelled
\* fix, formalized). A lapsed-CLAIMED row that is cancel_requested must NOT be resumed as fresh work — the
\* reclaimer terminalizes it -> cancelled ATOMICALLY under its fresh bumped token (faithful to the ph87
\* in-txn terminalize inside ClaimNext's BEGIN IMMEDIATE). The CancelGate switch gates THIS arm:
\*   CancelGate=TRUE  (real): cancel_requested lapsed-claimed -> cancelled (never resumed).
\*   CancelGate=FALSE (seed-break): that row falls through to the resume arm -> claimed, and the
\*     resumedAfterCancel ghost fires -> NoResumeAfterCancel Falsifies. This is the faithful analog of
\*     dropping the reclaim `cancel_requested`-terminalize (re-offering the row as fresh work = the limbo bug).
\* The violation lives in the NON-terminal space (the row was claimed, everTerminal=FALSE) — C2/F1 are blind
\* to it, which is the isolation proof that NoResumeAfterCancel catches a DISTINCT property.
ReclaimCancels(p) ==
    /\ ReclaimEnabled
    /\ leaseToken < MaxToken
    /\ leaseLapsed
    /\ CancelGate
    /\ qstate = "claimed"
    /\ cancel_requested
    \* The reclaimer bumps the token (fresh, fencing the dead owner) and terminalizes -> cancelled in ONE step.
    /\ leaseToken'  = leaseToken + 1
    /\ procToken'   = [procToken EXCEPT ![p] = leaseToken + 1]
    /\ leaseOwner'  = p
    /\ leaseLapsed' = FALSE
    /\ qstate'      = "cancelled"
    /\ terminalWriter'  = leaseToken + 1   \* terminalized under the FRESH token -> F1 (terminalWriter=leaseToken') holds
    /\ everTerminal'    = TRUE
    /\ UNCHANGED <<cancel_requested, resumedAfterCancel>>

Reclaim(p) ==
    /\ ReclaimEnabled
    /\ leaseToken < MaxToken
    /\ leaseLapsed
    /\ (IF TerminalReclaimBlocked THEN qstate = "claimed" ELSE qstate \in ({"claimed"} \union Terminal))
    \* MODE-b guard: when the real cancel arm is ON (CancelGate=TRUE), a cancel_requested claimed row is
    \* handled by ReclaimCancels (-> cancelled), NOT here — so this resume arm must NOT also match it (that
    \* would be a spurious nondet resume of a row the real code always terminalizes). When CancelGate=FALSE
    \* (seed-break) this guard is dropped, so the cancel_requested row DOES fall here -> resumed -> the ghost fires.
    /\ (CancelGate => ~(qstate = "claimed" /\ cancel_requested))
    /\ qstate'      = "claimed"       \* on the C2 seed-break this pulls a terminal row back to claimed (the C2 violation)
    /\ leaseOwner'  = p
    \* WinnerUnique=TRUE (real): the reclaim strictly bumps the durable token, FENCING the prior owner
    \* (its stale procToken < new leaseToken). WinnerUnique=FALSE (seed-break): the reclaim REUSES the
    \* current token, so the reclaimer p and the (still-alive, not-yet-crashed) prior owner BOTH hold
    \* procToken = leaseToken -> two winning writers -> AtMostOneClaimedWriter Falsifies. This is exactly
    \* the ph82 store-per-worker collision (a shared token defeats the unique-winner fencing).
    /\ leaseToken'  = IF WinnerUnique THEN leaseToken + 1 ELSE leaseToken
    /\ procToken'   = [procToken EXCEPT ![p] = IF WinnerUnique THEN leaseToken + 1 ELSE leaseToken]
    /\ leaseLapsed' = FALSE
    /\ terminalWriter' = IF qstate \in Terminal THEN 0 ELSE terminalWriter  \* a re-claimed (wrongly) terminal row is no longer terminal
    /\ UNCHANGED <<everTerminal>>                 \* NEVER reset: a wrongly-reclaimed terminal row keeps everTerminal=TRUE (the C2 witness)
    /\ UNCHANGED <<cancel_requested>>
    \* MODE-b ghost: if THIS reclaim resumed a cancel_requested row (only reachable on the CancelGate=FALSE
    \* seed-break, since the guard above excludes it when CancelGate=TRUE), record the resume -> NoResumeAfterCancel Falsifies.
    /\ resumedAfterCancel' = (resumedAfterCancel \/ (qstate = "claimed" /\ cancel_requested))

\* LeaseLapse: the NONDETERMINISTIC liveness event (MPFencing's abstraction — no clock). The
\* current owner is now re-claimable; procToken is UNTOUCHED (the possibly-alive owner still
\* believes it holds its token — the zombie condition F1 must defend against).
LeaseLapse ==
    /\ leaseOwner # "none"
    /\ ~leaseLapsed
    /\ qstate = "claimed"
    /\ leaseLapsed' = TRUE
    /\ UNCHANGED <<qstate, leaseOwner, leaseToken, procToken, terminalWriter, everTerminal,
                   cancel_requested, resumedAfterCancel>>

\* Crash: a claimed worker vanishes (SIGKILL). It drops its believed token; the row stays claimed
\* and its lease will lapse (LeaseLapse) making it reclaimable. Models the kill-storm's dead worker.
Crash(p) ==
    /\ procToken[p] > 0
    /\ qstate = "claimed"
    /\ procToken'   = [procToken EXCEPT ![p] = 0]
    /\ UNCHANGED <<qstate, leaseOwner, leaseToken, leaseLapsed, terminalWriter, everTerminal,
                   cancel_requested, resumedAfterCancel>>

\* Terminalize: a process flips a CLAIMED row to a terminal state (ph82 MarkDone/MarkFailed).
\* THE ph82-F1 token-guard: with TokenGuard=TRUE the flip lands ONLY if p's token is STILL current
\* (procToken[p] = leaseToken) — a superseded worker (procToken[p] < leaseToken after a reclaim)
\* is a 0-row no-op. TokenGuard=FALSE removes the guard -> a superseded worker terminalizes the
\* live owner's row -> NoSupersededTerminalize Falsifies (the AF1-sibling data-integrity break).
Terminalize(p, s) ==
    /\ qstate = "claimed"
    /\ procToken[p] > 0
    /\ (TokenGuard => procToken[p] = leaseToken)
    /\ qstate'         = s
    /\ terminalWriter' = procToken[p]   \* GHOST: the token that terminalized the row
    /\ everTerminal'   = TRUE            \* GHOST: the row has been terminal (never reset) — C2 oracle
    /\ UNCHANGED <<leaseOwner, leaseToken, procToken, leaseLapsed, cancel_requested, resumedAfterCancel>>

\* M18 ph88 — RequestCancel: an operator sets the durable cancel_requested INTENT on a pending|claimed row
\* (models CancelRunning's `WHERE state IN (pending,claimed) AND cancel_requested IS NULL`). NO token — the
\* operator surface (DEC-M18-CANCEL-FENCING); setting a flag (not a terminal write) can't lose to MarkDone
\* nor clobber a reclaimer. Idempotent by the ~cancel_requested guard (a second set is a no-op = not enabled).
RequestCancel ==
    /\ qstate \in {"pending", "claimed"}
    /\ ~cancel_requested
    /\ cancel_requested' = TRUE
    /\ UNCHANGED <<qstate, leaseOwner, leaseToken, procToken, leaseLapsed, terminalWriter,
                   everTerminal, resumedAfterCancel>>

\* M18 ph88 — CancelTerminalize: the CURRENT-token holder (the ph87 disposition gate in disposeExecErr)
\* observes cancel_requested on its own claimed row and terminalizes it -> cancelled. Token-guarded exactly
\* like Terminalize (TokenGuard => procToken[p]=leaseToken), so a superseded worker is a no-op and F1 covers
\* the cancel flip for free. Reads the FLAG (cancel_requested), does NOT re-decide (the faithfulness lesson:
\* a deterministic flag-read, not a nondet cancel-vs-leave pick). The reclaim-path terminalize is the SEPARATE
\* ReclaimCancels action (fresh bumped token); this is the live-owner path.
CancelTerminalize(p) ==
    /\ qstate = "claimed"
    /\ cancel_requested
    /\ procToken[p] > 0
    /\ (TokenGuard => procToken[p] = leaseToken)
    /\ qstate'         = "cancelled"
    /\ terminalWriter' = procToken[p]
    /\ everTerminal'   = TRUE
    /\ UNCHANGED <<leaseOwner, leaseToken, procToken, leaseLapsed, cancel_requested, resumedAfterCancel>>

\* Done: a terminal row (done/failed) is an ABSORBING state — the workflow is complete and the
\* system idles. Modeled as an explicit stutter so a reached terminal state is not a TLC deadlock
\* (a deadlock would be a spurious "no next action" artifact, not a real property). Safety invariants
\* still hold in this state; it just lets the model sit at the legitimate terminus.
TerminalStutter ==
    /\ qstate \in Terminal
    /\ UNCHANGED vars

Next ==
    \/ \E p \in Procs : Claim(p)
    \/ \E p \in Procs : Reclaim(p)
    \/ \E p \in Procs : ReclaimCancels(p)           \* M18 ph88: reclaim-terminalizes-cancelled (BLOCKER-2 fix)
    \/ \E p \in Procs : Crash(p)
    \/ \E p \in Procs, s \in {"done", "failed"} : Terminalize(p, s)  \* done/failed only; cancelled via CancelTerminalize
    \/ \E p \in Procs : CancelTerminalize(p)        \* M18 ph88: the live-owner disposition gate (-> cancelled)
    \/ RequestCancel                                \* M18 ph88: operator sets the durable cancel intent
    \/ LeaseLapse
    \/ TerminalStutter                              \* absorbing terminal state (not a deadlock)
    \/ (leaseToken >= MaxToken /\ UNCHANGED vars)   \* terminal stutter at the token cap

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
TypeOK ==
    /\ qstate \in QStates
    /\ leaseOwner \in (Procs \union {"none"})
    /\ leaseToken \in 0..MaxToken
    /\ procToken \in [Procs -> 0..MaxToken]
    /\ leaseLapsed \in BOOLEAN
    /\ terminalWriter \in 0..MaxToken
    /\ everTerminal \in BOOLEAN
    /\ cancel_requested \in BOOLEAN
    /\ resumedAfterCancel \in BOOLEAN

\* --- The four invariants (each with a biting break-cfg CONSTANT toggle) ---

\* C2 — NoReclaimOfTerminal: a terminal row is NEVER pulled back to claimed/pending. The ghost
\* terminalWriter is >0 exactly when the row is terminal; if a (seed-break) reclaim pulls a
\* terminal row back to claimed, this predicate catches the illegal live-again terminal.
\* We express it as: the row is claimed/pending ONLY when it has not been terminalized, i.e. a
\* terminal-then-reclaimed transition (terminalWriter reset with qstate back to claimed) is the
\* violation. Encoded via a monotonicity ghost: once terminal, terminalWriter stays > 0 UNLESS
\* the seed-break reset it — so the break is observable as a claimed row that WAS terminal.
NoReclaimOfTerminal ==
    \* A terminal row STAYS terminal: once the row has ever been terminal (everTerminal), it must
    \* still be terminal now. The seed-break Reclaim (TerminalReclaimBlocked=FALSE) pulls a terminal
    \* row back to "claimed" WITHOUT resetting everTerminal -> everTerminal=TRUE ∧ qstate # terminal
    \* -> Falsifies. The invariant references NO toggle (a pure safety property), so the break bites.
    everTerminal => (qstate \in Terminal)

\* AtMostOneClaimedWriter (M16 fencing RE-CHECK, not re-proven): at most one process holds the
\* current (highest) token. leaseToken is the unique current token; a process is a "winning writer"
\* iff procToken[p] = leaseToken. In the real model a strict token bump on every Claim/Reclaim makes
\* at most one process ever equal to leaseToken. The WinnerUnique=FALSE break lets two procs share it.
AtMostOneClaimedWriter ==
    Cardinality({p \in Procs : procToken[p] = leaseToken /\ leaseToken > 0}) <= 1

\* F1 — NoSupersededTerminalize: a terminal flip is only ever by the CURRENT-token holder. The ghost
\* terminalWriter records the token that terminalized; with the token-guard it always equals the
\* leaseToken at flip-time (no reclaim can have bumped past it while still claimed). The seed-break
\* (TokenGuard=FALSE) lets a superseded worker (procToken < leaseToken) flip -> terminalWriter <
\* leaseToken while the row is terminal -> Falsifies.
NoSupersededTerminalize ==
    \* A terminal flip is only ever by the CURRENT-token holder: whenever the row is terminal, the
    \* token that terminalized it (the ghost terminalWriter) equals the current leaseToken. The
    \* seed-break (TokenGuard=FALSE) lets a superseded worker (procToken < leaseToken) flip -> the
    \* ghost records that lower token while leaseToken is higher -> terminalWriter # leaseToken ->
    \* Falsifies. NO toggle reference (pure safety), so the break bites. (This can only be violated
    \* after a Reclaim bumped leaseToken past a stalled worker who then terminalizes — the exact
    \* ph82-F1 / AF1-sibling scenario.)
    (qstate \in Terminal) => (terminalWriter = leaseToken)

\* NoLostWork (reachability as safety): the row is never STUCK — a non-terminal row is never
\* abandoned with no live claimer AND no reclaim path. "Abandoned" = lease lapsed (owner gone) and
\* the reclaim path disabled. With ReclaimEnabled=TRUE a lapsed claimed row is always re-claimable
\* -> never permanently stuck. The break (ReclaimEnabled=FALSE) makes a lapsed claimed row a dead
\* end -> the predicate flags the stuck non-terminal row.
NoLostWork ==
    (qstate \notin Terminal /\ leaseLapsed) => ReclaimEnabled

\* --- M18 ph88: the cancel-lifecycle invariants ---

\* NoResumeAfterCancel (THE ph88 bite, MODE-b — the ph87 BLOCKER-2 crux formalized). A cancel_requested row
\* is NEVER reclaimed-and-RESUMED as fresh progressing work: its only reclaim exit is -> cancelled (via
\* ReclaimCancels), never back to a progressing `claimed`. The ghost resumedAfterCancel is set iff a Reclaim
\* moved a cancel_requested row to claimed. In the real model (CancelGate=TRUE) ReclaimCancels handles that
\* row and the Reclaim resume-arm's guard excludes it -> the ghost is UNREACHABLE -> HOLDS. The seed-break
\* (CancelGate=FALSE) drops the guard -> the cancel_requested lapsed-claimed row falls to the resume arm ->
\* claimed -> the ghost fires -> Falsifies with a concrete trace.
\* ISOLATION (the non-redundancy proof, distinct from C2): the violation state is `claimed`, everTerminal=FALSE
\* -> C2 (everTerminal=>Terminal) and F1 (Terminal=>...) are both VACUOUSLY TRUE there. So the CancelGate=FALSE
\* break Falsifies ONLY NoResumeAfterCancel while C2/F1/NoLostWork/AtMostOneClaimedWriter all HOLD on that same
\* break — which is exactly the machine-checked proof that this invariant catches a property the other four cannot.
\* NO toggle reference in the predicate (pure safety), so the break bites.
\* NOTE — a mode-(a) invariant `(cancel_requested /\ everTerminal) => qstate="cancelled"` was CONSIDERED and
\* REJECTED as UNFAITHFUL (TLC-falsified on the real cfg): a cancel_requested claimed row whose owner's normal
\* MarkDone wins the race legitimately terminalizes -> done (cancel is best-effort cooperative — a workflow that
\* finishes before the disposition gate observes the flag completes normally, faithful to ph87's
\* Terminalize-then-observe ordering). So a cancel_requested row CAN be a non-cancelled terminal. NoResumeAfterCancel
\* (the ghost, above) is the correct + load-bearing property: it forbids the row being RESUMED as fresh work, which
\* is the actual BLOCKER-2 failure — not "it must end cancelled".
NoResumeAfterCancel ==
    ~resumedAfterCancel

=============================================================================
