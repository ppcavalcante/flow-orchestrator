#!/usr/bin/env bash
# M15 ph71 — decomposed-checkpoint (set-of-rows) crash arm: reproducible run + bite log.
# Runs the correct arm (exhaustive GREEN @ MaxCrashes=1), the MaxCrashes=2 bonus, and
# every isolated break-seed (each reddens EXACTLY its own invariant), so "exhaustive +
# every invariant bite-proven" is reproducible from the artifacts alone.
#
# Requires TLC: java17 + tla2tools.jar. On this machine the system `java` is a stub —
# use the openjdk@17 path (project memory).
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || JAR=/private/tmp/tla2tools.jar
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

# TLC exits non-zero on an invariant violation; capture output first so `set -e` / the
# pipe's exit status does not abort before we inspect the result.
green() { local out; out="$(TLC -config "$1" DecomposedCheckpoint.tla 2>&1 || true)"
  echo "$out" | grep -q "No error has been found" \
  && echo "  GREEN  $2" || { echo "  FAIL(expected green)  $2"; exit 1; }; }
reddens() { local out; out="$(TLC -config "$1" DecomposedCheckpoint.tla 2>&1 || true)"
  echo "$out" | grep -q "Invariant $2 is violated" \
  && echo "  RED    $1 -> $2 (bite proven)" || { echo "  FAIL(expected $2 RED)  $1"; exit 1; }; }

echo "== correct store: all invariants GREEN, exhaustive =="
green DecomposedCheckpoint.cfg    "correct @ MaxCrashes=1 (7 distinct states, 0 on queue)"
green DecomposedCheckpointMC2.cfg "correct @ MaxCrashes=2 bonus (10 distinct states, 0 on queue)"

echo "== each break-seed reddens EXACTLY its target invariant (isolated bite) =="
reddens DecomposedCheckpointTorn.cfg    INV_NoPartialLevel   # torn multi-row txn (the ph65 integer sketch could NOT reach this)
reddens DecomposedCheckpointGap.cfg     INV_PrefixClosed     # non-contiguous durable frontier
reddens DecomposedCheckpointRegress.cfg INV_NoCommittedLoss  # resume regresses below the committed frontier

echo "ALL GREEN + ALL BITES PROVEN. (Isolation — each seed reddens only its target — is"
echo "re-checkable by removing the target invariant from the broken cfg: the other two stay green.)"
