#!/bin/bash

# Coverage gate with a per-package ratchet.
#
# WHY A RATCHET (not a bare `coverage < threshold`):
#   The previous gate was a knife-edge `coverage < 90`. pkg/workflow sat at ~90.1%,
#   so deleting or refactoring ANY covered statement could drop the ratio under 90
#   and turn CI red on a change that added no risk. That false-red was "fixed" twice
#   in the release history by DELETING covered code to raise the ratio — managing the
#   number, not the risk. The honest way to raise coverage is to TEST branches; the
#   gate should reward that and stop punishing unrelated deletions.
#
# THE DESIGN (two independent guards; a package must clear BOTH):
#   1. HARD FLOOR  — a per-package absolute minimum (e.g. pkg/workflow = 90). This is
#      the contractual bar and never moves. Coverage below the floor always fails.
#   2. RATCHET      — a per-package high-water baseline recorded in
#      scripts/testing/coverage_baselines.txt. Coverage is allowed to dip at most
#      RATCHET_EPSILON below the recorded baseline before it fails. This catches
#      SILENT EROSION (a slow slide that stays above the floor) that a bare floor
#      misses, while RATCHET_EPSILON absorbs the sub-percent jitter from unrelated
#      covered-statement churn so a no-risk change does not false-red.
#      When coverage RISES above the baseline, the baseline auto-ratchets UP (the
#      new high-water mark is written back), so the bar only ever tightens — gains
#      are locked in, never silently lost.
#
# UPDATING BASELINES:
#   Normal runs auto-write improvements back to coverage_baselines.txt; commit that
#   file when coverage legitimately rises. To intentionally LOWER a baseline (rare —
#   e.g. removing a whole well-tested subsystem), edit the file by hand in the same
#   commit so the drop is a reviewed, deliberate act, not an accident.
#   Set RATCHET=0 to run floor-only (e.g. a first run with no baseline file).

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Set the project root
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT" || exit 2

# Ratchet configuration.
BASELINE_FILE="scripts/testing/coverage_baselines.txt"
RATCHET="${RATCHET:-1}"            # 1 = ratchet enabled (default), 0 = floor-only
RATCHET_EPSILON="${RATCHET_EPSILON:-0.3}" # allowed dip (pct points) below baseline

