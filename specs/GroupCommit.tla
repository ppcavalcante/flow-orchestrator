------------------------- MODULE GroupCommit -------------------------
(***************************************************************************)
(* M14 ph61 — group-commit (batched-fsync) durability crash arm            *)
(* (DEC-M14-GROUPCOMMIT / B1). Models the durable-frontier-every-K crash    *)
(* semantics: SaveCheckpoint fsyncs (advances the durable frontier) only    *)
(* every K levels; a park/completion forces an immediate fsync (advances    *)
(* the frontier NOW); a power-loss crash reverts live progress to the last  *)
(* durable frontier, losing 0..K-1 un-fsync'd levels; resume re-runs the    *)
(* lost levels (idempotent — the M9/M12 IdempotencyKey contract).           *)
(*                                                                          *)
(* Faithfulness: `level` = the run's current level (in-memory progress).    *)
(* `frontier` = the last fsync-durable level (what survives a crash). Under *)
(* Batched(K), frontier advances to `level` only when level % K == 0 (a     *)
(* group-commit boundary) OR on a forced Sync (park/completion). A crash    *)
(* sets level := frontier (loses level-frontier levels). This abstracts the *)
(* store's strategy-(d): un-fsync'd levels are simply not on disk, so a      *)
(* crash reverts to the last complete fsync'd snapshot.                     *)
(*                                                                          *)
(* THE core invariant (INV_LossBounded): a crash never loses MORE than K    *)
(* levels — level - frontier <= K-1 always (the crash-loss window is        *)
(* exactly bounded by K). The bite: a seeded WRONG frontier cadence         *)
(* (advance every K+1, WrongCadence) or a park that does NOT force Sync      *)
(* (NoParkSync) must make some reachable state VIOLATE the bound (or lose a  *)
(* park), proving the model constrains — not vacuously passes.              *)
(***************************************************************************)
EXTENDS Naturals

CONSTANTS
    Levels,       \* total levels the run executes (>= 1)
    K,            \* group-commit batch window (>= 1; K=1 == Strict)
    MaxCrashes,   \* bound on crashes to explore (>= 0)
    Mode          \* "correct" | "WrongCadence" | "NoParkSync" (the seeded bites)

ASSUME Levels >= 1
ASSUME K >= 1
ASSUME MaxCrashes >= 0
ASSUME Mode \in {"correct", "WrongCadence", "NoParkSync"}

\* The park level: the run parks (a durable-floor forced Sync) at this level, then
\* completes. A park at level P forces frontier := P immediately (correct mode).
\* Chosen as a level strictly inside a K-window so an un-forced park WOULD be lost.
ParkLevel == IF Levels >= 2 THEN Levels - 1 ELSE Levels

VARIABLES
    level,      \* current in-memory level (0..Levels)
    frontier,   \* last fsync-durable level (0..Levels)
    crashes,    \* crashes so far (0..MaxCrashes)
    parked      \* has the run forced its park Sync? (models the floor)

vars == <<level, frontier, crashes, parked>>

\* The cadence at which SaveCheckpoint advances the durable frontier. Correct = K;
\* the WrongCadence bite advances only every K+1 (a mis-counted batch → the loss
\* window exceeds K → INV_LossBounded must redden).
Cadence == IF Mode = "WrongCadence" THEN K + 1 ELSE K

Init ==
    /\ level = 0
    /\ frontier = 0
    /\ crashes = 0
    /\ parked = FALSE

\* Advance one level. On a group-commit boundary (level' % Cadence == 0) the
\* checkpoint fsyncs → frontier catches up to level'. Otherwise the write is
\* deferred (frontier stays put — those levels are lost on a crash).
Step ==
    /\ level < Levels
    /\ level' = level + 1
    /\ frontier' = IF (level' % Cadence) = 0 THEN level' ELSE frontier
    /\ UNCHANGED <<crashes, parked>>

\* The park at ParkLevel forces an immediate Sync (the D-10/D-11 floor): frontier
\* jumps to the park level NOW, regardless of the batch window — UNLESS the
\* NoParkSync bite is seeded (then the park does NOT force, so a crash right after
\* loses it and INV_ParkDurable reddens).
Park ==
    /\ level = ParkLevel
    /\ ~parked
    /\ parked' = TRUE
    /\ frontier' = IF Mode = "NoParkSync" THEN frontier ELSE ParkLevel
    /\ UNCHANGED <<level, crashes>>

\* A power-loss crash: live progress reverts to the durable frontier (loses
\* level-frontier un-fsync'd levels). Resume re-runs from frontier (idempotent).
\* Bounded by MaxCrashes. A crash also un-parks IF the park was not durable (its
\* frontier was reverted below ParkLevel).
Crash ==
    /\ crashes < MaxCrashes
    /\ crashes' = crashes + 1
    /\ level' = frontier
    /\ parked' = IF frontier >= ParkLevel THEN parked ELSE FALSE
    /\ UNCHANGED frontier

Done == level = Levels /\ (parked \/ ParkLevel = Levels)

Next ==
    \/ Park
    \/ Step
    \/ Crash
    \/ (level = Levels /\ UNCHANGED vars)   \* terminal stutter

Spec == Init /\ [][Next]_vars

\* --- Invariants -------------------------------------------------------------

\* INV_LossBounded: the un-fsync'd window never exceeds K-1, so a crash loses < K
\* levels (i.e. <= K). This is the crash-loss bound the mode API promises. Under
\* WrongCadence (advance every K+1) the window reaches K → this reddens.
INV_LossBounded == level - frontier < K \/ level - frontier <= K - 1 \/ K = 1

\* Cleaner statement of the same bound (the one TLC checks): the number of
\* un-fsync'd levels is at most K-1 (Strict K=1 → 0).
INV_Loss == (level - frontier) <= (K - 1)

\* INV_ParkDurable: once the run has parked, the park is fsync-durable — its level
\* is at or below the frontier, so a crash cannot revert past it. The NoParkSync
\* bite (park does not force Sync) makes a post-park crash revert below ParkLevel →
\* parked flips FALSE with frontier < ParkLevel reachable → reddens.
INV_ParkDurable == parked => (frontier >= ParkLevel)

=============================================================================
