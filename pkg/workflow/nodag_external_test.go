package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
	"github.com/stretchr/testify/require"
)

// THE PARTIAL SEAL, and its guard. T6 seals Workflow's graph handle but deliberately
// leaves the seven config knobs beside it exported, so this stays legal from outside:
//
//	&workflow.Workflow{WorkflowID: "x", Store: s}
//
// It compiles, it has no graph, and before ErrWorkflowNoDAG every public drive entry
// PANICKED on it with an invalid-memory-address dereference. T6 would otherwise have
// removed the WORKING external idiom (&Workflow{DAG: dag, …}) while leaving the BROKEN
// one legal — the inverse of what the phase claims to do.
//
// IN package workflow_test ON PURPOSE. In-package this literal is unremarkable and a
// test could simply assign the unexported field, so an in-package version would prove
// the guard fires without proving anyone outside can reach the state that needs it.
// External construction IS the finding; this file is the only place it can be stated.
//
// PLACEMENT WAS AN ENUMERATION, NOT AN INSTINCT — the point of the four arms below.
// "Guard the three public drive entries" is the obvious design and it is wrong twice:
//
//   - DueTimers is a FOURTH public entry that reaches the graph on its own, via
//     runHasHardFailure.
//   - Tick reaches THAT deref BEFORE executeLocked — so a guard placed in the single
//     funnel, which is the natural home because every drive passes through it, leaves
//     Tick panicking anyway.
//
// Both were established by driving the panic and reading the stack, never by reading
// the call graph. Measured at HEAD before the guard, all four arms:
//
//	Execute              panic: (*DAG).Validate <- executeLocked <- Execute
//	Tick (timer due)     panic: (*DAG).GetNode <- checkGraphIdentity <- executeLocked <- Tick
//	Tick (due + Failed)  panic: (*DAG).GetNode <- runHasHardFailure <- DueTimers <- Tick
//	DeliverAndResume     panic: (*DAG).GetNode <- checkGraphIdentity <- executeLocked <- DeliverAndResume
//
// The third row is the one that decided the design: it is the only arm that never
// enters executeLocked, and it is reachable ONLY when the run holds a Failed node —
// which is why the plain parked arm above it came back clean. A clean result there
// would have been read as "Tick is covered".
//
// BITE — TWO of them, one per guard, and the MEASURED partition is below. It is not
// the partition I predicted when I wrote this comment: I expected removing the
// executeLocked guard to red the Tick arms too, and it does not, because Tick calls
// DueTimers FIRST and the other guard catches it there. The table is what the arms
// actually did, run one at a time (a panic kills the test binary, so a single run
// reports only the first arm and would have hidden this):
//
//	arm                        exec-guard removed   DueTimers-guard removed
//	Execute                    PANIC                pass
//	Tick (due)                 pass                 pass
//	Tick (due + Failed)        pass                 PANIC
//	DeliverAndResume           PANIC                pass
//	DueTimers direct           pass                 FAIL (assertion)
//
// Read it as: the two guards cover DISJOINT arms and neither is redundant —
// executeLocked is load-bearing for Execute and DeliverAndResume, DueTimers for the
// hard-failure Tick and the direct call. "Tick (due)" is the one arm either guard
// alone would cover, which is exactly why it is not sufficient evidence on its own.
//
// Note the two failure SHAPES. Four cells red as a `panic: runtime error: invalid
// memory address or nil pointer dereference`, not as a testify assertion — a
// panicking test reports differently from a failing one, so read the panic line.
// The DueTimers-direct cell reds as a normal assertion instead, because that call
// returns rather than dereferencing.
func TestWorkflowNoDAG_EveryPublicDriveEntryIsRefused(t *testing.T) {
	ctx := context.Background()

	// parked writes a durable state whose node "n" is Waiting on a timer that is
	// already due. Without it Tick returns (false, nil) from DueTimers' early-out and
	// never approaches the graph — the vacuous arm this precondition removes.
	parked := func(t *testing.T, store workflow.WorkflowStore, extra func(*workflow.WorkflowData)) {
		t.Helper()
		d := workflow.NewWorkflowData("x")
		d.SetNodeStatus("n", workflow.Waiting)
		d.SetWait("n", time.Now().Add(-time.Hour).UnixNano())
		if extra != nil {
			extra(d)
		}
		require.NoError(t, store.Save(d))
	}

	t.Run("Execute", func(t *testing.T) {
		w := &workflow.Workflow{WorkflowID: "x", Store: workflow.NewInMemoryStore()}
		err := w.Execute(ctx)
		require.ErrorIs(t, err, workflow.ErrWorkflowNoDAG,
			"Execute on a graph-less Workflow must name its cause, not dereference nil")
	})

	t.Run("Tick with a due timer", func(t *testing.T) {
		store := workflow.NewInMemoryStore()
		parked(t, store, nil)
		w := &workflow.Workflow{WorkflowID: "x", Store: store}
		_, err := w.Tick(ctx, time.Now())
		require.ErrorIs(t, err, workflow.ErrWorkflowNoDAG)
	})

	// The arm that decides guard placement: it reaches runHasHardFailure, which is
	// upstream of executeLocked. A Failed node is the precondition — runHasHardFailure
	// calls GetNode only for a Failed status, so without one the nil graph is never
	// touched here.
	t.Run("Tick with a due timer and a hard failure", func(t *testing.T) {
		store := workflow.NewInMemoryStore()
		parked(t, store, func(d *workflow.WorkflowData) { d.SetNodeStatus("f", workflow.Failed) })
		w := &workflow.Workflow{WorkflowID: "x", Store: store}
		_, err := w.Tick(ctx, time.Now())
		require.ErrorIs(t, err, workflow.ErrWorkflowNoDAG)
	})

	t.Run("DeliverAndResume", func(t *testing.T) {
		store, err := workflow.NewSQLiteStore(":memory:")
		require.NoError(t, err, "DeliverAndResume needs a SignalStore; InMemoryStore is not one")
		t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // cleanup
		parked(t, store, nil)
		w := &workflow.Workflow{WorkflowID: "x", Store: store}
		err = w.DeliverAndResume(ctx, workflow.Signal{ID: "sig-1", Name: "s"})
		require.ErrorIs(t, err, workflow.ErrWorkflowNoDAG)
	})

	// DueTimers reached DIRECTLY, because it is public and a host may call it to decide
	// whether to Tick at all. Listing it as an entry point is not enough; it has to be
	// driven, or the claim "all four public entries" rests on the same kind of reading
	// that produced the three-entry framing.
	t.Run("DueTimers called directly", func(t *testing.T) {
		store := workflow.NewInMemoryStore()
		parked(t, store, nil)
		w := &workflow.Workflow{WorkflowID: "x", Store: store}
		_, err := w.DueTimers(time.Now())
		require.ErrorIs(t, err, workflow.ErrWorkflowNoDAG)
	})
}

