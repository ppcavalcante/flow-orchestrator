package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// M19 ph96 coverage earn-back — BITING tests for the SignalStore fail-safe error paths
// (the completion-signal mailbox backing sub-workflow park/wake). Each asserts the REAL
// fail-safe behavior of an edge the happy-path suites don't reach: a typed error, a
// fail-safe reject, never a phantom success. These are not coverage-only call-throughs —
// remove the guard each targets and the assertion reddens.

// --- decodeApprovalDecision: every reject path fails SAFE (never a phantom approve) ---

func TestDecodeApprovalDecision_RejectPaths_FailSafe(t *testing.T) {
	// A nil *ApprovalDecision → reject (not a zero-value phantom approve).
	var nilPtr *ApprovalDecision
	_, err := decodeApprovalDecision(nilPtr)
	require.ErrorIs(t, err, ErrValidation, "nil *ApprovalDecision must reject, not approve")

	// An unexpected payload type → reject.
	_, err = decodeApprovalDecision(42)
	require.ErrorIs(t, err, ErrValidation, "unexpected payload type must reject")

	// A bare-scalar JSON string (not an object) → reject, never a phantom approve.
	_, err = decodeApprovalDecision("123")
	require.ErrorIs(t, err, ErrValidation, "a non-object JSON string must reject")

	// The empty string (FB nil-payload encoding) → reject.
	_, err = decodeApprovalDecision("")
	require.ErrorIs(t, err, ErrValidation, "empty payload must reject")

	// An undecodable string → reject.
	_, err = decodeApprovalDecision("{not json")
	require.ErrorIs(t, err, ErrValidation, "undecodable string must reject")

	// A well-formed *ApprovalDecision → passes through (non-vacuity: the reject paths
	// above aren't rejecting *everything*).
	ok := &ApprovalDecision{Approved: true}
	got, err := decodeApprovalDecision(ok)
	require.NoError(t, err)
	require.True(t, got.Approved, "a valid decision must decode through")
}

// --- decodeSignalJSON / decodeSignalFB: corrupt bytes → typed ErrCorruptData ---

func TestDecodeSignal_CorruptBytes_ErrCorruptData(t *testing.T) {
	_, err := decodeSignalJSON([]byte("{not valid json"))
	require.ErrorIs(t, err, ErrCorruptData, "corrupt JSON signal → ErrCorruptData")

	_, err = decodeSignalFB([]byte("\x00\x01not-a-flatbuffer"))
	require.Error(t, err, "corrupt FB signal must error, never silently decode")
}

// --- decodeSignalFB: the totality-hardening arms each reject as ErrCorruptData (never a host panic) ---

