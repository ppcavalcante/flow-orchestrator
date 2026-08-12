package workflow

// Parked sub-workflow path correctness — F-PARK-03 (nil child) and F-PARK-04 (the
// suspendable-child error over-steer).
//
// F-PARK-03 — a nil parked sub-workflow child killed the host process.
//
// AddSubWorkflowParked did no nil validation, so Build accepted it silently. The nil
// stayed LATENT: childRunFailed only reads dag.Nodes inside its `case Failed` arm, so
// an all-success child run converged normally. The first child run that terminalized
// with any Failed node dereferenced the nil *DAG — on a level worker goroutine inside
// executeNodesInLevel, where a caller's recover() cannot reach it, so the whole host
// process died.
//
// There are TWO routes by which a nil reaches that field, and the builder guard only
// closes one:
//   1. AddSubWorkflowParked(name, nil)              — the reported defect, crash reproduced;
//   2. a registered DAGFactory returning (nil, nil) — the queue path builds the SAME
//      parkedSubWorkflowAction from the factory result (subworkflow_queue.go). Measured:
//      unguarded, this PARKS and carries the nil forward rather than crashing outright.
//
// Both are refused loudly now. These tests pin the guards and the latency property
// that made the bug so unkind.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParkedNilChild_BuildRefuses is the primary guard: the thing that used to build
// clean and kill the process later now fails at Build.
func TestParkedNilChild_BuildRefuses(t *testing.T) {
	b := NewWorkflowBuilder()
	b.AddStartNode("root").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	b.AddSubWorkflowParked("sub", nil).DependsOn("root")

	dag, err := b.Build()
	require.Error(t, err, "a nil parked child must be refused at Build, not accepted silently")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), "sub-workflow child DAG is nil",
		"must use the same wording the inline sibling already uses")
	assert.Nil(t, dag)
}

// TestParkedNonNilChild_BuildUnchanged is the other half of the bite: the guard must
// refuse ONLY nil. A guard that also rejects valid children would be a regression.
func TestParkedNonNilChild_BuildUnchanged(t *testing.T) {
	child := NewWorkflowBuilder()
	child.AddStartNode("c1").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	childDAG, err := child.Build()
	require.NoError(t, err)

	b := NewWorkflowBuilder()
	b.AddStartNode("root").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	b.AddSubWorkflowParked("sub", childDAG).DependsOn("root")

	dag, err := b.Build()
	require.NoError(t, err, "a valid parked child must still build")
	require.NotNil(t, dag)
}

// TestParkedNilChild_SuspendableChildStillAccepted pins that the parked guard is a NIL
// check only. Parked legitimately accepts a suspendable child — that is the whole
// difference from the inline path — so reusing scanChildInlineSafe here would have
// broken a supported pattern while fixing the crash.
func TestParkedNilChild_SuspendableChildStillAccepted(t *testing.T) {
	child := NewWorkflowBuilder()
	child.AddStartNode("c1").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	child.AddTimer("wait", 10).DependsOn("c1")
	childDAG, err := child.Build()
	require.NoError(t, err)

	b := NewWorkflowBuilder()
	b.AddStartNode("root").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	b.AddSubWorkflowParked("sub", childDAG).DependsOn("root")

	dag, err := b.Build()
	require.NoError(t, err, "parked must still accept a SUSPENDABLE child — only nil is refused")
	require.NotNil(t, dag)
}

