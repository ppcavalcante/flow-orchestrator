package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// Configuration options
type Config struct {
	UseArena       bool
	ArenaBlockSize int
	WorkerCount    int
	Pattern        string // "linear", "fan_out", "fan_in", "diamond", "complex"
}

func main() {
	// Parse command line arguments for pattern
	pattern := "linear"
	if len(os.Args) > 1 {
		pattern = os.Args[1]
	}

	// Define configuration
	config := Config{
		UseArena:       true,
		ArenaBlockSize: 8192,
		WorkerCount:    runtime.NumCPU(),
		Pattern:        pattern,
	}

	fmt.Printf("=== DAG Pattern Example: %s ===\n", config.Pattern)
	fmt.Printf("Configuration:\n")
	fmt.Printf("- Arena-based memory: %v\n", config.UseArena)
	fmt.Printf("- Arena block size: %d bytes\n", config.ArenaBlockSize)
	fmt.Printf("- Worker count: %d\n", config.WorkerCount)
	fmt.Println()

	// Create a store for the workflow data
	store, err := workflow.NewJSONFileStore("./workflow_data.json")
	if err != nil {
		fmt.Printf("Error creating store: %v\n", err)
		return
	}

	// Run the selected pattern
	switch config.Pattern {
	case "linear":
		runLinearWorkflow(config, store)
	case "fan_out":
		runFanOutWorkflow(config, store)
	case "fan_in":
		runFanInWorkflow(config, store)
	case "diamond":
		runDiamondWorkflow(config, store)
	case "complex":
		runComplexWorkflow(config, store)
	default:
		fmt.Printf("Unknown pattern: %s\n", config.Pattern)
		fmt.Println("Available patterns: linear, fan_out, fan_in, diamond, complex")
	}
}

// createWorkflowData creates a workflow data instance based on configuration
func createWorkflowData(config Config, id string) *workflow.WorkflowData {
	// Create workflow data config
	dataConfig := workflow.DefaultWorkflowDataConfig()
	dataConfig.ExpectedNodes = 20
	dataConfig.ExpectedData = 50

	// Create workflow data based on configuration
	if config.UseArena {
		if config.ArenaBlockSize > 0 {
			return workflow.NewWorkflowDataWithArenaBlockSize(id, dataConfig, config.ArenaBlockSize)
		}
		return workflow.NewWorkflowDataWithArena(id, dataConfig)
	}

	return workflow.NewWorkflowDataWithConfig(id, dataConfig)
}

// nodeExecTracker creates a function that tracks node execution time
func nodeExecTracker(nodeName string) func() {
	start := time.Now()
	return func() {
		duration := time.Since(start)
		fmt.Printf("Node %s completed in %v\n", nodeName, duration)
	}
}

// simpleAction creates a simple action that sleeps for a given duration
func simpleAction(name string, sleepTime time.Duration) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		defer nodeExecTracker(name)()
		fmt.Printf("Executing %s...\n", name)
		time.Sleep(sleepTime)
		data.Set(name+"_completed", true)
		data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
		return nil
	})
}

// runLinearWorkflow demonstrates a linear workflow pattern (A → B → C → D)
func runLinearWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Linear Workflow Pattern ===")
	fmt.Println("Pattern: A → B → C → D")
	fmt.Println("Each node depends on the previous node in a linear sequence.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("linear-workflow").
		WithStateStore(store)

	// Add workflow nodes in a linear pattern
	builder.AddStartNode("node-a").
		WithAction(simpleAction("node-a", 100*time.Millisecond))

	builder.AddNode("node-b").
		WithAction(simpleAction("node-b", 150*time.Millisecond)).
		DependsOn("node-a")

	builder.AddNode("node-c").
		WithAction(simpleAction("node-c", 200*time.Millisecond)).
		DependsOn("node-b")

	builder.AddNode("node-d").
		WithAction(simpleAction("node-d", 100*time.Millisecond)).
		DependsOn("node-c")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "linear-workflow")

	// Execute the workflow
	fmt.Println("Starting linear workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Linear workflow completed in %v\n", time.Since(startTime))
	printWorkflowResults(data)
}

