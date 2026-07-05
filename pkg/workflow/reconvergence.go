package workflow

import (
	"errors"
	"fmt"
)

// Reconvergence validator (M11 phase 42, D-P42-STRICT). Only STRUCTURED,
// single-Choice, local OR-joins are expressible: a MergeNode reconverges the
// branches of exactly one ChoiceNode. Every unstructured shape is a typed build
// error, not a runtime surprise — and this strictness is load-bearing for the
// runtime semantics (T1b relies on "a Choice takes exactly one branch, no
// cross-Choice merges" so a merge's Failed predecessor is always the sole taken
// branch failing).
var (
	// ErrSharedBranch: a single node is a branch target of two different
	// ChoiceNodes (the 41-F2 shape) — ownership is ambiguous.
	ErrSharedBranch = errors.New("branch entry owned by more than one ChoiceNode")
	// ErrUnstructuredMerge: a non-MergeNode reconverges branches (an implicit
	// OR-join — only a MergeNode may sit at a reconvergence point), or a merge
	// joins tails from more than one ChoiceNode (a cross-Choice merge).
	ErrUnstructuredMerge = errors.New("unstructured reconvergence")
	// ErrDanglingMerge: a MergeNode joins a tail that is under no ChoiceNode.
	ErrDanglingMerge = errors.New("merge joins a tail under no ChoiceNode")
)

// isChoiceNode identifies a ChoiceNode by its action marker (a merge is matched
// inline with the comma-ok form where its *mergeAction is also needed).
func isChoiceNode(n *Node) bool { _, ok := n.Action.(*choiceAction); return ok }

// dependsOn reports whether n already lists a dependency named depName.
func dependsOn(n *Node, depName string) bool {
	for _, d := range n.DependsOn {
		if d.Name == depName {
			return true
		}
	}
	return false
}

// nearestBranch finds the nearest branch the node sits inside: it walks UP the
// dependency graph and returns the first branch ENTRY encountered (a node that
// depends directly on a ChoiceNode) together with that ChoiceNode. For a node
// interior to choice C's branch this yields (C, entry); for a nested branch it
// yields the INNERMOST choice (nearest by hop count). ok=false means the node is
// under no ChoiceNode (a ChoiceNode itself is not "under a branch" — empty-branch
// merge tails are rejected by the validator, so this case is not load-bearing).
func nearestBranch(start *Node) (choice *Node, entry *Node, ok bool) {
	visited := make(map[string]bool)
	queue := []*Node{start}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if visited[n.Name] {
			continue
		}
		visited[n.Name] = true
		// Is n a branch entry? (depends directly on a ChoiceNode.)
		for _, d := range n.DependsOn {
			if isChoiceNode(d) {
				return d, n, true
			}
		}
		// Not an entry — climb.
		queue = append(queue, n.DependsOn...)
	}
	return nil, nil, false
}

