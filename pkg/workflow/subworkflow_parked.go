package workflow

import (
	"context"
	"errors"
	"fmt"
)

// M19 ph92 — WAKE mechanism (parent-side parked-await). Where ph91's inline path BLOCKS on the
// child (subWorkflowAction), the PARKED path parks (Waiting) while the child — run out-of-band —
// is not yet terminal; a durable completion signal delivered to the parent's mailbox + a host
// DeliverAndResume wakes it; on wake it reads the child's result DATA key (the uniform ph91
// contract) and converges. DEC-M19-WAKE made real: there is NO scheduler — the completion signal
// + host re-drive IS the wake. The child-side producer (what emits the completion signal on
// child-terminal) is ph94; here completion signals are delivered manually (tests).
//
// Shaped like waitForConditionAction (signal.go), but the "condition" is "Load(childID) is
// terminal" — read through the ph91 ctx-injected parent store (parentStoreFrom). Suspendable
// via the same unexported marker (direct-set, never middleware) so node.Execute honors the park.

// completionSignalName derives the deterministic completion-signal name for a parked
// sub-workflow node. Drift-proof (like ph90's node-name derivation): the host's
// SubWorkflowCompletionSignal helper derives the SAME name, so a delivered completion signal
// always matches the node waiting for it.
func completionSignalName(nodeName string) string {
	return "subworkflow-complete:" + nodeName
}

// SubWorkflowCompletionSignal constructs the bare completion TRIGGER a host delivers to wake a
// parked sub-workflow-await node. It carries NO result payload (DEC-P92-SIGNAL-IS-TRIGGER +
// DEC-P91-RESULT-DATAKEY): the result is read from the child's data key on wake, never the
// signal. sigID is the host-supplied dedupe key (idempotent by ID). nodeName is the parent's
// parked node name (the same name AddSubWorkflowParked used).
func SubWorkflowCompletionSignal(nodeName, sigID string) Signal {
	return Signal{ID: sigID, Name: completionSignalName(nodeName), Payload: nil}
}

// parkedSubWorkflowAction awaits a child workflow that runs OUT-OF-BAND (not inline). It parks
// (ErrSuspended) while the child journal is non-terminal; on a wake where the child is terminal
// it renders the coe-aware verdict from (journal + child DAG) and either populates the declared
// result data key + completes, or fails the parent node (INV-01). It carries the child *DAG
// (symmetric with the inline subWorkflowAction) so it can render the terminal verdict without a
// child.Execute to defer to (DEC-P92-COE-VERDICT-FROM-DAG, Option 1).
type parkedSubWorkflowAction struct {
	nodeName   string // parent node name — keys the deterministic child ID + the completion-signal name
	child      *DAG   // the definition-value child (its own graph; carried for the coe verdict)
	resultKey  string // parent data key the child's result is written to (may be empty)
	resultFrom string // child DATA key whose value is the result (ph91 WithResult shape; may be empty)
}

func (a *parkedSubWorkflowAction) suspendable() {}