// runFanOutWorkflow demonstrates a fan-out workflow pattern (A → [B, C, D])
func runFanOutWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Fan-Out Workflow Pattern ===")
	fmt.Println("Pattern: A → [B, C, D]")
	fmt.Println("One node fans out to multiple parallel nodes.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("fan-out-workflow").
		WithStateStore(store)

	// Add workflow nodes in a fan-out pattern
	builder.AddStartNode("source").
		WithAction(simpleAction("source", 100*time.Millisecond))

	builder.AddNode("parallel-1").
		WithAction(simpleAction("parallel-1", 200*time.Millisecond)).
		DependsOn("source")

	builder.AddNode("parallel-2").
		WithAction(simpleAction("parallel-2", 150*time.Millisecond)).
		DependsOn("source")

	builder.AddNode("parallel-3").
		WithAction(simpleAction("parallel-3", 180*time.Millisecond)).
		DependsOn("source")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "fan-out-workflow")

	// Execute the workflow
	fmt.Println("Starting fan-out workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Fan-out workflow completed in %v\n", time.Since(startTime))
	printWorkflowResults(data)
}

// runFanInWorkflow demonstrates a fan-in workflow pattern ([A, B, C] → D)
func runFanInWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Fan-In Workflow Pattern ===")
	fmt.Println("Pattern: [A, B, C] → D")
	fmt.Println("Multiple parallel nodes converge to a single node.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("fan-in-workflow").
		WithStateStore(store)

	// Add workflow nodes in a fan-in pattern
	builder.AddStartNode("source-1").
		WithAction(simpleAction("source-1", 100*time.Millisecond))

	builder.AddStartNode("source-2").
		WithAction(simpleAction("source-2", 150*time.Millisecond))

	builder.AddStartNode("source-3").
		WithAction(simpleAction("source-3", 200*time.Millisecond))

	builder.AddNode("sink").
		WithAction(simpleAction("sink", 100*time.Millisecond)).
		DependsOn("source-1", "source-2", "source-3")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "fan-in-workflow")

	// Execute the workflow
	fmt.Println("Starting fan-in workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Fan-in workflow completed in %v\n", time.Since(startTime))
	printWorkflowResults(data)
}

// runDiamondWorkflow demonstrates a diamond workflow pattern (A → [B, C] → D)
func runDiamondWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Diamond Workflow Pattern ===")
	fmt.Println("Pattern: A → [B, C] → D")
	fmt.Println("A node fans out to multiple parallel nodes, which then converge to a single node.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("diamond-workflow").
		WithStateStore(store)

	// Add workflow nodes in a diamond pattern
	builder.AddStartNode("source").
		WithAction(simpleAction("source", 100*time.Millisecond))

	builder.AddNode("parallel-1").
		WithAction(simpleAction("parallel-1", 200*time.Millisecond)).
		DependsOn("source")

	builder.AddNode("parallel-2").
		WithAction(simpleAction("parallel-2", 150*time.Millisecond)).
		DependsOn("source")

	builder.AddNode("sink").
		WithAction(simpleAction("sink", 100*time.Millisecond)).
		DependsOn("parallel-1", "parallel-2")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "diamond-workflow")

	// Execute the workflow
	fmt.Println("Starting diamond workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Diamond workflow completed in %v\n", time.Since(startTime))
	printWorkflowResults(data)
}

