package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 116-AF2 BLOCKER 1 + BLOCKER 2, found by independent review after the unit had already
// shipped a guard at these sites. The arms are written from what the DeepEqual axis MEANS,
// and the first group would all pass on the ENCODER walk — which is the point: the defect
// was a correct guard pointed at the wrong consumer.

type deUnexported struct {
	Val  int
	next *deUnexported //nolint:unused // the whole point: invisible to the encoder, VISIBLE to DeepEqual
}

type deTagged struct {
	Val  int
	Next *deTagged `json:"-"`
}

type deMarshaler struct{ Next *deMarshaler }

func (deMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"opaque"`), nil }

// deBackPointerTree is the ordinary tree whose unexported parent link the ENCODER walk
// must skip and this walk must descend — and, descending it, must not refuse.
type deBackPointerTree struct {
	Kids   []*deBackPointerTree
	parent *deBackPointerTree //nolint:unused // read by reflect, not by name
}

// TestAF2_DeepEqualWalkSeesWhatTheEncoderWalkSkips is BLOCKER 1's regression arm.
//
// THE PROPERTY: for every value, the DeepEqual-axis walk must report at least what
// reflect.DeepEqual will actually recurse. The encoder walk does NOT have that property
// and was never supposed to — it mirrors encoding/json, which skips unexported fields,
// honors `json:"-"`, and stops at Marshaler. deepValueEqual does none of those.
//
// EVERY CASE HERE PASSES ON THE ENCODER WALK, which is why the defect survived: the guard
// returned nil at depth 3 (or 0) for chains DeepEqual walks hundreds deep, ON THE SUCCESS
// PATH of an ordinary idempotent re-apply.
func TestAF2_DeepEqualWalkSeesWhatTheEncoderWalkSkips(t *testing.T) {
	const n = 200
	cases := map[string]func() any{
		"unexported link — encoder skips it, DeepEqual descends": func() any {
			c := &deUnexported{}
			for i := 0; i < n; i++ {
				c = &deUnexported{next: c}
			}
			return c
		},
		`json:"-" link — encoder honors the tag, DeepEqual ignores it`: func() any {
			c := &deTagged{}
			for i := 0; i < n; i++ {
				c = &deTagged{Next: c}
			}
			return c
		},
		"Marshaler type — encoder stops, DeepEqual walks straight through": func() any {
			c := &deMarshaler{}
			for i := 0; i < n; i++ {
				c = &deMarshaler{Next: c}
			}
			return c
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			v := build()
			encoderView, _ := walkFrames(v, 1<<30)
			deepEqualView, _, exceeded := deepEqualFrames(v, 1<<30)
			require.False(t, exceeded)

			t.Logf("MEASURED: encoder walk sees %d frames, DeepEqual walk sees %d (chain of %d)",
				encoderView, deepEqualView, n)

			require.Less(t, encoderView, n,
				"precondition: the ENCODER walk must be blind here, or this arm is not testing the defect")
			require.GreaterOrEqual(t, deepEqualView, n,
				"BLOCKER 1 HAS REGRESSED. The DeepEqual-axis walk reported %d frames for a chain "+
					"reflect.DeepEqual recurses %d deep. It is skipping something deepValueEqual descends — "+
					"deepequal.go has NO export filter, NO tag filter and NO marshaler dispatch, so this walk "+
					"must have none either. Do NOT fix this by widening checkValueDepth: the encoder walk's "+
					"skips are correct for ITS axis and TestAF2_UnexportedFieldsAreNotDescended pins them",
				deepEqualView, n)
		})
	}
}

// TestAF2_DeepEqualWalkIsNotExponentialOnSharing is BLOCKER 2's regression arm.
//
// The encoder walk is a path-enumerating DFS with no memo, so acyclic SHARED substructure
// costs it 2^L while reflect.DeepEqual — which memoizes — is flat. MEASURED before the
// fix: L=24 took 2.7 s against DeepEqual's 14 us, and L=30 did not terminate at all.
//
// THE GUARD WAS ASYMPTOTICALLY WORSE THAN THE THING IT PROTECTS, and this suite's own file
// says why that is not acceptable: trading an unrecoverable crash for a silent hang is not
// a fix.
//
// Sharing is not a cycle, so the "no visited-set is needed" argument — which is about
// cycles and stays true — never covered this.
func TestAF2_DeepEqualWalkIsNotExponentialOnSharing(t *testing.T) {
	for _, levels := range []int{24, 30, 40} {
		t.Run(fmt.Sprintf("levels=%d", levels), func(t *testing.T) {
			var v any = 1
			for i := 0; i < levels; i++ {
				v = []any{v, v} // acyclic, ~levels+1 objects, 2^levels PATHS
			}
			start := time.Now()
			done := make(chan int, 1)
			go func() {
				f, _, _ := deepEqualFrames(v, 1<<30)
				done <- f
			}()
			select {
			case f := <-done:
				t.Logf("MEASURED: %d levels (2^%d paths) walked in %v, depth %d", levels, levels, time.Since(start), f)
			case <-time.After(20 * time.Second):
				t.Fatalf("THE MEMO IS GONE. %d levels of acyclic SHARED substructure did not finish in 20 s — "+
					"the walk is enumerating 2^%d paths again. reflect.DeepEqual does this in microseconds "+
					"because it memoizes; a guard asymptotically worse than the thing it guards is a hang "+
					"where there was a crash", levels, levels)
			}
		})
	}
}

// TestAF2_DeepEqualMemoDistinguishesSliceLength is the arm the architect asked for, and it
// guards the one place the memo could UNDER-report — which is BLOCKER 1's failure mode
// with a different cause.
//
// deepValueEqual may key a slice on its data pointer alone because its map is a
// CYCLE-BREAKER returning true. Ours is a VALUE CACHE returning a subtree DEPTH, and two
// slices over one backing array with different lengths share a data pointer while having
// different depths.
func TestAF2_DeepEqualMemoDistinguishesSliceLength(t *testing.T) {
	back := make([]any, 10)
	var deep any = 1
	for i := 0; i < 40; i++ {
		deep = []any{deep}
	}
	back[9] = deep // reachable ONLY through the longer slice

	short, long := back[0:2], back[0:10]
	require.Equal(t, reflect.ValueOf(short).Pointer(), reflect.ValueOf(long).Pointer(),
		"precondition: both slices must share a data pointer, or the arm is not testing the key")

	// BOTH SLICES IN ONE VALUE, walked in ONE call. The first draft measured them in two
	// SEPARATE calls and was VACUOUS: deepEqualFrames builds a fresh memo per call, so two
	// calls can never collide and dropping the length from the key left the arm GREEN.
	// The hazard is a memo COLLISION, which only exists within a single walk — and the
	// slice is visited first, so a pointer-only key memoizes the SHALLOW answer and then
	// hands it back for the deep one.
	together := []any{short, long}
	dBoth, _, exceeded := deepEqualFrames(together, 1<<30)
	require.False(t, exceeded)

	dShortOnly, _, _ := deepEqualFrames(short, 1<<30)
	dLongOnly, _, _ := deepEqualFrames(long, 1<<30)
	t.Logf("MEASURED: same data pointer; alone len2=%d len10=%d; TOGETHER in one walk=%d",
		dShortOnly, dLongOnly, dBoth)

	require.Greater(t, dLongOnly, dShortOnly, "precondition: the two slices must differ in depth")
	require.GreaterOrEqual(t, dBoth, dLongOnly,
		"THE MEMO IS UNDER-REPORTING. These two slices share a data pointer and have different "+
			"depths, so a memo keyed on the pointer ALONE returns the shallower answer for the deeper "+
			"slice. Under-report is the unsafe direction — it is exactly BLOCKER 1's failure mode with "+
			"a new cause. The key must carry the slice LENGTH")
}

// TestAF2_DeepEqualWalkDoesNotRefuseWhatDeepEqualHandles is the ACCEPTS half, and it is
// the reason this walk does not simply copy checkValueDepth's cycle policy.
//
// MEASURED: reflect.DeepEqual TERMINATES on a map cycle, a slice cycle and a pointer-struct
// cycle, returning true in microseconds. So a cycle is NOT a crash vector on this axis, and
// refusing one would be a false refusal — including on the ordinary tree with an unexported
// parent back-pointer, which THIS walk descends precisely because the encoder walk must not.
func TestAF2_DeepEqualWalkDoesNotRefuseWhatDeepEqualHandles(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	sl := make([]any, 1)
	sl[0] = sl

	root := &deBackPointerTree{}
	cur := root
	for i := 0; i < 8; i++ {
		k := &deBackPointerTree{parent: cur}
		cur.Kids = append(cur.Kids, k)
		cur = k
	}

	for name, v := range map[string]any{
		"map cycle":                     m,
		"slice cycle":                   sl,
		"tree with unexported back-ptr": root,
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan bool, 1)
			go func() { done <- reflect.DeepEqual(v, v) }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("precondition: reflect.DeepEqual must handle this, or refusing it is not a false refusal")
			}
			require.NoError(t, checkDeepEqualPairDepth(v, v, name),
				"FALSE REFUSAL on the DeepEqual axis. reflect.DeepEqual handles this value in microseconds "+
					"— it has its own visited map — so refusing it buys nothing and costs a legal value. "+
					"Cycles are refused on the ENCODER axis for a different reason (json.Marshal's detector "+
					"never runs because the walk trips first); that reason does not transfer here")
		})
	}
}

// TestAF2_TheTwoWalksAreDeliberatelyDifferent states the relation between them, so that
// "just use one walk" reds instead of looking like a simplification.
func TestAF2_TheTwoWalksAreDeliberatelyDifferent(t *testing.T) {
	c := &deUnexported{}
	for i := 0; i < 50; i++ {
		c = &deUnexported{next: c}
	}
	enc, _ := walkFrames(c, 1<<30)
	de, _, _ := deepEqualFrames(c, 1<<30)

	b, err := json.Marshal(c)
	require.NoError(t, err)
	t.Logf("MEASURED: encoder walk %d, DeepEqual walk %d, encoder output %s", enc, de, b)

	require.Less(t, enc, de,
		"THE TWO WALKS HAVE CONVERGED. They bound DIFFERENT consumers with DIFFERENT descent rules "+
			"and must not be merged: encoding/json skips unexported fields (correct — descending them "+
			"would falsely refuse an ordinary back-pointer tree), while reflect.DeepEqual descends "+
			"everything (correct — it really does recurse there). One walk cannot be sound for both, "+
			"and BLOCKER 1 was exactly the attempt")
}

// deCycNode builds a cycle of an exact length, so two operands can be given COPRIME
// periods — the shape that breaks the inherited "one side is enough" proof.
type deCycNode struct{ Next *deCycNode }

func mkDECycle(n int) *deCycNode {
	ns := make([]*deCycNode, n)
	for i := range ns {
		ns[i] = &deCycNode{}
	}
	for i := range ns {
		ns[i].Next = ns[(i+1)%n]
	}
	return ns[0]
}

// TestAF2_MismatchedCyclePeriodsAreRefused is the arm for the defect the ARCHITECT found
// by asking whether an inherited proof transferred — and it did not.
//
// "One side is enough" rests on deepValueEqual descending only where BOTH operands have
// the corresponding element, so depth <= min(d1,d2). That is an argument about STRUCTURAL
// depth and it holds only for ACYCLIC values. For a cyclic pair both structural depths are
// infinite and min() says nothing; what terminates deepValueEqual is its MEMO, and the
// memo matches on a repeated PAIR rather than a repeated node.
//
// So with cycle lengths c1 and c2 the pair does not repeat until lcm(c1,c2).
//
// MEASURED in child processes reading exit status: cycles of 800 and 801 — about 1,600
// nodes, a few KB — recurse 640,800 deep and the process DIES, while a single-side walk
// reports 1,601 and the guard accepted. That is BLOCKER 1's failure mode a third time, on
// the axis built to fix it, and no finite single-side number can close it: lcm is
// unbounded in the two lengths.
func TestAF2_MismatchedCyclePeriodsAreRefused(t *testing.T) {
	t.Run("coprime periods are REFUSED", func(t *testing.T) {
		a, b := mkDECycle(800), mkDECycle(801)

		fa, cycA, _ := deepEqualFrames(a, maxWalkFrames)
		fb, cycB, _ := deepEqualFrames(b, maxWalkFrames)
		require.True(t, cycA)
		require.True(t, cycB)
		t.Logf("MEASURED: single-side walks report %d and %d frames; the PAIR recurses lcm(800,801)=640800",
			fa, fb)
		require.Less(t, fa, maxWalkFrames,
			"precondition: each side ALONE must look harmless, or the arm is not testing the gap")

		err := checkDeepEqualPairDepth(a, b, "coprime cycles")
		require.Error(t, err,
			"UNBOUNDED PAIR ACCEPTED. Two cyclic operands with different periods drive "+
				"reflect.DeepEqual to lcm(c1,c2) frames — measured 640,800 for 800 and 801, which "+
				"kills the process. Neither side's own depth bounds that, so accepting on the strength "+
				"of one side is the same defect BLOCKER 1 was")
		require.ErrorIs(t, err, ErrValidation)
		require.Contains(t, err.Error(), "least common multiple",
			"the refusal must say WHY one side cannot bound this, or the next reader will 'simplify' "+
				"it back to a single-side check")
	})

	// The exception that keeps the rule from being a false refusal.
	t.Run("the IDENTICAL object is still accepted", func(t *testing.T) {
		a := mkDECycle(800)
		require.NoError(t, checkDeepEqualPairDepth(a, a, "same object"),
			"FALSE REFUSAL: with a == b every descent step has the pair (x,x), so the PAIR repeats "+
				"exactly when the node does and deepValueEqual's memo cuts at the value's own cycle. "+
				"Refusing here would reject a comparison that terminates immediately")
	})

	// And the acyclic side still does the bounding when there is one.
	t.Run("one acyclic side still bounds the pair", func(t *testing.T) {
		cyclic := mkDECycle(800)
		acyclic := &deCycNode{Next: &deCycNode{}}
		require.NoError(t, checkDeepEqualPairDepth(acyclic, cyclic, "mixed"),
			"an ACYCLIC operand bounds the recursion whatever the other side does — it runs out of "+
				"structure first. Refusing here would discard the proof that does transfer")
	})
}

// TestAF2_ReComputedVsReReferencedCyclicResults states the ACCEPTANCE-BOUNDARY change in
// the distinction a consumer actually needs, rather than in terms of cycle periods.
//
// The idempotent re-apply splits in two, and only one half is affected:
//
//	RE-REFERENCED — the child hands back the object it was given. Identical operands, so
//	                deepequal.go short-circuits at depth 1. ACCEPTED, unchanged.
//	RE-COMPUTED   — the child rebuilds an equal value. Distinct objects, so if both are
//	                cyclic the comparison runs to lcm of the two periods. REFUSED.
//
// Before AF2 the re-computed case either crashed or terminated only if the periods
// happened to match.
//
// 🔴 116-AF9, SIXTH INSTANCE — found by grepping the PHRASE repo-wide rather than fixing
// the site that was reported. The sentence that stood here was the same one that shipped in
// checkDeepEqualPairDepth's comment, verbatim: "the value was already refused at Save on the
// encoder axis, so what changed is WHERE the refusal happens, not whether one does." It is
// false. The encoder walk skips exactly the links that make deepValueEqual see depth, so the
// two guards are complementary BY CONSTRUCTION — MEASURED: a 5-node ring whose link carries
// `json:"-"` returns nil from checkValueDepth, saves through JSONFileStore, and reads back
// as map[Val:0]. The refusal is NEW, not relocated. And for an EQUAL pair it replaces a nil,
// so it costs a run that would have succeeded, not merely a mislabelled error.
func TestAF2_ReComputedVsReReferencedCyclicResults(t *testing.T) {
	t.Run("RE-REFERENCED — same object, still accepted", func(t *testing.T) {
		v := mkDECycle(500)
		require.NoError(t, checkDeepEqualPairDepth(v, v, "re-referenced"),
			"a child that hands back the object it was given must still be accepted: the operands "+
				"are identical, so reflect.DeepEqual returns at depth 1 via its UnsafePointer "+
				"short-circuit and never recurses at all")
	})

	t.Run("RE-COMPUTED — equal shape, distinct objects, refused", func(t *testing.T) {
		a, b := mkDECycle(500), mkDECycle(500)
		require.True(t, reflect.DeepEqual(a, b), "precondition: the two values ARE equal")

		err := checkDeepEqualPairDepth(a, b, "re-computed")
		require.Error(t, err,
			"ACCEPTANCE-BOUNDARY CHANGE: a re-computed cyclic result is refused. This arm exists so "+
				"the change is a decision on the record rather than something a consumer discovers. "+
				"If this ever starts passing, the boundary moved and that is a product call")
		require.ErrorIs(t, err, ErrValidation)
	})

	t.Run("the over-refusal is deliberate — equal periods are bounded and refused anyway", func(t *testing.T) {
		a, b := mkDECycle(500), mkDECycle(500)
		require.Error(t, checkDeepEqualPairDepth(a, b, "equal periods"),
			"equal periods give lcm = the period, which IS bounded — and they are refused anyway, "+
				"because lcm across multiple cycles per value is not lcm(max,max) (4,6 vs 9 gives 36, "+
				"not 18) and a bound I cannot defend is worse than a loud error")
	})
}

type deM1Chain struct {
	Val  int
	next *deM1Chain //nolint:unused // reached by reflect, not by name
}

type deM2Inner struct{ N *deM2Inner }
type deM2Holder struct{ N *deM2Inner }

// TestAF2_IdenticalObjectIsAcceptedUnconditionally is review #2's MAJOR 1 — a FALSE
// REFUSAL on the idempotent re-apply success path, introduced by this guard.
//
// The identical-object accept had been gated on `cycA && cycB`. deepValueEqual's
// short-circuit is not conditional on anything: case Pointer / Map / Slice each OPEN with
// `if v1.UnsafePointer() == v2.UnsafePointer() { return true }`. So an ACYCLIC value over
// the bound — legal and persistable, because the ENCODER walk skips its unexported link —
// was refused for a comparison that returns at depth 1.
func TestAF2_IdenticalObjectIsAcceptedUnconditionally(t *testing.T) {
	c := &deM1Chain{}
	for i := 0; i < 40000; i++ {
		c = &deM1Chain{next: c}
	}
	require.NoError(t, checkValueDepth(c, "encoder axis"),
		"precondition: this value is LEGAL on the encoder axis — the unexported link makes it "+
			"depth 3 there — so refusing it on the comparator axis is a regression, not a tightening")
	_, cyc, exceeded := deepEqualFrames(c, maxWalkFrames)
	require.False(t, cyc, "precondition: ACYCLIC, so the old `cycA && cycB` gate could not fire")
	require.True(t, exceeded, "precondition: over the bound, so the depth rule would refuse it")

	require.NoError(t, checkDeepEqualPairDepth(c, c, "re-referenced"),
		"FALSE REFUSAL. reflect.DeepEqual returns TRUE at depth 1 for identical operands via its "+
			"UnsafePointer short-circuit, whatever the depth and whether or not the value is cyclic. "+
			"The identical-object accept must sit ABOVE the cyclicity decision, not behind it")
}

// TestAF2_TheRefusalDoesNotClaimTwoPeriodsItNeverMeasured is review #2's MAJOR 2.
//
// sameObject models identity at the ROOT only, while deepValueEqual short-circuits at
// EVERY level where the references coincide. A struct wrapping an identical pointer is
// therefore refused — conservative and accepted — but the message must not diagnose it as
// two cycles of different lengths, because there is ONE cycle referenced twice and no
// period was ever measured.
func TestAF2_TheRefusalDoesNotClaimTwoPeriodsItNeverMeasured(t *testing.T) {
	n := &deM2Inner{}
	n.N = &deM2Inner{N: n}
	h := deM2Holder{N: n}
	var a, b any = h, h

	require.True(t, reflect.DeepEqual(a, b), "precondition: the stdlib short-circuits below the root")
	require.False(t, sameObject(a, b), "precondition: sameObject is root-only and answers NO here")

	err := checkDeepEqualPairDepth(a, b, "holder")
	require.Error(t, err, "the conservative refusal is accepted; only its WORDING was the finding")
	require.Contains(t, err.Error(), "could not be shown to be the same object",
		"the refusal must say what was actually established — that identity could not be shown — "+
			"rather than asserting two cycle periods nothing measured. A refusal that misdiagnoses "+
			"is worse than one that says less")
	require.NotContains(t, err.Error(), "runs to the least common multiple",
		"the message must not state the lcm as a fact about THIS value; it may only offer it as the "+
			"bound that cannot be ruled out")
}
