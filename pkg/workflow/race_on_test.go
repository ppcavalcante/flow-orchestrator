//go:build race

package workflow

// raceEnabled is true when the test binary is built with -race. The det-tax alloc
// ceiling (perf_ceiling_test.go) is measured via testing.Benchmark().AllocsPerOp(),
// which the race detector inflates by a fixed instrumentation overhead (~+9 allocs
// on this DAG) — a race-mode reading is meaningless for the zero-determinism-tax
// guard, so the test skips under -race and is enforced non-race (incl. a blocking
// non-race CI step). Build-tagged pair: race_on_test.go (race) / race_off_test.go (!race).
const raceEnabled = true
