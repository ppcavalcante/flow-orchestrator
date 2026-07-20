package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildApprovalWorkflow wires an approval node "gate" with a downstream "after" node
// (whose runs are counted via counter, so a test can assert downstream did/did not
// run) over store, with workflow id wf. The decision signal name equals the node name.
func buildApprovalWorkflow(t *testing.T, store WorkflowStore, wf string, counter *atomic.Int32) *Workflow {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID(wf)
	wb.AddApproval("gate")
	wb.AddNode("after").DependsOn("gate").WithAction(countingAction(counter))
	dag, err := wb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = wf
	w.DAG = dag
	return w
}

// TestApproval_ParkThenApprove — hard-bar core: an approval node parks (Waiting); an
// approve decision resumes to terminal success with approver + comment readable in
// both the node output and the journal (IdempotencyKey) for the audit trail.
func TestApproval_ParkThenApprove(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-appr", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended, "an undelivered decision parks")
	parked, err := store.Load("wf-appr")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "gate", Waiting)
	assertNodeStatus(t, parked, "after", Pending)
	require.EqualValues(t, 0, afterN.Load(), "downstream must not run while parked")

	// Deliver an approve decision (a typed payload; in-process, no durable round-trip).
	require.NoError(t, w.DeliverAndResume(context.Background(), ApproveSignal("gate", "alice", "ship it", "d1")))

	final, err := store.Load("wf-appr")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load(), "downstream runs once after approve")

	// Approver + comment readable via the node OUTPUT and via the journal key.
	out, ok := final.GetOutput("gate")
	require.True(t, ok, "the applied decision is surfaced as the node output")
	gotOut, derr := decodeApprovalDecision(out)
	require.NoError(t, derr)
	assert.Equal(t, "alice", gotOut.Approver)
	assert.Equal(t, "ship it", gotOut.Comment)
	assert.True(t, gotOut.Approved)

	journalVal, jok := final.Get(IdempotencyKey(final, "gate"))
	require.True(t, jok, "the applied decision is persisted to the journal (audit)")
	gotJournal, jerr := decodeApprovalDecision(journalVal)
	require.NoError(t, jerr)
	assert.Equal(t, "alice", gotJournal.Approver)
}

// TestApproval_Reject_FailFast — INV-01: a reject decision fails the run with a typed
// *ApprovalRejectedError (classifiable via errors.As, carrying approver+comment) and
// NO downstream node runs. Bite: removing the reject branch makes this redden.
func TestApproval_Reject_FailFast(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-rej", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	err := w.DeliverAndResume(context.Background(), RejectSignal("gate", "bob", "no budget", "d1"))
	require.Error(t, err, "a reject must fail the run")

	var rejErr *ApprovalRejectedError
	require.True(t, errors.As(err, &rejErr), "the failure is classifiable as *ApprovalRejectedError")
	assert.Equal(t, "gate", rejErr.Node)
	assert.Equal(t, "bob", rejErr.Approver)
	assert.Equal(t, "no budget", rejErr.Comment)

	require.EqualValues(t, 0, afterN.Load(), "no downstream node runs after a reject (fail-fast)")
	// A rejection wraps no cause — errors.As never reaches a *ApprovalRejectedError
	// through a spurious Unwrap chain (DEC-P90-REJECT-ERR-NO-UNWRAP): the field read
	// above IS the classification path.
}

// TestApproval_NoSignalStore_LoudFail — seed-break (honesty half): an approval node
// on a store that does NOT implement SignalStore fails loudly with
// ErrWaitRequiresSignalStore, never a silent forever-park. Removing the ss==nil guard
// in approvalAction.Execute must stop this test reddening.
func TestApproval_NoSignalStore_LoudFail(t *testing.T) {
	// Drive the action directly through DAG.Execute with no SignalStore injected —
	// the same shape as TestWaitForSignal_RequiresSignalStore.
	d := NewDAG("wf-nostore")
	require.NoError(t, d.AddNode(NewNode("gate", &approvalAction{nodeName: "gate", signalName: "gate"})))
	cp := func(*WorkflowData) error { return nil }
	err := d.Execute(withCheckpoint(context.Background(), cp), NewWorkflowData("wf-nostore"))
	require.ErrorIs(t, err, ErrWaitRequiresSignalStore, "no SignalStore → loud failure, never a forever-park")
}

// TestApproval_DecodeAfterDurableRoundTrip — the payload landmine: on a FILE-backed
// SignalStore the delivered ApprovalDecision survives a JSON round-trip as a generic
// map[string]any (UseNumber), NOT the typed struct. The defensive decoder must still
// yield the decision. Bite: replacing decodeApprovalDecision with a naive
// sig.Payload.(ApprovalDecision) assertion makes this redden.
func TestApproval_DecodeAfterDurableRoundTrip(t *testing.T) {
	store, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-rt", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Deliver, then re-drive as two separate steps so the payload is written to and
	// read back from disk (the durable round-trip that generalizes the payload).
	require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "carol", "lgtm", "d1")))
	require.NoError(t, w.Execute(context.Background()), "the defensively-decoded round-tripped decision resumes")

	final, err := store.Load("wf-rt")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())

	out, ok := final.GetOutput("gate")
	require.True(t, ok)
	got, derr := decodeApprovalDecision(out)
	require.NoError(t, derr)
	assert.True(t, got.Approved)
	assert.Equal(t, "carol", got.Approver)
}

