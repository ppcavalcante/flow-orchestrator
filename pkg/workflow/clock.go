package workflow

import (
	"context"
	"sync"
	"time"
)

// Clock is the single source of "now" for all durable-timer logic (M10 chunk 2,
// D36-07). Every time read on the timer / scheduler / executor path goes through
// a Clock — production wires the real wall clock (SystemClock), tests wire a
// FakeClock they advance explicitly. The rule is mechanical and load-bearing:
// NEVER call time.Now() directly in timer/scheduler/executor logic. Routing every
// read through the injected Clock is (a) what makes durable-time tests
// deterministic and instant (no real sleeping), and (b) what the no-determinism-
// tax SMTC analyzer-spec mechanically enforces. A durable timer is persisted
// absolute DATA (fireAt), not a replayed clock call, so reading the clock to
// decide "is fireAt <= now?" is an ordinary side-effecting read at the scheduler
// boundary — never replayed, never expected to reproduce a prior value. (See
// .planning/research/M10-tier2/durable-timers.md §Q4/§Q6.)
type Clock interface {
	// Now returns the current instant. Implementations must be safe for
	// concurrent use (a level runs parked nodes in parallel goroutines).
	Now() time.Time
}

// systemClock is the production Clock: a thin wrapper over time.Now. It is the
// ONLY place in the timer/scheduler path that calls time.Now() — the wrapper that
// the no-determinism-tax gate carves out as the legitimate clock boundary.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns the production Clock backed by the real wall clock. It is
// the default a Workflow uses when no Clock is injected.
func SystemClock() Clock { return systemClock{} }

// FakeClock is a deterministic, test-advanced Clock. The host (a test, or a user
// exercising their own durable-timer workflow) constructs it at a fixed instant
// and advances it explicitly with Advance/Set — no real time passes. This is the
// "testing durable time" aid from the durable-timers research (§Q6): "fire across
// a 3h restart" becomes an instant, deterministic unit test. It is safe for
// concurrent reads/advances.
type FakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFakeClock returns a FakeClock pinned at the given instant.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

// Now returns the fake clock's current instant.
func (c *FakeClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the fake clock forward by d (a negative d moves it backward,
// modelling NTP skew — firing only ever happens when fireAt <= now, so a backward
// jump merely delays a fire, never causes an early one).
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set pins the fake clock to an absolute instant.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// clockCtxKey is the private context key carrying the per-invocation Clock. A
// Clock is injected at the top of Workflow.Execute (the workflow's Clock, or the
// system clock by default) and at Workflow.Tick (a clock pinned to the host-
// supplied "now"), and flows down to the timer action through the same ctx the
// executor already threads to every node. Using the ctx — rather than a field on
// the action — means Tick(now) can pin "now" for one invocation with no shared
// mutable clock state, and a normal Execute and a host Tick read time through the
// identical seam.
type clockCtxKey struct{}

// withClock returns a child context carrying c as the timer/scheduler clock.
func withClock(ctx context.Context, c Clock) context.Context {
	return context.WithValue(ctx, clockCtxKey{}, c)
}

// clockFrom extracts the injected Clock from ctx, falling back to the system
// clock when none was injected (e.g. a TimerNode driven through DAG.Execute
// directly, outside Workflow.Execute). This fallback is the ONE place the timer
// path resolves to the real clock without an explicit injection, and it resolves
// to SystemClock (not a raw time.Now()), keeping the "route through a Clock"
// discipline intact.
func clockFrom(ctx context.Context) Clock {
	if c, ok := ctx.Value(clockCtxKey{}).(Clock); ok && c != nil {
		return c
	}
	return systemClock{}
}
