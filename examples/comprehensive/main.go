package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// Define error types for consistency
const (
	ErrInvalidInput = "invalid_input"
	ErrProcessing   = "processing_error"
	ErrTimeout      = "timeout_error"
	ErrRetryable    = "retryable_error"
)

// Configuration options
type Config struct {
	UseArena         bool
	ArenaBlockSize   int
	EnableMetrics    bool
	SamplingRate     float64
	WorkerCount      int
	EnableProfiling  bool
	ProfileOutputDir string
}

func main() {
	// Define configuration
	config := Config{
		UseArena:         true,
		ArenaBlockSize:   8192,
		EnableMetrics:    true,
		SamplingRate:     1.0,
		WorkerCount:      runtime.NumCPU(),
		EnableProfiling:  false,
		ProfileOutputDir: "./profiles",
	}

	// Setup profiling if enabled
	if config.EnableProfiling {
		setupProfiling(config.ProfileOutputDir)
	}

	// Configure metrics
	if config.EnableMetrics {
		metricsConfig := metrics.DefaultConfig()
		metricsConfig.WithEnabled(true).WithSamplingRate(config.SamplingRate)
		metrics.ApplyConfig(metricsConfig)
		metrics.Reset() // Clear any previous metrics
	}

	fmt.Println("=== Comprehensive Workflow Example ===")
	fmt.Printf("Configuration:\n")
	fmt.Printf("- Arena-based memory: %v\n", config.UseArena)
	fmt.Printf("- Arena block size: %d bytes\n", config.ArenaBlockSize)
	fmt.Printf("- Metrics enabled: %v\n", config.EnableMetrics)
	fmt.Printf("- Metrics sampling rate: %.2f\n", config.SamplingRate)
	fmt.Printf("- Worker count: %d\n", config.WorkerCount)
	fmt.Printf("- Profiling enabled: %v\n", config.EnableProfiling)
	fmt.Println()

	// Create a store for the workflow data
	store, err := workflow.NewFlatBuffersStore("./workflow_data.fb")
	if err != nil {
		fmt.Printf("Error creating store: %v\n", err)
		return
	}

	// Run the e-commerce workflow example
	runECommerceWorkflow(config, store)

	// Run the data processing workflow example
	runDataProcessingWorkflow(config, store)

	// Run the error handling workflow example
	runErrorHandlingWorkflow(config, store)

	// Display metrics if enabled
	if config.EnableMetrics {
		displayMetrics()
	}
}

// setupProfiling configures CPU and memory profiling
func setupProfiling(outputDir string) {
	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create profile directory: %v", err)
	}

	// Start CPU profiling
	cpuFile, err := os.Create(fmt.Sprintf("%s/cpu.prof", outputDir))
	if err != nil {
		log.Fatalf("Failed to create CPU profile: %v", err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		log.Fatalf("Failed to start CPU profile: %v", err)
	}

	// Setup memory profiling to be written on exit
	memFile, err := os.Create(fmt.Sprintf("%s/mem.prof", outputDir))
	if err != nil {
		log.Fatalf("Failed to create memory profile: %v", err)
	}

	// Defer stopping CPU profile and writing memory profile
	defer func() {
		pprof.StopCPUProfile()
		if err := pprof.WriteHeapProfile(memFile); err != nil {
			log.Fatalf("Failed to write memory profile: %v", err)
		}
		cpuFile.Close()
		memFile.Close()
	}()
}

// createWorkflowData creates a workflow data instance based on configuration
func createWorkflowData(config Config, id string) *workflow.WorkflowData {
	// Create workflow data config
	dataConfig := workflow.DefaultWorkflowDataConfig()

	// Configure metrics
	if config.EnableMetrics {
		dataConfig.MetricsConfig = metrics.ConfigFromLiteral(config.EnableMetrics, config.SamplingRate)
	} else {
		dataConfig.MetricsConfig = metrics.DisabledMetricsConfig()
	}

	dataConfig.ExpectedNodes = 50
	dataConfig.ExpectedData = 100

	// Create workflow data based on configuration
	if config.UseArena {
		if config.ArenaBlockSize > 0 {
			return workflow.NewWorkflowDataWithArena(id, dataConfig, config.ArenaBlockSize)
		}
		return workflow.NewWorkflowDataWithArena(id, dataConfig)
	}

	return workflow.NewWorkflowDataWithConfig(id, dataConfig)
}

