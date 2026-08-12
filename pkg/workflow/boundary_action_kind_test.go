package workflow

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// actionKindsFromSource enumerates every type in the package's non-test source that
// DECLARES a method named Execute whose signature is (context.Context, *WorkflowData)
// error.
//
// 🔴 READ THE VERB: DECLARES, NOT IMPLEMENTS (118-F2). The earlier wording said "every
// type that implements Action", and that claim is FALSE of this sweep. Go promotes
// methods through EMBEDDING, so `type W struct{ Action }` satisfies Action while
// declaring no Execute at all -- invisible to a FuncDecl sweep. Such a type would evade
// BOTH layers of this guard: no Execute to enumerate, and no notBoundaryEligible method,
// so it would be eligible while wrapping something opaque.
//
// The gap is CLOSED BY ASSERTION rather than by widening the sweep:
// TestBoundary_NoTypeEmbedsAnAction reds if any type in the package embeds Action or an
// action kind, so the population this sweep CAN see is the whole population as long as
// that arm is green. Closing it by enumeration instead would need go/types over a
// type-checked package (or golang.org/x/tools, which is not a dependency) -- a real cost
// for a hole that can be held shut with fifteen lines.
//
// This is the same family the file already warns about one paragraph down: the earlier
// draft missed six kinds by matching STRINGS, and this one would miss a class by
// matching DECLARED METHODS. An enumeration is only as complete as the thing it
// enumerates over.
//
// IT MATCHES ON TYPES, NOT ON PARAMETER NAMES. An earlier draft of this sweep grepped
// for the literal string "Execute(ctx context.Context, data *WorkflowData) error" and
// silently missed SIX kinds -- parkedSubWorkflowAction names its second parameter
// parentData, and others name theirs _. A sweep that misses the kinds it exists to
// find is worse than none, because it reports a complete-looking census.
//
// BOTH RECEIVER FORMS ARE MATCHED: ActionFunc has a value receiver, every other kind
// has a pointer receiver.
func actionKindsFromSource(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "sanity: the sweep must find package source to parse")

	isCtx := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "context" && sel.Sel.Name == "Context"
	}
	isWorkflowData := func(e ast.Expr) bool {
		star, ok := e.(*ast.StarExpr)
		if !ok {
			return false
		}
		id, ok := star.X.(*ast.Ident)
		return ok && id.Name == "WorkflowData"
	}

	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		ast.Inspect(af, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "Execute" || fd.Recv == nil || len(fd.Recv.List) != 1 {
				return true
			}
			ft := fd.Type
			if ft.Params == nil || len(ft.Params.List) != 2 || ft.Results == nil || len(ft.Results.List) != 1 {
				return true
			}
			if !isCtx(ft.Params.List[0].Type) || !isWorkflowData(ft.Params.List[1].Type) {
				return true
			}
			if id, ok := ft.Results.List[0].Type.(*ast.Ident); !ok || id.Name != "error" {
				return true
			}
			recv := fd.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok {
				out = append(out, id.Name)
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// markerImplementorsByMethod enumerates the types declaring the named marker method.
// Same shape as suspendable_capability_test.go's markerImplementorsFromSource, which
// is the precedent DEC-M23-VB08-R3 points at.
func markerImplementorsByMethod(t *testing.T, method string) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		ast.Inspect(af, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != method || fd.Recv == nil || len(fd.Recv.List) != 1 {
				return true
			}
			if st, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := st.X.(*ast.Ident); ok {
					out = append(out, id.Name)
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// TestBoundary_EveryActionKindIsTriaged is the mechanism DEC-M23-VB08-R3 requires: the
// VB-08 action clause is a PROPERTY, and a new action kind must RED until somebody has
// decided which side of the criterion it falls on.
//
// THE CRITERION (boundary_action_kind.go): a node may be V or S only if reaching
// Completed implies an attributable act occurred AT that node.
//
// WHAT THIS GUARD DOES AND DOES NOT CLOSE. It closes the ENUMERATION gap: no action
// kind can be added to this package and default silently into eligibility. It does NOT
// check that a kind is triaged CORRECTLY -- the classification below is a human
// judgement against the criterion, and this test only forces the judgement to be made.
func TestBoundary_EveryActionKindIsTriaged(t *testing.T) {
	// The triage. Every Action implementor in the package must appear here exactly
	// once, with the criterion's verdict.
	opaque := []string{
		"DAG", // found by the census, not the triage -- see boundary_action_kind.go
		"fanOutAction",
		"mergeAction",
		"parkedSubWorkflowAction",
		"queueSubWorkflowAction",
		"subWorkflowAction",
		"timerAction",
		"waitForSignalOrTimeoutAction",
	}
	// CONDITIONAL, and four of these five moved here as the 118-F1 fix. They were
	// triaged `eligible` on the reasoning "runs consumer actions in band" -- true of
	// their NORMAL form and false of their DEGENERATE one, which is the whole of the
	// blocker: an empty CompositeAction reached Completed with nothing having run.
	// A kind belongs here whenever the criterion's verdict depends on the VALUE.
	conditional := []string{
		"ActionFunc",             // a typed nil FUNC: zero value is nil -- 118-D5
		"CompositeAction",        // empty, or any operand opaque/nil -- 118-F1
		"MapAction",              // nil transform: cannot run
		"RetryableAction",        // transparent wrapper: inherits what it retries -- 118-F1
		"ValidationAction",       // nil validator: cannot run
		"approvalAction",         // opaque only when empty
		"choiceAction",           // ZERO BRANCHES completes on its default with no predicate run
		"waitForConditionAction", // nil predicate: cannot run
	}
	// ELIGIBLE UNCONDITIONALLY -- and this list has now shrunk three times, each time
	// because a kind was triaged on its NORMAL form. Before adding one here, ask what its
	// ZERO VALUE and its EMPTY form do, and MEASURE the answer.
	eligible := []string{
		"waitForSignalAction", // an external party signalling
	}

	var triaged []string
	triaged = append(triaged, opaque...)
	triaged = append(triaged, conditional...)
	triaged = append(triaged, eligible...)
	sort.Strings(triaged)

	fromSource := actionKindsFromSource(t)
	require.NotEmpty(t, fromSource, "sanity: the sweep found no Action implementors at all")

	require.Equal(t, fromSource, triaged,
		"an Action kind in this package is not triaged against the VB-08 criterion.\n"+
			"  in source: %v\n"+
			"  triaged:   %v\n"+
			"A new action kind must be classified as opaque (it can reach Completed on "+
			"structure or the clock alone) or eligible (completion implies an "+
			"attributable act AT the node). Until it is, it would default into "+
			"eligibility silently -- which is the defect this guard exists to prevent.",
		fromSource, triaged)

	// The marker set in source must be exactly the unconditional-opaque triage. This is
	// what keeps the runtime check and the triage from drifting apart: if a marker
	// method is added or removed, one of these two lists is stale and this reds.
	require.Equal(t, opaque, markerImplementorsByMethod(t, "notBoundaryEligible"),
		"the boundaryOpaqueAction marker implementors in source do not match the "+
			"unconditional-opaque triage above")
}

// TestBoundary_OpaqueVerdictMatchesTheMarker is the runtime converse of the census: it
// asserts boundaryOpaqueReason actually refuses each triaged kind, on a real value.
//
// NOT VACUOUS: the eligible arm below is an absolute require.False on each eligible
// kind, so a boundaryOpaqueReason that returned true for everything reds here.
func TestBoundary_OpaqueVerdictMatchesTheMarker(t *testing.T) {
	opaque := []struct {
		name   string
		action Action
	}{
		{"DAG", &DAG{}},
		{"fanOutAction", &fanOutAction{}},
		{"subWorkflowAction", &subWorkflowAction{}},
		{"parkedSubWorkflowAction", &parkedSubWorkflowAction{}},
		{"queueSubWorkflowAction", &queueSubWorkflowAction{}},
		{"mergeAction", &mergeAction{}},
		{"timerAction", &timerAction{nodeName: "n", duration: time.Second}},
		{"waitForSignalOrTimeoutAction", &waitForSignalOrTimeoutAction{nodeName: "n", signalName: "s", duration: time.Second}},
		{"approvalAction (empty)", &approvalAction{}},
	}
	for _, c := range opaque {
		t.Run(c.name, func(t *testing.T) {
			why, got := boundaryOpaqueReason(c.action)
			require.True(t, got, "%s must be refused as a boundary verifier or sink", c.name)
			require.NotEmpty(t, why, "%s was refused with no reason -- a refusal must say what to change", c.name)
		})
	}

	eligible := []struct {
		name   string
		action Action
	}{
		{"ActionFunc", ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })},
		// A choice with at least one branch -- the shape DEC-M23-VB08-R3's table ruled
		// on. The EMPTY one is conditional and lives in the degenerate sweep (118-D8).
		{"choiceAction (one branch)", &choiceAction{nodeName: "n", branches: []choiceBranch{{
			predicate: func(*WorkflowData) bool { return true }, target: "t"}}}},
		{"waitForSignalAction", &waitForSignalAction{nodeName: "n", signalName: "s"}},
		{"waitForConditionAction", &waitForConditionAction{predicate: func(*WorkflowData) bool { return true }}},
		{"approvalAction (named)", &approvalAction{nodeName: "n", signalName: "n"}},
	}
	for _, c := range eligible {
		t.Run(c.name, func(t *testing.T) {
			_, got := boundaryOpaqueReason(c.action)
			require.False(t, got, "%s must be ELIGIBLE as a boundary verifier or sink", c.name)
		})
	}
}

// TestBoundary_DegenerateFormsAreRefused is 118-F1's regression arm, and it is written as
// a CLASS sweep rather than as the two types the blocker named.
//
// THE BLOCKER: boundaryOpaqueReason asked `a.(boundaryOpaqueAction)` and stopped, which
// is a type-identity test wearing a property's clothes. `NewCompositeAction()` -- one
// public call, no DAG -- was accepted as a verifier and completed with nothing having
// run, which is verbatim what fanOutAction is refused for.
//
// EVERY DEGENERATE FORM OF EVERY CONDITIONAL KIND IS HERE, not just the reported one:
// a reported site is a SAMPLE. The empty composite is the minimal witness and is named
// as such -- if the recursion is ever removed, this arm reds first.
func TestBoundary_DegenerateFormsAreRefused(t *testing.T) {
	opaqueInner := &fanOutAction{} // a marker-carrying kind, to test transparency
	deep := Action(&fanOutAction{})
	for range 3 {
		deep = NewCompositeAction(deep)
	}

	cases := []struct {
		action Action
		name   string
		expect string // a distinctive fragment of the required reason
	}{
		// -- the minimal witness, BYPASS-3 --
		{NewCompositeAction(), "CompositeAction EMPTY (118-F1 BYPASS-3)", "completes without running anything"},
		// -- transparency: a wrapper inherits what it wraps --
		{NewCompositeAction(opaqueInner), "CompositeAction(fan-out)", "composed action 0"},
		{NewCompositeAction(&DAG{}), "CompositeAction(*DAG) (118-F1 BYPASS-1)", "composed action 0"},
		{deep, "CompositeAction nested 3 deep over a fan-out", "composed action 0"},
		{NewCompositeAction(NewCompositeAction()), "CompositeAction(empty composite)", "composed action 0"},
		{NewRetryableAction(opaqueInner, 1, 0), "RetryableAction(fan-out)", "the action it retries"},
		{NewRetryableAction(NewCompositeAction(), 1, 0), "RetryableAction(empty composite)", "the action it retries"},
		// -- cannot run at all: the SECOND ground, named separately in the source --
		{NewCompositeAction(nil), "CompositeAction(nil operand)", "nil action cannot run"},
		{NewRetryableAction(nil, 1, 0), "RetryableAction(nil)", "nil action cannot run"},
		{NewMapAction("in", "out", nil), "MapAction nil transform", "no transform cannot run"},
		{NewValidationAction("in", nil, "out", "err"), "ValidationAction nil validator", "no validator cannot run"},
		{(*CompositeAction)(nil), "typed-nil *CompositeAction", "nil composite action cannot run"},
		{(*RetryableAction)(nil), "typed-nil *RetryableAction", "nil retryable action cannot run"},
		{(*MapAction)(nil), "typed-nil *MapAction", "no transform cannot run"},
		{(*ValidationAction)(nil), "typed-nil *ValidationAction", "no validator cannot run"},
		{nil, "a nil Action", "nil action cannot run"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why, opaque := boundaryOpaqueReason(c.action)
			require.True(t, opaque, "%s must be refused as a boundary verifier or sink", c.name)
			require.Contains(t, why, c.expect,
				"refused for the WRONG REASON. A refusal that happens to fire on the right input for the "+
					"wrong cause is not a guard -- the reason string is what a consumer acts on")
		})
	}
}

// TestBoundary_NormalFormsOfConditionalKindsStayEligible is the converse, and without it
// the sweep above is satisfied by refusing every wrapper outright -- which would be a
// silent public-API narrowing, not a fix.
func TestBoundary_NormalFormsOfConditionalKindsStayEligible(t *testing.T) {
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })
	cases := []struct {
		action Action
		name   string
	}{
		{NewCompositeAction(noop), "CompositeAction(consumer func)"},
		{NewCompositeAction(noop, noop), "CompositeAction(two consumer funcs)"},
		{NewCompositeAction(NewCompositeAction(noop)), "CompositeAction nested over a consumer func"},
		{NewRetryableAction(noop, 3, 0), "RetryableAction(consumer func)"},
		{NewRetryableAction(NewCompositeAction(noop), 3, 0), "RetryableAction(composite of consumer func)"},
		{NewMapAction("in", "out", func(v any) (any, error) { return v, nil }), "MapAction with a transform"},
		{NewValidationAction("in", func(any) error { return nil }, "out", "err"), "ValidationAction with a validator"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why, opaque := boundaryOpaqueReason(c.action)
			require.False(t, opaque,
				"%s runs consumer code in band and must stay ELIGIBLE; refusing it would narrow the public "+
					"API rather than close the blocker (refused with: %s)", c.name, why)
		})
	}
}

