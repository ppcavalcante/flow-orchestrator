package workflow

// M9 chunk 2 — durable checkpoint + resume CORE.
//
// These are the load-bearing tests for crash-resume: a process that dies mid-run
// and restarts must resume from the last completed level barrier, skipping every
// already-completed node and rehydrating its output, while the not-done frontier
// re-runs. The crash is modelled faithfully: a checkpoint that persists levels
// 0..L-1 and then fails at level L (a crash caught work in flight), after which
// resume loads ONLY what was persisted.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execCounter tracks how many times each node's action runs, across resume.
type execCounter struct {
	mu    sync.Mutex
	count map[string]int
}

func newExecCounter() *execCounter { return &execCounter{count: map[string]int{}} }

func (c *execCounter) inc(name string) {
	c.mu.Lock()
	c.count[name]++
	c.mu.Unlock()
}

func (c *execCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[name]
}

// countingNode builds a node whose action increments the counter and writes a
// deterministic output (so we can assert output fidelity survives resume).
func countingNode(name string, c *execCounter) *Node {
	return NewNode(name, ActionFunc(func(_ context.Context, data *WorkflowData) error {
		c.inc(name)
		data.SetOutput(name, "out-"+name)
		return nil
	}))
}

// TestDurableResume_CrashResumeSkipsCompleted is the headline crash-resume test.
// A 4-level chain a→b→c→d runs under an InMemoryStore (a Checkpointer). We crash
// at the checkpoint AFTER level 1 (node b): levels a,b are persisted; c,d are not.
// On resume: a and b must NOT re-execute (skipped — persisted Completed); c and d
// run; the final state equals a clean run's.
func TestDurableResume_CrashResumeSkipsCompleted(t *testing.T) {
	store := NewInMemoryStore()
	const id = "crash-resume"
	counter := newExecCounter()

	buildDAG := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, countingNode("a", counter))
		mustAddNode(t, d, countingNode("b", counter))
		mustAddNode(t, d, countingNode("c", counter))
		mustAddNode(t, d, countingNode("d", counter))
		mustAddDep(t, d, "a", "b")
		mustAddDep(t, d, "b", "c")
		mustAddDep(t, d, "c", "d")
		return d
	}

	// --- Phase 1: run until a "crash" at the checkpoint after level 1 (node b). ---
	dag := buildDAG()
	levelsCheckpointed := 0
	dag.config.checkpoint = func(snap *WorkflowData) error {
		levelsCheckpointed++
		if levelsCheckpointed > 2 {
			// Levels 0 (a) and 1 (b) have been persisted; the crash hits before
			// level 2 (c)'s checkpoint would persist. Simulate process death.
			return errors.New("simulated crash")
		}
		return store.SaveCheckpoint(snap)
	}
	data1 := NewWorkflowData(id)
	err := dag.Execute(context.Background(), data1)
	require.Error(t, err, "the simulated crash must surface as an error")

	// Precondition: the store persisted exactly a and b as Completed.
	persisted, loadErr := store.Load(id)
	require.NoError(t, loadErr)
	for _, n := range []string{"a", "b"} {
		st, _ := persisted.GetNodeStatus(n)
		require.Equal(t, Completed, st, "node %s must be persisted Completed before the crash", n)
	}

	// --- Phase 2: resume from the persisted state with a clean checkpointer. ---
	resumeDAG := buildDAG()
	resumeDAG.config.checkpoint = func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }
	data2, loadErr := store.Load(id)
	require.NoError(t, loadErr)
	require.NoError(t, resumeDAG.Execute(context.Background(), data2))

	// a and b were persisted Completed → must NOT re-execute (the memoization
	// guarantee). Each ran exactly once, in phase 1.
	assert.Equal(t, 1, counter.get("a"), "completed node a must not re-execute on resume")
	assert.Equal(t, 1, counter.get("b"), "completed node b must not re-execute on resume")
	// c was the node IN FLIGHT at the crash: its level completed (so it ran in
	// phase 1) but the crash hit before its checkpoint persisted, so resume re-runs
	// it → executed TWICE. This is the documented at-least-once-on-in-flight
	// contract (the crash caught work that had run but was not yet durable).
	assert.Equal(t, 2, counter.get("c"), "in-flight node c re-runs on resume (at-least-once)")
	// d never ran in phase 1 (the crash aborted before its level) → runs once on resume.
	assert.Equal(t, 1, counter.get("d"), "frontier node d runs once on resume")

	// Final state: every node Completed with its output rehydrated/produced.
	for _, n := range []string{"a", "b", "c", "d"} {
		st, _ := data2.GetNodeStatus(n)
		assert.Equal(t, Completed, st, "node %s final status", n)
		out, ok := data2.GetOutput(n)
		require.True(t, ok, "node %s output must be present after resume", n)
		assert.Equal(t, "out-"+n, out, "node %s output fidelity across resume", n)
	}
}

