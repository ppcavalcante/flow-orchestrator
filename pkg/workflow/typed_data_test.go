package workflow

import (
	"sync"
	"testing"
)

// roundTrip is a tiny helper asserting that a value stored via a typed key reads
// back equal and present.
func roundTrip[T comparable](t *testing.T, name string, want T) {
	t.Helper()
	d := NewWorkflowData("rt")
	k := NewKey[T](name)
	Set(d, k, want)
	got, ok := Get(d, k)
	if !ok {
		t.Fatalf("%s: Get returned ok=false, want true", name)
	}
	if got != want {
		t.Fatalf("%s: Get returned %v, want %v", name, got, want)
	}
}

func TestTypedRoundTripScalars(t *testing.T) {
	roundTrip(t, "int", 42)
	roundTrip(t, "int64", int64(1)<<40) // > MaxInt32, faithful via int64
	roundTrip(t, "string", "hello")
	roundTrip(t, "bool", true)
}

func TestTypedRoundTripStruct(t *testing.T) {
	type point struct {
		X, Y int
		Tag  string
	}
	d := NewWorkflowData("struct")
	k := NewKey[point]("p")
	want := point{X: 3, Y: -4, Tag: "origin-ish"}
	Set(d, k, want)
	got, ok := Get(d, k)
	if !ok {
		t.Fatalf("struct: ok=false, want true")
	}
	if got != want {
		t.Fatalf("struct: got %+v, want %+v", got, want)
	}
}

func TestTypedRoundTripSlice(t *testing.T) {
	d := NewWorkflowData("slice")
	k := NewKey[[]int]("xs")
	want := []int{1, 2, 3, 5, 8}
	Set(d, k, want)
	got, ok := Get(d, k)
	if !ok {
		t.Fatalf("slice: ok=false, want true")
	}
	if len(got) != len(want) {
		t.Fatalf("slice: len %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slice[%d]: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestTypedTypeMismatch: an int is stored under a name, then read with a Key
// declared for string. The value is present but of the wrong dynamic type, so
// Get must return (zero, false) and must not panic.
func TestTypedTypeMismatch(t *testing.T) {
	d := NewWorkflowData("mismatch")
	Set(d, NewKey[int]("x"), 7)

	got, ok := Get(d, NewKey[string]("x"))
	if ok {
		t.Fatalf("mismatch: ok=true, want false")
	}
	if got != "" {
		t.Fatalf("mismatch: got %q, want zero string", got)
	}
}

// TestTypedAbsentKey: an absent key reads as (zero, false).
func TestTypedAbsentKey(t *testing.T) {
	d := NewWorkflowData("absent")
	got, ok := Get(d, NewKey[int]("nope"))
	if ok {
		t.Fatalf("absent: ok=true, want false")
	}
	if got != 0 {
		t.Fatalf("absent: got %d, want 0", got)
	}
}

// TestTypedStoredZeroDistinctFromAbsent: a stored zero value must read as
// (zero, true), distinguishing it from an absent key which reads (zero, false).
func TestTypedStoredZeroDistinctFromAbsent(t *testing.T) {
	d := NewWorkflowData("zero")

	kInt := NewKey[int]("i")
	Set(d, kInt, 0)
	if got, ok := Get(d, kInt); !ok || got != 0 {
		t.Fatalf("stored zero int: got (%d,%v), want (0,true)", got, ok)
	}

	kBool := NewKey[bool]("b")
	Set(d, kBool, false)
	if got, ok := Get(d, kBool); !ok || got != false {
		t.Fatalf("stored zero bool: got (%v,%v), want (false,true)", got, ok)
	}

	kStr := NewKey[string]("s")
	Set(d, kStr, "")
	if got, ok := Get(d, kStr); !ok || got != "" {
		t.Fatalf("stored zero string: got (%q,%v), want (\"\",true)", got, ok)
	}
}

// TestTypedInteropTypedToString: a value written with the typed API is visible
// through the string API under the key's Name().
func TestTypedInteropTypedToString(t *testing.T) {
	d := NewWorkflowData("interop1")
	k := NewKey[int]("answer")
	Set(d, k, 42)

	v, ok := d.Get(k.Name())
	if !ok {
		t.Fatalf("typed->string: string Get ok=false, want true")
	}
	if iv, ok := v.(int); !ok || iv != 42 {
		t.Fatalf("typed->string: got %v (%T), want int 42", v, v)
	}
}

// TestTypedInteropStringToTyped: a value written with the string API is readable
// through a matching typed Key.
func TestTypedInteropStringToTyped(t *testing.T) {
	d := NewWorkflowData("interop2")
	d.Set("greeting", "hi")

	got, ok := Get(d, NewKey[string]("greeting"))
	if !ok {
		t.Fatalf("string->typed: ok=false, want true")
	}
	if got != "hi" {
		t.Fatalf("string->typed: got %q, want %q", got, "hi")
	}
}

// TestGetOr covers the convenience wrapper: present value returned as-is, absent
// and type-mismatched both fall back to the default.
func TestGetOr(t *testing.T) {
	d := NewWorkflowData("getor")

	if v := GetOr(d, NewKey[int]("missing"), -1); v != -1 {
		t.Fatalf("GetOr absent: got %d, want -1", v)
	}

	Set(d, NewKey[int]("present"), 99)
	if v := GetOr(d, NewKey[int]("present"), -1); v != 99 {
		t.Fatalf("GetOr present: got %d, want 99", v)
	}

	// Type mismatch falls back to default.
	Set(d, NewKey[int]("typed"), 5)
	if v := GetOr(d, NewKey[string]("typed"), "fallback"); v != "fallback" {
		t.Fatalf("GetOr mismatch: got %q, want fallback", v)
	}
}

// TestTypedConcurrentSet exercises parallel typed Set/Get on a shared
// WorkflowData, mirroring nodes in a level writing distinct keys concurrently.
// It must be green under -race; correctness is inherited from WorkflowData's
// RWMutex, this asserts the typed layer adds no unsynchronized access.
func TestTypedConcurrentSet(t *testing.T) {
	d := NewWorkflowData("concurrent")
	const n = 64

	keys := make([]Key[int], n)
	for i := range keys {
		keys[i] = NewKey[int]("k" + itoa(i))
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			Set(d, keys[i], i)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		got, ok := Get(d, keys[i])
		if !ok || got != i {
			t.Fatalf("concurrent key %d: got (%d,%v), want (%d,true)", i, got, ok, i)
		}
	}
}

// itoa is a tiny non-allocating-enough integer-to-string for test key names,
// avoiding a strconv import in the test file's hot path.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
