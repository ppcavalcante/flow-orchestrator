#!/bin/bash

# Set the project root (assuming script is in scripts/tools/)
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print section header
print_header() {
    echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# Check if FlatBuffers compiler is installed
print_header "Checking FlatBuffers Compiler"
if ! command -v flatc &> /dev/null; then
    echo -e "${RED}Error: flatc command not found.${NC}"
    echo -e "Please install the FlatBuffers compiler:"
    echo -e "${BLUE}For macOS:${NC} brew install flatbuffers"
    echo -e "${BLUE}For Ubuntu/Debian:${NC} sudo apt-get install flatbuffers-compiler"
    echo -e "${BLUE}For CentOS/RHEL:${NC} sudo yum install flatbuffers"
    echo -e "${BLUE}From source:${NC} https://github.com/google/flatbuffers/releases"
    echo -e "\nSee README-FLATBUFFERS.md for more details."
    exit 1
fi

FLATC_VERSION=$(flatc --version)
echo -e "${GREEN}FlatBuffers compiler found: ${FLATC_VERSION}${NC}"

# Check FlatBuffers schema file
SCHEMA_FILE="pkg/workflow/schema/workflow_data.fbs"
print_header "Checking FlatBuffers Schema"
if [ ! -f "$SCHEMA_FILE" ]; then
    echo -e "${RED}Error: Schema file not found at ${SCHEMA_FILE}${NC}"
    exit 1
fi

echo -e "${GREEN}Schema file found at ${SCHEMA_FILE}${NC}"

# Validate the schema by trying to generate a temporary file
print_header "Validating FlatBuffers Schema"
echo -e "Validating schema... "

# Create a temporary directory
TMP_DIR=$(mktemp -d)
VALIDATION_OUTPUT=$(flatc --go -o "$TMP_DIR" "$SCHEMA_FILE" 2>&1)
VALIDATION_EXIT_CODE=$?

# Clean up temporary directory
rm -rf "$TMP_DIR"

if [ $VALIDATION_EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}Schema validation successful!${NC}"
else
    echo -e "${RED}Schema validation failed:${NC}"
    echo -e "$VALIDATION_OUTPUT"
    exit 1
fi

# Check output directory for generated code
FB_DIR="pkg/workflow/fb"
print_header "Checking Output Directory"
if [ ! -d "$FB_DIR" ]; then
    echo -e "${YELLOW}Output directory does not exist. Creating ${FB_DIR}${NC}"
    mkdir -p "$FB_DIR"
else
    echo -e "${GREEN}Output directory exists: ${FB_DIR}${NC}"
fi

# If this point is reached, everything looks good
print_header "FlatBuffers Setup Status"
echo -e "${GREEN}✓ FlatBuffers compiler is installed${NC}"
echo -e "${GREEN}✓ Schema file is valid${NC}"
echo -e "${GREEN}✓ Output directory is ready${NC}"
echo -e "\n${BLUE}You can now generate the FlatBuffers code with:${NC}"
echo -e "make generate-fb"
echo -e "\n${BLUE}Or run:${NC}"
echo -e "./scripts/testing/run_tests.sh -flatbuffers\n"

exit 0 