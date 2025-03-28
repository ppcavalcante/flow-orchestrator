package workflow

import (
	"context"
	"fmt"
	"time"
)

// Workflow represents a workflow execution.
// It combines a DAG with execution context like ID and persistence.
type Workflow struct {
	// DAG is the directed acyclic graph representing the workflow structure
	DAG *DAG

	// WorkflowID uniquely identifies this workflow
	WorkflowID string

	// Store is used for persisting workflow state
	Store WorkflowStore
}

// NewWorkflow creates a new workflow with the given workflow store
func NewWorkflow(store WorkflowStore) *Workflow {
	return &Workflow{
		DAG:        NewDAG("workflow"),
		WorkflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
		Store:      store,
	}
}

// AddNode adds a node to the workflow.
// Returns an error if a node with the same name already exists.
func (w *Workflow) AddNode(node *Node) error {
	return w.DAG.AddNode(node)
}

// AddDependency adds a dependency between nodes.
// Returns an error if either node doesn't exist or if adding the dependency would create a cycle.
func (w *Workflow) AddDependency(from, to string) error {
	return w.DAG.AddDependency(from, to)
}

// WithWorkflowID sets the workflow ID.
// Returns the workflow for method chaining.
func (w *Workflow) WithWorkflowID(id string) *Workflow {
	w.WorkflowID = id
	return w
}

// Execute runs the workflow.
// It loads any existing state, executes the DAG, and persists the final state.
// Returns an error if execution fails.
func (w *Workflow) Execute(ctx context.Context) error {
	// Create workflow data
	data := NewWorkflowData(w.WorkflowID)

	// Load existing state if available
	if w.Store != nil {
		existingData, err := w.Store.Load(w.WorkflowID)
		if err == nil && existingData != nil {
			data = existingData
		}
	}

	// Validate DAG
	if err := w.DAG.Validate(); err != nil {
		return fmt.Errorf("workflow validation failed: %w", err)
	}

	// Execute DAG
	if err := w.DAG.Execute(ctx, data); err != nil {
		// Save failed state
		if w.Store != nil {
			saveErr := w.Store.Save(data)
			if saveErr != nil {
				fmt.Printf("Failed to save state: %v\n", saveErr)
			}
		}
		return err
	}

	// Save final state
	if w.Store != nil {
		err := w.Store.Save(data)
		if err != nil {
			fmt.Printf("Failed to save state: %v\n", err)
			return err
		}
	}

	return nil
}

// FromBuilder creates a workflow from a builder.
// Returns an error if the workflow definition is invalid.
func FromBuilder(builder *WorkflowBuilder) (*Workflow, error) {
	dag, err := builder.Build()
	if err != nil {
		return nil, err
	}

	return &Workflow{
		DAG:        dag,
		WorkflowID: builder.workflowID,
		Store:      builder.store,
	}, nil
}

// NewWorkflowFromBuilder creates a new workflow builder.
// This is a convenience function for the fluent API.
func NewWorkflowFromBuilder() *WorkflowBuilder {
	return NewWorkflowBuilder()
}