// TestBoundary_WrapperNestingIsBounded pins the recursion's bound. The wrappers are
// publicly composable IN A LOOP and CompositeAction.Add is exported, so both an
// arbitrarily deep chain and a CYCLE are constructible from outside the package -- and an
// unbounded walk over either exhausts the stack inside build(), which is a `fatal error`
// and not a refusal.
//
// The cap FAILS CLOSED: past it the action cannot be certified free of an ineligible
// kind, and "cannot certify" must refuse.
func TestBoundary_WrapperNestingIsBounded(t *testing.T) {
	noop := ActionFunc(func(context.Context, *WorkflowData) error { return nil })

	t.Run("past the cap, an otherwise-eligible chain is refused", func(t *testing.T) {
		deep := Action(noop)
		for range maxBoundaryActionNestingDepth + 2 {
			deep = NewCompositeAction(deep)
		}
		why, opaque := boundaryOpaqueReason(deep)
		require.True(t, opaque, "a chain deeper than the cap must be refused, not walked")
		require.Contains(t, why, "or are cyclic",
			"the message must not diagnose confidently between deep and cyclic — it cannot tell them apart")
	})

	t.Run("a CYCLE terminates and is refused", func(t *testing.T) {
		// Add is exported, so this is consumer-constructible.
		c := NewCompositeAction(noop)
		c.Add(c)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, opaque := boundaryOpaqueReason(c)
			require.True(t, opaque, "a cyclic composite must be refused")
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("boundaryOpaqueReason did not terminate on a cyclic composite — an unbounded walk here " +
				"is a stack-exhaustion channel from the public API, which is a fatal error rather than a refusal")
		}
	})

	t.Run("just under the cap is still walked, not blanket-refused", func(t *testing.T) {
		deep := Action(noop)
		for range maxBoundaryActionNestingDepth - 2 {
			deep = NewCompositeAction(deep)
		}
		_, opaque := boundaryOpaqueReason(deep)
		require.False(t, opaque,
			"the cap must bound the walk, not replace it — an eligible chain under the cap stays eligible")
	})
}