// TestDurableResume_NoCheckpointerNoBehaviorChange: a Store that does NOT
// implement Checkpointer gets no mid-run checkpointing and behaves exactly as
// before (the callback stays nil — zero overhead). We use a minimal non-
// checkpointer store and assert a normal run completes with no checkpoint wiring.
type nonCheckpointerStore struct{ inner *InMemoryStore }

func (s *nonCheckpointerStore) Save(d *WorkflowData) error            { return s.inner.Save(d) }
func (s *nonCheckpointerStore) Load(id string) (*WorkflowData, error) { return s.inner.Load(id) }
func (s *nonCheckpointerStore) ListWorkflows() ([]string, error)      { return s.inner.ListWorkflows() }
func (s *nonCheckpointerStore) Delete(id string) error                { return s.inner.Delete(id) }

func TestDurableResume_NoCheckpointerNoBehaviorChange(t *testing.T) {
	// Compile-time + runtime proof it is NOT a Checkpointer.
	var st WorkflowStore = &nonCheckpointerStore{inner: NewInMemoryStore()}
	_, isCP := st.(Checkpointer)
	require.False(t, isCP, "the test store must not implement Checkpointer")

	counter := newExecCounter()
	wf := NewWorkflow(st)
	wf.WorkflowID = "no-cp"
	mustAddNode(t, wf.DAG, countingNode("a", counter))
	mustAddNode(t, wf.DAG, countingNode("b", counter))
	mustAddDep(t, wf.DAG, "a", "b")

	require.NoError(t, wf.Execute(context.Background()))
	assert.Equal(t, 1, counter.get("a"))
	assert.Equal(t, 1, counter.get("b"))
	// The DAG must carry no checkpoint callback after a run with a non-checkpointer.
	assert.Nil(t, wf.DAG.config.checkpoint, "no checkpoint callback may be wired for a non-Checkpointer store")
}

// TestDurableResume_GraphIdentityGuard: resuming against a DAG that no longer
// contains a persisted node must be rejected (fail loudly), not silently
// mis-resumed.
func TestDurableResume_GraphIdentityGuard(t *testing.T) {
	store := NewInMemoryStore()
	const id = "graph-guard"

	// Persist a state that references node "ghost" (as if an earlier graph had it).
	seed := NewWorkflowData(id)
	seed.SetNodeStatus("ghost", Completed)
	seed.SetNodeStatus("a", Completed)
	require.NoError(t, store.Save(seed))

	// Current DAG has only "a" — "ghost" is gone.
	wf := NewWorkflow(store)
	wf.WorkflowID = id
	mustAddNode(t, wf.DAG, NewNode("a", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))

	err := wf.Execute(context.Background())
	require.Error(t, err, "resume against a changed graph must be rejected")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), "ghost", "the error must name the missing node")
}

