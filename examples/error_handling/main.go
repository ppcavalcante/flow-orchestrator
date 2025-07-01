package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Define error types for consistency
const (
	ErrInvalidInput = "invalid_input"
	ErrProcessing   = "processing_error"
	ErrTimeout      = "timeout_error"
	ErrRetryable    = "retryable_error"
	ErrFatal        = "fatal_error"
)

// Configuration options
type Config struct {
	UseArena       bool
	ArenaBlockSize int
	WorkerCount    int
	ErrorType      string // "retry", "timeout", "conditional", "fallback", "circuit_breaker"
}

func main() {
	// Parse command line arguments for error type
	errorType := "retry"
	if len(os.Args) > 1 {
		errorType = os.Args[1]
	}

	// Define configuration
	config := Config{
		UseArena:       true,
		ArenaBlockSize: 8192,
		WorkerCount:    runtime.NumCPU(),
		ErrorType:      errorType,
	}

	fmt.Printf("=== Error Handling Example: %s ===\n", config.ErrorType)
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

	// Run the selected error handling pattern
	switch config.ErrorType {
	case "retry":
		runRetryWorkflow(config, store)
	case "timeout":
		runTimeoutWorkflow(config, store)
	case "conditional":
		runConditionalErrorWorkflow(config, store)
	case "fallback":
		runFallbackWorkflow(config, store)
	case "circuit_breaker":
		runCircuitBreakerWorkflow(config, store)
	default:
		fmt.Printf("Unknown error handling type: %s\n", config.ErrorType)
		fmt.Println("Available types: retry, timeout, conditional, fallback, circuit_breaker")
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

// createFlakyAction creates an action that fails intermittently
func createFlakyAction(name string, failureRate float64, errorMsg string) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		defer nodeExecTracker(name)()

		fmt.Printf("Executing %s...\n", name)
		time.Sleep(100 * time.Millisecond)

		// Get attempt count
		attemptCount, _ := data.GetInt(name + "_attempt_count")
		attemptCount++
		data.Set(name+"_attempt_count", attemptCount)

		// Simulate failure based on failure rate
		if rand.Float64() < failureRate {
			fmt.Printf("%s failed (attempt %d): %s\n", name, attemptCount, errorMsg)
			return fmt.Errorf("%s: %s (attempt %d)", ErrRetryable, errorMsg, attemptCount)
		}

		// Success
		fmt.Printf("%s succeeded on attempt %d\n", name, attemptCount)
		data.Set(name+"_completed", true)
		data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
		return nil
	})
}

// createTimeoutAction creates an action that times out
func createTimeoutAction(name string, executionTime time.Duration) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		fmt.Printf("Executing %s...\n", name)

		// Simulate long-running task
		select {
		case <-time.After(executionTime):
			defer nodeExecTracker(name)()
			fmt.Printf("%s completed successfully\n", name)
			data.Set(name+"_completed", true)
			data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
			return nil
		case <-ctx.Done():
			fmt.Printf("%s timed out\n", name)
			return fmt.Errorf("%s: operation timed out", ErrTimeout)
		}
	})
}

// createConditionalErrorAction creates an action that fails based on input conditions
func createConditionalErrorAction(name string, condition string, value interface{}) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		defer nodeExecTracker(name)()

		fmt.Printf("Executing %s...\n", name)
		time.Sleep(100 * time.Millisecond)

		// Check condition
		conditionValue, exists := data.Get(condition)
		if !exists {
			fmt.Printf("%s failed: condition %s not found\n", name, condition)
			return fmt.Errorf("%s: condition %s not found", ErrInvalidInput, condition)
		}

		// Compare condition value
		if fmt.Sprintf("%v", conditionValue) == fmt.Sprintf("%v", value) {
			fmt.Printf("%s failed: condition %s=%v triggered error\n", name, condition, value)
			return fmt.Errorf("%s: condition %s=%v triggered error", ErrProcessing, condition, value)
		}

		// Success
		fmt.Printf("%s completed successfully\n", name)
		data.Set(name+"_completed", true)
		data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
		return nil
	})
}