// runECommerceWorkflow demonstrates a typical e-commerce checkout flow
func runECommerceWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== E-Commerce Order Processing Workflow ===")

	// Track workflow execution time
	ecommerceOp := metrics.OperationType("ECommerceWorkflow")
	metrics.TrackOperation(ecommerceOp, func() {
		// Create a workflow builder
		builder := workflow.NewWorkflowBuilder().
			WithWorkflowID("ecommerce-workflow").
			WithStore(store)

		// Add custom metrics tracking for node execution
		nodeExecTracker := func(nodeName string) func() {
			start := time.Now()
			return func() {
				duration := time.Since(start)
				fmt.Printf("Node %s completed in %v\n", nodeName, duration)
			}
		}

		// Add workflow nodes
		builder.AddStartNode("validate-cart").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("validate-cart")()
				fmt.Println("Validating cart...")
				time.Sleep(100 * time.Millisecond)

				// Simulate cart data
				data.Set("cart", map[string]interface{}{
					"items": []map[string]interface{}{
						{"id": "item1", "name": "Product 1", "price": 29.99, "quantity": 2},
						{"id": "item2", "name": "Product 2", "price": 49.99, "quantity": 1},
					},
					"customer_id": "cust_12345",
				})

				// Calculate total
				data.Set("cart_total", 29.99*2+49.99)
				return nil
			})

		builder.AddNode("check-inventory").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("check-inventory")()
				fmt.Println("Checking inventory...")
				time.Sleep(150 * time.Millisecond)

				// Simulate inventory check
				data.Set("inventory_status", "available")
				return nil
			}).
			DependsOn("validate-cart")

		builder.AddNode("calculate-tax").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("calculate-tax")()
				fmt.Println("Calculating tax...")
				time.Sleep(100 * time.Millisecond)

				// Get cart total
				total, _ := data.GetFloat64("cart_total")

				// Calculate tax (8%)
				tax := total * 0.08
				data.Set("tax", tax)
				return nil
			}).
			DependsOn("validate-cart")

		builder.AddNode("calculate-shipping").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("calculate-shipping")()
				fmt.Println("Calculating shipping...")
				time.Sleep(120 * time.Millisecond)

				// Simulate shipping calculation
				data.Set("shipping", 10.00)
				return nil
			}).
			DependsOn("validate-cart")

		builder.AddNode("process-payment").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("process-payment")()
				fmt.Println("Processing payment...")
				time.Sleep(200 * time.Millisecond)

				// Get amounts
				total, _ := data.GetFloat64("cart_total")
				tax, _ := data.GetFloat64("tax")
				shipping, _ := data.GetFloat64("shipping")

				// Calculate final amount
				finalAmount := total + tax + shipping
				data.Set("final_amount", finalAmount)

				// Simulate payment processing
				data.Set("payment_id", "pmt_"+randomString(10))
				data.Set("payment_status", "approved")

				return nil
			}).
			DependsOn("calculate-tax", "calculate-shipping", "check-inventory")

		builder.AddNode("create-order").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("create-order")()
				fmt.Println("Creating order...")
				time.Sleep(150 * time.Millisecond)

				// Generate order ID
				orderId := "ord_" + randomString(10)
				data.Set("order_id", orderId)

				// Get values for order
				paymentId, _ := data.GetString("payment_id")
				finalAmount, _ := data.GetFloat64("final_amount")
				tax, _ := data.GetFloat64("tax")
				shipping, _ := data.GetFloat64("shipping")

				// Create order record
				data.Set("order", map[string]interface{}{
					"id":          orderId,
					"customer_id": "cust_12345",
					"payment_id":  paymentId,
					"total":       finalAmount,
					"tax":         tax,
					"shipping":    shipping,
					"status":      "created",
					"created_at":  time.Now().Format(time.RFC3339),
				})

				return nil
			}).
			DependsOn("process-payment")

		builder.AddNode("send-confirmation").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				defer nodeExecTracker("send-confirmation")()
				fmt.Println("Sending confirmation...")
				time.Sleep(100 * time.Millisecond)

				// Simulate sending email
				data.Set("email_sent", true)
				data.Set("email_id", "email_"+randomString(10))

				return nil
			}).
			DependsOn("create-order")

		// Build the workflow
		dag, err := builder.Build()
		if err != nil {
			fmt.Printf("Error building workflow: %v\n", err)
			return
		}

		// Create workflow data
		data := createWorkflowData(config, "ecommerce-workflow")

		// Execute the workflow
		fmt.Println("Starting e-commerce workflow execution...")
		startTime := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = dag.Execute(ctx, data)
		if err != nil {
			fmt.Printf("Workflow failed: %v\n", err)
			return
		}

		fmt.Printf("E-commerce workflow completed in %v\n", time.Since(startTime))

		// Display results
		fmt.Println("\nE-Commerce Workflow Results:")
		orderId, _ := data.GetString("order_id")
		paymentId, _ := data.GetString("payment_id")
		finalAmount, _ := data.GetFloat64("final_amount")

		fmt.Printf("Order ID: %s\n", orderId)
		fmt.Printf("Payment ID: %s\n", paymentId)
		fmt.Printf("Final Amount: $%.2f\n", finalAmount)

		// Display arena stats if used
		if config.UseArena {
			displayArenaStats(data)
		}
	})
}

