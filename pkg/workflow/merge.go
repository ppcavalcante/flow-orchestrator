package workflow

import (
	"context"
	"fmt"
)

// mergeAction is the action of a MergeNode — the OR-join below a ChoiceNode's
// branches. A MergeNode FIRES when >=1 of its taken branch-tails Completed (a
// Bypassed tail is satisfied, not blocking), and is itself Bypassed when every
// branch was bypassed (composes downward). That fire-vs-bypass decision is made
// on the launch-eligibility path in the executor (the mergeDependent role of the
// cause-aware gate), NOT here — this action runs only once the merge has already
// been decided to fire, and by default is a pass-through join (it simply
// Completes; downstream reads the taken branch's output from WorkflowData).
//
// The action doubles as the MergeNode marker: dependentRole type-asserts
// *mergeAction to select mergeDependent, so the OR-join role survives even when
// the caller supplies a user action (unlike ChoiceNode, whose action IS the
// routing decision). (M11 phase 42; D-P42-MERGE-ACTION.)
type mergeAction struct {
	nodeName string
	// tails are the From() branch-tail names — the OR-join predecessors. The
	// "taken" count for firing ranges over these, EXCLUDING the structural
	// always-Completed Choice-dep (anti-vacuity, DEC-M11-DEPMODEL). The source
	// Choice each tail reconverges from is derived by the reconvergence validator
	// (T2) — the field that records it lands with that logic, so it is not dead.
	tails []string
	// userAction, when non-nil, replaces the pass-through join.
	userAction Action
}

// Execute implements Action. Pass-through by default (the merge just Completes on
// fire); if a user action was supplied it runs that. The fire/bypass gate is in
// the executor, so reaching Execute means the merge fired.
func (a *mergeAction) Execute(ctx context.Context, data *WorkflowData) error {
	if a.userAction != nil {
		return a.userAction.Execute(ctx, data)
	}
	return nil
}

// newMergeNode builds a declared MergeNode directly (AddMerge is the fluent front
// door; this is the underlying representation, symmetric with newChoiceNode).
// tails are the branch-tail dependency names; the caller wires the DependsOn
// edges (AddMerge/From does that automatically).
func newMergeNode(name string, tails []string) *Node {
	return NewNode(name, &mergeAction{nodeName: name, tails: append([]string(nil), tails...)})
}

// mergeEdge records that a MergeNode (merge) must depend on a branch-tail (tail).
// Collected by From() and folded into the merge's dependencies in Build(), so the
// wiring is independent of node-declaration order (mirrors choiceEdge).
type mergeEdge struct {
	merge string
	tail  string
}

// mergeBuilder is the fluent builder for a MergeNode. Distinct from NodeBuilder
// (mirrors choiceBuilder): a merge is configured by its branch tails (From) and
// its optional join action (WithAction), and OR-joins them.
type mergeBuilder struct {
	wb     *WorkflowBuilder
	node   *NodeBuilder
	action *mergeAction
}

// AddMerge declares a MergeNode named name and returns a mergeBuilder to chain
// From(...) branch tails and an optional WithAction. When reached, the merge
// fires iff >=1 taken branch-tail Completed (a Bypassed tail is satisfied); if
// every branch was bypassed the merge is itself Bypassed:
//
//	wb.AddChoice("route").When(pred, "big").Otherwise("small")
//	// ... branch bodies ending at bigEnd / smallEnd ...
//	wb.AddMerge("done").From("bigEnd", "smallEnd")
//	wb.AddNode("after").DependsOn("done")
//
// Every From tail must be a node inside a branch of the SAME ChoiceNode; a tail
// that is a ChoiceNode itself (an empty/bodyless branch) is NOT supported and is
// rejected at Build (it carries no per-branch taken signal).
func (b *WorkflowBuilder) AddMerge(name string) *mergeBuilder {
	action := &mergeAction{nodeName: name}
	nb := b.AddNode(name)
	nb.action = action
	return &mergeBuilder{wb: b, node: nb, action: action}
}

// From declares the branch-tail predecessors the merge OR-joins. Each named tail
// is wired as a dependency of the merge; the merge fires on the taken tail(s).
// May be called more than once (tails accumulate).
func (m *mergeBuilder) From(tails ...string) *mergeBuilder {
	m.action.tails = append(m.action.tails, tails...)
	for _, t := range tails {
		m.wb.mergeEdges = append(m.wb.mergeEdges, mergeEdge{merge: m.node.name, tail: t})
	}
	return m
}

// WithAction sets an optional join action, replacing the default pass-through.
// Accepts an Action or a func(context.Context, *WorkflowData) error. The MergeNode
// marker (its *mergeAction) is preserved, so the OR-join role is unaffected.
func (m *mergeBuilder) WithAction(action interface{}) *mergeBuilder {
	switch a := action.(type) {
	case Action:
		m.action.userAction = a
	case func(ctx context.Context, data *WorkflowData) error:
		m.action.userAction = ActionFunc(a)
	default:
		// Defer the error to Build (mirrors NodeBuilder.WithAction): the merge
		// keeps its *mergeAction marker, but Build reports the invalid user action.
		m.node.actionErr = fmt.Errorf("unsupported merge action type: %T", action)
	}
	return m
}

// DependsOn wires additional upstream dependencies of the MergeNode beyond its
// branch tails. Rarely needed (the From tails carry the reconvergence), but kept
// for symmetry with choiceBuilder.
func (m *mergeBuilder) DependsOn(deps ...string) *mergeBuilder {
	m.node.DependsOn(deps...)
	return m
}
