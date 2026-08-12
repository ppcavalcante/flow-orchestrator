package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-025 — the approval correlation nonce. A decision is consumed only if it
// carries the nonce that correlates it to THIS park; a stale / stray / mis-
// correlated decision is inert (the node keeps waiting), so it can neither approve
// nor reject the run.

func TestAUD025_CorrectNonceApproves(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-n-ok", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	nonce := w.ApprovalNonce("gate")
	require.NotEmpty(t, nonce)
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ship it", "d1", nonce)))

	final, err := store.Load("wf-n-ok")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_WrongNonceIsInert_ThenCorrectApproves(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-n-wrong", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// A decision with a bogus nonce is NOT this park's decision → inert. The run
	// stays parked and downstream does not run.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), ApproveSignal("gate", "mallory", "forged", "bad", "not-the-nonce")),
		ErrSuspended, "a wrong-nonce approval must not resume the run")
	parked, err := store.Load("wf-n-wrong")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "gate", Waiting)
	require.EqualValues(t, 0, afterN.Load(), "a wrong-nonce approval must not run downstream")

	// The correctly-correlated decision (a distinct sig.ID; the stale one stays
	// buffered but inert) resumes to success.
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "good", w.ApprovalNonce("gate"))))
	final, err := store.Load("wf-n-wrong")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_EmptyNonceIsInert(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-n-empty", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// The pre-AUD-025 call shape (no nonce → empty) can no longer approve.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), ApproveSignal("gate", "alice", "ok", "d1", "")),
		ErrSuspended, "an empty nonce must not resume the run")
	require.EqualValues(t, 0, afterN.Load())
}

func TestAUD025_RejectRequiresCorrectNonce(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-n-rej", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// A stale reject is inert too — it must not fail the run.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), RejectSignal("gate", "mallory", "stale", "bad", "wrong-nonce")),
		ErrSuspended, "a wrong-nonce reject must not fail the run")

	// A correctly-correlated reject fails fast with the typed error.
	err := w.DeliverAndResume(context.Background(), RejectSignal("gate", "bob", "no", "good", w.ApprovalNonce("gate")))
	var rej *ApprovalRejectedError
	require.True(t, errors.As(err, &rej), "a correct-nonce reject fails with *ApprovalRejectedError")
	require.Equal(t, "bob", rej.Approver)
	require.EqualValues(t, 0, afterN.Load())
}

func TestAUD025_NonceIsDeterministicAndBinds(t *testing.T) {
	store := NewInMemoryStore()
	w := buildApprovalWorkflow(t, store, "wf-bind", nil)

	// (*Workflow).ApprovalNonce is exactly the pure derivation over the same inputs.
	got := w.ApprovalNonce("gate")
	require.Equal(t, ApprovalNonce("wf-bind", "gate", w.dag.DefinitionDigest()), got)

	// Deterministic: recomputing yields the identical token.
	require.Equal(t, got, w.ApprovalNonce("gate"))

	// Binds to each input independently — a change in workflow, node, or definition
	// digest yields a different nonce (so a decision cannot cross any of them).
	digest := w.dag.DefinitionDigest()
	require.NotEqual(t, got, ApprovalNonce("other-wf", "gate", digest), "binds to workflow id")
	require.NotEqual(t, got, ApprovalNonce("wf-bind", "other-node", digest), "binds to node name")
	require.NotEqual(t, got, ApprovalNonce("wf-bind", "gate", "different-digest"), "binds to definition digest")

	// No length-extension collision between the two length-prefixed segments:
	// (wf="ab", node="c") and (wf="a", node="bc") must differ.
	require.NotEqual(t, ApprovalNonce("ab", "c", digest), ApprovalNonce("a", "bc", digest))
}

func TestAUD025_NonceSurvivesDurableRoundTrip(t *testing.T) {
	// A durable store round-trips the delivered decision through JSON, so the engine
	// sees a map[string]any, not the typed struct. The nonce must survive that decode
	// or a correctly-correlated decision would wrongly go inert on a durable store.
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-durable", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "durable", "d1", w.ApprovalNonce("gate"))))
	final, err := store.Load("wf-durable")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_NonceIsResumeStable(t *testing.T) {
	// The expected nonce must be identical across a crash-resume: a fresh Workflow
	// value rebuilt over the SAME store and graph derives the SAME nonce, so a
	// decision delivered before a crash still verifies after it.
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w1 := buildApprovalWorkflow(t, store, "wf-resume", &afterN)
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)

	nonceBefore := w1.ApprovalNonce("gate")

	// Simulate a process restart: a brand-new Workflow over the same store + graph.
	w2 := buildApprovalWorkflow(t, store, "wf-resume", &afterN)
	require.Equal(t, nonceBefore, w2.ApprovalNonce("gate"), "the nonce is resume-stable")

	// The decision minted against the pre-restart nonce resumes the post-restart run.
	require.NoError(t, w2.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "d1", nonceBefore)))
	final, err := store.Load("wf-resume")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())
}
