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

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// M12 ph46 saga rollback — ADVERSARIAL suite (independent of the author's tests).
//
// Targets: driveRollback / compensateLevel (saga_rollback.go), the executeLocked
// trigger (workflow.go), the rolling_back marker + Compensated status across the
// three stores.
//
// SCOPE: ph46 HAPPY PATH — every compensation succeeds. We do NOT test a
// compensation returning an error (ph47). We attack the trigger precision, the
// reverse-topological ordering, the per-level concurrency (bounded by
// MaxConcurrency), the idempotency handle, and 3-store durability.
// ============================================================================

// ----------------------------------------------------------------------------
// ANGLE 1 — CONCURRENCY / -race: wide level, MaxConcurrency=1 and =many.
//
// compensateLevel spawns one goroutine per eligible node and each goroutine both
// writes data.SetNodeStatus(Compensated) and increments a shared counter. Hammer
// a WIDE level over many iterations at both concurrency bounds under `go test
// -race`. Oracle: the minimum bar (no race, no panic) + exactly-once totality
// (every Completed compensable node compensated once, count == N).
// ----------------------------------------------------------------------------

func buildWideCompLevel(n int, counter *int64, perNode []int64) []*Node {
	level := make([]*Node, n)
	for i := 0; i < n; i++ {
		i := i
		nd := NewNode(fmt.Sprintf("w%d", i), benchNoopAction())
		nd.Compensation = ActionFunc(func(context.Context, *WorkflowData) error {
			atomic.AddInt64(counter, 1)
			atomic.AddInt64(&perNode[i], 1)
			return nil
		})
		level[i] = nd
	}
	return level
}

func freshCompletedData(id string, level []*Node) *WorkflowData {
	data := NewWorkflowData(id)
	for _, nd := range level {
		data.SetNodeStatus(nd.Name, Completed)
	}
	return data
}

func TestSagaAdv_WideLevel_ConcurrentCompensate_RaceClean(t *testing.T) {
	const (
		nodes = 128
		iters = 200
	)
	for _, maxConc := range []int{1, 4, 16, nodes, nodes * 4} {
		maxConc := maxConc
		t.Run(fmt.Sprintf("maxConc=%d", maxConc), func(t *testing.T) {
			t.Parallel()
			for it := 0; it < iters; it++ {
				var counter int64
				perNode := make([]int64, nodes)
				level := buildWideCompLevel(nodes, &counter, perNode)
				data := freshCompletedData("wide", level)

				compensateLevel(context.Background(), level, data, maxConc, &sagaOutcome{})

				require.Equalf(t, int64(nodes), atomic.LoadInt64(&counter),
					"iter %d: every compensable node runs its compensation exactly once", it)
				for i := 0; i < nodes; i++ {
					require.Equalf(t, int64(1), perNode[i], "iter %d node %d compensated exactly once", it, i)
					st, _ := data.GetNodeStatus(fmt.Sprintf("w%d", i))
					require.Equalf(t, Compensated, st, "iter %d node %d marked Compensated", it, i)
				}
			}
		})
	}
}