// ANTI-VACUITY CONTROL, and it guards a specific way the test above could rot into
// theatre. Every arm there asserts an error; every arm would still "pass" if the
// durable state I build were never actually drive-worthy — if the wait entry were not
// due, Tick would bail out of DueTimers early and the arms would be asserting the
// guard from a path that never approaches the graph.
//
// So: the SAME store state, driven by a properly built Workflow. fired=true is the
// measurement that matters — it says DueTimers got past every early-out and found the
// timer genuinely due, which is exactly the precondition the arms above depend on and
// cannot check for themselves.
//
// It is also the positive half of the seal: a Workflow built the sanctioned way runs.
// A guard that refused everything would pass every arm above.
func TestWorkflowNoDAG_ControlTheParkedStateIsGenuinelyDriveWorthy(t *testing.T) {
	store := workflow.NewInMemoryStore()

	b := workflow.NewWorkflowBuilder().WithWorkflowID("x").WithStore(store)
	b.AddStartNode("n").WithAction(workflow.ActionFunc(
		func(context.Context, *workflow.WorkflowData) error { return nil }))
	w, err := workflow.FromBuilder(b)
	require.NoError(t, err)

	d := workflow.NewWorkflowData("x")
	d.SetNodeStatus("n", workflow.Waiting)
	d.SetWait("n", time.Now().Add(-time.Hour).UnixNano())
	require.NoError(t, store.Save(d))

	fired, err := w.Tick(context.Background(), time.Now())
	require.True(t, fired,
		"the parked state must be genuinely due, or the refusal arms assert the guard "+
			"from a path that never reaches the graph")
	require.False(t, errors.Is(err, workflow.ErrWorkflowNoDAG),
		"a Workflow built the sanctioned way must never trip the no-DAG guard")
}
