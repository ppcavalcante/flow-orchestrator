package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-068: WithDefinitionBudget bounds a definition's size at Build. Each axis is
// an independent, opt-in cap; the zero value imposes no limit.

// budgetNoop is a trivially-succeeding action for budget-shape tests.
func budgetNoop() ActionFunc {
	return func(_ context.Context, _ *WorkflowData) error { return nil }
}

// chainBuilder builds a linear chain n0->n1->...->n(count-1): count nodes,
// count-1 edges, every level width 1.
func chainBuilder(count int) *WorkflowBuilder {
	b := NewWorkflowBuilder().WithWorkflowID("budget-chain")
	prev := ""
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("n%d", i)
		var nb *NodeBuilder
		if i == 0 {
			nb = b.AddStartNode(name)
		} else {
			nb = b.AddNode(name).DependsOn(prev)
		}
		nb.WithActionFunc(budgetNoop())
		prev = name
	}
	return b
}

// fanBuilder builds one start node with `width` independent children: width+1
// nodes, width edges, and a level of static width `width`.
func fanBuilder(width int) *WorkflowBuilder {
	b := NewWorkflowBuilder().WithWorkflowID("budget-fan")
	b.AddStartNode("root").WithActionFunc(budgetNoop())
	for i := 0; i < width; i++ {
		b.AddNode(fmt.Sprintf("c%d", i)).WithActionFunc(budgetNoop()).DependsOn("root")
	}
	return b
}

func TestDefinitionBudget_ZeroValueImposesNoLimit(t *testing.T) {
	// A large-ish graph with a zero-value budget builds fine (opt-in, backward-compatible).
	_, err := chainBuilder(50).WithDefinitionBudget(DefinitionBudget{}).Build()
	require.NoError(t, err)

	// And a builder that never calls WithDefinitionBudget behaves identically.
	_, err = fanBuilder(50).Build()
	require.NoError(t, err)
}

func TestDefinitionBudget_MaxNodesBites(t *testing.T) {
	// 10 nodes, budget 5 → rejected; the message names the count and the ceiling.
	_, err := chainBuilder(10).WithDefinitionBudget(DefinitionBudget{MaxNodes: 5}).Build()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "10 nodes")
	require.Contains(t, err.Error(), "5-node budget")

	// Exactly at the cap is allowed (>, not >=).
	_, err = chainBuilder(5).WithDefinitionBudget(DefinitionBudget{MaxNodes: 5}).Build()
	require.NoError(t, err, "a graph exactly at the node cap is allowed")
}

func TestDefinitionBudget_MaxEdgesBites(t *testing.T) {
	// A 6-node chain has 5 edges; budget 3 → rejected.
	_, err := chainBuilder(6).WithDefinitionBudget(DefinitionBudget{MaxEdges: 3}).Build()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "5 dependency edges")
	require.Contains(t, err.Error(), "3-edge budget")

	// At the cap is allowed.
	_, err = chainBuilder(4).WithDefinitionBudget(DefinitionBudget{MaxEdges: 3}).Build()
	require.NoError(t, err, "a graph exactly at the edge cap is allowed")
}

func TestDefinitionBudget_MaxWidthBites(t *testing.T) {
	// root + 8 independent children → a level of static width 8; budget 4 → rejected.
	_, err := fanBuilder(8).WithDefinitionBudget(DefinitionBudget{MaxWidth: 4}).Build()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "static width 8")
	require.Contains(t, err.Error(), "4-width budget")

	// A chain (width 1 everywhere) passes even a tight width budget.
	_, err = chainBuilder(20).WithDefinitionBudget(DefinitionBudget{MaxWidth: 1}).Build()
	require.NoError(t, err, "a linear chain never exceeds width 1")
}

func TestDefinitionBudget_FirstFailingAxisReported(t *testing.T) {
	// A graph over MULTIPLE caps: the node check runs first and is the one reported,
	// so the error is deterministic regardless of the other overages.
	_, err := fanBuilder(8). // 9 nodes, 8 edges, width 8
					WithDefinitionBudget(DefinitionBudget{MaxNodes: 2, MaxEdges: 2, MaxWidth: 2}).
					Build()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "9 nodes", "the node axis is checked first")
}

func TestDefinitionBudget_UnitValidator(t *testing.T) {
	// The pure validator, exercised directly for the boundary cases.
	require.NoError(t, validateDefinitionBudget(3, 2, nil, DefinitionBudget{}))
	require.NoError(t, validateDefinitionBudget(3, 2, nil, DefinitionBudget{MaxNodes: 3}))
	require.Error(t, validateDefinitionBudget(4, 2, nil, DefinitionBudget{MaxNodes: 3}))

	// A negative cap is treated as "no limit", same as zero.
	require.NoError(t, validateDefinitionBudget(1000, 1000, nil, DefinitionBudget{MaxNodes: -1, MaxEdges: -1, MaxWidth: -1}))

	require.True(t, errors.Is(validateDefinitionBudget(4, 2, nil, DefinitionBudget{MaxNodes: 3}), ErrValidation))
}
