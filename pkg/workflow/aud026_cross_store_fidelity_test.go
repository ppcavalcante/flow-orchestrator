package workflow_test

import (
	"reflect"
	"testing"

	workflow "github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// AUD-026: the canonical cross-store value contract. Every store (InMemory, JSON,
// FlatBuffers, SQLite) MUST round-trip a data value or node output to the SAME
// canonical Go type and value — the "honest common denominator" the durable stores
// (FB/SQLite) already yield:
//
//	scalars   int/int32/int64 -> int64 ; float32/float64 -> float64 ; bool ; string
//	complex   map / slice / other  -> canonical JSON string
//	outputs   any non-string       -> canonical JSON string
//
// The danger this closes: a workflow tested on InMemory (real maps, real int) that
// behaves differently in production on FB/SQLite (strings). InMemory is now a
// FAITHFUL substitute.

func newStores(t *testing.T) map[string]workflow.WorkflowStore {
	t.Helper()
	js, err := workflow.NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	fb, err := workflow.NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	sq, err := workflow.NewSQLiteStore(t.TempDir() + "/s.db")
	require.NoError(t, err)
	return map[string]workflow.WorkflowStore{
		"InMemory":    workflow.NewInMemoryStore(),
		"JSON":        js,
		"FlatBuffers": fb,
		"SQLite":      sq,
	}
}

func TestAUD026_DataValueFidelity(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want interface{} // canonical form every store must return
	}{
		{"int", 42, int64(42)},
		{"int32", int32(7), int64(7)},
		{"int64", int64(-9), int64(-9)},
		{"int64-magnitude", int64(9000000000000000000), int64(9000000000000000000)},
		{"float64", 1.5, 1.5},
		{"bool", true, true},
		{"string", "hello", "hello"},
		{"json-looking-string", `{"a":1}`, `{"a":1}`}, // a genuine string stays a string
		{"map", map[string]interface{}{"a": 1, "b": "x"}, `{"a":1,"b":"x"}`},
		{"slice", []interface{}{1, 2, 3}, `[1,2,3]`},
		{"nested", map[string]interface{}{"o": map[string]interface{}{"i": 2}}, `{"o":{"i":2}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := newStores(t)
			for sname, s := range stores {
				d := workflow.NewWorkflowData("wf")
				d.Set("k", tc.in)
				require.NoError(t, s.Save(d), sname)
				got, err := s.Load("wf")
				require.NoError(t, err, sname)
				val, ok := got.Get("k")
				require.True(t, ok, "%s: key missing after reload", sname)
				require.True(t, reflect.DeepEqual(tc.want, val),
					"%s: data %q reloaded as %T(%v), want %T(%v)",
					sname, tc.name, val, val, tc.want, tc.want)
			}
		})
	}
}

func TestAUD026_OutputFidelity(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{"string", "out", "out"},
		{"int", 99, "99"}, // outputs are string-on-wire in FB/SQLite -> canonical is the string form
		{"bool", true, "true"},
		{"map", map[string]interface{}{"out": 7}, `{"out":7}`},
		{"slice", []interface{}{"a", "b"}, `["a","b"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := newStores(t)
			for sname, s := range stores {
				d := workflow.NewWorkflowData("wf")
				d.SetNodeStatus("n", workflow.Completed)
				d.SetOutput("n", tc.in)
				require.NoError(t, s.Save(d), sname)
				got, err := s.Load("wf")
				require.NoError(t, err, sname)
				val, ok := got.GetOutput("n")
				require.True(t, ok, "%s: output missing after reload", sname)
				require.True(t, reflect.DeepEqual(tc.want, val),
					"%s: output %q reloaded as %T(%v), want %T(%v)",
					sname, tc.name, val, val, tc.want, tc.want)
			}
		})
	}
}

// The stores must agree with EACH OTHER, not merely each with an expected literal —
// the property a consumer relies on when swapping a store.
func TestAUD026_AllStoresAgree(t *testing.T) {
	build := func() *workflow.WorkflowData {
		d := workflow.NewWorkflowData("wf")
		d.Set("i", 5)
		d.Set("m", map[string]interface{}{"x": 1})
		d.Set("s", []interface{}{1, "a"})
		d.SetNodeStatus("n", workflow.Completed)
		d.SetOutput("n", map[string]interface{}{"r": 9})
		return d
	}
	stores := newStores(t)
	results := map[string]map[string]interface{}{}
	for sname, s := range stores {
		require.NoError(t, s.Save(build()))
		got, err := s.Load("wf")
		require.NoError(t, err)
		i, _ := got.Get("i")
		m, _ := got.Get("m")
		sl, _ := got.Get("s")
		o, _ := got.GetOutput("n")
		results[sname] = map[string]interface{}{"i": i, "m": m, "s": sl, "o": o}
	}
	var ref string
	for sname := range results {
		if ref == "" {
			ref = sname
			continue
		}
		require.True(t, reflect.DeepEqual(results[ref], results[sname]),
			"%s and %s disagree:\n %s=%#v\n %s=%#v", ref, sname,
			ref, results[ref], sname, results[sname])
	}
}
