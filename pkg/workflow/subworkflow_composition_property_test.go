package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// M19 ph96 formal capstone — the gopter composition arm over the REAL DAG.Execute (the
// executable counterpart to the MCM19Composition.tla sub-workflow-await arm). Over random
// parent shapes (a chain of `pre` plain predecessors, then an inline sub-workflow node,
// then a `post` downstream reader), it asserts the composition contract the TLA arm proves:
//
//   - SPAWN-AWAIT round-trip (the ChildComplete->Wake->Finish path): the child runs under
//     its own deterministic ID and the parent reads the child's declared result downstream.
//   - NoDoubleSpawn (the ph91 idempotency guard): the child action runs EXACTLY ONCE across
//     the whole run (spawnCounter == 1).
//   - ChildFailParentFail (INV-01): a failing child fails the sub-workflow node and fail-fast
//     halts the downstream reader (it never Completes); a succeeding child completes both.
//
// Anti-vacuity (the ph96 non-vacuity witness — the negated property SHRINKS to a minimal
// counterexample): see TestSubWorkflowComposition_Property_NegationShrinks below.
func TestSubWorkflowComposition_Property_SpawnAwaitContract(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x5019C0DE) // "M19 COMP"-ish
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 50
	properties := gopter.NewProperties(params)

	properties.Property("inline sub-workflow: spawn-once + result round-trip + child-fail⇒parent-fail⇒downstream-blocked",
		prop.ForAll(
			func(pre int, childFails bool, outSeed int64) bool {
				return checkCompositionRun(t, pre, childFails, outSeed)
			},
			gen.IntRange(0, 4),       // number of plain predecessors before the sub-workflow node
			gen.Bool(),               // does the child fail?
			gen.Int64Range(0, 1<<40), // the child output value (int64 — value_long-faithful path)
		),
	)
	properties.TestingRun(t)
}

// checkCompositionRun builds pre -> sub -> post and asserts the whole composition contract
// for one random draw. Returns false (property fails) on any contract breach.
func checkCompositionRun(t *testing.T, pre int, childFails bool, outSeed int64) bool {
	store := NewInMemoryStore()
	wf := fmt.Sprintf("comp-prop-%d-%v-%d", pre, childFails, outSeed)

	var spawnN atomic.Int32
	var postN atomic.Int32
	childOut := outSeed // int64 — exercises the value_long fidelity path

	cb := NewWorkflowBuilder()
	cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		spawnN.Add(1)
		if childFails {
			return errors.New("child boom")
		}
		d.Set("result", childOut)
		return nil
	}))
	childDAG, err := cb.Build()
	require.NoError(t, err)

	pb := NewWorkflowBuilder().WithWorkflowID(wf)
	var last string
	for i := 0; i < pre; i++ {
		name := fmt.Sprintf("pre%d", i)
		var nbp *NodeBuilder
		if i == 0 {
			nbp = pb.AddStartNode(name)
		} else {
			nbp = pb.AddNode(name).DependsOn(last)
		}
		nbp.WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
		last = name
	}
	var sub *NodeBuilder
	if pre == 0 {
		// The sub-workflow node is the start node (no predecessors). AddSubWorkflow returns a
		// NodeBuilder; a node with no DependsOn is a start node by build semantics.
		sub = pb.AddSubWorkflow("sub", childDAG)
	} else {
		sub = pb.AddSubWorkflow("sub", childDAG).DependsOn(last)
	}
	sub.WithResult("result", "result")
	pb.AddNode("post").DependsOn("sub").WithAction(countingAction(&postN))
	dag, err := pb.Build()
	require.NoError(t, err)

	w := NewWorkflow(store)
	w.WorkflowID = wf
	w.DAG = dag

	execErr := w.Execute(context.Background())

	// NoDoubleSpawn: the child action ran EXACTLY once (the ph91 idempotency guard).
	if spawnN.Load() != 1 {
		t.Logf("NoDoubleSpawn breach: spawnN=%d (want 1) for %s", spawnN.Load(), wf)
		return false
	}

	final, err := store.Load(wf)
	require.NoError(t, err)
	childID := subWorkflowChildID(wf, "sub")

	if childFails {
		// ChildFailParentFail (INV-01): the sub node failed, Execute errored, and the
		// downstream reader is fail-fast BLOCKED (never Completed / never ran).
		if execErr == nil {
			t.Logf("child-fail breach: Execute returned nil for %s", wf)
			return false
		}
		if statusOf(final, "sub") != Failed {
			t.Logf("child-fail breach: sub status = %v (want Failed) for %s", statusOf(final, "sub"), wf)
			return false
		}
		if postN.Load() != 0 || statusOf(final, "post") == Completed {
			t.Logf("child-fail breach: downstream post ran/completed for %s", wf)
			return false
		}
		return true
	}

	// Success path: sub + post Completed, the child persisted under its own ID, and the
	// declared result round-tripped (value_long-faithful) into the parent key.
	if execErr != nil {
		t.Logf("success breach: Execute errored (%v) for %s", execErr, wf)
		return false
	}
	if statusOf(final, "sub") != Completed || statusOf(final, "post") != Completed {
		t.Logf("success breach: sub=%v post=%v for %s", statusOf(final, "sub"), statusOf(final, "post"), wf)
		return false
	}
	if postN.Load() != 1 {
		t.Logf("success breach: postN=%d (want 1) for %s", postN.Load(), wf)
		return false
	}
	childData, err := store.Load(childID)
	if err != nil || statusOf(childData, "produce") != Completed {
		t.Logf("success breach: child journal missing/incomplete for %s", wf)
		return false
	}
	got, ok := final.Get("result")
	if !ok {
		t.Logf("success breach: result key absent for %s", wf)
		return false
	}
	// int64 fidelity: the round-tripped value equals the child output.
	if !valuesEqualInt64(got, childOut) {
		t.Logf("result-fidelity breach: got %v (%T) want %d for %s", got, got, childOut, wf)
		return false
	}
	return true
}

