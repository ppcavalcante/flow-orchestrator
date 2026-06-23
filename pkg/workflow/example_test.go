package workflow_test

import (
	"context"
	"fmt"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Example shows the core happy path: build a small DAG with a dependency,
// then execute it against a WorkflowData. Actions communicate by reading and
// writing keys on the shared WorkflowData.
func Example() {
	// fetch produces a value; process consumes it.
	fetch := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		data.Set("count", 21)
		return nil
	})
	process := workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
		n, _ := data.GetInt("count")
		data.Set("result", n*2)
		return nil
	})

	builder := workflow.NewWorkflowBuilder().WithWorkflowID("doubler")
	builder.AddStartNode("fetch").WithAction(fetch)
	builder.AddNode("process").WithAction(process).DependsOn("fetch")

	dag, err := builder.Build()
	if err != nil {
		fmt.Println("build error:", err)
		return
	}

	data := workflow.NewWorkflowData("doubler")
	if err := dag.Execute(context.Background(), data); err != nil {
		fmt.Println("execute error:", err)
		return
	}

	result, _ := data.GetInt("result")
	fmt.Println("result:", result)
	// Output: result: 42
}

// ExampleWorkflowData demonstrates the string-keyed data API: values are stored
// under string keys and read back with type-specific accessors.
func ExampleWorkflowData() {
	data := workflow.NewWorkflowData("demo")

	data.Set("user_id", 12345)
	data.Set("status", "active")

	id, _ := data.GetInt("user_id")
	status, _ := data.GetString("status")

	fmt.Println(id, status)
	// Output: 12345 active
}

// ExampleKey demonstrates the typed-key data API. A Key[T] carries the value
// type alongside its name, so producer and consumer that share the key are
// checked at compile time — the recommended way to pass typed values between
// nodes.
func ExampleKey() {
	// Declare a typed key once and share it between producer and consumer.
	userIDKey := workflow.NewKey[int]("user_id")

	data := workflow.NewWorkflowData("demo")

	// Set is compile-time checked against the key's type.
	workflow.Set(data, userIDKey, 12345)

	// Get returns (value, true) only when a value of the right type is present.
	id, ok := workflow.Get(data, userIDKey)
	fmt.Println(id, ok)

	// GetOr returns a default when the key is absent or holds another type.
	missing := workflow.NewKey[int]("absent")
	fmt.Println(workflow.GetOr(data, missing, -1))
	// Output:
	// 12345 true
	// -1
}

// ExampleInMemoryStore shows a store round-trip: persist a WorkflowData and
// load it back. NewInMemoryStore is the simplest store; for on-disk persistence
// use NewFlatBuffersStore the same way.
func ExampleInMemoryStore() {
	store := workflow.NewInMemoryStore()

	data := workflow.NewWorkflowData("order-42")
	data.Set("total", 999)
	if err := store.Save(data); err != nil {
		fmt.Println("save error:", err)
		return
	}

	loaded, err := store.Load("order-42")
	if err != nil {
		fmt.Println("load error:", err)
		return
	}

	total, _ := loaded.GetInt("total")
	fmt.Println("total:", total)
	// Output: total: 999
}