// runComplexWorkflow demonstrates a complex workflow pattern with multiple branches and dependencies
func runComplexWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Complex Workflow Pattern ===")
	fmt.Println("Pattern: A complex DAG with multiple branches and dependencies")
	fmt.Println("This pattern demonstrates a more realistic workflow with various dependency patterns.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("complex-workflow").
		WithStateStore(store)

	// Add workflow nodes in a complex pattern
	builder.AddStartNode("init").
		WithAction(simpleAction("init", 100*time.Millisecond))

	builder.AddNode("fetch-data").
		WithAction(simpleAction("fetch-data", 200*time.Millisecond)).
		DependsOn("init")

	builder.AddNode("validate-data").
		WithAction(simpleAction("validate-data", 150*time.Millisecond)).
		DependsOn("fetch-data")

	builder.AddNode("process-data-1").
		WithAction(simpleAction("process-data-1", 180*time.Millisecond)).
		DependsOn("validate-data")

	builder.AddNode("process-data-2").
		WithAction(simpleAction("process-data-2", 160*time.Millisecond)).
		DependsOn("validate-data")

	builder.AddNode("process-data-3").
		WithAction(simpleAction("process-data-3", 170*time.Millisecond)).
		DependsOn("validate-data")

	builder.AddNode("enrich-data-1").
		WithAction(simpleAction("enrich-data-1", 120*time.Millisecond)).
		DependsOn("process-data-1")

	builder.AddNode("enrich-data-2").
		WithAction(simpleAction("enrich-data-2", 130*time.Millisecond)).
		DependsOn("process-data-2", "process-data-3")

	builder.AddNode("aggregate-results").
		WithAction(simpleAction("aggregate-results", 140*time.Millisecond)).
		DependsOn("enrich-data-1", "enrich-data-2")

	builder.AddNode("generate-report").
		WithAction(simpleAction("generate-report", 110*time.Millisecond)).
		DependsOn("aggregate-results")

	builder.AddNode("notify-completion").
		WithAction(simpleAction("notify-completion", 90*time.Millisecond)).
		DependsOn("generate-report")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "complex-workflow")

	// Execute the workflow
	fmt.Println("Starting complex workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Complex workflow completed in %v\n", time.Since(startTime))
	printWorkflowResults(data)
}

// printWorkflowResults prints the results of the workflow execution
func printWorkflowResults(data *workflow.WorkflowData) {
	fmt.Println("\nWorkflow Results:")

	// Print node statuses
	fmt.Println("\nNode Statuses:")
	for _, nodeName := range getNodeNames() {
		status, _ := data.GetNodeStatus(nodeName)
		fmt.Printf("- %s: %s\n", nodeName, status)
	}

	// Print completion timestamps
	fmt.Println("\nCompletion Timestamps:")
	for _, nodeName := range getNodeNames() {
		timestamp, ok := data.GetString(nodeName + "_timestamp")
		if ok {
			fmt.Printf("- %s: %s\n", nodeName, timestamp)
		}
	}

	// Print arena stats if available
	stats := data.GetArenaStats()
	if len(stats) > 0 {
		fmt.Println("\nArena Memory Statistics:")

		if arenaStats, ok := stats["arena"]; ok {
			fmt.Printf("- Total allocated: %d bytes\n", arenaStats["totalAllocated"])
			fmt.Printf("- Current used: %d bytes\n", arenaStats["currentUsed"])
			fmt.Printf("- Block size: %d bytes\n", arenaStats["blockSize"])
			fmt.Printf("- Block count: %d\n", arenaStats["blockCount"])
		}

		if poolStats, ok := stats["stringPool"]; ok {
			fmt.Printf("- String pool size: %d strings\n", poolStats["size"])
			fmt.Printf("- String pool hits: %d\n", poolStats["hits"])
			fmt.Printf("- String pool misses: %d\n", poolStats["misses"])
		}
	}
}

// getNodeNames returns a list of all possible node names used in the examples
func getNodeNames() []string {
	return []string{
		// Linear workflow
		"node-a", "node-b", "node-c", "node-d",

		// Fan-out workflow
		"source", "parallel-1", "parallel-2", "parallel-3",

		// Fan-in workflow
		"source-1", "source-2", "source-3", "sink",

		// Complex workflow
		"init", "fetch-data", "validate-data",
		"process-data-1", "process-data-2", "process-data-3",
		"enrich-data-1", "enrich-data-2", "aggregate-results",
		"generate-report", "notify-completion",
	}
}
