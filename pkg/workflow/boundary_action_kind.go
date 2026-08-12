package workflow

import (
	"fmt"
	"strings"
)

// THE VB-08 ACTION CLAUSE IS A PROPERTY, NOT A LIST (DEC-M23-VB08-R3).
//
// THE CRITERION, which is the requirement's own harm restated as a test:
//
//	A node may be a boundary's VERIFIER or SINK only if reaching Completed implies
//	that some consumer- or operator-attributable act occurred AT that node. A kind
//	that can reach Completed on structure or on the clock alone is refused.
//
// VB-08 exists because an empty AddFanOut or a fast-pathing AddSubWorkflow reaches
// Completed with nothing having run at that node, which makes the declaration name a
// position no act passes through.
//
// THE SET IS DEFINED BY THE boundaryOpaqueAction MARKER, NOT BY A HAND-WRITTEN LIST OF
// TYPES. That is the suspend.go:82 precedent, and following it here is not decoration:
// the first draft of this clause WAS a hand list, and it was short by
// parkedSubWorkflowAction -- a hand list inside the clause would have reproduced,
// inside the guard, the exact enumeration defect the guard exists to prevent.
// TestBoundary_EveryActionKindIsTriaged enumerates the package's Action implementors
// mechanically and REDS on any kind that is in neither the opaque set nor the eligible
// set, so a new action kind cannot land silently eligible.

type boundaryOpaqueAction interface {
	Action
	notBoundaryEligible() string
}

// The opaque kinds. Each returns WHY, so a refusal tells a consumer what to change
// rather than only that something is wrong. Grouped by the criterion's two arms.

// -- completes on STRUCTURE --

// notBoundaryEligible: an empty fan-out has zero branches and completes with no branch
// body having run.
func (a *fanOutAction) notBoundaryEligible() string {
	return "a fan-out with zero branches completes without running any branch body"
}

// notBoundaryEligible: the inline sub-workflow fast path completes the node without
// driving the child (subworkflow.go).
func (a *subWorkflowAction) notBoundaryEligible() string {
	return "an inline sub-workflow can complete on its fast path without driving the child"
}

// notBoundaryEligible: the child runs OUT-OF-BAND; this node never drives it, so its
// completion attributes no act to this position.
func (a *parkedSubWorkflowAction) notBoundaryEligible() string {
	return "a parked sub-workflow's child runs out-of-band; this node never drives it"
}

// notBoundaryEligible: as parked, but dispatched by the engine.
func (a *queueSubWorkflowAction) notBoundaryEligible() string {
	return "a queued sub-workflow's child runs out-of-band; this node never drives it"
}

// notBoundaryEligible: a merge completes because one branch of a join arrived. That is
// a structural fact about the graph, not an act at the merge.
func (a *mergeAction) notBoundaryEligible() string {
	return "a merge completes on the structure of a join, not on work done at the merge"
}

// notBoundaryEligible: FOUND BY THE CENSUS, NOT BY THE TRIAGE -- and it is the reason
// the census exists. (*DAG).Execute has the Action signature, so *DAG structurally
// SATISFIES Action and `WithAction(childDAG)` compiles: a whole DAG can be a node's
// action. Verified mechanically -- `var _ Action = (*DAG)(nil)` and
// `AddStartNode("n").WithAction(&DAG{})` both compile at fec05029.
//
// Opaque for the same reason as the sub-workflow kinds: the acts occur in the CHILD
// graph, not at this node, so this node's Completed attributes nothing to this
// position.
//
// SEPARATELY, AND NOT FOR THIS CLAUSE TO SETTLE: this is an unsanctioned nesting
// channel. AddSubWorkflow enforces a depth ceiling, an ancestor-cycle guard and a
// suspendable-child refusal, all documented INLINE-ONLY (builder.go); a DAG smuggled
// in through WithAction gets none of them. Routed up as F118-ENG-01 rather than
// handled here -- refusing it as a boundary V/S does not close it.
func (d *DAG) notBoundaryEligible() string {
	return "a DAG used as an action runs a child graph; the acts occur in the child, not at this node"
}

