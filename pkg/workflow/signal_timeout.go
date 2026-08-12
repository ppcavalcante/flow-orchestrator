package workflow

import (
	"context"
	"math"
	"time"
)

// waitForSignalOrTimeoutAction is a declared suspension node (M22 ph113): a durable
// first-of(signal, timer). It parks Waiting until EITHER the named signal lands in
// the durable mailbox OR an absolute deadline passes — exactly one of {signal,
// timeout} wins. It fuses timerAction's arming (an absolute fireAt in the WorkflowData
// `waits` section, frozen at first encounter, durable-remaining across restart) with
// waitForSignalAction's non-destructive mailbox peek, behind ONE suspendable() marker.
//
// It is an ADDITIVE sibling: waitForSignalAction, timerAction, and WithTimeout are all
// untouched. It inherits BOTH wake paths for free — DeliverAndResume (the mailbox is
// re-checked every Execute) AND Tick/DueTimers (DueTimers reports any Waiting node with
// a due fireAt in `waits`; it does not special-case timerAction — timer.go:156-186).
type waitForSignalOrTimeoutAction struct {
	// nodeName keys both the durable timer arming (waits entry) and the idempotent
	// signal apply. The action carries its own name because Action.Execute is not
	// given the node name; the constructor wires it.
	nodeName string
	// signalName is the name this node waits for in the mailbox.
	signalName string
	// duration is the relative timeout; the absolute fireAt = clock.Now()+duration is
	// computed ONCE, at the first (arming) encounter, and is durable thereafter.
	duration time.Duration
}

func (a *waitForSignalOrTimeoutAction) suspendable() {}

// timedOutKey is the disposition key the timeout arm sets so a downstream M11
// ChoiceNode can branch signal-vs-timeout into separate subgraphs (this is what lets
// the combined node cover the separate-subgraph case without an OR-join). It is set to
// true ONLY on the timeout path; the signal path applies the payload as
// waitForSignalAction does and never sets it. Namespaced by node name so two combined
// nodes in one workflow do not collide.
func timedOutKey(nodeName string) string { return nodeName + ".__timedOut__" }

// Execute is the first-of(signal, timer) park-or-fire decision. Each encounter, in
// THIS order (the mailbox-before-everything ordering IS the tie-break — do NOT reorder):
//
//  1. Peek the mailbox FIRST (every encounter, including before arming): the named
//     signal present → apply the payload idempotently (identical to waitForSignalAction)
//     + ClearWait (disarm the timer if armed) + return nil (SIGNAL WON — a delivered
//     signal is a real external event; timeout is the fallback, so it wins a
//     same-encounter tie AND an early-buffered signal wins immediately, ph113-F1).
//  2. Else first encounter (no armed fireAt) → arm fireAt = clock.Now()+duration (the
//     exact timerAction arming incl. the overflow/underflow clamps) and park.
//  3. Else if the absolute fireAt has passed → set the timedOut disposition + ClearWait
//     + return nil (TIMEOUT WON).
//  4. Else re-park.
//
// All time is read through clockFrom(ctx); it NEVER calls time.Now() — the retry
// OUTCOME (which arm won) is journaled via the checkpoint, never the clock instant, so
// there is no determinism tax (the D36-07 discipline the determinism spec enforces).
// engineTrusted marks waitForSignalOrTimeoutAction as engine machinery: it arms and
// disarms the durable timeout wait (SetWait/ClearWait), so it runs against unsealed
// data rather than a sealed per-node view (M24 DEC-M24-MEDIATION).
func (a *waitForSignalOrTimeoutAction) engineTrusted() {}

func (a *waitForSignalOrTimeoutAction) Execute(ctx context.Context, data *WorkflowData) error {
	ss := signalStoreFrom(ctx)
	if ss == nil {
		return ErrWaitRequiresSignalStore
	}
	clock := clockFrom(ctx)

	// TIE-BREAK + early-signal: peek the mailbox on EVERY encounter — including the
	// FIRST, before arming — so (1) a same-encounter tie (signal present ∧ fireAt<=now)
	// is won by the signal, and (2) an EARLY-BUFFERED signal (delivered before this node
	// was first reached — a supported DeliverSignal feature, D37-03) wins immediately
	// instead of being deferred up to the whole timeout (review ph113-F1). This is the
	// same first-thing peek waitForSignalAction does. Non-destructive peek — the ack
	// happens post-checkpoint via the consumedSignals collector (a crash before the
	// checkpoint re-runs and re-applies the same idempotent write, D37-04).
	sigs, err := ss.TakeSignals(data.GetWorkflowID())
	if err != nil {
		return err
	}
	for _, sig := range sigs {
		if sig.Name != a.signalName {
			continue
		}
		// Idempotent apply (D37-05) — byte-identical to waitForSignalAction.
		key := IdempotencyKey(data, a.nodeName)
		data.Set(key, sig.Payload)
		data.SetOutput(a.nodeName, sig.Payload)
		if c := consumedSignalsFrom(ctx); c != nil {
			c.add(sig.ID)
		}
		data.ClearWait(a.nodeName) // disarm the timer if it was armed (no-op on first encounter)
		return nil                 // SIGNAL WON
	}

	fireAt, armed := data.GetWait(a.nodeName)
	if !armed {
		// No signal yet, first encounter: arm an ABSOLUTE due instant and park. The
		// persisted fireAt survives a stop (GetWait returns armed on resume) so we never
		// re-arm — the original absolute deadline is durable-remaining, not reset.
		// ponytail: the clamp below is timerAction's exact arming (timer.go:82-101),
		// duplicated deliberately — timerAction is TLA-modeled and additive-sibling
		// contract says do NOT mutate it, so a shared helper (which would touch its
		// body) is worse than one 12-line copy. Keep the two in sync.
		now := clock.Now()
		fireAt = now.Add(a.duration).UnixNano()
		if a.duration > 0 && fireAt <= now.UnixNano() {
			fireAt = math.MaxInt64 // positive overflow: park far-future, not fire-now
		} else if a.duration < 0 && fireAt > now.UnixNano() {
			fireAt = math.MinInt64 // negative underflow: fire-now, not park-forever
		}
		data.SetWait(a.nodeName, fireAt)
		return ErrSuspended
	}

	if clock.Now().UnixNano() >= fireAt {
		// TIMEOUT WON: record the disposition so a downstream ChoiceNode can branch,
		// clear the durable wait, and complete.
		data.Set(timedOutKey(a.nodeName), true)
		data.SetOutput(a.nodeName, true)
		data.ClearWait(a.nodeName)
		return nil
	}

	// Armed, no signal, not yet due — re-park (the durable fireAt is the source of
	// truth; this live re-check makes a spurious early wake harmless).
	return ErrSuspended
}

// newWaitForSignalOrTimeoutNode builds a declared first-of(signal, timer) node: when
// reached it parks the run (Waiting) until the named signal arrives OR the timeout
// deadline (absolute, durable-remaining across restart) passes — exactly one wins,
// signal-first on a same-encounter tie. The timeout arm sets timedOutKey(name)=true so
// a downstream M11 ChoiceNode can branch signal-vs-timeout. Requires a Store
// implementing SignalStore (else ErrWaitRequiresSignalStore at run time). The action is
// set DIRECTLY (not via middleware) so the suspend marker stays visible to node.Execute.
func newWaitForSignalOrTimeoutNode(name, signalName string, timeout time.Duration) *Node {
	return newNode(name, &waitForSignalOrTimeoutAction{nodeName: name, signalName: signalName, duration: timeout})
}
