package workflow

// Typed-key data API.
//
// This is an additive, type-safe layer over the existing string-keyed
// WorkflowData.Set/Get. A Key[T] carries the value type T alongside its string
// name, so a producer and a consumer that share the same Key are checked at
// compile time: storing or reading the wrong type is a compilation error, not a
// runtime assertion at the call site.
//
// These are package-level generic functions rather than methods because Go does
// not permit type parameters on methods. Use them as workflow.Set(d, key, v)
// and workflow.Get(d, key).
//
// The values are stored in the same underlying map[string]interface{} as the
// string API (the v.(T) assertion bridges the interface{} store), so the two
// APIs fully interoperate: a value written with Set[T] is visible to the string
// Get, and vice versa. A dedicated typed store that avoids interface{} boxing
// is a possible follow-on; this layer is deliberately a thin, additive bridge.

// Key is a typed handle to a value in a WorkflowData. The type parameter T is
// the type of the value the key addresses; the name is the underlying string
// key used in the store. Declare a Key once and share it between the node that
// produces the value and the nodes that consume it.
type Key[T any] struct {
	name string
}

// NewKey returns a Key addressing the given name and carrying value type T.
func NewKey[T any](name string) Key[T] {
	return Key[T]{name: name}
}

// Name returns the underlying string key. This makes a typed Key inspectable
// and lets callers reach the same value through the string API when needed.
func (k Key[T]) Name() string {
	return k.name
}

// Set stores v under the typed key k in d. It writes through the existing
// string-keyed store, so the value is also visible via WorkflowData.Get(k.Name()).
// It is thread-safe (it inherits WorkflowData.Set's locking).
func Set[T any](d *WorkflowData, k Key[T], v T) {
	d.Set(k.name, v)
}

// Get retrieves the value stored under the typed key k in d.
//
// It returns (value, true) only when a value is present at k's name AND that
// value's dynamic type is T. When the key is absent, or a value is present but
// of a different type, it returns (zero, false). A stored zero value of type T
// is reported as (zero, true), so an absent key is distinguishable from a
// stored zero. Get never panics: the type check uses the comma-ok assertion.
func Get[T any](d *WorkflowData, k Key[T]) (T, bool) {
	v, ok := d.Get(k.name)
	if !ok {
		var zero T
		return zero, false
	}
	t, ok := v.(T)
	if !ok {
		var zero T
		return zero, false
	}
	return t, true
}

// GetOr retrieves the value stored under the typed key k in d, or returns def
// when the key is absent or holds a value of a different type. It is a
// convenience wrapper over Get for call sites that always want a usable value.
func GetOr[T any](d *WorkflowData, k Key[T], def T) T {
	if v, ok := Get(d, k); ok {
		return v
	}
	return def
}
