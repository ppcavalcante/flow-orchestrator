package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// M12 ph47 — HONEST FINAL STATE adversarial suite (partition exactness under
// FAILURE INJECTION). Independent complement to saga_honest_state_test.go, which
// exercises ONE hand-picked diamond. This file attacks the SagaError partition
// invariant with RANDOM failure subsets over random shapes, concurrency, the
// best-effort guarantee, boundaries, deadline races, retry exactness, and the
// M11 non-Completed-status interaction.
//
// THE HARD BAR (the invariant every test below targets):
//   SagaError.{Compensated ⊎ FailedToCompensate ⊎ Skipped} is an EXACT partition
//   of the run's Completed nodes — union == Completed, pairwise disjoint, no node
//   missing, double-counted, or mislabeled — AND best-effort: an early comp
//   failure must NEVER suppress a later comp attempt.
//
// ORACLES: (1) the partition invariant, keyed off the per-node behavior the test
// ASSIGNS (not a hand-picked shape); (2) totality — every Completed node lands in
// exactly one set; (3) the minimum bar — no panic / hang / race.
// ============================================================================

// ---- small set helpers (uniquely named to avoid collision with sibling tests) --

func partSetOf(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func partNamesOf(nes []NodeError) map[string]bool {
	m := make(map[string]bool, len(nes))
	for _, ne := range nes {
		m[ne.NodeName] = true
	}
	return m
}

func partKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func partSetEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------
// ANGLE 1 (CORE) — PARTITION EXACTNESS under RANDOM failure injection.
//
// Generate random layered saga-DAGs (chains / diamonds / wide fans / deep). Each
// Completed node is randomly assigned one of {comp-succeeds, comp-fails, no-comp}.
// For EVERY such failure subset the returned *SagaError's partition must:
//   - label each node exactly as its assignment says (no mislabel),
//   - union to the full Completed set, pairwise disjoint (totality),
//   - run every declared compensation (best-effort — no early failure suppresses
//     a later attempt),
//   - persist each node's terminal status consistent with the partition.
// Bite: drop/double-count a node, or abort-on-first-failure → RED (verified out of
// band by mutating saga_rollback.go).
// ----------------------------------------------------------------------------

type injectedSaga struct {
	w       *Workflow
	expComp map[string]bool // declared comp, succeeds  -> Compensated
	expFail map[string]bool // declared comp, fails      -> CompensationFailed
	expSkip map[string]bool // Completed, no comp         -> Skipped (status stays Completed)

	ranMu sync.Mutex
	ran   map[string]int // per-node compensation invocation count
}

// buildFailInjectedSaga deterministically builds a random layered saga from a seed:
// `layers` layers each of random width in [1,maxWidth]; every node in layer L>0
// depends on a random NON-EMPTY subset of layer L-1. Each node is randomly assigned
// a compensation behavior; the shallowest node (L0_0) is FORCED to declare a failing
// compensation so the run always produces a *SagaError (≥1 comp fails). A terminal
// "fail" node depends on the whole last layer and hard-fails to trigger rollback.
func buildFailInjectedSaga(seed int64, layers, maxWidth int) *injectedSaga {
	rng := rand.New(rand.NewSource(seed))
	b := NewWorkflowBuilder().WithWorkflowID("inject")
	is := &injectedSaga{
		expComp: map[string]bool{},
		expFail: map[string]bool{},
		expSkip: map[string]bool{},
		ran:     map[string]int{},
	}
	record := func(name string) {
		is.ranMu.Lock()
		is.ran[name]++
		is.ranMu.Unlock()
	}
	mkComp := func(name string, fail bool) func(context.Context, *WorkflowData) error {
		return func(context.Context, *WorkflowData) error {
			record(name)
			if fail {
				return errors.New("comp-fail:" + name)
			}
			return nil
		}
	}

	prev := []string{}
	forcedFail := true
	for l := 0; l < layers; l++ {
		width := 1 + rng.Intn(maxWidth)
		cur := make([]string, width)
		for i := 0; i < width; i++ {
			name := fmt.Sprintf("L%d_%d", l, i)
			cur[i] = name
			nb := b.AddNode(name).WithAction(benchNoopAction())

			beh := rng.Intn(3) // 0 succeed, 1 fail, 2 no-comp
			if forcedFail {
				beh = 1 // guarantee at least one failing comp -> always a *SagaError
				forcedFail = false
			}
			switch beh {
			case 0:
				nb.WithCompensation(mkComp(name, false))
				is.expComp[name] = true
			case 1:
				nb.WithCompensation(mkComp(name, true))
				is.expFail[name] = true
			default:
				is.expSkip[name] = true
			}

			if l > 0 {
				var chosen []string
				for _, p := range prev {
					if rng.Intn(2) == 0 {
						chosen = append(chosen, p)
					}
				}
				if len(chosen) == 0 {
					chosen = []string{prev[rng.Intn(len(prev))]}
				}
				nb.DependsOn(chosen...)
			}
		}
		prev = cur
	}
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn(prev...)

	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	// vary concurrency by seed to exercise the mutex-guarded collection alongside it.
	dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 1 + int(seed%8)})
	is.w = &Workflow{DAG: dag, WorkflowID: "inject", Store: NewInMemoryStore()}
	return is
}