// Execute is the park-or-wake decision, re-evaluated on EVERY invocation (so a host re-drive
// re-checks — Waiting is non-terminal). No SignalStore → ErrWaitRequiresSignalStore (loud, never
// a forever-park). The completion GATE is the child JOURNAL being terminal (not the mere presence
// of a signal — DEC-P92-SIGNAL-IS-TRIGGER); the signal only triggers the host DeliverAndResume.
func (a *parkedSubWorkflowAction) Execute(ctx context.Context, parentData *WorkflowData) error {
	ss := signalStoreFrom(ctx)
	if ss == nil {
		return ErrWaitRequiresSignalStore
	}
	store := parentStoreFrom(ctx)
	if store == nil {
		// A parked await cannot read the child journal without the parent store. Loud, not a park.
		return ErrSubWorkflowRequiresStore
	}
	childID := subWorkflowChildID(parentData.GetWorkflowID(), a.nodeName)

	child, err := store.Load(childID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrSuspended // child not spawned yet → park, re-check on the next wake
		}
		return fmt.Errorf("parked sub-workflow %q: load child %q: %w", a.nodeName, childID, err)
	}

	// TERMINAL AUTHORITY by dispatch mode (DEC-P94-QUEUE-TERMINAL-AUTHORITY). A QUEUE-dispatched child's
	// `work_queue` row IS its lifecycle record (M17 design) — so consult it FIRST. This closes the F-P94-05
	// deadlock: an operator-cancelled parked queue child terminalizes the row (`cancelled`) but NOT the
	// child's DATA journal, so a journal-only gate would re-park the woken parent forever. The queue row is
	// the authority for a queue child; the journal stays the RESULT-DATA source (read on `done`). For a
	// MANUAL-signal ph92 child there is NO row (queueTerminalState → exists=false) → fall through to the
	// journal-gate exactly as ph92 sealed it (byte-unchanged — the OR-clause is simply false).
	if sq, ok := store.(*SQLiteStore); ok {
		wqState, exists, qerr := sq.queueTerminalState(childID)
		if qerr != nil {
			return fmt.Errorf("parked sub-workflow %q: read queue state for %q: %w", a.nodeName, childID, qerr)
		}
		if exists {
			switch wqState {
			case wqDone:
				// Queue-terminal success → the journal carries the result (RunNext populated it before
				// MarkDone). Read the result data key + ack via the uniform ph91 path below.
				return a.completeSuccess(ctx, ss, parentData, child)
			case wqFailed, wqCancelled:
				// Queue-terminal non-success → fail the parent node (INV-01). Do NOT re-derive from the
				// journal — a cancelled child's journal is incomplete; the wqState IS the disposition.
				return fmt.Errorf("parked sub-workflow %q: child terminal %s (queue authority)", a.nodeName, wqState)
			default:
				// pending|claimed → the queue child is NOT terminal yet (a park is `claimed`, not terminal).
				return ErrSuspended
			}
		}
		// exists=false → not a queue-dispatched child → fall through to the journal-gate.
	}

	if !childTerminal(child) {
		return ErrSuspended // child still running → park (re-checked every drive)
	}

	// Child is terminal — render the coe-aware verdict from (journal + DAG), the SHARED rule.
	if failed, firstFailed := childRunFailed(a.child, child); failed {
		// Fail the parent node (INV-01, terminal contract). We do NOT ack the completion signal
		// here: the failure path never drains the consumed collector (executeLocked's fail/rollback
		// arms leave it INERT — the ph90 reject-no-ack shape), so an add() would be a silent no-op.
		// The signal stays in the mailbox and is reclaimed by the store's Delete; the parent run is
		// terminal, so a re-drive is a no-op and never re-consumes it. (F-P92-04.)
		return fmt.Errorf("parked sub-workflow %q: child failed (node %q terminal-failed)", a.nodeName, firstFailed)
	}

	// Child succeeded — populate the declared result data key (the uniform ph91 path) and ack.
	return a.completeSuccess(ctx, ss, parentData, child)
}

// completeSuccess is the ONE shared success tail (both the queue-authority `done` case and the
// journal-gate success path route through it): populate the declared result data key from the child
// journal (the uniform ph91 result contract) then ack the completion signal. Keeping it single means
// there is exactly one populate+ack path regardless of which terminal authority fired.
func (a *parkedSubWorkflowAction) completeSuccess(ctx context.Context, ss SignalStore, parentData, child *WorkflowData) error {
	if err := a.applyResult(parentData, child); err != nil {
		return err
	}
	a.ackCompletion(ctx, ss, parentData.GetWorkflowID())
	return nil
}

// applyResult reuses the ph91 uniform result contract: read the child's declared DATA key into
// the declared parent key, with the reflect.DeepEqual collision check. Delegates to the inline
// action's applyResult so there is exactly ONE populate path (no second result path).
func (a *parkedSubWorkflowAction) applyResult(parentData, childData *WorkflowData) error {
	return (&subWorkflowAction{nodeName: a.nodeName, resultKey: a.resultKey, resultFrom: a.resultFrom}).
		applyResult(parentData, childData)
}

// ackCompletion take/acks the completion signal on the drive that completes (or fails) the node,
// for mailbox hygiene (D37-04 ordering: the ack lands after the node is durably terminal because
// Workflow.Execute drains the consumed collector post-checkpoint). Best-effort: a take error does
// not block the completion (the journal, not the signal, is authoritative).
func (a *parkedSubWorkflowAction) ackCompletion(ctx context.Context, ss SignalStore, workflowID string) {
	sigs, err := ss.TakeSignals(workflowID)
	if err != nil {
		return
	}
	want := completionSignalName(a.nodeName)
	c := consumedSignalsFrom(ctx)
	for _, sig := range sigs {
		if sig.Name == want && c != nil {
			c.add(sig.ID)
		}
	}
}
