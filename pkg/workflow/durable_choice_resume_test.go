package workflow

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

// Phase 43 (M11) — INV-03 durability composition: choice/merge DAGs must compose
// with the M9 checkpoint + M10 suspend/timer/signal machinery WITHOUT breaking
// durability. Verification-first: the three INV-03 clauses hold by construction
// (see 43-CONTEXT); these gopter crash-resume properties PROVE it, each with a
// bite (a should-fail mutation that falsifies only it).

// resumeCountAction increments the shared execCounter and writes a deterministic
// output, so we can assert both run-count (exactly-once / never-ran) and output
// fidelity across a crash.
func resumeCountAction(c *execCounter, name string) Action {
	return ActionFunc(func(_ context.Context, data *WorkflowData) error {
		c.inc(name)
		data.SetOutput(name, "out-"+name)
		return nil
	})
}

// buildChoiceResumeDAG builds seed -> route(choice, k branches) -> each branch
// (entry -> _end) -> done(merge over the tails) -> after. The Choice reads the
// SEED key "route_key" (a guaranteed-run-ancestor key, FIRSTMATCH data-precondition)
// so routing is a pure function of checkpointed state. Branch `taken` is the one
// whose predicate matches (the last branch is the Otherwise default).
func buildChoiceResumeDAG(t *testing.T, id string, k int, c *execCounter) *DAG {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID(id)
	wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
	ch := wb.AddChoice("route").DependsOn("seed")
	tails := make([]string, 0, k)
	for j := 0; j < k; j++ {
		entry := fmt.Sprintf("b%d", j)
		end := entry + "_end"
		wb.AddNode(entry).WithAction(resumeCountAction(c, entry))
		wb.AddNode(end).DependsOn(entry).WithAction(resumeCountAction(c, end))
		tails = append(tails, end)
		if j < k-1 {
			jj := j
			ch.When(func(d *WorkflowData) bool { v, _ := d.GetInt("route_key"); return v == jj }, entry)
		} else {
			ch.Otherwise(entry) // default branch
		}
	}
	wb.AddMerge("done").From(tails...)
	wb.AddNode("after").DependsOn("done").WithAction(resumeCountAction(c, "after"))
	dag, err := wb.Build()
	require.NoError(t, err)
	return dag
}

// crashAfterLevels returns a checkpoint callback that persists levels 0..L-1 into
// store and then errors (a simulated crash) once more than L checkpoints have
// fired — mirroring durable_resume_test.go.
func crashAfterLevels(store *InMemoryStore, l int) func(*WorkflowData) error {
	n := 0
	return func(snap *WorkflowData) error {
		n++
		if n > l {
			return errors.New("simulated crash")
		}
		return store.SaveCheckpoint(snap)
	}
}

// branchNodes43 returns [entry, end] for branch j.
func branchNodes43(j int) []string {
	e := fmt.Sprintf("b%d", j)
	return []string{e, e + "_end"}
}

// TestINV03_a_SameBranchOnResume_Worked is the readable worked table test for
// clause (a): a 3-branch choice-merge, route_key=1 (branch 1 taken). Crash after
// the choice's level (its decision — the not-taken Bypassed statuses — is already
// persisted at the level-1 checkpoint); resume clean. The SAME branch stays taken,
// the not-taken branches stay Bypassed and their nodes NEVER run, the merge fires.
func TestINV03_a_SameBranchOnResume_Worked(t *testing.T) {
	store := NewInMemoryStore()
	const id, k, taken = "inv03a", 3, 1
	c := newExecCounter()

	// Phase 1: crash after level 2 (seed=0, route=1, entries=2 persisted).
	data1 := NewWorkflowData(id)
	data1.Set("route_key", taken)
	err := buildChoiceResumeDAG(t, id, k, c).Execute(
		withCheckpoint(context.Background(), crashAfterLevels(store, 2)), data1)
	require.Error(t, err, "the simulated crash surfaces as an error")

	// The choice's decision is durable: the not-taken entries persisted Bypassed.
	persisted, err := store.Load(id)
	require.NoError(t, err)
	assertNodeStatus(t, persisted, "route", Completed)
	for j := 0; j < k; j++ {
		if j == taken {
			continue
		}
		st, _ := persisted.GetNodeStatus(fmt.Sprintf("b%d", j))
		require.Equal(t, Bypassed, st, "not-taken entry b%d must persist Bypassed (the frozen decision)", j)
	}

	// Phase 2: resume clean.
	data2, err := store.Load(id)
	require.NoError(t, err)
	require.NoError(t, buildChoiceResumeDAG(t, id, k, c).Execute(
		withCheckpoint(context.Background(), func(s *WorkflowData) error { return store.SaveCheckpoint(s) }), data2))

	// Same branch taken; not-taken branches Bypassed with zero runs; merge fired.
	for j := 0; j < k; j++ {
		for _, n := range branchNodes43(j) {
			st, _ := data2.GetNodeStatus(n)
			if j == taken {
				assert.Equal(t, Completed, st, "taken node %s", n)
			} else {
				assert.Equal(t, Bypassed, st, "not-taken node %s stays Bypassed", n)
				assert.Equal(t, 0, c.get(n), "bypassed node %s never ran across crash+resume", n)
			}
		}
	}
	assertNodeStatus(t, data2, "done", Completed) // merge fired
	assertNodeStatus(t, data2, "after", Completed)
}