// TestSagaAdv_WideFan_FullExecute_RaceClean drives the whole executeLocked trigger
// path (not just compensateLevel) with a wide fan of compensable nodes under a hard
// failure, at MaxConcurrency 1 and many, under -race.
func TestSagaAdv_WideFan_FullExecute_RaceClean(t *testing.T) {
	const width = 64
	for _, maxConc := range []int{1, width} {
		maxConc := maxConc
		t.Run(fmt.Sprintf("maxConc=%d", maxConc), func(t *testing.T) {
			t.Parallel()
			var compCount int64
			store := NewInMemoryStore()
			b := NewWorkflowBuilder().WithWorkflowID("wide-fan")
			b.AddNode("root").WithAction(benchNoopAction()).
				WithCompensation(func(context.Context, *WorkflowData) error { atomic.AddInt64(&compCount, 1); return nil })
			for i := 0; i < width; i++ {
				b.AddNode(fmt.Sprintf("mid%d", i)).WithAction(benchNoopAction()).
					WithCompensation(func(context.Context, *WorkflowData) error { atomic.AddInt64(&compCount, 1); return nil }).
					DependsOn("root")
			}
			deps := make([]string, width)
			for i := range deps {
				deps[i] = fmt.Sprintf("mid%d", i)
			}
			b.AddNode("fail").
				WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
				DependsOn(deps...)
			dag, err := b.Build()
			require.NoError(t, err)
			dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: maxConc})
			w := &Workflow{DAG: dag, WorkflowID: "wide-fan", Store: store}

			require.Error(t, w.Execute(context.Background()))
			// root + width mids all Completed compensable => width+1 compensations.
			require.Equal(t, int64(width+1), atomic.LoadInt64(&compCount))
			got, err := store.Load("wide-fan")
			require.NoError(t, err)
			require.True(t, got.IsRollingBack())
			assertStatus(t, got, "root", Compensated)
			assertStatus(t, got, "fail", Failed)
			for i := 0; i < width; i++ {
				assertStatus(t, got, fmt.Sprintf("mid%d", i), Compensated)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ANGLE 3 — REVERSE-TOPO under parallelism: property over random layered DAGs.
//
// Generate random layered saga-DAGs (chains, diamonds, wide fans, deep). Property:
// for every dependency edge u<-v (v DependsOn u), u's compensation runs strictly
// AFTER v's — the reverse-topological partial order. Oracle: the invariant, keyed
// off the actual DAG edges (not a hand-picked shape). Bite: flip driveRollback's
// level loop to forward order and this fails.
// ----------------------------------------------------------------------------

// buildLayeredSaga deterministically builds a layered DAG from a seed: `layers`
// layers, each of random width in [1,maxWidth]; every node in layer L>0 depends on
// a random NON-EMPTY subset of layer L-1. All nodes are compensable noops. A final
// "fail" node depends on every last-layer node and hard-fails to trigger rollback.
// Returns the workflow and the recorder shared by all compensations.
func buildLayeredSaga(seed int64, layers, maxWidth int, tick *tickRecorder) *Workflow {
	rng := rand.New(rand.NewSource(seed))
	b := NewWorkflowBuilder().WithWorkflowID("layered")

	prev := []string{}
	for l := 0; l < layers; l++ {
		width := 1 + rng.Intn(maxWidth)
		cur := make([]string, width)
		for i := 0; i < width; i++ {
			name := fmt.Sprintf("L%d_%d", l, i)
			cur[i] = name
			nb := b.AddNode(name).WithAction(benchNoopAction()).
				WithCompensation(tick.comp(name))
			if l > 0 {
				// pick a non-empty random subset of prev as dependencies
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
	// terminal failing node depends on the whole last layer.
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn(prev...)

	dag, err := b.Build()
	if err != nil {
		panic(err)
	}
	// vary concurrency by seed to exercise the bound alongside the ordering.
	dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 1 + int(seed%8)})
	return &Workflow{DAG: dag, WorkflowID: "layered", Store: NewInMemoryStore()}
}

// tickRecorder assigns a monotonically increasing tick to each compensation as it
// runs, thread-safe (a level compensates concurrently).
type tickRecorder struct {
	mu   sync.Mutex
	next int
	at   map[string]int
}

func newTickRecorder() *tickRecorder { return &tickRecorder{at: map[string]int{}} }

func (r *tickRecorder) comp(name string) func(context.Context, *WorkflowData) error {
	return func(context.Context, *WorkflowData) error {
		r.mu.Lock()
		r.at[name] = r.next
		r.next++
		r.mu.Unlock()
		return nil
	}
}

// edgesOf returns the dependency edges (dep -> node) of the DAG's compensable nodes.
func edgesOf(w *Workflow) [][2]string {
	var edges [][2]string
	for _, n := range w.DAG.Nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, [2]string{dep.Name, n.Name})
		}
	}
	return edges
}

func TestSagaAdv_ReverseTopo_Property(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 300
	props := gopter.NewProperties(params)

	props.Property("dependency compensates strictly after every dependent", prop.ForAll(
		func(seed int64, layers, maxWidth int) bool {
			rec := newTickRecorder()
			w := buildLayeredSaga(seed, 2+layers, 1+maxWidth, rec)
			if err := w.Execute(context.Background()); err == nil {
				return false // the terminal node must fail => rollback must trigger
			}
			// Totality: every compensable node (all but the non-compensating "fail"
			// terminal) must have been compensated exactly once.
			for _, n := range w.DAG.Nodes {
				if n.Name == "fail" {
					continue // the failing terminal declares no compensation
				}
				if _, ok := rec.at[n.Name]; !ok {
					return false
				}
			}
			// Reverse-topo: for every dependency edge (dep -> node) between two
			// compensated nodes, dep compensates strictly AFTER node. Edges touching
			// the non-compensable "fail" node are irrelevant to the ordering.
			for _, e := range edgesOf(w) {
				dep, node := e[0], e[1]
				td, okd := rec.at[dep]
				tn, okn := rec.at[node]
				if !okd || !okn {
					continue // one endpoint (the "fail" terminal) is not compensated
				}
				if td <= tn {
					return false // dep compensated before/at its dependent: reverse-topo violated
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

// TestSagaAdv_ReverseTopo_ExactlyOnce is the totality companion to the property:
// over a batch of random DAGs, every Completed compensable node is compensated
// EXACTLY once (never zero, never twice).
func TestSagaAdv_ReverseTopo_ExactlyOnce(t *testing.T) {
	for seed := int64(1); seed <= 60; seed++ {
		rec := newTickRecorder()
		w := buildLayeredSaga(seed, 4, 4, rec)
		require.Error(t, w.Execute(context.Background()))

		// count declared compensable nodes (all but "fail")
		want := 0
		for _, n := range w.DAG.Nodes {
			if n.Name != "fail" {
				want++
			}
		}
		require.Lenf(t, rec.at, want, "seed %d: every compensable node compensated exactly once", seed)
	}
}

// ----------------------------------------------------------------------------
// ANGLE 2 — TRIGGER PRECISION boundaries.
// ----------------------------------------------------------------------------

// TestSagaAdv_MultiNodeFailFast_Triggers — several nodes in one wide level fail
// concurrently. Rollback still fires exactly once and compensates the Completed
// compensable siblings (the failed nodes are never Completed => not compensated).
func TestSagaAdv_MultiNodeFailFast_Triggers(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("multi-fail")
	b.AddNode("root").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("root"); return nil })
	// three concurrent hard failures + two compensable survivors, all in one level.
	for _, f := range []string{"f0", "f1", "f2"} {
		b.AddNode(f).
			WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom-" + f) })).
			DependsOn("root")
	}
	for _, s := range []string{"s0", "s1"} {
		s := s
		b.AddNode(s).WithAction(benchNoopAction()).
			WithCompensation(func(context.Context, *WorkflowData) error { rec.record(s); return nil }).
			DependsOn("root")
	}
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "multi-fail", Store: store}

	require.Error(t, w.Execute(context.Background()))
	got, err := store.Load("multi-fail")
	require.NoError(t, err)
	require.True(t, got.IsRollingBack(), "a multi-node fail-fast must trigger rollback")
	assertStatus(t, got, "root", Compensated)
	// failed nodes are never compensated
	for _, f := range []string{"f0", "f1", "f2"} {
		assertStatus(t, got, f, Failed)
	}
	// survivors that reached Completed before fail-fast get compensated;
	// survivors that were skipped by fail-fast do not. Whatever their forward
	// status, the recorder must never contain a failed node, and root must appear.
	snap := rec.snapshot()
	require.Contains(t, snap, "root")
	for _, name := range snap {
		require.NotContains(t, []string{"f0", "f1", "f2"}, name, "a failed node is never compensated")
	}
}

// TestSagaAdv_CoeFailsAlongsideHardFail — a continue-on-error node FAILS (soft, so
// it stays Failed, never Completed) in the same run as a hard failure elsewhere.
// The hard failure triggers rollback; the coe node is NOT compensated (Failed), but
// the Completed compensable nodes are.
func TestSagaAdv_CoeFailsAlongsideHardFail(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("coe-plus-hard")
	b.AddNode("root").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("root"); return nil })
	// coe node fails softly but declares a compensation
	b.AddNode("coe").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("soft") })).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("coe"); return nil }).
		WithContinueOnError().
		DependsOn("root")
	// a Completed compensable node in the same level as the eventual hard fail's parent
	b.AddNode("ok").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("ok"); return nil }).
		DependsOn("root")
	// hard failure downstream of the coe node (so the run still fails hard)
	b.AddNode("hard").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("coe", "ok")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "coe-plus-hard", Store: store}

	require.Error(t, w.Execute(context.Background()))
	got, err := store.Load("coe-plus-hard")
	require.NoError(t, err)
	require.True(t, got.IsRollingBack())
	assertStatus(t, got, "coe", Failed) // soft-failed => never Completed => not compensated
	assertStatus(t, got, "hard", Failed)
	assertStatus(t, got, "root", Compensated)
	assertStatus(t, got, "ok", Compensated)
	snap := rec.snapshot()
	require.NotContains(t, snap, "coe", "a Failed coe node must NOT be compensated")
	require.Contains(t, snap, "root")
	require.Contains(t, snap, "ok")
}

