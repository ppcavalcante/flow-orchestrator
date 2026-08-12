package workflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// AUD-025 — approval correlation nonce. Before this, an approval node consumed ANY
// well-formed Approved=true payload delivered to its mailbox under the node name:
// a stale decision left from an earlier context, a stray payload, or a decision
// meant for a different graph could silently approve. The nonce binds a decision
// to a SPECIFIC park — this (workflowID, node, graph-definition) — so the engine
// consumes it only if it is deliberately correlated to the park it is satisfying.
//
// It is a FRESHNESS / CORRELATION token, not a secret. Deliberately, it is derived
// from public, durable inputs (workflowID, node name, the graph's DefinitionDigest),
// so a host recomputes it with ApprovalNonce / (*Workflow).ApprovalNonce and there
// is no key to distribute. The HONEST CEILING (consistent with AUD-069 and the M9
// trust model, where the persistence store is an untrusted input TCB): an attacker
// who can WRITE the mailbox can also READ these inputs and forge a matching nonce,
// so this does NOT provide adversarial integrity. It makes an honest host's stale /
// stray / mis-correlated decision inert; for adversarial integrity the host must
// authenticate the decision itself before delivering it (AUD-069).
//
// It is deterministic and RESUME-STABLE: every input is stable for the lifetime of
// a workflow run (the DefinitionDigest is fixed for a given graph, and a resume onto
// a changed graph is already rejected by the AUD-010 digest guard before any node
// runs), so a crash-resume re-derives the identical nonce and a decision delivered
// before the crash still matches after it.

// approvalNonceDomain is a domain-separation tag so an approval nonce can never
// collide with another SHA-256-derived token in this package (IdempotencyKey,
// SubWorkflowChildID, ...).
const approvalNonceDomain = "flow-orchestrator/approval-nonce\x00"

// ApprovalNonce derives the correlation nonce for the approval node named nodeName
// in the workflow workflowID whose graph has the given DefinitionDigest (see
// DAG.DefinitionDigest). A host attaches this to the decision it delivers (via
// ApproveSignal / RejectSignal); the engine consumes the decision only if the
// attached nonce matches. It is a pure function of its inputs — recompute it, do
// not store it. (*Workflow).ApprovalNonce is the convenience form when you hold a
// live Workflow.
//
// The workflowID and nodeName are length-prefixed before hashing so that no two
// distinct (workflowID, nodeName) pairs can produce the same pre-image (the same
// collision guard SubWorkflowChildID uses); definitionDigest is a fixed-length hex
// string appended last.
func ApprovalNonce(workflowID, nodeName, definitionDigest string) string {
	h := sha256.New()
	h.Write([]byte(approvalNonceDomain)) //nolint:errcheck // sha256 Write never errors
	var lp [8]byte
	binary.LittleEndian.PutUint64(lp[:], uint64(len(workflowID)))
	h.Write(lp[:])              //nolint:errcheck // sha256 Write never errors
	h.Write([]byte(workflowID)) //nolint:errcheck // sha256 Write never errors
	binary.LittleEndian.PutUint64(lp[:], uint64(len(nodeName)))
	h.Write(lp[:])                    //nolint:errcheck // sha256 Write never errors
	h.Write([]byte(nodeName))         //nolint:errcheck // sha256 Write never errors
	h.Write([]byte(definitionDigest)) //nolint:errcheck // sha256 Write never errors
	return hex.EncodeToString(h.Sum(nil))
}

// ApprovalNonce derives the correlation nonce for this workflow's approval node
// named nodeName (AUD-025). It reads the workflow ID and the current graph's
// DefinitionDigest, so a host that holds the Workflow can compute the nonce to
// attach to a decision without recomputing the digest itself.
func (w *Workflow) ApprovalNonce(nodeName string) string {
	return ApprovalNonce(w.WorkflowID, nodeName, w.dag.DefinitionDigest())
}

// ApprovalNonceFromStore derives the approval correlation nonce for nodeName in the
// workflow workflowID by reading its parked state from store — the store-only
// counterpart to (*Workflow).ApprovalNonce (AUD-025). A dispatcher / signal pump /
// competing-consumer driver that delivers approvals by (store, workflowID) holds no
// live *Workflow and would otherwise have to rebuild the graph purely to read its
// DefinitionDigest; this reads the digest the executor already stamped into the durable
// state (defDigestKey) and derives — via the engine's own expectedApprovalNonce, so the
// two cannot drift — the identical nonce the engine will check the delivered decision
// against.
//
// It returns a typed ErrValidation if the run has not yet stamped a digest (it has not
// started, or not reached its first checkpoint): a nonce derived without the real digest
// would not match, so the correct caller behaviour is to retry once the run has parked —
// exactly the poll a signal-delivering driver already performs (the approval node is
// Waiting only after a checkpoint). A store Load error (e.g. ErrNotFound for an unknown
// id) is returned unchanged.
//
// Because the nonce is a public correlation token, not a secret (see ApprovalNonce),
// deriving it from the store grants no capability a signal-delivering caller lacks: any
// party that can DeliverSignal to the mailbox can already read the same state.
func ApprovalNonceFromStore(store WorkflowStore, workflowID, nodeName string) (string, error) {
	// interfaceHoldsNil, not `store == nil`: a typed-nil WorkflowStore (a non-nil interface
	// wrapping a nil concrete pointer) passes `== nil` but panics on store.Load (CUR-002/AUD-031).
	if interfaceHoldsNil(store) {
		return "", fmt.Errorf("%w: ApprovalNonceFromStore requires a non-nil store", ErrValidation)
	}
	data, err := store.Load(workflowID)
	if err != nil {
		return "", err
	}
	if data == nil { // a store that violates the Load contract (nil,nil) must not panic here
		return "", fmt.Errorf("%w: store returned no data for workflow %q", ErrCorruptData, workflowID)
	}
	// Require a well-formed stamped digest, so the helper never returns a silently-wrong
	// nonce (expectedApprovalNonce would otherwise fold an absent/malformed digest to the
	// empty string and derive a mismatching token).
	v, ok := data.Get(defDigestKey)
	if !ok {
		return "", fmt.Errorf("%w: workflow %q has not stamped a definition digest yet "+
			"(not started or not checkpointed); retry once the approval node is parked", ErrValidation, workflowID)
	}
	if s, isStr := v.(string); !isStr || s == "" {
		return "", fmt.Errorf("%w: definition digest for workflow %q is malformed", ErrValidation, workflowID)
	}
	return expectedApprovalNonce(data, nodeName), nil
}

// expectedApprovalNonce is the engine-side nonce the approvalAction checks a
// delivered decision against. It reads the SAME DefinitionDigest the host used, from
// the reserved key the executor stamped into the run data at drive start
// (defDigestKey == w.dag.DefinitionDigest()), so the engine's expected nonce and the
// host's ApprovalNonce cannot drift. A missing digest (a data shape that never
// carried the stamp) folds to the empty string on both sides.
func expectedApprovalNonce(data *WorkflowData, nodeName string) string {
	digest := ""
	if v, ok := data.Get(defDigestKey); ok {
		if s, isStr := v.(string); isStr {
			digest = s
		}
	}
	return ApprovalNonce(data.GetWorkflowID(), nodeName, digest)
}
