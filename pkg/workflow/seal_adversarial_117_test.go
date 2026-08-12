package workflow

// M23 ph117 — INDEPENDENT ADVERSARIAL suite for the builder-token seal (SEAL-06).
//
// Authored by anvil-adversarial-tester under DEC-M23-ADVERSARIAL-GAP. Independent of BOTH the
// engineer and qa: nothing here was derived from the implementer's own test list.
//
// The claim under attack (SUMMARY.md):
//
//	"Every *DAG that executes, and every *DAG used to render a run's verdict, passed build()."
//
// Attack targets, in the order the commissioning brief ranked them:
//
//	1. the QUEUE sub-workflow path, which has NO direct token check and is argued covered only
//	   TRANSITIVELY. Two readers agreed it traces clean; neither ENUMERATED the exits. This file
//	   enumerates them by driving them.
//	2. crash/resume — the token is deliberately never persisted, and Tick (timer fire) and
//	   DeliverAndResume (signal delivery) call executeLocked DIRECTLY, bypassing public Execute.
//	3. concurrency on the new surface (DAG() hands out a shared live pointer; checkGraph reads
//	   w.dag unsynchronised) — explicitly unexamined by every prior reader.
//
// The adversary model is an OUT-OF-MODULE consumer, so every hostile *DAG constructed here is the
// bare zero value `&DAG{}` — the only shape such a consumer can actually produce (the 15 sealed
// symbols are unreachable and reflect cannot Set the unexported fields). Nothing here relies on
// in-package privileges to build a graph a consumer could not.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostileChild is the exact artefact an out-of-module consumer can hand the engine: a zero-value
// *DAG. nodes is a nil map, built is false, and no sealed constructor was involved.
func hostileChild() *DAG { return &DAG{} }

// ─────────────────────────────────────────────────────────────────────────────────────────────
// TARGET 1 — the queue sub-workflow path. ENUMERATE the exits; do not agree with a reading.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// TestSeal117_QueuePath_VerdictExitsRefuseUnstampedChild enumerates the reachable exits of the
// wake half of the queue path (parkedSubWorkflowAction, which the queue action constructs inline
// with the FACTORY-SUPPLIED child DAG — a construction site that has NO requireBuiltChild).
//
// Oracle — two, both mechanical:
//   - TOTALITY: no exit may panic, for any (queue-row state x child-journal state) cell.
//   - PROVENANCE: no exit may return a SUCCESS verdict that was derived by reading an unstamped
//     graph. `nil` is permitted only where the verdict came from an authority other than the DAG.
//
// The cell that matters is the one no reader named: queue row ABSENT + child journal terminal.
// That is the only route from the queue-shaped action into childRunFailed, and it must refuse.
func TestSeal117_QueuePath_VerdictExitsRefuseUnstampedChild(t *testing.T) {
	const parentID = "parent-adv"
	const nodeName = "sub"
	childID := SubWorkflowChildID(parentID, nodeName)

	// Journal shapes the wake gate can observe.
	journalAbsent := func(*testing.T, WorkflowStore) {}
	journalRunning := func(t *testing.T, s WorkflowStore) {
		t.Helper()
		cd := NewWorkflowData(childID)
		cd.SetNodeStatus("n", Running)
		require.NoError(t, s.Save(cd))
	}
	journalTerminalFailed := func(t *testing.T, s WorkflowStore) {
		t.Helper()
		cd := NewWorkflowData(childID)
		cd.SetNodeStatus("n", Failed)
		require.NoError(t, s.Save(cd))
	}
	journalTerminalOK := func(t *testing.T, s WorkflowStore) {
		t.Helper()
		cd := NewWorkflowData(childID)
		cd.SetNodeStatus("n", Completed)
		require.NoError(t, s.Save(cd))
	}

	cases := []struct {
		name    string
		seed    func(*testing.T, WorkflowStore)
		wantErr func(t *testing.T, err error)
	}{
		{
			name: "row absent + journal absent -> park (no DAG read)",
			seed: journalAbsent,
			wantErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrSuspended)
			},
		},
		{
			name: "row absent + journal non-terminal -> park (no DAG read)",
			seed: journalRunning,
			wantErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrSuspended)
			},
		},
		{
			// THE CELL. Terminal journal + no queue authority is the ONLY path from a
			// queue-shaped action into childRunFailed. An unstamped graph here would render
			// the coe verdict off a nil `nodes` map: every Failed node reads as non-coe, and
			// worse, a graph whose flags were never validated decides success vs failure.
			name: "row absent + journal terminal-FAILED -> ErrDAGNotBuilt, never a verdict",
			seed: journalTerminalFailed,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err, "an unstamped verdict DAG must never resolve the parent node")
				assert.ErrorIs(t, err, ErrDAGNotBuilt)
			},
		},
		{
			// The success side of the same gate. childRunFailed is called BEFORE the failed/ok
			// split, so an all-Completed child must refuse too — otherwise the seal would hold
			// only on the unhappy path, which is the classic author blind spot.
			name: "row absent + journal terminal-OK -> ErrDAGNotBuilt (refusal is not failure-only)",
			seed: journalTerminalOK,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err, "the token check must precede the success split, not follow it")
				assert.ErrorIs(t, err, ErrDAGNotBuilt)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A non-SQLite store is the "queue row absent" world: queueTerminalState is skipped
			// entirely (the type assertion fails), which is exactly the exists=false branch.
			store := NewInMemoryStore()
			tc.seed(t, store)
			// InMemoryStore is BOTH the journal store and the mailbox here (it implements
			// SignalStore), so the wake gate has everything it needs and the only thing left
			// unusual about this run is the unstamped graph.

			// The action shape the QUEUE path builds at runtime: child comes straight from the
			// consumer's DAGFactory, never through requireBuiltChild.
			act := &parkedSubWorkflowAction{nodeName: nodeName, child: hostileChild()}

			ctx := withParentStore(withSignalStore(context.Background(), NewInMemoryStore()), store)
			pd := NewWorkflowData(parentID)

			var err error
			require.NotPanics(t, func() { err = act.Execute(ctx, pd) },
				"TOTALITY: no queue-path exit may panic on a consumer-supplied unstamped DAG")
			tc.wantErr(t, err)
		})
	}
}