// TestSagaAdv_CallerCancel_Triggers — a caller cancel that surfaces from DAG.Execute
// as context.Canceled must trigger rollback (DEC-M12-TRIGGER). We cancel the ctx
// from inside a first-level node's action and return ctx.Err(); a downstream node
// would otherwise run forward.
func TestSagaAdv_CallerCancel_Triggers(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	b := NewWorkflowBuilder().WithWorkflowID("caller-cancel")
	b.AddNode("a").
		WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil })
	b.AddNode("cancel").
		WithAction(ActionFunc(func(c context.Context, _ *WorkflowData) error {
			cancel()
			return c.Err() // context.Canceled
		})).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "caller-cancel", Store: store}

	err = w.Execute(ctx)
	require.Error(t, err)
	got, lerr := store.Load("caller-cancel")
	require.NoError(t, lerr)
	// 'a' completed before the cancel => it is compensated; the run is rolling_back.
	if got.IsRollingBack() {
		assertStatus(t, got, "a", Compensated)
		require.Equal(t, []string{"a"}, rec.snapshot())
	} else {
		t.Fatalf("a caller-cancel that surfaced as %v must trigger rollback (DEC-M12-TRIGGER); "+
			"persisted rolling_back=false, comps=%v", err, rec.snapshot())
	}
}

