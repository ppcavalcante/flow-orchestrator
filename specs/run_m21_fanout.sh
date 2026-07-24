#!/usr/bin/env bash
# M21 ph108 — the DYNAMIC FAN-OUT formal capstone (FANOUT-09). Two machine-checked properties,
# BITE-proven: the real model HOLDS exhaustively; a committed seed-break .cfg makes TLC FALSIFY.
# A property whose invariant cannot redden is theater — this proves the M21 no-replay claim is
# load-bearing, reproducible from the artifacts alone:
#
#   ExactlyNSpawn    (M21FanOut) — the expander is journaled ONCE; N is stable across a kill-storm
#                     (no re-expand on resume = the moat's no-determinism-tax leg). Seed:
#                     re-expand-on-resume (AtomicExpansion=FALSE) -> expandCount=2 -> Falsifies.
#   FanInWaitsForAll (M21FanOut) — the fan node terminalizes IFF all N branches terminal. Held in
#                     both arms (the same spec); the ExactlyNSpawn break is the discriminator.
#
# Requires TLC: java17 + tla2tools.jar. System `java` is a stub on this machine -> use openjdk@17.
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

echo "== M21FanOut OK — ExactlyNSpawn + FanInWaitsForAll must HOLD, exhaustive =="
TLC -config M21FanOut.cfg M21FanOut.tla | grep -E "No error|states found" || { echo "FAIL: OK model did not hold"; exit 1; }

echo "== M21FanOut BREAK — ExactlyNSpawn must FALSIFY (re-expand-on-resume) =="
# TLC EXITS NON-ZERO (12) on an invariant violation — which is the EXPECTED, desired outcome of the
# seed-break. Capture the output without letting pipefail/`set -e` abort on that expected non-zero.
set +e +o pipefail
BREAK_OUT="$(TLC -config M21FanOutBreak.cfg M21FanOut.tla 2>&1)"
set -e
if echo "$BREAK_OUT" | grep -q "Invariant ExactlyNSpawn is violated"; then
  echo "OK: seed-break falsified ExactlyNSpawn (bite-proven)"
else
  echo "FAIL: seed-break did NOT falsify — the invariant is theater"; exit 1
fi
echo "== M21 fan-out capstone: both arms bite-proven =="