// TestQueueSubWorkflow_NilFactoryDAG_LoudReject closes the SECOND route by which a nil
// child reaches parkedSubWorkflowAction: the queue path builds that action from the
// registered factory's result, so a factory returning (nil, nil) — an easy mistake,
// since forgetting to return an error looks harmless — stores the identical nil in the
// identical field. The builder guard cannot see this one.
//
// Measured, not assumed: WITHOUT this guard the first Execute does not crash, it PARKS
// ("workflow suspended") and carries the nil forward — the same latent-then-fatal shape
// as the builder route, where the deref would come on a later wake once the child has
// terminalized with a Failed node. This test proves the nil is refused before being
// stored; it does NOT drive the full enqueue → child-fails → wake cycle, so the panic
// via this route is reasoned from the shared code path, not reproduced here.
func TestQueueSubWorkflow_NilFactoryDAG_LoudReject(t *testing.T) {
	// A MULTI-PROCESS store: without it the enqueue rejects first and this test would
	// pass for the wrong reason, never exercising the factory boundary at all.
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "q.db"), WithMultiProcess())
	require.NoError(t, err)

	reg := NewRegistry()
	require.NoError(t, reg.Register("child", func() (*DAG, error) {
		return nil, nil // the mistake: a nil DAG with no error
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("q-nilfactory")
	pb.AddSubWorkflowQueued("sub", "child").WithResult("r", "r")
	dag, berr := pb.Build()
	require.NoError(t, berr)

	w := newWorkflowForTest(s)
	w.WorkflowID = "q-nilfactory"
	w.dag = dag
	w.registry = reg

	execErr := w.Execute(context.Background())
	require.Error(t, execErr, "a factory returning a nil DAG must be refused, not carried to the deref")
	assert.ErrorIs(t, execErr, ErrValidation)
	assert.Contains(t, execErr.Error(), "returned a nil DAG",
		"the error must name the factory as the cause, or the operator cannot find it")
}

// TestSuspendableChildError_NamesBothOnwardPaths pins F-PARK-04. The message used to say
// only "route it to the queue-dispatch path instead", steering every caller onto the
// heaviest option (*SQLiteStore + Pool + Registry) when parked would often serve them.
//
// The trap this test exists to catch is that the two "which is lighter" comparisons run in
// OPPOSITE directions: parked is lighter than queue, but HEAVIER than inline. So naming
// only parked would recreate the same over-steer with the sign flipped. The message must
// name both onward paths AND state what each requires.
func TestSuspendableChildError_NamesBothOnwardPaths(t *testing.T) {
	msg := ErrSubWorkflowSuspendableChild.Error()

	assert.Contains(t, msg, "AddSubWorkflowParked", "must name the parked path")
	assert.Contains(t, msg, "AddSubWorkflowQueued", "must name the queue path")

	// Naming a path without its cost is how a caller gets steered wrongly.
	assert.Contains(t, msg, "SignalStore", "must state what parked requires")
	assert.Contains(t, msg, "SQLiteStore", "must state what queue requires")

	// And it must still say WHY inline cannot serve, or the refusal is unexplained.
	assert.Contains(t, msg, "suspendable node")
	assert.Contains(t, msg, "inline cannot park")
}

// TestSuspendableChildError_StillFiresOnInlineChild keeps the wording test honest: the
// error must still actually be returned by the inline scan, not just be a nice string.
func TestSuspendableChildError_StillFiresOnInlineChild(t *testing.T) {
	child := NewWorkflowBuilder()
	child.AddStartNode("c1").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	child.AddTimer("wait", 10).DependsOn("c1")
	childDAG, err := child.Build()
	require.NoError(t, err)

	b := NewWorkflowBuilder()
	b.AddStartNode("root").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
	b.AddSubWorkflow("sub", childDAG).DependsOn("root")

	_, err = b.Build()
	require.Error(t, err, "an inline child with a suspendable node must still be refused")
	assert.ErrorIs(t, err, ErrSubWorkflowSuspendableChild)
}

// TestChildRunFailed_NilDagIsRefusedNotDerefed — REWRITTEN BY M23 SEAL-06, and the
// rewrite is the point rather than an accommodation.
//
// This test previously asserted that childRunFailed(nil, …) PANICS, and its comment said
// "the Build guard is the only thing standing between a caller and this deref". Both
// halves are now false: SEAL-06's verdict-side token check runs before any node lookup,
// so a nil (or merely unstamped) DAG returns a typed ErrDAGNotBuilt instead of dereferencing.
//
// KEPT RATHER THAN DELETED, because the hazard it documents is real and worth a standing
// guard — it just has a different answer now. The old failure mode was especially unkind:
// the panic fired on a LEVEL WORKER GOROUTINE, where a consumer cannot recover it and the
// process dies, and it was LATENT because a smoke test whose child never fails a node
// never reaches the lookup at all. The second subtest below pins exactly that latency, and
// is the reason the fix had to be a check at the top of the function rather than a nil
// guard on the lookup: only a check that runs unconditionally covers both arms.
//
// This is a case of a seal subsuming a pre-existing crash rather than merely adding a
// refusal, so it is recorded here where the next reader of the old assertion will find it.
func TestChildRunFailed_NilDagIsRefusedNotDerefed(t *testing.T) {
	t.Run("a Failed node no longer derefs the nil DAG", func(t *testing.T) {
		childData := NewWorkflowData("child")
		childData.SetNodeStatus("n1", Failed)

		var panicked any
		var err error
		func() {
			defer func() { panicked = recover() }()
			_, _, err = childRunFailed(nil, childData)
		}()
		require.Nil(t, panicked,
			"the SEAL-06 token check runs before the node lookup, so the deref is unreachable")
		require.ErrorIs(t, err, ErrDAGNotBuilt,
			"a nil verdict DAG must be REFUSED with a typed error, not crash a worker goroutine")
	})

	t.Run("an all-success child is refused too — the arm that used to be silent", func(t *testing.T) {
		childData := NewWorkflowData("child")
		childData.SetNodeStatus("n1", Completed)

		var panicked any
		var err error
		func() {
			defer func() { panicked = recover() }()
			_, _, err = childRunFailed(nil, childData)
		}()
		require.Nil(t, panicked)
		// THE ARM THAT MATTERS. Before SEAL-06 this path returned (false, "") without
		// touching the DAG at all — a happy child rendered a clean verdict from a graph
		// that did not exist, which is why the defect stayed latent through every smoke
		// test. It is now refused on the same branch as the failing case.
		require.ErrorIs(t, err, ErrDAGNotBuilt,
			"an all-success child must ALSO be refused: rendering a verdict from an absent or "+
				"unvalidated graph is not made safe by the child happening to have succeeded")
	})
}

// 117-F5 / 117-F6 — THE TWO TOKEN CHECKS THAT HAD NO BITE.
//
// Review round 3 seeded both and the FULL SUITE STAYED GREEN (ok 459.960s, exit 0):
// removing `!dag.built` from childRunFailed, and deleting the `!child.built` block from
// requireBuiltChild, cost nothing. Every existing test above passes only `nil`, so what
// was witnessed was NULLITY — while the comments and the test names claimed PROVENANCE.
// The nil arm is served by the `dag == nil` half of the same condition; the `built` half
// was never exercised at all.
//
// THE PRINCIPLE WAS ALREADY WRITTEN DOWN, ONE FILE OVER, AND NOT APPLIED HERE.
// dispatch_unvalidated_dag_test.go, in the same diff, says: "if the fixture were an empty
// &DAG{}, a passing test would be equally consistent with an engine that merely refuses
// zero-node graphs, which is NOT the property" — and uses a POPULATED unstamped graph for
// exactly that reason. Knowing the rule did not make it travel. Standing rule adopted:
// A GUARD'S BITE MUST BE COMMISSIONED PER GUARD, NOT PER MECHANISM. Three call sites
// share one token; sharing a mechanism is not sharing evidence.
//
// So both subtests below use a graph that is WELL-FORMED, NON-EMPTY and NEVER STAMPED —
// built through newDAG/addNode, never through Build(). Such a graph would execute
// perfectly well; it is refused solely because it lacks provenance, which is the property
// under test and the one nullity cannot stand in for.
func TestSealed_ProvenanceIsWitnessed_NotOnlyNullity(t *testing.T) {
	// unstamped returns a populated graph that never passed build(). Deliberately not a
	// helper shared with the nil tests: the whole finding is that one fixture was standing
	// in for two different properties.
	unstamped := func(t *testing.T) *DAG {
		t.Helper()
		d := newDAG("hand-rolled")
		require.NoError(t, d.addNode(newNode("work", ActionFunc(
			func(context.Context, *WorkflowData) error { return nil }))))
		require.False(t, d.built, "fixture must be UNSTAMPED, or this test proves nothing")
		require.NotEmpty(t, d.nodes, "fixture must be POPULATED, or it cannot tell provenance from emptiness")
		return d
	}

	// 117-F5 — childRunFailed's verdict-path check.
	//
	// BITE: removing `!dag.built` from the condition (leaving `dag == nil`) reds ONLY this
	// subtest — every nil test above stays green, which is precisely how the gap survived.
	t.Run("childRunFailed refuses a populated unstamped verdict DAG", func(t *testing.T) {
		childData := NewWorkflowData("child")
		childData.SetNodeStatus("work", Completed)

		_, _, err := childRunFailed(unstamped(t), childData)
		require.ErrorIs(t, err, ErrDAGNotBuilt,
			"an ALL-SUCCESS child is the arm that matters: an unvalidated graph here renders a "+
				"clean verdict from a graph the engine never inspected, which is this phase's own "+
				"defect shape on the verdict path")
	})

	// 117-F6 — requireBuiltChild's admission check, at BOTH call sites. Parked and inline
	// are separate subtests because they are separate call sites: R-04 guards two doors,
	// and a bite through one door says nothing about the other.
	for _, tc := range []struct {
		name string
		add  func(b *WorkflowBuilder, child *DAG)
	}{
		{"AddSubWorkflow", func(b *WorkflowBuilder, child *DAG) {
			b.AddSubWorkflow("sub", child).DependsOn("root")
		}},
		{"AddSubWorkflowParked", func(b *WorkflowBuilder, child *DAG) {
			b.AddSubWorkflowParked("sub", child).DependsOn("root")
		}},
	} {
		// BITE: deleting the `!child.built` block from requireBuiltChild reds both of these
		// and nothing else — the nil arm keeps its own half of the guard.
		t.Run(tc.name+" refuses a populated unstamped child", func(t *testing.T) {
			b := NewWorkflowBuilder()
			b.AddStartNode("root").WithAction(ActionFunc(
				func(context.Context, *WorkflowData) error { return nil }))
			tc.add(b, unstamped(t))

			dag, err := b.Build()
			require.ErrorIs(t, err, ErrDAGNotBuilt,
				"a caller-supplied child must prove PROVENANCE, not merely be non-nil — refused at "+
					"the PARENT's build(), which is the earliest point the parent can fail")
			require.Nil(t, dag)
		})
	}
}
