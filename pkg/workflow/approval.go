package workflow

import (
	"context"
	"fmt"
)

// ApprovalDecision is the payload a host delivers to an approval node (M19 ph90):
// a human/external decision to approve or reject, carrying the approver's identity
// and an optional free-text comment for the audit trail. It is JSON-round-trip-safe
// (all bool/string — no int64 magnitude trap), but a durable round-trip decodes it
// back as a generic value (see decodeApprovalDecision), never this typed struct.
type ApprovalDecision struct {
	Approved bool   // true = approve (resume downstream); false = reject (fail fast)
	Approver string // identity of the deciding party (persisted for audit)
	Comment  string // optional free-text rationale (persisted for audit)
}

// ApprovalRejectedError is returned by an approval node when the delivered decision
// is a REJECT (M19 ph90, INV-01 fail-fast). It is a real failure — a non-ErrSuspended
// error — so it rides the existing chosen-branch-failure path (dag.go): the node goes
// Failed and no downstream node runs. It carries the approver + comment so a host can
// classify a rejection distinctly from an execution failure via errors.As and read the
// audit fields off it.
//
// It deliberately has NO Unwrap (DEC-P90-REJECT-ERR-NO-UNWRAP): a rejection wraps no
// underlying cause, and exposing one would make this a new poison source into any
// errors.Is/As classifier over DAG.Execute's return (the ExecutionError.Unwrap
// poison-precedence trap). errors.As(err, &ApprovalRejectedError{}) is the correct,
// sufficient classification path for a terminal fail-fast.
type ApprovalRejectedError struct {
	Node     string // the approval node whose decision was a reject
	Approver string // who rejected (may be empty if the decision omitted it)
	Comment  string // the rejection rationale (may be empty)
}

// Error renders a deterministic, value-free summary — like SagaError.Error it names
// only the node and the decision metadata, never WorkflowData values.
func (e *ApprovalRejectedError) Error() string {
	return fmt.Sprintf("approval %q rejected by %q: %s", e.Node, e.Approver, e.Comment)
}

// approvalAction is a thin suspendable variant of waitForSignalAction (signal.go): it
// parks the run (Waiting) on a named decision signal, then on arrival APPLIES the
// decision. Approve → persist the decision + complete (downstream resumes); reject →
// return an *ApprovalRejectedError (fail fast). It reproduces the exact chunk-1 suspend
// seam — the unexported suspendable() marker (so node.Execute honors ErrSuspended as a
// PARK, bypassing retry/timeout), the ctx-injected SignalStore (so a normal Execute and
// a host DeliverAndResume read the identical mailbox), and the ErrWaitRequiresSignalStore
// honesty guard.
type approvalAction struct {
	// nodeName keys the idempotent apply (Action.Execute is not given the node name;
	// the constructor wires it).
	nodeName string
	// signalName is the decision signal this node waits for. Derived 1:1 from the node
	// name by AddApproval, so a host's ApproveSignal/RejectSignal helpers cannot drift.
	signalName string
}

func (a *approvalAction) suspendable() {}

