#!/usr/bin/env bash
# M16 ph77 — the MP fencing formal arm: run the correct model (NoStaleOverwrite HOLDS, exhaustive)
# AND the seed-break (fencing removed -> NoStaleOverwrite Falsifies). Both must behave as asserted,
# so "the fence is machine-proven load-bearing" is reproducible from the artifacts alone.
#
# Requires TLC: java17 + tla2tools.jar. On this machine the system `java` is a stub — use openjdk@17.
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

echo "== MPFencing OK (Fencing=TRUE) — NoStaleOverwrite must HOLD, exhaustive (~2809 distinct states) =="
TLC -config MPFencing.cfg MPFencing.tla > /tmp/mpf_ok.log 2>&1 || true
if grep -q "No error has been found" /tmp/mpf_ok.log; then
  grep -E "distinct states" /tmp/mpf_ok.log | tail -1
  echo "  GREEN  NoStaleOverwrite HOLDS"
else
  echo "  FAIL   MPFencing OK did not verify clean"; tail -5 /tmp/mpf_ok.log; exit 1
fi

echo "== MPFencing BREAK (Fencing=FALSE) — NoStaleOverwrite must FALSIFY (seed-the-break) =="
TLC -config MPFencingBreak.cfg MPFencing.tla > /tmp/mpf_break.log 2>&1 || true
if grep -q "Invariant NoStaleOverwrite is violated" /tmp/mpf_break.log; then
  echo "  GREEN  seed-break FALSIFIES NoStaleOverwrite (the fence is load-bearing)"
else
  echo "  FAIL   seed-break did NOT falsify — the invariant is vacuous/non-biting"; tail -5 /tmp/mpf_break.log; exit 1
fi

echo "ALL GREEN — MP fencing arm holds; the no-fencing mutation reddens."
