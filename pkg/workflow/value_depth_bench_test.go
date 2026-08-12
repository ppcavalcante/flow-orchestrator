package workflow

import (
	"encoding/json"
	"fmt"
	"testing"
)

// AF2 cost. The walk runs on EVERY write, including 64 MiB snapshots, so its cost is a
// number that has to be MEASURED rather than asserted — this exact function family
// already produced one unmeasured hot-path cost claim in this phase (F116-SELF-2).
//
// EVERY EARLIER NUMBER FOR THIS WALK IS DEAD, and the reason is worth keeping next to the
// benchmark rather than in a report: the engineer's 6-11% and the tester's 6-17% both
// characterize a TYPE-SWITCH / reference walk. The shipped walk uses `reflect`, and
// quoting a ratio across that boundary is the phase's own recurring defect — a statement
// true at the scope measured, restated at a scope where it does not hold. So the file
// exists to make the number re-derivable rather than to record one:
//
//	go test ./pkg/workflow/ -run '^$' -bench 'BenchmarkAF2' -benchtime 200x
//
// Read it as a RATIO of walk to marshal, not as absolute times. The ratio is what the
// design question is about, and it is far more robust to a loaded machine than either
// time alone — which matters, because the box these were first taken on was not quiet.
//
// The corpus is a SAMPLE and its shapes are not fuzzed. It covers the axes the walk's
// cost actually varies along — breadth, depth, scalar-vs-container ratio, and the two
// leaf rules ([]byte and string) that exist partly for cost — but a shape that is
// pathological for `reflect` and absent here would not show up.
func benchCorpus() map[string]any {
	flat := map[string]any{}
	for i := 0; i < 1000; i++ {
		flat[fmt.Sprintf("k%d", i)] = i
	}
	wide := make([]any, 10000)
	for i := range wide {
		wide[i] = map[string]any{"i": i, "s": "value"}
	}
	strings1k := map[string]any{}
	for i := 0; i < 1000; i++ {
		strings1k[fmt.Sprintf("k%d", i)] = "a moderately long string value that is not walked into"
	}
	structs := make([]any, 5000)
	for i := range structs {
		structs[i] = benchRow{ID: i, Name: "n", Tags: []string{"a", "b"}, Meta: map[string]int{"x": 1}}
	}
	return map[string]any{
		"flat-1k-ints":     flat,
		"flat-1k-strings":  strings1k,
		"wide-10k-objects": wide,
		"structs-5k":       structs,
		"deep-100":         nestValue(100),
		"deep-1000":        nestValue(1000),
		"nested-map-1000":  nestMapValue(1000),
		"bytes-1mib":       map[string]any{"blob": make([]byte, 1<<20)},
		"realistic-run":    benchRealisticRun(),
	}
}

type benchRow struct {
	ID   int
	Name string
	Tags []string
	Meta map[string]int
}

// benchRealisticRun is the shape that decides whether the cost matters in practice: a
// checkpoint from a run with a few hundred nodes, each holding a small structured output.
func benchRealisticRun() any {
	outputs := map[string]any{}
	for i := 0; i < 300; i++ {
		outputs[fmt.Sprintf("node-%03d", i)] = map[string]any{
			"status": "ok", "attempt": 1, "elapsed_ms": 1234,
			"result": map[string]any{"id": i, "items": []any{1, 2, 3}},
		}
	}
	return map[string]any{"id": "wf", "outputs": outputs, "data": map[string]any{"n": 300}}
}

func BenchmarkAF2Walk(b *testing.B) {
	for name, v := range benchCorpus() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := checkValueDepth(v, "bench"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkAF2Marshal(b *testing.B) {
	for name, v := range benchCorpus() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := json.Marshal(v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAF2WalkThenMarshal is the number a caller actually pays — the pair, as
// encodeHostValue runs it. Quoting the walk alone against a marshal measured separately
// is how a ratio drifts from what anyone experiences.
func BenchmarkAF2WalkThenMarshal(b *testing.B) {
	for name, v := range benchCorpus() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := checkValueDepth(v, "bench"); err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