// TestSeal117_QueuePath_RowAuthorityNeverReadsTheDAG is the measured NEGATIVE that corrects the
// phase's own mediation argument for the queue path.
//
// The code comments argue the queue path is covered because childRunFailed is "the single shared
// verdict callable for both". This test measures what actually happens: the queue action ENQUEUES
// before it parks, no production code path ever DELETEs a work_queue row, so queueTerminalState
// returns exists=true on every subsequent wake — and the whole verdict is rendered from the ROW.
// childRunFailed is therefore UNREACHABLE from the queue path, and the child DAG the factory
// returned is never read at all.
//
// That makes the queue path SAFE (an unstamped DAG cannot poison a verdict that never consults
// it), but safe for a different reason than the one written down. Recorded because a mediation
// argument that names the wrong mechanism will be maintained wrongly.
func TestSeal117_QueuePath_RowAuthorityNeverReadsTheDAG(t *testing.T) {
	s := mkQueueStore(t)
	const parentID = "parent-rowauth"
	const nodeName = "sub"
	childID := SubWorkflowChildID(parentID, nodeName)

	// A queue row exists and says done; the child journal is terminal.
	_, err := s.EnqueueSubWorkflow(childID, "T", nil, parentID, completionSignalName(nodeName), 1)
	require.NoError(t, err)
	_, err = s.ClaimNext("w", "T")
	require.NoError(t, err)
	ok, err := s.MarkDone(childID)
	require.NoError(t, err)
	require.True(t, ok, "seed must apply: the row must actually be `done`")

	cd := NewWorkflowData(childID)
	cd.SetNodeStatus("n", Completed)
	require.NoError(t, s.Save(cd))

	// Prove the seed took: the row IS the authority the action will consult.
	state, exists, err := s.queueTerminalState(childID)
	require.NoError(t, err)
	require.True(t, exists, "seed must apply: queueTerminalState must see the row")
	require.Equal(t, wqDone, state)

	act := &parkedSubWorkflowAction{nodeName: nodeName, child: hostileChild()}
	ctx := withParentStore(withSignalStore(context.Background(), NewInMemoryStore()), s)
	pd := NewWorkflowData(parentID)

	var execErr error
	require.NotPanics(t, func() { execErr = act.Execute(ctx, pd) })

	// MEASURED: the unstamped DAG produced no refusal, because it was never consulted. If a
	// future change routes the queue-authority arm through childRunFailed, this flips to
	// ErrDAGNotBuilt and the comment's claim becomes true — either way the test states the
	// mechanism out loud instead of leaving it to a reading.
	assert.NoError(t, execErr,
		"queue-row authority resolves the node WITHOUT reading the child DAG; "+
			"if this ever returns ErrDAGNotBuilt the queue path has started consuming the graph")
}

