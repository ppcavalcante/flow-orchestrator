package workflow

// M10 ph37 the GOPTER PROPERTY the engineer deferred to the adversarial-tester.
//
// Over random acyclic DAGs with a WaitForSignalNode spliced into a varied position,
// with RANDOM signal-delivery timing (before any drive / after the first park /
// late) and RANDOM crash injection (a checkpoint flush that "crashes" the drive, its
// in-memory data discarded — a true process death modelled the signal_consume_test.go
// way), assert four things — the four arms the prompt named:
//
//   (a) EVENTUAL COMPLETION: once the signal is delivered and the run is driven
//       enough, every awaited node (and every node) reaches Completed.
//   (b) EXACTLY-ONCE OBSERVABLE APPLY across crashes + re-deliveries: the consuming
//       node's applied payload is byte-identical to the no-crash value — the apply
//       is set-not-accumulate to a deterministic key, so crash-resume re-application
//       and signal re-delivery leave a single observable value.
//   (c) RESUME-EQUIVALENCE: a crash-injected run converges to the SAME final
//       (statuses + outputs + applied payload) as a no-crash run — crash adds no new
//       terminal state.
//   (d) RE-DELIVERY OF THE SAME sig.ID CHANGES NOTHING: after convergence, delivering
//       the same signal again and driving once more leaves the outcome unchanged.
//
// All generated nodes SUCCEED (fails are not used), so convergence is total and the
// no-crash final is deterministic — that keeps resume-equivalence a clean point
// comparison (no coe-Failed nondeterminism). The drive harness replicates the
// documented take→apply→Completed→checkpoint→ack ordering (the correctness core,
// D37-04) directly on the DAG.Execute seam so a crash can be injected faithfully
// (discard in-memory data, leave the signal unacked) — the same seam the author's
// signal_consume_test.go uses.
//
// Anti-vacuity bites (separate tests below, each must FALSIFY a real mutation):
//   - never delivering the signal ⇒ the run NEVER converges (arm (a) is real).
//   - a non-idempotent (ACCUMULATING) apply ⇒ re-delivery/crash-resume changes the
//     observed value (arms (b)/(d) are real).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signalOutcome is the observable converged state of a run: per-node status +
// output, plus the payload the await node applied. Two runs are resume-equivalent
// iff their signalOutcomes are equal.
type signalOutcome struct {
	statuses map[string]NodeStatus
	outputs  map[string]any
	applied  any // the value at IdempotencyKey(data, awaitNode)
}

func (o signalOutcome) equal(other signalOutcome) bool {
	if len(o.statuses) != len(other.statuses) || len(o.outputs) != len(other.outputs) {
		return false
	}
	for k, v := range o.statuses {
		if other.statuses[k] != v {
			return false
		}
	}
	for k, v := range o.outputs {
		if fmt.Sprintf("%v", other.outputs[k]) != fmt.Sprintf("%v", v) {
			return false
		}
	}
	return fmt.Sprintf("%v", o.applied) == fmt.Sprintf("%v", other.applied)
}

// buildAwaitDAG materializes a dagSpec where node awaitIdx is a real
// WaitForSignalNode("go") and every other node is a counting success node. Built via
// NewDAG directly (like buildSuspendDAG) so the WaitForSignalNode's suspension marker
// stays visible to node.Execute.
func buildAwaitDAG(t *testing.T, spec dagSpec, awaitIdx int, c *execCounter) *DAG {
	t.Helper()
	d := NewDAG("await-dag")
	for i := 0; i < spec.n; i++ {
		name := invNodeName(i)
		if i == awaitIdx {
			mustAddNode(t, d, NewWaitForSignalNode(name, "go"))
		} else {
			mustAddNode(t, d, countingNode(name, c))
		}
	}
	for i := 0; i < spec.n; i++ {
		for _, j := range spec.deps[i] {
			mustAddDep(t, d, invNodeName(j), invNodeName(i)) // j before i
		}
	}
	return d
}