// createFallbackAction creates a primary action with a fallback
func createFallbackAction(name string, shouldFail bool) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		defer nodeExecTracker(name)()

		fmt.Printf("Executing %s (primary)...\n", name)
		time.Sleep(100 * time.Millisecond)

		// Simulate primary action failure
		if shouldFail {
			fmt.Printf("%s primary action failed, trying fallback\n", name)

			// Execute fallback
			fmt.Printf("Executing %s (fallback)...\n", name)
			time.Sleep(150 * time.Millisecond)

			// Record fallback execution
			data.Set(name+"_used_fallback", true)
			data.Set(name+"_completed", true)
			data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))

			fmt.Printf("%s fallback completed successfully\n", name)
			return nil
		}

		// Primary succeeded
		fmt.Printf("%s primary completed successfully\n", name)
		data.Set(name+"_completed", true)
		data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
		return nil
	})
}

// createCircuitBreakerAction creates an action that implements circuit breaker pattern
func createCircuitBreakerAction(name string, failureThreshold int) workflow.Action {
	return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		defer nodeExecTracker(name)()

		fmt.Printf("Executing %s...\n", name)

		// Check if circuit is open
		circuitOpen, _ := data.GetBool("circuit_open")
		if circuitOpen {
			fmt.Printf("%s skipped: circuit is open\n", name)
			return fmt.Errorf("%s: circuit is open", ErrFatal)
		}

		// Get failure count
		failureCount, _ := data.GetInt("failure_count")

		// Check if we've reached the threshold
		if failureCount >= failureThreshold {
			fmt.Printf("%s failed: circuit breaker tripped (failures: %d, threshold: %d)\n",
				name, failureCount, failureThreshold)
			data.Set("circuit_open", true)
			return fmt.Errorf("%s: circuit breaker tripped", ErrFatal)
		}

		// Simulate random failure
		if rand.Float64() < 0.5 {
			failureCount++
			data.Set("failure_count", failureCount)
			fmt.Printf("%s failed (failure %d of %d)\n", name, failureCount, failureThreshold)
			return fmt.Errorf("%s: random failure", ErrProcessing)
		}

		// Success
		fmt.Printf("%s completed successfully\n", name)
		data.Set(name+"_completed", true)
		data.Set(name+"_timestamp", time.Now().Format(time.RFC3339))
		return nil
	})
}

// runRetryWorkflow demonstrates retry error handling
func runRetryWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Retry Error Handling ===")
	fmt.Println("Pattern: Automatically retry failed operations with exponential backoff")
	fmt.Println("This pattern demonstrates how to handle transient failures by retrying operations.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("retry-workflow").
		WithStateStore(store)

	// Add workflow nodes
	builder.AddStartNode("init").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("init")()
			fmt.Println("Initializing workflow...")
			time.Sleep(100 * time.Millisecond)
			data.Set("init_completed", true)
			return nil
		}))

	// Add flaky nodes with different retry configurations
	builder.AddNode("flaky-no-retry").
		WithAction(createFlakyAction("flaky-no-retry", 0.8, "transient error")).
		DependsOn("init")

	builder.AddNode("flaky-with-retry").
		WithAction(createFlakyAction("flaky-with-retry", 0.8, "transient error")).
		WithRetries(3).
		DependsOn("init")

	builder.AddNode("flaky-with-backoff").
		WithAction(createFlakyAction("flaky-with-backoff", 0.8, "transient error")).
		WithRetries(5).
		DependsOn("init")

	// Add a final node that depends on all flaky nodes
	builder.AddNode("finalize").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("finalize")()
			fmt.Println("Finalizing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Check status of flaky nodes
			noRetryStatus, _ := data.GetNodeStatus("flaky-no-retry")
			withRetryStatus, _ := data.GetNodeStatus("flaky-with-retry")
			withBackoffStatus, _ := data.GetNodeStatus("flaky-with-backoff")

			fmt.Printf("Node status summary:\n")
			fmt.Printf("- flaky-no-retry: %s\n", noRetryStatus)
			fmt.Printf("- flaky-with-retry: %s\n", withRetryStatus)
			fmt.Printf("- flaky-with-backoff: %s\n", withBackoffStatus)

			data.Set("finalize_completed", true)
			return nil
		})).
		DependsOn("flaky-no-retry", "flaky-with-retry", "flaky-with-backoff")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "retry-workflow")

	// Execute the workflow
	fmt.Println("Starting retry workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Retry workflow completed in %v\n", time.Since(startTime))
	}

	printWorkflowResults(data)
}

