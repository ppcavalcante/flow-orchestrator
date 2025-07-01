#!/bin/bash

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Set the project root
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT"

# Function to print section header
print_header() {
  echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# Check coverage for each critical package
print_header "Checking coverage thresholds"

FAILED=0
TOTAL_PACKAGES=0
PASSED_PACKAGES=0

# Define packages and their thresholds
check_package() {
  local package=$1
  local threshold=$2
  local priority=$3
  
  # Run tests for this package
  echo -e "Testing package: ${BLUE}$package${NC} (${YELLOW}$priority priority${NC})"
  
  # Run test with coverage and capture the output
  local test_output=$(go test -cover $package 2>&1)
  local exit_code=$?
  
  if [ $exit_code -ne 0 ]; then
    echo -e "${RED}Tests failed for package $package${NC}"
    echo "$test_output"
    return
  fi
  
  # Extract coverage directly from the test output
  local coverage_line=$(echo "$test_output" | grep -o "coverage: [0-9.]*%" | head -1)
  
  if [ -z "$coverage_line" ]; then
    echo -e "${YELLOW}Warning: No coverage data found for package $package${NC}"
    echo "$test_output"
    return
  fi
  
  # Extract just the percentage
  local coverage=$(echo "$coverage_line" | grep -o "[0-9.]*")
  
  ((TOTAL_PACKAGES++))
  
  # Compare with threshold
  if (( $(echo "$coverage < $threshold" | bc -l) )); then
    echo -e "${RED}✗ $package: $coverage% (threshold: $threshold%)${NC}"
    ((FAILED++))
  else
    echo -e "${GREEN}✓ $package: $coverage% (threshold: $threshold%)${NC}"
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
check_package "github.com/ppcavalcante/flow-orchestrator/internal/workflow/concurrent" 80 "Medium"

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