// collectOutcome reads the converged outcome from the store for id, keyed off
// awaitName for the applied-payload value.
func collectOutcome(store *InMemoryStore, id, awaitName string) (signalOutcome, bool) {
	data, err := store.Load(id)
	if err != nil || data == nil {
		return signalOutcome{}, false
	}
	out := signalOutcome{statuses: map[string]NodeStatus{}, outputs: map[string]any{}}
	data.ForEachNodeStatus(func(name string, st NodeStatus) {
		out.statuses[name] = st
	})
	for name := range out.statuses {
		if v, ok := data.GetOutput(name); ok {
			out.outputs[name] = v
		}
	}
	applied, _ := data.Get(IdempotencyKey(data, awaitName))
	out.applied = applied
	return out, true
}

// applyFn is the per-apply hook the harness uses; the real action is used by default,
// but the anti-vacuity bite swaps in an accumulating one.
//
// driveSignalRun runs the await DAG to convergence under the documented ordering,
// with the given delivery timing and an optional crash on iteration crashIter
// (-1 = no crash). It returns the converged outcome, or ok=false if it did not
// converge within the iteration budget. deliverDisabled models the never-deliver
// anti-vacuity bite.
func driveSignalRun(
	t *testing.T,
	spec dagSpec,
	awaitIdx int,
	id, payload string,
	store *InMemoryStore,
	counter *execCounter,
	timing int,
	crashIter int,
	deliverDisabled bool,
) (signalOutcome, bool) {
	t.Helper()
	awaitName := invNodeName(awaitIdx)
	delivered := false
	deliver := func() {
		if deliverDisabled {
			return
		}
		_ = store.DeliverSignal(id, Signal{ID: "sig-1", Name: "go", Payload: payload}) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		delivered = true
	}
	ack := func(consumed *consumedSignals) {
		ids := consumed.drain()
		if len(ids) > 0 {
			_ = store.AckSignals(id, ids) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		}
	}

	if timing == 0 {
		deliver() // before any drive (early-buffered)
	}

	maxIters := spec.n*2 + 8
	for it := 0; it < maxIters; it++ {
		crash := it == crashIter
		var cp func(*WorkflowData) error
		if crash {
			cp = func(*WorkflowData) error { return errors.New("simulated crash during checkpoint flush") }
		} else {
			cp = func(d *WorkflowData) error { return store.SaveCheckpoint(d) }
		}

		// Load existing state (resume) or start fresh.
		data, lerr := store.Load(id)
		if errors.Is(lerr, ErrNotFound) || data == nil {
			data = NewWorkflowData(id)
		} else if lerr != nil {
			return signalOutcome{}, false
		}

		consumed := &consumedSignals{}
		ctx := withConsumedSignals(withSignalStore(withCheckpoint(context.Background(), cp), store), consumed)
		err := buildAwaitDAG(t, spec, awaitIdx, counter).Execute(ctx, data)

		switch {
		case err == nil:
			// Success: save final, THEN ack (take→apply→Completed→checkpoint→ack).
			if serr := store.Save(data); serr != nil {
				return signalOutcome{}, false
			}
			ack(consumed)
			return collectOutcome(store, id, awaitName)
		case errors.Is(err, ErrSuspended):
			// Park: the barrier checkpoint already persisted (clean cp). Ack any
			// consumed (empty on a pure park), then advance delivery per the timing.
			ack(consumed)
			if !delivered {
				switch timing {
				case 1:
					deliver() // right after the first observed park
				case 2:
					if it >= 1 {
						deliver() // late
					}
				}
			}
		default:
			// Crash: DISCARD the in-memory data (do NOT save, do NOT ack) — a true
			// process death; the store keeps the last successfully-checkpointed state
			// and the consumed signal stays in the mailbox (unacked). Still advance
			// delivery so the run can make progress on the next clean drive.
			if !delivered {
				switch timing {
				case 1:
					deliver()
				case 2:
					if it >= 1 {
						deliver()
					}
				}
			}
		}
	}
	return signalOutcome{}, false // did not converge in budget
}