// TestBoundary_BuildRefusesTheBlockerThroughThePublicAPI is the end-to-end arm: the unit
// checks above prove boundaryOpaqueReason's verdict, this proves build() acts on it. The
// blocker was reported as `build err = <nil>`, so that is the assertion.
func TestBoundary_BuildRefusesTheBlockerThroughThePublicAPI(t *testing.T) {
	for _, c := range []struct {
		action Action
		name   string
	}{
		{NewCompositeAction(), "empty composite (BYPASS-3)"},
		{NewCompositeAction(&DAG{}), "composite wrapping a DAG (BYPASS-1)"},
		{NewRetryableAction(NewCompositeAction(), 1, 0), "retryable wrapping an empty composite"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noop := func(context.Context, *WorkflowData) error { return nil }
			b := NewWorkflowBuilder().WithWorkflowID("f1")
			b.AddStartNode("v").WithAction(c.action)
			b.AddNode("d").WithActionFunc(noop).DependsOn("v")
			b.AddNode("s").WithActionFunc(noop).DependsOn("d")
			b.WithBoundary("d", "v", "s")
			_, err := b.Build()
			require.ErrorIs(t, err, ErrValidation,
				"build() must refuse this declaration; the blocker was reported as build err = <nil>")
			require.Contains(t, err.Error(), "verifier",
				"and must name which role it refused")
		})
	}
}