// TestDecodeApprovalDecision — the defensive decoder in isolation, over every shape
// the payload can arrive in plus the reject-on-garbage contract.
func TestDecodeApprovalDecision(t *testing.T) {
	typed := ApprovalDecision{Approved: true, Approver: "a", Comment: "c"}

	got, err := decodeApprovalDecision(typed)
	require.NoError(t, err)
	assert.Equal(t, typed, got)

	got, err = decodeApprovalDecision(&typed)
	require.NoError(t, err)
	assert.Equal(t, typed, got)

	// The post-durable-round-trip shape: a generic map with bool + strings.
	got, err = decodeApprovalDecision(map[string]any{"Approved": false, "Approver": "b", "Comment": "d"})
	require.NoError(t, err)
	assert.Equal(t, ApprovalDecision{Approved: false, Approver: "b", Comment: "d"}, got)

	// A map missing fields decodes to the zero decision (not-approved default) — a
	// missing Approved defaults false, which is fail-safe (reject, not phantom approve).
	got, err = decodeApprovalDecision(map[string]any{"Approver": "b"})
	require.NoError(t, err)
	assert.False(t, got.Approved)
	assert.Equal(t, "b", got.Approver)

	// Garbage / wrong-typed fields → typed ErrValidation, never a phantom approve.
	_, err = decodeApprovalDecision("a bare string")
	require.ErrorIs(t, err, ErrValidation)
	_, err = decodeApprovalDecision(map[string]any{"Approved": "yes"}) // wrong type
	require.ErrorIs(t, err, ErrValidation)
	_, err = decodeApprovalDecision(nil)
	require.ErrorIs(t, err, ErrValidation)
	var nilPtr *ApprovalDecision
	_, err = decodeApprovalDecision(nilPtr)
	require.ErrorIs(t, err, ErrValidation)
}

// TestApproval_NoDoubleApply — idempotency (D37-05): re-driving after an approve
// re-applies BYTE-IDENTICALLY, and a duplicate sig.ID leaves ONE mailbox entry. The
// apply persists the raw payload, so re-application is a pure overwrite of the same
// bytes — NoDoubleApply holds for the same reason the wait seam's does.
func TestApproval_NoDoubleApply(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-idem", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Deliver the same approve twice under the SAME sig.ID → one mailbox entry.
	require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "dana", "ok", "dup")))
	require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "dana", "ok", "dup")))
	pending, err := store.TakeSignals("wf-idem")
	require.NoError(t, err)
	require.Len(t, pending, 1, "a duplicate sig.ID is deduped to one mailbox entry")

	// First drive applies + completes; capture the journal value.
	require.NoError(t, w.Execute(context.Background()))
	first, ferr := appliedApprovalValue(t, store, "wf-idem", "gate")
	require.True(t, ferr)
	require.EqualValues(t, 1, afterN.Load())

	// Re-drive (resume of an already-terminal run) — the applied value is BYTE-IDENTICAL.
	require.NoError(t, w.Execute(context.Background()))
	second, serr := appliedApprovalValue(t, store, "wf-idem", "gate")
	require.True(t, serr)
	assert.Equal(t, first, second, "re-drive re-applies byte-identically (NoDoubleApply)")
	require.EqualValues(t, 1, afterN.Load(), "the downstream node is not re-run on a re-drive of a terminal run")
}

// TestApproval_Reject_ReDriveStable — a rejection is PERMANENT and terminal. The first
// drive fails the node (a *ApprovalRejectedError, wrapped in the standard
// *ExecutionError envelope, errors.As-reachable). The node is durably Failed → the run
// is terminal, so a RE-DRIVE is a no-op: it returns nil, leaves the node Failed, and
// NEVER runs downstream or produces a phantom completion. (This is the stronger
// idempotency contract than the PLAN's initial "re-fires the same error" assumption —
// terminal-failed state is durable, re-drive does not re-execute the failed node.)
func TestApproval_Reject_ReDriveStable(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-rej2", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
	require.NoError(t, w.DeliverSignal(RejectSignal("gate", "erin", "denied", "d1")))

	// First drive: fail-fast with a classifiable *ApprovalRejectedError.
	err1 := w.Execute(context.Background())
	require.Error(t, err1)
	var r1 *ApprovalRejectedError
	require.True(t, errors.As(err1, &r1), "reject wrapped in ExecutionError is still As-reachable")
	assert.Equal(t, "erin", r1.Approver)
	failed, lerr := store.Load("wf-rej2")
	require.NoError(t, lerr)
	assertNodeStatus(t, failed, "gate", Failed)

	// Re-drive: the run is already terminal-failed → no-op (nil), node stays Failed,
	// downstream still never runs. No phantom completion, no re-fired error.
	err2 := w.Execute(context.Background())
	require.NoError(t, err2, "re-driving a terminal-failed run is a no-op")
	final, lerr := store.Load("wf-rej2")
	require.NoError(t, lerr)
	assertNodeStatus(t, final, "gate", Failed)
	require.EqualValues(t, 0, afterN.Load(), "downstream never runs across reject re-drives")
}

// appliedApprovalValue reads back the payload an approval node applied, via the
// deterministic journal key (IdempotencyKey of (workflowID, nodeName)).
func appliedApprovalValue(t *testing.T, store WorkflowStore, wf, nodeName string) (any, bool) {
	t.Helper()
	data, err := store.Load(wf)
	require.NoError(t, err)
	return data.Get(IdempotencyKey(data, nodeName))
}