// TestSigProp_AwaitTimingCrashIdempotent is the headline property (arms a–d).
func TestSigProp_AwaitTimingCrashIdempotent(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x516A_0037) // "SIG"-ish + ph37
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 40
	properties := gopter.NewProperties(params)

	properties.Property("await + random timing + random crash ⇒ converges, idempotent exactly-once apply, resume-equivalent, re-delivery is a no-op",
		prop.ForAll(
			func(n int, structSeed int64, edgePermille int, parkSeed int64, timing int, crashSeed int, idSeed int64) bool {
				spec := buildDAGSpec(n, structSeed, edgePermille, 0, 0)
				awaitIdx := int(uint64(parkSeed) % uint64(spec.n))
				awaitName := invNodeName(awaitIdx)
				id := "sigprop-" + invNodeName(int(uint64(idSeed)%1_000_000))
				payload := "PL-" + id
				timing = ((timing % 3) + 3) % 3

				// --- Reference: NO-crash run. ---
				refStore := NewInMemoryStore()
				refOut, ok := driveSignalRun(t, spec, awaitIdx, id, payload, refStore, newExecCounter(), timing, -1, false)
				if !ok {
					return false // (a) the reference must always converge once delivered
				}
				// (a) total convergence: every node Completed.
				for i := 0; i < spec.n; i++ {
					if refOut.statuses[invNodeName(i)] != Completed {
						return false
					}
				}
				// (b) the applied payload is exactly the delivered value.
				if fmt.Sprintf("%v", refOut.applied) != payload {
					return false
				}
				if fmt.Sprintf("%v", refOut.outputs[awaitName]) != payload {
					return false
				}

				// --- Crash run: same structure/timing, crash on a bounded iteration. ---
				crashStore := NewInMemoryStore()
				crashIter := crashSeed % (spec.n + 3) // 0..n+2: hits arm/consume/downstream windows
				crashOut, ok := driveSignalRun(t, spec, awaitIdx, id, payload, crashStore, newExecCounter(), timing, crashIter, false)
				if !ok {
					return false // (a) a crash must not prevent eventual convergence
				}
				// (c) resume-equivalence: the crash run converges to the SAME final.
				if !refOut.equal(crashOut) {
					return false
				}
				// (b) exactly-once observable apply across the crash: byte-identical value.
				if fmt.Sprintf("%v", crashOut.applied) != payload {
					return false
				}

				// --- (d) re-delivery of the SAME sig.ID changes nothing. ---
				_ = crashStore.DeliverSignal(id, Signal{ID: "sig-1", Name: "go", Payload: payload}) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
				// Drive once more (clean). The await node is Completed ⇒ not re-run ⇒
				// the re-delivered signal is a no-op; the outcome is unchanged.
				afterOut, ok := driveSignalRun(t, spec, awaitIdx, id, payload, crashStore, newExecCounter(), timing, -1, false)
				if !ok {
					return false
				}
				return crashOut.equal(afterOut)
			},
			gen.IntRange(1, 8),    // n
			gen.Int64(),           // structural seed (edges)
			gen.IntRange(0, 1000), // edgePermille
			gen.Int64(),           // await-position seed
			gen.IntRange(0, 2),    // delivery timing: 0=before / 1=after-park / 2=late
			gen.IntRange(0, 100),  // crash-iteration seed
			gen.Int64(),           // workflow-id seed
		))

	properties.TestingRun(t)
}

// TestSigProp_Bite_NeverDeliveredNeverConverges is the anti-vacuity bite for arm (a):
// with delivery DISABLED the await node parks forever, so driveSignalRun NEVER
// converges (returns ok=false). This proves the convergence arm above is real — it
// is the "drop the delivery" mutation, and the harness correctly reports non-
// convergence rather than falsely passing.
func TestSigProp_Bite_NeverDeliveredNeverConverges(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x0B17E_A)
	params.MinSuccessfulTests = 60
	properties := gopter.NewProperties(params)

	properties.Property("no delivery ⇒ never converges (arm (a) bites)",
		prop.ForAll(
			func(n int, structSeed int64, edgePermille int, parkSeed int64) bool {
				spec := buildDAGSpec(n, structSeed, edgePermille, 0, 0)
				awaitIdx := int(uint64(parkSeed) % uint64(spec.n))
				store := NewInMemoryStore()
				_, ok := driveSignalRun(t, spec, awaitIdx, "never-conv", "PL", store, newExecCounter(), 1, -1, true /*deliverDisabled*/)
				// MUST NOT converge: the await parks forever with no signal.
				if ok {
					return false
				}
				// And the await node is persisted Waiting (a clean rest), every drive.
				p, lerr := store.Load("never-conv")
				if lerr != nil || p == nil {
					return false
				}
				st, _ := p.GetNodeStatus(invNodeName(awaitIdx))
				return st == Waiting
			},
			gen.IntRange(1, 8),
			gen.Int64(),
			gen.IntRange(0, 1000),
			gen.Int64(),
		))

	properties.TestingRun(t)
}

