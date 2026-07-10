package workflow

// Phase 54 (M13) — performance validation + 1.0 baseline. Benchmarks over 3 realistic DAG
// shapes at scale points N ∈ {10,100,1000}, each run WITH and WITHOUT a durable store so the
// durability/checkpoint overhead is isolated (Store=nil is the non-durable baseline; an FB
// Checkpointer store is durable). MEASURE, don't tune — a slow shape is an M13 finding, not
// a change. Arch: arm64 (stated; no amd64 claim, DEC-M13-ARCH). Run:
//   go test ./pkg/workflow -run='^$' -bench='BenchmarkPerf' -benchmem -count=6

import (
	"context"
	"fmt"
	"testing"
)

var perfSink error

func perfNoop(context.Context, *WorkflowData) error { return nil }

// buildWide: one root fanning out to N parallel leaves (one wide level).
func buildWide(n int) *DAG {
	b := NewWorkflowBuilder().WithWorkflowID("wide")
	b.AddStartNode("root").WithAction(perfNoop)
	for i := 0; i < n; i++ {
		b.AddNode(fmt.Sprintf("leaf-%d", i)).WithAction(perfNoop).DependsOn("root")
	}
	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	return dag
}

// buildDeep: N sequential levels (a chain of N nodes).
func buildDeep(n int) *DAG {
	b := NewWorkflowBuilder().WithWorkflowID("deep")
	prev := "n-0"
	b.AddStartNode(prev).WithAction(perfNoop)
	for i := 1; i < n; i++ {
		name := fmt.Sprintf("n-%d", i)
		b.AddNode(name).WithAction(perfNoop).DependsOn(prev)
		prev = name
	}
	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	return dag
}

// buildLargeData: a fixed 10-node chain where each node writes a payloadKB-sized output.
// Here N is the PER-NODE PAYLOAD SIZE in KiB (not the node count) — this shape scales DATA,
// isolating serialization cost (most visible under a durable store).
func buildLargeData(payloadKB int) *DAG {
	payload := make([]byte, payloadKB*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	set := func(name string) func(context.Context, *WorkflowData) error {
		return func(_ context.Context, d *WorkflowData) error {
			d.SetOutput(name, payload)
			return nil
		}
	}
	b := NewWorkflowBuilder().WithWorkflowID("large")
	prev := "d-0"
	b.AddStartNode(prev).WithAction(set(prev))
	for i := 1; i < 10; i++ {
		name := fmt.Sprintf("d-%d", i)
		b.AddNode(name).WithAction(set(name)).DependsOn(prev)
		prev = name
	}
	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	return dag
}

// benchRun drives `dag` b.N times, each a FRESH forward run (unique WorkflowID → no resume).
// store isolates the checkpoint cost across three points:
//
//	"none"  — Store=nil, NO checkpoint (the det-tax forward hot path);
//	"inmem" — InMemory Checkpointer, per-level checkpoint to MEMORY (isolates checkpoint LOGIC);
//	"fb"    — FlatBuffers Checkpointer, per-level checkpoint to DISK (durable; inmem→fb delta = disk I/O).
func benchRun(b *testing.B, dag *DAG, store string) {
	var newStore func() WorkflowStore
	switch store {
	case "none":
		newStore = func() WorkflowStore { return nil }
	case "inmem":
		newStore = func() WorkflowStore { return NewInMemoryStore() }
	case "fb":
		dir := b.TempDir()
		newStore = func() WorkflowStore {
			s, err := NewFlatBuffersStore(dir)
			if err != nil {
				b.Fatal(err)
			}
			return s
		}
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wf := &Workflow{DAG: dag, WorkflowID: fmt.Sprintf("w%d", i), Store: newStore()}
		perfSink = wf.Execute(ctx)
	}
}

func benchMatrix(b *testing.B, build func(int) *DAG) {
	for _, n := range []int{10, 100, 1000} {
		dag := build(n)
		for _, store := range []string{"none", "inmem", "fb"} {
			b.Run(fmt.Sprintf("N=%d/store=%s", n, store), func(b *testing.B) {
				benchRun(b, dag, store)
			})
		}
	}
}

func BenchmarkPerfWide(b *testing.B)      { benchMatrix(b, buildWide) }
func BenchmarkPerfDeep(b *testing.B)      { benchMatrix(b, buildDeep) }
func BenchmarkPerfLargeData(b *testing.B) { benchMatrix(b, buildLargeData) }
