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

# Set up environment
BENCHMARK_DIR="./internal/workflow/benchmark"
RESULTS_DIR="./benchmark_results"
mkdir -p "$RESULTS_DIR/reports"
mkdir -p "$RESULTS_DIR/profiles"
mkdir -p "$RESULTS_DIR/charts"

# Benchmark parameters
BENCHTIME="1s"
COUNT=2
TIMEOUT="10m"

# Print section header
print_header() {
    echo -e "\n${YELLOW}=== $1 ===${NC}"
}

# Run benchmarks with memory profiling
run_benchmark() {
    NAME=$1
    PATTERN=$2
    MEM_PROFILE="$RESULTS_DIR/profiles/$NAME-mem.prof"
    CPU_PROFILE="$RESULTS_DIR/profiles/$NAME-cpu.prof"
    RESULTS="$RESULTS_DIR/$NAME-results.txt"
    TRACE="$RESULTS_DIR/profiles/$NAME-trace.out"

    print_header "Running $NAME benchmarks ($PATTERN)"
    go test -benchmem -run=^$ \
      -bench="$PATTERN" \
      -benchtime="$BENCHTIME" \
      -count="$COUNT" \
      -timeout="$TIMEOUT" \
      -cpuprofile="$CPU_PROFILE" \
      -memprofile="$MEM_PROFILE" \
      -trace="$TRACE" \
      "$BENCHMARK_DIR" | tee "$RESULTS"
    
    # Generate memory allocation report
    go tool pprof -alloc_space -cum -nodecount=20 -text "$MEM_PROFILE" > "$RESULTS_DIR/reports/$NAME-mem-allocs.txt"
    go tool pprof -alloc_objects -cum -nodecount=20 -text "$MEM_PROFILE" > "$RESULTS_DIR/reports/$NAME-mem-objects.txt"
    
    # Generate CPU profile report
    go tool pprof -cum -nodecount=20 -text "$CPU_PROFILE" > "$RESULTS_DIR/reports/$NAME-cpu.txt"
    
    echo -e "${GREEN}Results saved to $RESULTS${NC}"
    echo -e "${BLUE}CPU profile: $CPU_PROFILE${NC}"
    echo -e "${BLUE}Memory profile: $MEM_PROFILE${NC}"
}

print_header "Starting Enhanced Workflow Benchmarks"
echo "Results will be saved to $RESULTS_DIR"

# Run real-world scenario benchmarks
run_benchmark "real_world_scenarios" "BenchmarkRealWorldScenarios"

# Run scalability benchmarks
run_benchmark "scalability" "BenchmarkScalability"

# Run error handling benchmarks
run_benchmark "error_handling" "BenchmarkErrorHandling"

# Run storage benchmarks
run_benchmark "storage" "BenchmarkStorage"

# Run general workflow data operations benchmarks
run_benchmark "workflow_data" "BenchmarkWorkflowData"

# Run DAG operation benchmarks
run_benchmark "dag_operations" "BenchmarkDAG"

# Run memory pattern benchmarks
run_benchmark "memory_patterns" "BenchmarkMemory"

# Run serialization benchmarks
run_benchmark "json_serialization" "BenchmarkJSON"
run_benchmark "flatbuffers_serialization" "BenchmarkFlatBuffers"

# Generate summary report
print_header "Generating Summary Report"

# Get the top benchmark results for JSON vs FlatBuffers
JSON_RESULTS=$(grep "^Benchmark.*JSON" "$RESULTS_DIR/storage-results.txt" | sort -k 1,1)
FB_RESULTS=$(grep "^Benchmark.*FlatBuffer" "$RESULTS_DIR/flatbuffers_serialization-results.txt" | sort -k 1,1)

# Get throughput results for different DAG topologies
DAG_RESULTS=$(grep "^BenchmarkScalability/DagTopologyScaling" "$RESULTS_DIR/scalability-results.txt")

# Get workflow data operation results
DATA_RESULTS=$(grep "^BenchmarkWorkflowData" "$RESULTS_DIR/workflow_data-results.txt")

