package workflow

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
)

// maxWalkFrames bounds how deeply nested a VALUE this package will hand to
// json.Marshal (116-AF2).
//
// WHY A SECOND DEPTH BOUND EXISTS AT ALL, since maxJSONNestingDepth already caps
// nesting: they close different axes that happen to share the word "depth".
//
//	maxJSONNestingDepth (10^4)  — the WEDGE axis. Measured on the ENCODED BYTES, after
//	  json.Marshal returns. It stops us writing a document our own reader cannot decode.
//	maxWalkFrames (32,768)      — the CRASH axis. Measured on the VALUE, in WALK FRAMES,
//	  before json.Marshal is called at all. NOT the same unit as the line above: an
//	  interface-carrying value costs TWO walk frames per JSON level, and MORE when
//	  pointer-wrapped — see the FLOOR table below.
//
// json.Marshal is RECURSIVE and has no depth limit of its own, so a deep enough value
// exhausts the goroutine stack INSIDE the encoder — and a Go stack overflow is a `fatal
// error`, not a panic: unrecoverable, no deferred recover runs, the host process dies.
// Every byte-measuring guard in this package is therefore a guard on bytes that only
// exist if the process survived. checkJSONDepth cannot see this vector; it runs on the
// next line and that line is never reached. Measured band on one box below.
//
// ── A FIXED "ABSURDITY CEILING" WAS THE FIRST ANSWER HERE AND IT WAS REFUTED ──
//
// This constant was 10^5, justified as "an absurdity ceiling ~7x below the measured crash
// band, the standing signalMailboxCap = 2^20 gets". BOTH HALVES WERE WRONG.
//
// A crash band is a property of the HOST'S STACK, not of this package. debug.SetMaxStack
// is a plausible hardening choice in an embedding process, and `embeddable` is this
// library's entire pitch. MEASURED, worst-case shape, both recursing classes:
//
//	stack    B=10^5 marshal   B=10^5 DeepEqual      B=32768 marshal   B=32768 DeepEqual
//	 16 MiB      —                 —                    DIES              survives
//	 32 MiB     DIES              DIES                survives           survives
//	 64 MiB   survives          survives              survives           survives
//
// At a 32 MiB stack the old bound PASSED a value that then killed the process — the AF2
// defect surviving the AF2 fix. So a fixed B is not a margin; it is a bet on the host.
//
// ── THE SOUNDNESS CONDITION, which is what actually governs ──
//
//	B x bytes_per_walk_frame  <  effective_stack
//
// UNITS ARE THE TRAP AND THEY COST A WRONG FLOOR ONCE ALREADY. B counts WALK FRAMES, not
// JSON levels, and the two differ by the walk's over-report factor. Per-level costs quoted
// against a `[]any` chain (743 B/level for marshal, 922 for DeepEqual) are per JSON LEVEL
// of a shape that costs TWO walk frames per level. Normalized per walk frame, and measured
// on `type chain []chain`, the worst shape — one walk frame == one JSON level == one
// encoder recursion:
//
//	json.Marshal      ~646 bytes per walk frame
//	reflect.DeepEqual ~465 bytes per walk frame
//
// WHICH CLASS BINDS DEPENDS ON THE UNIT *AND* THE SHAPE, and an earlier version of this
// comment said "MARSHAL binds" unrestricted, which is true only of the shape it was
// measured on. Per walk frame, reflect.DeepEqual is ~461-465 B SHAPE-INVARIANTLY, while
// json.Marshal ranges ~358-646 depending on whether the shape carries interface hops —
// the encoder does not take a full frame on one, deepValueEqual does. So:
//
//	DeepEqual binds on every INTERFACE-CARRYING shape — which is everything WorkflowData
//	holds, since Set(string, any) boxes all of it — and json.Marshal binds on a named
//	recursive type like `type chain []chain`.
//
// B is therefore sized against the MAXIMUM across both classes and all shapes, ~646 B per
// walk frame, which is marshal on the ratio-1.0 shape. That is also why the soundness arm
// is built on that shape rather than on a []any chain.
//
// ── WHY 32768 AND NOT SOMETHING ROUNDER ──
//
// It is squeezed between two measured constraints, and the window is real but not wide:
//
//	FLOOR   B should not refuse a document maxJSONNestingDepth calls LEGAL — and "should
//	        not" rather than "does not" is exact, see below. MEASURED frames per JSON
//	        level, and it is NOT a single number:
//
//	          []any / map[string]any     2.005  -> 20,050 at the ceiling, ACCEPTED (1.63x)
//	          *[]any / *map[string]any   3.005  -> 30,050 at the ceiling, ACCEPTED (1.09x)
//	          **[]any                    4.005  -> 40,050 at the ceiling, REFUSED
//	          ***[]any                   5.005  -> 50,050 at the ceiling, REFUSED
//
//	        🔴 SO THE WALK DOES FALSELY REFUSE SOME LEGAL DOCUMENTS. An earlier version of
//	        this comment claimed "exactly 2.00x on every idiomatic shape", which was true
//	        of the four shapes measured and false as stated. Each POINTER wrapper adds ONE
//	        frame per level while adding ZERO encoded depth, so the ratio is UNBOUNDED in
//	        principle and NO finite B accepts every legal document. The floor is not a
//	        number; it is a shape-dependent family, and B chooses where the line falls.
//
//	        THE RATIO IS NOT RESTRICTED TO WHOLE NUMBERS EITHER: wrapping only alternate
//	        levels measures 2.502x, which is the realistic shape — a host wraps some values
//	        and not others.
//
//	        EMBEDDING INFLATES *ONLY WHEN THE PATH GOES THROUGH THE EMBEDDED FIELD*, and
//	        this comment has now been wrong in BOTH directions. It first listed embedding
//	        beside pointer wrapping as an inflator; the correction then said it is NOT one.
//	        Both were measured, on two DIFFERENT shapes, and neither said which:
//
//	          embed as a SIBLING (the recursion path uses another field)   2.005x
//	          embed ON THE PATH  (the chain runs through the embed)        3.005x
//
//	        A promoted embedded field costs the walk a frame and the encoder NO bracket,
//	        so on the path it inflates exactly like a pointer. As a sibling it never
//	        appears on the depth path at all.
//
//	        THE MITIGATION IS REAL AND IS NOT THE SAME CLAIM: chaining through embedded
//	        fields needs a chain of DISTINCT NAMED TYPES, so its depth is bounded by the
//	        source file rather than by the value. That is why it is not a practical vector
//	        — not because it does not inflate.
//
//	        ── THE COMPLETE SET, derived rather than sampled ──
//
//	        Measured per wrapper as (frames added : brackets added):
//
//	          slice, array, map, struct    2 : 1   <- 1 for the container, 1 for the `any`
//	          pointer over any of them     3 : 1
//	          promoted embedded field      3 : 1
//
//	        Containers are 1:1 with the bracket they emit. TWO families inflate:
//
//	          TRANSPARENT TRAVERSALS — pointer, interface, promoted embed. The walk must
//	          charge for them (or it stops being a cycle bound); the encoder emits nothing.
//	          DROPPED FIELDS — typeFields DISCARDS fields whose json names COLLIDE, and
//	          shadowed embedded fields. The walk descends them in full. MEASURED:
//	          struct{A []any `json:"x"`; B []any `json:"x"`} encodes to `{}`, depth 1,
//	          while the walk reports 4004 on a 2000-deep payload — an unbounded ratio from
//	          a shape with NO wrapper at all.
//
//	        🔴 AN EARLIER VERSION CLAIMED THE FIRST FAMILY WAS THE WHOLE SET — "no fourth
//	        mechanism can exist without encoding/json gaining a new transparent traversal"
//	        — derived by differencing encoderDescends' cases against jsonNestingDepth's
//	        brackets. FALSE, and HOW it is false is the durable part: a DROPPED field is
//	        not a transparent traversal, and the encoder's field SELECTION lives in a THIRD
//	        function that neither of those two is.
//
//	        A DIFFERENCE OF CASE SETS IS ONLY COMPLETE OVER THE FUNCTIONS YOU DIFFERENCED.
//	        The argument that opened this unit — checkJSONDepth takes BYTES, therefore no
//	        call to it can close the crash axis — survives because it differences a TYPE
//	        against physics rather than two implementations. This one differenced two
//	        implementations out of three.
//
//	        WHAT SURVIVES is the property that matters: THE WALK NEVER UNDER-REPORTS
//	        relative to the encoder. Every mechanism found, transparent or dropped, makes
//	        it report MORE, which refuses earlier. Under-reporting would pass a value the
//	        encoder then dies on, and that direction has no known mechanism.
//
//	        B = 32,768 accepts up to ~3.005 frames/level at the legal ceiling — a value
//	        plus one layer of pointer wrapping, which covers the ordinary
//	        `d.Set(k, &slice)`. TestAF2_TheFloorIsAShapeFamilyNotANumber pins that line, so
//	        moving it is a decision rather than a side effect of retuning the constant.
//
//	        HOW REACHABLE THE REFUSED REGION IS — measured, because the first version of
//	        this paragraph guessed and understated it. It said the refused side "needs a
//	        10,000-level structure whose EVERY level is doubly wrapped". FUZZED instead:
//	        6,000 random shapes drawn from a grammar of the ordinary constructors (slice,
//	        map, named struct, and pointers over each) span ratios 2.541-3.292, and 5.6%
//	        of them EXCEED 3.005 — the worst needing ~32,921 frames at the legal ceiling
//	        against a bound of 32,768. So the line is crossed by MIXTURES at ordinary
//	        wrapper density, not only by uniform double wrapping. Still nobody's real
//	        workload at 10^4 levels, but reachable by a mix rather than by construction.
//	CEILING B x 646 < effective_stack. At 32 MiB that is B < ~51,900 (bisected: marshal
//	        survives 50,875, dies by 52,433).
//
// 32,768 sits at 1.64x the 2.005x floor and 0.63x the 32 MiB ceiling, and is a power of
// two because that is how Go grows stacks. The 1.64x is exactly the tolerance for
// over-reporting shapes, and it buys ONE layer of pointer wrapping (3.005x, headroom
// 1.09x) and not two (4.005x, refused). That is the trade, stated as a measurement rather
// than as slack.
//
// RAISING B WOULD NOT FIX THE FAMILY, only move the line: the soundness ceiling at 32 MiB
// is ~52,304, so even B at the very top would still refuse a 5.005x shape at the full
// legal depth, and would do it with ~1.0x soundness headroom. Since the ratio is unbounded
// in principle, the choice is WHERE to cut, not WHETHER.
//
// THE ASYMMETRY IS WHY IT ERRS LOW. Too high is a host-process kill; too low is a loud
// ErrValidation on a legal document. Those are not comparable harms.
//
// ── MINIMUM SUPPORTED EFFECTIVE STACK: 32 MiB, AND IT IS LOAD-BEARING, NOT ADVISORY ──
//
// "Effective" is doing real work here: Go grows stacks by DOUBLING, so the usable limit is
// the largest power of two <= the configured one. MEASURED — SetMaxStack at 8 MiB, 12 MiB
// and 13 MiB all produce the IDENTICAL crash depth (~12,984), and only 16 MiB moves it
// (~26,120). So the reachable effective stacks are 8, 16, 32, 64 ... MiB and NOTHING in
// between; an arithmetic threshold like "12.6 MiB" describes no host that can exist. The
// 1e9 default yields 512 MiB, not 1 GB, and every crash number this package quotes was
// taken against 512 MiB usable.
//
// Per REACHABLE effective stack, worst shape, floor = 20,050 walk frames (the 2.005x
// shape; a pointer-wrapped shape raises the floor further and narrows every window below):
//
//	 8 MiB   ceiling ~13,048  <  floor        ==> NO SOUND B EXISTS AT ALL
//	16 MiB   ceiling ~25,875                  ==> window (20050, 25875], 1.29x — B=32768 DOES NOT FIT
//	32 MiB   ceiling ~52,304                  ==> window (20050, 52304], 2.61x — B=32768 fits
//
// 🔴 THE FIRST ROW IS A BOUNDARY OF THE GUARANTEE, NOT A DOCUMENTATION GAP. At 8 MiB the
// floor and the ceiling have crossed: the crash bound would have to sit BELOW the deepest
// document maxJSONNestingDepth calls legal. The two guards become MUTUALLY UNSATISFIABLE,
// and no choice of B — not this one, not a smaller one — both accepts every legal document
// and prevents the fatal crash. Totality on the depth axis is simply not on offer to a host
// with 8 MiB of effective goroutine stack.
//
// The 16 MiB row is why 32 MiB is the stated minimum rather than 16: a window exists there,
// but it tops out at ~25,875 and B = 32,768 is outside it. A 16 MiB host is unsound with
// the shipped bound even though a smaller bound would have worked.
//
// AND THIS CONTRACT CANNOT BE ENFORCED. Reading the live limit requires calling
// debug.SetMaxStack TWICE — it returns the previous value, so there is no read without a
// write — which is a process-global mutation with a window in which the HOST'S limit is
// wrong. A library must not do that to its host. The construction is legitimate in a TEST,
// which owns its process, and that is where it lives: TestAF2_BoundIsSoundOnThisBox reds on
// a box where this bound is unsound. It reds on a developer's box; it does NOT red in the
// host's process. A host that lowers its stack after our tests ran gets no signal, and the
// failure it gets instead is the fatal crash this guard exists to prevent.
//
// WHY THE WINDOW IS NARROWER THAN ANY REAL DOCUMENT NEEDS, before someone "optimizes" it:
// the FLOOR is set by the INTERFACE-CARRYING shape (2.005x over-report, more when pointer-
// wrapped) and the CEILING by
// the NAMED recursive shape (631 B/frame). Those are different shapes and no single
// document exhibits both. The window is deliberately conservative — sized so that the worst
// shape on each side is covered simultaneously, which no real value can be.
//
// ── AND THE SELF-VERIFYING TEST *IS* AVAILABLE, contrary to what this comment said ──
//
// The claim was: unlike maxJSONNestingDepth there is no stdlib limit to mirror, so the
// self-verifying trick is unavailable. There is no CONSTANT to mirror — but there is a
// CONSTRUCTION. TestAF2_BoundIsSoundOnThisBox builds a value at exactly this depth and
// marshals it in a CHILD PROCESS on the box's live limit, asserting the child survives.
// That self-verifies across architecture, Go version AND stack limit — strictly more than
// maxJSONNestingDepth gets, which only self-verifies across Go versions. On amd64 it
// simply runs and tells the truth; nobody needs to have measured there first.
const maxWalkFrames = 32768

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// checkValueDepth refuses a value nested deeply enough to overflow the stack inside a
// recursive consumer — json.Marshal or reflect.DeepEqual.
//
// ── READ THE SIGNATURE. IT IS THE DISTINCTION, AND IT IS MEANT TO BE UNMISSABLE ──
//
//	checkValueDepth(v any,        subject string) error   <- CRASH axis. Takes a VALUE.
//	checkJSONDepth (encoded []byte, subject string) error <- WEDGE axis. Takes BYTES.
//
// checkJSONDepth takes bytes. Bytes only exist if an encoder already returned. Therefore
// NO call to checkJSONDepth — present, future, or not yet written — can protect against a
// crash inside the encoder that produced its argument. That is complete from the types; it
// needs no site survey and it holds for the eleventh site too.
//
// This is written down because the alternative already happened: seven marshal sites were
// recorded as "COVERED" on the strength of having a checkJSONDepth nearby, and every one
// of them was live on the crash axis. The two guards share the word "depth" and nothing
// else. Keeping them at different TYPES means "every host-value site has a pre-marshal
// walk" is checkable mechanically, and can never be satisfied by adding a checkJSONDepth.
//
// It ADDS to checkJSONDepth; it does not replace it. Neither subsumes the other:
//
//   - this one cannot see a custom MarshalJSON's output, a json.RawMessage's contents,
//     or the levels an ENVELOPE adds — only the encoded bytes show those. The JSON store's
//     signalWire wraps a payload in one more level, so the walk's answer and the encoded
//     document's depth legitimately differ by exactly that. That disagreement is CORRECT
//     and belongs to checkJSONDepth; tuning this walk to compensate for it would leave it
//     answering a third question that is neither axis.
//   - checkJSONDepth cannot see anything at all if the encoder killed the process first.
//
// ── WHAT THIS GUARD IS NOT, and it is a real limit rather than a caveat ──
//
//	The walk is a CRASH-AVOIDANCE bound over a value the host may still mutate. It is
//	not an authority on the encoded document. checkJSONDepth, on the encoded bytes,
//	remains the sole authority for the maxJSONNestingDepth ceiling.
//
// WorkflowData.Set stores the caller's REFERENCE verbatim — no copy — and the mutex
// protects the map, not the objects it points at. A host that retains its alias can deepen
// the structure without taking any lock. So a walk that passes at depth 5 can be followed
// by a marshal that crashes, and NO amount of adjacency between the check and the encode
// closes that. Checking beside the encode buys the narrowest window available, not a
// guarantee. This is the mechanism-level reason the tester's "must ADD, never REPLACE" is
// true, rather than merely a rule to follow.
//
// TWO PROPERTIES ARE LOAD-BEARING AND EACH RULES OUT A REMEDY THAT IS WORSE THAN THE
// DEFECT.
//
// (1) IT IS ITERATIVE. A recursive depth-checker has the identical stack overflow it was
// written to prevent, and would pass every functional test — the fix ships still broken
// and looks fine. The stack here is an explicit FRAME stack (one frame per container
// currently open, each holding a cursor), NOT a worklist of pending children. That
// distinction is not stylistic: a worklist holds the whole frontier, so a value whose
// every level fans out — `m["a"]=m; m["b"]=m; ...` — grows the worklist by (fanout-1)
// per level and exhausts memory before it reaches the bound. A frame stack is O(depth),
// bounded by maxWalkFrames by construction, whatever the fanout.
//
// (2) IT IS NON-TRANSPARENT: EVERY structural link costs a level, including a pointer
// hop, an interface hop, and an embedded field, with no exceptions. This is what makes
// the depth bound double as the CYCLE bound, and it is the whole reason no visited-set
// is needed: a cycle necessarily re-traverses links, so its depth grows without bound
// and it is refused at 10^5 rather than spun on forever. Trading an unrecoverable crash
// for a silent hang is not a fix.
//
// The "no exceptions" is load-bearing and was nearly written the other way. Collapsing
// embedded fields to the parent's level would mirror encoding/json's field promotion
// exactly — and MEASURED, `type N struct{ *N }` with `n.N = &n` marshals cleanly to `{}`
// while a promotion-collapsing walk descends it forever at constant depth. The bound
// stops being a cycle bound the moment any link is free.
//
// WHERE IT DOES NOT DESCEND — one rule, not a list of special cases:
//
//	Any field the encoder cannot see is a field the walk must not descend into, or the
//	walk is answering a different question than the one that matters.
//
// So it skips unexported fields, honors `json:"-"`, stops at json.Marshaler and
// encoding.TextMarshaler (mirroring the encoder's addressability rule for pointer
// receivers), and treats []byte as the base64 string the encoder emits. Skipping
// unexported fields is a CORRECTNESS requirement and not an optimization: MEASURED, an
// ordinary tree with an unexported parent back-pointer — `struct{ Kids []*Node; parent
// *Node }` — marshals cleanly at depth 9 in ~100 bytes, while a walk that descends into
// `parent` sees a cycle and runs to the bound. That is a false refusal on one of the most
// common shapes in Go, on a value the encoder is perfectly happy with.
//
// A consequence worth stating because it bounds the cost claim: the walk visits a SUBSET
// of the nodes json.Marshal would visit — the same containers by the same rules, minus
// whatever it skips, and it aborts at the bound where the encoder keeps going. So it
// cannot diverge on a value the encoder handles, and it cannot cost unboundedly where
// the encoder would not.
//
// ErrValidation, matching checkJSONDepth and its byte/element twins: nothing is corrupt,
// the caller handed over a structure that cannot be serialized safely. The message names
// the subject so a host with several inputs, outputs and signals in flight is told WHICH
// value was refused — a guard that fires correctly and cannot be acted on has closed the
// vector and not the incident.
func checkValueDepth(v any, subject string) error {
	if _, exceeded := walkFrames(v, maxWalkFrames); exceeded {
		// THE MESSAGE MUST NOT CLAIM WHAT THE WALK CANNOT KNOW. Tripping this bound
		// means one of two things and the walk cannot tell them apart — that is the
		// non-transparency property working, not a gap in the diagnosis. Saying
		// "nests more than N levels" about a value that is actually CYCLIC is a
		// confidently wrong diagnosis, and a vague correct one beats it every time.
		//
		// It costs a real diagnostic that used to exist: json.Marshal has its own
		// cycle detector, it is cheap and exact, and it returns "encountered a cycle".
		// The walk runs FIRST and trips before the encoder is ever entered, so that
		// precise answer is no longer produced. Stated here rather than lost.
		return fmt.Errorf("%w: %s nests more than %d levels deep, or is cyclic — the walk cannot distinguish the two, and either exhausts the goroutine stack inside json.Marshal or reflect.DeepEqual (a fatal, unrecoverable error, not a panic)",
			ErrValidation, subject, maxWalkFrames)
	}
	return nil
}

