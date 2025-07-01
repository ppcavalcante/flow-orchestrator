# Your First Workflow

This step-by-step tutorial will guide you through creating a complete, real-world workflow using Flow Orchestrator. By the end, you'll have a solid understanding of how to build workflows for your own applications.

## Overview

We'll create an e-commerce order processing workflow that includes:

1. Order validation
2. Inventory checking
3. Payment processing
4. Order fulfillment
5. Notification sending

This workflow demonstrates key features including:
- Parallel execution
- Conditional paths
- Error handling
- Middleware for logging and retries
- Persistent workflow state

## Prerequisites

- Go 1.18 or higher
- Basic understanding of Go
- Flow Orchestrator installed (see [Installation Guide](./installation.md))

## Project Setup

First, let's create a new directory for our project:

```bash
mkdir order-workflow
cd order-workflow
```

Initialize a new Go module:

```bash
go mod init order-workflow
```

Add Flow Orchestrator as a dependency:

```bash
go get github.com/ppcavalcante/flow-orchestrator@v0.1.1-alpha
```

## Step 1: Create the Main Application File

Create a file named `main.go` with basic imports:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

func main() {
	// We'll implement this next
}
```

## Step 2: Define Our Order Data Model

Let's define our order data model:

```go
// Add this inside main.go

// Order represents an e-commerce order
type Order struct {
	ID             string    `json:"id"`
	CustomerID     string    `json:"customer_id"`
	Items          []Item    `json:"items"`
	Total          float64   `json:"total"`
	PaymentMethod  string    `json:"payment_method"`
	ShippingAddress string    `json:"shipping_address"`
	CreatedAt      time.Time `json:"created_at"`
}

