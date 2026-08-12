package workflow

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-069 / S-02: Approval is ORCHESTRATION, not authorization. This test pins the
// reframed contract behaviorally: the engine accepts ANY approver string delivered to
// the node's mailbox — it authenticates no one — and carries the host-asserted approver
// verbatim into the audit trail. A host that needs a trustworthy approver must
// authenticate the decision BEFORE delivering it; the engine will not do it.
func TestAUD069_ApprovalDoesNotAuthenticateApprover(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-aud069", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended, "an undelivered decision parks")

	// An arbitrary, unauthenticated approver identity — the engine has no way to and
	// does NOT verify it. It is accepted and the run converges just as for any other.
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "anyone-at-all — not authenticated", "forged?", "d1", w.ApprovalNonce("gate"))))

	final, err := store.Load("wf-aud069")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load(),
		"the engine accepts the decision from ANY approver — it is orchestration, not auth")

	// The host-asserted approver is carried verbatim for audit, exactly as delivered —
	// not validated, not rewritten.
	out, ok := final.GetOutput("gate")
	require.True(t, ok)
	require.Contains(t, out, "anyone-at-all",
		"the approver is persisted for audit as host-asserted, unauthenticated text")
}