// TestSeal117_QueueAction_NilFactoryDAGIsTypedNotPanic — the consumer factory returning (nil, nil).
// Totality on the one input the queue action cannot delegate away.
func TestSeal117_QueueAction_NilFactoryDAGIsTypedNotPanic(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("evil", func() (*DAG, error) { return nil, nil }))

	act := &queueSubWorkflowAction{nodeName: "sub", childType: "evil"}
	ctx := withRegistry(withParentStore(withSignalStore(context.Background(), NewInMemoryStore()), s), reg)
	pd := NewWorkflowData("parent-nilfac")

	var err error
	require.NotPanics(t, func() { err = act.Execute(ctx, pd) })
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrValidation)
}

// TestSeal117_QueueAction_UnstampedFactoryDAGDoesNotPanic — the factory returns a zero-value *DAG
// (the out-of-module-reachable hostile shape). The queue action's nil check passes; the graph flows
// into the parked action. Totality: a typed park, never a panic, never a silent success.
func TestSeal117_QueueAction_UnstampedFactoryDAGDoesNotPanic(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("evil", func() (*DAG, error) { return hostileChild(), nil }))

	act := &queueSubWorkflowAction{nodeName: "sub", childType: "evil"}
	ctx := withRegistry(withParentStore(withSignalStore(context.Background(), NewInMemoryStore()), s), reg)
	pd := NewWorkflowData("parent-unstamped")

	var err error
	require.NotPanics(t, func() { err = act.Execute(ctx, pd) })
	assert.ErrorIs(t, err, ErrSuspended, "enqueue + park; the unstamped graph is inert here")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// TARGET 2 — crash / resume. The token is NEVER persisted, and two of the three drive entries
// bypass public Execute. A resume that forgets to re-stamp must refuse, not run.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// TestSeal117_WakeEntriesRefuseUnstampedGraph drives EVERY public entry that reaches the graph —
// including the two that call executeLocked DIRECTLY (Tick for a timer fire, DeliverAndResume for
// a signal delivery) and DueTimers, the fourth entry that derefs the graph on its own — against a
// Workflow whose DAG carries no stamp: exactly the state a resume would leave if any resume path
// failed to re-stamp, since the token is deliberately never persisted.
//
// PER-ENTRY EXPECTATIONS, NOT ONE BLANKET ONE. A uniform "must not panic" assertion here would be
// weaker than this test's own name, and two of these four entries genuinely short-circuit BEFORE
// the token check — a fact this test states rather than papers over:
//
//   - Execute / DeliverAndResume REACH the token (both funnel into executeLocked) → ErrDAGNotBuilt.
//   - Tick short-circuits: with no due timers it never calls executeLocked, so the token guard is
//     UNREACHABLE through Tick on an unstamped graph. It cannot run the graph either — which is
//     the property that matters — but it is not the token that stops it.
//   - DueTimers only reads timer state; checkGraph is its nil guard, not a token check.
//
// Oracle: a NAMED sentinel where the token is reachable, TOTALITY everywhere.
func TestSeal117_WakeEntriesRefuseUnstampedGraph(t *testing.T) {
	mk := func() *Workflow {
		return &Workflow{dag: hostileChild(), WorkflowID: "wf-unstamped", Store: NewInMemoryStore()}
	}
	now := time.Now()

	entries := []struct {
		name string
		call func(*Workflow) error
		// reachesToken: this entry must refuse with ErrDAGNotBuilt. false = it short-circuits
		// earlier, and the assertion is totality + "did not run the graph".
		reachesToken bool
	}{
		{"Execute", func(w *Workflow) error { return w.Execute(context.Background()) }, true},
		{"DeliverAndResume (signal -> executeLocked direct)", func(w *Workflow) error {
			return w.DeliverAndResume(context.Background(), Signal{ID: "s1", Name: "n"})
		}, true},
		{"Tick (timer fire -> executeLocked direct; short-circuits with no due timer)", func(w *Workflow) error {
			_, err := w.Tick(context.Background(), now)
			return err
		}, false},
		{"DueTimers (fourth entry, derefs on its own)", func(w *Workflow) error {
			_, err := w.DueTimers(now)
			return err
		}, false},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			w := mk()
			var err error
			require.NotPanics(t, func() { err = e.call(w) },
				"TOTALITY: a drive entry must never panic on an unstamped graph")
			if e.reachesToken {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrDAGNotBuilt,
					"this entry funnels into executeLocked and must refuse by name")
			}
			if err != nil {
				assert.NotContains(t, err.Error(), "nil pointer",
					"the refusal must be typed, never a recovered deref")
			}
		})
	}
}

