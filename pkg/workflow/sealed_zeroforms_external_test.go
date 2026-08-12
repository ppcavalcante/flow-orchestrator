package workflow_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// THE R-01 CONSTRUCTION CLASS, and the sweep that keeps its claim honest.
//
// M23 SEAL-06 rests on a property that is easy to state and easy to get wrong:
// unexporting every constructor and every field does NOT seal construction. Go only
// forbids NAMING an unexported field in a composite literal, so any expression that
// sets no fields is still legal from outside — and each one yields a DAG that never
// passed build().
//
// IN package workflow_test ON PURPOSE, the same reason nodag_external_test.go gives:
// nearly every pkg/workflow test file is `package workflow`, where these literals are
// unremarkable and an in-package version could just assign the unexported field. The
// hole is a BOUNDARY property; it can only be exhibited from outside the package.
//
// THE TABLE IS EXAMPLES OF AN OPEN SET, NOT AN INVENTORY — and the assertion is
// deliberately shaped so that stays true. The table below stops by choice rather than
// exhaustion: append(...)[0], a chan struct{ workflow.DAG }, a sync.Pool New and a
// zero-value type assertion all qualify too and are simply not rows. These are not
// distinct constructions; they are ways to SPELL THE ZERO VALUE of an exported type, and
// that set is open under the language — generics added one for free, and the next syntax
// addition will add more with nobody touching this package.
//
// No count appears in this comment, deliberately. Every count attached to this class has
// been wrong within a single phase — the in-code list and the phase record disagreed with
// each other and both disagreed with the sweep — so the population is the table, read
// from the table.
//
// So there is NO count assertion and NO length check here, on purpose. Asserting the
// table's size would re-introduce exactly the completeness claim that keeps turning out
// wrong. What is asserted is the INVARIANT: every form in the table is
// refused. A future form is one more row, never a recount.
//
// WHAT MAKES THE INVARIANT HOLD is that the forms are open but the EFFECT is closed:
// every one of them produces a ZERO graph, never a populated rogue one (reflect reaches
// the unexported nodes map, but CanSet is false), so a single check at consumption is
// total over the class however many ways there turn out to be to spell it. This file is
// the "re-run the sweep and confirm every form still refuses" that the built field's
// doc comment names as the checkability story.
//
// If a row here ever stops refusing, the seal has a hole — not a stale test.
func TestSealed_EveryExternalZeroFormIsRefused(t *testing.T) {
	for _, tc := range externalZeroForms() {
		t.Run(tc.name, func(t *testing.T) {
			dag := tc.mint()
			require.NotNil(t, dag, "the form must actually produce a *DAG, or the row proves nothing")

			err := dag.Execute(context.Background(), workflow.NewWorkflowData("zero-form-probe"))

			// ErrorIs, not just "an error": before SEAL-06 these drove to a VACUOUS
			// SUCCESS (Execute returned nil with zero nodes journaled, which on the M17
			// dispatch path becomes a durable MarkDone). A generic non-nil assertion
			// would pass on any unrelated failure and hide a regression to that shape.
			require.ErrorIs(t, err, workflow.ErrDAGNotBuilt,
				"an externally-minted zero DAG must be refused by the builder token, not executed")
		})
	}
}

type zeroForm struct {
	name string
	mint func() *workflow.DAG
}

// embedsDAG is the row that is NOT an attack: embedding is an ordinary Go idiom that
// hands a consumer the zero value for free, with no literal written anywhere. It is the
// form a real user trips, which is why the refusal names its cause.
type embedsDAG struct{ workflow.DAG }

// holdsDAG is the plain (non-embedded) field — distinct from embedding because it names
// a field of THIS struct, never a field of DAG, so the unexported-field rule never bites.
type holdsDAG struct{ D workflow.DAG }

// zeroOf is the generic form: Go 1.18 added this route to the class for free, without
// anyone touching the workflow package. It is the concrete reason no enumeration here
// can stay complete.
func zeroOf[T any]() T {
	var t T
	return t
}

func externalZeroForms() []zeroForm {
	return []zeroForm{
		{"composite literal", func() *workflow.DAG {
			return &workflow.DAG{}
		}},
		{"new", func() *workflow.DAG {
			return new(workflow.DAG)
		}},
		{"var declaration", func() *workflow.DAG {
			var d workflow.DAG
			return &d
		}},
		{"slice element", func() *workflow.DAG {
			s := make([]workflow.DAG, 1)
			return &s[0]
		}},
		{"slice composite literal", func() *workflow.DAG {
			s := []workflow.DAG{{}}
			return &s[0]
		}},
		{"array element", func() *workflow.DAG {
			var a [1]workflow.DAG
			return &a[0]
		}},
		{"embedded field", func() *workflow.DAG {
			var e embedsDAG
			return &e.DAG
		}},
		{"plain struct field", func() *workflow.DAG {
			var h holdsDAG
			return &h.D
		}},
		{"map of pointers, elided literal", func() *workflow.DAG {
			m := map[string]*workflow.DAG{"k": {}}
			return m["k"]
		}},
		{"bare named result", func() *workflow.DAG {
			v := namedResult()
			return &v
		}},
		{"reflect.New", func() *workflow.DAG {
			return reflect.New(reflect.TypeOf(workflow.DAG{})).Interface().(*workflow.DAG) //nolint:errcheck // reflection constructs a known *DAG
		}},
		{"generic zero value", func() *workflow.DAG {
			v := zeroOf[workflow.DAG]()
			return &v
		}},
	}
}

// namedResult is the bare-named-result form: the zero value is produced by the return
// statement itself, with no literal, no new, and no var the caller can see.
func namedResult() (d workflow.DAG) { return }

// TWO KNOWN FORMS ARE DELIBERATELY ABSENT ABOVE, and the reason is worth recording
// because it says something about the instrument rather than about the class.
//
// A map VALUE read (m["k"] into a local) and a receive from a closed channel are both
// legal external zero-value forms, and both were measured refusing. They are not here
// because `go vet`'s copylocks rejects them at the gate — DAG contains a sync.RWMutex,
// so those two reads are "assignment copies lock value" even though the mutex being
// copied is the ZERO one and nothing is locked. Keeping them would red a required gate
// to restate a property the surviving rows already carry, so they were dropped rather
// than the harness contorted around them.
//
// THE MEASURED ASYMMETRY IS THE INTERESTING PART: copylocks flags those two and does
// NOT flag the bare-named-result or generic-zero rows, which copy a DAG value just as
// surely — the copy simply arrives through a function return. So copylocks is a partial
// instrument here, exactly as it is partial against reflect. Do not read "vet is clean"
// as "no value copy occurs"; it means "no value copy in a shape this analyzer matches."
//
// Their absence changes nothing about the claim, which was never a count.