func TestSagaHonest_PartitionExact_FailureInjection_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 300
	props := gopter.NewProperties(params)

	props.Property("SagaError partitions the Completed set exactly under any failure subset", prop.ForAll(
		func(seed int64, layers, maxWidth int) bool {
			is := buildFailInjectedSaga(seed, 2+layers, 1+maxWidth)
			err := is.w.Execute(context.Background())

			var se *SagaError
			if !errors.As(err, &se) {
				return false // ≥1 comp fails by construction -> MUST be a *SagaError
			}

			gotComp := partSetOf(se.Compensated)
			gotFail := partNamesOf(se.FailedToCompensate)
			gotSkip := partSetOf(se.Skipped)

			// (1) membership matches the assigned behavior EXACTLY (no mislabel).
			if !partSetEqual(gotComp, is.expComp) {
				return false
			}
			if !partSetEqual(gotFail, is.expFail) {
				return false
			}
			if !partSetEqual(gotSkip, is.expSkip) {
				return false
			}

			// (2) totality: union == the Completed set (all non-"fail" nodes),
			//     pairwise disjoint — no node missing, phantom, or double-counted.
			completed := map[string]bool{}
			for _, n := range is.w.DAG.Nodes {
				if n.Name != "fail" {
					completed[n.Name] = true
				}
			}
			union := map[string]int{}
			for n := range gotComp {
				union[n]++
			}
			for n := range gotFail {
				union[n]++
			}
			for n := range gotSkip {
				union[n]++
			}
			if len(union) != len(completed) {
				return false
			}
			for n, c := range union {
				if c != 1 || !completed[n] {
					return false
				}
			}
			for n := range completed {
				if union[n] != 1 {
					return false
				}
			}

			// (3) best-effort: EVERY node that declared a compensation actually RAN it
			//     — an early (deep-level) failure did not suppress a later attempt.
			is.ranMu.Lock()
			for n := range is.expComp {
				if is.ran[n] == 0 {
					is.ranMu.Unlock()
					return false
				}
			}
			for n := range is.expFail {
				if is.ran[n] == 0 {
					is.ranMu.Unlock()
					return false
				}
			}
			is.ranMu.Unlock()

			// (4) durable: each node's persisted terminal status matches the partition.
			got, lerr := is.w.Store.Load("inject")
			if lerr != nil {
				return false
			}
			for n := range is.expComp {
				if st, _ := got.GetNodeStatus(n); st != Compensated {
					return false
				}
			}
			for n := range is.expFail {
				if st, _ := got.GetNodeStatus(n); st != CompensationFailed {
					return false
				}
			}
			for n := range is.expSkip {
				if st, _ := got.GetNodeStatus(n); st != Completed {
					return false
				}
			}
			return true
		},
		gen.Int64Range(1, 1<<31),
		gen.IntRange(0, 4), // => 2..6 layers
		gen.IntRange(0, 4), // => width 1..5
	))

	props.TestingRun(t)
}

// ----------------------------------------------------------------------------
// ANGLE 2 — CONCURRENCY: a WIDE level with MANY comps failing/succeeding/skipping
// CONCURRENTLY, at maxConc ∈ {1, some, N}, under `go test -race`. The sagaOutcome
// collection (mutex-guarded) + concurrent SetNodeStatus must produce an EXACT
// partition every iteration with no race.
// ----------------------------------------------------------------------------