# Function to print section header
print_header() {
  echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# read_baseline <package> -> echoes the recorded baseline % or empty if none.
read_baseline() {
  local package=$1
  [ -f "$BASELINE_FILE" ] || return 0
  # Lines are "<package> <pct>"; ignore comments/blank.
  grep -E "^${package}[[:space:]]" "$BASELINE_FILE" 2>/dev/null | awk '{print $2}' | head -1
}

# write_baseline <package> <pct> — upsert the baseline line atomically.
write_baseline() {
  local package=$1
  local pct=$2
  local tmp
  tmp="$(mktemp)"
  if [ -f "$BASELINE_FILE" ]; then
    grep -vE "^${package}[[:space:]]" "$BASELINE_FILE" > "$tmp" 2>/dev/null
  else
    {
      echo "# Per-package coverage high-water baselines for the ratchet gate."
      echo "# Auto-updated upward by check_coverage.sh; commit increases. See the"
      echo "# script header for how to intentionally lower a baseline. Format: <pkg> <pct>"
    } > "$tmp"
  fi
  echo "${package} ${pct}" >> "$tmp"
  # Keep entries sorted (comments float to top via the '#' sort order is not
  # guaranteed, so preserve header by re-prepending if it was a fresh file).
  mv "$tmp" "$BASELINE_FILE"
}

print_header "Checking coverage thresholds"

FAILED=0
TOTAL_PACKAGES=0
PASSED_PACKAGES=0

# check_package <package> <floor> <priority>
check_package() {
  local package=$1
  local threshold=$2
  local priority=$3

  echo -e "Testing package: ${BLUE}$package${NC} (${YELLOW}$priority priority${NC})"

  # Run test with coverage and capture the output
  local test_output
  test_output=$(go test -cover "$package" 2>&1)
  local exit_code=$?

  if [ $exit_code -ne 0 ]; then
    echo -e "${RED}Tests failed for package $package${NC}"
    echo "$test_output"
    # A failing test suite is a coverage-gate FAILURE — count it and flag it so
    # the script cannot exit 0 with red tests (the swallow-bug fix). Previously
    # this bare `return` left FAILED=0 and TOTAL unincremented -> green on red.
    ((TOTAL_PACKAGES++))
    ((FAILED++))
    return
  fi

  # Extract coverage directly from the test output
  local coverage_line
  coverage_line=$(echo "$test_output" | grep -o "coverage: [0-9.]*%" | head -1)

  if [ -z "$coverage_line" ]; then
    echo -e "${YELLOW}Warning: No coverage data found for package $package${NC}"
    echo "$test_output"
    return
  fi

  # Extract just the percentage
  local coverage
  coverage=$(echo "$coverage_line" | grep -o "[0-9.]*")

  ((TOTAL_PACKAGES++))

  local pkg_failed=0

  # Guard 1: HARD FLOOR (never moves).
  if (( $(echo "$coverage < $threshold" | bc -l) )); then
    echo -e "${RED}✗ $package: $coverage% < floor $threshold%${NC}"
    pkg_failed=1
  fi

  # Guard 2: RATCHET (high-water baseline minus epsilon).
  if [ "$RATCHET" = "1" ]; then
    local baseline
    baseline=$(read_baseline "$package")
    if [ -n "$baseline" ]; then
      local min_allowed
      min_allowed=$(echo "$baseline - $RATCHET_EPSILON" | bc -l)
      if (( $(echo "$coverage < $min_allowed" | bc -l) )); then
        echo -e "${RED}✗ $package: $coverage% dropped > ${RATCHET_EPSILON}pt below baseline ${baseline}% (min ${min_allowed}%)${NC}"
        echo -e "${RED}  coverage eroded — add tests for the lost branches, or lower the baseline deliberately in this commit.${NC}"
        pkg_failed=1
      elif (( $(echo "$coverage > $baseline" | bc -l) )); then
        # Ratchet UP: lock in the gain so it can't silently regress later.
        write_baseline "$package" "$coverage"
        echo -e "${GREEN}↑ $package: $coverage% — baseline ratcheted up from ${baseline}%${NC}"
      fi
    else
      # First sighting of this package: seed the baseline at the current value.
      write_baseline "$package" "$coverage"
      echo -e "${BLUE}• $package: $coverage% — baseline seeded${NC}"
    fi
  fi

  if [ "$pkg_failed" = "1" ]; then
    ((FAILED++))
  else
    echo -e "${GREEN}✓ $package: $coverage% (floor ${threshold}%${RATCHET:+, ratchet on})${NC}"
    ((PASSED_PACKAGES++))
  fi
}

# Check each package according to the test coverage strategy
# Critical packages (90% coverage)
check_package "github.com/ppcavalcante/flow-orchestrator/pkg/workflow" 90 "Critical"

# High priority packages (90% coverage)
check_package "github.com/ppcavalcante/flow-orchestrator/internal/workflow/arena" 90 "High"
check_package "github.com/ppcavalcante/flow-orchestrator/internal/workflow/memory" 90 "High"

# Medium priority packages (70-80% coverage)
check_package "github.com/ppcavalcante/flow-orchestrator/internal/workflow/metrics" 70 "Medium"
check_package "github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils" 70 "Medium"

# Print summary
print_header "Coverage Summary"
echo -e "Total packages checked: ${TOTAL_PACKAGES}"
echo -e "Packages meeting threshold: ${GREEN}${PASSED_PACKAGES}${NC}"
echo -e "Packages below threshold: ${RED}${FAILED}${NC}"

# Exit with error if any package is below threshold
if [ $FAILED -gt 0 ]; then
  echo -e "\n${RED}Some packages are below their coverage thresholds!${NC}"
  exit 1
else
  echo -e "\n${GREEN}All packages meet their coverage thresholds!${NC}"
  exit 0
fi