func TestDecodeSignalFB_HardeningArms(t *testing.T) {
	// (a) a too-short buffer (< SizeUOffsetT) → the length pre-check rejects.
	_, err := decodeSignalFB([]byte{0x01})
	require.ErrorIs(t, err, ErrCorruptData, "a short FB buffer must reject, not index out of bounds")

	// (b) a buffer whose root offset points past its end → the root-offset pre-check rejects.
	_, err = decodeSignalFB([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	require.ErrorIs(t, err, ErrCorruptData, "an out-of-range root offset must reject")

	// (c) a well-formed round-trip decodes clean (non-vacuity — the arms above aren't rejecting all).
	buf, err := encodeSignalFB(Signal{ID: "s1", Name: "n", Payload: "p"})
	require.NoError(t, err)
	got, err := decodeSignalFB(buf)
	require.NoError(t, err)
	require.Equal(t, "s1", got.ID)
}

// --- marshalSignalPayload / encodeSignalFB: an unmarshalable payload → ErrValidation (propagates) ---

func TestMarshalSignalPayload_Unencodable_Rejects(t *testing.T) {
	// A channel cannot be JSON-marshaled → marshalSignalPayload rejects with ErrValidation.
	_, err := marshalSignalPayload(make(chan int))
	require.ErrorIs(t, err, ErrValidation, "an unencodable payload must reject")

	// The same error propagates through encodeSignalFB (which marshals the payload first).
	_, err = encodeSignalFB(Signal{ID: "s1", Payload: make(chan int)})
	require.ErrorIs(t, err, ErrValidation, "encodeSignalFB propagates an unencodable-payload error")

	// nil payload is fine (the happy edge) — non-vacuity.
	s, err := marshalSignalPayload(nil)
	require.NoError(t, err)
	require.Equal(t, "", s, "a nil payload encodes to the empty string")
}

// --- unmarshalSignalPayload: corrupt JSON → ErrCorruptData; empty → nil ---

func TestUnmarshalSignalPayload_Edges(t *testing.T) {
	v, err := unmarshalSignalPayload("")
	require.NoError(t, err)
	require.Nil(t, v, "empty payload → nil, no error")

	_, err = unmarshalSignalPayload("{corrupt")
	require.ErrorIs(t, err, ErrCorruptData, "corrupt payload JSON → ErrCorruptData")
}

// --- seedInput: a non-JSON-object input → ErrValidation (the queue-child seed guard) ---

func TestSeedInput_NonObject_Rejects(t *testing.T) {
	d := NewWorkflowData("wf")
	// A bare JSON scalar is not an object → reject.
	require.ErrorIs(t, seedInput(d, []byte("42")), ErrValidation, "a non-object input must reject")
	require.ErrorIs(t, seedInput(d, []byte("not json")), ErrValidation, "malformed input must reject")

	// A valid JSON object seeds the KV (non-vacuity).
	require.NoError(t, seedInput(d, []byte(`{"k":"v"}`)))
	got, ok := d.Get("k")
	require.True(t, ok)
	require.Equal(t, "v", got)
}

// --- deliverSignalToDir: the validation + encode guards fail SAFE ---

func TestDeliverSignalToDir_Guards(t *testing.T) {
	dir := t.TempDir()

	// Invalid workflowID (traversal) → reject before any write.
	err := deliverSignalToDir(dir, "../escape", Signal{ID: "s1"}, encodeSignalJSON)
	require.ErrorIs(t, err, ErrValidation, "traversal workflowID must reject")

	// Invalid signal ID (separator) → reject.
	err = deliverSignalToDir(dir, "wf", Signal{ID: "a/b"}, encodeSignalJSON)
	require.ErrorIs(t, err, ErrValidation, "separator in signal ID must reject")

	// An encode failure propagates (a codec that errors → the delivery errors, no partial write).
	boom := errors.New("encode boom")
	err = deliverSignalToDir(dir, "wf", Signal{ID: "s1"}, func(Signal) ([]byte, error) { return nil, boom })
	require.ErrorIs(t, err, boom, "an encode error must propagate, not swallow")
	// No mailbox dir was created for the failed delivery.
	_, statErr := os.Stat(filepath.Join(dir, "wf"+signalDirSuffix))
	require.True(t, os.IsNotExist(statErr), "a failed delivery leaves no mailbox dir")
}

// --- takeSignalsFromDir: the mailbox-cap guard BITES (oversized mailbox → ErrCorruptData) ---

func TestTakeSignalsFromDir_MailboxCap_Bites(t *testing.T) {
	dir := t.TempDir()
	wf := "cap-wf"
	// Deliver 3 real signals.
	for _, id := range []string{"s1", "s2", "s3"} {
		require.NoError(t, deliverSignalToDir(dir, wf, Signal{ID: id, Name: "n"}, encodeSignalJSON))
	}
	// Temporarily lower the cap below the entry count → the guard must fire.
	orig := signalMailboxCap
	signalMailboxCap = 2
	defer func() { signalMailboxCap = orig }()
	_, err := takeSignalsFromDir(dir, wf, decodeSignalJSON)
	require.ErrorIs(t, err, ErrCorruptData, "an over-cap mailbox must reject (DoS guard), not iterate")

	// Restore the cap → the same mailbox reads clean (proves the bite was the cap, not the data).
	signalMailboxCap = orig
	sigs, err := takeSignalsFromDir(dir, wf, decodeSignalJSON)
	require.NoError(t, err)
	require.Len(t, sigs, 3, "under the cap the mailbox reads all entries")
}

// --- takeSignalsFromDir: a corrupt entry file → the decode error surfaces (no silent skip) ---

func TestTakeSignalsFromDir_CorruptEntry_Surfaces(t *testing.T) {
	dir := t.TempDir()
	wf := "corrupt-wf"
	require.NoError(t, deliverSignalToDir(dir, wf, Signal{ID: "good", Name: "n"}, encodeSignalJSON))
	// Hand-write a corrupt entry into the mailbox dir.
	mbox := filepath.Join(dir, wf+signalDirSuffix)
	require.NoError(t, os.WriteFile(filepath.Join(mbox, "bad"+signalFileSuffix), []byte("{garbage"), 0600))
	_, err := takeSignalsFromDir(dir, wf, decodeSignalJSON)
	require.ErrorIs(t, err, ErrCorruptData, "a corrupt mailbox entry must surface, never be silently dropped")
}

// --- ackSignalsInDir: an invalid ID rejects (a traversal ack must not escape the mailbox) ---

func TestAckSignalsInDir_InvalidID_Rejects(t *testing.T) {
	dir := t.TempDir()
	err := ackSignalsInDir(dir, "wf", []string{"../escape"})
	require.ErrorIs(t, err, ErrValidation, "a traversal ack ID must reject")

	// A genuinely-absent (but well-formed) ID is idempotent — not an error.
	require.NoError(t, ackSignalsInDir(dir, "wf", []string{"never-delivered"}),
		"acking an absent well-formed ID is idempotent, not an error")
}

// --- removeSignalDir: invalid workflowID rejects; absent dir is idempotent ---

func TestRemoveSignalDir_Guards(t *testing.T) {
	dir := t.TempDir()
	require.ErrorIs(t, removeSignalDir(dir, "../escape"), ErrValidation, "traversal workflowID must reject")
	require.NoError(t, removeSignalDir(dir, "never-existed"), "removing an absent mailbox is idempotent")
}

// --- SQLiteStore SignalStore: validation guards + the mailbox-cap DoS guard fail SAFE ---
// (the M19 SQLite completion-signal mailbox — the queue-child park/wake channel).

func TestSQLiteSignals_Guards_FailSafe(t *testing.T) {
	s := mustSQLiteSignals(t)

	// DeliverSignal rejects a traversal workflowID and an invalid signal ID BEFORE any write.
	require.ErrorIs(t, s.DeliverSignal("../escape", Signal{ID: "s1"}), ErrValidation, "traversal workflowID rejects")
	require.ErrorIs(t, s.DeliverSignal("wf", Signal{ID: "a/b"}), ErrValidation, "separator sig ID rejects")
	require.ErrorIs(t, s.DeliverSignal("wf", Signal{ID: ""}), ErrValidation, "empty sig ID rejects")

	// TakeSignals rejects a traversal workflowID.
	_, err := s.TakeSignals("../escape")
	require.ErrorIs(t, err, ErrValidation, "TakeSignals traversal workflowID rejects")

	// AckSignals rejects a traversal workflowID.
	require.ErrorIs(t, s.AckSignals("../escape", []string{"s1"}), ErrValidation, "AckSignals traversal workflowID rejects")

	// Non-vacuity: a valid deliver→take→ack round-trips (the guards above aren't rejecting everything).
	require.NoError(t, s.DeliverSignal("wf", Signal{ID: "s1", Name: "n", Payload: "yes"}))
	got, err := s.TakeSignals("wf")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "yes", got[0].Payload, "a valid signal round-trips")
	require.NoError(t, s.AckSignals("wf", []string{"s1"}))
	after, err := s.TakeSignals("wf")
	require.NoError(t, err)
	require.Empty(t, after, "acked signal is removed")
}

func TestSQLiteSignals_MailboxCap_Bites(t *testing.T) {
	s := mustSQLiteSignals(t)
	for _, id := range []string{"s1", "s2", "s3"} {
		require.NoError(t, s.DeliverSignal("capwf", Signal{ID: id, Name: "n"}))
	}
	orig := signalMailboxCap
	signalMailboxCap = 2
	defer func() { signalMailboxCap = orig }()
	_, err := s.TakeSignals("capwf")
	require.ErrorIs(t, err, ErrCorruptData, "an over-cap SQLite mailbox rejects (DoS guard), not iterates")

	signalMailboxCap = orig
	got, err := s.TakeSignals("capwf")
	require.NoError(t, err)
	require.Len(t, got, 3, "under the cap the SQLite mailbox reads all entries")
}

// --- Workflow.DeliverSignal / DeliverAndResume: a non-SignalStore fails LOUD (never a silent no-op) ---

func TestDeliverSignal_NonSignalStore_LoudReject(t *testing.T) {
	// A store that is NOT a SignalStore → the signal APIs must reject with ErrWaitRequiresSignalStore,
	// never silently drop the signal (a dropped completion signal = a forever-parked parent).
	w := NewWorkflow(&nonSignalStore{})
	w.WorkflowID = "wf"
	require.ErrorIs(t, w.DeliverSignal(Signal{ID: "s1"}), ErrWaitRequiresSignalStore,
		"DeliverSignal on a non-SignalStore must reject loud")
	require.ErrorIs(t, w.DeliverAndResume(context.Background(), Signal{ID: "s1"}), ErrWaitRequiresSignalStore,
		"DeliverAndResume must fail at the deliver step on a non-SignalStore")
}

// nonSignalStore is a minimal WorkflowStore that deliberately does NOT implement SignalStore, so the
// DeliverSignal type-assertion fails — exercising the loud-reject guard.
type nonSignalStore struct{}

func (n *nonSignalStore) Save(*WorkflowData) error           { return nil }
func (n *nonSignalStore) Load(string) (*WorkflowData, error) { return nil, ErrNotFound }
func (n *nonSignalStore) ListWorkflows() ([]string, error)   { return nil, nil }
func (n *nonSignalStore) Delete(string) error                { return nil }

// --- queueSubWorkflowAction.Execute: a queue-dispatched child on a NON-SQLite store fails LOUD ---
// (the ¬{P} guard — the queue path structurally needs the MP SQLite store; a wrong store must be a
// typed error at run, never a silent park-forever).

func TestQueueSubWorkflow_NonSQLiteStore_LoudReject(t *testing.T) {
	// Register a child type so the Registry lookup isn't the thing that fails.
	reg := NewRegistry()
	require.NoError(t, reg.Register("child", func() (*DAG, error) {
		cb := NewWorkflowBuilder()
		cb.AddStartNode("c").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		return cb.Build()
	}))

	pb := NewWorkflowBuilder().WithWorkflowID("q-nonsqlite")
	pb.AddSubWorkflowQueued("sub", "child").WithResult("r", "r")
	dag, err := pb.Build()
	require.NoError(t, err)

	// InMemoryStore IS a SignalStore (passes that guard) but is NOT a *SQLiteStore → the queue
	// action must reject with ErrValidation, not park.
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "q-nonsqlite"
	w.DAG = dag
	w.Registry = reg
	execErr := w.Execute(context.Background())
	require.Error(t, execErr, "a queue sub-workflow on a non-SQLite store must fail, not park")
	require.ErrorIs(t, execErr, ErrValidation, "the non-SQLite queue store is a typed ErrValidation")
}

// --- queueSubWorkflowAction.Execute: no Registry → ErrSubWorkflowRequiresRegistry (loud, not park) ---

func TestQueueSubWorkflow_NoRegistry_LoudReject(t *testing.T) {
	s := mustSQLiteSignals(t)
	pb := NewWorkflowBuilder().WithWorkflowID("q-noreg")
	pb.AddSubWorkflowQueued("sub", "child").WithResult("r", "r")
	dag, err := pb.Build()
	require.NoError(t, err)

	// A SQLiteStore (passes the store guards) but NO Registry injected → the queue action must
	// reject with ErrSubWorkflowRequiresRegistry, never park forever on an unresolvable type.
	w := NewWorkflow(s)
	w.WorkflowID = "q-noreg"
	w.DAG = dag
	// w.Registry deliberately nil.
	execErr := w.Execute(context.Background())
	require.ErrorIs(t, execErr, ErrSubWorkflowRequiresRegistry, "no Registry → loud reject, not park")
}

// --- applyResult: a declared result key absent in the child → ErrValidation (the result contract) ---

func TestSubWorkflow_DeclaredResultKeyAbsent_Rejects(t *testing.T) {
	// The child produces "actual" but the parent declares WithResult reading "missing" — the
	// result contract must reject (a declared-but-absent key is a build/wiring error, not a
	// silent empty result).
	cb := NewWorkflowBuilder()
	cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("actual", "v")
		return nil
	}))
	child, err := cb.Build()
	require.NoError(t, err)

	pb := NewWorkflowBuilder().WithWorkflowID("res-absent")
	pb.AddSubWorkflow("sub", child).WithResult("r", "missing") // reads a key the child never sets
	dag, err := pb.Build()
	require.NoError(t, err)

	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "res-absent"
	w.DAG = dag
	execErr := w.Execute(context.Background())
	require.ErrorIs(t, execErr, ErrValidation, "a declared-but-absent child result key must reject")
}
