package workflow

import (
	"context"
	"sync"
)

// Locker serializes concurrent drives of the SAME WorkflowID within one process
// (M10 phase 37, D37-07b). A "drive" is the load→run→checkpoint→save→ack span of
// Workflow.Execute; two concurrent drives of one WorkflowID — an event handler, a
// poller, and a startup timer re-arm all firing at once — would otherwise race
// that span (lost durability via interleaved load-run-save). The lease makes them
// take turns.
//
// The default implementation (NewInProcessLocker) is a per-WorkflowID mutex. The
// interface is the SEAM a future cross-process lease implements behind the same
// Acquire call: a SQLite `claimed_at` row carrying owner + expiry, with
// renew-while-running and steal-if-stale semantics, would satisfy Locker without
// any API change — so adding cross-process serialization later is a new Locker
// impl, not a breaking change.
//
// Scope boundary (honest): the in-process Locker serializes only within ONE OS
// process. Cross-process exactly-once / single-writer is DEFERRED to the optional
// SQLite store (DEC-M10(B)); until then, driving the same WorkflowID from multiple
// OS processes is the host's responsibility.
type Locker interface {
	// Acquire blocks until it holds the lease for workflowID, returning a release
	// func the caller MUST invoke (typically deferred) to release it. ctx lets a
	// future cross-process impl honor cancellation/timeout while waiting for a held
	// lease; the in-process impl cannot cancel a sync.Mutex Lock and ignores it.
	Acquire(ctx context.Context, workflowID string) (release func(), err error)
}

// inProcessLocker is the default Locker: one sync.Mutex per WorkflowID, held for
// the duration of a drive. Distinct WorkflowIDs use distinct mutexes (independent,
// no contention); the same WorkflowID is serialized. Process-local only.
//
// REFCOUNTED (ph37 review F4): each entry tracks how many drives currently hold or
// are waiting for it; the entry is removed from the registry on the LAST release, so
// the map does NOT grow unbounded for a host driving many short-lived, uniquely-id'd
// workflows (the M10 long-running-server use case). refs is mutated only under the
// registry mutex; the per-ID drive lock is taken/released OUTSIDE it (so waiters
// block on the drive lock, not the registry).
type inProcessLocker struct {
	mu    sync.Mutex
	locks map[string]*leaseEntry
}

// leaseEntry is one WorkflowID's drive lock plus its registry refcount.
type leaseEntry struct {
	lock sync.Mutex // the per-WorkflowID drive lock (held for the whole drive)
	refs int        // holders + waiters; guarded by inProcessLocker.mu
}

// NewInProcessLocker returns the default in-process per-WorkflowID Locker. Share a
// single instance across Workflow objects to serialize same-ID drives that span
// different *Workflow instances (the package default does exactly this).
func NewInProcessLocker() Locker {
	return &inProcessLocker{locks: make(map[string]*leaseEntry)}
}

// Acquire blocks until it holds the per-WorkflowID drive lock and returns a release
// that drops it and reclaims the registry entry when no drive references it. The
// refcount is bumped under the registry mutex BEFORE the (possibly blocking) drive
// lock is taken, guaranteeing the entry survives for a waiter and is deleted only
// when the last holder/waiter releases.
func (l *inProcessLocker) Acquire(_ context.Context, workflowID string) (func(), error) {
	l.mu.Lock()
	e, ok := l.locks[workflowID]
	if !ok {
		e = &leaseEntry{}
		l.locks[workflowID] = e
	}
	e.refs++ // reserve a reference before releasing the registry mutex
	l.mu.Unlock()

	e.lock.Lock() // blocks here (NOT under the registry mutex) until we hold the drive

	return func() {
		e.lock.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, workflowID) // reclaim — no other drive references it
		}
		l.mu.Unlock()
	}, nil
}

// defaultLocker is the process-wide default in-process Locker. Every Workflow that
// does not set its own Locker uses it, so two DIFFERENT *Workflow objects sharing a
// WorkflowID are still serialized within the process. It is a concurrency primitive
// (a lease registry is inherently process-shared), not mutable global config.
var defaultLocker = NewInProcessLocker()
