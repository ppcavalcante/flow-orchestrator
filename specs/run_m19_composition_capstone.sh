#!/usr/bin/env bash
# Phase-96 M19 sub-workflow-await capstone — reproducible run + bite log.
# Runs the composition config exhaustively + the preservation re-runs (every base
# config re-runs byte-behaviour-unchanged) + every invariant bite (falsify -> restore),
# so "composition green + preservation + every invariant bite-proven + the discriminator
# RED-in-composition/INERT-in-base" is reproducible from the artifacts alone.
#
# Requires TLC: java17 + tla2tools.jar. On this machine the system `java` is a stub —
# use the openjdk@17 path (see project memory).
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

# green cfg spec label : assert the run finds NO error. (Capture first — grep -q under a
# pipe can SIGPIPE TLC and, with pipefail, mask the result; so run to a var, then match.)
green() { local out; out="$(TLC -config "$1" "$2" 2>&1 || true)"
  case "$out" in *"No error has been found"*) echo "  GREEN  $3";;
    *) echo "  FAIL(expected green)  $3"; exit 1;; esac; }
# red cfg spec inv label : assert the run VIOLATES the named invariant. (TLC exits non-zero
# on a violation; `|| true` keeps `set -e` from aborting before we inspect the output.)
red() { local out; out="$(TLC -config "$1" "$2" 2>&1 || true)"
  case "$out" in *"Invariant $3 is violated"*) echo "  RED    $4  ($3 falsified)";;
    *) echo "  FAIL(expected red)  $4"; exit 1;; esac; }
setc() { sed -i '' "$2" "$1"; }   # macOS sed; drop the '' on GNU sed

SPEC=M10DurableExecutor.tla
BAK=/tmp/m19_capstone_$$.bak
cp "$SPEC" "$BAK"
restore() { cp "$BAK" "$SPEC"; }
trap restore EXIT

echo "== composition (exhaustive, MaxCrashes=1) — all 4 invariants + Termination =="
green MCM19Composition.cfg MCM19Composition.tla "composition (62,790 states)"

echo "== preservation — every base config re-runs byte-behaviour-unchanged (arm inert) =="
green M10DurableExecutor.cfg MCM10DurableExecutor.tla "M10 diamond (14,380 states)"
green M10FailResume.cfg      MCM10FailResume.tla      "M10 fail-resume (915 states)"
green M12Saga.cfg            MCM12Saga.tla            "M12 saga (722 states)"
green M11ChoiceMerge.cfg     MCM11ChoiceMerge.tla     "M11 choice-merge (73 states)"

echo "== BITE — each of the 4 composition invariants falsifies under its should-fail mutation =="

# BITE 1 — NoDoubleSpawn (THE DISCRIMINATOR). Drop the spawn-idempotency guard.
setc "$SPEC" 's/    \/\\ spawned\[n\] = 0/    \/\\ TRUE \\* BITE/'
red   MCM19Composition.cfg MCM19Composition.tla NoDoubleSpawn "discriminator: composition RED"
# ...and the SAME mutation is INERT in the base config (nothing spawns) — anti-vacuity.
green M10DurableExecutor.cfg MCM10DurableExecutor.tla "discriminator: base INERT (no spawn)"
restore

# BITE 2 — ParkWakeExactlyOnTerminal. ChildComplete wakes without recording the terminal.
setc "$SPEC" 's/= IF n \\in ChildFailSet THEN "failed" ELSE "succeeded"\]/= "none"] \\* BITE/'
red   MCM19Composition.cfg MCM19Composition.tla ParkWakeExactlyOnTerminal "park/wake before child terminal"
restore

# BITE 3 — ChildFailParentFail (INV-01). A failed child no longer fails the parent.
setc "$SPEC" 's/SubWorkflowFails(n) == n \\in SubWorkflowNodes \/\\ childTerminal\[n\] = "failed"/SubWorkflowFails(n) == FALSE \\* BITE/'
red   MCM19Composition.cfg MCM19Composition.tla ChildFailParentFail "child-fail -> parent-fail dropped"
restore

# BITE 4 — BoundedNesting (the ph95 DoS ceiling). SpawnChild spawns at/over the ceiling.
setc "$SPEC" 's/    \/\\ NodeDepth\[n\] < MaxDepth/    \/\\ TRUE \\* BITE/'
red   MCM19Composition.cfg MCM19Composition.tla BoundedNesting "spawn past the depth ceiling"
restore

echo "== all green + every invariant bite-proven + discriminator RED-in-comp/INERT-in-base =="
