package benchmark

import (
	"context"
	"testing"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
	"github.com/pparaujo/flow-orchestrator/pkg/workflow/metrics"
)

// BenchmarkRealWorldScenarios benchmarks realistic workflow patterns
func BenchmarkRealWorldScenarios(b *testing.B) {
	// E-commerce checkout workflow
	b.Run("E-CommerceCheckout", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			benchmarkECommerceCheckout(b)
		}
	})

	// ETL data processing workflow
	b.Run("ETLDataProcessing", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			benchmarkETLProcessing(b)
		}
	})

	// API orchestration with external calls
	b.Run("APIOrchestration", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			benchmarkAPIOrchestration(b)
		}
	})

	// Approval process with conditional paths
	b.Run("ApprovalProcess", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			benchmarkApprovalProcess(b)
		}
	})
}

// E-commerce checkout workflow benchmark
func benchmarkECommerceCheckout(b *testing.B) {
	b.StopTimer()
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("checkout-flow")

	// Add nodes representing a typical e-commerce checkout flow
	builder.AddStartNode("validate-cart").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate cart validation
			data.Set("is_valid", true)
			data.Set("cart_total", 129.99)
			return nil
		})

	builder.AddNode("calculate-tax").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate tax calculation
			total, _ := data.GetFloat64("cart_total")
			data.Set("tax", total*0.08)
			return nil
		}).
		DependsOn("validate-cart")

	builder.AddNode("calculate-shipping").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate shipping calculation
			data.Set("shipping", 10.50)
			return nil
		}).
		DependsOn("validate-cart")

	builder.AddNode("process-payment").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate payment processing
			cartTotal, _ := data.GetFloat64("cart_total")
			tax, _ := data.GetFloat64("tax")
			shipping, _ := data.GetFloat64("shipping")
			data.Set("final_total", cartTotal+tax+shipping)
			data.Set("payment_id", "pmt_123456789")
			return nil
		}).
		DependsOn("calculate-tax", "calculate-shipping")

	builder.AddNode("create-order").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate order creation
			data.Set("order_id", "ord_987654321")
			return nil
		}).
		DependsOn("process-payment")

	builder.AddNode("send-confirmation").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate sending confirmation email
			return nil
		}).
		DependsOn("create-order")

	dag, err := builder.Build()
	if err != nil {
		b.Fatalf("Failed to build workflow: %v", err)
	}

	// Start actual benchmark
	b.StartTimer()

	// Use arena-based WorkflowData to avoid concurrent map access issues
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metrics.NewConfig().WithEnabled(false) // Disable metrics to avoid concurrent map access
	data := workflow.NewWorkflowDataWithArena("checkout-123", config)

	ctx := context.Background()
	err = dag.Execute(ctx, data)
	if err != nil {
		b.Fatalf("Failed to execute workflow: %v", err)
	}
}

// ETL data processing workflow benchmark
func benchmarkETLProcessing(b *testing.B) {
	b.StopTimer()
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("etl-flow")

	// Add nodes representing a typical ETL process
	builder.AddStartNode("extract-data").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate data extraction
			records := make([]map[string]interface{}, 100)
			for i := 0; i < 100; i++ {
				records[i] = map[string]interface{}{
					"id":    i,
					"value": i * 10,
					"name":  "Record-" + string(rune(65+i%26)),
				}
			}
			data.Set("records", records)
			return nil
		})

	builder.AddNode("validate-schema").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate schema validation
			data.Set("schema_valid", true)
			return nil
		}).
		DependsOn("extract-data")

	builder.AddNode("transform-data").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate data transformation
			records, _ := data.Get("records")
			transformedData := records // In a real scenario, this would be transformed
			data.Set("transformed_data", transformedData)
			return nil
		}).
		DependsOn("validate-schema")

	builder.AddNode("enrich-data").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate data enrichment - adding fields
			data.Set("enrichment_complete", true)
			return nil
		}).
		DependsOn("transform-data")

	builder.AddNode("load-data").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate loading data to destination
			data.Set("load_success", true)
			data.Set("records_loaded", 100)
			return nil
		}).
		DependsOn("enrich-data")

	dag, err := builder.Build()
	if err != nil {
		b.Fatalf("Failed to build workflow: %v", err)
	}

	// Start actual benchmark
	b.StartTimer()

	// Use arena-based WorkflowData to avoid concurrent map access issues
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metrics.NewConfig().WithEnabled(false) // Disable metrics for benchmark
	data := workflow.NewWorkflowDataWithArena("etl-process", config)

	ctx := context.Background()
	err = dag.Execute(ctx, data)
	if err != nil {
		b.Fatalf("Failed to execute workflow: %v", err)
	}
}

