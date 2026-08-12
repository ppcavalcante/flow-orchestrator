package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// M23 VB-01 / T5 — THE ROLLBACK ARM, DRIVEN THROUGH THE TWO ENTRIES THAT BYPASS PUBLIC
// Execute.
//
// What is being pinned, stated narrowly: build() is the only place a boundary
// declaration is ever validated, and the built token is the only durable evidence that
// it happened (the token is never persisted, so a resume re-derives it by rebuilding).
// The rollback drive runs CONSUMER COMPENSATIONS without ever calling DAG.Execute --
// finishRollback walks w.dag.nodes directly -- so on that arm the executeLocked token
// check is the whole mediation, for boundaries exactly as for the graph.
//
// WHY Tick AND DeliverAndResume RATHER THAN Execute. 117 already covers the rollback arm
// through public Execute (sealed_rollback_drive_test.go). These two are the entries that
// take the per-ID lease themselves and call executeLocked DIRECTLY, and they are the
// crash-relevant ones: resume-into-rollback is reached by a timer fire or a signal
// delivery far more often than by a host calling Execute again.
//
// AND ONE OF THEM WAS PREVIOUSLY UNREACHED. TestSeal117_WakeEntriesRefuseUnstampedGraph
// records, honestly, that Tick SHORT-CIRCUITS on its fixture: with no due timer it
// returns before executeLocked, so the token guard is never reached through Tick there.
// These tests seed a DUE PARKED TIMER precisely so Tick does reach it -- the arm 117
// documented as unreachable is reached here.
//
// FIXTURE NOTE (the same technique 117 used, and the reason it is sound): the durable
// state a crash-mid-rollback leaves behind is SEEDED into the store rather than produced
// by a real crash. The property under test is "the rollback arm refuses a graph whose
// boundaries were never validated", not "a crash produces a rollback marker" -- the
// latter is ph48's. Seeding IS the resume: executeLocked loads that state and takes the
// branch, which is exactly what a real resume does.

// seedResumeIntoRollback writes the durable state of a run that crashed mid-rollback
// with a timer still parked: the marker set, one Completed compensable node, and one
// Waiting node whose fireAt has passed so DueTimers reports it due.
//
// No Failed node, deliberately: DueTimers refuses to wake a run holding a hard failure
// (the F2 boundary), so a Failed node here would make Tick short-circuit again and the
// test would pass without ever reaching the code it names.
func seedResumeIntoRollback(t *testing.T, store WorkflowStore, wfID string, past time.Time) {
	t.Helper()
	data := NewWorkflowData(wfID)
	data.SetNodeStatus("d", Completed)
	data.SetNodeStatus("t", Waiting)
	data.SetWait("t", past.UnixNano())
	data.SetRollingBack(true)
	require.NoError(t, store.Save(data))
}

// builtBoundaryRollbackFixture is the SATISFIABLE declaration, built through the public
// API: v -> d -> s with a parked timer off v, and the boundary (d, v, s), which holds
// because v is the sole root and is the verifier. d carries the compensation the
// rollback drive must run.
func builtBoundaryRollbackFixture(t *testing.T, wfID string, compensated *int) *DAG {
	t.Helper()
	noop := func(context.Context, *WorkflowData) error { return nil }
	b := NewWorkflowBuilder().WithWorkflowID(wfID)
	b.AddStartNode("v").WithActionFunc(noop)
	b.AddNode("d").WithActionFunc(noop).DependsOn("v").
		WithCompensationFunc(func(context.Context, *WorkflowData) error { *compensated++; return nil })
	b.AddNode("s").WithActionFunc(noop).DependsOn("d")
	b.AddTimer("t", time.Hour).DependsOn("v")
	b.WithBoundary("d", "v", "s")
	dag, err := b.Build()
	require.NoError(t, err)
	require.True(t, dag.hasBoundaries, "the fixture must actually carry a declaration, or it tests nothing")
	return dag
}