// valueDepth is the walk itself: it returns the deepest nesting it reached and whether it
// hit the bound. checkValueDepth is the policy on top; this is the measurement underneath.
//
// SPLIT OUT SO THE WALK CAN BE MEASURED RATHER THAN ONLY OBEYED. Two things need the
// number and not the verdict:
//
//   - the metamorphic arm, which compares this walk's answer against
//     jsonNestingDepth(json.Marshal(v)) to establish that the walk NEVER UNDER-REPORTS
//     relative to the encoder — the property that makes the bound sound at all;
//   - choosing the bound, which requires knowing by how much the walk OVER-reports on a
//     legal document, since the bound must sit above that or it refuses legal input.
//
// `bound` is required, not optional, and that is deliberate: an unbounded version of this
// function would not terminate on a cyclic value, so there must be no way to call one.
// depth is capped at bound; when exceeded is true, depth == bound and the walk stopped
// early, so the returned number is a floor and not the value's real depth.
func walkFrames(v any, bound int) (depth int, exceeded bool) {
	root := reflect.ValueOf(v)
	if !encoderDescends(root) {
		return 0, false
	}
	stack := []valueDepthFrame{newValueDepthFrame(root)}
	deepest := 1
	for len(stack) > 0 {
		child, ok := stack[len(stack)-1].next()
		if !ok {
			stack[len(stack)-1] = valueDepthFrame{} // drop the reference; frames can be huge
			stack = stack[:len(stack)-1]
			continue
		}
		// ── THE COUNTING RULE, STATED WHERE THE COUNTING HAPPENS ──
		//
		// THIS COUNTS WALK FRAMES, NOT JSON LEVELS, AND THEY ARE NOT THE SAME NUMBER.
		// Every structural link costs one: a map hop, a slice hop, a struct field, AND a
		// pointer or interface hop. So a `map[string]any` or `[]any` chain — which is
		// everything WorkflowData holds, since Set(string, any) boxes it all — costs
		// exactly TWO frames per JSON level, one for the container and one for the
		// interface it is boxed in. A named recursive type like `type chain []chain` costs
		// ONE. MEASURED: 2.00x and 1.00x respectively.
		//
		// It is written at the increment rather than left to the constant's doc comment
		// because that is the substitution this whole unit is about: the same word "depth"
		// for two quantities that differ by 2x. Anyone checking whether the bound is sound
		// must be able to see, from THIS line, which quantity it bounds.
		//
		// EVERY CHILD COSTS A FRAME, including one the walk will not descend into. The
		// encoder's recursion depth counts INVOCATIONS of its per-type encoder, and it
		// invokes one for every element — a leaf, a nil, an empty container — not only for
		// the ones it then recurses through.
		//
		// This was an off-by-one IN THE UNSAFE DIRECTION and the metamorphic arm is what
		// found it, not review. `type chain []chain` nested 50 deep around an EMPTY chain
		// encodes to 51 levels, because the encoder still calls sliceEncoder on the empty
		// innermost slice to emit `[]`. The walk skipped it as a non-container and reported
		// 50. Under-reporting is the one direction that can pass a value the encoder then
		// dies on, so it is worth the extra comparison per child.
		//
		// The consequence is that the walk now over-reports by ~1 against
		// jsonNestingDepth on ordinary values (a scalar leaf costs an encoder frame and no
		// bracket). That is the safe direction and the bound has room for it.
		depth := len(stack) + 1
		if depth > deepest {
			deepest = depth
		}
		if depth > bound {
			return bound, true
		}
		if !encoderDescends(child) {
			continue
		}
		stack = append(stack, newValueDepthFrame(child))
	}
	return deepest, false
}