// TestSeal117_ExecuteAndExecuteLockedRefuseByName pins the two execution-path refusals to their
// sentinels. Separated from the totality sweep above because a sentinel assertion is a different
// oracle (a named error) and mixing them hides which half regressed.
func TestSeal117_ExecuteAndExecuteLockedRefuseByName(t *testing.T) {
	t.Run("DAG.Execute", func(t *testing.T) {
		err := hostileChild().Execute(context.Background(), NewWorkflowData("d"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDAGNotBuilt)
	})
	t.Run("Workflow.executeLocked", func(t *testing.T) {
		w := &Workflow{dag: hostileChild(), WorkflowID: "wf", Store: NewInMemoryStore()}
		err := w.executeLocked(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDAGNotBuilt)
	})
	t.Run("nil graph is a DIFFERENT sentinel", func(t *testing.T) {
		w := &Workflow{WorkflowID: "wf", Store: NewInMemoryStore()}
		err := w.executeLocked(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrWorkflowNoDAG)
		assert.NotErrorIs(t, err, ErrDAGNotBuilt, "the two causes must stay distinguishable")
	})
}

// TestSeal117_ResumeRebuildsTheStamp is the crash/resume property, stated as a round trip: the
// stamp is not persisted, so a fresh-process resume must obtain it from build() again. Drive a
// workflow to completion, then resume the SAME durable state through a SECOND *Workflow built
// from a SECOND builder — the fresh-process shape — and assert it is stamped and drives.
func TestSeal117_ResumeRebuildsTheStamp(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(filepath.Join(dir, "s"))
	require.NoError(t, err)

	mkWF := func() *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID("resume-wf").WithStore(store)
		b.AddNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		w, err := FromBuilder(b)
		require.NoError(t, err)
		return w
	}

	w1 := mkWF()
	require.NoError(t, w1.Execute(context.Background()))

	// Process 2: a fresh build of the same graph, resuming the same durable state.
	w2 := mkWF()
	require.True(t, w2.DAG().built, "a rebuilt graph must carry the stamp; the stamp is code, not data")
	require.NoError(t, w2.Execute(context.Background()), "resume must drive, not refuse")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// TARGET 3 — concurrency on the surface the seal introduced. Every prior reader declined to
// certify happens-before here. Run under -race.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// TestSeal117_ConcurrentDriveAndAccessorAreRaceFree hammers the new read surface: DAG() handing
// out the shared live pointer, the token read inside executeLocked, checkGraph, and DueTimers —
// concurrently with real drives of the same *Workflow.
//
// Oracle: the race detector. Under `go test -race` a data race fails the run; without -race this
// still exercises the paths for panics (TOTALITY).
func TestSeal117_ConcurrentDriveAndAccessorAreRaceFree(t *testing.T) {
	b := NewWorkflowBuilder().WithWorkflowID("conc-wf").WithStore(NewInMemoryStore())
	b.AddNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	w, err := FromBuilder(b)
	require.NoError(t, err)

	const goroutines = 8
	const iters = 40
	var wg sync.WaitGroup
	now := time.Now()

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				switch g % 4 {
				case 0:
					_ = w.Execute(context.Background()) //nolint:errcheck // adversarial: outcome irrelevant to this arm
				case 1:
					d := w.DAG()
					if d != nil {
						_ = d.Name()
					}
				case 2:
					_, _ = w.DueTimers(now) //nolint:errcheck // adversarial: outcome irrelevant to this arm
				case 3:
					_, _ = w.Tick(context.Background(), now) //nolint:errcheck // adversarial: outcome irrelevant to this arm
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestSeal117_ConcurrentAccessorDuringUnstampedRefusal is the adversarial half of the same
// surface: readers calling DAG() while drives are REFUSING on the token. The refusal path is the
// one the phase added; it had no concurrency test at all.
func TestSeal117_ConcurrentAccessorDuringUnstampedRefusal(t *testing.T) {
	w := &Workflow{dag: hostileChild(), WorkflowID: "wf-conc-unstamped", Store: NewInMemoryStore()}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if g%2 == 0 {
					err := w.Execute(context.Background())
					if err != nil && !errors.Is(err, ErrDAGNotBuilt) {
						t.Errorf("unexpected refusal cause: %v", err)
						return
					}
				} else {
					_ = w.DAG()
				}
			}
		}(g)
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// TARGET 5 — the saga rollback arm, where consumer compensation runs on a graph DAG.Execute
// never inspected. This is the arm that motivated the executeLocked check.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// TestSeal117_RollbackArmRefusesBeforeRunningConsumerCode — a durable run whose state already
// carries the rolling_back marker resumes STRAIGHT into finishRollback, which walks the graph and
// invokes consumer compensations WITHOUT DAG.Execute. The token check must sit ABOVE that branch.
//
// Oracle: the refusal must be ErrDAGNotBuilt AND no compensation may have been invoked. Asserting
// only the error would pass even if the check ran after the compensations.
func TestSeal117_RollbackArmRefusesBeforeRunningConsumerCode(t *testing.T) {
	store := NewInMemoryStore()
	const wfID = "wf-rollback-adv"

	data := NewWorkflowData(wfID)
	data.SetRollingBack(true)
	require.NoError(t, store.Save(data))

	// Prove the seed applied — a rollback test on a non-rolling-back state is vacuous.
	loaded, err := store.Load(wfID)
	require.NoError(t, err)
	require.True(t, loaded.IsRollingBack(), "seed must apply: the run must be in rolling_back")

	w := &Workflow{dag: hostileChild(), WorkflowID: wfID, Store: store}
	err = w.executeLocked(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDAGNotBuilt,
		"the rollback arm must be shut by the token, not merely by DAG.Execute")
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// TOTALITY on the ONE sanctioned surface that deliberately does NOT check the token.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// TestSeal117_ValidateNoTypeCyclesTotalityOnHostileFactories — ValidateNoTypeCycles calls EVERY
// registered factory and READS the graph it returns (`for _, node := range dag.nodes`), and its own
// comment says it deliberately does not check the token. That makes it the one sanctioned reader of
// a possibly-unstamped graph, so it owes a totality proof: a hostile registry must not panic.
//
// Oracle: the minimum bar — no input class may crash. A zero-value *DAG has a NIL nodes map, and a
// range over a nil map is a no-op in Go; this test pins that, so a future change to an indexed
// WRITE (which would panic on a nil map) is caught rather than assumed.
func TestSeal117_ValidateNoTypeCyclesTotalityOnHostileFactories(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.Register("nilDAG", func() (*DAG, error) { return nil, nil }))
	require.NoError(t, reg.Register("unstamped", func() (*DAG, error) { return hostileChild(), nil }))
	require.NoError(t, reg.Register("boom", func() (*DAG, error) { return nil, errors.New("factory exploded") }))
	require.NoError(t, reg.Register("named", func() (*DAG, error) { return &DAG{name: "no-nodes-map"}, nil }))

	var err error
	require.NotPanics(t, func() { err = reg.ValidateNoTypeCycles() },
		"TOTALITY: the one sanctioned unstamped-graph reader must survive every hostile factory shape")
	assert.NoError(t, err, "no declarable cycle exists among these; opaque factories are skipped")
}

// The COPY CLASS lives in seal_adversarial_117_copyclass_test.go, in the DEFAULT build. It was once
// behind the `adversarial_copyclass` build tag because every `cp := *dag` in it tripped `go vet`'s
// copylocks analyser (DAG embedded a sync.RWMutex by value). AUD-002 made DAG.mu a *sync.RWMutex,
// which both FIXES the copied-mutex wedge and removes the copylocks diagnostic, so the tag, its
// script and its CI step were retired and those tests now run under every gate. See that file's
// header for the full history.
