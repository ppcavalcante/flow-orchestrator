package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// THE INPUT AXIS, AND THE CUSTOMER-VISIBLE CONSEQUENCE.
//
// checkDeepEqualPairDepth has exactly two call sites (fanout.go:539,
// subworkflow.go:405) and at both it sits IMMEDIATELY BEFORE the collision
// check:
//
//	if derr := checkDeepEqualPairDepth(existing, results[i], ...); derr != nil {
//	        return derr                                   // <- ErrValidation
//	}
//	if !reflect.DeepEqual(existing, results[i]) {
//	        return ...ErrFanOutResultKeyCollision         // <- the real answer
//	}
//
// So a FALSE REFUSAL is not merely conservative. It PRE-EMPTS the collision
// check and substitutes a different sentinel error, on a pair reflect.DeepEqual
// would have resolved to `false` in nanoseconds. A consumer branching on
// errors.Is(err, ErrFanOutResultKeyCollision) stops seeing the collision.
// ============================================================================

// advCyclicBranchAction sets a CYCLIC branch result under the branch result key.
func advCyclicBranchAction(period int) Action {
	return ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("out", mkAdvTagged(period, 7)) // cyclic, type *advTagged
		return nil
	})
}

// TestADV_CallSite_FalseRefusalMasksTheCollisionError drives the REAL fan-out
// path. The pre-existing indexed value and the branch result are BOTH cyclic
// and of DIFFERENT TYPES — which reflect.DeepEqual settles at its first line
// (`if v1.Type() != v2.Type() { return false }`), with no recursion at all.
//
// The documented contract for that situation is a loud collision.
func TestADV_CallSite_FalseRefusalMasksTheCollisionError(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("wf-adv-mask")
	b.AddStartNode("seed").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		// A FOREIGN pre-existing value: cyclic, and a different type from what
		// the branch produces. A genuine collision by every rule the caller has.
		d.Set(fanOutResultIndexKey("results", 1), mkAdvCyc(5))
		return nil
	}))
	b.AddFanOut("fan", intItemsExpander(3), advCyclicBranchAction(9)).
		WithResults("results", "out").DependsOn("seed")
	dag, err := b.Build()
	require.NoError(t, err)

	w := newWorkflowForTest(NewInMemoryStore())
	w.WorkflowID = "wf-adv-mask"
	w.dag = dag

	execErr := w.Execute(context.Background())
	t.Logf("MEASURED at the real call site: %v", execErr)
	require.Error(t, execErr, "precondition: a foreign pre-existing indexed value must be refused somehow")

	if errors.Is(execErr, ErrValidation) && !errors.Is(execErr, ErrFanOutResultKeyCollision) {
		t.Errorf("ERROR-TYPE SUBSTITUTION AT THE CALL SITE.\n"+
			"  expected: ErrFanOutResultKeyCollision (the pre-existing value is foreign — different "+
			"type — and reflect.DeepEqual settles that at its FIRST LINE, before hard(), before the "+
			"visited map, before any recursion)\n"+
			"  got:      ErrValidation, from checkDeepEqualPairDepth's both-cyclic branch, which "+
			"refused the pair for a stack-depth risk that cannot exist on a type mismatch\n"+
			"  customer effect: code branching on errors.Is(err, ErrFanOutResultKeyCollision) no "+
			"longer sees the collision, and is told the value is too deep instead.\n"+
			"  got err: %v", execErr)
	}
}