// TestSagaAdv_SuccessfulCancelRace_NoRollback — the metamorphic negative of the
// trigger: if the DAG COMPLETES successfully, a cancel that arrives too late must
// NOT trigger rollback (Execute returns nil, the err block is never entered). We
// cancel AFTER Execute returns.
func TestSagaAdv_SuccessfulRun_NoRollback(t *testing.T) {
	rec := &compRecorder{}
	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("clean")
	b.AddNode("a").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("a"); return nil })
	b.AddNode("b").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { rec.record("b"); return nil }).
		DependsOn("a")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "clean", Store: store}

	require.NoError(t, w.Execute(context.Background()), "a fully successful saga run does not fail")
	got, err := store.Load("clean")
	require.NoError(t, err)
	require.False(t, got.IsRollingBack(), "a successful run must NEVER trigger rollback")
	require.Empty(t, rec.snapshot(), "no compensation runs on a successful run")
	assertStatus(t, got, "a", Completed)
	assertStatus(t, got, "b", Completed)
}

// ----------------------------------------------------------------------------
// ANGLE 5 — IDEMPOTENCY handle present + stable inside every compensation.
// ----------------------------------------------------------------------------

// TestSagaAdv_IdempotencyKey_PresentAndStable — inside every compensation,
// CompensationIdempotencyKey(ctx) must be present and equal IdempotencyKey(data,
// node). Oracle: the documented contract (derived from WorkflowID+nodeName).
func TestSagaAdv_IdempotencyKey_PresentAndStable(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{} // node -> observed key
	present := map[string]bool{}

	store := NewInMemoryStore()
	b := NewWorkflowBuilder().WithWorkflowID("idem")
	mk := func(name string) func(context.Context, *WorkflowData) error {
		return func(ctx context.Context, data *WorkflowData) error {
			key, ok := CompensationIdempotencyKey(ctx)
			mu.Lock()
			present[name] = ok
			seen[name] = key
			mu.Unlock()
			return nil
		}
	}
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(mk("a"))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(mk("b")).DependsOn("a")
	b.AddNode("c").WithAction(benchNoopAction()).WithCompensation(mk("c")).DependsOn("b")
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("c")
	dag, err := b.Build()
	require.NoError(t, err)
	w := &Workflow{DAG: dag, WorkflowID: "idem", Store: store}

	require.Error(t, w.Execute(context.Background()))

	got, err := store.Load("idem")
	require.NoError(t, err)
	for _, node := range []string{"a", "b", "c"} {
		require.Truef(t, present[node], "CompensationIdempotencyKey MUST be present inside %s's compensation", node)
		want := IdempotencyKey(got, node)
		require.Equalf(t, want, seen[node], "the handle inside %s must equal IdempotencyKey(data, %q)", node, node)
		require.Len(t, seen[node], 64, "the key is a 64-char hex sha-256 digest")
	}
	// distinct nodes get distinct handles (framed by nodeName)
	require.NotEqual(t, seen["a"], seen["b"])
	require.NotEqual(t, seen["b"], seen["c"])

	// STABILITY: a second identical run (same WorkflowID+nodes) yields byte-identical
	// handles — the at-least-once re-invocation contract the ph48 crash-resume leans on.
	seen2 := map[string]string{}
	var mu2 sync.Mutex
	b2 := NewWorkflowBuilder().WithWorkflowID("idem")
	mk2 := func(name string) func(context.Context, *WorkflowData) error {
		return func(ctx context.Context, _ *WorkflowData) error {
			key, _ := CompensationIdempotencyKey(ctx)
			mu2.Lock()
			seen2[name] = key
			mu2.Unlock()
			return nil
		}
	}
	b2.AddNode("a").WithAction(benchNoopAction()).WithCompensation(mk2("a"))
	b2.AddNode("b").WithAction(benchNoopAction()).WithCompensation(mk2("b")).DependsOn("a")
	b2.AddNode("c").WithAction(benchNoopAction()).WithCompensation(mk2("c")).DependsOn("b")
	b2.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("c")
	dag2, err := b2.Build()
	require.NoError(t, err)
	w2 := &Workflow{DAG: dag2, WorkflowID: "idem", Store: NewInMemoryStore()}
	require.Error(t, w2.Execute(context.Background()))
	require.Equal(t, seen, seen2, "compensation idempotency handles must be stable across identical runs")
}

