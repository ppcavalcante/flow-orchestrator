/*
Package workflow provides a lightweight, high-performance workflow orchestration engine for Go applications.

Flow Orchestrator allows you to define, execute, and monitor complex workflows with a clean, fluent API
while handling parallelism, dependencies, error handling, and persistence automatically.

# Core Concepts

## Workflow Builder

The recommended way to create workflows is using the fluent builder API:

	builder := workflow.NewWorkflowBuilder().
		WithWorkflowID("my-workflow").
		WithStore(store)

	builder.AddStartNode("fetch-data").
		WithAction(fetchAction).
		WithRetries(3)

	builder.AddNode("process-data").
		WithAction(processAction).
		WithTimeout(5 * time.Second).
		DependsOn("fetch-data")

	dag, err := builder.Build()

## Actions

Actions are the executable units of a workflow:

	action := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		// Perform some work
		data.Set("result", "success")
		return nil
	})

## WorkflowData

WorkflowData is the central data store for workflow execution:

	data := workflow.NewWorkflowData("my-workflow-id")
	data.Set("user_id", 12345)
	userID, _ := data.GetInt("user_id")
	output, exists := data.GetOutput("fetch-data")

## Middleware

Middleware provides cross-cutting concerns like logging, retries, and timeouts:

	stack := workflow.NewMiddlewareStack()
	stack.Use(workflow.LoggingMiddleware())
	stack.Use(workflow.RetryMiddleware(3, 1*time.Second))
	wrappedAction := stack.Apply(myAction)

## Persistence

Workflow state can be persisted using various storage backends:

	// In-memory store (for testing)
	store := workflow.NewInMemoryStore()

	// File-based store
	store, err := workflow.NewJSONFileStore("./workflows")

# Examples

See the examples directory for complete working examples:

- Simple workflow: examples/new_simple
- API workflow: examples/api_workflow
- Error handling: examples/error_handling
- Comprehensive example: examples/comprehensive
*/
package workflow