// API orchestration workflow benchmark
func benchmarkAPIOrchestration(b *testing.B) {
	b.StopTimer()
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("api-flow")

	// Simulating API calls with mock actions
	builder.AddStartNode("fetch-user").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate user API call
			data.Set("user", map[string]interface{}{
				"id":    12345,
				"name":  "John Doe",
				"email": "john@example.com",
			})
			return nil
		})

	builder.AddNode("fetch-product").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate product API call
			data.Set("product", map[string]interface{}{
				"id":    67890,
				"name":  "Awesome Product",
				"price": 99.99,
			})
			return nil
		})

	builder.AddNode("check-inventory").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate inventory API call
			data.Set("in_stock", true)
			data.Set("quantity", 42)
			return nil
		}).
		DependsOn("fetch-product")

	builder.AddNode("check-user-credit").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate credit check API call
			data.Set("credit_approved", true)
			return nil
		}).
		DependsOn("fetch-user")

	builder.AddNode("create-order").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate order creation API call
			creditApproved, _ := data.GetBool("credit_approved")
			inStock, _ := data.GetBool("in_stock")

			if creditApproved && inStock {
				data.Set("order_id", "order-123-456")
				data.Set("status", "created")
			}
			return nil
		}).
		DependsOn("check-inventory", "check-user-credit")

	dag, err := builder.Build()
	if err != nil {
		b.Fatalf("Failed to build workflow: %v", err)
	}

	// Start actual benchmark
	b.StartTimer()

	// Use arena-based WorkflowData to avoid concurrent map access issues
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metrics.NewConfig().WithEnabled(false) // Disable metrics for benchmark
	data := workflow.NewWorkflowDataWithArena("api-orchestration", config)

	ctx := context.Background()
	err = dag.Execute(ctx, data)
	if err != nil {
		b.Fatalf("Failed to execute workflow: %v", err)
	}
}

// Approval process workflow benchmark
func benchmarkApprovalProcess(b *testing.B) {
	b.StopTimer()
	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("approval-flow")

	// Add nodes representing a business approval process
	builder.AddStartNode("submit-request").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate request submission
			data.Set("request", map[string]interface{}{
				"id":          "req-12345",
				"type":        "expense",
				"amount":      1250.00,
				"description": "Conference expenses",
				"requester":   "employee-789",
			})
			return nil
		})

	builder.AddNode("validate-request").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate request validation
			data.Set("is_valid", true)
			return nil
		}).
		DependsOn("submit-request")

	builder.AddNode("manager-approval").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate manager approval
			request, _ := data.Get("request")
			if req, ok := request.(map[string]interface{}); ok {
				if amount, ok := req["amount"].(float64); ok && amount <= 2000.0 {
					data.Set("manager_approved", true)
				} else {
					data.Set("manager_approved", false)
					data.Set("needs_director", true)
				}
			}
			return nil
		}).
		DependsOn("validate-request")

	builder.AddNode("director-approval").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate director approval (only if needed)
			needsDirector, _ := data.GetBool("needs_director")
			if needsDirector {
				data.Set("director_approved", true)
			}
			return nil
		}).
		DependsOn("manager-approval")

	builder.AddNode("finance-processing").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate finance processing
			managerApproved, _ := data.GetBool("manager_approved")
			directorApproved, directorFound := data.GetBool("director_approved")

			if managerApproved || (directorFound && directorApproved) {
				data.Set("status", "approved")
				data.Set("payment_scheduled", true)
			} else {
				data.Set("status", "rejected")
			}
			return nil
		}).
		DependsOn("manager-approval", "director-approval")

	builder.AddNode("notify-requester").
		WithAction(func(ctx context.Context, data *workflow.WorkflowData) error {
			// Simulate notification
			return nil
		}).
		DependsOn("finance-processing")

	dag, err := builder.Build()
	if err != nil {
		b.Fatalf("Failed to build workflow: %v", err)
	}

	// Start actual benchmark
	b.StartTimer()

	// Use arena-based WorkflowData to avoid concurrent map access issues
	config := workflow.DefaultWorkflowDataConfig()
	config.MetricsConfig = metrics.NewConfig().WithEnabled(false) // Disable metrics for benchmark
	data := workflow.NewWorkflowDataWithArena("approval-process", config)

	ctx := context.Background()
	err = dag.Execute(ctx, data)
	if err != nil {
		b.Fatalf("Failed to execute workflow: %v", err)
	}
}