// ----------------------------------------------------------------------------
// ANGLE 4 — 3-STORE PARITY: Compensated + rolling_back round-trip on
// InMemory / JSON / FlatBuffers, including edge cases.
// ----------------------------------------------------------------------------

func eachStore(t *testing.T) map[string]WorkflowStore {
	t.Helper()
	fbDir := t.TempDir()
	jsonDir := t.TempDir()
	fb, err := NewFlatBuffersStore(fbDir)
	require.NoError(t, err)
	js, err := NewJSONFileStore(jsonDir)
	require.NoError(t, err)
	return map[string]WorkflowStore{
		"inmem":       NewInMemoryStore(),
		"json":        js,
		"flatbuffers": fb,
	}
}

// TestSagaAdv_StoreParity_MarkerAndStatus round-trips a rolling_back run carrying
// a Compensated node (plus each other status) through all three stores.
func TestSagaAdv_StoreParity_MarkerAndStatus(t *testing.T) {
	for name, store := range eachStore(t) {
		name, store := name, store
		t.Run(name, func(t *testing.T) {
			id := "parity-" + name
			data := NewWorkflowData(id)
			data.SetRollingBack(true)
			data.SetNodeStatus("comp", Compensated)
			data.SetNodeStatus("done", Completed)
			data.SetNodeStatus("failed", Failed)
			data.SetNodeStatus("bypass", Bypassed)
			data.SetNodeStatus("skip", Skipped)
			require.NoError(t, store.Save(data))

			got, err := store.Load(id)
			require.NoError(t, err)
			require.True(t, got.IsRollingBack(), "%s: rolling_back marker must round-trip", name)
			assertStatus(t, got, "comp", Compensated)
			assertStatus(t, got, "done", Completed)
			assertStatus(t, got, "failed", Failed)
			assertStatus(t, got, "bypass", Bypassed)
			assertStatus(t, got, "skip", Skipped)
		})
	}
}

