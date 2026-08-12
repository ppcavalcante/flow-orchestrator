// Command 08-sub-workflows is composition: a PARENT workflow runs a whole CHILD graph as
// a single node.
//
//	parent:  prepare ──▶ [index]  ──▶ finalize
//	                        │
//	                        └─ child:  do-index   (its own DAG, its own journal)
//
// AddSubWorkflow spawns and awaits a definition-value child DAG IN-PROCESS under a
// deterministic child WorkflowID (f(parentID, nodeName)) with the child's OWN journal —
// parent and child are DISTINCT durable workflows (one-writer-per-workflow preserved).
// On child success, WithResult copies a child DATA key up into parent data; on child
// failure the sub-workflow node fails (fail-fast). The spawn is idempotent: a crash-resume
// that finds the child already completed does not re-run it.
//
// The child here is INLINE (it blocks the parent), so it may not contain a suspendable
// node — Build scans the whole child closure and refuses one (route a parking child to
// AddSubWorkflowParked / AddSubWorkflowQueued instead). Nesting is bounded by
// MaxSubWorkflowDepth (default 8) — a loud ErrSubWorkflowMaxDepth, never a silent cap.
//
// Run it:
//
//	go run ./examples/08-sub-workflows
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Keys and identifiers. keyChildCount is the child's DATA key that WithResult reads and
// copies into the parent under keyIndexed — a SCALAR (int64) result round-trips
// type-faithfully across every store.
const (
	parentWorkflowID = "doc-pipeline"
	nodePrepare      = "prepare"
	nodeIndex        = "index"    // the AddSubWorkflow node — runs the child DAG
	nodeFinalize     = "finalize" // downstream of index; consumes the child's result
	keyDocCount      = "doc_count"
	keyIndexed       = "indexed_total" // parent key WithResult writes the child result into

	childStartNode = "do-index"
	keyChildDone   = "child_done"  // set in the CHILD's own data (proves the child ran)
	keyChildCount  = "index_count" // the child's result data key the parent reads
)

// buildIndexChild constructs the child DAG. It is built with a bare builder (no store) —
// Build() returns a *DAG, which is exactly what AddSubWorkflow consumes; the child inherits
// the parent's store at spawn time under its own deterministic id.
func buildIndexChild() (*workflow.DAG, error) {
	cb := workflow.NewWorkflowBuilder().WithWorkflowID("index-child")
	cb.AddStartNode(childStartNode).WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
		// The child does not receive the parent's data directly (a def-value child runs
		// under its own id); it indexes its own fixed corpus and records the result.
		const indexed = 3
		d.Set(keyChildDone, true)
		d.Set(keyChildCount, int64(indexed))
		fmt.Printf("child do-index: indexed %d documents (in the child's own journal)\n", indexed)
		return nil
	})
	return cb.Build()
}

// buildParentWorkflow composes the child DAG as the "index" node. It is a standalone helper
// so the smoke test builds the identical graph. AddSubWorkflow requires the run to have a
// Store; the store-backed run goes through FromBuilder.
func buildParentWorkflow(store workflow.WorkflowStore) (*workflow.Workflow, error) {
	child, err := buildIndexChild()
	if err != nil {
		return nil, fmt.Errorf("build child: %w", err)
	}

	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(parentWorkflowID).
		WithStore(store)

	b.AddStartNode(nodePrepare).WithActionFunc(prepare)

	// The child runs as one node. WithResult declares that the child's keyChildCount data
	// key is written into parent data under keyIndexed on child success. The action is set
	// by AddSubWorkflow directly — do not also call WithAction.
	b.AddSubWorkflow(nodeIndex, child).
		WithResult(keyIndexed, keyChildCount).
		DependsOn(nodePrepare)

	b.AddNode(nodeFinalize).WithActionFunc(finalize).DependsOn(nodeIndex)

	return workflow.FromBuilder(b)
}

// prepare seeds the parent with a document count.
func prepare(_ context.Context, data *workflow.WorkflowData) error {
	const docs = 3
	data.Set(keyDocCount, int64(docs))
	fmt.Printf("prepare: %d documents to index\n", docs)
	return nil
}

// finalize consumes the child's result (copied up by WithResult) — the parent sees the
// child's output as ordinary data.
func finalize(_ context.Context, data *workflow.WorkflowData) error {
	indexed, ok := data.GetInt64(keyIndexed)
	if !ok {
		return fmt.Errorf("finalize: %q not written by the index sub-workflow", keyIndexed)
	}
	fmt.Printf("finalize: parent sees indexed_total=%d from the child\n", indexed)
	return nil
}

func run() error {
	store := workflow.NewInMemoryStore()

	wf, err := buildParentWorkflow(store)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// A straightforward successful composition — a non-nil Execute here is a real defect.
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	// The parent's durable result carries the child's copied-up value.
	parent, err := store.Load(parentWorkflowID)
	if err != nil {
		return fmt.Errorf("load parent: %w", err)
	}
	indexed, _ := parent.GetInt64(keyIndexed)

	// The child is a DISTINCT durable workflow under a deterministic id — its own journal
	// is loadable independently, proving it ran as its own run rather than inline code.
	childID := workflow.SubWorkflowChildID(parentWorkflowID, nodeIndex)
	child, err := store.Load(childID)
	if err != nil {
		return fmt.Errorf("load child %q: %w", childID, err)
	}
	childDone, _ := child.GetBool(keyChildDone)

	fmt.Printf("\nresult: parent indexed_total=%d, child(%s) done=%v\n", indexed, childID, childDone)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("08-sub-workflows: %v", err)
	}
}
