package main

import (
	"context"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestSubWorkflow_ParentAndChildEffectsLand runs the exact composition the command builds
// and asserts BOTH effects landed: the parent reached Completed with the child's result
// copied up, and the child ran as its own distinct durable workflow.
func TestSubWorkflow_ParentAndChildEffectsLand(t *testing.T) {
	store := workflow.NewInMemoryStore()

	wf, err := buildParentWorkflow(store)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Parent effect: every parent node reached Completed and the child's scalar result was
	// copied up into parent data via WithResult.
	parent, err := store.Load(parentWorkflowID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	for _, node := range []string{nodePrepare, nodeIndex, nodeFinalize} {
		if st, ok := parent.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("parent node %q status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
	if indexed, ok := parent.GetInt64(keyIndexed); !ok || indexed != 3 {
		t.Errorf("parent %s = %d (ok=%v), want 3 (the child's result copied up)", keyIndexed, indexed, ok)
	}

	// Child effect: the child ran as a DISTINCT durable workflow under its deterministic
	// id, with its own journal — loadable independently, its start node Completed, and its
	// own data marker set.
	childID := workflow.SubWorkflowChildID(parentWorkflowID, nodeIndex)
	child, err := store.Load(childID)
	if err != nil {
		t.Fatalf("load child %q: %v", childID, err)
	}
	if st, ok := child.GetNodeStatus(childStartNode); !ok || st != workflow.Completed {
		t.Errorf("child node %q status = %v (ok=%v), want Completed", childStartNode, st, ok)
	}
	if done, ok := child.GetBool(keyChildDone); !ok || !done {
		t.Errorf("child %s = %v (ok=%v), want true", keyChildDone, done, ok)
	}
	if count, ok := child.GetInt64(keyChildCount); !ok || count != 3 {
		t.Errorf("child %s = %d (ok=%v), want 3", keyChildCount, count, ok)
	}
}
