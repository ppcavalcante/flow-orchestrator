package workflow

import (
	"fmt"
	"reflect"
)

// 116-AF2 BLOCKER 1. checkValueDepth implements the ENCODER's descent rules and was
// deployed to bound reflect.DeepEqual, whose rules are STRICTLY WIDER. That is the same
// defect this whole unit is about — a bound validated against one consumer, restated as
// sound — one level in.
//
// MEASURED, 200-deep chains, the guard returning nil on every one:
//
//	struct{Val int; next *T}          walkFrames 3   json.Marshal {"Val":0}   DeepEqual dies
//	struct{Val int; Next *T json:"-"} walkFrames 3   json.Marshal {"Val":0}   DeepEqual dies
//	struct{Next *T} + MarshalJSON     walkFrames 0   json.Marshal "opaque"    DeepEqual dies
//
// deepValueEqual has no export filter, no tag filter and no marshaler dispatch: it walks
// every field of every struct. The three things checkValueDepth is DELIBERATELY BUILT TO
// SKIP — and those skips are correct for the encoder axis, which is why the answer is a
// second guard rather than a widened first one. Two callees, two bounds; the same lesson
// checkJSONDepth vs checkValueDepth already teaches, one level in.
//
// THIS WALK IS SIMPLER THAN THE ENCODER'S, AND THAT IS THE POINT. checkValueDepth is
// complicated because it must mirror encoding/json's typeFields — which is exactly what
// let BLOCKER 1 happen, and exactly what MAJOR 3 (typeFields drops colliding names) has
// since shown it still does not mirror completely. This one models nothing: it descends
// everything, so neither defect can occur in it.

// deKey identifies a node for the memo. It is NOT the same key deepValueEqual uses, and
// the difference is load-bearing.
//
// deepValueEqual keys visit{addr1, addr2, typ} and can key a slice on its data pointer
// alone, because its map is a CYCLE-BREAKER: re-arriving at the same pair means a cycle
// and it returns true. Ours is a VALUE CACHE — it returns a subtree DEPTH — and the same
// key is not sufficient for that:
//
//	MEASURED: back := make([]any, 10) with a 40-deep value at back[9].
//	          back[0:2] has depth 2. back[0:10] has depth 83. Same data pointer.
//	          Keyed on the pointer alone the memo returns 2 for the longer slice —
//	          a 41x UNDER-report, which is BLOCKER 1's failure mode with a new cause.
//
// So a slice is keyed on (data pointer, len). An array is not keyed at all: it is an
// inline value, so two arrays are never the same object.
type deKey struct {
	ptr uintptr
	typ reflect.Type
	n   int // slice length; 0 for other kinds
}

// deFrame is one open node plus a cursor and the deepest child seen so far. Subtree depth
// is computed in POST-ORDER — a node's depth is known only once its children are done —
// which is what makes the memo possible at all.
type deFrame struct {
	v    reflect.Value
	i    int
	iter *reflect.MapIter
	best int // deepest child subtree seen so far
	key  deKey
	hasK bool
}