// valueDepthFrame is one open container plus a cursor into its children. One frame per
// level currently open — never one per pending child, see checkValueDepth (1).
type valueDepthFrame struct {
	v    reflect.Value
	i    int              // cursor for slice/array/struct, and the once-flag for ptr/interface
	iter *reflect.MapIter // maps only; an iterator rather than MapKeys() so a wide map costs no slice
}

func newValueDepthFrame(v reflect.Value) valueDepthFrame {
	if v.Kind() == reflect.Map {
		return valueDepthFrame{v: v, iter: v.MapRange()}
	}
	return valueDepthFrame{v: v}
}

// next yields this container's next child, or ok=false when it is exhausted.
//
// Map KEYS are deliberately not yielded. A JSON object key is a string: the encoder
// either has one already or obtains it from MarshalText, whose output is a string too.
// A key can therefore contribute no nesting, and walking keys would only cost time.
func (f *valueDepthFrame) next() (reflect.Value, bool) {
	switch f.v.Kind() {
	case reflect.Interface, reflect.Pointer:
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
		t := f.v.Type()
		for f.i < t.NumField() {
			f.i++
			if encoderVisitsField(t.Field(f.i - 1)) {
				return f.v.Field(f.i - 1), true
			}
		}
		return reflect.Value{}, false
	default:
		return reflect.Value{}, false
	}
}