// Item represents an order item
type Item struct {
	ProductID string  `json:"product_id"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	Quantity  int     `json:"quantity"`
}
```

## Step 3: Create Action Functions

Now, let's implement action functions for each step in our workflow:

```go
// Add to main.go

// validateOrderAction checks that the order is valid
func validateOrderAction(ctx context.Context, data *workflow.WorkflowData) error {
	log.Println("Validating order...")
	
	// Get order from workflow data
	orderJSON, _ := data.GetString("order")
	
	var order Order
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return fmt.Errorf("invalid order data: %w", err)
	}
	
	// Basic validation checks
	if order.ID == "" || order.CustomerID == "" {
		return fmt.Errorf("order missing required fields")
	}
	
	if len(order.Items) == 0 {
		return fmt.Errorf("order has no items")
	}
	
	// Calculate total to verify
	calculatedTotal := 0.0
	for _, item := range order.Items {
		calculatedTotal += item.Price * float64(item.Quantity)
	}
	
	// Allow for small floating-point differences
	if abs(calculatedTotal-order.Total) > 0.01 {
		return fmt.Errorf("order total mismatch: got %.2f, calculated %.2f", 
			order.Total, calculatedTotal)
	}
	
	log.Println("Order validation successful")
	data.Set("is_valid", true)
	return nil
}

// checkInventoryAction verifies product availability
func checkInventoryAction(ctx context.Context, data *workflow.WorkflowData) error {
	log.Println("Checking inventory...")
	
	// Get order from workflow data
	orderJSON, _ := data.GetString("order")
	
	var order Order
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return fmt.Errorf("invalid order data: %w", err)
	}
	
	// Simulate inventory check (in a real app, this would query a database)
	for _, item := range order.Items {
		// Simulate 10% chance of inventory shortage
		if item.ProductID == "LOW_STOCK_ITEM" {
			data.Set("inventory_available", false)
			return fmt.Errorf("insufficient inventory for product: %s", item.ProductID)
		}
	}
	
	log.Println("All items in stock")
	data.Set("inventory_available", true)
	return nil
}

// processPaymentAction handles payment processing
func processPaymentAction(ctx context.Context, data *workflow.WorkflowData) error {
	log.Println("Processing payment...")
	
	// Get order
	orderJSON, _ := data.GetString("order")
	
	var order Order
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return fmt.Errorf("invalid order data: %w", err)
	}
	
	// Simulate payment processing time
	time.Sleep(500 * time.Millisecond)
	
	// Simulate payment (in a real app, this would call a payment gateway)
	paymentID := fmt.Sprintf("pmt_%d", time.Now().Unix())
	
	// Simulate payment failure for specific payment method
	if order.PaymentMethod == "INVALID_CARD" {
		return fmt.Errorf("payment declined")
	}
	
	log.Printf("Payment successful with ID: %s", paymentID)
	data.Set("payment_id", paymentID)
	return nil
}

// fulfillOrderAction handles order fulfillment
func fulfillOrderAction(ctx context.Context, data *workflow.WorkflowData) error {
	log.Println("Fulfilling order...")
	
	// Get order
	orderJSON, _ := data.GetString("order")
	
	var order Order
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return fmt.Errorf("invalid order data: %w", err)
	}
	
	// Check payment was successful
	paymentID, ok := data.GetString("payment_id")
	if !ok || paymentID == "" {
		return fmt.Errorf("missing payment information")
	}
	
	// Simulate fulfillment process
	time.Sleep(300 * time.Millisecond)
	
	fulfillmentID := fmt.Sprintf("ful_%d", time.Now().Unix())
	log.Printf("Order fulfilled with ID: %s", fulfillmentID)
	
	data.Set("fulfillment_id", fulfillmentID)
	data.Set("fulfillment_date", time.Now().Format(time.RFC3339))
	return nil
}

// sendNotificationAction sends order confirmation to customer
func sendNotificationAction(ctx context.Context, data *workflow.WorkflowData) error {
	log.Println("Sending notification...")
	
	// Get order
	orderJSON, _ := data.GetString("order")
	
	var order Order
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return fmt.Errorf("invalid order data: %w", err)
	}
	
	// Simulate sending email
	time.Sleep(200 * time.Millisecond)
	
	notificationID := fmt.Sprintf("not_%d", time.Now().Unix())
	log.Printf("Notification sent with ID: %s", notificationID)
	
	data.Set("notification_id", notificationID)
	return nil
}

// Helper function for float comparison
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
```

## Step 4: Define the Workflow

Now, let's create the workflow using Flow Orchestrator's builder pattern:

```go
// Add to main.go

func buildOrderWorkflow() (*workflow.DAG, error) {
	// Create a workflow builder
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("order-processing")
	
	// Create middleware stack for logging and retries
	stack := workflow.NewMiddlewareStack()
	stack.Use(workflow.LoggingMiddleware())
	stack.Use(workflow.RetryMiddleware(2, 500*time.Millisecond))
	
	// Add workflow steps
	builder.AddStartNode("validate-order").
		WithAction(stack.Apply(validateOrderAction))
	
	builder.AddNode("check-inventory").
		WithAction(stack.Apply(checkInventoryAction)).
		DependsOn("validate-order")
	
	builder.AddNode("process-payment").
		WithAction(stack.Apply(processPaymentAction)).
		DependsOn("check-inventory")
	
	builder.AddNode("fulfill-order").
		WithAction(stack.Apply(fulfillOrderAction)).
		DependsOn("process-payment")
	
	builder.AddNode("send-notification").
		WithAction(stack.Apply(sendNotificationAction)).
		DependsOn("fulfill-order")
	
	// Build the workflow
	return builder.Build()
}
```

## Step 5: Implement the Main Function

Now, let's implement the main function to create and execute the workflow:

```go
// Replace the main function in main.go

func main() {
	fmt.Println("=== Order Processing Workflow ===")

	// Build the workflow
	dag, err := buildOrderWorkflow()
	if err != nil {
		log.Fatalf("Failed to build workflow: %v", err)
	}

	// Create workflow data
	data := workflow.NewWorkflowData("order-processing")

	// Create a sample order
	order := Order{
		ID:             "ord_123456",
		CustomerID:     "cust_789012",
		PaymentMethod:  "credit_card",
		ShippingAddress: "123 Main St, Anytown, AT 12345",
		CreatedAt:      time.Now(),
		Items: []Item{
			{
				ProductID: "prod_1",
				Name:      "Widget A",
				Price:     29.99,
				Quantity:  2,
			},
			{
				ProductID: "prod_2",
				Name:      "Widget B",
				Price:     49.99,
				Quantity:  1,
			},
		},
		Total: 109.97, // 29.99*2 + 49.99
	}

	// Serialize order to JSON and store in workflow data
	orderJSON, _ := json.Marshal(order)
	data.Set("order", string(orderJSON))

	// Execute the workflow
	fmt.Println("Starting workflow execution...")
	startTime := time.Now()

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Execute the workflow
	err = dag.Execute(ctx, data)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Workflow completed in %v\n", time.Since(startTime))
		printWorkflowResults(data)
	}
}

// printWorkflowResults displays the workflow results
func printWorkflowResults(data *workflow.WorkflowData) {
	fmt.Println("\n=== Workflow Results ===")
	
	fmt.Println("\nOrder Processing Status:")
	
	// Print node statuses
	nodes := []string{
		"validate-order", 
		"check-inventory", 
		"process-payment", 
		"fulfill-order", 
		"send-notification",
	}
	
	for _, node := range nodes {
		status, _ := data.GetNodeStatus(node)
		fmt.Printf("- %s: %s\n", node, status)
	}
	
	fmt.Println("\nWorkflow Data:")
	
	// Get various data values
	isValid, _ := data.GetBool("is_valid")
	inventoryAvailable, _ := data.GetBool("inventory_available")
	paymentID, _ := data.GetString("payment_id")
	fulfillmentID, _ := data.GetString("fulfillment_id")
	fulfillmentDate, _ := data.GetString("fulfillment_date")
	notificationID, _ := data.GetString("notification_id")
	
	fmt.Printf("- Valid order: %v\n", isValid)
	fmt.Printf("- Inventory available: %v\n", inventoryAvailable)
	fmt.Printf("- Payment ID: %s\n", paymentID)
	fmt.Printf("- Fulfillment ID: %s\n", fulfillmentID)
	fmt.Printf("- Fulfillment date: %s\n", fulfillmentDate)
	fmt.Printf("- Notification ID: %s\n", notificationID)
}
```

## Step 6: Run the Workflow

Save the file and run the application:

```bash
go run main.go
```

You should see output showing the workflow execution steps, with logs for each node and the final results.

## Step 7: Add Persistence

Let's modify our workflow to use persistence:

```go
// Update the main function to use persistence

func main() {
	fmt.Println("=== Order Processing Workflow ===")

	// Create a persistence store
	store, err := workflow.NewJSONFileStore("./workflow_data.json")
	if err != nil {
		log.Fatalf("Failed to create store: %v", err)
	}

	// Build the workflow
	dag, err := buildOrderWorkflow()
	if err != nil {
		log.Fatalf("Failed to build workflow: %v", err)
	}

	// Create the workflow with persistence
	wf := &workflow.Workflow{
		DAG:        dag,
		WorkflowID: "order-processing",
		Store:      store,
	}

	// Create a sample order
	order := Order{
		ID:             "ord_123456",
		CustomerID:     "cust_789012",
		PaymentMethod:  "credit_card",
		ShippingAddress: "123 Main St, Anytown, AT 12345",
		CreatedAt:      time.Now(),
		Items: []Item{
			{
				ProductID: "prod_1",
				Name:      "Widget A",
				Price:     29.99,
				Quantity:  2,
			},
			{
				ProductID: "prod_2",
				Name:      "Widget B",
				Price:     49.99,
				Quantity:  1,
			},
		},
		Total: 109.97, // 29.99*2 + 49.99
	}

	// Serialize order to JSON
	orderJSON, _ := json.Marshal(order)

	// Check if we have an existing workflow state
	existingData, err := store.Load("order-processing")
	if err == nil {
		// We have existing state - get the order ID to display
		var existingOrder Order
		if orderJSONStr, ok := existingData.GetString("order"); ok {
			_ = json.Unmarshal([]byte(orderJSONStr), &existingOrder)
			fmt.Printf("Resuming workflow for order: %s\n", existingOrder.ID)
		}
	} else {
		// No existing state - initialize the workflow data
		fmt.Println("Starting new workflow execution...")
		existingData = workflow.NewWorkflowData("order-processing")
		existingData.Set("order", string(orderJSON))
	}

	// Execute the workflow with existing data
	startTime := time.Now()

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Execute the workflow
	err = wf.Execute(ctx)
	if err != nil {
		fmt.Printf("Workflow failed: %v\n", err)
	} else {
		fmt.Printf("Workflow completed in %v\n", time.Since(startTime))
		
		// Load the final state
		finalData, _ := store.Load("order-processing")
		printWorkflowResults(finalData)
	}
}
```

## Step 8: Experimenting with the Workflow

Let's try modifying our order to see the error handling in action:

1. Try changing the `Total` to see validation failures
2. Change a `ProductID` to "LOW_STOCK_ITEM" to simulate inventory shortages
3. Change the `PaymentMethod` to "INVALID_CARD" to see payment failures

## What You've Learned

In this tutorial, you've learned how to:

1. Define a multi-step workflow with dependencies
2. Create action functions for workflow nodes
3. Use middleware for logging and retries
4. Handle errors in workflow steps
5. Create and use persistent workflow state
6. Execute and monitor a workflow

## Next Steps

Now that you've built your first workflow, you might want to explore:

- [Middleware System](../guides/middleware.md) for more advanced middleware usage
- [DAG Execution Model](../architecture/dag-execution.md) for a deeper understanding of workflow execution
- [Workflow Patterns](../guides/workflow-patterns.md) for common workflow patterns
- [Performance Optimization](../guides/performance-optimization.md) for optimizing workflows 