// TestBoundary_EmptyNodeNameIsNotTheAvoidSentinel is 118-F4's regression arm.
//
// reachAvoiding took `avoid string` with "" meaning "avoid nothing" -- but "" IS A LEGAL
// NODE NAME, so a graph containing one collided with the sentinel and the anti-vacuity
// clause reported "no path from doer" about a doer that HAS a path. It failed closed, so
// nothing was wrongly accepted; the defect was a FACTUALLY FALSE diagnostic, which sends
// a consumer looking for an edge that is present.
//
// THE ORACLE COULD NOT SEE THIS. The exhaustive small-DAG corpus names nodes n0..n5, so
// no amount of graph coverage reaches a defect living in the NAME space. That is the
// bound of an exhaustive corpus, and it is why this arm is hand-written.
func TestBoundary_EmptyNodeNameIsNotTheAvoidSentinel(t *testing.T) {
	noop := func(context.Context, *WorkflowData) error { return nil }

	t.Run("a boundary over a graph containing the empty node name is ACCEPTED", func(t *testing.T) {
		// ""->v->s with doer "": the doer genuinely reaches the sink, and v is the sole
		// root, so the declaration holds.
		b := NewWorkflowBuilder().WithWorkflowID("f4")
		b.AddStartNode("v").WithActionFunc(noop)
		b.AddNode("").WithActionFunc(noop).DependsOn("v")
		b.AddNode("s").WithActionFunc(noop).DependsOn("")
		b.WithBoundary("", "v", "s")
		_, err := b.Build()
		require.NoError(t, err,
			"the empty string is a legal node name and must not collide with the avoid sentinel; "+
				"before 118-F4 this was refused with the factually false \"no path from doer\"")
	})

	t.Run("the empty node name still works as the VERIFIER being avoided", func(t *testing.T) {
		// "" is the verifier here, so it is what clause (b) must avoid. A nil-vs-"" mixup
		// in the other direction would make the walk avoid nothing and wrongly ACCEPT.
		b := NewWorkflowBuilder().WithWorkflowID("f4b")
		b.AddStartNode("r").WithActionFunc(noop)
		b.AddNode("").WithActionFunc(noop).DependsOn("r")
		b.AddNode("d").WithActionFunc(noop).DependsOn("r")
		b.AddNode("s").WithActionFunc(noop).DependsOn("d")
		_, err := b.Build()
		require.NoError(t, err, "fixture must build without the declaration")

		b2 := NewWorkflowBuilder().WithWorkflowID("f4c")
		b2.AddStartNode("r").WithActionFunc(noop)
		b2.AddNode("").WithActionFunc(noop).DependsOn("r")
		b2.AddNode("d").WithActionFunc(noop).DependsOn("r")
		b2.AddNode("s").WithActionFunc(noop).DependsOn("d")
		b2.WithBoundary("d", "", "s")
		_, err2 := b2.Build()
		require.ErrorIs(t, err2, ErrValidation,
			"r reaches s without passing the verifier \"\", so this must be REFUSED — if the walk treated "+
				"the empty verifier as \"avoid nothing\" it would wrongly accept")
		require.Contains(t, err2.Error(), "without passing verifier",
			"and refused by the predicate, not incidentally")
	})
}