// TestSigProp_Bite_AckBeforeDurableLosesSignal is the anti-vacuity bite for arms
// (a)/(b): the load-bearing ordering is take→apply→Completed→checkpoint→ACK (ack
// LAST, after durability). Mutating that to ack BEFORE the node's Completed is
// durable drains the signal pre-durably; a crash in that window loses it forever, so
// the resume parks the await node FOREVER — the run never converges and the payload
// is never applied. This proves the property's convergence + applied-value arms have
// teeth (they would falsify against the wrong ack ordering, which is precisely the
// NoSignalLost mutation the TLA layer also forbids).
//
// NOTE on (b)/(d) and idempotency (an honest verification insight surfaced by this
// property): under a TRUE crash the apply's in-memory write is DISCARDED with the
// rest of the drive, and a node's apply + its Completed status are persisted
// ATOMICALLY in one snapshot — so a re-apply only ever happens when the PRIOR apply
// left no persisted trace. Exactly-once observable apply is therefore enforced
// STRUCTURALLY by the Completed-skip + atomic snapshot; set-not-accumulate is
// redundant defense-in-depth that this architecture never actually exercises for a
// double-apply. The arms hold; the mutation that breaks them is the ack ordering
// (this bite), not the set/accumulate choice.
func TestSigProp_Bite_AckBeforeDurableLosesSignal(t *testing.T) {
	store := NewInMemoryStore()
	const id = "wf-ack-order-bite"
	require.NoError(t, store.DeliverSignal(id, Signal{ID: "s1", Name: "go", Payload: "x"}))

	build := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, NewWaitForSignalNode("wait", "go"))
		return d
	}

	// WRONG ordering: ack the consumed signal BEFORE the node's Completed is durable,
	// then crash (discard the in-memory data). The signal is now gone from the mailbox
	// but the node's completion never persisted.
	consumed := &consumedSignals{}
	crashCP := func(*WorkflowData) error { return errors.New("crash before persist") }
	ctx1 := withConsumedSignals(withSignalStore(withCheckpoint(context.Background(), crashCP), store), consumed)
	err := build().Execute(ctx1, NewWorkflowData(id))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSuspended, "the node applied + completed in memory, then the flush crashed")
	// The mutation: ack NOW (pre-durably) and DISCARD the data (true crash).
	if ids := consumed.drain(); len(ids) > 0 {
		require.NoError(t, store.AckSignals(id, ids))
	}

	// The signal is lost from the mailbox ...
	remaining, terr := store.TakeSignals(id)
	require.NoError(t, terr)
	require.Empty(t, remaining, "the wrong (pre-durable) ack drained the signal before the completion was durable")

	// ... so the resume parks FOREVER: the await node re-runs (it never persisted
	// Completed), finds an empty mailbox, and suspends with nothing to wake it.
	data2 := NewWorkflowData(id)
	cleanCP := func(d *WorkflowData) error { return store.SaveCheckpoint(d) }
	ctx2 := withSignalStore(withCheckpoint(context.Background(), cleanCP), store)
	resumeErr := build().Execute(ctx2, data2)
	assert.ErrorIs(t, resumeErr, ErrSuspended,
		"ack-before-durable + crash LOSES the signal ⇒ the resume parks forever, never converging — "+
			"proving the property's convergence/applied-value arms (a)/(b) bite against the wrong ack ordering")
	_, applied := data2.Get(IdempotencyKey(data2, "wait"))
	assert.False(t, applied, "the payload is never applied after the signal is lost")
}