# Generate summary report
{
    echo "# Workflow System Benchmark Summary"
    echo "## Generated on $(date)"
    echo
    echo "## Real-World Workflow Scenarios"
    echo "These benchmarks simulate complete real-world workflow patterns."
    echo
    echo "| Scenario | Operations/sec | Avg Time | Allocations |"
    echo "|----------|---------------|----------|-------------|"
    grep "^BenchmarkRealWorldScenarios" "$RESULTS_DIR/real_world_scenarios-results.txt" | 
    awk '{ printf "| %s | %s ops/s | %s ns/op | %s B/op, %s allocs/op |\n", $1, $3, $4, $6, $8 }'
    echo
    
    echo "## Storage Backend Comparison"
    echo "These benchmarks compare different storage backends for saving and loading workflow state."
    echo
    echo "| Operation | Backend | Data Size | Operations/sec | Avg Time | Allocations |"
    echo "|-----------|---------|-----------|---------------|----------|-------------|"
    grep "^BenchmarkStorage" "$RESULTS_DIR/storage-results.txt" | 
    awk '{ 
      split($1, parts, "/"); 
      op = parts[1]; sub("BenchmarkStorage/", "", op);
      backend = parts[2];
      size = ""; if (length(parts) > 2) { size = parts[2]; sub("DataSize=", "", size); }
      printf "| %s | %s | %s | %s ops/s | %s ns/op | %s B/op, %s allocs/op |\n", 
        op, backend, size, $3, $4, $6, $8 
    }'
    echo
    
    echo "## Error Handling & Recovery Performance"
    echo "These benchmarks measure the overhead of error handling and recovery mechanisms."
    echo
    echo "| Operation | Configuration | Operations/sec | Avg Time | Allocations |"
    echo "|-----------|---------------|---------------|----------|-------------|"
    grep "^BenchmarkErrorHandling" "$RESULTS_DIR/error_handling-results.txt" | 
    awk '{ 
      split($1, parts, "/"); 
      op = parts[1]; sub("BenchmarkErrorHandling/", "", op);
      config = ""; if (length(parts) > 1) { config = parts[2]; }
      printf "| %s | %s | %s ops/s | %s ns/op | %s B/op, %s allocs/op |\n", 
        op, config, $3, $4, $6, $8 
    }'
    echo
    
    echo "## Scalability Results"
    echo "These benchmarks measure how performance scales with different configurations."
    echo
    echo "| Dimension | Configuration | Operations/sec | Avg Time | Allocations |"
    echo "|-----------|---------------|---------------|----------|-------------|"
    grep "^BenchmarkScalability" "$RESULTS_DIR/scalability-results.txt" | 
    awk '{ 
      split($1, parts, "/"); 
      dim = parts[1]; sub("BenchmarkScalability/", "", dim);
      config = ""; if (length(parts) > 1) { config = parts[2]; }
      printf "| %s | %s | %s ops/s | %s ns/op | %s B/op, %s allocs/op |\n", 
        dim, config, $3, $4, $6, $8 
    }'
    
    echo
    echo "## Recommendations"
    echo
    echo "Based on these benchmark results, consider these optimization opportunities:"
    echo
    echo "1. **Storage Optimization**: ${JSON_RESULTS:+Use FlatBuffers for larger datasets to reduce load/save times.}"
    echo "2. **DAG Topology**: Choose appropriate DAG topology based on your workflow's logical structure."
    echo "3. **Worker Pool Size**: Configure worker pool size based on your specific workflow patterns and resource constraints."
    echo "4. **Error Handling**: Use retries judiciously - they add overhead but improve reliability."
    echo
    echo "For detailed analysis, review the profile reports in $RESULTS_DIR/reports/ directory."
    
} > "$RESULTS_DIR/benchmark_summary.md"

echo -e "${GREEN}Summary report generated at $RESULTS_DIR/benchmark_summary.md${NC}"
echo -e "${BLUE}To visualize profiles, run: make visualize-profiles${NC}"

print_header "Benchmark Comparison"

# Compare JSON and FlatBuffers serialization
echo -e "${YELLOW}JSON vs FlatBuffers Performance:${NC}"
paste "$RESULTS_DIR/json_serialization-results.txt" "$RESULTS_DIR/flatbuffers_serialization-results.txt" | 
  grep "Save\|Load" | column -t

print_header "Benchmarks Completed"
echo -e "All benchmark results saved in ${GREEN}$RESULTS_DIR${NC}" 