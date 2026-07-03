package workflow

import (
	"context"
	"sync"
)

// This file is the TEST-ONLY park vehicle for the M10 suspend/re-enter spine
// (phase 35, T5). It is deliberately in a _test.go file in `package workflow`
// (internal test) so it can satisfy the unexported suspendableAction marker
// WITHOUT leaking a suspension API into the public/real-node surface. The real
// TimerNode / WaitForSignalNode land in phases 36/37 and reuse this exact seam;
// this vehicle exists only to drive suspend → wake → converge deterministically
// (no timing) so the bite-proofs are reproducible. (CONTEXT D-09;
// test-vehicle-stays-test-only lens.)

// parkGate is the deterministic, thread-safe control surface for the test park
// vehicle. While it is "parked", a suspendingAction returns ErrSuspended each
// time it runs (modeling a node blocked on an external event); wake() clears it
// (modeling the event arriving), after which the next run completes. It is
// concurrency-safe because a level's parked node runs in its own goroutine
// alongside its siblings, and because chunk-3 will extend this surface to expose
// the signal double-apply race (NoDoubleApply) — the mutex is the seam for that.
type parkGate struct {
	mu       sync.Mutex
	parked   bool
	parkRuns int // how many times the action observed the gate parked
	wakeRuns int // how many times it observed the gate woken (completed)
}

// newParkGate returns a gate that starts parked (the node will suspend on its
// first run, the common case the spine must handle).
func newParkGate() *parkGate {
	return &parkGate{parked: true}
}

// wake is the deterministic force-wake hook: it clears the parked flag so the
// next run of the suspendingAction completes instead of re-parking. Modeling
// "the external event arrived." Idempotent.
func (g *parkGate) wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parked = false
}

// observe records and returns whether the gate is currently parked.
func (g *parkGate) observe() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked {
		g.parkRuns++
		return true
	}
	g.wakeRuns++
	return false
}

// stats returns (parkRuns, wakeRuns) for assertions that the seam was actually
// traversed (the run parked at least once) and converged (completed exactly once
// after wake).
func (g *parkGate) stats() (parkRuns, wakeRuns int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parkRuns, g.wakeRuns
}

// suspendingAction is the test-only declared suspension action. It satisfies the
// unexported suspendableAction marker, so node.Execute honors its ErrSuspended
// return as an intentional park. While the gate is parked it returns
// ErrSuspended; once woken it runs onComplete (or succeeds trivially), letting a
// re-entered Execute converge.
type suspendingAction struct {
	gate       *parkGate
	onComplete func(ctx context.Context, data *WorkflowData) error
}

// suspendable marks this action as a declared suspension node. The empty body is
// the whole point — the method's mere presence is the static capability gate.
func (a *suspendingAction) suspendable() {}

func (a *suspendingAction) Execute(ctx context.Context, data *WorkflowData) error {
	if a.gate.observe() {
		return ErrSuspended
	}
	if a.onComplete != nil {
		return a.onComplete(ctx, data)
	}
	return nil
}

// newSuspendingNode builds a test node whose action parks per the given gate.
func newSuspendingNode(name string, gate *parkGate) *Node {
	return NewNode(name, &suspendingAction{gate: gate})
}