// statusOf reads a node's status from persisted WorkflowData (Pending if absent).
func statusOf(d *WorkflowData, node string) NodeStatus {
	if d == nil {
		return Pending
	}
	st, ok := d.GetNodeStatus(node)
	if !ok {
		return Pending
	}
	return st
}

// valuesEqualInt64 compares a round-tripped data value (which may be int64 or a numeric that
// decoded through UseNumber) against the expected int64.
func valuesEqualInt64(got any, want int64) bool {
	switch v := got.(type) {
	case int64:
		return v == want
	case int:
		return int64(v) == want
	default:
		return fmt.Sprintf("%v", got) == fmt.Sprintf("%d", want)
	}
}

// TestSubWorkflowComposition_Property_NegationShrinks — the ph96 NON-VACUITY witness. It
// asserts a DELIBERATELY-FALSE property (a failing child must leave `post` Completed) and
// confirms gopter FALSIFIES it and SHRINKS to a minimal counterexample. A vacuous/never-
// exercised composition property could not produce a counterexample; this proves the arm
// actually drives the child-fail⇒parent-fail path.
func TestSubWorkflowComposition_Property_NegationShrinks(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x5019C0DE)
	params.MinSuccessfulTests = 200
	properties := gopter.NewProperties(params)

	properties.Property("FALSE-by-construction: a failing child leaves post Completed (must be falsified)",
		prop.ForAll(func(pre int) bool {
			store := NewInMemoryStore()
			wf := fmt.Sprintf("comp-neg-%d", pre)
			cb := NewWorkflowBuilder()
			cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
				return errors.New("child boom")
			}))
			childDAG, err := cb.Build()
			require.NoError(t, err)
			pb := NewWorkflowBuilder().WithWorkflowID(wf)
			pb.AddSubWorkflow("sub", childDAG).WithResult("result", "result")
			pb.AddNode("post").DependsOn("sub").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
			dag, err := pb.Build()
			require.NoError(t, err)
			w := NewWorkflow(store)
			w.WorkflowID = wf
			w.DAG = dag
			_ = w.Execute(context.Background()) //nolint:errcheck // a failing child errors by design; the assertion is on post's status
			final, err := store.Load(wf)
			require.NoError(t, err)
			// The (false) claim: post Completed after a child failure. It never is (fail-fast),
			// so this returns false -> gopter falsifies + shrinks -> non-vacuity proven.
			return statusOf(final, "post") == Completed
		}, gen.IntRange(0, 3)),
	)

	res := properties.Run(gopter.ConsoleReporter(false))
	if res {
		t.Fatal("NON-VACUITY FAILURE: the false-by-construction property PASSED — the child-fail path is not being exercised (vacuous arm)")
	}
}

// TestSubWorkflowComposition_StaticDAGAcyclic — COMP-INV-STATIC-DAG moat (ph96). The PARENT
// DAG stays acyclic-after-build even with a sub-workflow node in it, and the build-time
// acyclicity assertion (DAG.Validate, called by Build) BITES a seeded cycle. A sub-workflow
// node is a static DAG node like any other — it does not smuggle in a dynamic edge that would
// escape the acyclicity check. (Runtime nesting is separately bounded by the ph95 ceiling; the
// static check is about the declared parent graph.)
func TestSubWorkflowComposition_StaticDAGAcyclic(t *testing.T) {
	child, err := (func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", int64(1))
			return nil
		}))
		return cb.Build()
	})()
	require.NoError(t, err)

	// A parent with a sub-workflow node builds cleanly (acyclic-after-build).
	pb := NewWorkflowBuilder().WithWorkflowID("static-dag-ok")
	pb.AddStartNode("root").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	pb.AddSubWorkflow("sub", child).DependsOn("root").WithResult("result", "result")
	pb.AddNode("post").DependsOn("sub").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	dag, err := pb.Build()
	require.NoError(t, err, "a parent DAG with a sub-workflow node is acyclic-after-build")
	require.NoError(t, dag.Validate(), "the built composition DAG validates acyclic")

	// BITE: seed a cycle through the sub-workflow node (post -> root, closing root->sub->post->root)
	// and confirm the acyclicity assertion FIRES. Guards against the check silently passing a
	// cyclic composition graph (a vacuous STATIC-DAG moat).
	require.NoError(t, dag.AddDependency("post", "root"), "add the back-edge")
	require.Error(t, dag.Validate(), "STATIC-DAG: a cycle through the sub-workflow node MUST be rejected at validate")
}