// runDataProcessingWorkflow demonstrates a data processing pipeline
func runDataProcessingWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Data Processing Workflow ===")

	// Track workflow execution time
	dataProcessingOp := metrics.OperationType("DataProcessingWorkflow")
	metrics.TrackOperation(dataProcessingOp, func() {
		// Create a workflow builder
		builder := workflow.NewWorkflowBuilder().
			WithWorkflowID("data-processing").
			WithStore(store)

		// Add workflow nodes
		builder.AddStartNode("extract-data").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Extracting data...")
				time.Sleep(200 * time.Millisecond)

				// Simulate data extraction
				records := make([]map[string]interface{}, 100)
				for i := 0; i < 100; i++ {
					records[i] = map[string]interface{}{
						"id":    i,
						"value": rand.Float64() * 100,
						"name":  fmt.Sprintf("Record-%d", i),
						"date":  time.Now().AddDate(0, 0, -i).Format("2006-01-02"),
					}
				}

				data.Set("records", records)
				data.Set("record_count", len(records))
				return nil
			})

		builder.AddNode("validate-schema").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Validating schema...")
				time.Sleep(150 * time.Millisecond)

				// Simulate schema validation
				data.Set("schema_valid", true)
				data.Set("validation_errors", []string{})
				return nil
			}).
			DependsOn("extract-data")

		builder.AddNode("transform-data").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Transforming data...")
				time.Sleep(300 * time.Millisecond)

				// Get records
				rawRecords, _ := data.Get("records")
				records := rawRecords.([]map[string]interface{})

				// Transform records
				transformedRecords := make([]map[string]interface{}, len(records))
				for i, record := range records {
					transformedRecords[i] = map[string]interface{}{
						"record_id":    record["id"],
						"record_value": record["value"].(float64) * 1.5, // Apply transformation
						"record_name":  strings.ToUpper(record["name"].(string)),
						"record_date":  record["date"],
						"processed_at": time.Now().Format(time.RFC3339),
					}
				}

				data.Set("transformed_records", transformedRecords)
				return nil
			}).
			DependsOn("validate-schema")

		builder.AddNode("aggregate-data").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Aggregating data...")
				time.Sleep(250 * time.Millisecond)

				// Get transformed records
				rawRecords, _ := data.Get("transformed_records")
				records := rawRecords.([]map[string]interface{})

				// Perform aggregation
				var sum, min, max float64
				min = 1000000 // Large initial value

				for i, record := range records {
					value := record["record_value"].(float64)
					sum += value

					if i == 0 || value < min {
						min = value
					}

					if i == 0 || value > max {
						max = value
					}
				}

				// Store aggregation results
				data.Set("aggregation", map[string]interface{}{
					"count": len(records),
					"sum":   sum,
					"avg":   sum / float64(len(records)),
					"min":   min,
					"max":   max,
				})

				return nil
			}).
			DependsOn("transform-data")

		builder.AddNode("generate-report").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Generating report...")
				time.Sleep(200 * time.Millisecond)

				// Get aggregation data
				rawAgg, _ := data.Get("aggregation")
				agg := rawAgg.(map[string]interface{})

				// Create report
				report := map[string]interface{}{
					"title":           "Data Processing Report",
					"generated_at":    time.Now().Format(time.RFC3339),
					"record_count":    agg["count"],
					"statistics":      agg,
					"processing_time": "500ms",
				}

				data.Set("report", report)
				return nil
			}).
			DependsOn("aggregate-data")

		// Build the workflow
		dag, err := builder.Build()
		if err != nil {
			fmt.Printf("Error building workflow: %v\n", err)
			return
		}

		// Create workflow data
		data := createWorkflowData(config, "data-processing")

		// Execute the workflow
		fmt.Println("Starting data processing workflow execution...")
		startTime := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = dag.Execute(ctx, data)
		if err != nil {
			fmt.Printf("Workflow failed: %v\n", err)
			return
		}

		fmt.Printf("Data processing workflow completed in %v\n", time.Since(startTime))

		// Display results
		fmt.Println("\nData Processing Workflow Results:")
		recordCount, _ := data.GetInt("record_count")

		// Get aggregation data
		rawAgg, _ := data.Get("aggregation")
		if agg, ok := rawAgg.(map[string]interface{}); ok {
			fmt.Printf("Processed %d records\n", recordCount)
			fmt.Printf("Sum: %.2f\n", agg["sum"])
			fmt.Printf("Average: %.2f\n", agg["avg"])
			fmt.Printf("Min: %.2f\n", agg["min"])
			fmt.Printf("Max: %.2f\n", agg["max"])
		}

		// Display arena stats if used
		if config.UseArena {
			displayArenaStats(data)
		}
	})
}

