//go:build !race

package workflow

// raceEnabled is false in a normal (non-race) build — det-tax runs and asserts its
// alloc ceiling. See race_on_test.go for the rationale.
const raceEnabled = false
