#!/usr/bin/env bash
# M20 ph102 — the FULL scheduling + concurrency-cap formal capstone. Three machine-checked
# properties, each BITE-proven (the real model HOLDS exhaustively; a committed seed-break .cfg
# makes TLC FALSIFY it). A property whose invariant cannot redden is theater — these prove the
# three M20 correctness decisions are load-bearing, reproducible from the artifacts alone:
#
#   NoDoubleFire     (M20Scheduling) — DEC-P100-RECHECK-IS-ARBITER: the in-txn re-check + ATOMIC
#                     advance is the fire arbiter (NOT the fence). Seed: deferred advance -> double-fire.
#   CapNeverExceeded (M20Caps)       — DEC-P98-COUNT-IN-TXN: the atomic row-COUNT-in-txn gates the
#                     cap. Seed: non-atomic count-then-claim (TOCTOU) -> running = cap + 1.
#   NoCapWedge       (M20Parked)     — DEC-M20-D1 / parked-column: a parked child is cap-EXEMPT ->
#                     deadlock-free. Seed: parked counts -> K parked parents wedge the cap.
#
# Requires TLC: java17 + tla2tools.jar. System `java` is a stub on this machine -> use openjdk@17.
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

# arm <name> <spec> <ok-cfg> <break-cfg> <invariant>
arm() {
  local name="$1" spec="$2" okcfg="$3" brkcfg="$4" inv="$5"
  echo "== $name OK — $inv must HOLD, exhaustive =="
  TLC -config "$okcfg" "$spec" > "/tmp/${name}_ok.log" 2>&1 || true
  if grep -q "No error has been found" "/tmp/${name}_ok.log"; then
    grep -E "distinct states" "/tmp/${name}_ok.log" | tail -1
    echo "  GREEN  $inv HOLDS"
  else
    echo "  FAIL   $name OK did not verify clean"; tail -5 "/tmp/${name}_ok.log"; exit 1
  fi
  echo "== $name BREAK — $inv must FALSIFY (seed-the-break) =="
  TLC -config "$brkcfg" "$spec" > "/tmp/${name}_break.log" 2>&1 || true
  if grep -qE "Invariant $inv is violated|Deadlock reached" "/tmp/${name}_break.log"; then
    echo "  GREEN  seed-break FALSIFIES $inv (the guard is load-bearing)"
  else
    echo "  FAIL   seed-break did NOT falsify — $inv is vacuous/non-biting"; tail -5 "/tmp/${name}_break.log"; exit 1
  fi
  echo ""
}

arm "M20Scheduling" M20Scheduling.tla M20Scheduling.cfg M20SchedulingBreak.cfg NoDoubleFire
arm "M20Caps"       M20Caps.tla       M20Caps.cfg       M20CapsBreak.cfg       CapNeverExceeded
arm "M20Parked"     M20Parked.tla     M20Parked.cfg     M20ParkedBreak.cfg     NoWedge

echo "ALL GREEN — the M20 scheduling+cap capstone holds exhaustively; every guard's mutation reddens."
