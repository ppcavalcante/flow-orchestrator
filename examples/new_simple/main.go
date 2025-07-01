package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Define error types for consistency
const (
	// Error types
	ErrInvalidInput = "invalid_input"
	ErrProcessing   = "processing_error"
)

func main() {
	fmt.Println("=== Simple Workflow Example (New API) ===")

	// Create a store for the workflow data
	store, err := workflow.NewJSONFileStore("./workflow_data.json")
	if err != nil {
		fmt.Printf("Error creating store: %v\n", err)
		return
	}

	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("simple-workflow").
		WithStateStore(store)

	// Add a fetch data node - start of the workflow
	builder.AddStartNode("fetch").
		WithAction(fetchAction).
		WithRetries(1).
		WithTimeout(2 * time.Second)

	// Add a process data node that depends on fetch
	builder.AddNode("process").
		WithAction(processAction).
		WithRetries(1).
		WithTimeout(3 * time.Second).
		DependsOn("fetch")

	// Add a save data node that depends on process
	builder.AddNode("save").
		WithAction(saveAction).
		WithRetries(1).
		WithTimeout(2 * time.Second).
		DependsOn("process")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow with the DAG
	workflow := &workflow.Workflow{
		DAG:        dag,
		WorkflowID: "simple-workflow",
		Store:      store,
	}

	// Execute the workflow
	fmt.Println("Starting workflow execution...")
	startTime := time.Now()

	err = workflow.Execute(context.Background())
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Workflow completed in %v\n", time.Since(startTime))

	// Load and display the workflow data
	data, err := store.Load("simple-workflow")
	if err != nil {
		fmt.Printf("Error loading workflow data: %v\n", err)
		return
	}

	// Print node outputs
	fmt.Println("\nWorkflow Results:")
	printNodeOutput(data, "fetch")
	printNodeOutput(data, "process")
	printNodeOutput(data, "save")
}

// printNodeOutput formats and prints a node's output
func printNodeOutput(data *workflow.WorkflowData, nodeName string) {
	output, ok := data.GetOutput(nodeName)
	if !ok {
		fmt.Printf("%s: No output available\n", nodeName)
		return
	}

	if str, ok := output.(string); ok {
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(str), &jsonData); err == nil {
			prettyJSON, _ := json.MarshalIndent(jsonData, "", "  ")
			fmt.Printf("\n=== %s Output ===\n%s\n", nodeName, prettyJSON)
		} else {
			fmt.Printf("\n=== %s Output ===\n%s\n", nodeName, str)
		}
	} else {
		fmt.Printf("\n=== %s Output ===\n%v\n", nodeName, output)
	}
}

// fetchAction simulates fetching data from an external service
func fetchAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Fetching data...")

	// Update node status to running
	data.SetNodeStatus("fetch", workflow.Running)

	// Simulate work
	time.Sleep(500 * time.Millisecond)

	// Prepare the data
	userData := map[string]interface{}{
		"user_id": 123,
		"name":    "John Doe",
		"email":   "john@example.com",
	}

	// Store the fetched data
	jsonData, _ := json.Marshal(userData)
	data.SetOutput("fetch", string(jsonData))

	// Mark as completed
	data.SetNodeStatus("fetch", workflow.Completed)

	fmt.Println("✅ Data fetched successfully")
	return nil
}

// processAction processes the fetched data
func processAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Processing data...")

	// Update node status to running
	data.SetNodeStatus("process", workflow.Running)

	// Get data from the fetch node
	fetchOutput, ok := data.GetOutput("fetch")
	if !ok {
		return fmt.Errorf("%s: fetch output not found", ErrInvalidInput)
	}

	// Parse the fetch output
	var userData map[string]interface{}
	if fetchStr, ok := fetchOutput.(string); ok {
		if err := json.Unmarshal([]byte(fetchStr), &userData); err != nil {
			return fmt.Errorf("%s: failed to parse fetch data: %w", ErrProcessing, err)
		}
	} else {
		return fmt.Errorf("%s: fetch output is not a string", ErrInvalidInput)
	}

	// Simulate work
	time.Sleep(1 * time.Second)

	// Process the data
	processedData := map[string]interface{}{
		"user": map[string]interface{}{
			"id":    userData["user_id"],
			"name":  userData["name"],
			"email": userData["email"],
		},
		"processed_at": time.Now().Format(time.RFC3339),
	}

	// Store the processed data
	jsonData, _ := json.Marshal(processedData)
	data.SetOutput("process", string(jsonData))

	// Mark as completed
	data.SetNodeStatus("process", workflow.Completed)

	fmt.Println("✅ Data processed successfully")
	return nil
}

// saveAction simulates saving data to a database
func saveAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Saving data...")

	// Update node status to running
	data.SetNodeStatus("save", workflow.Running)

	// Get data from the process node
	processOutput, ok := data.GetOutput("process")
	if !ok {
		return fmt.Errorf("%s: process output not found", ErrInvalidInput)
	}

	// Parse the process output
	var processedData map[string]interface{}
	if processStr, ok := processOutput.(string); ok {
		if err := json.Unmarshal([]byte(processStr), &processedData); err != nil {
			return fmt.Errorf("%s: failed to parse process data: %w", ErrProcessing, err)
		}
	} else {
		return fmt.Errorf("%s: process output is not a string", ErrInvalidInput)
	}

	// Simulate work
	time.Sleep(500 * time.Millisecond)

	// Save result
	saveResult := map[string]interface{}{
		"success":     true,
		"saved_at":    time.Now().Format(time.RFC3339),
		"record_id":   "rec_12345",
		"data_source": "example_workflow",
		"data_summary": map[string]interface{}{
			"user_id":   processedData["user"].(map[string]interface{})["id"],
			"timestamp": processedData["processed_at"],
		},
	}

	// Store the save result
	jsonData, _ := json.Marshal(saveResult)
	data.SetOutput("save", string(jsonData))

	// Mark as completed
	data.SetNodeStatus("save", workflow.Completed)

	fmt.Println("✅ Data saved successfully")
	return nil
}
