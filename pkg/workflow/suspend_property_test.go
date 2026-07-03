package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// M10 phase-35 chunk-1 T6 — the headline suspend -> wake -> converge property.
//
// Over random acyclic DAGs with a declared suspension node spliced into a varied
// position, assert:
//   - PARK: the first run returns ErrSuspended, the parked node is persisted
//     Waiting (through the FlatBuffers conversion switches), and the seam was
//     actually traversed (the gate observed a park) — a "wakes before it parked"
//     run is rejected.
//   - MODEL A / WaitingIsLiveNotTerminal: every node strictly downstream of the
//     park stays Pending and does NOT run while parked.
//   - CONVERGE: after the wake, re-entering Execute drains the whole run to the
//     no-suspend final (every node Completed with its output), and no node that
//     completed before the park re-runs on resume.
//
// Anti-vacuity bite-proofs (must FALSIFY — receipts in the SUMMARY / verified by
// the harness mutation runs):
//   - ErrSuspended -> nil (executor swallows the park): the requireParked arm fails.
//   - wake-without-having-parked: the gate.parkRuns >= 1 + persisted-Waiting arm fails.
//   - drop the Waiting case from statusToFBStatus: the persisted-Waiting arm fails
//     (the FB store clobbers Waiting -> Pending across the checkpoint round-trip).
//
// A FlatBuffersStore is used deliberately so the park status round-trips through
// statusToFBStatus / fbStatusToNodeStatus — that is what makes the drop-Waiting
// mutation observable here (an InMemoryStore copies the map and would hide it).
func TestSuspend_Property_SuspendWakeConverge(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x5057E2D0) // "SUSPEND" -ish
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 50
	properties := gopter.NewProperties(params)

	baseDir := t.TempDir()

	properties.Property("park at any node ⇒ suspend persists Waiting, run converges on wake, no completed node re-runs",
		prop.ForAll(
			func(n int, seed int64, edgePermille int, parkSeed int64, idSeed int64) bool {
				spec := buildDAGSpec(n, seed, edgePermille, 0, 0)
				parkIdx := int(uint64(parkSeed) % uint64(spec.n))
				id := "susp-prop-" + invNodeName(int(uint64(idSeed)%1_000_000))

				store, err := NewFlatBuffersStore(baseDir)
				require.NoError(t, err)

				gate := newParkGate()
				counter := newExecCounter()

				build := func() *DAG {
					return buildSuspendDAG(t, spec, parkIdx, id, gate, counter)
				}

				// --- Phase 1: run until the park. ---
				dag1 := build()
				cp := func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }
				data1 := NewWorkflowData(id)
				err1 := dag1.Execute(withCheckpoint(context.Background(), cp), data1)

				// PARK arm: the run must suspend (not complete, not fail).
				if !errors.Is(err1, ErrSuspended) {
					return false
				}
				// Seam-traversal (anti-vacuity): the gate must have actually observed
				// a park — a run that "woke" without parking is rejected.
				if parkRuns, _ := gate.stats(); parkRuns < 1 {
					return false
				}
				// The parked node is persisted Waiting THROUGH the FB switches.
				persisted, lerr := store.Load(id)
				if lerr != nil {
					return false
				}
				if st, ok := persisted.GetNodeStatus(invNodeName(parkIdx)); !ok || st != Waiting {
					return false
				}
				// MODEL A / WaitingIsLiveNotTerminal: everything strictly downstream
				// of the park is still Pending and did not run while parked.
				for d := range spec.downstreamOf(parkIdx) {
					if st, _ := persisted.GetNodeStatus(invNodeName(d)); st != Pending {
						return false
					}
					if counter.get(invNodeName(d)) != 0 {
						return false
					}
				}

				// --- Phase 2: the external event arrives; resume converges. ---
				gate.wake()
				data2, lerr := store.Load(id)
				if lerr != nil {
					return false
				}
				dag2 := build()
				cp2 := func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }
				if err := dag2.Execute(withCheckpoint(context.Background(), cp2), data2); err != nil {
					return false
				}
				// CONVERGE: every node ended Completed with its output present — the
				// same final a never-suspended run reaches.
				for i := 0; i < spec.n; i++ {
					name := invNodeName(i)
					if st, _ := data2.GetNodeStatus(name); st != Completed {
						return false
					}
					if out, ok := data2.GetOutput(name); !ok || out != "out-"+name {
						return false
					}
				}
				// No node that completed before the park re-runs on resume: each node
				// completes exactly once across the two phases (the park runs its
				// onComplete once; upstreams once; downstream once).
				for i := 0; i < spec.n; i++ {
					if counter.get(invNodeName(i)) != 1 {
						return false
					}
				}
				return true
			},
			gen.IntRange(1, 10),   // n
			gen.Int64(),           // structural seed (edges)
			gen.IntRange(0, 1000), // edgePermille
			gen.Int64(),           // park-position seed
			gen.Int64(),           // workflow-id seed (unique file per iteration)
		))

	properties.TestingRun(t)
}

// TestSuspend_Property_NoCheckpointerRefuses is the D-11 (DEC-M10-P35-A) property:
// over random acyclic DAGs with a suspension node spliced in, running with NO
// durable checkpoint wired (DAG.Execute driven directly) must return the distinct
// ErrSuspendRequiresCheckpointer — NEVER ErrSuspended (which would be a silently
// non-durable park). Every generated node succeeds, so the park is always reached
// and always parks; the contract must refuse it the same way every time.
func TestSuspend_Property_NoCheckpointerRefuses(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x0D11C0DE)
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 50
	properties := gopter.NewProperties(params)

	properties.Property("park with no Checkpointer wired ⇒ ErrSuspendRequiresCheckpointer, never ErrSuspended",
		prop.ForAll(
			func(n int, seed int64, edgePermille int, parkSeed int64) bool {
				spec := buildDAGSpec(n, seed, edgePermille, 0, 0)
				parkIdx := int(uint64(parkSeed) % uint64(spec.n))
				gate := newParkGate() // starts parked; the event never fires here
				counter := newExecCounter()

				// No checkpoint wired (no withCheckpoint injection → checkpointFrom(ctx) nil).
				dag := buildSuspendDAG(t, spec, parkIdx, "susp-nocp-prop", gate, counter)
				err := dag.Execute(context.Background(), NewWorkflowData("susp-nocp-prop"))

				return errors.Is(err, ErrSuspendRequiresCheckpointer) && !errors.Is(err, ErrSuspended)
			},
			gen.IntRange(1, 10),
			gen.Int64(),
			gen.IntRange(0, 1000),
			gen.Int64(),
		))

	properties.TestingRun(t)
}

// buildSuspendDAG materializes a dagSpec into a real DAG where node parkIdx is a
// declared suspension node (parks per the gate) and every other node is a
// counting success node. Built via NewDAG directly so the typed suspendingAction
// can be injected (the builder's WithAction wraps a func as ActionFunc, which is
// not suspension-capable).
func buildSuspendDAG(t *testing.T, spec dagSpec, parkIdx int, id string, gate *parkGate, c *execCounter) *DAG {
	t.Helper()
	d := NewDAG(id)
	for i := 0; i < spec.n; i++ {
		name := invNodeName(i)
		if i == parkIdx {
			mustAddNode(t, d, completingSuspendNode(name, gate, c))
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
