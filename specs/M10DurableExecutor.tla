---------------------- MODULE M10DurableExecutor ----------------------
(***************************************************************************)
(* Formal model of M10 chunk-1 durable suspend/re-enter (Tier 2: durable     *)
(* continuations — the MINIMAL core). Refines the M9 DurableExecutor model    *)
(* (specs/DurableExecutor.tla, left BYTE-UNCHANGED — DESIGN-M9 §8 / DEC-M10    *)
(* sanction a separate refining module) by adding ONE non-terminal status and  *)
(* the suspend/wake layer that rides the existing durable seam:                *)
(*                                                                            *)
(*   - A declared SUSPENSION node (n in Suspendable — a static code property,  *)
(*     never persisted) that runs while its wake event has NOT fired PARKS:    *)
(*     status becomes the new non-terminal "waiting" (the action returned       *)
(*     ErrSuspended). Its level drains to the barrier and the M9 Checkpoint     *)
(*     flushes the snapshot CARRYING the waiting status (Running = {} holds      *)
(*     with a node waiting). "Suspend is a crash you chose."                    *)
(*   - FireEvent models the external timer/signal arriving (boolean-per-node    *)
(*     logical time, monotone). It is UNFAIR + bounded (like Crash) and DURABLE *)
(*     (survives a crash), so the strong drain liveness is CONDITIONAL on event *)
(*     arrival — never an engine claim.                                         *)
(*   - Wake re-enters a ready waiter (status waiting -> pending), the M9 resume  *)
(*     path: the executor re-runs it and it now completes. Wake is an ENGINE     *)
(*     obligation (weak-fair, like Recover).                                     *)
(*                                                                            *)
(* The load-bearing anti-vacuity device is the WakeReady-CONDITIONED Stuck arm: *)
(* a waiting node with its event fired is Stuck (the engine MUST wake it), while *)
(* a waiting node whose event has NOT fired RESTS legitimately (a wait for a     *)
(* signal that may never come is not a liveness violation). This is the same     *)
(* conditioned-Stuck discipline the team already uses for pending-eligible vs     *)
(* pending-blocked — it gives <>[]Settled real teeth on the wake machinery        *)
(* without making "park forever waiting for an event" a failure.                  *)
(*                                                                            *)
(* MINIMAL scope (phase 35): Waiting + the conditioned Stuck arm + Suspend/Wake/ *)
(* FireEvent + WaitingSound + WokeOnlyWhenReady, with all M9 safety RETAINED and  *)
(* re-checked under suspend. The exhaustive crash×suspend composition + the 5     *)
(* signal/timer invariants are the phase-39 capstone; the minimal config runs     *)
(* MaxCrashes = 0 (the durable machinery is present for 39 to turn up).           *)
(*                                                                            *)
(* FAITHFULNESS (superset argument, as in M9): this is a per-node scheduling     *)
(* model — an independent branch may progress while another branch is parked. The *)
(* Go executor parks the WHOLE run at the level barrier (Model A), a STRICTER     *)
(* schedule. The model's behaviors are therefore a SUPERSET of the code's; a      *)
(* safety/liveness property proven over the superset holds for the code.          *)
(***************************************************************************)
EXTENDS FiniteSets, Naturals

CONSTANTS
    Nodes,           \* set of node ids
    Deps,            \* dependency relation: <<d, n>> in Deps  <=>  n depends on d
    ContinueOnError, \* subset of Nodes flagged continue-on-error
    FailSet,         \* subset of Nodes whose action fails when it runs
    MaxConc,         \* max concurrent running nodes (>= 1)
    MaxCrashes,      \* max number of process crashes to explore (>= 0)
    Suspendable,     \* subset of Nodes that may park (declared suspension nodes)
    TimerNodes,      \* subset of Suspendable driven by a DURABLE TIMER (clock>=fireAt), as
                     \* opposed to a signal (FireEvent). The phase-36 timer dimension.
    MaxTick,         \* logical-clock ceiling (clock in 0..MaxTick) — bounded discrete time
    FireAt,          \* FireAt[n]: the absolute logical due-time of a timer node (in 0..MaxTick)
    \* --- M11 OR-join topology (phase 44). ALL default to {} / trivial for the M10
    \* diamond config, so the OR-join arm is INERT and M10 re-runs byte-behaviour-
    \* unchanged (preservation-by-re-verification, DEC-M11-P44-PRESERVE). ---
    ChoiceNodes,     \* subset of Nodes that are ChoiceNodes (first-match router; abstracted to a nondet pick)
    ChoiceFailSet,   \* subset of ChoiceNodes that FAIL to route (no branch matched + no default -> ErrNoBranchMatched,
                     \* choice.go:52-59): the choice FAILS (non-coe), its branch entries are left UNTOUCHED and the
                     \* cascade SKIPS them (never bypasses) — the 41-F1 distinction. Default {} (choices route).
    MergeNodes,      \* subset of Nodes that are MergeNodes (OR-join)
    ChoiceBranches,  \* [ChoiceNodes -> SUBSET Nodes]: the branch-entry nodes each Choice picks among
    ChosenBranch,    \* [ChoiceNodes -> Nodes]: the DETERMINISTIC branch each Choice takes. Models the
                     \* first-match predicate as a PURE FUNCTION of checkpointed seed keys (CHOICE-02
                     \* same-branch-on-resume) — a durable-by-data pick, like a timer's FireAt. A nondet
                     \* pick would be UNFAITHFUL under crashes: it could re-route on resume (run a branch,
                     \* then bypass it after a crash reverts an un-checkpointed decision -> exec>0 for a
                     \* bypassed node), violating the resume-determinism ph43 §1 proved. Explore multiple
                     \* routes by running the config once per ChosenBranch value.
    MergeTails       \* [MergeNodes -> SUBSET Nodes]: the branch-TAIL predecessors the merge OR-joins (the
                     \* count set). The structural always-`done` Choice-dep is a dep of the merge but is
                     \* NOT in MergeTails, so it is EXCLUDED from the taken count (anti-vacuity, MAJOR-1)