// 🔴 WHAT THIS CLAUSE CANNOT SEE, STATED HERE BECAUSE THE OTHER BOUND IS STATED HERE.
//
// The VB-08 action clause is IN-PACKAGE COMPLETE AND CONSUMER-INCOMPLETE, and the second
// half is the honest one. A consumer-defined type that embeds *workflow.CompositeAction is
// invisible to BOTH layers: the embedding census sweeps THIS package's files, and a
// consumer's package is not among them; and the triage below switches on CONCRETE types,
// so `type MyAction struct{ *workflow.CompositeAction }` matches no case. It promotes
// Execute, it satisfies Action, and it arrives at the eligibility check as a type that
// neither layer has a rule for.
//
// IT IS NOT CORRECT-BY-CRITERION. IT IS UNREACHABLE-BY-INSTRUMENT, and the difference
// decides how a green here may be read. The tempting justification -- that a
// consumer-authored action is the consumer's own act, and so is theirs to answer for --
// does not survive the empty case: a consumer type embedding an EMPTY composite reaches
// Completed with nothing having run at that node, which is the harm VB-08 exists to
// prevent, and which the triage itself spells out for the in-package form as "an empty
// composite completes without running anything". THE IN-PACKAGE FORM IS REFUSED THERE.
// THE EMBEDDED FORM IS NOT.
//
// No in-package sweep can enumerate consumer types, and a mechanism that appeared to would
// be theatre. So the bound is stated rather than instrumented: A GREEN FROM THIS CLAUSE IS
// A CLAIM ABOUT THIS PACKAGE'S ACTION KINDS, NEVER ABOUT CONSUMER-DEFINED ONES.

// -- completes on THE CLOCK --

// notBoundaryEligible: a timer completes when the clock passes its due instant. No
// party acted.
func (a *timerAction) notBoundaryEligible() string {
	return "a timer completes on the clock alone; no party acts at this node"
}

// notBoundaryEligible: THE CASE THAT MADE THIS A PROPERTY RATHER THAN A LIST. This
// kind is eligible-looking because its signal arm is a real external act -- but its
// TIMEOUT arm completes on the clock alone, and a boundary cannot depend on which arm
// fires at run time. Refused for the arm it can take, not the arm it usually takes.
func (a *waitForSignalOrTimeoutAction) notBoundaryEligible() string {
	return "its timeout arm completes on the clock alone, and which arm fires is a run-time fact"
}

// maxBoundaryActionNestingDepth bounds the WRAPPER recursion below. Mirrors
// maxSubWorkflowDepthCap (subworkflow.go), which is this package's existing answer to
// "a consumer-controlled nesting count reachable from the public API".
//
// It is needed because the wrappers are PUBLICLY COMPOSABLE IN A LOOP:
// NewCompositeAction(NewCompositeAction(...)) nests arbitrarily, and CompositeAction.Add
// is exported, so `c := NewCompositeAction(); c.Add(c)` builds a CYCLE. An unbounded
// walk over either shape exhausts the stack inside build(), which is a `fatal error`
// rather than a refusal -- in the milestone that has spent a phase closing exactly that
// class. The cap FAILS CLOSED: past it we cannot certify the action carries no opaque
// kind, and "cannot certify" must refuse, not accept.
//
// Like checkValueDepth's walk, the cap cannot distinguish DEEP from CYCLIC and the
// message says so rather than diagnosing confidently and wrongly.
const maxBoundaryActionNestingDepth = 1024

// boundaryOpaqueReason reports whether an action may not be a boundary's verifier or
// sink, and why.
//
// THREE KINDS OF ANSWER, and conflating them is what 118-F1 was:
//
//  1. UNCONDITIONAL, via the boundaryOpaqueAction marker. A type either implements the
//     method or does not.
//  2. CONDITIONAL ON VALUE. An approvalAction is opaque only when EMPTY; a
//     CompositeAction only when it has zero actions or an opaque operand; a MapAction
//     only when its transform is nil. A marker interface cannot express any of these,
//     because it can only ask a question of the TYPE.
//  3. CONDITIONAL ON WHAT IT WRAPS -- the case that defeated the first version. A
//     CompositeAction or RetryableAction is a TRANSPARENT WRAPPER: it can reach
//     Completed exactly when the things inside it do. So the criterion has to be
//     evaluated on the OPERANDS, recursively.
//
// 🔴 118-F1, AND WHY A "PROPERTY, NOT A LIST" CAN STILL BE A LIST. The first version
// asked `a.(boundaryOpaqueAction)` and stopped. That is a criterion stated behaviourally
// and realised STRUCTURALLY -- a type-identity test wearing a property's clothes -- and
// one public call defeated it. Measured at 83c9c8e, in a workflow built through the
// public API:
//
//	V = NewCompositeAction()          -> build ACCEPTED, exec nil, V status COMPLETED
//
// Nothing ran at V and it completed, which is VERBATIM the harm VB-08 exists to prevent
// and verbatim what fanOutAction is refused for. Same harm, opposite verdict, because
// one wore a wrapper. Recursing is what makes the clause a property of BEHAVIOUR rather
// than of declared type.
//
// A SECOND, SEPARATELY-NAMED GROUND, so nobody reads the criterion as having grown: a
// nil operand or nil transform is refused because THE NODE'S ACTION CANNOT RUN, so the
// node can never reach Completed and a boundary naming it can never be satisfied. That
// is anti-vacuity, not the completes-on-structure criterion. Measured, and the reason it
// is not merely theoretical: each of these takes the PROCESS DOWN rather than failing
// the node, because the panic happens in an executor worker goroutine where no caller's
// recover() can reach it.
//
//	V = NewCompositeAction(nil)                     -> panic: nil pointer, process dies
//	V = NewRetryableAction(nil, 1, 0)               -> panic: nil pointer, process dies
//	V = NewMapAction("in","out", nil)               -> panic: nil pointer, process dies
//	V = NewValidationAction("in", nil, "out","err") -> panic: nil pointer, process dies
//
// REFUSING THEM HERE DOES NOT FIX THAT PANIC and must not be read as fixing it: the node
// still dies the same way when it runs, boundary or no boundary. It is reachable with no
// declaration involved at all, so it is an action-layer hole, routed up as F118-ENG-06
// rather than absorbed here.
func boundaryOpaqueReason(a Action) (string, bool) {
	return boundaryOpaqueReasonAt(a, 0)
}