func TestSagaHonest_WideMixed_ConcurrentPartition_RaceClean(t *testing.T) {
	const (
		nodes = 96
		iters = 100
	)
	for _, maxConc := range []int{1, 8, nodes} {
		maxConc := maxConc
		t.Run(fmt.Sprintf("maxConc=%d", maxConc), func(t *testing.T) {
			t.Parallel()
			for it := 0; it < iters; it++ {
				level := make([]*Node, nodes)
				expComp := map[string]bool{}
				expFail := map[string]bool{}
				expSkip := map[string]bool{}
				for i := 0; i < nodes; i++ {
					name := fmt.Sprintf("w%d", i)
					nd := NewNode(name, benchNoopAction())
					switch i % 3 {
					case 0: // failing comp
						nd.Compensation = ActionFunc(func(context.Context, *WorkflowData) error {
							return errors.New("boom")
						})
						expFail[name] = true
					case 1: // succeeding comp
						nd.Compensation = ActionFunc(func(context.Context, *WorkflowData) error { return nil })
						expComp[name] = true
					default: // no comp -> skipped
						expSkip[name] = true
					}
					level[i] = nd
				}
				data := freshCompletedData("wide", level)
				out := &sagaOutcome{}

				compensateLevel(context.Background(), level, data, maxConc, out)

				// exact partition — the three sets equal the assignment, disjoint, total.
				require.ElementsMatchf(t, partKeysOf(expComp), out.compensated, "iter %d: compensated set", it)
				require.ElementsMatchf(t, partKeysOf(expFail), failedNames(out.failedToCompensate), "iter %d: failed set", it)
				require.ElementsMatchf(t, partKeysOf(expSkip), out.skipped, "iter %d: skipped set", it)
				require.Equalf(t, nodes, len(out.compensated)+len(out.failedToCompensate)+len(out.skipped),
					"iter %d: union covers every node exactly once", it)

				// terminal statuses match the partition.
				for i := 0; i < nodes; i++ {
					name := fmt.Sprintf("w%d", i)
					st, _ := data.GetNodeStatus(name)
					switch i % 3 {
					case 0:
						require.Equalf(t, CompensationFailed, st, "iter %d node %s", it, name)
					case 1:
						require.Equalf(t, Compensated, st, "iter %d node %s", it, name)
					default:
						require.Equalf(t, Completed, st, "iter %d node %s", it, name)
					}
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ANGLE 3 — BEST-EFFORT completeness across depth: the DEEPEST compensation (which
// runs FIRST in the reverse pass) FAILS; every shallower compensation must still
// run. Oracle: count comps run == depth (every node attempted), deep = failed.
// Bite: abort-on-first-failure -> fewer than `depth` comps ran -> RED.
// ----------------------------------------------------------------------------

func TestSagaHonest_BestEffort_DeepFailureRunsAllShallow(t *testing.T) {
	for _, depth := range []int{2, 5, 20} {
		depth := depth
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			rec := &compRecorder{}
			store := NewInMemoryStore()
			b := NewWorkflowBuilder().WithWorkflowID("besteffort")
			prev := ""
			for i := 0; i < depth; i++ {
				name := fmt.Sprintf("n%d", i)
				fail := i == depth-1 // deepest compensable node fails (compensates first)
				comp := func(context.Context, *WorkflowData) error {
					rec.record(name)
					if fail {
						return errors.New("deep comp boom")
					}
					return nil
				}
				nb := b.AddNode(name).WithAction(benchNoopAction()).WithCompensation(comp)
				if prev != "" {
					nb.DependsOn(prev)
				}
				prev = name
			}
			b.AddNode("fail").
				WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
				DependsOn(prev)
			dag, err := b.Build()
			require.NoError(t, err)
			w := &Workflow{DAG: dag, WorkflowID: "besteffort", Store: store}

			err = w.Execute(context.Background())
			var se *SagaError
			require.ErrorAs(t, err, &se, "the deep comp failed -> partial rollback -> *SagaError")

			// best-effort: EVERY one of the `depth` compensations ran despite the deepest
			// failing first — a failure never aborted the wave.
			require.Lenf(t, rec.snapshot(), depth, "every compensation must run (best-effort) at depth %d", depth)

			// exact partition: the deepest is CompensationFailed, the rest Compensated.
			require.Equal(t, []string{fmt.Sprintf("n%d", depth-1)}, failedNames(se.FailedToCompensate))
			require.Len(t, se.Compensated, depth-1)
			require.Empty(t, se.Skipped)

			got, lerr := store.Load("besteffort")
			require.NoError(t, lerr)
			assertStatus(t, got, fmt.Sprintf("n%d", depth-1), CompensationFailed)
			for i := 0; i < depth-1; i++ {
				assertStatus(t, got, fmt.Sprintf("n%d", i), Compensated)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ANGLE 4 — NEVER-FALSE-CLEAN boundaries: all-fail, zero-completed.
// ----------------------------------------------------------------------------

// TestSagaHonest_Boundary_AllFail — every Completed compensable node's comp FAILS.
// The *SagaError must have EMPTY Compensated, EMPTY Skipped, and ALL nodes in
// FailedToCompensate (the partition degenerates to one set, still exact).
func TestSagaHonest_Boundary_AllFail(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("all-fail")
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(failComp(rec, "a"))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(failComp(rec, "b")).DependsOn("a")
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("b")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "all-fail", Store: store}

	err = w.Execute(context.Background())
	var se *SagaError
	require.ErrorAs(t, err, &se)
	require.Empty(t, se.Compensated, "no comp succeeded")
	require.Empty(t, se.Skipped, "every Completed node declared a comp")
	require.Equal(t, []string{"a", "b"}, failedNames(se.FailedToCompensate), "all in FailedToCompensate")
	// best-effort: both were attempted despite both failing.
	require.ElementsMatch(t, []string{"a", "b"}, rec.snapshot())
	// still unwraps to the original trigger cause.
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "the *SagaError still unwraps to the *ExecutionError cause")
	got, lerr := store.Load("all-fail")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", CompensationFailed)
	assertStatus(t, got, "b", CompensationFailed)
}

// TestSagaHonest_Boundary_ZeroCompleted — a run whose FIRST node fails (nothing ever
// reaches Completed) but which DECLARES a compensation. The trigger fires
// (hasCompensations true, marker persisted), but the partition is EMPTY, so
// finishRollback must return the BARE *ExecutionError — never a false *SagaError —
// and no compensation runs.
func TestSagaHonest_Boundary_ZeroCompleted(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("zero-completed")
	// 'a' fails immediately AND declares a compensation (so hasCompensations is true),
	// but 'a' is Failed, never Completed -> not in the partition.
	b.AddNode("a").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil })
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "zero-completed", Store: store}

	err = w.Execute(context.Background())
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*SagaError), "zero Completed nodes -> NO false *SagaError")
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "the bare trigger *ExecutionError surfaces")
	require.Empty(t, rec.snapshot(), "nothing Completed -> no compensation runs")

	got, lerr := store.Load("zero-completed")
	require.NoError(t, lerr)
	require.True(t, got.IsRollingBack(), "the marker was set before the (empty) compensation pass")
	assertStatus(t, got, "a", Failed)
}

// ----------------------------------------------------------------------------
// ANGLE 5 — DEADLINE (ph46-F2) with a MIX: a fast comp that finishes before the
// deadline (deeper level, compensates first) -> Compensated; a comp that BLOCKS
// forever (shallower level, compensates after) -> CompensationFailed when the
// scoped deadline fires. The rollback must COMPLETE (never hang) and the partition
// must stay exact across the deadline boundary.
// ----------------------------------------------------------------------------

func TestSagaHonest_Deadline_MixedFinishAndBlock(t *testing.T) {
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("deadline-mix")
	// shallow 'root' blocks until the deadline (compensates LAST in the reverse pass).
	b.AddNode("root").WithAction(benchNoopAction()).
		WithCompensation(func(ctx context.Context, _ *WorkflowData) error {
			<-ctx.Done()
			return ctx.Err()
		})
	// deep 'child' compensates FIRST and returns immediately (well before the deadline).
	b.AddNode("child").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { return nil }).
		DependsOn("root")
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("child")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "deadline-mix", Store: store}
	w.WithRollbackTimeout(200 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case err := <-done:
		var se *SagaError
		require.ErrorAs(t, err, &se, "a blocked comp past the deadline -> *SagaError")
		require.Equal(t, []string{"child"}, se.Compensated, "the fast comp finished before the deadline")
		require.Equal(t, []string{"root"}, failedNames(se.FailedToCompensate), "the blocker is CompensationFailed")
		require.Empty(t, se.Skipped)
	case <-time.After(5 * time.Second):
		t.Fatal("rollback hung past its scoped deadline — a blocking compensation hung the run")
	}
	got, lerr := store.Load("deadline-mix")
	require.NoError(t, lerr)
	assertStatus(t, got, "child", Compensated)
	assertStatus(t, got, "root", CompensationFailed)
}