// TestSagaAdv_StoreParity_EdgeCases — a rolling_back run with ZERO Completed nodes,
// and a NON-rolling-back run that must NOT come back marked (the false path).
func TestSagaAdv_StoreParity_EdgeCases(t *testing.T) {
	for name, store := range eachStore(t) {
		name, store := name, store
		t.Run(name, func(t *testing.T) {
			// (a) rolling_back with zero Completed/Compensated nodes at all
			idEmpty := "parity-empty-" + name
			empty := NewWorkflowData(idEmpty)
			empty.SetRollingBack(true)
			require.NoError(t, store.Save(empty))
			got, err := store.Load(idEmpty)
			require.NoError(t, err)
			require.True(t, got.IsRollingBack(), "%s: a rolling_back run with zero nodes still round-trips the marker", name)

			// (b) NOT rolling back => must load back false (the omitted/false path)
			idClean := "parity-clean-" + name
			clean := NewWorkflowData(idClean)
			clean.SetNodeStatus("a", Completed)
			require.NoError(t, store.Save(clean))
			got2, err := store.Load(idClean)
			require.NoError(t, err)
			require.False(t, got2.IsRollingBack(), "%s: a non-saga run must NEVER load as rolling_back", name)
			assertStatus(t, got2, "a", Completed)
		})
	}
}

// TestSagaAdv_StoreParity_HugeNodeSet round-trips a large rolling_back run (a wide
// saga) through all three stores — every Compensated node survives.
func TestSagaAdv_StoreParity_HugeNodeSet(t *testing.T) {
	const n = 3000
	for name, store := range eachStore(t) {
		name, store := name, store
		t.Run(name, func(t *testing.T) {
			id := "parity-huge-" + name
			data := NewWorkflowData(id)
			data.SetRollingBack(true)
			for i := 0; i < n; i++ {
				data.SetNodeStatus(fmt.Sprintf("n%d", i), Compensated)
			}
			require.NoError(t, store.Save(data))
			got, err := store.Load(id)
			require.NoError(t, err)
			require.True(t, got.IsRollingBack())
			// spot-check every node (not just a sample)
			bad := 0
			for i := 0; i < n; i++ {
				st, ok := got.GetNodeStatus(fmt.Sprintf("n%d", i))
				if !ok || st != Compensated {
					bad++
				}
			}
			require.Zerof(t, bad, "%s: all %d Compensated nodes must round-trip; %d wrong", name, n, bad)
		})
	}
}

// TestSagaAdv_StoreParity_FullExecute runs the SAME saga on all three stores and
// asserts identical final status maps + rolling_back marker — the metamorphic
// "store must not change the outcome" relation.
func TestSagaAdv_StoreParity_FullExecute(t *testing.T) {
	build := func(id string, store WorkflowStore) *Workflow {
		b := NewWorkflowBuilder().WithWorkflowID(id)
		b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(func(context.Context, *WorkflowData) error { return nil })
		b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(func(context.Context, *WorkflowData) error { return nil }).DependsOn("a")
		b.AddNode("fail").
			WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
			DependsOn("b")
		dag, err := b.Build()
		require.NoError(t, err)
		return &Workflow{DAG: dag, WorkflowID: id, Store: store}
	}

	type outcome struct {
		rolling bool
		status  map[string]string
	}
	results := map[string]outcome{}
	for name, store := range eachStore(t) {
		id := "pfe-" + name
		w := build(id, store)
		require.Error(t, w.Execute(context.Background()))
		got, err := store.Load(id)
		require.NoError(t, err)
		sm := map[string]string{}
		for _, node := range []string{"a", "b", "fail"} {
			st, _ := got.GetNodeStatus(node)
			sm[node] = string(st)
		}
		results[name] = outcome{rolling: got.IsRollingBack(), status: sm}
	}

	// all three stores must agree
	names := make([]string, 0, len(results))
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)
	ref := results[names[0]]
	for _, n := range names[1:] {
		require.Equalf(t, ref.rolling, results[n].rolling, "store %s disagrees on rolling_back", n)
		require.Equalf(t, ref.status, results[n].status, "store %s disagrees on final status map", n)
	}
	// and the reference outcome is the expected one
	require.True(t, ref.rolling)
	require.Equal(t, string(Compensated), ref.status["a"])
	require.Equal(t, string(Compensated), ref.status["b"])
	require.Equal(t, string(Failed), ref.status["fail"])
}