VARIABLES
    status,    \* status[n]: in-memory status in {pending,running,done,failed,skipped,waiting}
    halted,    \* TRUE once a hard (non-coe) node has failed (in memory)
    journal,   \* journal[n]: the DURABLE persisted status (the result journal)
    exec,      \* exec[n]: how many times n's action has actually executed
    up,        \* process alive? FALSE between Crash and Recover
    crashes,   \* number of crashes so far (bounds the search)
    wakeReady, \* wakeReady[n]: has n's external wake event (timer due / signal) fired? (durable, monotone)
    clock,     \* logical wall clock (0..MaxTick); advanced by Tick, durable across Crash
    fireCount, \* fireCount[n]: number of times n's timer has FIRED (for NoDoubleFire; <= 1)
    mailbox,   \* mailbox[n]: a signal is delivered and UNACKED for n (phase-37 durable channel,
               \* SEPARATE from status/journal — survives Crash; cleared only by Ack)
    delivered, \* delivered[n]: a signal was EVER delivered for n (monotone, never cleared by Ack;
               \* durable). NoSignalLost keys off this: an apply implies a real delivery.
    applied,   \* applied[n]: how many times n's signal was CONSUMED/applied (count; for NoSignalLost)
    recorded   \* recorded[n]: the RECORDED VALUE of n's applied signal — 0 (unset) or ApplyVal (the
               \* one canonical value). The apply OVERWRITES to ApplyVal (set-not-accumulate), so it
               \* stays ApplyVal across any number of crash re-applies. NoDoubleApply (restated, ph39)
               \* is observable-idempotence over THIS value, not the apply count. (ph39 F3 closure.)

\* ApplyVal is the single canonical value a signal apply records — a constant, NOT a
\* counter. An idempotent apply sets recorded[n] := ApplyVal every time; the recorded
\* value is therefore invariant across re-applies. A should-fail mutation that
\* ACCUMULATES (recorded := recorded + ApplyVal) drives recorded off ApplyVal and
\* falsifies NoDoubleApply — the anti-vacuity teeth.
ApplyVal == 1

vars == <<status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* A node may run up to twice per (crash-reverted) attempt: once to park, once to
\* complete after waking. Bounds exec finitely.
MaxRuns == 2 * (MaxCrashes + 1)

\* 7th status "bypassed" (M11 ph41/44): a not-taken ChoiceNode branch. Terminal
\* (mirrors Bypassed in isTerminalStatus), distinct from "skipped" (which means an
\* upstream you needed failed). For an AND node a bypassed dep is neither Resolved
\* nor a SkipCause — it is a distinct BYPASS cause (the Bypass/DiamondSkip actions);
\* for a MergeNode a bypassed predecessor is SATISFIED (MergeResolved), not blocking.
Statuses == {"pending","running","done","failed","skipped","waiting","bypassed"}

------------------------------------------------------------------------
(* Base scheduling helpers — mirror DurableExecutor.tla / Executor.tla.      *)

Terminal == {"done", "failed", "skipped", "bypassed"}

DepsOf(n) == { d \in Nodes : <<d, n>> \in Deps }

Running == { n \in Nodes : status[n] = "running" }

Resolved(d) == \/ status[d] = "done"
               \/ (d \in ContinueOnError /\ status[d] = "failed")

DepsResolved(n) == \A d \in DepsOf(n) : Resolved(d)

SkipCause(d) == \/ status[d] = "skipped"
                \/ (status[d] = "failed" /\ d \notin ContinueOnError)

HasSkipCauseDep(n) == \E d \in DepsOf(n) : SkipCause(d)

HardFailedIn(s) == \E n \in Nodes : s[n] = "failed" /\ n \notin ContinueOnError

------------------------------------------------------------------------

TypeOK ==
    /\ status    \in [Nodes -> Statuses]
    /\ halted    \in BOOLEAN
    /\ journal   \in [Nodes -> Statuses]
    /\ exec      \in [Nodes -> 0..MaxRuns]
    /\ up        \in BOOLEAN
    /\ crashes   \in 0..MaxCrashes
    /\ wakeReady \in [Nodes -> BOOLEAN]
    /\ clock     \in 0..MaxTick
    /\ fireCount \in [Nodes -> 0..MaxRuns]
    /\ mailbox   \in [Nodes -> BOOLEAN]
    /\ delivered \in [Nodes -> BOOLEAN]
    /\ applied   \in [Nodes -> 0..MaxRuns]
    /\ recorded  \in [Nodes -> 0..(ApplyVal * MaxRuns)]

Init ==
    /\ status    = [n \in Nodes |-> "pending"]
    /\ halted    = FALSE
    /\ journal   = [n \in Nodes |-> "pending"]
    /\ exec      = [n \in Nodes |-> 0]
    /\ up        = TRUE
    /\ crashes   = 0
    /\ wakeReady = [n \in Nodes |-> FALSE]
    /\ clock     = 0
    /\ fireCount = [n \in Nodes |-> 0]
    /\ mailbox   = [n \in Nodes |-> FALSE]
    /\ delivered = [n \in Nodes |-> FALSE]
    /\ applied   = [n \in Nodes |-> 0]
    /\ recorded  = [n \in Nodes |-> 0]

------------------------------------------------------------------------
(* RUN actions (require the process to be up). *)