// encoderDescends reports whether json.Marshal walks INTO this value rather than
// emitting it as a scalar. It is the whole of the doc-comment rule in one place, so a
// reader can check the walk against the encoder in one screen.
func encoderDescends(v reflect.Value) bool {
	if !v.IsValid() { // an untyped nil, or a nil interface's Elem()
		return false
	}
	if stopsAtCustomMarshaler(v) {
		return false
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		return !v.IsNil()
	case reflect.Map:
		return !v.IsNil() && v.Len() > 0
	case reflect.Slice:
		// []byte is emitted as a base64 STRING, so the encoder never sees its elements.
		// (Only slices — encoding/json does NOT base64 a byte ARRAY, which it emits as a
		// list of numbers; both are leaves for depth purposes, a uint8 being a scalar.)
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		return !v.IsNil() && v.Len() > 0
	case reflect.Array:
		return v.Type().Elem().Kind() != reflect.Uint8 && v.Len() > 0
	case reflect.Struct:
		return v.NumField() > 0
	default:
		// Strings, numbers, bools: scalars. Chan, func, complex, unsafe.Pointer:
		// json.Marshal REFUSES these, and that refusal is the caller's existing error
		// path — walking into them would be answering a question the encoder never asks.
		return false
	}
}

// stopsAtCustomMarshaler mirrors encoding/json's marshaler dispatch, INCLUDING its
// addressability rule, which is not a detail: MEASURED, `json.Marshal(HoldsPM{...})`
// encodes the field structurally while `json.Marshal(&HoldsPM{...})` calls the pointer
// receiver's MarshalJSON on the very same field — the only difference is that reaching it
// through a pointer makes it addressable. A walk that ignored CanAddr would descend where
// the encoder does not (or the reverse) on a value that differs only in how it was passed.
func stopsAtCustomMarshaler(v reflect.Value) bool {
	t := v.Type()
	if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) {
		return true
	}
	if v.CanAddr() {
		p := reflect.PointerTo(t)
		return p.Implements(jsonMarshalerType) || p.Implements(textMarshalerType)
	}
	return false
}

