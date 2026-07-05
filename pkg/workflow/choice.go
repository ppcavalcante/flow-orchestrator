package workflow

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoBranchMatched is returned (wrapped with the choice node name) when a
// ChoiceNode evaluates every When predicate to false and no Otherwise default
// was declared (CHOICE-04). A routing dead-end is surfaced as a typed error, not
// a silent hang or a panic — callers can errors.Is(err, ErrNoBranchMatched).
var ErrNoBranchMatched = errors.New("no choice branch matched")

// choiceBranch is one ordered When arm: a predicate over WorkflowData plus the
// branch entry node it routes to when it is the FIRST arm to match.
type choiceBranch struct {
	predicate func(*WorkflowData) bool
	target    string
}

// choiceAction is the action of a ChoiceNode. When the node runs it evaluates
// the branches in DECLARED ORDER (first match wins — DEC-M11-FIRSTMATCH), marks
// every NOT-taken branch entry Bypassed (the cause-aware launch gate then
// propagates Bypassed through each not-taken subgraph, per T2), and Completes.
// A ChoiceNode makes a decision, so it is itself always Completed, never
// Bypassed (D38-01). No match with an Otherwise -> the default is taken; no
// match without an Otherwise -> ErrNoBranchMatched.
type choiceAction struct {
	nodeName      string
	branches      []choiceBranch
	defaultTarget string // meaningful only when hasDefault
	hasDefault    bool
}

// Execute implements Action. It is a pure routing decision — it never blocks,
// never parks, and (barring the no-match-no-default dead-end) always Completes.
func (a *choiceAction) Execute(_ context.Context, data *WorkflowData) error {
	for _, br := range a.branches {
		// A predicate is caller code returning a bool; a predicate that reads an
		// absent key returns false (the caller's data-precondition, D-09) and we
		// simply fall through to the next arm / default / error — never a panic.
		if br.predicate(data) {
			a.bypassExcept(data, br.target)
			return nil
		}
	}
	if a.hasDefault {
		a.bypassExcept(data, a.defaultTarget)
		return nil
	}
	// No arm matched and no Otherwise: this is a routing FAILURE, not a routing
	// decision. Surface the typed error and leave the branch entries untouched
	// (Pending) — the choice node becomes Failed (fail-fast, non-coe), and the
	// normal cause-aware cascade then classifies the downstream Skipped ("an
	// upstream you needed failed"), which is the honest cause. Force-marking the
	// subgraph Bypassed here would mislabel a failed run as a clean not-taken
	// branch (code-review 41-F1). (D-09 / CHOICE-04.)
	return fmt.Errorf("%w: choice %q had no matching branch and no Otherwise", ErrNoBranchMatched, a.nodeName)
}

// bypassExcept marks every branch entry (all When targets + the Otherwise
// default) Bypassed, except the taken one. taken=="" bypasses all. Duplicate
// targets and a target equal to taken are handled naturally (idempotent set;
// taken is never marked).
func (a *choiceAction) bypassExcept(data *WorkflowData, taken string) {
	seen := make(map[string]struct{}, len(a.branches)+1)
	mark := func(t string) {
		if t == "" || t == taken {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		data.SetNodeStatus(t, Bypassed)
	}
	for _, br := range a.branches {
		mark(br.target)
	}
	if a.hasDefault {
		mark(a.defaultTarget)
	}
}

// NewChoiceNode builds a declared ChoiceNode directly (the builder's AddChoice is
// the fluent front door; this is the underlying representation, symmetric with
// NewTimerNode). branches are evaluated in slice order (first match wins);
// defaultTarget=="" means no Otherwise (an unmatched choice returns
// ErrNoBranchMatched). The caller is responsible for wiring each target to depend
// on this node — AddChoice does that automatically.
func newChoiceNode(name string, branches []choiceBranch, defaultTarget string, hasDefault bool) *Node {
	return NewNode(name, &choiceAction{
		nodeName:      name,
		branches:      branches,
		defaultTarget: defaultTarget,
		hasDefault:    hasDefault,
	})
}

// choiceEdge records that a branch-entry node (target) must depend on its
// ChoiceNode (choice). Collected by When/Otherwise and folded into the target's
// dependencies in Build(), so branch wiring is independent of node-declaration
// order.
type choiceEdge struct {
	choice string
	target string
}

// choiceBuilder is the fluent builder for a ChoiceNode. It is deliberately
// distinct from NodeBuilder (D-05): a choice is configured by its routing arms
// (When/Otherwise) and its own upstream (DependsOn), not by WithAction /
// WithRetries / WithTimeout — its action IS the routing decision.
type choiceBuilder struct {
	wb     *WorkflowBuilder
	node   *NodeBuilder
	action *choiceAction
}

// AddChoice declares a ChoiceNode named name and returns a choiceBuilder to chain
// When(...) arms and an optional Otherwise(...) default. When reached, the node
// first-match-routes over WorkflowData, activating exactly one branch's entry and
// bypassing the rest:
//
//	wb.AddChoice("route").
//	    When(func(d *WorkflowData) bool { return d.GetInt("amt") > 1000 }, "big").
//	    When(func(d *WorkflowData) bool { return d.GetInt("amt") > 0 },    "small").
//	    Otherwise("zero")
//
// Wire the ChoiceNode's own upstream with DependsOn on the returned choiceBuilder.
func (b *WorkflowBuilder) AddChoice(name string) *choiceBuilder {
	action := &choiceAction{nodeName: name}
	nb := b.AddNode(name)
	nb.action = action
	return &choiceBuilder{wb: b, node: nb, action: action}
}

// When adds an ordered routing arm. When the ChoiceNode runs, the FIRST arm whose
// pred(data) returns true activates target's branch; every other branch is
// Bypassed (DEC-M11-FIRSTMATCH). target is wired to depend on the ChoiceNode.
func (c *choiceBuilder) When(pred func(*WorkflowData) bool, target string) *choiceBuilder {
	c.action.branches = append(c.action.branches, choiceBranch{predicate: pred, target: target})
	c.wb.choiceEdges = append(c.wb.choiceEdges, choiceEdge{choice: c.node.name, target: target})
	return c
}

// Otherwise sets the default branch, taken when no When predicate matches
// (CHOICE-04). Without an Otherwise, an unmatched choice returns
// ErrNoBranchMatched. A repeated Otherwise keeps the last default. target is
// wired to depend on the ChoiceNode.
func (c *choiceBuilder) Otherwise(target string) *choiceBuilder {
	c.action.defaultTarget = target
	c.action.hasDefault = true
	c.wb.choiceEdges = append(c.wb.choiceEdges, choiceEdge{choice: c.node.name, target: target})
	return c
}

// DependsOn wires the ChoiceNode's OWN upstream dependencies (the nodes that must
// complete before the routing decision is made). Returns the choiceBuilder for
// chaining.
func (c *choiceBuilder) DependsOn(deps ...string) *choiceBuilder {
	c.node.DependsOn(deps...)
	return c
}
