package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// blockedFixture builds a dependent node plus a WorkflowData in which each named
// dependency carries the given status. coe names the deps that are
// continue-on-error (so a Failed status on them resolves rather than skip-causes).
// It is the direct harness for classifyBlockedStatus — the cause-aware selector
// both the launch gate and markSkippedFrom consult (single source of truth).
func blockedFixture(deps map[string]NodeStatus, coe map[string]bool) (*Node, *WorkflowData) {
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })
	node := NewNode("dependent", noop)
	data := NewWorkflowData("wf")
	// Deterministic dep order is irrelevant to classifyBlockedStatus (it is a
	// full scan with no early break) — that order-independence is the whole point
	// of the redesign, so we do not sort here.
	for name, st := range deps {
		dep := NewNode(name, noop)
		dep.ContinueOnError = coe[name]
		node.AddDependency(dep)
		data.SetNodeStatus(name, st)
	}
	return node, data
}

// TestCauseAware_BypassVsFailure is the core cause-discrimination (MH-2,
// DEC-M11-STATUS-CAUSE): the assigned terminal status depends on WHY the node is
// blocked. Bypass-cause -> Bypassed; failure/skip-cause -> Skipped.
func TestCauseAware_BypassVsFailure(t *testing.T) {
	// Blocked only by a Bypassed dep -> Bypassed (a purely not-taken interior).
	node, data := blockedFixture(map[string]NodeStatus{"b": Bypassed}, nil)
	st, assign := classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Bypassed, st, "blocked-only-by-Bypassed -> Bypassed")

	// Blocked by a non-coe Failed dep -> Skipped (a failure cascade).
	node, data = blockedFixture(map[string]NodeStatus{"f": Failed}, nil)
	st, assign = classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Skipped, st, "blocked-by-Failed -> Skipped")

	// Blocked by an already-Skipped dep -> Skipped (transitive skip).
	node, data = blockedFixture(map[string]NodeStatus{"s": Skipped}, nil)
	st, assign = classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Skipped, st, "blocked-by-Skipped -> Skipped")
}

// TestCauseAware_Diamond is the load-bearing D-03 / DEC-M11-P41-DIAMOND bite: a
// non-merge node with BOTH a taken Completed dep AND a Bypassed dep is Skipped
// (the surviving taken-path ancestor wins), NOT Bypassed.
//
// BITE: change classifyBlockedStatus rule 3 from `return Skipped, true` to
// `return Bypassed, true` (or delete rule 3 so rule 4 fires) -> this test FAILS
// (got Bypassed, want Skipped). Verified to falsify (T2 mutation log).
func TestCauseAware_Diamond(t *testing.T) {
	node, data := blockedFixture(map[string]NodeStatus{"taken": Completed, "notTaken": Bypassed}, nil)
	st, assign := classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Skipped, st, "{Completed, Bypassed} on a non-merge node -> Skipped (taken ancestor wins)")
}

// TestCauseAware_CoeFailedDiamond guards DEC-M11-P41-DIAMOND's generalization: a
// continue-on-error Failed dep is a RESOLVED taken ancestor (depResolved unifies
// it with Completed), so {coe-Failed, Bypassed} is also Skipped, not Bypassed.
// This is why rule 3 keys on sawResolved (Completed OR coe-Failed), not Completed.
//
// BITE: narrow depResolved so coe-Failed no longer resolves, OR make rule 3 key
// on Completed-only -> this becomes Bypassed and the test FAILS.
func TestCauseAware_CoeFailedDiamond(t *testing.T) {
	node, data := blockedFixture(
		map[string]NodeStatus{"tolerated": Failed, "notTaken": Bypassed},
		map[string]bool{"tolerated": true},
	)
	st, assign := classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Skipped, st, "{coe-Failed, Bypassed} -> Skipped (a coe-Failed path is a taken ancestor)")
}

// TestCauseAware_WaitingSibling is the D-03a noninterference guard: a Waiting
// (suspended) sibling must NOT be read as a terminal cause. A node with a
// {Waiting, Bypassed} dep set is NOT decidable this pass — classifyBlockedStatus
// returns assign=false, leaving the node Pending for a later pass (once the
// Waiting sibling wakes to a terminal status). Misreading Waiting as terminal
// would corrupt the M10 suspend semantics.
//
// BITE: move the Waiting bucket out of sawNonTerminal (e.g. fold Waiting into
// the Bypassed/skip arms) -> assign becomes true here and the test FAILS.
func TestCauseAware_WaitingSibling(t *testing.T) {
	node, data := blockedFixture(map[string]NodeStatus{"parked": Waiting, "notTaken": Bypassed}, nil)
	_, assign := classifyBlockedStatus(node, data, andDependent)
	assert.False(t, assign, "{Waiting, Bypassed} is not decidable this pass — node stays Pending (D-03a)")

	// A failure still dominates a Waiting sibling (rule 1 before rule 2): a run
	// that has both a parked node and a hard-failed dep of THIS node settles this
	// node Skipped — the failure cascade does not wait on the park.
	node, data = blockedFixture(map[string]NodeStatus{"parked": Waiting, "boom": Failed}, nil)
	st, assign := classifyBlockedStatus(node, data, andDependent)
	assert.True(t, assign)
	assert.Equal(t, Skipped, st, "a hard-failure dominates a Waiting sibling")
}

// TestCauseAware_DependentRole pins the role dispatch (M11 ph42): a MergeNode is
// mergeDependent (its *mergeAction marker), every other node is andDependent. The
// merge arm of depResolved treats a Bypassed predecessor as satisfied (OR-join).
func TestCauseAware_DependentRole(t *testing.T) {
	plain := NewNode("any", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	assert.Equal(t, andDependent, dependentRole(plain), "a plain node is an AND-dependent")

	merge := newMergeNode("m", []string{"a"})
	assert.Equal(t, mergeDependent, dependentRole(merge), "a MergeNode is a mergeDependent (OR-join)")

	// The merge arm treats Bypassed as resolving; the AND arm does not.
	dep := NewNode("dep", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	assert.True(t, depResolved(dep, Bypassed, mergeDependent), "merge arm: Bypassed resolves an OR-join")
	assert.False(t, depResolved(dep, Bypassed, andDependent), "AND arm: Bypassed does NOT resolve")
}