// encoderVisitsField mirrors encoding/json's typeFields eligibility PARTIALLY, and the gap
// is stated because a claim of FULL mirroring is what MAJOR 3 refuted.
//
// It models the per-field rules: unexported, `json:"-"`, and the anonymous carve-out. It
// does NOT model typeFields' DOMINANCE AND AMBIGUITY resolution — colliding json names are
// dropped by the encoder and descended here. Deliberate: modelling dominance would add
// exactly the mirroring complexity that produced BLOCKER 1 on this same guard, and the
// divergence is in the OVER-report direction, which refuses earlier rather than crashing.
//
// The unexported rule is the one that was measured rather than reasoned about (see
// checkValueDepth). The anonymous carve-out is encoding/json's: an EMBEDDED unexported
// field is still traversed when its type is a struct, because its exported fields are
// promoted into the parent object — so skipping it wholesale would blind the walk to
// fields the encoder does emit.
//
// Note this decides only WHETHER to descend, never at what depth. Embedded fields are
// promoted by the encoder but still cost a level here; see checkValueDepth (2) for why
// making that link free would reintroduce the hang.
func encoderVisitsField(sf reflect.StructField) bool {
	if sf.Anonymous {
		t := sf.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if !sf.IsExported() && t.Kind() != reflect.Struct {
			return false
		}
	} else if !sf.IsExported() {
		return false
	}
	// Exactly "-" means omitted. `json:"-,"` names the field "-" and is NOT omitted.
	return sf.Tag.Get("json") != "-"
}