// TestBoundary_DAGDoesNotAliasTheBuildersSlice is 118-F5's regression arm: dag.boundaries
// is in the RUN-CONSTANCY class, so it must not share a backing array with a builder the
// consumer still holds and can keep calling WithBoundary on.
func TestBoundary_DAGDoesNotAliasTheBuildersSlice(t *testing.T) {
	noop := func(context.Context, *WorkflowData) error { return nil }
	b := NewWorkflowBuilder().WithWorkflowID("f5")
	b.AddStartNode("v").WithActionFunc(noop)
	b.AddNode("d").WithActionFunc(noop).DependsOn("v")
	b.AddNode("s").WithActionFunc(noop).DependsOn("d")
	b.WithBoundary("d", "v", "s")

	dag, err := b.Build()
	require.NoError(t, err)
	require.Len(t, dag.boundaries, 1)

	// Mutate the builder's slice in place, which a further WithBoundary can do by
	// growing into the same backing array.
	b.boundaries[0] = boundaryDecl{doer: "X", verifier: "Y", sink: "Z"}

	require.Equal(t, boundaryDecl{doer: "d", verifier: "v", sink: "s"}, dag.boundaries[0],
		"the built DAG must hold its own copy: a validated set that the builder can still rewrite is "+
			"validated-then-changed, on a graph already stamped as built")
}