// Execute is the take-or-park-then-decide decision, re-run on EVERY invocation so a
// resume re-checks the mailbox correctly (Waiting is non-terminal). No SignalStore →
// ErrWaitRequiresSignalStore (loud failure, never a forever-park). Named signal absent
// → ErrSuspended (park). Present → decode the decision DEFENSIVELY (a durable round-trip
// returns a generic map[string]any, NOT the typed struct) and act:
//   - approve → idempotent apply (byte-identical to the wait seam: Set(IdempotencyKey)
//   - SetOutput of the raw payload) + record the consumed sig.ID + complete;
//   - reject  → return *ApprovalRejectedError (the node fails; the reject signal is NOT
//     recorded consumed — see the reject arm below).
//
// The ack is NOT done here — it happens after the consuming completion is durably
// checkpointed (Workflow.Execute drains the consumed collector, D37-04), so a crash
// before the checkpoint re-runs this node and re-applies the same idempotent write.
func (a *approvalAction) Execute(ctx context.Context, data *WorkflowData) error {
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
		decision, derr := decodeApprovalDecision(sig.Payload)
		if derr != nil {
			return derr
		}
		if !decision.Approved {
			// A reject fails the node (non-ErrSuspended → Failed → *ExecutionError). The
			// signal is deliberately NOT recorded consumed: the failure path never drains
			// the consumed collector (an ack happens only after a durable Completed,
			// D37-04), and re-offer is already prevented by the terminal-failed run being
			// a no-op on re-drive — not by an ack. So there is nothing to add here.
			return &ApprovalRejectedError{Node: a.nodeName, Approver: decision.Approver, Comment: decision.Comment}
		}
		// Idempotent apply (D37-05), byte-identical to the wait seam: persist the RAW
		// payload (not the re-typed struct) so re-application writes byte-identical data
		// — NoDoubleApply holds for the same reason it holds there. Surfaced as the
		// node output too so dependents read the decision naturally. The consumed sig.ID
		// is recorded so Workflow.Execute acks it AFTER the Completed status is durable.
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

// ApproveSignal constructs the decision Signal a host delivers to APPROVE the approval
// node named node. sigID is the host-supplied dedupe key (re-delivering the same ID is
// idempotent — one mailbox entry). The signal Name is the node name, matching
// AddApproval's 1:1 derivation, so the two can never drift.
func ApproveSignal(node, approver, comment, sigID string) Signal {
	return Signal{ID: sigID, Name: node, Payload: ApprovalDecision{Approved: true, Approver: approver, Comment: comment}}
}

// RejectSignal constructs the decision Signal a host delivers to REJECT the approval
// node named node (→ fail-fast *ApprovalRejectedError). sigID is the host-supplied
// dedupe key; Name is the node name (see ApproveSignal).
func RejectSignal(node, approver, comment, sigID string) Signal {
	return Signal{ID: sigID, Name: node, Payload: ApprovalDecision{Approved: false, Approver: approver, Comment: comment}}
}

// decodeApprovalDecision decodes a delivered-or-persisted approval payload into an
// ApprovalDecision, tolerating every shape it can arrive in (⚠ the int64/JSON-fidelity
// landmine) — the delivery path AND the audit read-back path across all three stores:
//   - the typed ApprovalDecision (or *ApprovalDecision) — an in-process deliver that has
//     NOT round-tripped a durable store;
//   - a generic map[string]any — the shape the InMemory/JSON mailbox + JSONFileStore
//     load return after JSON-decoding the payload with UseNumber (signal.go:
//     unmarshalSignalPayload; workflow_data JSON load);
//   - a raw JSON string — the shape a FlatBuffersStore reload returns for a persisted
//     node output/data value (workflow_store.go:1017 SetOutput(string)); parsed with
//     UseNumber and folded into the map path so the SAME field validation applies.
//
// It NEVER does sig.Payload.(ApprovalDecision) — that assertion fails on the
// round-tripped map/string. A payload that is none of these shapes is a typed error
// (ErrValidation), never a phantom approve — Approved=true is reachable only from a
// typed bool-true or a map/string whose "Approved" is a genuine bool true.
func decodeApprovalDecision(payload any) (ApprovalDecision, error) {
	switch p := payload.(type) {
	case ApprovalDecision:
		return p, nil
	case *ApprovalDecision:
		if p == nil {
			return ApprovalDecision{}, fmt.Errorf("%w: nil approval decision payload", ErrValidation)
		}
		return *p, nil
	case string:
		// A FlatBuffersStore reload returns a persisted output/data value as its raw
		// JSON string. Parse it (UseNumber, like every other decode path) and fold into
		// the map path. An empty string (FB's nil-payload encoding) or a non-object JSON
		// (a bare scalar) yields ErrValidation, not a phantom approve.
		v, perr := unmarshalSignalPayload(p)
		if perr != nil {
			return ApprovalDecision{}, fmt.Errorf("%w: undecodable approval decision string", ErrValidation)
		}
		if v == nil {
			return ApprovalDecision{}, fmt.Errorf("%w: empty approval decision payload", ErrValidation)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return ApprovalDecision{}, fmt.Errorf("%w: approval decision string is not a JSON object (%T)", ErrValidation, v)
		}
		return decodeApprovalDecisionMap(m)
	case map[string]any:
		return decodeApprovalDecisionMap(p)
	default:
		return ApprovalDecision{}, fmt.Errorf("%w: unexpected approval decision payload type %T", ErrValidation, payload)
	}
}

// decodeApprovalDecisionMap validates the generic-map form of an ApprovalDecision. A
// missing field defaults to its zero (a missing Approved → false → a fail-safe reject,
// never a phantom approve); a present field of the wrong type is a typed ErrValidation.
func decodeApprovalDecisionMap(p map[string]any) (ApprovalDecision, error) {
	d := ApprovalDecision{}
	if v, ok := p["Approved"]; ok {
		b, bok := v.(bool)
		if !bok {
			return ApprovalDecision{}, fmt.Errorf("%w: approval decision Approved is not a bool (%T)", ErrValidation, v)
		}
		d.Approved = b
	}
	if v, ok := p["Approver"]; ok {
		s, sok := v.(string)
		if !sok {
			return ApprovalDecision{}, fmt.Errorf("%w: approval decision Approver is not a string (%T)", ErrValidation, v)
		}
		d.Approver = s
	}
	if v, ok := p["Comment"]; ok {
		s, sok := v.(string)
		if !sok {
			return ApprovalDecision{}, fmt.Errorf("%w: approval decision Comment is not a string (%T)", ErrValidation, v)
		}
		d.Comment = s
	}
	return d, nil
}