// runTimeoutWorkflow demonstrates timeout error handling
func runTimeoutWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Timeout Error Handling ===")
	fmt.Println("Pattern: Set timeouts for operations to prevent indefinite waiting")
	fmt.Println("This pattern demonstrates how to handle operations that take too long.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("timeout-workflow").
		WithStateStore(store)

	// Add workflow nodes
	builder.AddStartNode("init").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("init")()
			fmt.Println("Initializing workflow...")
			time.Sleep(100 * time.Millisecond)
			data.Set("init_completed", true)
			return nil
		}))

	// Add nodes with different timeout configurations
	builder.AddNode("quick-task").
		WithAction(createTimeoutAction("quick-task", 200*time.Millisecond)).
		WithTimeout(500 * time.Millisecond).
		DependsOn("init")

	builder.AddNode("slow-task").
		WithAction(createTimeoutAction("slow-task", 800*time.Millisecond)).
		WithTimeout(500 * time.Millisecond).
		DependsOn("init")

	builder.AddNode("very-slow-task").
		WithAction(createTimeoutAction("very-slow-task", 1500*time.Millisecond)).
		WithTimeout(500 * time.Millisecond).
		DependsOn("init")

	// Add a final node that depends on all timed nodes
	builder.AddNode("finalize").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("finalize")()
			fmt.Println("Finalizing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Check status of timed nodes
			quickStatus, _ := data.GetNodeStatus("quick-task")
			slowStatus, _ := data.GetNodeStatus("slow-task")
			verySlowStatus, _ := data.GetNodeStatus("very-slow-task")

			fmt.Printf("Node status summary:\n")
			fmt.Printf("- quick-task: %s\n", quickStatus)
			fmt.Printf("- slow-task: %s\n", slowStatus)
			fmt.Printf("- very-slow-task: %s\n", verySlowStatus)

			data.Set("finalize_completed", true)
			return nil
		})).
		DependsOn("quick-task", "slow-task", "very-slow-task")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "timeout-workflow")

	// Execute the workflow
	fmt.Println("Starting timeout workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Timeout workflow completed in %v\n", time.Since(startTime))
	}

	printWorkflowResults(data)
}

// runConditionalErrorWorkflow demonstrates conditional error handling
func runConditionalErrorWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Conditional Error Handling ===")
	fmt.Println("Pattern: Handle errors differently based on conditions")
	fmt.Println("This pattern demonstrates how to handle errors based on input conditions.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("conditional-workflow").
		WithStateStore(store)

	// Add workflow nodes
	builder.AddStartNode("init").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("init")()
			fmt.Println("Initializing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Set different conditions for testing
			data.Set("status", "valid")
			data.Set("amount", 100)
			data.Set("user_type", "premium")

			data.Set("init_completed", true)
			return nil
		}))

	// Add nodes with conditional errors
	builder.AddNode("check-status").
		WithAction(createConditionalErrorAction("check-status", "status", "invalid")).
		DependsOn("init")

	builder.AddNode("check-amount").
		WithAction(createConditionalErrorAction("check-amount", "amount", 0)).
		DependsOn("init")

	builder.AddNode("check-user-type").
		WithAction(createConditionalErrorAction("check-user-type", "user_type", "blocked")).
		DependsOn("init")

	// Add a final node that depends on all check nodes
	builder.AddNode("finalize").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("finalize")()
			fmt.Println("Finalizing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Check status of conditional nodes
			statusCheckStatus, _ := data.GetNodeStatus("check-status")
			amountCheckStatus, _ := data.GetNodeStatus("check-amount")
			userTypeCheckStatus, _ := data.GetNodeStatus("check-user-type")

			fmt.Printf("Node status summary:\n")
			fmt.Printf("- check-status: %s\n", statusCheckStatus)
			fmt.Printf("- check-amount: %s\n", amountCheckStatus)
			fmt.Printf("- check-user-type: %s\n", userTypeCheckStatus)

			data.Set("finalize_completed", true)
			return nil
		})).
		DependsOn("check-status", "check-amount", "check-user-type")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "conditional-workflow")

	// Execute the workflow
	fmt.Println("Starting conditional workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Conditional workflow completed in %v\n", time.Since(startTime))
	}

	printWorkflowResults(data)
}

