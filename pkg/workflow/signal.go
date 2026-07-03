package workflow

import (
	"context"
	"errors"
	"sync"
)

// ErrWaitRequiresSignalStore is returned (a real FAILURE, not the park arm) when a
// WaitForSignalNode is reached but the Store does not implement SignalStore. A
// signal wait can never be satisfied without a durable mailbox to deliver into, so
// this is a configuration error surfaced loudly rather than a forever-park — the
// SignalStore analog of ErrSuspendRequiresCheckpointer (D37-01).
var ErrWaitRequiresSignalStore = errors.New("workflow cannot wait for a signal: store does not implement SignalStore")

// signalStoreCtxKey carries the per-Execute SignalStore down to a
// WaitForSignalNode action, threaded on ctx exactly like the Clock (clock.go) and
// the checkpoint callback (checkpoint.go). Ctx-injection (not a field on the
// action) keeps the action stateless and means a normal Execute and a host
// DeliverAndResume read the mailbox through the identical seam.
type signalStoreCtxKey struct{}

func withSignalStore(ctx context.Context, ss SignalStore) context.Context {
	return context.WithValue(ctx, signalStoreCtxKey{}, ss)
}

// signalStoreFrom extracts the injected SignalStore, returning nil when none was
// injected (a non-SignalStore Store, or DAG.Execute driven directly).
func signalStoreFrom(ctx context.Context) SignalStore {
	if ss, ok := ctx.Value(signalStoreCtxKey{}).(SignalStore); ok {
		return ss
	}
	return nil
}

// consumedSignals collects the sig.IDs a run consumed, so Workflow.Execute can ack
// them AFTER the consuming completion is durable (the take→apply→Completed→
// checkpoint→ack ordering, D37-04). It is created fresh per Execute and threaded on
// ctx; the action appends as it consumes. Concurrency-safe because a level runs its
// nodes in parallel goroutines.
type consumedSignals struct {
	mu  sync.Mutex
	ids []string
}

func (c *consumedSignals) add(id string) {
	c.mu.Lock()
	c.ids = append(c.ids, id)
	c.mu.Unlock()
}

func (c *consumedSignals) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.ids
	c.ids = nil
	return out
}

type consumedSignalsCtxKey struct{}

func withConsumedSignals(ctx context.Context, c *consumedSignals) context.Context {
	return context.WithValue(ctx, consumedSignalsCtxKey{}, c)
}

func consumedSignalsFrom(ctx context.Context) *consumedSignals {
	if c, ok := ctx.Value(consumedSignalsCtxKey{}).(*consumedSignals); ok {
		return c
	}
	return nil
}

// waitForSignalAction is a declared suspension node (D37-01): it parks Waiting
// until a named signal lands in the durable mailbox, then applies the payload
// idempotently and completes. It reuses the chunk-1 seam — node.Execute honors its
// ErrSuspended return as a PARK (not a failure) because it implements the
// unexported suspendable() marker, bypassing retry/timeout.
type waitForSignalAction struct {
	// nodeName keys the idempotent apply. The action carries its own name because
	// Action.Execute is not given the node name; the constructor wires it.
	nodeName string
	// signalName is the name this node waits for in the mailbox.
	signalName string
}

func (a *waitForSignalAction) suspendable() {}

// Execute is the take-or-park decision. Re-checks the mailbox on EVERY invocation
// (so resume re-runs it correctly — Waiting is non-terminal): TakeSignals
// (non-destructive) → if the named signal is absent, park (ErrSuspended) → if
// present, apply the payload idempotently and complete (nil). The ack is NOT done
// here — it happens after the Completed status is durably checkpointed
// (Workflow.Execute drains the consumed collector), so a crash before the
// checkpoint re-runs this node and re-applies the same idempotent write (D37-04).
func (a *waitForSignalAction) Execute(ctx context.Context, data *WorkflowData) error {
	ss := signalStoreFrom(ctx)
	if ss == nil {
		return ErrWaitRequiresSignalStore
	}
	sigs, err := ss.TakeSignals(data.GetWorkflowID())
	if err != nil {
		return err
	}
	for _, sig := range sigs {
		if sig.Name != a.signalName {
			continue
		}
		// Idempotent apply (D37-05): a pure, set-not-accumulate data write to a
		// deterministic key derived from (workflowID, nodeName) via the
		// IdempotencyKey construction (resume-stable; re-applying the same signal
		// writes byte-identical data). Also surfaced as the node's output so
		// dependents can read it naturally; both writes are overwrites, so
		// re-application stays byte-identical (NoDoubleApply).
		key := IdempotencyKey(data, a.nodeName)
		data.Set(key, sig.Payload)
		data.SetOutput(a.nodeName, sig.Payload)
		if c := consumedSignalsFrom(ctx); c != nil {
			c.add(sig.ID)
		}
		return nil
	}
	return ErrSuspended
}

