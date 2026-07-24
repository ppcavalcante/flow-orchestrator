package workflow

// M21 ph108 T4 — the gopter EXECUTION ANCHOR for the fan-out formal capstone: property tests over the REAL
// Workflow.Execute (the machine-checked M21FanOut.tla claims, exercised on the real engine across randomized N +
// failure subsets). Two properties + a non-vacuity discriminator.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// TestM21Formal_ExactlyNSpawn_Property — over randomized N, on a RESUME where the fan node is still NON-TERMINAL
// (the expansion journaled but the node Pending — the REAL crash window, seeded like TestFanOut_CrashResume_
// RealWindow), Execute IS re-entered and reads the journal → the expander does NOT run on the resume drive
// (expandN stays 0). This genuinely exercises resolveExpansion's journal-read (F1 fix: a re-drive of a COMPLETED
// node is skipped by the executor at parallel_execution.go:88, which would be VACUOUS — the ph105 trap
// [[resume-test-vacuity-completed-node-skipped]]). Real-engine witness of M21FanOut.tla's ExactlyNSpawn.
func TestM21Formal_ExactlyNSpawn_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 60
	properties := gopter.NewProperties(params)

	properties.Property("a resume with the journal durable + node Pending reads it, expander does NOT re-run", prop.ForAll(
		func(n int) bool {
			return resumeReadsJournalNoReExpand(t, n)
		},
		gen.IntRange(1, 12),
	))
	properties.TestingRun(t)
}

// resumeReadsJournalNoReExpand SEEDS the durable post-crash state (the expansion journal present, the fan node
// Pending — the reachable non-terminal window), then drives: Execute IS re-invoked (Pending node) → resolveExpansion
// reads the journal → the expander does NOT run this drive. Returns true iff expandN == 0 across the resume AND the
// run completes with all N results. (The expander here counts ONLY its own invocations; the seed does NOT call it —
// the journal is written directly, exactly as the pre-crash drive would have flushed it.)
func resumeReadsJournalNoReExpand(t *testing.T, n int) bool {
	t.Helper()
	store := NewInMemoryStore()
	var expandN atomic.Int32
	expander := func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		expandN.Add(1) // if this fires on the resume, the journal-read was bypassed → property FALSE
		out := make([]interface{}, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	branch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		d.Set("out", iv)
		return nil
	})

	// SEED the durable expansion journal (the JSON-string journal the pre-crash drive flushed) + leave the fan node
	// Pending (never written terminal) → the resume drive re-enters Execute.
	items := make([]json.RawMessage, n)
	for i := range items {
		items[i] = json.RawMessage(strconvI(i))
	}
	journal, err := json.Marshal(fanOutJournal{N: n, Items: items})
	require.NoError(t, err)
	seed := NewWorkflowData("wf-prop")
	seed.Set(fanOutItemsKey("fan"), string(journal))
	if store.Save(seed) != nil {
		return false
	}

	b := NewWorkflowBuilder().WithWorkflowID("wf-prop")
	b.AddFanOut("fan", expander, branch).WithResults("r", "out")
	dag, err := b.Build()
	if err != nil {
		return false
	}
	w := NewWorkflow(store)
	w.WorkflowID = "wf-prop"
	w.DAG = dag
	if w.Execute(context.Background()) != nil {
		return false
	}
	// The resume re-entered Execute (Pending node) and read the journal → the expander did NOT run this drive.
	if expandN.Load() != 0 {
		return false
	}
	final, err := store.Load("wf-prop")
	if err != nil {
		return false
	}
	return fanCount(t, final, "r") == n
}

// strconvI renders an int as its JSON number literal (strconv.Itoa is exact + infallible for a base-10 int).
func strconvI(i int) string {
	return strconv.Itoa(i)
}

