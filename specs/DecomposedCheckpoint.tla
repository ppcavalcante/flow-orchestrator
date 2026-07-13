-------------------- MODULE DecomposedCheckpoint --------------------
(***************************************************************************)
(* M15 ph71 — the DECOMPOSED per-level checkpoint crash arm, re-modelled at  *)
(* ROW granularity (the full arm; the ph65 integer sketch was the seed).     *)
(*                                                                          *)
(* THE CLAIM UNDER TEST (SQL-03). The SQLiteStore commits each level with a   *)
(* SINGLE SQLite transaction that UPSERTs that level's per-node ROWS (one row  *)
(* per node — nodes/data_kv/waits, workflow_store_sqlite.go). SQLite COMMIT    *)
(* atomicity means a level's rows land ALL-OR-NOTHING: a crash mid-txn leaves  *)
(* the durable frontier at the last COMMITTED level, never a torn half-level.  *)
(* Resume reassembles the WorkflowData from the committed rows and continues.  *)
(*                                                                          *)
(* WHY A SET, NOT AN INTEGER (the ph71 handshake — this is the whole point).  *)
(* The ph65 sketch modelled `committed` as an INTEGER frontier level. That    *)
(* can express "a clean level did / didn't commit" but CANNOT express the      *)
(* real hazard the decomposition introduces: a MULTI-ROW transaction that      *)
(* lands a PROPER SUBSET of one level's rows (row 1 of level L durable, row 2   *)
(* of level L lost). An integer frontier literally has no state for "half of    *)
(* level L". So the durable frontier here is a SET/function of committed ROWS,  *)
(* and the load-bearing invariant is INV_NoPartialLevel: the committed rows are  *)
(* ALWAYS a union of WHOLE levels — never a proper non-empty subset of any       *)
(* single level's rows. THIS is the multi-row-atomicity claim the integer        *)
(* sketch could not reach; the torn seed drives a partial level and reddens it.  *)
(*                                                                          *)
(* FAITHFULNESS. This is a durability/crash-atomicity abstraction of the store   *)
(* layer, deliberately ORTHOGONAL to the executor scheduling model (that is       *)
(* M9 DurableExecutor / M10 — left EXACT). It models exactly the store's durable  *)
(* unit (a level's row-set) and the one property the decomposition must preserve  *)
(* vs the whole-snapshot store: crash-atomicity at the level boundary. Rows are    *)
(* abstract tokens (<<level, row-index>>) — their payload is irrelevant to         *)
(* atomicity; the ph66 fidelity property + the ph71 gopter substitution cover the   *)
(* payload. Bounded (small Levels x RowsPerLevel) so MaxCrashes=1 stays EXHAUSTIVE. *)
(***************************************************************************)
EXTENDS FiniteSets, Naturals

CONSTANTS
    Levels,        \* number of levels (>= 1)
    RowsPerLevel,  \* rows (per-node UPSERTs) in each level's checkpoint txn (>= 1)
    MaxCrashes,    \* crashes to explore (>= 0)
    Seed           \* which store to model — "none" is the CORRECT store; the other three
                   \* are BROKEN stores, each an ISOLATED break-seed for exactly one invariant
                   \* (falsify->restore, the team bite discipline [[guard-must-bite-seed-the-break]]):
                   \*   "none"    = correct COMMIT atomicity + correct resume (all invariants GREEN)
                   \*   "torn"    = a per-level txn commits a PROPER SUBSET of a level's rows
                   \*               -> INV_NoPartialLevel Falsifies (the multi-row-torn bite)
                   \*   "gap"     = a txn commits level L+1 while level L's rows are ABSENT (a
                   \*               non-contiguous frontier) -> INV_PrefixClosed Falsifies
                   \*   "regress" = resume sets the in-memory level BELOW the committed frontier
                   \*               (loses a durably-committed level) -> INV_NoCommittedLoss Falsifies

ASSUME Levels       \in Nat /\ Levels >= 1
ASSUME RowsPerLevel \in Nat /\ RowsPerLevel >= 1
ASSUME MaxCrashes   \in Nat
ASSUME Seed \in {"none", "torn", "gap", "regress"}

\* A ROW is a <<level, rowIndex>> token. RowsOfLevel(L) is level L's whole row-set
\* (the atomic unit of one per-level checkpoint transaction).
LevelIds     == 1..Levels
RowIds       == 1..RowsPerLevel
RowsOfLevel(L) == { <<L, r>> : r \in RowIds }
AllRows      == { <<L, r>> : L \in LevelIds, r \in RowIds }

VARIABLES
    level,      \* highest level whose checkpoint txn has been ATTEMPTED in memory (0..Levels)
    committed,  \* durable decomposed frontier: the SET of COMMITTED node-rows (SUBSET AllRows)
    crashes     \* crashes so far (bounds the search)

vars == <<level, committed, crashes>>

TypeOK ==
    /\ level     \in 0..Levels
    /\ committed \in SUBSET AllRows
    /\ crashes   \in 0..MaxCrashes

Init ==
    /\ level     = 0
    /\ committed = {}
    /\ crashes   = 0

\* LevelFullyCommitted(L): every row of level L is durable.
LevelFullyCommitted(L) == RowsOfLevel(L) \subseteq committed

\* CommittedFrontier: the highest level L such that levels 1..L are ALL fully
\* committed (a clean union-of-whole-levels frontier). 0 if level 1 is not complete.
\* This is what resume reads: it reassembles from the committed rows up to the last
\* WHOLE committed level and re-runs from there.
CommittedFrontier ==
    IF \E L \in LevelIds : ~LevelFullyCommitted(L)
    THEN (CHOOSE L \in LevelIds : ~LevelFullyCommitted(L) /\ \A K \in 1..(L-1) : LevelFullyCommitted(K)) - 1
    ELSE Levels

------------------------------------------------------------------------
(* ACTIONS. *)

\* CommitRows(L): the row-set the per-level SQLite transaction for level L makes durable.
\*   correct        -> the WHOLE row-set of L (atomic COMMIT).
\*   "torn"         -> a PROPER SUBSET (row RowsPerLevel dropped): a non-atomic txn lands
\*                     half a level -> INV_NoPartialLevel Falsifies. (Needs RowsPerLevel>=2
\*                     for a proper subset to exist — the config guarantees it.)
\*   "gap"          -> on the FIRST level only (L=1), commit level L+1's rows INSTEAD, so a
\*                     HIGHER whole level lands while level 1 stays empty -> a non-contiguous
\*                     frontier -> INV_PrefixClosed Falsifies. (Needs Levels>=2.) Later levels
\*                     commit normally, so INV_NoPartialLevel stays clean (each level whole),
\*                     ISOLATING the gap bite to PrefixClosed alone.
CommitRows(L) ==
    CASE Seed = "torn" -> RowsOfLevel(L) \ { <<L, RowsPerLevel>> }
      [] Seed = "gap"  -> IF L = 1 /\ Levels >= 2 THEN RowsOfLevel(L + 1) ELSE RowsOfLevel(L)
      [] OTHER         -> RowsOfLevel(L)   \* "none" and "regress" commit correctly (regress breaks resume, not commit)

\* Enabled whenever the run has not reached the last level. After a crash, resume
\* re-reads the committed frontier (Crash sets level := CommittedFrontier) and KEEPS
\* checkpointing forward — a crash bounds `crashes`, it does NOT freeze forward progress
\* (the integer-sketch `crashes=0` guard would deadlock a resumed run before it finished).
Checkpoint ==
    /\ level < Levels
    /\ level' = level + 1
    /\ committed' = committed \union CommitRows(level + 1)
    /\ UNCHANGED crashes

\* Crash: the process dies. Durable state (committed) is UNCHANGED — SQLite COMMIT is
\* the durability boundary, so only fully-committed levels survive. In-memory forward
\* progress (level) is LOST; on resume it reverts to the committed decomposed frontier.
\*   correct   -> level' = CommittedFrontier (resume re-reads the committed rows exactly).
\*   "regress" -> level' = CommittedFrontier - 1 when the frontier is >= 1: resume regresses
\*                BELOW a durably-committed level (loses a committed level) -> INV_NoCommittedLoss
\*                Falsifies. Does NOT touch `committed`, so INV_NoPartialLevel/PrefixClosed stay
\*                clean, ISOLATING the regress bite to NoCommittedLoss alone.
ResumeLevel ==
    IF Seed = "regress" /\ CommittedFrontier >= 1
    THEN CommittedFrontier - 1
    ELSE CommittedFrontier

Crash ==
    /\ crashes < MaxCrashes
    /\ level >= 1
    /\ crashes' = crashes + 1
    /\ level' = ResumeLevel
    /\ UNCHANGED committed

\* Terminal stutter so TLC evaluates the invariants on the halt state rather than
\* reporting a spurious deadlock. The run is TERMINAL when it has reached the last level
\* AND cannot crash further (crash budget exhausted) — no forward step (level=Levels) and
\* no Crash (crashes=MaxCrashes) remain enabled. Under the correct store committed=AllRows
\* holds there; the torn store leaves a partial level (already caught by the invariant).
Halt == level = Levels /\ crashes = MaxCrashes

Next == Checkpoint \/ Crash \/ (Halt /\ UNCHANGED vars)

Spec == Init /\ [][Next]_vars

------------------------------------------------------------------------
(* INVARIANTS. *)

(* INV_NoPartialLevel (THE load-bearing one — the multi-row-atomicity claim the ph65   *)
(* integer sketch could NOT express). For EVERY level, its committed rows are the empty *)
(* set OR its whole row-set — NEVER a proper non-empty subset. Equivalently: the         *)
(* durable frontier is always a union of WHOLE levels. The "torn" seed commits a proper  *)
(* subset of a level's rows, so committed \cap RowsOfLevel(L) is a proper non-empty       *)
(* subset -> this Falsifies. This is the real torn-multi-row bite.                        *)
INV_NoPartialLevel ==
    \A L \in LevelIds :
        \/ (committed \cap RowsOfLevel(L)) = {}
        \/ (committed \cap RowsOfLevel(L)) = RowsOfLevel(L)

(* INV_PrefixClosed — the committed WHOLE levels form a downward-closed prefix: if       *)
(* level L is fully committed then every level below it is too. The per-level txn only    *)
(* commits level+1 (the next level), so committed whole-levels are contiguous from 1. A   *)
(* store that committed level 3 while level 2's rows were absent would violate this (a     *)
(* gap in the frontier — resume could not reassemble a coherent prefix). BITE (isolated,  *)
(* NOT true-by-construction — reviewer ph71-F1): the "gap" seed commits level 2 while      *)
(* level 1 stays empty -> a non-contiguous frontier -> this Falsifies (verified RED).      *)
INV_PrefixClosed ==
    \A L \in LevelIds :
        LevelFullyCommitted(L) => (\A K \in 1..L : LevelFullyCommitted(K))

(* INV_NoCommittedLoss — resume NEVER regresses below the durable committed frontier: the *)
(* in-memory level is always at least the highest fully-committed level. Correct Crash sets *)
(* level := CommittedFrontier EXACTLY, so equality holds. BITE (isolated, NOT true-by-       *)
(* construction — reviewer ph71-F1): the "regress" seed makes Crash resume to                *)
(* CommittedFrontier-1 (loses a durably-committed level) -> level < CommittedFrontier ->      *)
(* this Falsifies (verified RED). The M9 crash-resume floor, preserved by the decomposition.  *)
INV_NoCommittedLoss ==
    level >= CommittedFrontier

=============================================================================