// TestBoundary_NoTypeEmbedsAnAction closes 118-F2's hole by assertion.
//
// actionKindsFromSource enumerates types that DECLARE Execute. A type that EMBEDS an
// Action gets Execute promoted without declaring it, so it satisfies Action, is invisible
// to that sweep, AND misses the notBoundaryEligible marker -- eligible by default while
// potentially wrapping an opaque kind. Exactly the 118-F1 shape, one mechanism over.
//
// No such type exists today. This arm is what makes that a CHECKED fact rather than an
// unstated assumption behind the census's completeness claim.
func TestBoundary_NoTypeEmbedsAnAction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "sanity: the sweep must find package source to parse")

	// Every kind the triage knows about, plus the interface itself.
	actionish := map[string]bool{"Action": true, "ActionFunc": true, "CompositeAction": true,
		"RetryableAction": true, "MapAction": true, "ValidationAction": true, "DAG": true}

	var offenders []string
	structsSeen := 0
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, perr := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, perr)
		ast.Inspect(af, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			structsSeen++
			for _, fld := range st.Fields.List {
				if len(fld.Names) != 0 { // named field: not embedded, no promotion
					continue
				}
				e := fld.Type
				if star, ok := e.(*ast.StarExpr); ok {
					e = star.X
				}
				if id, ok := e.(*ast.Ident); ok && actionish[id.Name] {
					offenders = append(offenders, ts.Name.Name+" embeds "+id.Name+" ("+fset.Position(ts.Pos()).String()+")")
				}
			}
			return true
		})
	}

	// Anti-vacuity: an instrument that parsed nothing reports the same empty result.
	require.Greater(t, structsSeen, 20,
		"only %d struct types seen; this package has many more, so the sweep is blind and its empty "+
			"result means nothing", structsSeen)

	require.Empty(t, offenders,
		"a type EMBEDS an Action, which promotes Execute without declaring it: %v\n\n"+
			"Such a type satisfies Action but is INVISIBLE to actionKindsFromSource (a FuncDecl sweep) "+
			"and also misses the notBoundaryEligible marker, so it would be eligible as a boundary "+
			"verifier or sink by default while possibly wrapping an opaque kind. Either give it an "+
			"explicit Execute so the census can see it, or triage it and extend this arm.", offenders)
}

