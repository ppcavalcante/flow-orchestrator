#!/bin/bash

# Create directories for profiles
mkdir -p benchmark_profiles/cpu
mkdir -p benchmark_profiles/mem
mkdir -p benchmark_profiles/reports
mkdir -p benchmark_profiles/trace
mkdir -p benchmark_profiles/analisys

# Helper function to run benchmark with profiling
run_bench() {
  BENCH_NAME=$1
  echo "Running $BENCH_NAME benchmark..."
  go test -run=XXX -bench=$BENCH_NAME -benchmem \
    -cpuprofile=benchmark_profiles/cpu/${BENCH_NAME}_cpu.prof \
    -memprofile=benchmark_profiles/mem/${BENCH_NAME}_mem.prof \
    -trace=benchmark_profiles/trace/${BENCH_NAME}_trace.out \
    ./internal/workflow/benchmark
    
  # Generate CPU profile text report
  go tool pprof -text benchmark_profiles/cpu/${BENCH_NAME}_cpu.prof > benchmark_profiles/reports/${BENCH_NAME}_cpu.txt
  
  # Generate memory profile text report
  go tool pprof -text benchmark_profiles/mem/${BENCH_NAME}_mem.prof > benchmark_profiles/reports/${BENCH_NAME}_mem.txt
  
  echo "Profiles saved for $BENCH_NAME"
  echo "--------------------------------------------"
}

# Run core component benchmarks
echo "=== Running Core Component Benchmarks ==="
run_bench "BenchmarkDAG"
run_bench "BenchmarkWorkflowData"
run_bench "BenchmarkNodeStatus"
run_bench "BenchmarkWorkflowDataSetGet"
run_bench "BenchmarkWorkflowDataDependencyResolution"

# Run memory optimization benchmarks
echo "=== Running Memory Optimization Benchmarks ==="
run_bench "BenchmarkArenaAllocation"
run_bench "BenchmarkArenaStringAllocation"
run_bench "BenchmarkStringPoolIntern"
run_bench "BenchmarkArenaReset"
run_bench "BenchmarkWorkflowDataArenaComparison"

# Run string interning benchmarks
echo "=== Running String Interning Benchmarks ==="
run_bench "BenchmarkFocusedStringInterning"

# Run concurrency model benchmarks
echo "=== Running Concurrency Model Benchmarks ==="
run_bench "BenchmarkFocusedConcurrency"
run_bench "BenchmarkFocusedConcurrentAccess"
run_bench "BenchmarkParallelExecution"

# Run integrated workflow benchmarks
echo "=== Running Integrated Workflow Benchmarks ==="
run_bench "BenchmarkIntegratedWorkflow"
run_bench "BenchmarkWorkflowWithGC"
run_bench "BenchmarkWorkflowReuse"
run_bench "BenchmarkFocusedWorkflowExecution"

# Run scalability benchmarks
echo "=== Running Scalability Benchmarks ==="
run_bench "BenchmarkScalability"
run_bench "BenchmarkLargeWorkflowOperations"

# Run error handling and recovery benchmarks
echo "=== Running Error Handling Benchmarks ==="
run_bench "BenchmarkErrorHandling"
run_bench "BenchmarkWorkflowRecoveryPattern"

# Run serialization benchmarks
echo "=== Running Serialization Benchmarks ==="
run_bench "BenchmarkSnapshotAndRestore"
run_bench "BenchmarkJSON"
run_bench "BenchmarkFlatBuffers"
run_bench "BenchmarkFocusedSerialization"

# Run real-world scenario benchmarks
echo "=== Running Real-World Scenario Benchmarks ==="
run_bench "BenchmarkRealWorld"

# Generate comparison report
echo "=== Generating Comparison Report ==="
echo "# Workflow System Benchmark Results" > benchmark_profiles/analisys/benchmark_summary.md
echo "Generated on: $(date)" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md
echo "## Core Component Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkDAG|BenchmarkWorkflowData|BenchmarkNodeStatus" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## Memory Optimization Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkArena|BenchmarkStringPool|BenchmarkWorkflowDataArenaComparison" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## String Interning Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkFocusedStringInterning" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## Concurrency Model Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkFocusedConcurrency|BenchmarkParallelExecution" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## Integrated Workflow Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkIntegratedWorkflow|BenchmarkWorkflowWithGC|BenchmarkWorkflowReuse" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## Serialization Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkJSON|BenchmarkFlatBuffers|BenchmarkFocusedSerialization" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
echo "" >> benchmark_profiles/analisys/benchmark_summary.md

echo "## Real-World Scenario Performance" >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md
go test -run=XXX -bench="BenchmarkRealWorld" -benchmem ./internal/workflow/benchmark >> benchmark_profiles/analisys/benchmark_summary.md
echo "```" >> benchmark_profiles/analisys/benchmark_summary.md

echo "Benchmark analysis complete. Results saved to benchmark_profiles/analisys/benchmark_summary.md"