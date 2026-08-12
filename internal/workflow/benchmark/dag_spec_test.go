package benchmark

import (
	"fmt"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// dagSpec is the migration adapter for M23 SEAL-06, and it exists to make the
// benchmark helpers' move to the builder a MECHANICAL substitution rather than a
// re-derivation.
//
// The problem it solves. dag.AddDependency(from, to) means "to depends on from" — it
// is written PARENT-FIRST. The builder is CHILD-FIRST: AddNode(child).DependsOn(parent).
// Translating a helper by hand therefore means inverting every loop, and the helpers in
// this package are exactly the wrong shape for that:
//
//   - createLinearDAGForBenchmark calls mustAddDep(dag, toName, fromName) — arguments
//     already swapped relative to its neighbours, so "linear-benchmark" is really a
//     REVERSED chain (node0 depends on node1 depends on node2 …).
//   - createDiamondDAGForBenchmark calls mustAddDep(dag, node_i, "node0"), which makes
//     node0 depend on every node of the first half — a fan-IN to node0, though the name
//     says diamond.
//   - createComplexWorkflow and createComplexDAGForBenchmark pick dependencies with
//     rand, so a direction error there produces a DIFFERENT RANDOM GRAPH rather than an
//     obviously wrong one, and no reviewer could spot it by reading.
//
// Those shapes may well be accidents, but fixing them here would be a silent
// MEASUREMENT change smuggled inside a compile fix, and this package's baseline is
// already void for other reasons (F117-T3-01). So the rule is: preserve the edge set
// EXACTLY, oddities included, and let anything that looks wrong be raised as its own
// finding rather than quietly corrected.
//
// dagSpec accepts edges in AddDependency's original (from, to) order and does the one
// inversion itself, once, in a place with a test. Every migrated call site is then a
// like-for-like textual swap with no thinking required at the site — which is the only
// way to move ~8 helpers without introducing the very error this comment describes.
//
// It lives in a _test.go file on purpose: nothing in the production build should be
// able to reach a DAG-assembly shim.
type dagSpec struct {
	name    string
	order   []string // insertion order, so the built graph is deterministic
	actions map[string]workflow.Action
	parents map[string][]string // child -> parents, the builder's direction
}

func newDAGSpec(name string) *dagSpec {
	return &dagSpec{
		name:    name,
		actions: make(map[string]workflow.Action),
		parents: make(map[string][]string),
	}
}

// node declares a node. It replaces NewNode + AddNode at the call sites.
func (s *dagSpec) node(name string, action workflow.Action) *dagSpec {
	if _, dup := s.actions[name]; !dup {
		s.order = append(s.order, name)
	}
	s.actions[name] = action
	return s
}

// dep records an edge with EXACTLY dag.AddDependency's meaning: to depends on from.
// The inversion to the builder's child-first form happens here and only here.
func (s *dagSpec) dep(from, to string) *dagSpec {
	s.parents[to] = append(s.parents[to], from)
	return s
}

// build assembles the graph through the sanctioned builder API. It panics on error,
// matching the mustAddNode/mustAddDep helpers it replaces — a benchmark whose graph
// cannot be built is broken, and these call sites have no *testing.B in scope.
func (s *dagSpec) build() *workflow.DAG {
	b := workflow.NewWorkflowBuilder().WithWorkflowID(s.name)
	for _, name := range s.order {
		nb := b.AddNode(name).WithAction(s.actions[name])
		if ps := s.parents[name]; len(ps) > 0 {
			nb.DependsOn(ps...)
		}
	}
	dag, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("benchmark setup: build %s: %v", s.name, err))
	}
	return dag
}

// buildWithNodes builds and also returns the nodes in declaration order, so a helper
// that used to hand back the *Node values it minted keeps its exact signature and its
// call sites need no edit.
//
// The nodes come from the BUILT graph via GetNode rather than from anything this spec
// held: after SEAL-01 a *Node is an opaque handle, and the only legitimate way to hold
// one is to ask the graph that owns it. Every current caller uses them for Name() only.
func (s *dagSpec) buildWithNodes() (*workflow.DAG, []*workflow.Node) {
	dag := s.build()
	nodes := make([]*workflow.Node, 0, len(s.order))
	for _, name := range s.order {
		n, ok := dag.GetNode(name)
		if !ok {
			panic(fmt.Sprintf("benchmark setup: %s: built graph is missing node %s", s.name, name))
		}
		nodes = append(nodes, n)
	}
	return dag, nodes
}