Start(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ ~halted
    /\ n \notin MergeNodes   \* M11: a MergeNode launches via MergeStart (OR-join), not the strict-AND Start
    /\ DepsResolved(n)
    /\ Cardinality(Running) < MaxConc
    /\ status' = [status EXCEPT ![n] = "running"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

(* Finish: a running node's action runs to completion. A SUSPENDABLE node may   *)
(* COMPLETE here only once its wake event has fired; while ~wakeReady it parks   *)
(* (Suspend), never completing. (Suspendable nodes do not fail — the config      *)
(* keeps FailSet disjoint from Suspendable, modelling Timer/Signal nodes.)       *)
Finish(n) ==
    /\ up
    /\ status[n] = "running"
    /\ n \notin ChoiceNodes   \* M11: a ChoiceNode completes via ChoiceFinish (activates one branch, bypasses the rest)
    /\ (n \in Suspendable => wakeReady[n])
    /\ exec' = [exec EXCEPT ![n] = exec[n] + 1]
    /\ IF n \in FailSet
         THEN /\ status' = [status EXCEPT ![n] = "failed"]
              /\ halted' = (halted \/ (n \notin ContinueOnError))
         ELSE /\ status' = [status EXCEPT ![n] = "done"]
              /\ UNCHANGED halted
    /\ UNCHANGED <<journal, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

(* Suspend: a running suspendable node whose wake event has NOT fired PARKS —    *)
(* the non-terminal "waiting" status (the action ran and returned ErrSuspended,  *)
(* so exec++). Only a declared suspension node may park (static topology /        *)
(* DEC-M10-mechanism), enforced by the guard and witnessed by WaitingSound.      *)
Suspend(n) ==
    /\ up
    /\ status[n] = "running"
    /\ n \in Suspendable
    /\ ~wakeReady[n]
    /\ status' = [status EXCEPT ![n] = "waiting"]
    /\ exec' = [exec EXCEPT ![n] = exec[n] + 1]
    /\ UNCHANGED <<halted, journal, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

(* SendSignal: the external SIGNAL is DELIVERED to a non-timer suspendable node's   *)
(* durable mailbox (the phase-37 enqueue, D37-03). UNFAIR + monotone (~delivered    *)
(* guard fires it once) — the environment may or may not deliver, so the strong     *)
(* drain liveness is conditional on arrival. DURABLE: mailbox + delivered are       *)
(* UNCHANGED across Crash/Recover (the mailbox survives a crash). It sets BOTH       *)
(* mailbox (delivered-and-unacked) and delivered (the monotone ever-delivered record *)
(* NoSignalLost keys off). It does NOT touch status/wakeReady — delivery is decoupled *)
(* from consumption (an early signal is buffered before the node is even reached).    *)
(* Timer nodes are EXCLUDED — their wake is FireTimer (clock>=FireAt), not a signal.  *)
SendSignal(n) ==
    /\ up
    /\ n \in Suspendable
    /\ n \notin TimerNodes
    /\ ~delivered[n]
    /\ mailbox'   = [mailbox   EXCEPT ![n] = TRUE]
    /\ delivered' = [delivered EXCEPT ![n] = TRUE]
    /\ UNCHANGED <<status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount, applied, recorded>>

(* Consume: a signal node TAKES its mailbox and APPLIES the payload — the take→apply  *)
(* step (D37-04). Enabled while the node is running or parked (waiting) AND its signal *)
(* is in the mailbox AND it has not already consumed (~wakeReady). It records the     *)
(* apply (applied++) and sets the wake condition the existing Finish/Wake machinery   *)
(* uses (wakeReady). The ~wakeReady guard makes the apply HAPPEN AT MOST ONCE         *)
(* (idempotent, D37-05); dropping it re-applies and NoDoubleApply FALSIFIES. The      *)
(* mailbox guard makes an apply IMPLY a real delivery; dropping it conjures an apply  *)
(* with no delivery and NoSignalLost FALSIFIES. Weak-fair: a delivered signal IS      *)
(* eventually consumed (an engine obligation, like Wake).                            *)
Consume(n) ==
    /\ up
    /\ n \in Suspendable
    /\ n \notin TimerNodes
    /\ status[n] \in {"running", "waiting"}
    /\ mailbox[n]
    /\ ~wakeReady[n]
    /\ wakeReady' = [wakeReady EXCEPT ![n] = TRUE]
    /\ applied'   = [applied   EXCEPT ![n] = applied[n] + 1]
    \* Record the applied VALUE by OVERWRITE (set-not-accumulate) — every apply, incl.
    \* a crash re-apply, writes the SAME ApplyVal, so recorded[n] is invariant. This is
    \* the observable-idempotence the restated NoDoubleApply asserts. (ph39 F3.)
    /\ recorded'  = [recorded  EXCEPT ![n] = ApplyVal]
    /\ UNCHANGED <<status, halted, journal, exec, up, crashes, clock, fireCount, mailbox, delivered>>

(* Ack: drain the consumed signal from the mailbox — ONLY after the consuming        *)
(* completion is DURABLE (journal[n] = "done"), the take→apply→Completed→checkpoint→  *)
(* ack ordering (D37-04). Acking after durability is what makes a crash before the    *)
(* checkpoint re-apply (the signal still in the mailbox) rather than lose the signal. *)
(* It clears mailbox but NOT delivered (the ever-delivered record is permanent).      *)
Ack(n) ==
    /\ up
    /\ n \in Suspendable
    /\ n \notin TimerNodes
    /\ journal[n] = "done"
    /\ mailbox[n]
    /\ mailbox' = [mailbox EXCEPT ![n] = FALSE]
    /\ UNCHANGED <<status, halted, journal, exec, up, crashes, wakeReady, clock, fireCount, delivered, applied, recorded>>

(* Wake: a waiting node whose event has fired RE-ENTERS (status -> pending) — the *)
(* M9 resume path re-entered. The executor then re-runs it, and it completes      *)
(* (wakeReady holds). Wake is an ENGINE OBLIGATION (weak-fair). WokeOnlyWhenReady *)
(* proves a same-process wake never happens without the event having fired.       *)
Wake(n) ==
    /\ up
    /\ status[n] = "waiting"
    /\ wakeReady[n]
    /\ status' = [status EXCEPT ![n] = "pending"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

Skip(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ HasSkipCauseDep(n)
    /\ status' = [status EXCEPT ![n] = "skipped"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

------------------------------------------------------------------------
(* M11 OR-JOIN actions (phase 44 — the ChoiceNode + MergeNode arm).           *)
(* All are INERT when ChoiceNodes = MergeNodes = {} (the M10 diamond config),  *)
(* so M10 re-runs unchanged (DEC-M11-P44-PRESERVE). Models parallel_execution  *)
(* .go:140-161 (the cause-aware gate + the OR-join launch-eligibility count).  *)

AllDepsSettled(n) == \A d \in DepsOf(n) : status[d] \in Terminal
HasBypassDep(n)   == \E d \in DepsOf(n) : status[d] = "bypassed"

\* MergeResolved: for a MergeNode a Bypassed predecessor is SATISFIED (does not
\* block the OR-join) — the role-aware inverse of the strict-AND Resolved. (done /
\* coe-failed are the taken cases, already Resolved.)  [depResolved, merge arm]
MergeResolved(d)      == Resolved(d) \/ status[d] = "bypassed"
MergeDepsSatisfied(m) == \A d \in DepsOf(m) : MergeResolved(d)
\* TakenTails: the OR-join count set — branch tails that RAN (Resolved), ranging
\* over MergeTails ONLY, so the always-`done` structural Choice-dep is EXCLUDED
\* (anti-vacuity, red-team MAJOR-1). A Bypassed tail is satisfied but NOT taken.
TakenTails(m)         == { t \in MergeTails[m] : Resolved(t) }

\* ChoiceFinish (ph41): a running ChoiceNode COMPLETES (done — a Choice always
\* Completes, D38-01) and activates EXACTLY ONE branch (ChosenBranch[c]): it marks
\* every OTHER branch entry "bypassed". The pick is DETERMINISTIC (ChosenBranch, a
\* pure function of checkpointed seed keys) so it is STABLE across a crash/resume
\* (CHOICE-02 same-branch-on-resume). ABSTRACTION (documented, M9/M10 honesty): the
\* declared-order first-match (DEC-M11-FIRSTMATCH) is modeled as a fixed per-choice
\* pick — the routing DATA is irrelevant to the SAFETY invariants (exactly-one-taken,
\* bypass propagation, the separator); it is the exactly-one-ness + resume-stability
\* that matter, not which branch. Different routes are separate exhaustive runs.
ChoiceFinish(c) ==
    /\ up
    /\ c \in ChoiceNodes
    /\ c \notin ChoiceFailSet   \* a no-match choice FAILS instead (ChoiceFail); it does not route
    /\ status[c] = "running"
    /\ exec' = [exec EXCEPT ![c] = exec[c] + 1]
    /\ status' = [nn \in Nodes |->
                    IF nn = c THEN "done"
                    ELSE IF nn \in (ChoiceBranches[c] \ {ChosenBranch[c]}) THEN "bypassed"
                    ELSE status[nn]]
    /\ UNCHANGED <<halted, journal, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* ChoiceFail (ph41 / 41-F1, phase-44 review 44-F1): a ChoiceNode with NO matching
\* branch and NO default FAILS (non-coe -> halts the attempt) and leaves its branch
\* entries UNTOUCHED (choice.go:52-59 does NOT bypass on the error path). The cascade
\* then SKIPS the branches (a failed non-coe choice-dep is a SkipCause -> the existing
\* Skip fires -> "skipped", NEVER "bypassed"). This is the crux the milestone protects:
\* a FAILED route is distinguishable from a BYPASSED (not-taken) route AT the branches.
\* ChoiceFailureSkipsNotBypasses is the safety witness; the bite is letting ChoiceFail
\* bypass its branches (mislabel a routing failure as a clean bypass — the 41-F1 defect).
ChoiceFail(c) ==
    /\ up
    /\ c \in ChoiceFailSet
    /\ status[c] = "running"
    /\ exec'   = [exec   EXCEPT ![c] = exec[c] + 1]
    /\ status' = [status EXCEPT ![c] = "failed"]      \* branches left untouched (Pending) -> cascade Skips them
    /\ halted' = (halted \/ (c \notin ContinueOnError))
    /\ UNCHANGED <<journal, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* Bypass (classifyBlockedStatus rule 4): a non-merge node blocked SOLELY by
\* bypassed dep(s) — all deps settled, >=1 bypassed, NONE a skip-cause, NONE
\* resolved (no surviving taken ancestor) — is itself bypassed (a not-taken branch
\* interior). Skip (rule 1) dominates a failure cascade; DiamondSkip (rule 3) a
\* surviving taken ancestor — the three are mutually exclusive by their guards.
Bypass(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ n \notin MergeNodes
    /\ AllDepsSettled(n)
    /\ HasBypassDep(n)
    /\ ~HasSkipCauseDep(n)
    /\ ~(\E d \in DepsOf(n) : Resolved(d))
    /\ status' = [status EXCEPT ![n] = "bypassed"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* DiamondSkip (rule 3 / D-03 / DEC-M11-P41-DIAMOND): a non-merge node with a
\* bypassed dep AND a surviving taken (Resolved) ancestor is SKIPPED, not bypassed
\* (the taken-path ancestor wins).
DiamondSkip(n) ==
    /\ up
    /\ status[n] = "pending"
    /\ n \notin MergeNodes
    /\ AllDepsSettled(n)
    /\ HasBypassDep(n)
    /\ ~HasSkipCauseDep(n)
    /\ \E d \in DepsOf(n) : Resolved(d)
    /\ status' = [status EXCEPT ![n] = "skipped"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* MergeStart (ph42 launch-eligibility): a Merge is launch-eligible once ALL its
\* predecessors are resolved-for-merge (a bypassed tail SATISFIES). It FIRES (runs,
\* then Finish -> done) iff >=1 TAKEN branch-tail (over MergeTails, Choice-dep
\* EXCLUDED). A failed(non-coe)/skipped tail is NOT resolved-for-merge, so the merge
\* is not launch-eligible and Skip fires instead (fail-fast — BypassVsFailureSeparator).
MergeStart(m) ==
    /\ up
    /\ m \in MergeNodes
    /\ status[m] = "pending"
    /\ ~halted
    /\ MergeDepsSatisfied(m)
    /\ TakenTails(m) # {}
    /\ Cardinality(Running) < MaxConc
    /\ status' = [status EXCEPT ![m] = "running"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

\* MergeBypass (ph42 MH-2): a Merge whose every branch-tail is bypassed (0 taken) is
\* itself bypassed (composes downward — the nested all-bypassed case).
MergeBypass(m) ==
    /\ up
    /\ m \in MergeNodes
    /\ status[m] = "pending"
    /\ MergeDepsSatisfied(m)
    /\ TakenTails(m) = {}
    \* NOTE (superset abstraction, documented): MergeBypass is NOT gated on ~halted.
    \* MergeDepsSatisfied already excludes any merge with a failed/skipped tail (a
    \* skip-cause tail is not MergeResolved), so a failure of the merge's OWN branch
    \* never reaches here — it Skips (fail-fast, the separator). Only an all-bypassed
    \* merge reaches MergeBypass, which in a single-Choice topology implies no failure
    \* (hence no halt) anyway. On an UNRELATED halt the Go leaves a clean-dep merge
    \* Pending (markSkippedFrom); the model may bypass it — a SUPERSET behavior, safe
    \* for safety proofs. Gating on ~halted here would MASK the separator bite (a
    \* failed-tail scenario is always halted), hiding the anti-vacuity teeth.
    /\ status' = [status EXCEPT ![m] = "bypassed"]
    /\ UNCHANGED <<halted, journal, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

------------------------------------------------------------------------
(* DURABILITY actions (inherited from the M9 model). *)

(* Checkpoint at a quiescent barrier (Running = {}). A node may be "waiting" at   *)
(* the barrier, so the persisted snapshot faithfully carries the waiting status   *)
(* (the M10 park-flush). Same sound-superset argument as M9.                      *)
Checkpoint ==
    /\ up
    /\ Running = {}
    /\ journal # status
    /\ journal' = status
    /\ UNCHANGED <<status, halted, exec, up, crashes, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

Crash ==
    /\ up
    /\ crashes < MaxCrashes
    /\ up' = FALSE
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<status, halted, journal, exec, wakeReady, clock, fireCount, mailbox, delivered, applied, recorded>>

(* Recover restores in-memory status from the journal AND RE-DERIVES wakeReady +     *)
(* recorded from the DURABLE SUBSTRATE — crash-faithful (ph39 F3 closure, D39-02).   *)
(* The code has NO durable in-memory wake bit; on resume it re-derives everything     *)
(* from what's persisted, so the model must too rather than leaving wakeReady          *)
(* blanket-durable (the MaxCrashes=0 simplification this replaces):                    *)
(*   - TIMER dimension (durable BY DATA): wakeReady[n] is re-armed where               *)
(*     clock >= FireAt[n] — a timer that became due while the process was down fires    *)
(*     on resume (the code's on-resume re-arm, timer.go). Durable-by-data, not a flag.  *)
(*   - SIGNAL dimension (NOT a durable flag): wakeReady[n] reverts to FALSE; the node   *)
(*     reverts to its journal status (Waiting if the consume wasn't checkpointed) and   *)
(*     RE-CONSUMES the still-unacked mailbox (Consume re-fires on ~wakeReady /\          *)
(*     mailbox), re-applying byte-identically. recorded[n] is reverted to 0 for such a  *)
(*     not-yet-durable signal apply (journal # "done") so the re-apply re-establishes    *)
(*     ApplyVal — modelling the code's crash-before-checkpoint re-apply                  *)
(*     (TestSignalConsume_CrashBeforeCheckpoint_ReAppliesIdempotent). A signal whose     *)
(*     apply WAS durable (journal[n]="done") keeps recorded[n] (no re-apply).            *)
Recover ==
    /\ ~up
    /\ up' = TRUE
    /\ status' = journal
    \* halted RESETS on Recover (ph39 Option A, DEC-M10-P39-T5): fail-fast is
    \* PER-ATTEMPT in the Go code (a transient context-cancel, parallel_execution.go),
    \* NEVER reconstructed on resume — a loaded-Failed node is terminal/skipped
    \* (parallel_execution.go:88), not a fresh halt trigger, so a resume of a hard-failed
    \* run runs genuinely-INDEPENDENT nodes (an overdue timer completes) while the failed
    \* node stays Failed. Reconstructing halted from the journal here (the old
    \* HardFailedIn(journal)) was STRICTER than the code — it blocked that independent
    \* progress and made an overdue timer Stuck forever (Termination FALSE). Resetting it
    \* is the faithfulness fix; HardFailureHalts is correspondingly restated to within-
    \* attempt (a FRESH, not-yet-journalled non-coe failure halts THIS attempt).
    /\ halted' = FALSE
    /\ wakeReady' = [n \in Nodes |->
                       IF n \in TimerNodes THEN clock >= FireAt[n]
                       ELSE FALSE]
    /\ recorded' = [n \in Nodes |->
                       IF n \in Suspendable /\ n \notin TimerNodes /\ journal[n] # "done"
                       THEN 0
                       ELSE recorded[n]]
    /\ UNCHANGED <<journal, exec, crashes, clock, fireCount, mailbox, delivered, applied>>

------------------------------------------------------------------------
(* TIMER actions (the phase-36 durable-timer dimension). *)

(* Tick: logical wall time advances by one. Bounded (clock in 0..MaxTick) to keep  *)
(* the state space finite. Time is EXTERNAL: it is NOT guarded on `up`, so it       *)
(* advances even across a crash/downtime — which is exactly what makes an OVERDUE   *)
(* timer fire immediately on Recover (the clock crossed FireAt while the process    *)
(* was down). Tick is WEAK-FAIR (Fairness): time always eventually advances, so a   *)
(* sleeping timer is guaranteed to become due — this is the load-bearing fairness   *)
(* whose removal makes liveness FAIL (the bite-proof). A bounded clock means Tick   *)
(* stops at MaxTick, so its WF is dischargeable (it is not perpetually enabled).    *)
Tick ==
    /\ clock < MaxTick
    /\ clock' = clock + 1
    /\ UNCHANGED <<status, halted, journal, exec, up, crashes, wakeReady, fireCount, mailbox, delivered, applied, recorded>>

(* FireTimer: a timer node becomes DUE — the clock has reached its absolute         *)
(* FireAt. This is the timer analog of FireEvent, but DETERMINISTIC and GUARANTEED  *)
(* rather than an unfair environment event: given Tick fairness the clock WILL      *)
(* reach FireAt, so the timer WILL fire (it is weak-fair). The `~wakeReady[n]`      *)
(* guard makes it fire AT MOST ONCE (monotone), which NoDoubleFire witnesses; the   *)
(* fireCount bump is the observable counter for that invariant. Modelling a durable *)
(* timer as data: FireAt is a constant due-instant, firing just compares the clock  *)
(* to it — never a replayed time read (no determinism tax).                          *)
FireTimer(n) ==
    /\ up
    /\ n \in TimerNodes
    /\ status[n] = "waiting"   \* F2: a timer fires only while its node is parked (model<->code faithfulness, DEC-M10-P36-F2)
    /\ ~wakeReady[n]
    /\ clock >= FireAt[n]
    /\ wakeReady' = [wakeReady EXCEPT ![n] = TRUE]
    /\ fireCount' = [fireCount EXCEPT ![n] = fireCount[n] + 1]
    /\ UNCHANGED <<status, halted, journal, exec, up, crashes, clock, mailbox, delivered, applied, recorded>>

------------------------------------------------------------------------
(* Liveness scaffolding. *)

(* Stuck(n): a state that DEMANDS progress (the teeth of Termination — NOT        *)
(* ~ENABLED). The FINAL clause is the WakeReady-conditioned arm: a waiting node    *)
(* whose event HAS fired must wake (Stuck); a waiting node whose event has NOT     *)
(* fired is NOT Stuck — it rests legitimately (wait-for-an-event-that-may-never-   *)
(* come is not a liveness bug). This is the anti-hollow-liveness device.           *)
(* The FINAL TWO clauses are the waiting arms. (a) wakeReady-conditioned: a       *)
(* waiting node whose event HAS fired must wake (Stuck). (b) the TIMER arm: a      *)
(* waiting TIMER node is Stuck even BEFORE it is due — unlike a signal (which may  *)
(* never come and rests legitimately), a durable timer WILL fire as the clock      *)
(* advances, so a sleeping timer that never progresses IS a liveness bug. This is  *)
(* what gives Tick fairness teeth: drop WF(Tick) and the clock can stall below     *)
(* FireAt forever, leaving the timer arm Stuck and <>[]Settled FALSE (the bite).   *)
Stuck(n) ==
    \/ status[n] = "running"
    \/ (status[n] = "pending" /\ DepsResolved(n) /\ ~halted)
    \/ (status[n] = "pending" /\ HasSkipCauseDep(n))
    \/ (status[n] = "waiting" /\ wakeReady[n])
    \/ (status[n] = "waiting" /\ n \in TimerNodes)
    \* (c) the SIGNAL mailbox arm: a waiting node with a DELIVERED-but-unconsumed
    \* signal must progress (Consume -> wake). A waiting node with an EMPTY mailbox
    \* rests legitimately (the signal may never come — not a liveness bug), keeping
    \* the anti-hollow discipline; only a delivered signal demands progress.
    \/ (status[n] = "waiting" /\ mailbox[n])
    \* M11 OR-join must-progress arms. A launch-eligible MergeNode (all preds
    \* resolved-for-merge, run not halted) must fire or bypass; a non-merge node all
    \* of whose deps are settled with a bypassed one among them must bypass/skip
    \* (the cause-aware classification markSkippedFrom performs even on a halt, so no
    \* ~halted guard there). Inert when ChoiceNodes = MergeNodes = {}.
    \* A launch-eligible merge must progress: it can always BYPASS (0 taken), and
    \* can FIRE only when not halted (MergeStart is ~halted-gated) — so it is Stuck
    \* iff it can take one of those steps.
    \/ (status[n] = "pending" /\ n \in MergeNodes /\ MergeDepsSatisfied(n)
          /\ (TakenTails(n) = {} \/ ~halted))
    \/ (status[n] = "pending" /\ n \notin MergeNodes /\ AllDepsSettled(n) /\ HasBypassDep(n))

Settled == up /\ (\A n \in Nodes : ~Stuck(n))

Done == Settled /\ UNCHANGED vars

Next == \/ \E n \in Nodes : (Start(n) \/ Finish(n) \/ Skip(n) \/ Suspend(n) \/ Wake(n))
        \/ \E n \in Nodes : (SendSignal(n) \/ Consume(n) \/ Ack(n))
        \/ \E n \in Nodes : FireTimer(n)
        \* M11 OR-join actions (inert when ChoiceNodes = MergeNodes = {}).
        \/ \E c \in ChoiceNodes : (ChoiceFinish(c) \/ ChoiceFail(c))
        \/ \E n \in Nodes : (Bypass(n) \/ DiamondSkip(n))
        \/ \E m \in MergeNodes : (MergeStart(m) \/ MergeBypass(m))
        \/ Tick
        \/ Checkpoint
        \/ Crash
        \/ Recover
        \/ Done

(* Engine obligations are weak-fair; FireEvent, Crash, Checkpoint are UNFAIR      *)
(* (environment / optional). Wake's WF and Tick's WF are each broken out on their  *)
(* own line so the two liveness bite-proofs are one-line mutations:                *)
(*   - drop WF(Wake)  -> a fired-but-not-woken node is Stuck forever -> FAILS       *)
(*     (the chunk-1 bite, retained).                                                *)
(*   - drop WF(Tick)  -> the clock can stall below FireAt forever, so a sleeping    *)
(*     timer never becomes due and its TIMER Stuck arm holds forever -> <>[]Settled *)
(*     FAILS (the phase-36 Tick-fairness bite). FireTimer is weak-fair too: once    *)
(*     due (clock >= FireAt) a durable timer WILL fire — not an unfair environment  *)
(*     event like a signal.                                                         *)
Fairness ==
    /\ \A n \in Nodes : WF_vars(Start(n) \/ Finish(n) \/ Skip(n) \/ Suspend(n))
    \* M11 OR-join engine obligations are weak-fair (inert when the sets are empty).
    /\ \A c \in ChoiceNodes : WF_vars(ChoiceFinish(c) \/ ChoiceFail(c))
    /\ \A n \in Nodes : WF_vars(Bypass(n) \/ DiamondSkip(n))
    /\ \A m \in MergeNodes : WF_vars(MergeStart(m) \/ MergeBypass(m))
    /\ \A n \in Nodes : WF_vars(Wake(n))
    /\ \A n \in Nodes : WF_vars(FireTimer(n))
    /\ \A n \in Nodes : WF_vars(Consume(n))
    /\ \A n \in Nodes : WF_vars(Ack(n))
    /\ WF_vars(Tick)
    /\ WF_vars(Recover)

Spec == Init /\ [][Next]_vars /\ Fairness

------------------------------------------------------------------------
(* SAFETY — retained from the M9 model (re-checked under suspend). *)

ConcurrencyBound == Cardinality(Running) =< MaxConc

(* A node that has run (running/done) OR parked (waiting) had its deps resolved   *)
(* when it Started; deps are upstream-terminal and monotone, so this persists     *)
(* through a park. (Extends the M9 claim with the waiting case.)                  *)
(* M11 RESTATEMENT (phase 44, DEC-M11-P44-PRESERVE): "deps resolved" is ROLE-AWARE *)
(* — a MergeNode launches on the OR-join condition (MergeDepsSatisfied: a bypassed *)
(* tail SATISFIES), a strict-AND node on DepsResolved. Reduces EXACTLY to the M10  *)
(* strict-AND form when MergeNodes = {} (so M10 re-runs unchanged). This is a       *)
(* faithful role restatement, NOT a weakening: it still mandates every running/done *)
(* node had its launch precondition met — a bite that lets a merge run with an      *)
(* unresolved (pending/failed) tail still FALSIFIES it.                             *)
DepsBeforeRun ==
    \A n \in Nodes :
        (status[n] \in {"running","done","waiting"}) =>
            (IF n \in MergeNodes THEN MergeDepsSatisfied(n) ELSE DepsResolved(n))

(* HardFailureHalts (RESTATED ph39, within-attempt — DEC-M10-P39-T5 Option A). A FRESH *)
(* non-coe failure halts the CURRENT attempt: fail-fast cancels the level and stops     *)
(* further Start (parallel_execution.go cancel() + failChan). "Fresh" = the failure is  *)
(* not yet the durable truth (journal[n] # "failed") — an in-memory failure THIS         *)
(* attempt. A LOADED-Failed node (journal[n] = "failed", reloaded by Recover into a      *)
(* fresh attempt where halted has reset) is terminal/skipped, NOT a fresh halt trigger,  *)
(* so it does NOT require halted — that is the faithfulness fix that lets a resume of a   *)
(* hard-failed run run independent nodes (the Go per-attempt fail-fast; D39-05). Bite     *)
(* (re-bite, qa re-runs): drop `halted'=TRUE` in Finish's fail branch -> a fresh non-coe *)
(* failure (status=failed, journal#failed) does NOT halt -> FALSIFIES. So the restatement *)
(* is NOT weakened-to-vacuity: it still mandates within-attempt fail-fast.                *)
HardFailureHalts ==
    \A n \in Nodes :
        (status[n] = "failed" /\ n \notin ContinueOnError /\ journal[n] # "failed") => halted

(* NoResurrection (ph39 T5, closes FIND-M10-P36-T2 — guard rail 1). A node whose DURABLE *)
(* journal records a hard (non-coe) failure NEVER becomes "completed" in-memory: a resume *)
(* of a hard-failed run reloads the failed node as terminal and does NOT resurrect it to  *)
(* done, even though the resume DOES complete genuinely-independent nodes (an overdue      *)
(* timer). This is the crash/resume "adds no NEW terminal state" guarantee at the node     *)
(* level (a Failed node's terminal stays Failed — set-membership, M9 resume-equivalence).  *)
(* Bite: make Recover resurrect a hard-failed node (status' maps journal "failed" -> the   *)
(* terminal-success "done") -> a journalled-failed node shows done -> FALSIFIES. (NOTE:    *)
(* the model's terminal-success status is "done", NOT "completed" — using "completed" here  *)
(* would be VACUOUS, no state ever holds it; a first bite attempt that wrote "completed"    *)
(* surfaced exactly that via a TypeOK violation. The invariant is over the real "done".)    *)
NoResurrection ==
    \A n \in Nodes :
        (n \notin ContinueOnError /\ journal[n] = "failed") => status[n] # "done"

SkippedSound ==
    \A n \in Nodes :
        (status[n] = "skipped") => HasSkipCauseDep(n)

ExecFidelity ==
    \A n \in Nodes :
        (status[n] \in {"done","failed"}) => exec[n] >= 1

NoDoubleCommit ==
    \A n \in Nodes :
        (journal[n] = "done") => (status[n] = "done")

------------------------------------------------------------------------
(* SAFETY — NEW M10 claims. *)

(* WaitingSound — only a DECLARED suspension node is ever "waiting". A node that   *)
(* is not in Suspendable can never park (static topology). A mutation letting an   *)
(* ordinary node Suspend falsifies this.                                          *)
WaitingSound ==
    \A n \in Nodes :
        (status[n] = "waiting") => n \in Suspendable

(* NoDoubleFire — a durable timer fires AT MOST ONCE. fireCount[n] counts FireTimer *)
(* firings; the `~wakeReady[n]` guard on FireTimer makes firing monotone, so the    *)
(* count never exceeds 1. This is the model analog of the Go single-atomic-commit + *)
(* idempotent-fire contract (D36-08): mark-fired flips once, a crash-before re-fires *)
(* idempotently, a crash-after never re-fires. Bite: drop the `~wakeReady[n]` guard  *)
(* from FireTimer -> a due timer re-fires every step -> fireCount reaches 2 ->        *)
(* NoDoubleFire FALSIFIES.                                                            *)
NoDoubleFire ==
    \A n \in Nodes : fireCount[n] <= 1

------------------------------------------------------------------------
(* SAFETY — NEW phase-37 SIGNAL claims (mailbox + consume ordering). *)

(* NoSignalLost — a signal is never APPLIED without having been DELIVERED: an apply  *)
(* implies a real delivery (delivered is monotone + durable, so a delivered signal's *)
(* arrival is permanently on the record). Combined with the durable mailbox          *)
(* (UNCHANGED across Crash/Recover) and ack-after-durability, a delivered signal a    *)
(* node waits on is never discarded before consumption. Bite: drop the `mailbox[n]`  *)
(* guard on Consume -> a node consumes (applied++) with no delivery (delivered FALSE) *)
(* -> applied >= 1 /\ ~delivered -> NoSignalLost FALSIFIES.                            *)
NoSignalLost ==
    \A n \in Nodes : (applied[n] >= 1) => delivered[n]

(* NoDoubleApply (RESTATED ph39 — F3 CLOSED at MaxCrashes > 0) — OBSERVABLE-IDEMPOTENCE *)
(* over the RECORDED VALUE, NOT an apply-count. Under crashes the apply MAY repeat: on a *)
(* crash before the consume is checkpointed, Recover reverts the signal node to Waiting   *)
(* + reverts its not-yet-durable recorded, and the node RE-CONSUMES the still-unacked     *)
(* mailbox — re-applying. The code's real guarantee is therefore NOT "the apply fires at  *)
(* most once" but "every apply records the SAME value" (byte-identical overwrite via the  *)
(* deterministic resultKey, set-not-accumulate, D37-05). So the invariant is over the     *)
(* value: recorded[n] is ALWAYS either 0 (never applied) or ApplyVal (the one canonical   *)
(* value) — never divergent/accumulated, no matter how many crash re-applies occur. This  *)
(* is now genuinely EXERCISED at MaxCrashes > 0 (Recover forces the re-apply this          *)
(* invariant must tolerate). Anchor: TestSignalConsume_CrashBeforeCheckpoint_ReApplies-    *)
(* Idempotent.                                                                            *)
(*                                                                                        *)
(* Bite (anti-vacuity — NOT weakened-to-pass; qa's scrutiny point): change Consume's      *)
(* apply from the resume-STABLE key (recorded := ApplyVal) to a PER-ATTEMPT key           *)
(* (recorded := applied[n] + 1 — a value that depends on the attempt count, which is      *)
(* durable across Recover); the crash re-apply then records a DIFFERENT value (2) than the *)
(* first apply (1), driving recorded OUTSIDE {0, ApplyVal} -> FALSIFIES (verified: TLC      *)
(* "Invariant NoDoubleApply is violated", recorded reaches 2).                             *)
(*                                                                                          *)
(* NOTE (the subtle part, confirming the ph37 adversarial-tester's insight): an ACCUMULATE *)
(* mutation (recorded := recorded + ApplyVal) does NOT bite — Recover reverts the          *)
(* not-yet-durable recorded to 0, so the re-apply accumulates from 0 and still lands on    *)
(* ApplyVal (accumulate ≡ overwrite under crash-revert). So "set-not-accumulate" is        *)
(* structurally redundant here; the REAL guarantee observable-idempotence rests on is a    *)
(* RESUME-STABLE key/value (the deterministic resultKey, D37-05) — a value that does NOT   *)
(* vary by attempt. The per-attempt-key bite is the faithful falsifier; the invariant      *)
(* genuinely constrains recorded to the single canonical value, so it is not vacuous.      *)
NoDoubleApply ==
    \A n \in Nodes : recorded[n] \in {0, ApplyVal}

(* SuspendPreservesJournal — suspend (the chosen crash) + its checkpoint preserve the *)
(* durable mailbox: a DELIVERED-but-not-yet-applied signal is still in the mailbox.   *)
(* Because the mailbox lives OUTSIDE the snapshot (MH37-1 — Checkpoint leaves it      *)
(* UNCHANGED) and is cleared only by Ack (which requires journal="done", hence        *)
(* applied>=1), a delivered signal with applied=0 must still be buffered. Bite: make  *)
(* Checkpoint clear the mailbox (model the mailbox INSIDE the snapshot) -> a parked    *)
(* delivered-unconsumed node loses its signal at the suspend flush -> delivered /\     *)
(* applied=0 /\ ~mailbox -> SuspendPreservesJournal FALSIFIES (the early-signal-lost   *)
(* bite).                                                                              *)
SuspendPreservesJournal ==
    \A n \in Nodes : (delivered[n] /\ applied[n] = 0) => mailbox[n]

------------------------------------------------------------------------
(* TEMPORAL SAFETY — the wake is gated on the event. *)

(* WokeOnlyWhenReady — a SAME-PROCESS transition that moves a node from "waiting" *)
(* to "pending" (a Wake) happens only when its event has fired. The `up /\ up'`      *)
(* guard isolates a genuine same-process Wake from a Recover RELOAD (up: FALSE ->     *)
(* TRUE), which also moves a node waiting->pending (status' = journal) but is NOT a    *)
(* wake.                                                                              *)
(* CRASH-ISOLATION NOW MACHINE-CHECKED (ph39, closes FIND-M10-P35-N1): at MaxCrashes  *)
(* > 0 Crash/Recover fire, so TLC actually EXERCISES the reload. The `up /\ up'` guard *)
(* correctly excludes it (up-pre = FALSE on Recover) so a reload is not counted as a   *)
(* spurious wake. BITE (verified): drop the `up /\ up'` guard -> the Recover reload    *)
(* (waiting->pending with wakeReady re-derived FALSE for a signal) satisfies the bare  *)
(* transition without wakeReady -> TLC "Action property WokeOnlyWhenReady is           *)
(* violated". So the crash-isolation is load-bearing + exercised, not                  *)
(* asserted-by-composition.                                                           *)
WokeOnlyWhenReady ==
    [][ \A n \in Nodes :
          (up /\ up' /\ status[n] = "waiting" /\ status'[n] = "pending") => wakeReady[n]
      ]_vars

------------------------------------------------------------------------
(* M11 OR-JOIN SAFETY invariants (phase 44). Each is BITE-PROVEN: an isolated  *)
(* falsifying mutation reddens ONLY it (see 44-SUMMARY for the mutation log).  *)
(* Vacuously TRUE when ChoiceNodes = MergeNodes = {} (the M10 config), so they  *)
(* are safe to carry in the M10 invariant list too.                            *)

(* ExactlyOneBranchTaken — a ChoiceNode that has completed activated EXACTLY ONE   *)
(* branch: exactly one of its branch entries is NOT bypassed (the taken one; the   *)
(* rest are bypassed). BITE: let ChoiceFinish take two branches (mark only ONE     *)
(* non-taken bypassed) -> two non-bypassed -> FALSIFIES. *)
ExactlyOneBranchTaken ==
    \A c \in ChoiceNodes :
        status[c] = "done" =>
            Cardinality({ e \in ChoiceBranches[c] : status[e] # "bypassed" }) = 1

(* MergeFiresIffTakenTailComplete (anti-vacuity, red-team MAJOR-1) — a merge that   *)
(* fired (done) had >=1 TAKEN branch-tail Resolved, counted over MergeTails ONLY    *)
(* (the always-`done` structural Choice-dep is a dep but NOT a tail, so it is       *)
(* EXCLUDED). BITE: count the Choice-dep as a taken tail (TakenTails ranges over    *)
(* DepsOf instead of MergeTails) -> the all-bypassed merge fires on the Choice-dep  *)
(* alone -> a `done` merge with no Resolved TAIL -> FALSIFIES. *)
MergeFiresIffTakenTailComplete ==
    \A m \in MergeNodes :
        status[m] = "done" => (\E t \in MergeTails[m] : Resolved(t))

(* BypassedNeverRuns — a bypassed node NEVER executed (a not-taken branch interior, *)
(* or an all-bypassed merge, never runs its action). The only thing "below a        *)
(* bypassed branch" that reaches `done` is a MERGE that had >=1 taken tail (which is *)
(* `done`, not `bypassed`, so this invariant permits it). BITE: let a bypassed node *)
(* run (Bypass sets status while bumping exec) -> exec>0 for a bypassed node ->      *)
(* FALSIFIES. *)
BypassedNeverRuns ==
    \A n \in Nodes : status[n] = "bypassed" => exec[n] = 0

(* BypassVsFailureSeparator (THE critical one — the semantic M10 refused to model).  *)
(* At a merge, a `bypassed` predecessor SATISFIES (does not block) while a           *)
(* `failed`(non-coe)/`skipped` predecessor BLOCKS: a merge with a skip-cause dep is  *)
(* fail-fast SKIPPED — never `done` (fired) and never `bypassed`. So a merge that     *)
(* reached `done` OR `bypassed` had NO skip-cause dep. Bypass and failure are         *)
(* distinguishable AT the merge — the exact thing M10 could not tell apart. BITE:     *)
(* model "merge fires/satisfies on any resolved-OR-SKIPPED predecessor" (MergeResolved *)
(* treats a skip-cause as satisfied) -> a merge with a failed/skipped tail is         *)
(* launch-eligible and (0 taken among the survivors) BYPASSES instead of SKIPPING ->  *)
(* a `bypassed` merge WITH a skip-cause dep -> FALSIFIES. *)
BypassVsFailureSeparator ==
    \A m \in MergeNodes :
        (status[m] \in {"done", "bypassed"}) => ~HasSkipCauseDep(m)

(* ChoiceFailureSkipsNotBypasses (41-F1 / review 44-F1) — the bypass-vs-failure       *)
(* separator AT THE CHOICE (not just at the merge). A branch entry is "bypassed"      *)
(* ONLY because its Choice TOOK ANOTHER branch (status[c]="done"). If the Choice      *)
(* itself FAILED to route (ChoiceFail: no branch matched), its branches are SKIPPED   *)
(* by the cascade, NEVER bypassed — a routing FAILURE must not be mislabeled a clean  *)
(* BYPASS. So a bypassed branch entry implies its Choice completed. BITE: let         *)
(* ChoiceFail bypass its branches (the 41-F1 defect) -> a bypassed entry under a       *)
(* failed choice -> FALSIFIES. *)
ChoiceFailureSkipsNotBypasses ==
    \A c \in ChoiceNodes :
        \A e \in ChoiceBranches[c] :
            status[e] = "bypassed" => status[c] = "done"

------------------------------------------------------------------------
(* LIVENESS — every behavior eventually reaches a stable Settled fixed point.      *)
(* With the WakeReady-conditioned Stuck arm this has real teeth on the wake        *)
(* machinery: a behavior that fires an event and then never wakes the node leaves  *)
(* it Stuck forever and FAILS <>[]Settled (the Wake-fairness bite-proof). A        *)
(* behavior that simply never fires the event rests in a legitimately-Settled       *)
(* parked state — wait-forever is not a violation.                                  *)
Termination == <>[]Settled

=============================================================================
