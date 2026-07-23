#!/usr/bin/env bash
# M20 ph102 — the scheduling formal arm: run the correct model (NoDoubleFire HOLDS, exhaustive)
# AND the seed-break (atomic advance removed -> NoDoubleFire Falsifies). Both must behave as
# asserted, so "the in-txn re-check + atomic advance is machine-proven load-bearing" (the ph100
# DEC-P100-RECHECK-IS-ARBITER finding) is reproducible from the artifacts alone.
#
# Requires TLC: java17 + tla2tools.jar. On this machine the system `java` is a stub — use openjdk@17.
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

echo "== M20Scheduling OK (Recheck=TRUE) — NoDoubleFire must HOLD, exhaustive =="
TLC -config M20Scheduling.cfg M20Scheduling.tla > /tmp/m20sched_ok.log 2>&1 || true
if grep -q "No error has been found" /tmp/m20sched_ok.log; then
  grep -E "distinct states" /tmp/m20sched_ok.log | tail -1
  echo "  GREEN  NoDoubleFire HOLDS"
else
  echo "  FAIL   M20Scheduling OK did not verify clean"; tail -5 /tmp/m20sched_ok.log; exit 1
fi

echo "== M20Scheduling BREAK (Recheck=FALSE) — NoDoubleFire must FALSIFY (seed-the-break) =="
TLC -config M20SchedulingBreak.cfg M20Scheduling.tla > /tmp/m20sched_break.log 2>&1 || true
if grep -q "Invariant NoDoubleFire is violated" /tmp/m20sched_break.log; then
  echo "  GREEN  seed-break FALSIFIES NoDoubleFire (the in-txn re-check + atomic advance is load-bearing)"
else
  echo "  FAIL   seed-break did NOT falsify — the invariant is vacuous/non-biting"; tail -5 /tmp/m20sched_break.log; exit 1
fi

echo "ALL GREEN — M20 scheduling arm holds; the deferred-advance mutation reddens."