// checkDeepEqualPairDepth refuses a PAIR whose comparison by reflect.DeepEqual would
// exhaust the goroutine stack.
//
// IT TAKES BOTH VALUES, and that is the correction to a proof that did not transfer. The
// inherited "one side is enough" argument — depth <= min(d1,d2), so bounding one bounds
// the recursion — is about STRUCTURAL depth and holds only for ACYCLIC values. For a
// cyclic pair both structural depths are infinite and min() says nothing; what terminates
// deepValueEqual is its memo, and that memo matches on a repeated PAIR.
//
// Shares checkValueDepth's bound: DeepEqual costs ~461 bytes of stack per frame,
// shape-invariantly, so maxWalkFrames needs ~15 MB — inside the documented 32 MiB minimum.
//
// The subject names the value, never formats it: formatting a value in a refusal re-enters
// the AF4a crash on the very value being refused.
func checkDeepEqualPairDepth(a, b any, subject string) error {
	// 🔴 IDENTICAL OBJECTS FIRST, UNCONDITIONALLY — and the gate this replaced was a
	// REGRESSION found by review #2. It sat behind `cycA && cycB`, but deepValueEqual's
	// short-circuit is not conditional on anything: `case Pointer:`, `case Map:` and
	// `case Slice:` each OPEN with
	//     if v1.UnsafePointer() == v2.UnsafePointer() { return true }
	// so an identical operand returns at depth ONE whether or not it is cyclic.
	//
	// EXECUTED: a 40,000-long acyclic chain behind an unexported field —
	// walkFrames 3, checkValueDepth nil, so LEGAL AND PERSISTABLE TODAY —
	// DeepEqual(v,v) true in 4.2 us, and the gated version REFUSED it. A false refusal on
	// the idempotent re-apply success path, which is the exact path this guard exists for.
	//
	// a91706b had already written down WHY this is a defect ("an identical operand returns
	// at depth ONE, never reaching the memo at all") while leaving the gate in place. The
	// reason was recorded and not acted on.
	//
	// Hoisting also removes two walks from the common re-reference path.
	if sameObject(a, b) {
		return nil
	}

	// 🔴 AND THE REST OF THE CLASS. Review #2's MAJOR 1 was "the guard pre-empts a cheaper,
	// more accurate check", and hoisting sameObject fixed ONE disqualifier rather than the
	// class it belonged to. The arm-b pass found the others.
	//
	// THE PROPERTY: the guard must not refuse a pair that deepValueEqual disqualifies
	// BEFORE IT RECURSES. Such a pair has no stack to protect — there is nothing to bound.
	//
	// Everything below is read from $GOROOT/src/reflect/deepequal.go and sits AHEAD of
	// hard(), the visited map, and any recursive call:
	//
	//	v1.Type() != v2.Type()                  the first line after the validity check
	//	case Map:   v1.IsNil() != v2.IsNil()    both before the UnsafePointer short-circuit
	//	            v1.Len()   != v2.Len()      and before any element recursion
	//	case Slice: same two
	//
	// A worked consequence, since it is the HIGH severity one: a type-mismatched pair used
	// to be refused with ErrValidation, so the call site returned that INSTEAD of
	// ErrFanOutResultKeyCollision — a public error-contract regression. Accepting the pair
	// here lets the call site reach its own comparison and report the collision it found.
	// The remedy is in the GUARD rather than at the two call sites deliberately: the call
	// sites cannot answer "would DeepEqual disqualify this?" without duplicating this
	// reasoning, and a duplicated rule is the shape that produced BLOCKER 1.
	if deepEqualSettlesWithoutRecursing(a, b) {
		return nil
	}

	_, cycA, exA := deepEqualFrames(a, maxWalkFrames)
	_, cycB, exB := deepEqualFrames(b, maxWalkFrames)

	// ONE ACYCLIC SIDE WITHIN THE BOUND IS ENOUGH. deepValueEqual descends only where
	// BOTH operands have the corresponding element, so the recursion is bounded by the
	// SHALLOWER of the two — and an acyclic value's depth is its own bound.
	if !cycA && !exA {
		return nil
	}
	if !cycB && !exB {
		return nil
	}

	// 🔴 BOTH CYCLIC: one side cannot bound the pair at all, and this is measured, not
	// argued. deepValueEqual's memo cuts on a repeated PAIR, not a repeated node, so with
	// cycle lengths c1 and c2 the pair does not repeat until lcm(c1,c2). MEASURED: two
	// cycles of length 800 and 801 — about 1,600 nodes, a few KB — drive it 640,800
	// frames deep and the process dies, while a single-side walk reports 1,601.
	//
	// So the depth of neither operand bounds the comparison, and no finite single-side
	// number can: lcm is unbounded in the two lengths. Refusing is the only sound answer.
	// 🔴 ACCEPTANCE-BOUNDARY CHANGE, and it deserves the name a consumer will look for.
	//
	// Two structurally-EQUAL but DISTINCT cyclic objects are refused here. That is the
	// idempotent re-apply path WHEN THE CHILD RE-COMPUTES ITS RESULT RATHER THAN
	// RE-REFERENCING IT — same key, same shape, different objects. A child that returns
	// the value it was given is unaffected (identical object, above); a child that rebuilds
	// an equal cyclic value is now refused.
	//
	//   before AF2: crash, or terminate only if the two periods happened to match
	//   now:        a loud ErrValidation
	//
	// 116-AF9: THE MITIGATING ARGUMENT THAT STOOD HERE WAS FALSE, and the correction is not
	// a softer wording — it is the opposite conclusion. It read: "a cyclic result was ALREADY
	// refused at Save on the encoder axis, so such a value was never going to persist; what
	// changes is WHERE the refusal happens, not whether one does."
	//
	// The encoder walk SKIPS EXACTLY THE LINKS that make deepValueEqual see depth (that is
	// this file's opening measurement, applied to itself one level down), so the two guards
	// cover COMPLEMENTARY shape families BY CONSTRUCTION. "The encoder already refused it" is
	// therefore not merely unproven — it is systematically inapplicable to the class that
	// trips this branch. EXECUTED, 5-node rings, each guard NAMED:
	//
	//	link `json:"-"`    checkValueDepth nil  ·  JSONFileStore.Save nil, and the value READS
	//	                   BACK as map[Val:0] — it persisted
	//	link exported      checkValueDepth REFUSES ("nests more than 32768 levels deep, or is
	//	                   cyclic"), so that value never reaches Save at all
	//	two hidden rings   checkDeepEqualPairDepth REFUSES, right here
	//
	// So the value this branch refuses is one the encoder ACCEPTS AND STORES. The refusal is
	// NEW, not relocated.
	//
	// AND THE RESIDUAL IS LARGER THAN THE OLD TEXT IMPLIED, not smaller. Two distinct but
	// EQUAL rings share a period, so the pair repeats immediately and deepValueEqual answers
	// without difficulty — MEASURED: true in 79.9 us. The call site would have returned nil,
	// the idempotent re-apply SUCCESS path. It now returns ErrValidation. This branch does not
	// only mislabel an error; on an equal pair it fails a run that would have succeeded.
	//
	// The trade is still taken — see the over-refusal note below for why a defensible bound is
	// not available — but it is taken with that price named. Understating it was the third
	// instance in this phase of a statement true on one axis restated about another, and the
	// first one that shipped to a consumer.
	//
	// It also over-refuses on purpose. Two distinct cycles of the SAME period are bounded
	// (lcm = the period) and are refused anyway, because lcm across MULTIPLE cycles per
	// value is not lcm(max, max) — 4 and 6 against 9 gives 36, not 18 — and shipping a
	// bound I cannot defend is worse than a loud error. Same asymmetry that set the
	// constant low: a refusal is recoverable, a host-process kill is not.
	if cycA && cycB {
		// THE MESSAGE SAYS WHAT WE KNOW, NOT WHAT WE GUESS. Review #2 found the previous
		// wording asserting "the least common multiple of the two cycle lengths" for a
		// value that has ONE cycle referenced twice — sameObject only tests the ROOT, and
		// deepValueEqual short-circuits at EVERY level where the references coincide. We
		// did not measure two periods and must not claim them. Same ruling as the
		// cycle-or-depth message: a refusal that misdiagnoses is worse than one that
		// says less.
		// 116-AF5: the parenthetical used to say "recurse 640,800 deep" beside a bound
		// denominated in WALK FRAMES. 640,800 is the lcm of the two ring LENGTHS; the
		// measured frame depth is 1,281,601 — exactly 2x, because each ring hop costs a
		// pointer frame AND a struct frame. This project's own depth-vs-frames trap, in
		// the string written to explain that axis. The unit is now in the wording.
		return fmt.Errorf("%w: %s — both the existing and the new value are cyclic and could not be shown to be the same object, so the comparison cannot be bounded from either side alone: reflect.DeepEqual's cycle detection matches on the PAIR, and the pair can run as far as the least common multiple of the two cycle LENGTHS, which costs a multiple of that in walk FRAMES (measured: rings of 800 and 801 nodes give lcm 640,800 and 1,281,601 walk frames, and exhaust the stack)",
			ErrValidation, subject)
	}
	// "or is cyclic" because the walk may have stopped at the bound BEFORE reaching the
	// grey node that would have proved a cycle — EXECUTED: a 32,768-node ring reports
	// cyclic=false, exceeded=true. Claiming depth there would be the same confident
	// misdiagnosis the cyclic branch above was corrected for.
	// 116-AF4: THE NUMBERS ARE GONE, and their absence is the fix rather than a loss.
	// This message used to print "existing %d, new %d" as though both were measured
	// depths. Neither reliably is: a side that hit a grey node reports a TRUNCATED count
	// (9 for a value nowhere near 9 deep), and a side that hit the bound reports
	// maxWalkFrames itself — a SATURATION CONSTANT, not a measurement. Printing them
	// asserted about the PAIR what only one side triggered.
	//
	// Same class as the misdiagnosis corrected one branch over: a refusal describes what
	// the guard OBSERVED, not what it inferred. Here what it observed is that NEITHER side
	// could be shown bounded, and that is all it says.
	return fmt.Errorf("%w: %s — neither the existing nor the new value could be shown to nest within %d walk frames, so reflect.DeepEqual cannot be bounded on this pair without risking a stack overflow (a fatal, unrecoverable error, not a panic)",
		ErrValidation, subject, maxWalkFrames)
}