// TestDurableResume_CheckpointFailureAbortsRun: a checkpoint write error aborts
// the run (durability failure surfaced, not silently dropped).
func TestDurableResume_CheckpointFailureAbortsRun(t *testing.T) {
	dag := NewDAG("cp-fail")
	mustAddNode(t, dag, NewNode("a", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	dag.config.checkpoint = func(*WorkflowData) error { return errors.New("disk full") }

	err := dag.Execute(context.Background(), NewWorkflowData("cp-fail"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint failed")
	assert.Contains(t, err.Error(), "disk full")
}

// --- gopter crash-resume PROPERTY (headline verification) -------------------

// TestDurableResume_Property: over random acyclic DAGs, inject a crash at a
// random level boundary and resume; assert (1) the resumed final state (per-node
// status + outputs) equals a no-crash run's, and (2) NO node that was persisted
// Completed before the crash is ever re-executed on resume (the memoization
// guarantee).
//
// Mutation-bite (verified manually, see chunk-2 SUMMARY):
//   - disable the resume skip-terminal guard (executor re-runs completed nodes)
//     → assertion (2) fails (a persisted-Completed node re-executes);
//   - drop output rehydration / journal fidelity → assertion (1) fails
//     (resumed outputs differ from the no-crash run).
func TestDurableResume_Property(t *testing.T) {
	params := gopter.DefaultTestParametersWithSeed(0xD0DECA11)
	params.MinSuccessfulTests = 200
	params.MaxShrinkCount = 50
	properties := gopter.NewProperties(params)

	properties.Property("crash at any level ⇒ resume converges, no Completed node re-runs", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			spec := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)

			// --- Reference: a clean no-crash run. ---
			refCounter := newExecCounter()
			refDAG, ok := spec.buildDAG(t, false, func(i int) func(context.Context, *WorkflowData) error {
				name := invNodeName(i)
				return func(_ context.Context, data *WorkflowData) error {
					refCounter.inc(name)
					data.SetOutput(name, "v-"+name)
					return nil
				}
			})
			if !ok {
				return false
			}
			refData := NewWorkflowData("ref")
			if err := refDAG.Execute(context.Background(), refData); err != nil {
				return false // generated DAGs here never fail (no failing actions)
			}

			// --- Crash run: persist levels 0..L-1, crash at level L. ---
			store := NewInMemoryStore()
			const id = "prop"
			crashCounter := newExecCounter()
			mkAction := func(i int) func(context.Context, *WorkflowData) error {
				name := invNodeName(i)
				return func(_ context.Context, data *WorkflowData) error {
					crashCounter.inc(name)
					data.SetOutput(name, "v-"+name)
					return nil
				}
			}
			crashDAG, ok := spec.buildDAG(t, false, mkAction)
			if !ok {
				return false
			}
			numLevels := len(crashDAG.GetLevels())
			// Choose a crash level in [1, numLevels] (deterministic from seed). A
			// crash "at level L" persists checkpoints for levels 0..L-1.
			crashAtLevel := 1 + int(uint64(seed)%uint64(max(1, numLevels)))
			cpSeen := 0
			crashDAG.config.checkpoint = func(snap *WorkflowData) error {
				cpSeen++
				if cpSeen >= crashAtLevel {
					return errors.New("crash")
				}
				return store.SaveCheckpoint(snap)
			}
			data1 := NewWorkflowData(id)
			crashDAG.Execute(context.Background(), data1) //nolint:errcheck // may crash (injected) or complete; outcome read via the store below

			// Capture which nodes were persisted Completed before the crash.
			persisted, err := store.Load(id)
			persistedCompleted := map[string]bool{}
			if err == nil {
				persisted.ForEachNodeStatus(func(name string, st NodeStatus) {
					if st == Completed {
						persistedCompleted[name] = true
					}
				})
			}

			// --- Resume from the persisted state; count executions during resume only. ---
			resumeCounter := newExecCounter()
			resumeDAG, ok := spec.buildDAG(t, false, func(i int) func(context.Context, *WorkflowData) error {
				name := invNodeName(i)
				return func(_ context.Context, data *WorkflowData) error {
					resumeCounter.inc(name)
					data.SetOutput(name, "v-"+name)
					return nil
				}
			})
			if !ok {
				return false
			}
			var data2 *WorkflowData
			if err == nil && persisted != nil {
				data2 = persisted
			} else {
				data2 = NewWorkflowData(id)
			}
			resumeDAG.config.checkpoint = func(snap *WorkflowData) error { return store.SaveCheckpoint(snap) }
			if err := resumeDAG.Execute(context.Background(), data2); err != nil {
				return false
			}

			// (2) No persisted-Completed node may re-execute on resume.
			for name := range persistedCompleted {
				if resumeCounter.get(name) != 0 {
					return false
				}
			}

			// (1) Final state (status + output) equals the no-crash run for every node.
			for i := 0; i < spec.n; i++ {
				name := invNodeName(i)
				rst, _ := refData.GetNodeStatus(name)
				cst, _ := data2.GetNodeStatus(name)
				if rst != cst {
					return false
				}
				rout, rok := refData.GetOutput(name)
				cout, cok := data2.GetOutput(name)
				if rok != cok || fmt.Sprint(rout) != fmt.Sprint(cout) {
					return false
				}
			}
			return true
		},
		dagSpecGens()...,
	))

	properties.TestingRun(t)
}
