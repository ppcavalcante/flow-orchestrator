# Quickstart Guide

This guide will help you get up and running with Flow Orchestrator in just a few minutes. You'll learn how to install the library, create a simple workflow, and execute it.

## Installation

Install Flow Orchestrator using Go modules:

```bash
go get github.com/ppcavalcante/flow-orchestrator@v0.1.1-alpha
```

## Creating a Simple Workflow

Let's create a simple workflow with three steps:
1. Fetch data
2. Process data
3. Save data

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

func main() {
	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("simple-workflow")

	// Add a fetch data node - start of the workflow
	builder.AddStartNode("fetch").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			fmt.Println("Fetching data...")
			// Simulate work
			time.Sleep(100 * time.Millisecond)
			// Store some data
			data.Set("user_id", 123)
			return nil
		})

	// Add a process data node that depends on fetch
	builder.AddNode("process").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			fmt.Println("Processing data...")
			// Get data from previous step
			userID, _ := data.GetInt("user_id")
			fmt.Printf("Processing user ID: %d\n", userID)
			// Store processed data
			data.Set("processed", true)
			return nil
		}).
		DependsOn("fetch")

	// Add a save data node that depends on process
	builder.AddNode("save").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			fmt.Println("Saving data...")
			// Get data from previous step
			processed, _ := data.GetBool("processed")
			fmt.Printf("Data processed status: %v\n", processed)
			return nil
		}).
		DependsOn("process")

	// Build the workflow
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("Error building workflow: %v\n", err)
		return
	}

	// Create workflow data
	workflowData := workflow.NewWorkflowData("simple-workflow")

	// Execute the workflow
	fmt.Println("Starting workflow execution...")
	startTime := time.Now()

	err = dag.Execute(context.Background(), workflowData)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
		return
	}

	fmt.Printf("Workflow completed in %v\n", time.Since(startTime))
}
```

Save this code to a file named `simple_workflow.go` and run it:

```bash
go run simple_workflow.go
```

Expected output:

```
Starting workflow execution...
Fetching data...
Processing data...
Processing user ID: 123
Saving data...
Data processed status: true
Workflow completed in 300.123456ms
```

## Using Middleware

Flow Orchestrator provides middleware for common cross-cutting concerns. Let's add logging and retry middleware:

```go
// Create middleware stack
stack := workflow.NewMiddlewareStack()
stack.Use(workflow.LoggingMiddleware())
stack.Use(workflow.RetryMiddleware(3, 1*time.Second))

// Apply middleware to an action
fetchAction := func(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Fetching data...")
	data.Set("user_id", 123)
	return nil
}

// Create node with middleware
builder.AddStartNode("fetch").
	WithAction(stack.Apply(fetchAction))
```

## Persisting Workflow State

You can persist workflow state using the built-in storage implementations:

```go
// Create an in-memory store
store := workflow.NewInMemoryStore()

// Create a workflow with the store
workflow := &workflow.Workflow{
	DAG:        dag,
	WorkflowID: "simple-workflow",
	Store:      store,
}

// Execute the workflow with persistence
err = workflow.Execute(context.Background())
```

For file-based persistence:

```go
// Create a JSON file store
store, err := workflow.NewJSONFileStore("./workflow_data.json")
if err != nil {
	fmt.Printf("Error creating store: %v\n", err)
	return
}

// For better performance in production, use FlatBuffers store:
// store, err := workflow.NewFlatBuffersStore("./workflow_data")
```

## What's Next?

This quickstart guide covered the basics of Flow Orchestrator. To learn more:

- Read the [Basic Concepts](./basic-concepts.md) guide to understand the core concepts
- Follow the [Your First Workflow](./first-workflow.md) tutorial for a more detailed example
- Explore the [Middleware System](../guides/middleware.md) to learn about adding cross-cutting concerns
- Check out the [examples directory](../../examples/) for more advanced examples 