// ----------------------------------------------------------------------------
// ANGLE 6 — RETRYCOUNT exactness: an always-failing compensation is attempted
// EXACTLY RetryCount+1 times, then recorded CompensationFailed (DEC-M12-RETRY).
// ----------------------------------------------------------------------------

func TestSagaHonest_RetryCount_ExhaustExactAttempts(t *testing.T) {
	for _, rc := range []int{0, 1, 3, 5} {
		rc := rc
		t.Run(fmt.Sprintf("retries=%d", rc), func(t *testing.T) {
			var attempts int64
			store := NewInMemoryStore()
			b := NewWorkflowBuilder().WithWorkflowID("retry-exhaust")
			b.AddNode("a").WithAction(benchNoopAction()).
				WithRetries(rc).
				WithCompensation(func(context.Context, *WorkflowData) error {
					atomic.AddInt64(&attempts, 1)
					return errors.New("always fails")
				})
			b.AddNode("fail").
				WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
				DependsOn("a")
			dag, err := b.Build()
			require.NoError(t, err)
			w := &Workflow{DAG: dag, WorkflowID: "retry-exhaust", Store: store}

			err = w.Execute(context.Background())
			var se *SagaError
			require.ErrorAs(t, err, &se, "an exhausted comp -> *SagaError")
			require.Equalf(t, int64(rc+1), atomic.LoadInt64(&attempts),
				"exactly RetryCount+1 (%d) compensation attempts on exhaustion", rc+1)
			require.Equal(t, []string{"a"}, failedNames(se.FailedToCompensate))
			got, lerr := store.Load("retry-exhaust")
			require.NoError(t, lerr)
			assertStatus(t, got, "a", CompensationFailed)
		})
	}
}