// TestINV03_a_SameBranchOnResume_Property is the gopter form: over random branch
// count and crash level, resume takes the same branch, bypassed nodes never run,
// no node runs more than twice (at-least-once, bounded), the merge fires.
func TestINV03_a_SameBranchOnResume_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 120
	props := gopter.NewProperties(params)

	props.Property("choice-merge resumes the same branch; bypassed never run", prop.ForAll(
		func(k, taken, crashLevel int) bool {
			if taken >= k {
				taken = k - 1
			}
			store := NewInMemoryStore()
			id := fmt.Sprintf("inv03a-%d-%d-%d", k, taken, crashLevel)
			c := newExecCounter()

			data1 := NewWorkflowData(id)
			data1.Set("route_key", taken)
			// Crash mid-run. If the run completes before the crash level, there is
			// nothing to resume — skip that (non-)case by re-running clean below.
			//nolint:errcheck // phase-1 crash: the error is the simulated crash, intentionally ignored
			_ = buildChoiceResumeDAG(t, id, k, c).Execute(
				withCheckpoint(context.Background(), crashAfterLevels(store, crashLevel)), data1)

			persisted, err := store.Load(id)
			if err != nil {
				return false
			}
			data2 := persisted
			if err := buildChoiceResumeDAG(t, id, k, c).Execute(
				withCheckpoint(context.Background(), func(s *WorkflowData) error { return store.SaveCheckpoint(s) }), data2); err != nil {
				return false
			}

			for j := 0; j < k; j++ {
				for _, n := range branchNodes43(j) {
					st, _ := data2.GetNodeStatus(n)
					if j == taken {
						if st != Completed {
							return false
						}
					} else {
						if st != Bypassed || c.get(n) != 0 {
							return false // a bypassed node ran, or flipped off Bypassed
						}
					}
				}
			}
			if st, _ := data2.GetNodeStatus("done"); st != Completed {
				return false // merge must fire
			}
			// Bounded: no node runs more than twice across crash+resume.
			for j := 0; j < k; j++ {
				for _, n := range branchNodes43(j) {
					if c.get(n) > 2 {
						return false
					}
				}
			}
			return true
		},
		gen.IntRange(2, 4), // k branches
		gen.IntRange(0, 3), // taken (clamped to k-1)
		gen.IntRange(1, 4), // crash after this many level-checkpoints
	))

	props.TestingRun(t)
}

// TestINV03_a_Bite proves the same-branch property has TEETH: if the Choice reads
// a MUTABLE key that changes between crash and resume (violating the FIRSTMATCH
// data-precondition), a crash BEFORE the Choice completes re-routes on resume to a
// DIFFERENT branch. The stable-seed-key version (above) does not — that is the
// invariant. Restore = read only guaranteed-ancestor/seed keys.
func TestINV03_a_Bite(t *testing.T) {
	build := func(id string, c *execCounter) *DAG {
		wb := NewWorkflowBuilder().WithWorkflowID(id)
		wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
		// BITE: the predicate reads "mut" — a key mutated between crash and resume.
		wb.AddChoice("route").DependsOn("seed").
			When(func(d *WorkflowData) bool { v, _ := d.GetInt("mut"); return v == 0 }, "a").
			Otherwise("b")
		wb.AddNode("a").WithAction(resumeCountAction(c, "a"))
		wb.AddNode("b").WithAction(resumeCountAction(c, "b"))
		wb.AddMerge("done").From("a", "b")
		dag, err := wb.Build()
		require.NoError(t, err)
		return dag
	}

	store := NewInMemoryStore()
	const id = "inv03a-bite"
	c := newExecCounter()

	// Phase 1: mut=0 (would route "a"), crash after level 0 (seed) — BEFORE the
	// Choice runs, so on resume the Choice re-evaluates.
	data1 := NewWorkflowData(id)
	data1.Set("mut", 0)
	_ = build(id, c).Execute(withCheckpoint(context.Background(), crashAfterLevels(store, 1)), data1) //nolint:errcheck // phase-1 crash, intentionally ignored

	persisted, err := store.Load(id)
	require.NoError(t, err)
	rst, _ := persisted.GetNodeStatus("route")
	require.NotEqual(t, Completed, rst, "the crash must land before the Choice completed (else the decision is frozen)")

	// Between crash and resume, the mutable key flips → resume routes "b" not "a".
	persisted.Set("mut", 1)
	require.NoError(t, build(id, c).Execute(
		withCheckpoint(context.Background(), func(s *WorkflowData) error { return store.SaveCheckpoint(s) }), persisted))

	// The bite: the branch flipped (a mutable-key predicate re-routes on resume).
	aSt, _ := persisted.GetNodeStatus("a")
	bSt, _ := persisted.GetNodeStatus("b")
	assert.Equal(t, Bypassed, aSt, "with a mutable predicate key, resume routed AWAY from a")
	assert.Equal(t, Completed, bSt, "resume took b instead — this is the bite (why the predicate must read stable keys)")
}
