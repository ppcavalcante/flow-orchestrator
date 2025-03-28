#!/bin/bash

# run_property_tests.sh
# This script runs all property-based tests in the Flow Orchestrator project.
# Property-based tests verify that core components satisfy fundamental invariants
# across a wide range of inputs.

set -e

# Text formatting
BOLD="\033[1m"
GREEN="\033[32m"
YELLOW="\033[33m"
RED="\033[31m"
RESET="\033[0m"

# Function to print section headers
print_header() {
    echo -e "\n${BOLD}${GREEN}$1${RESET}"
    echo -e "${YELLOW}$(printf '=%.0s' {1..50})${RESET}\n"
}

# Function to run property tests for a specific package and test pattern
run_property_test() {
    local package=$1
    local test_pattern=$2
    local description=$3

    echo -e "${BOLD}Testing: ${description}${RESET}"
    
    if go test -v "./$package" -run "$test_pattern"; then
        echo -e "${GREEN}✓ Passed: $description${RESET}\n"
        return 0
    else
        echo -e "${RED}✗ Failed: $description${RESET}\n"
        return 1
    fi
}

# Main script
print_header "Flow Orchestrator Property-Based Tests"

echo -e "Running property-based tests to verify core invariants of the system.\n"

# Track failures
failures=0

# Workflow Engine Properties
print_header "Workflow Engine Properties"
if ! run_property_test "pkg/workflow" "TestWorkflowProperties" "Workflow Engine"; then
    ((failures++))
fi

# Memory Management Properties
print_header "Memory Management Properties"

if ! run_property_test "internal/workflow/arena" "TestArenaProperties" "Arena Memory Allocator"; then
    ((failures++))
fi

if ! run_property_test "internal/workflow/arena" "TestStringPoolProperties" "String Pool"; then
    ((failures++))
fi

if ! run_property_test "internal/workflow/memory" "TestBufferPoolProperties" "Buffer Pool"; then
    ((failures++))
fi

if ! run_property_test "internal/workflow/memory" "TestNodePoolProperties" "Node Pool"; then
    ((failures++))
fi

# Concurrent Data Structures Properties
print_header "Concurrent Data Structures Properties"

if ! run_property_test "internal/workflow/concurrent" "TestConcurrentMapProperties" "Concurrent Map"; then
    ((failures++))
fi

if ! run_property_test "internal/workflow/concurrent" "TestReadMapProperties" "Read Map (Lock-Free Map)"; then
    ((failures++))
fi

# Summary
print_header "Summary"

if [ $failures -eq 0 ]; then
    echo -e "${GREEN}All property-based tests passed successfully!${RESET}"
    echo -e "This indicates that the core components of Flow Orchestrator satisfy their fundamental invariants."
    exit 0
else
    echo -e "${RED}$failures property-based test(s) failed.${RESET}"
    echo -e "Please review the output above for details."
    exit 1
fi 