// ----------------------------------------------------------------------------
// ANGLE 7 — NON-COMPLETED exclusion: the partition is over the run's COMPLETED
// nodes ONLY. A compensable node in ANY other status (Pending / Running / Failed /
// Skipped / Bypassed / Waiting / already-Compensated / CompensationFailed) must
// appear in NO partition set and its compensation must NOT run. This guards the
// M11 interaction — a Bypassed-but-compensable node must not lie its way into the
// partition. Unit-level on compensateLevel (the direct oracle).
// ----------------------------------------------------------------------------

func TestSagaHonest_NonCompletedStatuses_ExcludedFromPartition(t *testing.T) {
	nonCompleted := []NodeStatus{
		Pending, Running, Failed, Skipped, Bypassed, Waiting, Compensated, CompensationFailed,
	}
	for _, st := range nonCompleted {
		st := st
		t.Run(string(st), func(t *testing.T) {
			var ran int64
			nd := NewNode("x", benchNoopAction())
			nd.Compensation = ActionFunc(func(context.Context, *WorkflowData) error {
				atomic.AddInt64(&ran, 1)
				return nil
			})
			data := NewWorkflowData("excl")
			data.SetNodeStatus("x", st)
			out := &sagaOutcome{}

			compensateLevel(context.Background(), []*Node{nd}, data, 4, out)

			require.Zerof(t, atomic.LoadInt64(&ran), "%s: a non-Completed node's comp must NOT run", st)
			require.Emptyf(t, out.compensated, "%s: not in compensated", st)
			require.Emptyf(t, out.failedToCompensate, "%s: not in failed", st)
			require.Emptyf(t, out.skipped, "%s: not in skipped", st)
			got, _ := data.GetNodeStatus("x")
			require.Equalf(t, st, got, "%s: a non-Completed node's status is untouched by rollback", st)
		})
	}
}