// runErrorHandlingWorkflow demonstrates error handling and recovery
func runErrorHandlingWorkflow(config Config, store workflow.WorkflowStore) {
	fmt.Println("\n=== Error Handling Workflow ===")

	// Track workflow execution time
	errorHandlingOp := metrics.OperationType("ErrorHandlingWorkflow")
	metrics.TrackOperation(errorHandlingOp, func() {
		// Create a workflow builder
		builder := workflow.NewWorkflowBuilder().
			WithWorkflowID("error-handling").
			WithStore(store)

		// Add workflow nodes
		builder.AddStartNode("start-task").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Starting task...")
				time.Sleep(100 * time.Millisecond)

				// Initialize data
				data.Set("retry_count", 0)
				data.Set("start_time", time.Now().Format(time.RFC3339))

				return nil
			})

		// This task will fail on first attempt but succeed on retry
		builder.AddNode("flaky-task").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Executing flaky task...")

				// Get retry count
				retryCount, _ := data.GetInt("retry_count")

				// Increment retry count
				data.Set("retry_count", retryCount+1)

				// Fail on first attempt
				if retryCount == 0 {
					fmt.Println("Flaky task failed, will retry...")
					return fmt.Errorf("%s: temporary failure", ErrRetryable)
				}

				// Succeed on retry
				fmt.Println("Flaky task succeeded on retry!")
				data.Set("flaky_result", "success after retry")

				return nil
			}).
			DependsOn("start-task").
			WithRetries(3)

		// This task will timeout
		builder.AddNode("timeout-task").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Executing timeout task...")

				// Simulate long-running task
				select {
				case <-time.After(2 * time.Second):
					data.Set("timeout_result", "completed")
					return nil
				case <-ctx.Done():
					return fmt.Errorf("%s: task timed out", ErrTimeout)
				}
			}).
			DependsOn("start-task").
			WithTimeout(500 * time.Millisecond)

		// This task handles errors from previous tasks
		builder.AddNode("error-handler").
			WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
				fmt.Println("Handling errors...")
				time.Sleep(100 * time.Millisecond)

				// Check status of previous tasks
				flakyStatus, _ := data.GetNodeStatus("flaky-task")
				timeoutStatus, _ := data.GetNodeStatus("timeout-task")

				// Get retry count
				retryCount, _ := data.GetInt("retry_count")

				// Record error information
				errorInfo := map[string]interface{}{
					"flaky_task_status":   flakyStatus,
					"timeout_task_status": timeoutStatus,
					"retry_count":         retryCount,
					"handled_at":          time.Now().Format(time.RFC3339),
				}

				data.Set("error_handling", errorInfo)

				return nil
			}).
			DependsOn("flaky-task", "timeout-task")

		// Build the workflow
		dag, err := builder.Build()
		if err != nil {
			fmt.Printf("Error building workflow: %v\n", err)
			return
		}

		// Create workflow data
		data := createWorkflowData(config, "error-handling")

		// Execute the workflow
		fmt.Println("Starting error handling workflow execution...")
		startTime := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = dag.Execute(ctx, data)
		if err != nil {
			fmt.Printf("Workflow failed: %v\n", err)
			return
		}

		fmt.Printf("Error handling workflow completed in %v\n", time.Since(startTime))

		// Display results
		fmt.Println("\nError Handling Workflow Results:")
		retryCount, _ := data.GetInt("retry_count")

		// Get node statuses
		flakyStatus, _ := data.GetNodeStatus("flaky-task")
		timeoutStatus, _ := data.GetNodeStatus("timeout-task")

		fmt.Printf("Flaky task status: %s\n", flakyStatus)
		fmt.Printf("Timeout task status: %s\n", timeoutStatus)
		fmt.Printf("Retry count: %d\n", retryCount)

		// Display arena stats if used
		if config.UseArena {
			displayArenaStats(data)
		}
	})
}