// deepEqualFrames returns the deepest recursion reflect.DeepEqual would perform over v,
// and whether it exceeded bound.
//
// ── CYCLES ARE NOT REFUSED, AND THAT IS DELIBERATE ──
//
// checkValueDepth refuses a cycle, because json.Marshal's cycle detector never runs — the
// walk trips first.
//
// 116-AF9: the SECOND reason that used to stand here — "and because a cyclic value is not
// durably representable anyway" — is FALSE, and it is the same defect corrected in
// checkDeepEqualPairDepth above, one function over, from the same premise. checkValueDepth
// refuses only the cycles IT CAN SEE. A cycle closed through a `json:"-"` link is invisible
// to the encoder walk: MEASURED, a 5-node such ring returns nil from checkValueDepth, saves
// through JSONFileStore, and reads back as map[Val:0]. Durably representable — just not
// durably representable AS A CYCLE, which is a different claim and the only true one.
//
// deepValueEqual is DIFFERENT: it has its own visited map and MEASURED, it TERMINATES on a
// map cycle, a slice cycle and a pointer-struct cycle, returning true in microseconds.
//
// So a cycle is not a crash vector on this axis, and refusing one here would be a FALSE
// REFUSAL — on, among other things, the ordinary `struct{ Kids []*Node; parent *Node }`
// tree, whose unexported back-pointer this walk (unlike the encoder's) does descend.
// Instead a cycle contributes one frame and stops, mirroring what deepValueEqual does when
// its visited map hits.
//
// ── THE MEMO, AND WHY IT IS SOUND ON ONE SIDE ──
//
// BLOCKER 2: the encoder walk is a path-enumerating DFS with no memo, so acyclic SHARED
// substructure is exponential — MEASURED, `v = []any{v,v}` x24 takes 2.7 s while
// DeepEqual takes 14 us, because DeepEqual memoizes and the walk does not. The walk was
// asymptotically worse than the thing it protects.
//
// We bound ONE value where deepValueEqual keys on a PAIR, so this memo is strictly more
// aggressive: it may skip a subtree DeepEqual would re-descend. That is sound for a DEPTH
// bound — re-traversal costs TIME, not recursion depth — and it can only make this walk
// report the same or MORE than DeepEqual's true stack, which is the safe direction.
//
// Memoizing only the kinds that can be SHARED is sufficient, not a heuristic: sharing
// requires indirection, and in Go that is a pointer, slice, map or interface. Struct
// fields and array elements are inline, so "sharing" through them is a copy. That is the
// same reasoning deepValueEqual's own hard() rests on, and its comment says so.
//
// uintptr identity assumes a non-moving collector — the identical assumption
// deepequal.go states in its own visited-map code, in this same standard library.
func deepEqualFrames(v any, bound int) (frames int, cyclic bool, exceeded bool) {
	root := reflect.ValueOf(v)
	if !deepEqualDescends(root) {
		if root.IsValid() {
			return 1, false, 1 > bound
		}
		return 0, false, false
	}

	memo := map[deKey]int{}
	onStack := map[deKey]bool{}
	stack := []deFrame{newDEFrame(root)}
	if stack[0].hasK {
		onStack[stack[0].key] = true
	}
	result := 0

	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		child, ok := top.next()
		if !ok {
			// POST-ORDER: this node is complete, so its subtree depth is known.
			d := 1 + top.best
			if top.hasK {
				memo[top.key] = d
				delete(onStack, top.key)
			}
			stack[len(stack)-1] = deFrame{}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				result = d
				break
			}
			if d > stack[len(stack)-1].best {
				stack[len(stack)-1].best = d
			}
			continue
		}

		k, hasK := deKeyOf(child)
		// 116-AF3: EVERY branch that charges a frame must check the bound. The leaf and
		// grey branches each raise `best` by one — which is a frame — while only the push
		// and memo-hit paths tested it, so `links=16384` returned frames=32769 with
		// exceeded=false at bound 32768: a self-contradictory return that breaks this
		// function's own postcondition (`frames <= bound || exceeded`).
		//
		// It cannot crash — one frame is ~922 B against a 32 MiB floor — and the severity
		// is stated that way rather than inflated. It is fixed because the postcondition is
		// the contract other code reads, and an off-by-one in a bound is this unit's
		// entire subject.
		if len(stack)+1 > bound {
			return bound, cyclic, true
		}
		if hasK && onStack[k] {
			cyclic = true
			// GREY: a cycle. deepValueEqual stops here and returns true, so it costs one
			// frame and no more. Not a refusal — see the doc comment.
			if top.best < 1 {
				top.best = 1
			}
			continue
		}
		if hasK {
			if d, seen := memo[k]; seen {
				// BLACK: known subtree. Its depth adds to the current path without
				// re-walking, which is the whole point of the memo.
				if len(stack)+d > bound {
					return bound, cyclic, true
				}
				if d > top.best {
					top.best = d
				}
				continue
			}
		}
		if !deepEqualDescends(child) {
			if top.best < 1 {
				top.best = 1 // a leaf still costs deepValueEqual a frame
			}
			continue
		}
		if len(stack)+1 > bound {
			return bound, cyclic, true
		}
		f := newDEFrame(child)
		if f.hasK {
			onStack[f.key] = true
		}
		stack = append(stack, f)
	}
	return result, cyclic, false
}

