package workflow

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-025 (ergonomic follow-up) — ApprovalNonceFromStore. A dispatcher / signal pump /
// competing-consumer driver delivers approvals by (store, workflowID) and holds no live
// *Workflow, so it cannot call (*Workflow).ApprovalNonce. Rather than force it to rebuild
// the graph purely to read the DefinitionDigest, ApprovalNonceFromStore reads the digest
// the executor already stamped into the durable state and derives the identical nonce the
// engine expects. These tests pin: (1) the store-derived nonce equals the live one and is
// engine-accepted end-to-end, on BOTH the in-memory and the SQLite parked path (the AF1
// path that once dropped the digest); (2) it errors, rather than returns a doomed nonce,
// before the run has stamped a digest; (3) input guards.

func TestApprovalNonceFromStore_MatchesLiveWorkflow(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-nfs-match", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	got, err := ApprovalNonceFromStore(store, "wf-nfs-match", "gate")
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, w.ApprovalNonce("gate"), got,
		"the store-derived nonce must equal the live (*Workflow).ApprovalNonce")
}

func TestApprovalNonceFromStore_DeliversValidApproval(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-nfs-deliver", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Derive the nonce WITHOUT the live workflow, exactly as a store-only driver would.
	nonce, err := ApprovalNonceFromStore(store, "wf-nfs-deliver", "gate")
	require.NoError(t, err)

	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ship it", "d1", nonce)),
		"a store-derived nonce must be accepted by the engine")
	final, err := store.Load("wf-nfs-deliver")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

// TestApprovalNonceFromStore_SQLiteParkedPath is the strongest test: it derives the nonce
// from a SQLite (incremental) store AFTER the run parks on the approval — the exact parked-
// checkpoint path that AF1 once dropped the digest from. A match here proves the digest
// survives the park AND that the store-only seam works on the durable store a real
// dispatcher uses.
func TestApprovalNonceFromStore_SQLiteParkedPath(t *testing.T) {
	store, err := NewSQLiteStore(t.TempDir()+"/wf.db", WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // test cleanup

	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-nfs-sqlite", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	nonce, err := ApprovalNonceFromStore(store, "wf-nfs-sqlite", "gate")
	require.NoError(t, err)
	require.Equal(t, w.ApprovalNonce("gate"), nonce,
		"the SQLite parked-path digest must survive and yield the identical nonce (AF1 regression guard)")

	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "d1", nonce)))
	final, err := store.Load("wf-nfs-sqlite")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
}

func TestApprovalNonceFromStore_ErrorsBeforeStamp(t *testing.T) {
	store := NewInMemoryStore()

	// An id the store has never seen → the store's ErrNotFound propagates unchanged.
	_, err := ApprovalNonceFromStore(store, "never-run", "gate")
	require.ErrorIs(t, err, ErrNotFound,
		"an unknown workflow id must surface the store's ErrNotFound, not a bogus nonce")
}

func TestApprovalNonceFromStore_ErrorsWhenDigestUnstamped(t *testing.T) {
	store := NewInMemoryStore()
	// Persist a bare WorkflowData that never ran (no executor stamp), so Load succeeds but
	// the digest is absent. The helper must refuse rather than derive a mismatching nonce.
	data := NewWorkflowData("wf-nfs-nostamp")
	require.NoError(t, store.Save(data))

	_, err := ApprovalNonceFromStore(store, "wf-nfs-nostamp", "gate")
	require.ErrorIs(t, err, ErrValidation,
		"a run with no stamped digest must yield a typed error, prompting the caller to retry once parked")
}

func TestApprovalNonceFromStore_NilStore(t *testing.T) {
	_, err := ApprovalNonceFromStore(nil, "wf", "gate")
	require.ErrorIs(t, err, ErrValidation)
}
