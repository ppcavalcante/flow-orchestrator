package workflow

// M10 phase-35 chunk-1 — N-CYCLE suspend↔resume PROPERTY (adversarial).
//
// The author's headline property (TestSuspend_Property_SuspendWakeConverge) does
// exactly ONE park then wake. This generalizes it to K∈[1,4] park→re-park cycles
// before the wake, over the same random acyclic DAGs with a declared suspension
// node spliced at a varied position, each cycle reloaded from the FlatBuffers store
// (modelling K successive process restarts — "suspend is a crash you chose", K
// times). It asserts the strictly stronger invariant the single-park property
// cannot: across ALL K reloads PLUS the converging run, every node's action runs
// EXACTLY ONCE — the parked node never re-completes, no completed upstream re-runs
// on any of the K reloads, and the final equals a never-suspended run.
//
// Anti-vacuity: each cycle must return ErrSuspended (a swallowed park fails the
// requireParked arm) and persist the parked node Waiting through the FB switches; a
// run that converged early (exec count != 1, or a non-Completed final) falsifies.

import (
	"context"
	"errors"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

func TestSuspend_Property_MultiCycleConverge(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0x4C00B5) // "LOOPS"-ish
	params.MinSuccessfulTests = 100
	params.MaxShrinkCount = 50
	properties := gopter.NewProperties(params)

	baseDir := t.TempDir()

	properties.Property("K park↔re-park cycles then wake ⇒ converge, every node runs exactly once",
		prop.ForAll(
			func(n int, seed int64, edgePermille int, parkSeed int64, idSeed int64, cyclesRaw int) bool {
				spec := buildDAGSpec(n, seed, edgePermille, 0, 0)
				parkIdx := int(uint64(parkSeed) % uint64(spec.n))
				cycles := 1 + (cyclesRaw % 4) // 1..4 park cycles before the wake
				id := "msusp-" + invNodeName(int(uint64(idSeed)%1_000_000))

				store, err := NewFlatBuffersStore(baseDir)
				if err != nil {
					return false
				}
				gate := newParkGate()
				counter := newExecCounter()
				cp := func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }
				build := func() *DAG {
					return buildSuspendDAG(t, spec, parkIdx, id, gate, counter)
				}

				// K successive park→re-park cycles, each reloaded from the store.
				for i := 0; i < cycles; i++ {
					var data *WorkflowData
					if i == 0 {
						data = NewWorkflowData(id)
					} else {
						data, err = store.Load(id)
						if err != nil {
							return false
						}
					}
					if err := build().Execute(withCheckpoint(context.Background(), cp), data); !errors.Is(err, ErrSuspended) {
						return false
					}
					persisted, lerr := store.Load(id)
					if lerr != nil {
						return false
					}
					if st, ok := persisted.GetNodeStatus(invNodeName(parkIdx)); !ok || st != Waiting {
						return false
					}
					// Nothing strictly downstream of the park has run or left Pending.
					for dn := range spec.downstreamOf(parkIdx) {
						if st, _ := persisted.GetNodeStatus(invNodeName(dn)); st != Pending {
							return false
						}
					}
				}

				// The parked node has not completed across any of the K cycles.
				if counter.get(invNodeName(parkIdx)) != 0 {
					return false
				}

				// The event arrives; the re-entry converges.
				gate.wake()
				data, lerr := store.Load(id)
				if lerr != nil {
					return false
				}
				if err := build().Execute(withCheckpoint(context.Background(), cp), data); err != nil {
					return false
				}

				// Converged: every node Completed with its output, and — the stronger
				// invariant — each node's action ran EXACTLY ONCE across all K+1 runs.
				for i := 0; i < spec.n; i++ {
					name := invNodeName(i)
					if st, _ := data.GetNodeStatus(name); st != Completed {
						return false
					}
					if out, ok := data.GetOutput(name); !ok || out != "out-"+name {
						return false
					}
					if counter.get(name) != 1 {
						return false
					}
				}
				return true
			},
			gen.IntRange(1, 8),    // n
			gen.Int64(),           // structural seed
			gen.IntRange(0, 1000), // edgePermille
			gen.Int64(),           // park-position seed
			gen.Int64(),           // workflow-id seed
			gen.IntRange(0, 3),    // cyclesRaw -> 1..4 park cycles
		))

	properties.TestingRun(t)
}
