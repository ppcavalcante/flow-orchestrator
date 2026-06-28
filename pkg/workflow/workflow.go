package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

	// Load existing state if available.
	//
	// A missing workflow (ErrNotFound) is the expected "no prior state" case —
	// resume simply starts fresh with the newly created data. Any OTHER error
	// (e.g. ErrCorruptData from a malformed/truncated persisted payload, or an
	// I/O failure) must NOT be swallowed: silently discarding it would start
	// fresh and overwrite the persisted state on the next Save, losing it.
	// Propagate it so a corrupt resume surfaces instead of masquerading as a
	// clean first run.
	if w.Store != nil {
		existingData, err := w.Store.Load(w.WorkflowID)
		switch {
		case err == nil:
			if existingData != nil {
				data = existingData
				// Graph-identity guard (M9 crash-resume): the persisted state was
				// produced by SOME DAG under this WorkflowID; on resume it must be
				// consistent with the CURRENT DAG, or we would rehydrate node
				// statuses/outputs that no longer correspond to the graph and
				// silently mis-resume. Validate that every persisted node name
				// still exists in this DAG; reject loudly on a mismatch rather than
				// guessing. This is the tractable, node-identity analog of
				// Temporal's workflow-versioning problem (we check graph identity,
				// not code shape). (DEC-M9, chunk 2.)
				if err := w.checkGraphIdentity(data); err != nil {
					return err
				}
			}
		case errors.Is(err, ErrNotFound):
			// No prior state — start fresh with the new data.
		default:
			return fmt.Errorf("failed to load workflow state: %w", err)
		}
	}

	// Validate DAG
	if err := w.DAG.Validate(); err != nil {
		return fmt.Errorf("workflow validation failed: %w", err)
	}

	// Wire durable mid-run checkpointing when the Store supports it (M9). A Store
	// that implements Checkpointer gets a per-level checkpoint flush inside
	// DAG.Execute (via the internal ExecutionConfig callback); a Store that does
	// not is unaffected (callback stays nil → zero overhead, save-at-boundaries
	// only). (DEC-M9, chunk 2.)
	if cp, ok := w.Store.(Checkpointer); ok {
		w.DAG.config.checkpoint = func(d *WorkflowData) error {
			return cp.SaveCheckpoint(d)
		}
		// Clear the callback after the run so a later Execute re-decides based on
		// the Store then in effect (and so the DAG carries no stale closure).
		defer func() { w.DAG.config.checkpoint = nil }()
	}

	// Execute DAG
	if err := w.DAG.Execute(ctx, data); err != nil {
		// Save failed state
		if w.Store != nil {
			if saveErr := w.Store.Save(data); saveErr != nil {
				return fmt.Errorf("%w (additionally, failed to save state: %w)", err, saveErr)
			}
		}
		return err
	}

	// Save final state
	if w.Store != nil {
		if err := w.Store.Save(data); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	return nil
}

// checkGraphIdentity verifies that the persisted state being resumed is
// consistent with the current DAG: every node name carrying a persisted status
// must still exist in this DAG. A persisted status for a node the DAG no longer
// has means the graph changed between the original run and this resume, so the
// rehydrated statuses/outputs cannot be trusted — we return an error instead of
// silently mis-resuming. (DEC-M9, chunk 2; the node-identity analog of workflow
// versioning.) Only node IDENTITY is checked, not action/code shape.
func (w *Workflow) checkGraphIdentity(data *WorkflowData) error {
	var unknown []string
	data.ForEachNodeStatus(func(nodeName string, _ NodeStatus) {
		if _, exists := w.DAG.GetNode(nodeName); !exists {
			unknown = append(unknown, nodeName)
		}
	})
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%w: cannot resume workflow %q: persisted state references node(s) %v not present in the current DAG (the graph changed since the checkpoint)",
			ErrValidation, w.WorkflowID, unknown)
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
