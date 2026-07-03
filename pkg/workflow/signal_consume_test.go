package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSignalConsume_CrashBeforeCheckpoint_ReAppliesIdempotent is the consume-
// ordering crash-window bite (MH37-2, D37-04): a process crash AFTER a signal node
// applies + completes but BEFORE the durable checkpoint must NOT lose the signal —
// on resume the node re-takes the (still-unacked) mailbox and re-applies the SAME
// payload idempotently, converging to the no-crash final.
//
// Modeled the M9 way: drive DAG.Execute directly with a checkpoint that fails
// (the durability flush "crashes"), discard the in-memory data (true process
// death — no failure-save persists), then resume from the store with a clean
// checkpoint. The ack lives in Workflow.Execute and never runs on this aborted
// drive, so the signal is still in the mailbox for the resume — which is exactly
// the no-loss guarantee (ack-AFTER-checkpoint). The dual mutation (ack BEFORE the
// checkpoint) would drain the signal pre-durability and the resume would park
// forever; that falsification is proven formally in the TLA NoSignalLost bite.
func TestSignalConsume_CrashBeforeCheckpoint_ReAppliesIdempotent(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-consume-crash"
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "g1", Name: "go", Payload: "V"}))

	build := func() *DAG {
		d := NewDAG(wf)
		require.NoError(t, d.AddNode(NewWaitForSignalNode("wait", "go")))
		return d
	}

	// --- Crash drive: the consume's durability flush fails. ---
	crashCP := func(*WorkflowData) error { return errors.New("crash before persist") }
	ctx1 := withSignalStore(withCheckpoint(context.Background(), crashCP), store)
	err := crashDAGExecute(build(), ctx1, NewWorkflowData(wf))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSuspended, "the node applied + completed, then the flush crashed (not a park)")

	// No loss: the signal is still in the mailbox (never acked on the aborted drive).
	sigs, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, sigs, 1, "crash before durable must NOT lose the signal")

	// --- Resume: clean checkpoint → re-apply idempotently, converge. ---
	data2 := NewWorkflowData(wf)
	cleanCP := func(d *WorkflowData) error { return store.SaveCheckpoint(d) }
	ctx2 := withSignalStore(withCheckpoint(context.Background(), cleanCP), store)
	require.NoError(t, build().Execute(ctx2, data2), "resume re-applies the unacked signal and converges")

	key := IdempotencyKey(data2, "wait")
	v, ok := data2.Get(key)
	require.True(t, ok)
	assert.Equal(t, "V", v, "the re-applied value is byte-identical to a no-crash run (idempotent)")
}

// crashDAGExecute drives d and returns the error, isolating the //nolint for the
// intentionally-crashing checkpoint path.
func crashDAGExecute(d *DAG, ctx context.Context, data *WorkflowData) error {
	return d.Execute(ctx, data)
}

// TestSignalConsume_IdempotentApplyStableKey is the idempotent-apply bite (D37-05):
// the apply key is deterministic from (workflowID, nodeName) via IdempotencyKey —
// NOT per-attempt — so re-consuming the same signal (a resume re-running the node)
// writes byte-identical data to the SAME key. A per-attempt key (the should-fail
// mutation) would scatter each apply to a different key, leaving the no-double
// assertion below unsatisfiable: only the stable key keeps the observable apply
// single-valued across attempts.
func TestSignalConsume_IdempotentApplyStableKey(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-idem-apply"
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: int64(42)}))

	build := func() *DAG {
		d := NewDAG(wf)
		require.NoError(t, d.AddNode(NewWaitForSignalNode("wait", "go")))
		return d
	}
	cp := func(d *WorkflowData) error { return store.SaveCheckpoint(d) }

	// Apply once.
	data1 := NewWorkflowData(wf)
	ctx := withSignalStore(withCheckpoint(context.Background(), cp), store)
	require.NoError(t, build().Execute(ctx, data1))
	key1 := IdempotencyKey(data1, "wait")
	v1, ok := data1.Get(key1)
	require.True(t, ok)

	// Re-consume the SAME signal id from a fresh run (the signal is re-delivered to
	// model a crash-resume re-take; same ID → idempotent at the mailbox too).
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: int64(42)}))
	data2 := NewWorkflowData(wf)
	require.NoError(t, build().Execute(ctx, data2))
	key2 := IdempotencyKey(data2, "wait")
	v2, _ := data2.Get(key2)

	// The key is stable across attempts (deterministic, not per-attempt) ...
	assert.Equal(t, key1, key2, "the apply key is deterministic from (workflowID,nodeName), not per-attempt")
	// ... and the applied value is byte-identical (no-double-apply).
	gotV1, e1 := payloadAsInt64(v1)
	require.NoError(t, e1)
	gotV2, e2 := payloadAsInt64(v2)
	require.NoError(t, e2)
	assert.Equal(t, int64(42), gotV1)
	assert.Equal(t, gotV1, gotV2, "re-apply writes byte-identical data to the same key")
}