// TestBoundary_PostBuildMutationCannotSmuggleAnOpaqueAction is 118-D4's regression arm,
// and it is the BLOCKER's arm: the action clause runs inside build(), while
// CompositeAction.Add is exported and appends to the slice the built DAG holds by
// pointer. Validation-time smuggling was closed; mutation-time smuggling was not.
//
// Measured before the fix, at 4b517c2: build accepted, then c.Add(builtChildDAG) made
// the DAG's OWN action opaque, and Execute returned nil -- IT RAN.
func TestBoundary_PostBuildMutationCannotSmuggleAnOpaqueAction(t *testing.T) {
	noop := func(context.Context, *WorkflowData) error { return nil }

	childB := NewWorkflowBuilder().WithWorkflowID("d4-child")
	childB.AddStartNode("c").WithActionFunc(noop)
	builtChild, err := childB.Build()
	require.NoError(t, err)

	c := NewCompositeAction(ActionFunc(noop)) // eligible at validation time
	b := NewWorkflowBuilder().WithWorkflowID("d4")
	b.AddStartNode("v").WithAction(c)
	b.AddNode("d").WithActionFunc(noop).DependsOn("v")
	b.AddNode("s").WithActionFunc(noop).DependsOn("d")
	b.WithBoundary("d", "v", "s")
	dag, err := b.Build()
	require.NoError(t, err, "the composite is eligible at build time, so this must be accepted")

	// The consumer still holds c. Smuggle an unconditionally-opaque action in after
	// validation. *DAG is the strongest case: it runs a whole child graph.
	c.Add(builtChild)

	why, opaque := boundaryOpaqueReason(dag.nodes["v"].action)
	require.False(t, opaque,
		"THE BLOCKER: the built DAG's verifier action changed AFTER validation certified it. The built "+
			"token would then certify a state the graph no longer has, which is SEAL-01's deleted "+
			"AddDependencies one level down — mutating the ACTION set instead of the edge set. "+
			"(got: %s)", why)

	// And the smuggled child must not run.
	ran := 0
	c2 := NewCompositeAction(ActionFunc(func(context.Context, *WorkflowData) error { ran++; return nil }))
	b2 := NewWorkflowBuilder().WithWorkflowID("d4b")
	b2.AddStartNode("v").WithAction(c2)
	b2.AddNode("d").WithActionFunc(noop).DependsOn("v")
	b2.AddNode("s").WithActionFunc(noop).DependsOn("d")
	b2.WithBoundary("d", "v", "s")
	dag2, err := b2.Build()
	require.NoError(t, err)

	smuggledRan := 0
	c2.Add(ActionFunc(func(context.Context, *WorkflowData) error { smuggledRan++; return nil }))
	require.NoError(t, dag2.Execute(context.Background(), NewWorkflowData("d4b")))
	require.Equal(t, 1, ran, "the validated operand must still run — the snapshot is a copy, not a removal")
	require.Equal(t, 0, smuggledRan,
		"an action appended AFTER build must not run on the built graph: run-constancy means the "+
			"validated set is what executes")
}

