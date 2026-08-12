package workflow

import (
	"reflect"
	"testing"
	"unsafe"
)

// ============================================================================
// THE ORACLE — an independent, iterative simulation of reflect.deepValueEqual
// that reports the PEAK RECURSION DEPTH the real call would reach on a PAIR.
//
// Written from $GOROOT/src/reflect/deepequal.go (go1.25.1), not from the
// subject file. Iterative because the shapes under test recurse deeper than
// this process's own stack.
//
// SOUNDNESS DIRECTION — this is a LOWER BOUND on the true depth:
//   - Pointer / Map: keyed on UnsafePointer(), which is EXACTLY stdlib's
//     ptrval() for those kinds. Exact.
//   - Slice: stdlib keys on the slice HEADER address (v.ptr, unreachable from
//     outside reflect); we key on the DATA pointer, which is strictly COARSER,
//     so we cut at least as early as stdlib.
//   - Interface: stdlib keys on the interface word address; we key on the
//     identity of the ELEMENT, again strictly coarser.
// Cutting earlier can only make us report the SAME or LESS than the truth.
// So `advPairDepth(a,b) > N` PROVES the real call reaches depth > N.
// ============================================================================

type advVisit struct {
	p1, p2 unsafe.Pointer
	typ    reflect.Type
	n1, n2 int // slice lengths; 0 for other kinds
}

func advID(v reflect.Value) (unsafe.Pointer, bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return nil, false
		}
		return v.UnsafePointer(), true
	case reflect.Interface:
		if v.IsNil() {
			return nil, false
		}
		return advID(v.Elem())
	}
	return nil, false
}

// advLen carries slice LENGTH into the visit key.
//
// 🔴 THE ORACLE HAD THE BUG IT WAS BUILT TO DETECT. Keyed on the data pointer
// alone, back[0:1] and back[0:4] collapsed into one entry and the oracle
// reported depth 5 for a value whose real descent is ~34,000 — the subject's
// own documented "41x UNDER-report", reproduced inside the instrument.
//
// (ptr,len) is still strictly COARSER than stdlib's slice-header address — two
// distinct headers with the same data pointer and length collapse here and do
// not collapse there — so the lower-bound direction is preserved.
// It MUST follow the interface element exactly as advID does. A version that
// did not (advID recursed into Elem, advLen inspected the interface and
// returned 0) gave both `any`-wrapped slices the key (ptr,ptr,any,0,0), so the
// second one read as already-visited and the walk stopped at depth 5 on a value
// whose real descent is ~34,000. The two functions form ONE key and have to
// unwrap in lockstep.
func advLen(v reflect.Value) int {
	if v.Kind() == reflect.Interface && !v.IsNil() {
		return advLen(v.Elem())
	}
	if v.Kind() == reflect.Slice && !v.IsNil() {
		return v.Len()
	}
	return 0
}

type advPF struct {
	v1, v2  reflect.Value
	entered bool
	mode    byte // 'e' index-wise, 's' single child, 'm' map, 0 = no children
	i, n    int
	keys    []reflect.Value
}