func boundaryOpaqueReasonAt(a Action, depth int) (string, bool) {
	if depth > maxBoundaryActionNestingDepth {
		return fmt.Sprintf("its wrappers nest more than %d deep, or are cyclic -- this check cannot "+
			"distinguish the two, and neither can be certified free of an ineligible action",
			maxBoundaryActionNestingDepth), true
	}
	// A nil action cannot run. Checked before the type switch because a nil interface
	// matches no case.
	if a == nil {
		return "a nil action cannot run, so this node can never complete", true
	}
	if o, ok := a.(boundaryOpaqueAction); ok {
		return o.notBoundaryEligible(), true
	}

	switch v := a.(type) {
	case *CompositeAction:
		// A typed nil pointer is NOT caught by the `a == nil` check above -- the
		// interface holds a type -- and reading v.actions off it would panic inside the
		// guard.
		if v == nil {
			return "a nil composite action cannot run, so this node can never complete", true
		}
		if len(v.actions) == 0 {
			return "an empty composite completes without running anything, so nothing occurs at this node", true
		}
		for i, inner := range v.actions {
			if why, opaque := boundaryOpaqueReasonAt(inner, depth+1); opaque {
				return fmt.Sprintf("its composed action %d is not eligible: %s", i, why), true
			}
		}
	case *RetryableAction:
		if v == nil {
			return "a nil retryable action cannot run, so this node can never complete", true
		}
		// A retry wrapper is transparent: it can reach Completed exactly when the
		// action it wraps can.
		if why, opaque := boundaryOpaqueReasonAt(v.action, depth+1); opaque {
			return fmt.Sprintf("the action it retries is not eligible: %s", why), true
		}
	case *MapAction:
		if v == nil || v.mapFn == nil {
			return "a map action with no transform cannot run, so this node can never complete", true
		}
	case *ValidationAction:
		if v == nil || v.validationFn == nil {
			return "a validation action with no validator cannot run, so this node can never complete", true
		}
	case ActionFunc:
		// 118-D5. A typed nil FUNC, exactly like the typed nil POINTERS above: the
		// interface holds a type, so `a == nil` is false and without this case the
		// switch finds nothing and returns ELIGIBLE. ActionFunc is the package's most
		// used action type, it is public, and its ZERO VALUE is nil -- `var fn
		// ActionFunc` reaches it with no deliberate act.
		//
		// Note the receiver: ActionFunc is a func type with a VALUE receiver, so the
		// case is `ActionFunc`, not `*ActionFunc`.
		if v == nil {
			return "a nil action cannot run, so this node can never complete", true
		}
	case *waitForConditionAction:
		// Its Execute calls a.predicate(data) unguarded, so a nil predicate panics.
		if v == nil || v.predicate == nil {
			return "a wait-for-condition with no predicate cannot run, so this node can never complete", true
		}
	case *choiceAction:
		// TWO DIFFERENT HARMS, ONE CONDITION, and the first is the one that matters --
		// it is VB-08's harm verbatim in a kind the original ruling table allowed
		// outright. MEASURED rather than reasoned:
		//
		//	choice{0 branches, hasDefault}  -> Execute = <nil>   COMPLETED, no predicate run
		//	choice{0 branches, no default}  -> Execute = ErrNoBranchMatched (can never complete)
		//
		// A choice with no branches that carries an Otherwise takes the default and
		// completes having evaluated NO consumer predicate: completion on STRUCTURE
		// alone, which is precisely what fanOutAction is refused for. Without a default
		// it dead-ends and can never complete. Either way it may not be a V or an S.
		//
		// "choiceAction is eligible" is true of a choice WITH branches -- the shape
		// DEC-M23-VB08-R3's table was ruling on -- and false of one without. That is the
		// value-dependence that makes this kind conditional rather than eligible.
		if v == nil || len(v.branches) == 0 {
			return "a choice with no branches completes on its default without evaluating any predicate, " +
				"or dead-ends if it has none", true
		}
	case *approvalAction:
		// An approval with no decision signal can never be satisfied, so it can never
		// carry an approver's act.
		//
		// REACHABILITY, MEASURED RATHER THAN ASSUMED: AddApproval derives signalName 1:1
		// from the node name and refuses a "" name at Build (builder.go), so a DAG built
		// through the public API cannot hold one. This arm is therefore defence in depth
		// against a state the builder already refuses -- it is covered by a
		// direct-construction unit test, NOT by a builder test, because no builder test
		// could reach it. Stated so a green here is not read as evidence the public path
		// produces this shape.
		if v == nil || v.signalName == "" {
			return "an approval with no decision signal can never be satisfied", true
		}
	}
	return "", false
}

