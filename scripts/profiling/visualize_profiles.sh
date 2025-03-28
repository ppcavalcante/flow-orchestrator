#!/bin/bash

# Set the project root (assuming script is in scripts/profiling/)
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Check if graphviz is installed
if ! command -v dot &> /dev/null; then
    echo -e "${RED}Error: graphviz is not installed${NC}"
    echo "Please install graphviz first:"
    echo "  macOS: brew install graphviz"
    echo "  Linux: sudo apt-get install graphviz"
    echo "  Windows: choco install graphviz"
    exit 1
fi

# Create visualization directory
mkdir -p visualizations

# Function to generate visualizations for a profile
generate_visualizations() {
    local profile_type=$1
    local profile_file=$2
    local output_base="visualizations/${profile_type}"
    
    echo -e "\n${YELLOW}=== Generating ${profile_type} Visualizations ===${NC}"
    
    # Generate SVG call graph
    echo -e "${BLUE}Generating call graph...${NC}"
    go tool pprof -svg profiles/${profile_file} > ${output_base}_callgraph.svg
    echo -e "${GREEN}Call graph saved to ${output_base}_callgraph.svg${NC}"
    
    # Generate flame graph
    echo -e "${BLUE}Generating flame graph...${NC}"
    go tool pprof -http=localhost:8081 profiles/${profile_file} &
    PPROF_PID=$!
    echo -e "${GREEN}Flame graph server started. Visit the URL above in your browser.${NC}"
    echo -e "${YELLOW}Press Enter to stop the server...${NC}"
    read
    kill $PPROF_PID
    
    # Generate dot graph
    echo -e "${BLUE}Generating dot graph...${NC}"
    go tool pprof -dot profiles/${profile_file} > ${output_base}.dot
    dot -Tpng ${output_base}.dot -o ${output_base}_dotgraph.png
    echo -e "${GREEN}Dot graph saved to ${output_base}_dotgraph.png${NC}"
    
    # Generate text report
    echo -e "${BLUE}Generating detailed text report...${NC}"
    go tool pprof -text -nodecount=20 profiles/${profile_file} > ${output_base}_report.txt
    echo -e "${GREEN}Text report saved to ${output_base}_report.txt${NC}"
}

echo -e "${YELLOW}=== Workflow System Profile Visualizations ===${NC}"

# CPU Profiles
for component in dag state middleware monad; do
    if [ -f "profiles/${component}_cpu.prof" ]; then
        generate_visualizations "${component}_cpu" "${component}_cpu.prof"
    fi
done

# Memory Profiles
for component in dag state middleware monad; do
    if [ -f "profiles/${component}_mem.prof" ]; then
        generate_visualizations "${component}_mem" "${component}_mem.prof"
    fi
done

# Block Profile
if [ -f "profiles/parallel_block.prof" ]; then
    generate_visualizations "parallel_block" "parallel_block.prof"
fi

# Mutex Profile
if [ -f "profiles/state_mutex.prof" ]; then
    generate_visualizations "state_mutex" "state_mutex.prof"
fi

echo -e "\n${YELLOW}=== Visualization Summary ===${NC}"
echo -e "The following visualizations have been generated in the 'visualizations' directory:"
echo -e "  - SVG call graphs (*_callgraph.svg)"
echo -e "  - PNG dot graphs (*_dotgraph.png)"
echo -e "  - Detailed text reports (*_report.txt)"
echo -e "\nTo view the visualizations:"
echo -e "  1. SVG files: Open in a web browser"
echo -e "  2. PNG files: Open with any image viewer"
echo -e "  3. Text reports: Open with any text editor"
echo -e "\n${BLUE}Interactive Visualization:${NC}"
echo -e "To start an interactive web UI for any profile:"
echo -e "  go tool pprof -http=localhost:8081 profiles/<profile_name>.prof"
echo -e "\n${BLUE}Available Views in Web UI:${NC}"
echo -e "  - Graph: Interactive call graph"
echo -e "  - Flame Graph: Hierarchical view of call stacks"
echo -e "  - Top: Sorted list of hottest functions"
echo -e "  - Source: Annotated source code"
echo -e "  - Disassembly: Assembly code (if available)"
echo -e "\n${BLUE}Tips for Analysis:${NC}"
echo -e "  1. Focus on the thickest edges in call graphs"
echo -e "  2. Look for unexpected memory allocations"
echo -e "  3. Check for goroutine blocking patterns"
echo -e "  4. Identify mutex contention points" 