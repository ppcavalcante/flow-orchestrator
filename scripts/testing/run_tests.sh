#!/bin/bash

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Set the project root (assuming script is in scripts/testing/)
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT"

# Default values
SHOW_COVERAGE=false
GEN_FLATBUFFERS=false

# Parse command line arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        -coverage) SHOW_COVERAGE=true ;;
        -flatbuffers) GEN_FLATBUFFERS=true ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

# Function to print section header
print_header() {
    echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# Function to print coverage instructions
print_coverage_instructions() {
    echo -e "\n${BLUE}To view coverage report in browser:${NC}"
    echo -e "go tool cover -html=coverage.out"
}

# Function to run tests and check result
run_test_suite() {
    local suite_name="$1"
    local test_path="$2"
    local coverage_file="$3"
    
    echo -e "\n${YELLOW}Running $suite_name...${NC}"
    
    if [ "$SHOW_COVERAGE" = true ]; then
        go test -v -coverprofile="$coverage_file" "$test_path"
    else
        go test -v "$test_path"
    fi
    
    local exit_code=$?
    
    if [ $exit_code -eq 0 ]; then
        echo -e "${GREEN}✓ $suite_name passed${NC}"
        
        if [ "$SHOW_COVERAGE" = true ]; then
            echo -e "\n${YELLOW}Coverage Report for $suite_name:${NC}"
            go tool cover -func="$coverage_file"
        fi
        
        return 0
    else
        echo -e "${RED}✗ $suite_name failed${NC}"
        return 1
    fi
}

# Function to merge coverage profiles
merge_coverage_profiles() {
    echo "mode: set" > coverage.out
    grep -h -v "^mode:" coverage.unit.out coverage.integration.out >> coverage.out
}

# Cleanup function
cleanup() {
    rm -f pkg/workflow/test_state.json
    rm -f pkg/workflow/integration/test_state.json
    if [ "$SHOW_COVERAGE" = false ]; then
        rm -f coverage.*.out coverage.out
    fi
    echo "Cleaned up test state files"
}

# Generate FlatBuffers code if requested
if [ "$GEN_FLATBUFFERS" = true ]; then
    print_header "Generating FlatBuffers Code"
    if ! make generate-fb; then
        echo -e "${RED}Failed to generate FlatBuffers code${NC}"
        exit 1
    fi
    echo -e "${GREEN}FlatBuffers code generated successfully${NC}"
fi

# Initialize test counters
total_suites=0
passed_suites=0

# Start testing
print_header "Workflow System Test Suite"

# Cleanup before starting
cleanup

# Build the project
print_header "Building the project"
if ! make build; then
    echo -e "${RED}Build failed${NC}"
    exit 1
fi
echo -e "${GREEN}Build succeeded${NC}"

# 1. Run unit tests
((total_suites++))
run_test_suite "Unit Tests" "./pkg/workflow" "coverage.unit.out" && ((passed_suites++))

# 2. Run integration tests
# ((total_suites++))
# run_test_suite "Integration Tests" "./pkg/workflow/integration" "coverage.integration.out" && ((passed_suites++))

# Merge coverage profiles if coverage is enabled
if [ "$SHOW_COVERAGE" = true ]; then
    merge_coverage_profiles
    print_header "Total Coverage Report"
    go tool cover -func=coverage.out
    print_coverage_instructions
fi

# Cleanup at the end
cleanup

# Print test summary
print_header "Test Summary"
echo -e "Total test suites: ${total_suites}"
echo -e "Passed test suites: ${GREEN}${passed_suites}${NC}"
echo -e "Failed test suites: ${RED}$((total_suites - passed_suites))${NC}"

# Set exit code based on test results
if [ $passed_suites -eq $total_suites ]; then
    echo -e "\n${GREEN}All test suites passed!${NC}"
    exit 0
else
    echo -e "\n${RED}Some test suites failed!${NC}"
    exit 1
fi 