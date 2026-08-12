#!/usr/bin/env bash
# CUR-007 self-test: prove preflight.sh actually BITES. A guard that is never
# observed failing is not a guard. We drive a deliberately MISMATCHED tag
# (v9.9.9, which cannot equal pkg/workflow.Version) and assert:
#   (a) the script exits NON-ZERO, and
#   (b) the failure is the tag<->version check, with a clear message.
# "A REPORTED bite is not a RUN bite" — so this RUNS the script and inspects the
# real exit code + stderr, it does not merely describe the expected behavior.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFLIGHT="$DIR/preflight.sh"

pass=0
fail=0
check() { # name, condition-already-evaluated ($? via caller), detail
	:
}

echo "== self-test 1: mismatched tag v9.9.9 must FAIL the tag<->version check =="
set +e
OUT="$(bash "$PREFLIGHT" v9.9.9 2>&1)"
RC=$?
set -e
echo "--- captured output ---"
echo "$OUT"
echo "--- exit code: $RC ---"

if [ "$RC" -eq 0 ]; then
	echo "SELFTEST FAIL: expected non-zero exit for mismatched tag, got 0" >&2
	fail=$((fail + 1))
else
	echo "SELFTEST ok: exit was non-zero ($RC)"
	pass=$((pass + 1))
fi

if echo "$OUT" | grep -q 'PREFLIGHT FAIL \[tag<->version\]'; then
	echo "SELFTEST ok: bite fired on the tag<->version check with a clear message"
	pass=$((pass + 1))
else
	echo "SELFTEST FAIL: expected a tag<->version failure message, not found" >&2
	fail=$((fail + 1))
fi

echo
echo "== self-test 2: missing tag arg must FAIL the args check =="
set +e
OUT2="$(bash "$PREFLIGHT" 2>&1)"
RC2=$?
set -e
echo "--- exit code: $RC2 ---"
if [ "$RC2" -ne 0 ] && echo "$OUT2" | grep -q 'PREFLIGHT FAIL \[args\]'; then
	echo "SELFTEST ok: missing-arg bite fired"
	pass=$((pass + 1))
else
	echo "SELFTEST FAIL: missing-arg case did not bite cleanly (rc=$RC2)" >&2
	fail=$((fail + 1))
fi

echo
echo "== summary: $pass passed, $fail failed =="
[ "$fail" -eq 0 ] || exit 1
echo "SELFTEST PASSED: preflight.sh bites as designed"