// TestBoundary_NonBoundaryNodesAreNotSnapshotted pins the fix's SCOPE. Snapshotting is
// deliberately narrow -- only a declared boundary's verifier and sink -- so a workflow
// that declares no boundary observes exactly the action the consumer supplied, and the
// det-tax moat is untouched.
func TestBoundary_NonBoundaryNodesAreNotSnapshotted(t *testing.T) {
	noop := func(context.Context, *WorkflowData) error { return nil }
	c := NewCompositeAction(ActionFunc(noop))
	b := NewWorkflowBuilder().WithWorkflowID("d4c")
	b.AddStartNode("v").WithAction(c)
	dag, err := b.Build()
	require.NoError(t, err)
	require.Same(t, c, dag.nodes["v"].action,
		"a node no boundary names must keep the consumer's own action object; narrowing the fix is what "+
			"keeps it from changing behaviour for workflows that declare nothing")
}

// TestBoundary_TypedNilFuncAndTheTwoUnsweptKinds covers 118-D5 and the two kinds the
// review flagged as untested. Swept here rather than waiting for a further pass.
//
// 118-D8 is the one nobody named: an empty choiceAction WITH a default COMPLETES having
// evaluated no consumer predicate at all. Measured:
//
//	choice{0 branches, hasDefault} -> Execute = <nil>              COMPLETED, nothing ran
//	choice{0 branches, no default} -> Execute = ErrNoBranchMatched can never complete
//
// The first is VB-08's harm verbatim, in a kind the original ruling table allowed
// outright -- true of a choice WITH branches, false of one without.
func TestBoundary_TypedNilFuncAndTheTwoUnsweptKinds(t *testing.T) {
	cases := []struct {
		action Action
		name   string
		expect string
	}{
		{ActionFunc(nil), "ActionFunc(nil) — the zero value (118-D5)", "nil action cannot run"},
		{NewCompositeAction(ActionFunc(nil)), "Composite(ActionFunc(nil))", "composed action 0"},
		{NewRetryableAction(ActionFunc(nil), 1, 0), "Retryable(ActionFunc(nil))", "the action it retries"},
		{&waitForConditionAction{}, "waitForCondition{nil predicate}", "no predicate cannot run"},
		{&choiceAction{nodeName: "n"}, "choice{0 branches, no default}", "no branches"},
		{&choiceAction{nodeName: "n", hasDefault: true, defaultTarget: "t"},
			"choice{0 branches, WITH default} — completes, nothing ran (118-D8)", "no branches"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why, opaque := boundaryOpaqueReason(c.action)
			require.True(t, opaque, "%s must be refused as a boundary verifier or sink", c.name)
			require.Contains(t, why, c.expect, "refused for the WRONG REASON")
		})
	}

	// The converse, so the sweep is not satisfied by refusing these kinds outright.
	var declared ActionFunc = func(context.Context, *WorkflowData) error { return nil }
	ok := []struct {
		action Action
		name   string
	}{
		{declared, "a declared ActionFunc"},
		{&waitForConditionAction{predicate: func(*WorkflowData) bool { return true }}, "waitForCondition with a predicate"},
		{&choiceAction{nodeName: "n", branches: []choiceBranch{{
			predicate: func(*WorkflowData) bool { return true }, target: "t"}}}, "choice with one branch"},
	}
	for _, c := range ok {
		t.Run("ELIGIBLE: "+c.name, func(t *testing.T) {
			why, opaque := boundaryOpaqueReason(c.action)
			require.False(t, opaque, "%s must stay eligible (refused with: %s)", c.name, why)
		})
	}
}