// displayArenaStats shows memory usage statistics for arena-based workflow data
func displayArenaStats(data *workflow.WorkflowData) {
	stats := data.GetArenaStats()

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

// randomString generates a random string of the specified length
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[rand.Intn(len(charset))]
	}
	return string(result)
}

// displayMetrics shows performance metrics for the workflow system
func displayMetrics() {
	allStats := metrics.GetAllOperationStats()

	fmt.Println("\n=== Performance Metrics ===")

	// Check if we have any metrics
	if len(allStats) == 0 {
		fmt.Println("\nNo metrics collected. Make sure metrics are enabled and operations are being tracked.")
		return
	}

	// Display operation counts and timing
	fmt.Println("\nOperation Statistics:")
	hasMetrics := false

	// Sort operations by name for consistent output
	var operations []metrics.OperationType
	for op := range allStats {
		operations = append(operations, op)
	}

	// Sort operations by string representation
	sort.Slice(operations, func(i, j int) bool {
		return string(operations[i]) < string(operations[j])
	})

	for _, op := range operations {
		stats := allStats[op]
		if stats.Count > 0 {
			hasMetrics = true
			fmt.Printf("- %s: %d operations, avg %.2f µs, min %.2f µs, max %.2f µs\n",
				op, stats.Count,
				float64(stats.AvgTimeNs)/1000.0,
				float64(stats.MinTimeNs)/1000.0,
				float64(stats.MaxTimeNs)/1000.0)
		}
	}

	if !hasMetrics {
		fmt.Println("No operation metrics recorded.")
	}

	// Add a summary section
	fmt.Println("\nMetrics Summary:")
	fmt.Printf("- Total operations tracked: %d\n", countTotalOperations(allStats))
	fmt.Printf("- Unique operation types: %d\n", len(operations))
}

// countTotalOperations counts the total number of operations across all types
func countTotalOperations(stats map[metrics.OperationType]metrics.OperationStats) int64 {
	var total int64
	for _, s := range stats {
		total += s.Count
	}
	return total
}