func newDEFrame(v reflect.Value) deFrame {
	k, hasK := deKeyOf(v)
	f := deFrame{v: v, key: k, hasK: hasK}
	if v.Kind() == reflect.Map {
		f.iter = v.MapRange()
	}
	return f
}

// deKeyOf mirrors deepValueEqual's hard(): only a non-nil pointer, map, slice or interface
// can be SHARED, so only those need identity. A slice additionally carries its length —
// see deKey for the measured under-report that omitting it causes.
func deKeyOf(v reflect.Value) (deKey, bool) {
	if !v.IsValid() {
		return deKey{}, false
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Map:
		if v.IsNil() {
			return deKey{}, false
		}
		return deKey{ptr: v.Pointer(), typ: v.Type()}, true
	case reflect.Slice:
		if v.IsNil() {
			return deKey{}, false
		}
		return deKey{ptr: v.Pointer(), typ: v.Type(), n: v.Len()}, true
	default:
		// Interface is deliberately NOT keyed: reflect.Value.Pointer is not defined for
		// it, and its element is keyed instead, which identifies the same object.
		return deKey{}, false
	}
}

// deepEqualDescends mirrors deepValueEqual's switch — and mirrors it by descending
// EVERYTHING, which is the one rule this walk has.
//
// No export filter, no `json:"-"`, no Marshaler stop. Each of those is a correct rule for
// the ENCODER axis and a BLOCKER-1 defect on this one.
func deepEqualDescends(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil()
	case reflect.Map, reflect.Slice:
		return !v.IsNil() && v.Len() > 0
	case reflect.Array:
		return v.Len() > 0
	case reflect.Struct:
		return v.NumField() > 0
	default:
		// deepValueEqual compares scalars, funcs and chans without recursing.
		return false
	}
}

