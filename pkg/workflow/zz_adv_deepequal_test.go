package workflow

import (
	"reflect"
	"testing"
	"time"
)

// ============================================================================
// ADVERSARIAL ARM — independent instrument, value_depth_deepequal.go @ 9c37a3b
// Nothing in this file is derived from the subject's own test suite.
// ============================================================================

// advChain is the minimal 2-frames-per-link spine: *advChain -> struct -> field.
type advChain struct{ Next *advChain }

func mkAdvChain(links int) *advChain {
	head := &advChain{}
	cur := head
	for i := 1; i < links; i++ {
		cur.Next = &advChain{}
		cur = cur.Next
	}
	return head
}

// ---------------------------------------------------------------------------
// A1 — `exceeded` is not checked on the LEAF branch or the GREY branch.
//
// deepEqualFrames bound-checks in exactly two places: before PUSHING a
// descending child (`len(stack)+1 > bound`) and on a MEMO hit
// (`len(stack)+d > bound`). A child that does NOT descend (a leaf) and a child
// that is GREY both take `top.best = 1` with no bound check at all — so each
// can sit at depth bound+1 and the function still reports exceeded=false.
//
// ORACLE: the function's own postcondition. Its doc says it "returns the
// deepest recursion reflect.DeepEqual would perform over v, and whether it
// exceeded bound". frames > bound with exceeded == false contradicts that
// literally, in its own return values.
// ---------------------------------------------------------------------------
func TestADV_A1_ExceededIsUncheckedOnLeafAndGrey(t *testing.T) {
	t.Run("LEAF at bound+1", func(t *testing.T) {
		const bound = 64
		// 2 frames per link + 1 terminal nil-pointer leaf = 2k+1.
		// k = bound/2 gives exactly bound+1 frames.
		v := mkAdvChain(bound / 2)
		frames, cyc, exceeded := deepEqualFrames(v, bound)
		t.Logf("MEASURED: bound=%d frames=%d cyclic=%v exceeded=%v", bound, frames, cyc, exceeded)

		if frames > bound && !exceeded {
			t.Errorf("SELF-CONTRADICTORY RETURN: frames=%d > bound=%d yet exceeded=false. "+
				"The leaf branch (`if !deepEqualDescends(child) { top.best = 1 }`) adds a frame "+
				"with no bound check, so the walk overshoots its own bound by one and does not "+
				"say so", frames, bound)
		}
	})

	t.Run("GREY at bound+1", func(t *testing.T) {
		// A ring whose grey hit lands exactly at the frame after the bound.
		const bound = 64
		ring := mkAdvChain(bound / 2)
		last := ring
		for last.Next != nil {
			last = last.Next
		}
		last.Next = ring // close the ring
		frames, cyc, exceeded := deepEqualFrames(ring, bound)
		t.Logf("MEASURED: bound=%d frames=%d cyclic=%v exceeded=%v", bound, frames, cyc, exceeded)
		if frames > bound && !exceeded {
			t.Errorf("SELF-CONTRADICTORY RETURN: frames=%d > bound=%d yet exceeded=false (grey branch)",
				frames, bound)
		}
	})

	// The same overshoot at the REAL constant, because that is the number the
	// acceptance decision is made on.
	t.Run("at maxWalkFrames the accept ceiling is one frame past the bound", func(t *testing.T) {
		v := mkAdvChain(maxWalkFrames / 2)
		frames, cyc, exceeded := deepEqualFrames(v, maxWalkFrames)
		t.Logf("MEASURED: maxWalkFrames=%d frames=%d cyclic=%v exceeded=%v",
			maxWalkFrames, frames, cyc, exceeded)
		if frames > maxWalkFrames && !exceeded {
			t.Errorf("ACCEPT CEILING IS %d FRAMES, NOT %d: deepEqualFrames reports exceeded=false "+
				"for a value whose own reported frame count is over the bound", frames, maxWalkFrames)
		}
	})
}

// ---------------------------------------------------------------------------
// A2 — FALSE REFUSAL CATALOG (P4).
//
// ORACLE: run the real reflect.DeepEqual. If it answers in microseconds, a
// refusal is a false refusal by construction — no modelling involved.
//
// Every shape here is small and provably terminating for DeepEqual (verified
// by running it first, under a timeout), so running it cannot kill the test
// process.
// ---------------------------------------------------------------------------

type advCyc struct{ Next *advCyc }

// mkAdvCycAcyclicChain builds an ACYCLIC chain of the same type mkAdvCyc produces, so a
// pair of (cyclic, deep-acyclic) can be formed without a type mismatch settling it first.
func mkAdvCycAcyclicChain(links int) *advCyc {
	var head *advCyc
	for i := 0; i < links; i++ {
		head = &advCyc{Next: head}
	}
	return head
}

