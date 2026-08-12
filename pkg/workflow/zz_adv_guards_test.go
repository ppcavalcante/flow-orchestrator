package workflow

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// GUARD ARMS — these PASS on the unmutated subject. Their job is to BITE.
//
// The first mutation matrix over this suite scored a blind row: loosening the
// push bound by 4096 frames (M2), dropping len(stack) from the memo-hit bound
// check (M3), and collapsing the slice memo key's length (M1) were noticed by
// NO arm I had written. The defect-reporting arms could not show it because
// they already FAIL at baseline, so their FAIL carries no signal; and the
// randomized arm's size ladder skipped the band where the bound actually
// decides. These three arms close that row.
// ============================================================================

// advPair fixes DFS ORDER: First is walked before Second, so a memo entry is
// necessarily CREATED under First and CONSUMED under Second.
type advPair struct {
	First  any
	Second any
}

func advChainEndingAt(links int, tail *advChain) *advChain {
	head := mkAdvChain(links)
	cur := head
	for cur.Next != nil {
		cur = cur.Next
	}
	cur.Next = tail
	return head
}

// ---------------------------------------------------------------------------
// G1 — THE ACCEPT EDGE IS WHERE THE BOUND SAYS IT IS.
//
// Binary-searches the largest plain chain the guard accepts and pins it. A
// loosened bound (M2) moves the edge out; a tightened one moves it in. Both
// must red.
//
// The edge is expressed in FRAMES, not links, because frames is the unit
// maxWalkFrames is denominated in.
// ---------------------------------------------------------------------------
func TestADV_G1_AcceptEdgeIsAtTheBound(t *testing.T) {
	accepts := func(links int) bool {
		a, b := mkAdvChain(links), mkAdvChain(links)
		return checkDeepEqualPairDepth(a, b, "edge") == nil
	}
	lo, hi := 1, maxWalkFrames // lo accepts, hi refuses
	if !accepts(lo) {
		t.Fatal("precondition: a 1-link chain must be accepted")
	}
	if accepts(hi) {
		t.Fatal("precondition: a maxWalkFrames-link chain must be refused")
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if accepts(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	edgeFrames, _, _ := deepEqualFrames(mkAdvChain(lo), 1<<30)
	oracle, _ := advPairDepth(mkAdvChain(lo), mkAdvChain(lo), advOracleCap)
	t.Logf("MEASURED: largest ACCEPTED chain = %d links = %d frames (oracle pair depth %d); "+
		"first REFUSED = %d links. Bound = %d", lo, edgeFrames, oracle, hi, maxWalkFrames)

	// +1 is the known, separately-reported overshoot (the leaf/grey branches are
	// not bound-checked). Anything beyond that is the bound not being enforced.
	if edgeFrames > maxWalkFrames+1 {
		t.Errorf("BOUND NOT ENFORCED: the deepest ACCEPTED value is %d frames, %d past "+
			"maxWalkFrames=%d. The guard's whole contract is that an accepted pair stays "+
			"within this bound", edgeFrames, edgeFrames-maxWalkFrames, maxWalkFrames)
	}
	if edgeFrames < maxWalkFrames-1 {
		t.Errorf("BOUND OVER-TIGHTENED: the deepest ACCEPTED value is only %d frames against a "+
			"bound of %d. Every value in between is now falsely refused", edgeFrames, maxWalkFrames)
	}
}

// ---------------------------------------------------------------------------
// G2 — A MEMOISED SUBTREE IS CHARGED AT THE POSITION IT IS CONSUMED AT.
//
// The memo stores a subtree's depth once. Consuming it deeper down must add it
// to the CURRENT stack position (`len(stack)+d > bound`), not judge it in
// isolation (`d > bound`). Both halves here are individually well inside the
// bound; only their SUM is over it, so an arm that forgets the position sees
// nothing wrong.
// ---------------------------------------------------------------------------
func TestADV_G2_MemoIsChargedAtTheConsumingPosition(t *testing.T) {
	const half = 9000 // 2 frames per link => ~18k frames each, ~36k summed
	// 🔴 THE TWO TAILS MUST BE DISTINCT OBJECTS ON THE b SIDE.
	// A first version shared ONE tail per operand. deepValueEqual's memo is keyed
	// on the PAIR, so the pair (tailA,tailB) visited under First short-circuited
	// the SAME pair under Second and the real descent was 18,004 — the shape was
	// never over the bound and the arm proved nothing. Giving b two distinct
	// tails makes (tailA,tailB1) and (tailA,tailB2) different pairs, so the
	// stdlib genuinely descends twice.
	mk := func(distinct bool) any {
		t1 := mkAdvChain(half)
		t2 := t1
		if distinct {
			t2 = mkAdvChain(half)
		}
		return &advPair{
			First:  t1,                         // memo CREATED here, shallow
			Second: advChainEndingAt(half, t2), // memo CONSUMED here, deep
		}
	}
	a, b := mk(false), mk(true)

	tf, _, tex := deepEqualFrames(mkAdvChain(half), maxWalkFrames)
	t.Logf("each half alone: %d frames, exceeded=%v (both must be WELL INSIDE the bound %d)",
		tf, tex, maxWalkFrames)
	if tex {
		t.Fatal("precondition: each half must fit, or the arm proves nothing about the SUM")
	}

	err := checkDeepEqualPairDepth(a, b, "memo-position")
	oracle, capped := advPairDepth(a, b, advOracleCap)
	t.Logf("MEASURED: summed shape -> guard %v, oracle pair depth %d (capped=%v), bound %d",
		err == nil, oracle, capped, maxWalkFrames)

	if oracle <= maxWalkFrames && !capped {
		t.Fatalf("precondition: the SUM must exceed the bound (got %d), or there is nothing to catch", oracle)
	}
	if err == nil {
		t.Errorf("MEMO CHARGED AT THE WRONG POSITION: the guard ACCEPTED a pair whose real "+
			"reflect.DeepEqual descent is %d frames, past the bound of %d. Each half is only %d "+
			"frames, so the subtree depth was judged on its own instead of being added to the "+
			"stack position it was consumed at", oracle, maxWalkFrames, tf)
	}
}

// ---------------------------------------------------------------------------
// G3 — THE SLICE MEMO KEY MUST CARRY ITS LENGTH.
//
// Two slices over ONE backing array share a data pointer. Keyed on the pointer
// alone they collide, and the short one's depth is returned for the long one.
// The subject documents this ("a 41x UNDER-report"); nothing in my suite was
// measuring it, so the mutation that removes the length went unnoticed.
//
// Sized so the collision crosses the ACCEPT BOUNDARY rather than merely
// changing a number.
// ---------------------------------------------------------------------------
func TestADV_G3_SliceMemoKeyCarriesItsLength(t *testing.T) {
	mk := func() any {
		back := make([]any, 4)
		back[0] = nil               // short slice is trivially shallow
		back[3] = mkAdvChain(17000) // ~34001 frames: past the bound on its own
		return &advPair{
			First:  back[0:1], // memo CREATED: same data pointer, length 1
			Second: back[0:4], // memo CONSUMED: same data pointer, length 4
		}
	}
	a, b := mk(), mk()

	err := checkDeepEqualPairDepth(a, b, "slice-memo")
	oracle, capped := advPairDepth(a, b, advOracleCap)
	t.Logf("MEASURED: guard=%v oracle pair depth=%d (capped=%v) bound=%d",
		err == nil, oracle, capped, maxWalkFrames)

	if oracle <= maxWalkFrames && !capped {
		t.Fatalf("precondition: the long slice must be over the bound (got %d)", oracle)
	}
	if err == nil {
		t.Errorf("SLICE MEMO KEY COLLIDED ON LENGTH: back[0:1] and back[0:4] share a data "+
			"pointer, so keying without the length returns the SHORT slice's depth for the LONG "+
			"one. The guard ACCEPTED a pair whose real descent is %d frames against a bound of %d",
			oracle, maxWalkFrames)
	}
}

// advOuter puts an advChain at offset 0, so &o and &o.Inner are the SAME
// ADDRESS with DIFFERENT pointer types.
type advOuter struct{ Inner advChain }

// ---------------------------------------------------------------------------
// G5 — WHY NO ARM BITES THE TYPE CHECK IN sameObject, stated as a measurement
// rather than left as a hole in the matrix.
//
// Removing `va.Type() != vb.Type()` from sameObject is an EQUIVALENT MUTANT for
// the depth contract: deepValueEqual rejects a type mismatch on its first line,
// so such a pair costs exactly one frame whether sameObject waves it through or
// not — and the caller's own reflect.DeepEqual still answers false, so the
// collision verdict is unchanged too.
//
// This arm pins the fact the equivalence RESTS ON. If a future reader "extends"
// sameObject to short-circuit the caller's DeepEqual as well, the type check
// becomes load-bearing and this comment is where they should be standing.
// ---------------------------------------------------------------------------
func TestADV_G5_TypeMismatchCostsExactlyOneFrame(t *testing.T) {
	o := &advOuter{}
	o.Inner = *mkAdvChain(20000)
	p := &o.Inner

	var a, b any = o, p
	t.Logf("addresses: %p vs %p (equal=%v), types %T vs %T", o, p, uintptr(0) == 0, a, b)

	require.False(t, sameObject(a, b),
		"precondition: the type check is what makes this NO; without it the addresses coincide")

	d, capped := advPairDepth(a, b, advOracleCap)
	t.Logf("MEASURED: real reflect.DeepEqual pair depth for a TYPE MISMATCH = %d (capped=%v)", d, capped)
	if capped || d > 1 {
		t.Errorf("THE TYPE CHECK IN sameObject IS LOAD-BEARING AFTER ALL: a type-mismatched pair "+
			"descends %d frames, so waving it through would not be safe", d)
	}
	require.False(t, reflect.DeepEqual(a, b),
		"and the caller's own comparison still answers false, so the collision verdict is unchanged")
}

// ---------------------------------------------------------------------------
// G4 — sameObject's YES is only ever handed to a comparison that ends at
// depth 1.
//
// sameObject compares Pointer() and IGNORES slice LENGTH, so it answers YES for
// back[0:8] and back[0:4] — two different slices. That is safe ONLY because
// deepValueEqual checks `v1.Len() != v2.Len()` BEFORE its UnsafePointer
// short-circuit. This arm states that dependency so it cannot be quietly lost.
// ---------------------------------------------------------------------------
func TestADV_G4_SameObjectYesAlwaysEndsAtDepthOne(t *testing.T) {
	back := make([]any, 8)
	for i := range back {
		back[i] = mkAdvChain(20000) // deep, so a wrong answer here would be a crash
	}
	cases := []struct {
		name string
		a, b any
	}{
		{"identical slice", back[0:8], back[0:8]},
		{"same data pointer, DIFFERENT length", back[0:8], back[0:4]},
		{"identical pointer", func() any { p := mkAdvChain(20000); return p }(), nil},
	}
	// fill the third case's b with the same object
	cases[2].b = cases[2].a

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if !sameObject(tc.a, tc.b) {
				t.Skipf("sameObject answers NO here; nothing to check")
			}
			d, capped := advPairDepth(tc.a, tc.b, advOracleCap)
			t.Logf("MEASURED: sameObject=YES, real pair depth=%d (capped=%v)", d, capped)
			if capped || d > 1 {
				t.Errorf("UNSAFE ACCEPT VIA sameObject: it answered YES and the real "+
					"reflect.DeepEqual descent is %d frames deep (capped=%v), not the depth-1 "+
					"short-circuit that YES is supposed to mean", d, capped)
			}
		})
	}
}