func (f *deFrame) next() (reflect.Value, bool) {
	switch f.v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if f.i > 0 {
			return reflect.Value{}, false
		}
		f.i++
		return f.v.Elem(), true
	case reflect.Map:
		if !f.iter.Next() {
			return reflect.Value{}, false
		}
		return f.iter.Value(), true
	case reflect.Slice, reflect.Array:
		if f.i >= f.v.Len() {
			return reflect.Value{}, false
		}
		f.i++
		return f.v.Index(f.i - 1), true
	case reflect.Struct:
		// EVERY field. This single line is BLOCKER 1's fix.
		if f.i >= f.v.NumField() {
			return reflect.Value{}, false
		}
		f.i++
		return f.v.Field(f.i - 1), true
	default:
		return reflect.Value{}, false
	}
}

// sameObject reports whether two values are the identical object — the case where
// deepValueEqual short-circuits at depth 1 and no bound is needed at all.
//
// 🔴 IT MODELS IDENTITY AT THE ROOT ONLY, and that is a stated limit rather than an
// oversight. deepValueEqual short-circuits at EVERY level where the operands' references
// coincide, so a struct WRAPPING an identical pointer is short-circuited by the stdlib and
// answered NO here — EXECUTED: struct{N *node} over a 5-cycle, DeepEqual true in 1.1 us,
// sameObject false, refused.
//
// Left conservative deliberately: extending it field-wise means re-deriving
// deepValueEqual's own descent a second time, which is the mirroring complexity that
// produced BLOCKER 1. A false NO costs a refusal; a false YES would cost a crash. What was
// NOT acceptable was the refusal MESSAGE claiming two cycle periods it never measured, and
// that is fixed above.
func sameObject(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() || va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		return !va.IsNil() && !vb.IsNil() && va.Pointer() == vb.Pointer()
	default:
		return false
	}
}