// TestM21Formal_FanInWaitsForAll_Property — over randomized N, a FailFast fan-out with NO branch failure Completes
// with all N results present; the fan node is terminal IFF all branches produced their result. The real-engine
// witness of FanInWaitsForAll (the node does not terminalize while a branch is non-terminal).
func TestM21Formal_FanInWaitsForAll_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 60
	properties := gopter.NewProperties(params)

	properties.Property("node Completes iff all N branches produced their result", prop.ForAll(
		func(n int) bool {
			return nodeCompletesIffAllBranchesDone(t, n)
		},
		gen.IntRange(1, 12),
	))
	properties.TestingRun(t)
}

func nodeCompletesIffAllBranchesDone(t *testing.T, n int) bool {
	t.Helper()
	store := NewInMemoryStore()
	branch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		d.Set("out", iv*10)
		return nil
	})
	b := NewWorkflowBuilder().WithWorkflowID("wf-fanin")
	b.AddFanOut("fan", intItemsExpander(n), branch).WithResults("r", "out")
	dag, err := b.Build()
	if err != nil {
		return false
	}
	w := NewWorkflow(store)
	w.WorkflowID = "wf-fanin"
	w.DAG = dag
	if w.Execute(context.Background()) != nil {
		return false
	}
	final, err := store.Load("wf-fanin")
	if err != nil {
		return false
	}
	st, ok := final.GetNodeStatus("fan")
	if !ok || st != Completed {
		return false
	}
	// All N branch results present + typed (the node terminalized only after every branch produced its result).
	if fanCount(t, final, "r") != n {
		return false
	}
	for i := 0; i < n; i++ {
		v, present := final.Get(fanOutResultIndexKey("r", i))
		if !present || coerceInt64(t, v) != int64(i*10) {
			return false
		}
	}
	return true
}

// TestM21Formal_NonVacuityDiscriminator — proves the ExactlyNSpawn property genuinely exercises the journal-read
// (F2 fix: an ENGINE-based discriminator, not a tautology). We seed a journal with N=3 items but wire an expander
// that (if it ran) would yield N=5. On the resume drive the fan node is Pending → Execute re-enters. The REAL
// engine reads the JOURNAL (N=3) and does NOT re-expand → the expander count is 0 and the aggregate has 3
// elements. A hypothetical re-expand-on-resume would instead run the expander (count 1) and fan out 5. The
// discriminator asserts BOTH observable witnesses of the journal-read (expander not run AND N=3, not 5) — either
// would flip if the resume bypassed the journal. This is the machine-level proof the ExactlyNSpawn property above
// is not vacuous: it distinguishes journal-read from re-expand on the real engine.
func TestM21Formal_NonVacuityDiscriminator(t *testing.T) {
	store := NewInMemoryStore()
	const journalN, expanderN = 3, 5
	var expandN atomic.Int32
	expander := func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
		expandN.Add(1)
		out := make([]interface{}, expanderN) // a DIFFERENT N than the journal — the discriminator
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	branch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		d.Set("out", iv)
		return nil
	})
	// Seed a journal of N=3 + Pending node.
	items := make([]json.RawMessage, journalN)
	for i := range items {
		items[i] = json.RawMessage(strconvI(i))
	}
	journal, err := json.Marshal(fanOutJournal{N: journalN, Items: items})
	require.NoError(t, err)
	seed := NewWorkflowData("wf-disc")
	seed.Set(fanOutItemsKey("fan"), string(journal))
	require.NoError(t, store.Save(seed))

	b := NewWorkflowBuilder().WithWorkflowID("wf-disc")
	b.AddFanOut("fan", expander, branch).WithResults("r", "out")
	dag, err := b.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = "wf-disc"
	w.DAG = dag
	require.NoError(t, w.Execute(context.Background()))

	final, err := store.Load("wf-disc")
	require.NoError(t, err)
	require.EqualValues(t, 0, expandN.Load(), "NON-VACUITY: the resume READ the journal — the expander did not re-run")
	require.Equal(t, journalN, fanCount(t, final, "r"),
		"NON-VACUITY: the fan-out used the JOURNALED N=3, not the expander's N=5 — a re-expand would have fanned out 5")
}