// validateReconvergence enforces D-P42-STRICT over a built DAG. Returns a typed
// error (wrapped with the offending node name) for the three unstructured shapes;
// nil for a well-formed structured choice-merge DAG.
func validateReconvergence(dag *DAG) error {
	// (c) Shared branch entry: a node owned by >1 ChoiceNode (41-F2).
	for _, n := range dag.Nodes {
		choices := make(map[string]bool)
		for _, d := range n.DependsOn {
			if isChoiceNode(d) {
				choices[d.Name] = true
			}
		}
		if len(choices) > 1 {
			return fmt.Errorf("%w: node %q is a branch of %d ChoiceNodes", ErrSharedBranch, n.Name, len(choices))
		}
	}

	// depmodelEdge records a merge->source-Choice edge to add AFTER the read pass.
	// The edge is applied post-validation, never mid-loop: mutating DependsOn while
	// nearestBranch reads it would make the verdict depend on Nodes map-iteration
	// order (code-review 42-F1 BLOCKER — reproduced 175/25 of 200 builds).
	type depmodelEdge struct{ merge, choice *Node }
	var pending []depmodelEdge

	for _, n := range dag.Nodes {
		if isChoiceNode(n) {
			continue // a Choice's own upstream is not a reconvergence
		}
		// Compute the owning (choice, entry) of each dependency.
		type owner struct{ choice, entry string }
		owners := make([]owner, 0, len(n.DependsOn))
		byChoice := make(map[string]map[string]bool) // choice -> set of distinct entries
		choiceNodes := make(map[string]*Node)        // choice name -> its *Node
		for _, d := range n.DependsOn {
			c, e, ok := nearestBranch(d)
			if !ok {
				owners = append(owners, owner{"", ""})
				continue
			}
			owners = append(owners, owner{c.Name, e.Name})
			choiceNodes[c.Name] = c
			if byChoice[c.Name] == nil {
				byChoice[c.Name] = make(map[string]bool)
			}
			byChoice[c.Name][e.Name] = true
		}

		if ma, isMerge := n.Action.(*mergeAction); isMerge {
			// The tail-shape checks apply ONLY to the recorded From tails — an
			// explicit mergeBuilder.DependsOn is an ordering constraint, not an
			// OR-join tail, so it does not participate in reconvergence validation
			// (and the fire gate likewise counts only over the tails).
			tailSet := make(map[string]bool, len(ma.tails))
			for _, t := range ma.tails {
				tailSet[t] = true
			}
			tailChoices := make(map[string]*Node)
			for i, d := range n.DependsOn {
				if !tailSet[d.Name] {
					continue // an explicit DependsOn, not a From join tail
				}
				// Empty-branch tails (From(<ChoiceNode>)) are NOT supported: a Choice
				// is always Completed, so it carries no per-branch "was this empty
				// branch taken?" signal — a merge could not tell a taken empty branch
				// from a bypassed one (code-review 42-F2 / adversarial 42-AF1). Reject
				// explicitly rather than silently mis-fire.
				if isChoiceNode(d) {
					return fmt.Errorf("%w: merge %q joins a ChoiceNode tail %q (empty-branch merge tails are not supported)",
						ErrUnstructuredMerge, n.Name, d.Name)
				}
				// (b) A merge's tails must all trace to a ChoiceNode, none dangling.
				if owners[i].choice == "" {
					return fmt.Errorf("%w: merge %q joins tail %q", ErrDanglingMerge, n.Name, d.Name)
				}
				tailChoices[owners[i].choice] = choiceNodes[owners[i].choice]
			}
			// (b) ... and to EXACTLY ONE ChoiceNode (no cross-Choice merge).
			if len(tailChoices) > 1 {
				return fmt.Errorf("%w: merge %q joins tails from %d different ChoiceNodes", ErrUnstructuredMerge, n.Name, len(tailChoices))
			}
			// DEC-M11-DEPMODEL: the merge depends on the reconvergence-source Choice
			// (not just its tails) — queued, applied after the read pass.
			for _, choiceNode := range tailChoices { // exactly one
				if !dependsOn(n, choiceNode.Name) {
					pending = append(pending, depmodelEdge{merge: n, choice: choiceNode})
				}
			}
			continue
		}

		// (a) A non-merge node must NOT reconverge two DIFFERENT branches of the
		// same ChoiceNode (that is an implicit OR-join — it must be a MergeNode).
		for choiceName, entries := range byChoice {
			if len(entries) > 1 {
				return fmt.Errorf("%w: node %q reconverges %d branches of ChoiceNode %q (use AddMerge)",
					ErrUnstructuredMerge, n.Name, len(entries), choiceName)
			}
		}
	}

	// Apply the DEC-M11-DEPMODEL edges now that all reads are done — deterministic.
	for _, e := range pending {
		e.merge.DependsOn = append(e.merge.DependsOn, e.choice)
	}
	return nil
}