// actionKindName renders an action's concrete type for a refusal message.
func actionKindName(a Action) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", a), "*workflow.")
}

// snapshotBoundaryAction returns an action equal to a but sharing no MUTABLE state with
// it, so a consumer holding the original cannot change what the built graph will run.
//
// 🔴 118-D4, AND IT IS THE BLOCKER THE ACTION CLAUSE ALONE DOES NOT CLOSE. The clause
// runs inside build(). CompositeAction.Add is EXPORTED and appends to the very slice the
// built DAG holds by pointer, so validation-time smuggling was closed and MUTATION-TIME
// smuggling was wide open. Measured at 4b517c2, before this function existed:
//
//	build with an eligible composite   -> <nil>        (accepted)
//	c.Add(builtChildDAG)                               (*DAG is unconditionally opaque)
//	boundaryOpaqueReason(dag V action) -> opaque=true  (the DAG'S OWN action, now opaque)
//	Execute                            -> <nil>        *** IT RAN ***
//
// That breaks RUN-CONSTANCY: the built token certifies a state the graph no longer has.
// It is the shape SEAL-01 deleted AddDependencies for -- post-build mutation of the edge
// set -- one level down, mutating the ACTION set.
//
// The fact was in view and the consequence was not: the depth cap above cites
// `c := NewCompositeAction(); c.Add(c)` as a CYCLE risk inside the walk. Reasoning about
// Add's exported mutability as a walk hazard and not as a post-validation channel is the
// same near-miss shape this milestone keeps producing.
//
// WHY A SNAPSHOT RATHER THAN RE-VALIDATING AT Execute: re-validation taxes the universal
// hot path and spends the zero-determinism-tax moat, which is this project's headline.
// The snapshot is taken ONCE, at build, on the two nodes a declaration actually names.
// It is the same aliasing fix already applied to dag.boundaries (slices.Clone, 118-F5) --
// the same defect one field over, which is why it is worth naming as a class: a validated
// set that the consumer still holds a handle to is validated-then-changed.
//
// SCOPE, deliberately narrow: only a declared boundary's verifier and sink are
// snapshotted. Every other node keeps the exact action the consumer supplied, so this
// changes no behaviour a workflow without boundaries can observe.
func snapshotBoundaryAction(a Action, depth int) Action {
	// Past the cap the action clause has already refused this declaration, so this is
	// unreachable in practice; returning a rather than recursing keeps it terminating
	// on a cyclic operand graph regardless.
	if depth > maxBoundaryActionNestingDepth {
		return a
	}
	switch v := a.(type) {
	case *CompositeAction:
		if v == nil {
			return a
		}
		clone := make([]Action, len(v.actions))
		for i, inner := range v.actions {
			clone[i] = snapshotBoundaryAction(inner, depth+1)
		}
		return &CompositeAction{actions: clone}
	case *RetryableAction:
		if v == nil {
			return a
		}
		c := *v // copy the retry policy fields verbatim
		c.action = snapshotBoundaryAction(v.action, depth+1)
		return &c
	}
	// Every other kind is either immutable from outside the package or carries no
	// operand list a consumer can append to.
	return a
}
