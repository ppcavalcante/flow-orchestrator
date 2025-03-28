#!/bin/bash

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print section header
print_header() {
    echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# Function to run test and check result
run_test() {
    local test_name="$1"
    local command="$2"
    local expected_exit="$3"
    
    echo -e "\n${YELLOW}Testing: ${test_name}${NC}"
    echo "Command: $command"
    
    # Clean up state file before each test
    rm -f api_workflow_state.json
    
    eval "$command"
    local exit_code=$?
    
    if [ $exit_code -eq $expected_exit ]; then
        echo -e "${GREEN}✓ Test passed (Exit code: $exit_code)${NC}"
        return 0
    else
        echo -e "${RED}✗ Test failed (Expected: $expected_exit, Got: $exit_code)${NC}"
        return 1
    fi
}

# Cleanup function
cleanup() {
    rm -f api_workflow_state.json
    echo "Cleaned up state files"
}

# Initialize test counters
total_tests=0
passed_tests=0

# Run test and update counters
execute_test() {
    local name="$1"
    local cmd="$2"
    local expected="$3"
    
    ((total_tests++))
    run_test "$name" "$cmd" "$expected" && ((passed_tests++))
}

# Start testing
print_header "API Workflow Test Scenarios"

# Cleanup before starting
cleanup

# 1. Basic successful execution
print_header "1. Basic Execution Tests"
execute_test "Normal execution" \
    "go run main.go" \
    0

# 2. Failure scenarios
print_header "2. Failure Scenario Tests"

execute_test "Fetch failure" \
    "go run main.go --force-fetch-fail" \
    1

execute_test "Process failure" \
    "go run main.go --force-process-fail" \
    1

execute_test "API failure" \
    "go run main.go --force-api-fail" \
    1

execute_test "DB failure" \
    "go run main.go --force-db-fail" \
    1

# 3. Retry scenarios
print_header "3. Retry Behavior Tests"

execute_test "Retry with verbose output" \
    "go run main.go --verbose-retries --fetch-fail-rate=0.2" \
    0

execute_test "Multiple retries with API failures" \
    "go run main.go --verbose-retries --api-fail-rate=0.3" \
    0

# 4. Performance scenarios
print_header "4. Performance Tests"

execute_test "Slow API simulation" \
    "go run main.go --slow-api --fetch-fail-rate=0.0 --api-fail-rate=0.0 --db-fail-rate=0.0" \
    0

execute_test "Custom worker pool" \
    "go run main.go --use-worker-pool --worker-pool-size=8 --max-queue-size=200 --fetch-fail-rate=0.0 --api-fail-rate=0.0 --db-fail-rate=0.0" \
    0

# 5. Combined scenarios
print_header "5. Combined Scenario Tests"

execute_test "High failure rates with retries" \
    "go run main.go --fetch-fail-rate=0.3 --api-fail-rate=0.3 --db-fail-rate=0.3 --verbose-retries" \
    0

execute_test "Slow execution with worker pool" \
    "go run main.go --slow-api --use-worker-pool --worker-pool-size=4 --fetch-fail-rate=0.0 --api-fail-rate=0.0 --db-fail-rate=0.0" \
    0

# 6. State management
print_header "6. State Management Tests"

# First run to create state
execute_test "Create initial state" \
    "go run main.go --fetch-fail-rate=0.0 --api-fail-rate=0.0 --db-fail-rate=0.0" \
    0

# Try to resume from state
execute_test "Resume from state" \
    "go run main.go --resume-state=api_workflow_state.json --fetch-fail-rate=0.0 --api-fail-rate=0.0 --db-fail-rate=0.0" \
    0

# Cleanup at the end
cleanup

# Print test summary
print_header "Test Summary"
echo -e "Total tests: ${total_tests}"
echo -e "Passed tests: ${GREEN}${passed_tests}${NC}"
echo -e "Failed tests: ${RED}$((total_tests - passed_tests))${NC}"

# Set exit code based on test results
if [ $passed_tests -eq $total_tests ]; then
    echo -e "\n${GREEN}All tests passed!${NC}"
    exit 0
else
    echo -e "\n${RED}Some tests failed!${NC}"
    exit 1
fi 