// runFallbackWorkflow demonstrates fallback error handling
func runFallbackWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Fallback Error Handling ===")
	fmt.Println("Pattern: Provide fallback mechanisms when primary operations fail")
	fmt.Println("This pattern demonstrates how to handle errors by providing alternative paths.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("fallback-workflow").
		WithStateStore(store)

	// Add workflow nodes
	builder.AddStartNode("init").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("init")()
			fmt.Println("Initializing workflow...")
			time.Sleep(100 * time.Millisecond)
			data.Set("init_completed", true)
			return nil
		}))

	// Add nodes with fallback mechanisms
	builder.AddNode("primary-succeeds").
		WithAction(createFallbackAction("primary-succeeds", false)).
		DependsOn("init")

	builder.AddNode("primary-fails").
		WithAction(createFallbackAction("primary-fails", true)).
		DependsOn("init")

	// Add a final node that depends on all fallback nodes
	builder.AddNode("finalize").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("finalize")()
			fmt.Println("Finalizing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Check status of fallback nodes
			primarySucceedsStatus, _ := data.GetNodeStatus("primary-succeeds")
			primaryFailsStatus, _ := data.GetNodeStatus("primary-fails")

			// Check if fallbacks were used
			primarySucceedsFallback, _ := data.GetBool("primary-succeeds_used_fallback")
			primaryFailsFallback, _ := data.GetBool("primary-fails_used_fallback")

			fmt.Printf("Node status summary:\n")
			fmt.Printf("- primary-succeeds: %s (used fallback: %v)\n", primarySucceedsStatus, primarySucceedsFallback)
			fmt.Printf("- primary-fails: %s (used fallback: %v)\n", primaryFailsStatus, primaryFailsFallback)

			data.Set("finalize_completed", true)
			return nil
		})).
		DependsOn("primary-succeeds", "primary-fails")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "fallback-workflow")

	// Execute the workflow
	fmt.Println("Starting fallback workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Fallback workflow completed in %v\n", time.Since(startTime))
	}

	printWorkflowResults(data)
}

// runCircuitBreakerWorkflow demonstrates circuit breaker error handling
func runCircuitBreakerWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Circuit Breaker Error Handling ===")
	fmt.Println("Pattern: Prevent cascading failures by stopping operations after too many failures")
	fmt.Println("This pattern demonstrates how to handle errors by breaking the circuit after a threshold.")

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("circuit-breaker-workflow").
		WithStateStore(store)

	// Add workflow nodes
	builder.AddStartNode("init").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("init")()
			fmt.Println("Initializing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Initialize circuit breaker state
			data.Set("circuit_open", false)
			data.Set("failure_count", 0)

			data.Set("init_completed", true)
			return nil
		}))

	// Add circuit breaker nodes
	// The threshold is 3, so after 3 failures, the circuit will open
	failureThreshold := 3

	// Add multiple attempts to trigger the circuit breaker
	for i := 1; i <= 5; i++ {
		nodeName := fmt.Sprintf("attempt-%d", i)
		builder.AddNode(nodeName).
			WithAction(createCircuitBreakerAction(nodeName, failureThreshold)).
			DependsOn("init")
	}

	// Add a final node that depends on all attempt nodes
	builder.AddNode("finalize").
		WithAction(workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
			defer nodeExecTracker("finalize")()
			fmt.Println("Finalizing workflow...")
			time.Sleep(100 * time.Millisecond)

			// Check circuit breaker state
			circuitOpen, _ := data.GetBool("circuit_open")
			failureCount, _ := data.GetInt("failure_count")

			fmt.Printf("Circuit breaker summary:\n")
			fmt.Printf("- Circuit open: %v\n", circuitOpen)
			fmt.Printf("- Failure count: %d\n", failureCount)
			fmt.Printf("- Failure threshold: %d\n", failureThreshold)

			// Check status of attempt nodes
			for i := 1; i <= 5; i++ {
				nodeName := fmt.Sprintf("attempt-%d", i)
				status, _ := data.GetNodeStatus(nodeName)
				fmt.Printf("- %s: %s\n", nodeName, status)
			}

			data.Set("finalize_completed", true)
			return nil
		})).
		DependsOn("attempt-1", "attempt-2", "attempt-3", "attempt-4", "attempt-5")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	data := createWorkflowData(config, "circuit-breaker-workflow")

	// Execute the workflow
	fmt.Println("Starting circuit breaker workflow execution...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Circuit breaker workflow completed in %v\n", time.Since(startTime))
	}

	printWorkflowResults(data)
}

// printWorkflowResults prints the results of the workflow execution
func printWorkflowResults(data *workflow.WorkflowData) {
	fmt.Println("\nWorkflow Results:")

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
