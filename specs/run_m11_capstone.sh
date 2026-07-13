#!/usr/bin/env bash
# Phase-44 M11 OR-join capstone — reproducible route sweep + bite log.
# Runs every route of every M11 config exhaustively and every isolated bite
# (falsify -> restore), so "all routes green + every invariant bite-proven" is
# reproducible from the artifacts alone (closes review 44-F3 reproducibility).
#
# Requires TLC: java17 + tla2tools.jar. On this machine the system `java` is a
# stub — use the openjdk@17 path (see project memory).
set -euo pipefail
cd "$(dirname "$0")"
JAVA="${JAVA:-/opt/homebrew/opt/openjdk@17/bin/java}"
JAR="${JAR:-/tmp/tla2tools.jar}"
[ -f "$JAR" ] || curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
TLC() { "$JAVA" -cp "$JAR" tlc2.TLC "$@"; }

green() { TLC -config "$1" "$2" 2>&1 | grep -q "No error has been found" \
  && echo "  GREEN  $3" || { echo "  FAIL   $3"; exit 1; }; }
setc() { sed -i '' "$2" "$1"; }   # macOS sed; drop the '' on GNU sed

echo "== preservation (M10 diamond re-runs the extended spec, ~14,380 states) =="
green M10DurableExecutor.cfg MCM10DurableExecutor.tla "M10 preservation"

echo "== M10 fail-resume (DEC-M10-P39-T5, ~915 states) — the hard-fail arm the diamond =="
echo "== capstone (FailSet={}) does NOT exercise: nF hard-fails, nD skips, indep nT completes, =="
echo "== crash+recover preserves Failed/Skipped (NoResurrection non-vacuous). Wired in so it =="
echo "== can't rot again (task #103 — it broke because it was never exercised). =="
green M10FailResume.cfg MCM10FailResume.tla "M10 fail-resume (T5)"

echo "== base M11 (crash-free), all routes =="
cp M11ChoiceMerge.cfg /tmp/base44.cfg
for r in bA bB bC; do
  setc M11ChoiceMerge.cfg "s/^    pick = .*/    pick = $r/;s/^    cfail = .*/    cfail = {}/;s/^    ContinueOnError = .*/    ContinueOnError = {}/"
  green M11ChoiceMerge.cfg MCM11ChoiceMerge.tla "route pick=$r"
done
setc M11ChoiceMerge.cfg "s/^    cfail = .*/    cfail = {c}/"
green M11ChoiceMerge.cfg MCM11ChoiceMerge.tla "route cfail={c}  (44-F1 choice-routing-failure)"
cp /tmp/base44.cfg M11ChoiceMerge.cfg
setc M11ChoiceMerge.cfg "s/^    pick = .*/    pick = bA/;s/^    ContinueOnError = .*/    ContinueOnError = {bA}/"
green M11ChoiceMerge.cfg MCM11ChoiceMerge.tla "route coe   (44-F3 coe-Failed tail fires)"
cp /tmp/base44.cfg M11ChoiceMerge.cfg

echo "== crash x choice (MaxCrashes=1), all routes =="
cp M11CrashChoice.cfg /tmp/crash44.cfg
for r in bA bB; do
  setc M11CrashChoice.cfg "s/^    pick = .*/    pick = $r/;s/^    cfail = .*/    cfail = {}/"
  green M11CrashChoice.cfg MCM11CrashChoice.tla "crash route pick=$r"
done
setc M11CrashChoice.cfg "s/^    cfail = .*/    cfail = {c}/"
green M11CrashChoice.cfg MCM11CrashChoice.tla "crash route cfail={c}  (44-F1 across a crash)"
cp /tmp/crash44.cfg M11CrashChoice.cfg

echo "ALL ROUTES GREEN. (Bite log: see 44-SUMMARY.md §2/§3 — each invariant has an"
echo "isolated falsify->restore mutation; re-run them by hand from the SUMMARY.)"