// unvalidatedBoundaryRollbackFixture is the same graph SHAPE hand-assembled without
// build(): three nodes, NO EDGES, carrying a boundary declaration that was never
// validated. This is what a consumer's DAGFactory or an embedded zero-value DAG amounts
// to on the dispatch path.
//
// The declaration it carries is one build() PROVABLY REFUSES -- s is a root, which is
// 118-AF1's class -- and the test asserts that refusal directly rather than asserting it
// from memory. That is what makes this fixture the right one: reaching the rollback
// drive here would run consumer compensations on a graph whose declared boundary is
// false of it.
func unvalidatedBoundaryRollbackFixture(t *testing.T, wfID string, compensated *int) *DAG {
	t.Helper()
	dag := newDAG(wfID) // NOT newDAGForTest: unstamped is the whole point
	for _, name := range []string{"v", "d", "s", "t"} {
		n := newNode(name, ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		if name == "d" {
			n.compensation = ActionFunc(func(context.Context, *WorkflowData) error { *compensated++; return nil })
		}
		require.NoError(t, dag.addNode(n))
	}
	dag.boundaries = []boundaryDecl{{doer: "d", verifier: "v", sink: "s"}}
	dag.hasBoundaries = true
	return dag
}

// TestBoundary_UnvalidatedDeclarationIsRefusedOnTheRollbackArm drives the unvalidated
// graph into resume-into-rollback through BOTH bypassing entries.
func TestBoundary_UnvalidatedDeclarationIsRefusedOnTheRollbackArm(t *testing.T) {
	past := time.Now().Add(-time.Hour)

	// FIRST: prove the declaration the fixture carries is one build() refuses. Without
	// this the test would only show "an unstamped graph is refused" -- true, and 117's
	// already. The point here is that the token is also what keeps an UNVALIDATED
	// BOUNDARY off the arm that runs consumer code.
	b := NewWorkflowBuilder().WithWorkflowID("refused-shape")
	noop := func(context.Context, *WorkflowData) error { return nil }
	b.AddStartNode("v").WithActionFunc(noop)
	b.AddStartNode("d").WithActionFunc(noop)
	b.AddStartNode("s").WithActionFunc(noop)
	b.WithBoundary("d", "v", "s")
	_, buildErr := b.Build()
	require.ErrorIs(t, buildErr, ErrValidation,
		"the fixture's declaration must be one build() actually refuses (118-AF1: the sink is a root)")
	require.Contains(t, buildErr.Error(), "is a root",
		"and refused for that reason, not incidentally")

	entries := []struct {
		drive func(*Workflow) error
		name  string
	}{
		{func(w *Workflow) error { _, err := w.Tick(context.Background(), time.Now()); return err },
			"Tick (timer fire -> executeLocked direct)"},
		{func(w *Workflow) error { return w.DeliverAndResume(context.Background(), Signal{ID: "s1", Name: "n"}) },
			"DeliverAndResume (signal delivery -> executeLocked direct)"},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			compensated := 0
			wfID := "vb01-rollback-refuse"
			store := NewInMemoryStore()
			seedResumeIntoRollback(t, store, wfID, past)
			w := &Workflow{dag: unvalidatedBoundaryRollbackFixture(t, wfID, &compensated), WorkflowID: wfID, Store: store}

			err := e.drive(w)

			require.ErrorIs(t, err, ErrDAGNotBuilt,
				"this entry calls executeLocked directly and must refuse by name: the rollback drive never "+
					"reaches DAG.Execute, so executeLocked's token is the only thing standing between an "+
					"unvalidated declaration and consumer compensations")
			require.Equal(t, 0, compensated,
				"THE NON-VACUITY ASSERTION: a refusal that still ran the compensation would have refused "+
					"nothing that mattered (got %d invocations)", compensated)
		})
	}
}

// TestBoundary_ValidatedDeclarationStillRollsBack is the converse, without which the
// test above proves only that something errored: the SAME scenario with a declaration
// build() accepted must reach the rollback drive and actually compensate. A gate that
// refused everything would pass the refusal arm and fail this one.
func TestBoundary_ValidatedDeclarationStillRollsBack(t *testing.T) {
	past := time.Now().Add(-time.Hour)

	entries := []struct {
		drive func(*Workflow) error
		name  string
	}{
		{func(w *Workflow) error { _, err := w.Tick(context.Background(), time.Now()); return err },
			"Tick (timer fire -> executeLocked direct)"},
		{func(w *Workflow) error { return w.DeliverAndResume(context.Background(), Signal{ID: "s1", Name: "n"}) },
			"DeliverAndResume (signal delivery -> executeLocked direct)"},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			compensated := 0
			wfID := "vb01-rollback-allow"
			store := NewInMemoryStore()
			seedResumeIntoRollback(t, store, wfID, past)
			w := &Workflow{dag: builtBoundaryRollbackFixture(t, wfID, &compensated), WorkflowID: wfID, Store: store}

			err := e.drive(w)

			require.NotErrorIs(t, err, ErrDAGNotBuilt,
				"a built, boundary-carrying graph must not be refused: the token gates PROVENANCE, it does "+
					"not disable the rollback drive for workflows that declare boundaries")
			// ErrRolledBack, not nil: the drive ran the rollback to completion and reports
			// the run's verdict. Asserting NoError here would be asserting the wrong thing
			// -- and it did, on the first run, which is how the expectation got corrected.
			require.ErrorIs(t, err, ErrRolledBack,
				"the rollback drive must complete and report the run rolled back")
			require.Equal(t, 1, compensated,
				"the accepted fixture must actually reach the compensation, or the refusal arm above passes "+
					"for the wrong reason (got %d)", compensated)
		})
	}
}

// TestBoundary_TickReallyReachesExecuteLocked is the REACHABILITY control, and it exists
// because of a specific way the two tests above could both pass while proving nothing.
//
// Tick returns (false, nil) when no timer is due. If the seeded fixture failed to make
// one due -- a fireAt in the future, a status that is not Waiting, a stray Failed node
// tripping the F2 boundary -- then Tick would never call executeLocked, the refusal arm
// would... still fail (nil is not ErrDAGNotBuilt), but the ACCEPT arm would pass with
// zero compensations and no drive at all. So: assert the fired flag, which is Tick's own
// report that it re-entered the resume.
func TestBoundary_TickReallyReachesExecuteLocked(t *testing.T) {
	compensated := 0
	wfID := "vb01-rollback-reach"
	store := NewInMemoryStore()
	seedResumeIntoRollback(t, store, wfID, time.Now().Add(-time.Hour))
	w := &Workflow{dag: builtBoundaryRollbackFixture(t, wfID, &compensated), WorkflowID: wfID, Store: store}

	fired, err := w.Tick(context.Background(), time.Now())
	require.ErrorIs(t, err, ErrRolledBack, "the drive it entered ran the rollback to its verdict")
	require.True(t, fired,
		"fired=false means Tick short-circuited on DueTimers and never called executeLocked — the arm "+
			"under test would not have run at all (this is the state 117 recorded for its own fixture)")
	require.Equal(t, 1, compensated, "and the drive it entered was the rollback drive")
}
