package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// WHICH of the two token checks refuses a queue-dispatched unstamped child.
//
// (*DAG).Execute's comment claims the queue path is mediated NOT by that check but by
// (*Workflow).executeLocked, because runNext builds &Workflow{dag: …} around the consumer
// factory's graph and calls w.Execute, so executeLocked returns before (*DAG).Execute is
// entered. That claim was measured but its evidence lived outside the tree. This is the
// evidence, in the tree.
//
// IN-PACKAGE ON PURPOSE, and the contrast with the R-01 sweep is the point. That sweep
// lives in package workflow_test because zero-form constructibility is a genuine BOUNDARY
// property — it cannot be exhibited from inside at all. THIS claim is not a boundary
// property: it is "which of two internal checks fires first", which in-package discharges
// fully. Reusing the external rationale here would carry a locally true justification past
// its warrant. In-package is also STRONGER here, because it can give the child graph a
// name (newDAG) — and the name is what makes the assertion below discriminating.
//
// # The obvious assertion is booby-trapped
//
// Both sites return the SAME sentinel, whose text opens:
//
//		DAG was not produced by WorkflowBuilder.Build (M23 SEAL-06: …): <subject>
//
//	  - ErrorIs(ErrDAGNotBuilt) cannot discriminate — TestDispatch_UnvalidatedDAGFromFactoryIsRefused
//	    asserts exactly that and explicitly disclaims the discrimination.
//	  - NotContains(err, "DAG") REDS ON A CORRECT REFUSAL: the sentinel prefix contains "DAG"
//	    whichever check fired. A test written that way fails against working code, gets
//	    "fixed" by weakening, and lands vacuous.
//	  - Contains(err, "workflow") is near-vacuous — the prefix already carries
//	    "WorkflowBuilder".
//
// So this discriminates on IDENTITY, not vocabulary. The child graph and the workflow get
// distinctive, unrelated names; the assertion is which object's identity got interpolated
// into the message. dag.go renders `DAG %q` (the graph's name), workflow.go renders
// `workflow %q` (the WorkflowID). That is immune to the sentinel prefix and survives
// anyone rewording the sentinel later.
func TestDispatch_QueueChildIsRefusedByExecuteLocked_NotByDAGExecute(t *testing.T) {
	const (
		dagName    = "child-graph-Q2"
		parentID   = "wf-parent-Q2"
		parentNode = "spawn-child"
	)

	// A GENUINE queue child, not a plain dispatch item. The earlier version of this test
	// enqueued via store.Enqueue — no parent_id, no parent_signal, no depth — and an item
	// with no parent is not a child, so the name asserted more than the fixture exercised.
	// Both entries do converge on the same &Workflow{…} construction in runNext, so the
	// evidence transferred; it transferred by an argument rather than by the fixture, and
	// the fixture is cheap to make honest.
	childID := SubWorkflowChildID(parentID, parentNode)

	store := mkDispatchStore(t)

	reg := NewRegistry()
	require.NoError(t, reg.Register("unstampedQ2", func() (*DAG, error) {
		// Well-formed, non-empty, NAMED — and never stamped, because it did not come
		// through WorkflowBuilder.build. The shape a consumer DAGFactory can still return.
		d := newDAG(dagName)
		require.NoError(t, d.addNode(newNode("work", ActionFunc(
			func(context.Context, *WorkflowData) error { return nil }))))
		return d, nil
	}))

	// The engine-set control columns are what make this a CHILD: parent address + depth,
	// never the input BLOB (DEC-P94-PARENT-ADDRESS-COLUMN).
	_, err := store.EnqueueSubWorkflow(childID, "unstampedQ2", nil,
		parentID, completionSignalName(parentNode), 1)
	require.NoError(t, err)

	ran, runErr := RunNext(context.Background(), store, reg, "owner-Q2")
	require.True(t, ran, "the worker must have claimed and handled the item, or the refusal below proves nothing")
	require.Error(t, runErr)
	require.ErrorIs(t, runErr, ErrDAGNotBuilt, "an unstamped factory graph must be refused by the builder token")

	msg := runErr.Error()

	// THE DISCRIMINATION. executeLocked interpolates the WorkflowID — here the child ID;
	// (*DAG).Execute interpolates the graph's name. Exactly one of the two appears.
	require.Contains(t, msg, childID,
		"expected executeLocked to be the refusing site — it names the WORKFLOW (the child ID)")
	require.NotContains(t, msg, dagName,
		"(*DAG).Execute must NOT be the refusing site: it names the GRAPH, so seeing %q here "+
			"means executeLocked stopped firing first and the queue path's mediator moved", dagName)

	// Guard the oracle: the two identities must actually be distinguishable, or both
	// assertions above could hold while discriminating nothing. TRUE BY CONSTRUCTION as
	// written — SubWorkflowChildID returns "sub:" + 64 hex chars, and dagName carries '-'
	// and letters outside [0-9a-f] — so this cannot currently fail. It is kept, not deleted,
	// because it constrains a FUTURE edit to the one identity that is editable: rename
	// dagName to something the child ID contains and this reds with a clear cause, instead
	// of a confusing NotContains failure. (childID is derived, so pointing it at dagName
	// would need a sha256 preimage over a non-hex string — not an edit anyone can make.)
	require.False(t, strings.Contains(dagName, childID) || strings.Contains(childID, dagName),
		"the two identities must be unrelated substrings for this test to discriminate")
}

// POSITIVE CONTROL — without it every assertion above is satisfied by an engine that
// refuses everything on this path. Same store, same registry shape, same drive: a BUILT
// graph must run to completion and terminalize as done.
//
// This is the sibling discipline from TestDispatch_UnvalidatedDAGFromFactoryIsRefused,
// which records that its own controls are "not decoration".
func TestDispatch_BuiltGraphOnTheSamePathStillRuns(t *testing.T) {
	store := mkDispatchStore(t)

	ran := false
	reg := NewRegistry()
	require.NoError(t, reg.Register("builtQ2", func() (*DAG, error) {
		b := NewWorkflowBuilder().WithWorkflowID("built-Q2")
		b.AddStartNode("work").WithAction(ActionFunc(
			func(context.Context, *WorkflowData) error {
				ran = true
				return nil
			}))
		return b.Build()
	}))

	_, err := store.Enqueue("wf-built-Q2", "builtQ2", nil)
	require.NoError(t, err)

	claimed, runErr := RunNext(context.Background(), store, reg, "owner-built-Q2")
	require.NoError(t, runErr, "a BUILT graph must not be refused on the dispatch path")
	require.True(t, claimed)
	require.True(t, ran, "the node must actually have executed, or this control proves nothing")
}