// deepEqualSettlesWithoutRecursing reports whether reflect.DeepEqual resolves this pair
// WITHOUT entering its recursion — in which case there is no stack to bound and refusing
// would be a false refusal (116-AF1/AF2).
//
// Every test here is read from $GOROOT/src/reflect/deepequal.go and sits ahead of hard(),
// the visited map and any recursive call. It answers only "settles"; a false NO costs a
// refusal we would have made anyway, so being incomplete is safe and being wrong in the
// other direction is not possible — nothing here can make the guard ACCEPT a pair that
// recurses.
//
// DELIBERATELY NOT MODELLED: pairs that deepValueEqual resolves DURING recursion, such as
// two structs differing at field 0. Knowing that requires performing the comparison, which
// is the equality re-implementation rejected in favour of this design — "bounding is safe
// to get slightly wrong; equality is not". Those pairs are still refused, and that refusal
// is conservative rather than incorrect.
func deepEqualSettlesWithoutRecursing(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() {
		return true // deepValueEqual: `return v1.IsValid() == v2.IsValid()`, no recursion
	}
	if va.Type() != vb.Type() {
		return true // its FIRST line after the validity check
	}
	switch va.Kind() {
	case reflect.Map, reflect.Slice:
		// Both checks precede the UnsafePointer short-circuit and any element recursion.
		return va.IsNil() != vb.IsNil() || va.Len() != vb.Len()
	default:
		return false
	}
}