// advPairDepth returns the peak recursion depth of reflect.DeepEqual(a,b) and
// whether the walk hit its own cap.
func advPairDepth(a, b any, cap int) (peak int, hitCap bool) {
	// DeepEqual's own preamble: nil short-circuit, then a top-level type check.
	if a == nil || b == nil {
		return 0, false
	}
	v1, v2 := reflect.ValueOf(a), reflect.ValueOf(b)
	if v1.Type() != v2.Type() {
		return 1, false // the type check is inside deepValueEqual: one frame, no recursion
	}

	visited := map[advVisit]bool{}
	st := []advPF{{v1: v1, v2: v2}}
	last := true

	mark := func(v1, v2 reflect.Value) (seen bool) {
		// stdlib hard(): Pointer (with pointers), Map, Slice, Interface, both non-nil.
		switch v1.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
		default:
			return false
		}
		if v1.IsNil() || v2.IsNil() {
			return false
		}
		p1, ok1 := advID(v1)
		p2, ok2 := advID(v2)
		if !ok1 || !ok2 {
			return false
		}
		n1, n2 := advLen(v1), advLen(v2)
		if uintptr(p1) > uintptr(p2) {
			p1, p2 = p2, p1
			n1, n2 = n2, n1
		}
		k := advVisit{p1, p2, v1.Type(), n1, n2}
		if visited[k] {
			return true
		}
		visited[k] = true
		return false
	}

	pop := func(r bool) {
		st[len(st)-1] = advPF{}
		st = st[:len(st)-1]
		last = r
	}

	for len(st) > 0 {
		if len(st) > peak {
			peak = len(st)
		}
		if len(st) >= cap {
			return peak, true
		}
		top := &st[len(st)-1]

		if !top.entered {
			top.entered = true
			x, y := top.v1, top.v2
			if !x.IsValid() || !y.IsValid() {
				pop(x.IsValid() == y.IsValid())
				continue
			}
			if x.Type() != y.Type() {
				pop(false)
				continue
			}
			if mark(x, y) {
				pop(true)
				continue
			}
			switch x.Kind() {
			case reflect.Array:
				top.mode, top.n = 'e', x.Len()
			case reflect.Slice:
				if x.IsNil() != y.IsNil() || x.Len() != y.Len() {
					pop(false)
					continue
				}
				if x.UnsafePointer() == y.UnsafePointer() {
					pop(true)
					continue
				}
				if x.Type().Elem().Kind() == reflect.Uint8 {
					pop(true) // bytealg.Equal — no recursion; the boolean is irrelevant to depth
					continue
				}
				top.mode, top.n = 'e', x.Len()
			case reflect.Interface:
				if x.IsNil() || y.IsNil() {
					pop(x.IsNil() == y.IsNil())
					continue
				}
				top.mode, top.n = 's', 1
			case reflect.Pointer:
				if x.UnsafePointer() == y.UnsafePointer() {
					pop(true)
					continue
				}
				top.mode, top.n = 's', 1
			case reflect.Struct:
				top.mode, top.n = 'e', x.NumField()
			case reflect.Map:
				if x.IsNil() != y.IsNil() || x.Len() != y.Len() {
					pop(false)
					continue
				}
				if x.UnsafePointer() == y.UnsafePointer() {
					pop(true)
					continue
				}
				top.keys = x.MapKeys()
				top.mode, top.n = 'm', len(top.keys)
			default:
				pop(advScalarEq(x, y))
				continue
			}
		} else if !last {
			pop(false) // a child answered false: deepValueEqual short-circuits
			continue
		}

		if top.i >= top.n {
			pop(true)
			continue
		}
		idx := top.i
		top.i++
		var c1, c2 reflect.Value
		switch top.mode {
		case 's':
			c1, c2 = top.v1.Elem(), top.v2.Elem()
		case 'm':
			k := top.keys[idx]
			c1 = top.v1.MapIndex(k)
			c2 = top.v2.MapIndex(k)
			if !c1.IsValid() || !c2.IsValid() {
				pop(false)
				continue
			}
		default: // 'e'
			if top.v1.Kind() == reflect.Struct {
				c1, c2 = top.v1.Field(idx), top.v2.Field(idx)
			} else {
				c1, c2 = top.v1.Index(idx), top.v2.Index(idx)
			}
		}
		st = append(st, advPF{v1: c1, v2: c2})
	}
	return peak, false
}

func advScalarEq(x, y reflect.Value) bool {
	switch x.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return x.Int() == y.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return x.Uint() == y.Uint()
	case reflect.String:
		return x.String() == y.String()
	case reflect.Bool:
		return x.Bool() == y.Bool()
	case reflect.Float32, reflect.Float64:
		return x.Float() == y.Float()
	case reflect.Complex64, reflect.Complex128:
		return x.Complex() == y.Complex()
	case reflect.Func:
		return x.IsNil() && y.IsNil()
	default:
		return x.Pointer() == y.Pointer()
	}
}

// ---------------------------------------------------------------------------
// ORACLE SELF-CALIBRATION — the oracle is an instrument and must be shown to
// read true before any verdict rests on it. Three independent checks.
// ---------------------------------------------------------------------------
func TestADV_OracleCalibration(t *testing.T) {
	t.Run("agrees with the file's MEASURED 800/801 = 640,800", func(t *testing.T) {
		a, b := mkAdvCyc(800), mkAdvCyc(801)
		peak, capped := advPairDepth(a, b, 5_000_000)
		t.Logf("MEASURED by oracle: peak=%d capped=%v (subject file claims 640,800)", peak, capped)
		if capped {
			t.Fatal("oracle capped — recalibrate")
		}
		// lcm(800,801) = 640800 pointer hops; each hop is ptr+struct = 2 frames.
		if peak < 640_800 {
			t.Errorf("ORACLE READS LOW: %d < 640,800. It would under-detect real defects", peak)
		}
	})

	t.Run("agrees with a hand-countable acyclic chain", func(t *testing.T) {
		// 2 frames per link + 1 terminal nil-pointer compare.
		for _, k := range []int{1, 2, 5, 50} {
			a, b := mkAdvChain(k), mkAdvChain(k)
			peak, _ := advPairDepth(a, b, 1_000_000)
			want := 2*k + 1
			t.Logf("links=%d oracle peak=%d want=%d", k, peak, want)
			if peak != want {
				t.Errorf("ORACLE MISCOUNTS: links=%d peak=%d want=%d", k, peak, want)
			}
		}
	})

	t.Run("agrees that identical operands cost exactly one frame", func(t *testing.T) {
		v := mkAdvCyc(500)
		peak, _ := advPairDepth(v, v, 1_000_000)
		t.Logf("identical operands: oracle peak=%d", peak)
		if peak != 1 {
			t.Errorf("ORACLE MISCOUNTS the UnsafePointer short-circuit: peak=%d want 1", peak)
		}
	})
}