func mkAdvCyc(n int) *advCyc {
	ns := make([]*advCyc, n)
	for i := range ns {
		ns[i] = &advCyc{}
	}
	for i := range ns {
		ns[i].Next = ns[(i+1)%n]
	}
	return ns[0]
}

type advTagged struct {
	Tag  int
	Next *advTagged
}

func mkAdvTagged(n, tag int) *advTagged {
	ns := make([]*advTagged, n)
	for i := range ns {
		ns[i] = &advTagged{Tag: tag}
	}
	for i := range ns {
		ns[i].Next = ns[(i+1)%n]
	}
	return ns[0]
}

func deepEqualFast(t *testing.T, a, b any) (bool, time.Duration) {
	t.Helper()
	type res struct {
		eq bool
		d  time.Duration
	}
	ch := make(chan res, 1)
	go func() {
		start := time.Now()
		eq := reflect.DeepEqual(a, b)
		ch <- res{eq, time.Since(start)}
	}()
	select {
	case r := <-ch:
		return r.eq, r.d
	case <-time.After(10 * time.Second):
		t.Fatal("precondition failed: reflect.DeepEqual did not terminate quickly on this shape, " +
			"so a refusal would NOT be a false refusal and this case proves nothing")
		return false, 0
	}
}

func TestADV_A2_FalseRefusalCatalog(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		why  string
		// wantRefused INVERTS the case rather than skipping it. Two entries below are
		// ACCEPTED RESIDUALS, ruled by the architect after the engineer refused them with
		// basis: reflect.DeepEqual resolves them DURING recursion, and knowing that
		// requires performing the comparison — the equality re-implementation this design
		// rejects. They stay REFUSED, and this arm now asserts that, so the property keeps
		// running and a change in the behaviour still bites. A t.Skip would stop testing.
		wantRefused bool
	}{
		{
			name: "DIFFERENT TYPES, both cyclic",
			a:    mkAdvCyc(7),
			b:    mkAdvTagged(9, 1),
			why: "reflect.DeepEqual returns false at its FIRST LINE for mismatched types " +
				"(`if v1.Type() != v2.Type()`) — before hard(), before the visited map, before " +
				"any recursion. There is no stack to protect and no lcm to fear",
		},
		{
			name: "SAME type, cyclic, DIFFER AT FIELD 0",
			a:    mkAdvTagged(800, 1),
			b:    mkAdvTagged(801, 2),
			why: "the Tag fields differ at the very first struct field, so deepValueEqual " +
				"returns false at depth ~3 and never reaches the pointer link. The 800/801 " +
				"periods are irrelevant to a comparison that stops before the second field",
			// ACCEPTED RESIDUAL under DEC-M23-AF2-REFUSAL (locked, cross_role). If that
			// decision is ever overturned, one grep on the id finds every arm encoding it.
			// deepValueEqual settles this DURING recursion, not before
			// it, so the guard cannot know without doing the comparison. Conservative and
			// rare — NOT unreachable: this check runs during Execute on in-memory branch
			// results, long before any Save.
			wantRefused: true,
		},
		{
			name: "root MAPS of different length, both cyclic",
			a:    func() any { m := map[string]any{}; m["s"] = m; return m }(),
			b: func() any {
				m := map[string]any{}
				m["s"] = m
				m["extra"] = 1
				return m
			}(),
			why: "`case Map: if v1.Len() != v2.Len() { return false }` fires before the " +
				"UnsafePointer short-circuit and before any element recursion",
		},
		{
			name: "root SLICES of different length, both cyclic",
			a:    func() any { s := make([]any, 1); s[0] = s; return s }(),
			b:    func() any { s := make([]any, 2); s[0] = s; s[1] = s; return s }(),
			why: "`case Slice: if v1.Len() != v2.Len() { return false }` fires before the " +
				"UnsafePointer short-circuit and before any element recursion",
		},
		{
			name: "struct WRAPPING the identical pointer (the file's stated limit)",
			a:    func() any { n := mkAdvCyc(5); return struct{ N *advCyc }{n} }(),
			b:    func() any { n := mkAdvCyc(5); return struct{ N *advCyc }{n} }(),
			why: "documented as a stated limit of root-only sameObject. Listed here to " +
				"score it against the rest of the class, not as a new finding",
			// ACCEPTED RESIDUAL under DEC-M23-AF2-REFUSAL (locked, cross_role).
			// The short-circuit fires at the FIELD, below the root, and
			// reaching it means descending — the comparison again.
			wantRefused: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			eq, d := deepEqualFast(t, tc.a, tc.b)
			err := checkDeepEqualPairDepth(tc.a, tc.b, "adv")
			t.Logf("MEASURED: reflect.DeepEqual -> %v in %v | guard -> %v", eq, d, err)
			if tc.wantRefused {
				if err == nil {
					t.Errorf("ACCEPTED RESIDUAL NO LONGER HOLDS (DEC-M23-AF2-REFUSAL): this pair is now ACCEPTED.\n"+
						"That may be an improvement, but it is a BEHAVIOUR CHANGE to a case the "+
						"architect ruled as an accepted residual, and it must be a decision rather "+
						"than a side effect.\nWHY IT WAS REFUSED: %s", tc.why)
				}
				return
			}
			if err != nil {
				t.Errorf("FALSE REFUSAL. reflect.DeepEqual resolved this pair in %v and the guard "+
					"refused it.\nWHY IT IS SAFE: %s\nREFUSAL TEXT: %v", d, tc.why, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A3 — P5: does the refusal message misdiagnose?
//
// The non-cyclic refusal asserts "<subject> nests more than N levels deep, or
// is cyclic beyond that bound (existing X, new Y)". That branch is reached when
// each side is (cyclic OR exceeded) and NOT both cyclic. So it fires on a pair
// where one side is a SHALLOW cycle — and then prints that side's own frame
// count, which is neither a depth nor "beyond that bound".
// ---------------------------------------------------------------------------
func TestADV_A3_RefusalMessageNumbers(t *testing.T) {
	// SAME TYPE on both sides. The original pair was *advCyc vs *advChain, and the
	// engineer's 116-AF1/AF2 fix now ACCEPTS a type-mismatched pair — correctly, since
	// deepValueEqual returns false at its first line without recursing. That moved this
	// arm's subject out from under it: the precondition stopped holding, not because the
	// message defect was fixed but because the pair no longer reaches the message.
	// Repaired to keep the arm testing its own property. (The repo's filed shape: a fix
	// that moves code changes which arms reach it, and the arm's diff is empty.)
	shallowCyclic := mkAdvCyc(4)                       // cyclic, ~9 frames
	deepAcyclic := mkAdvCycAcyclicChain(maxWalkFrames) // SAME TYPE, acyclic, over the bound

	fa, cycA, exA := deepEqualFrames(shallowCyclic, maxWalkFrames)
	fb, cycB, exB := deepEqualFrames(deepAcyclic, maxWalkFrames)
	t.Logf("side A (4-cycle):     frames=%d cyclic=%v exceeded=%v", fa, cycA, exA)
	t.Logf("side B (deep chain):  frames=%d cyclic=%v exceeded=%v", fb, cycB, exB)

	err := checkDeepEqualPairDepth(shallowCyclic, deepAcyclic, "SUBJ")
	if err == nil {
		t.Fatalf("precondition: this pair must be refused for the message to be under test")
	}
	t.Logf("REFUSAL: %v", err)

	if fa <= maxWalkFrames && !exA {
		msg := err.Error()
		// 🔴 THIS WAS A t.Skip AND IT SKIPPED FOREVER. The engineer's 116-AF4 fix changed
		// the message shape, so the guard clause fired on every run and the arm stopped
		// protecting anything — reading as "not a failure" in every summary line.
		//
		// AND THE BITE COULD NOT SEE IT: seeding the defect back RESTORES the old shape,
		// so the arm runs and reds exactly as reported. A bite proves an arm works in the
		// BROKEN state; it says nothing about whether the arm RUNS in the FIXED state.
		// PASS and SKIP are different outcomes and only one is protection.
		//
		// A shape mismatch is either the property holding or this arm being stale, and the
		// arm cannot tell — which is exactly why it must not decide by skipping.
		if !containsAll(msg, "nests more than") {
			if containsAll(msg, "could be shown to nest within") {
				return // the shipped wording: the misdiagnosis is gone, which is the property
			}
			t.Fatalf("UNRECOGNISED REFUSAL SHAPE — this arm can no longer tell whether the "+
				"property holds or it has gone stale, and it must not decide that by skipping.\n"+
				"Teach it the new shape or delete it deliberately.\nMESSAGE: %s", msg)
		}
		t.Errorf("MISDIAGNOSIS: the refusal asserts the SUBJECT %q \"nests more than %d levels "+
			"deep, or is cyclic beyond that bound\", and in the same sentence prints "+
			"\"existing %d\" — a number that is neither more than %d nor a depth (side A is a "+
			"%d-node CYCLE whose frame count is truncated at the grey edge). One side triggered "+
			"this and the message attributes the property to the pair.",
			"SUBJ", maxWalkFrames, fa, maxWalkFrames, 4)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