// NewWaitForSignalNode builds a declared WaitForSignalNode: when reached it parks
// the run (Waiting) until a signal named signalName is delivered to the workflow's
// mailbox, then applies the payload and converges. The action is set DIRECTLY (not
// via the middleware stack) so the suspension marker stays visible to node.Execute
// (middleware wrapping would hide it — chunk-1 forward constraint).
func NewWaitForSignalNode(name, signalName string) *Node {
	return NewNode(name, &waitForSignalAction{nodeName: name, signalName: signalName})
}

// waitForConditionAction is a declared suspension node (D37-08, "await"): it parks
// while a predicate over the workflow data is false and completes when it flips.
// It rides the exact wait/wake machinery — re-evaluated on every invocation, so a
// host re-drive (Execute/DeliverAndResume) re-checks it. No new mechanism beyond
// the chunk-1 suspend seam + a host-driven re-evaluation.
type waitForConditionAction struct {
	predicate func(*WorkflowData) bool
}

func (a *waitForConditionAction) suspendable() {}

// Execute parks (ErrSuspended) while the predicate is false; completes (nil) when
// it is true. The predicate is re-evaluated every invocation so a resume/re-drive
// re-checks it against the current data.
func (a *waitForConditionAction) Execute(_ context.Context, data *WorkflowData) error {
	if a.predicate(data) {
		return nil
	}
	return ErrSuspended
}

// NewWaitForConditionNode builds a declared WaitForConditionNode: when reached it
// parks the run while predicate(data) is false, re-evaluating on each wake, and
// converges when it flips. Set directly (not via middleware) to keep the marker
// visible (chunk-1 forward constraint).
func NewWaitForConditionNode(name string, predicate func(*WorkflowData) bool) *Node {
	return NewNode(name, &waitForConditionAction{predicate: predicate})
}

// DeliverSignal durably enqueues sig to this workflow's mailbox (enqueue-only —
// the server-less contract, D37-03). It succeeds with no process running and
// whether or not the instance exists yet (early-signal buffering). It does NOT
// drive the workflow; waking is a separate Execute (use DeliverAndResume for the
// enqueue-then-drive convenience). Returns ErrWaitRequiresSignalStore if the Store
// is not a SignalStore.
func (w *Workflow) DeliverSignal(sig Signal) error {
	ss, ok := w.Store.(SignalStore)
	if !ok {
		return ErrWaitRequiresSignalStore
	}
	return ss.DeliverSignal(w.WorkflowID, sig)
}

// DeliverAndResume durably enqueues sig and then drives the workflow in-process
// (enqueue then Execute) — the convenience for a host that wants to deliver and
// react in one call (D37-03). The two steps are distinct: the deliver is durable
// on its own, and a crash between them just means the signal is buffered for the
// next drive (no loss). NO background scheduler is involved.
func (w *Workflow) DeliverAndResume(ctx context.Context, sig Signal) error {
	// Deliver first WITHOUT the drive lease (the mailbox write is independently
	// safe under the store's own lock; delivery is decoupled from the drive).
	if err := w.DeliverSignal(sig); err != nil {
		return err
	}
	// Then acquire the per-WorkflowID lease at this public drive entry and delegate
	// to the lease-free executeLocked (single-funnel discipline — does NOT call
	// public Execute, so the non-reentrant lease is acquired exactly once).
	release, err := w.locker().Acquire(ctx, w.WorkflowID)
	if err != nil {
		return err
	}
	defer release()
	return w.executeLocked(ctx)
}
