package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSignalWorkflow wires a single WaitForSignalNode "wait" waiting on signalName
// over the given store, with workflow id wf.
func buildSignalWorkflow(t *testing.T, store WorkflowStore, wf, signalName string) *Workflow {
	t.Helper()
	w := NewWorkflow(store)
	w.WorkflowID = wf
	require.NoError(t, w.AddNode(NewWaitForSignalNode("wait", signalName)))
	return w
}

// appliedSignalPayload reads back the payload a WaitForSignalNode applied, via the
// deterministic resultKey (IdempotencyKey of (workflowID, nodeName)).
func appliedSignalPayload(t *testing.T, store WorkflowStore, wf, nodeName string) (any, bool) {
	t.Helper()
	data, err := store.Load(wf)
	require.NoError(t, err)
	key := IdempotencyKey(data, nodeName)
	return data.Get(key)
}

// TestWaitForSignal_ParksThenResumesOnDelivery: the core wait→deliver→wake cycle.
// First Execute parks (ErrSuspended, node Waiting); after a delivery the next
// Execute consumes it, applies the payload, and converges.
func TestWaitForSignal_ParksThenResumesOnDelivery(t *testing.T) {
	store := NewInMemoryStore()
	w := buildSignalWorkflow(t, store, "wf-park", "approve")

	err := w.Execute(context.Background())
	require.ErrorIs(t, err, ErrSuspended, "an undelivered signal parks the run")
	persisted, lerr := store.Load("wf-park")
	require.NoError(t, lerr)
	st, _ := persisted.GetNodeStatus("wait")
	assert.Equal(t, Waiting, st, "the node is persisted Waiting while parked")

	require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "approve", Payload: "ok"}))
	require.NoError(t, w.Execute(context.Background()), "delivery wakes the run to convergence")

	final, lerr := store.Load("wf-park")
	require.NoError(t, lerr)
	st, _ = final.GetNodeStatus("wait")
	assert.Equal(t, Completed, st)
	payload, ok := appliedSignalPayload(t, store, "wf-park", "wait")
	require.True(t, ok)
	assert.Equal(t, "ok", payload)
}

// TestWaitForSignal_EarlySignalBuffered (delivery race a): a signal delivered
// BEFORE the wait is ever reached is buffered, so the very first Execute completes
// without ever parking.
func TestWaitForSignal_EarlySignalBuffered(t *testing.T) {
	store := NewInMemoryStore()
	w := buildSignalWorkflow(t, store, "wf-early", "go")

	require.NoError(t, w.DeliverSignal(Signal{ID: "e1", Name: "go", Payload: 7}))
	require.NoError(t, w.Execute(context.Background()), "an early-buffered signal completes the run on first Execute")

	final, _ := store.Load("wf-early") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("wait")
	assert.Equal(t, Completed, st, "never parked — the buffered signal was consumed immediately")
}

// TestWaitForSignal_DoubleDeliverIdempotent (delivery race c): delivering the same
// sig.ID twice yields one mailbox entry and one idempotent apply.
func TestWaitForSignal_DoubleDeliverIdempotent(t *testing.T) {
	store := NewInMemoryStore()
	w := buildSignalWorkflow(t, store, "wf-dup", "go")

	sig := Signal{ID: "d1", Name: "go", Payload: "v"}
	require.NoError(t, w.DeliverSignal(sig))
	require.NoError(t, w.DeliverSignal(sig))
	require.NoError(t, w.Execute(context.Background()))

	final, _ := store.Load("wf-dup") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("wait")
	assert.Equal(t, Completed, st)
	payload, ok := appliedSignalPayload(t, store, "wf-dup", "wait")
	require.True(t, ok)
	assert.Equal(t, "v", payload)
}

// TestWaitForSignal_DeliverAndResume: the enqueue-then-drive convenience completes
// a parked run in one call.
func TestWaitForSignal_DeliverAndResume(t *testing.T) {
	store := NewInMemoryStore()
	w := buildSignalWorkflow(t, store, "wf-dar", "approve")

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	require.NoError(t, w.DeliverAndResume(context.Background(), Signal{ID: "a1", Name: "approve", Payload: "y"}))

	final, _ := store.Load("wf-dar") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("wait")
	assert.Equal(t, Completed, st)
}

// TestWaitForSignal_AckedAfterCompletion: once the consuming node completes (and is
// durably saved), the consumed signal is drained from the mailbox (D37-04 ack).
func TestWaitForSignal_AckedAfterCompletion(t *testing.T) {
	store := NewInMemoryStore()
	w := buildSignalWorkflow(t, store, "wf-ack", "go")

	require.NoError(t, w.DeliverSignal(Signal{ID: "g1", Name: "go", Payload: "p"}))
	require.NoError(t, w.Execute(context.Background()))

	remaining, err := store.TakeSignals("wf-ack")
	require.NoError(t, err)
	assert.Empty(t, remaining, "the consumed signal is acked (drained) after the completion is durable")
}

// TestWaitForSignal_RequiresSignalStore: a WaitForSignalNode driven with no
// SignalStore in scope (DAG.Execute directly, no injection) fails loudly rather
// than parking forever.
func TestWaitForSignal_RequiresSignalStore(t *testing.T) {
	d := NewDAG("wf-nostore")
	require.NoError(t, d.AddNode(NewWaitForSignalNode("wait", "go")))
	cp := func(*WorkflowData) error { return nil }
	err := d.Execute(withCheckpoint(context.Background(), cp), NewWorkflowData("wf-nostore"))
	require.ErrorIs(t, err, ErrWaitRequiresSignalStore)
}

// TestWaitForCondition_ParksUntilPredicateFlips (D37-08, "await"): the node parks
// while its predicate is false and converges once a re-drive sees it true.
func TestWaitForCondition_ParksUntilPredicateFlips(t *testing.T) {
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-cond"
	require.NoError(t, w.AddNode(NewWaitForConditionNode("await", func(d *WorkflowData) bool {
		v, ok := d.Get("ready")
		return ok && v == true
	})))

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended, "predicate false → parks")

	// Flip the predicate by persisting the awaited key, then re-drive.
	data, _ := store.Load("wf-cond") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	data.Set("ready", true)
	require.NoError(t, store.Save(data))
	require.NoError(t, w.Execute(context.Background()), "predicate true → converges")

	final, _ := store.Load("wf-cond") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("await")
	assert.Equal(t, Completed, st)
}
