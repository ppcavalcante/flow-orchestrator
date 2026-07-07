package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// M12 ph48 — DURABLE RESUME-IN-ROLLBACK adversarial suite (the DUR-01 crux).
//
// Independent complement to saga_durable_rollback_test.go, which crashes ONCE on
// a single hand-picked position (a's undo) of one hand-picked DAG. This file
// CRASHES THE ROLLBACK EVERYWHERE: every reverse-level checkpoint position, over
// random layered saga-DAGs, single- and multi-crash, and attacks the six angles:
//
//   1. crash at every rollback-save position  -> status exactly-once, partition exact
//   2. double-compensation window             -> status exactly-once, effect bounded
//   3. MAJOR-4                                 -> forward action NEVER re-runs on resume
//   4. crash after CompensationFailed          -> stays failed (durable reconstruct)
//   5. reconstruction honesty                  -> exact partition; cancel-trigger cause=nil
//   6. NEVER-NIL                               -> a rolled-back run is never nil on resume
//
// ORACLES: (a) status totality — every compensable Completed node ends
// Compensated XOR CompensationFailed, every no-comp Completed node stays Completed;
// (b) the SagaError partition == the persisted terminal statuses, exactly; (c) the
// forward-action-never-re-runs invariant (a per-node forward counter); (d) the
// minimum bar — no panic escapes a resume, no hang, no nil on a rolled-back run.
// ============================================================================

// ---------------------------------------------------------------------------
// rbCrashStore — panics on chosen ROLLBACK-PHASE Save positions.
//
// A "rollback-phase Save" is any Store.Save whose snapshot has rolling_back set:
// the marker save (position 1) plus each per-reverse-level checkpoint and the
// final authoritative save. Positions are 1-based over the store's WHOLE lifetime
// (they persist across resume Execute calls), so a set like {2,4} models a crash
// on the 2nd rollback save of run 1 and again on the 4th observed across resumes
// — the multi-crash model. Each position fires at most once. A crash panics
// BEFORE delegating to the wrapped Save, so the persisted snapshot is the one from
// the PREVIOUS successful save (true process-death semantics).
// ---------------------------------------------------------------------------
type rbCrashStore struct {
	*InMemoryStore
	mu         sync.Mutex
	rbSaveSeen int
	crashAt    map[int]bool
	fired      map[int]bool
}

func newRBCrashStore(positions ...int) *rbCrashStore {
	at := make(map[int]bool, len(positions))
	for _, p := range positions {
		at[p] = true
	}
	return &rbCrashStore{InMemoryStore: NewInMemoryStore(), crashAt: at, fired: map[int]bool{}}
}

func (s *rbCrashStore) Save(data *WorkflowData) error {
	s.mu.Lock()
	if data.IsRollingBack() {
		s.rbSaveSeen++
		if s.crashAt[s.rbSaveSeen] && !s.fired[s.rbSaveSeen] {
			s.fired[s.rbSaveSeen] = true
			s.mu.Unlock()
			panic(fmt.Sprintf("simulated crash on rollback save #%d", s.rbSaveSeen))
		}
	}
	s.mu.Unlock()
	return s.InMemoryStore.Save(data)
}

// SaveCheckpoint must route through the same crash gate — the forward pass uses it,
// but a rolling_back snapshot never reaches it (rollback never per-level checkpoints
// via Checkpointer). Delegated to Save for uniform crash accounting.
func (s *rbCrashStore) SaveCheckpoint(data *WorkflowData) error { return s.Save(data) }

// ---------------------------------------------------------------------------
// crashSaga — a random layered saga with instrumented forward + compensation
// effects, so every invariant has a concrete oracle.
// ---------------------------------------------------------------------------
type crashSaga struct {
	dag       *DAG
	id        string
	mu        sync.Mutex
	forward   map[string]int // per-node FORWARD action invocation count (MAJOR-4 oracle)
	effect    map[string]int // per-node COMPENSATION effect count (at-least-once oracle)
	keySeen   map[string]map[string]bool
	hasComp   map[string]bool // node declares a compensation
	compFails map[string]bool // that compensation is a permanent failure
	nodes     []string        // all non-"fail" node names
}

// buildCrashSaga deterministically builds a random layered saga from a seed. Each
// layer L>0 depends on a random non-empty subset of layer L-1 (chains / diamonds /
// wide fans / deep). Each node is randomly assigned {comp-succeeds, comp-fails,
// no-comp}; L0_0 is FORCED comp-fails when forceFail so the run yields a *SagaError.
// A terminal "fail" node hard-fails to trigger the rollback (Failed-trigger path).
func buildCrashSaga(seed int64, layers, maxWidth int, forceFail bool) *crashSaga {
	rng := rand.New(rand.NewSource(seed))
	id := fmt.Sprintf("crash-%d", seed)
	cs := &crashSaga{
		id:        id,
		forward:   map[string]int{},
		effect:    map[string]int{},
		keySeen:   map[string]map[string]bool{},
		hasComp:   map[string]bool{},
		compFails: map[string]bool{},
	}
	b := NewWorkflowBuilder().WithWorkflowID(id)

	mkForward := func(name string) ActionFunc {
		return func(context.Context, *WorkflowData) error {
			cs.mu.Lock()
			cs.forward[name]++
			cs.mu.Unlock()
			return nil
		}
	}
	mkComp := func(name string, fail bool) func(context.Context, *WorkflowData) error {
		return func(ctx context.Context, _ *WorkflowData) error {
			cs.mu.Lock()
			cs.effect[name]++
			// record the idempotency key presented on this invocation (MAJOR-3 stability).
			if key, ok := CompensationIdempotencyKey(ctx); ok {
				if cs.keySeen[name] == nil {
					cs.keySeen[name] = map[string]bool{}
				}
				cs.keySeen[name][key] = true
			}
			cs.mu.Unlock()
			if fail {
				return errors.New("comp-fail:" + name)
			}
			return nil
		}
	}

	prev := []string{}
	forced := forceFail
	for l := range layers {
		width := 1 + rng.Intn(maxWidth)
		cur := make([]string, width)
		for i := range width {
			name := fmt.Sprintf("L%d_%d", l, i)
			cur[i] = name
			cs.nodes = append(cs.nodes, name)
			nb := b.AddNode(name).WithAction(mkForward(name))

			beh := rng.Intn(3)
			if forced {
				beh = 1
				forced = false
			}
			switch beh {
			case 0:
				nb.WithCompensation(mkComp(name, false))
				cs.hasComp[name] = true
			case 1:
				nb.WithCompensation(mkComp(name, true))
				cs.hasComp[name] = true
				cs.compFails[name] = true
			default:
				// no compensation
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
	dag.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 1 + int(seed&7)})
	cs.dag = dag
	return cs
}

func (cs *crashSaga) workflow(store WorkflowStore) *Workflow {
	return &Workflow{DAG: cs.dag, WorkflowID: cs.id, Store: store}
}

// runToCompletion drives Execute, recovering a simulated-crash panic and re-driving
// (resume) until the run returns without crashing, up to maxResumes. Returns the
// final error and the number of crashes recovered.
func (cs *crashSaga) runToCompletion(t *testing.T, store WorkflowStore, maxResumes int) (crashes int, resErr error) {
	t.Helper()
	for range maxResumes {
		var out error
		crashed := func() (c bool) {
			defer func() {
				if r := recover(); r != nil {
					c = true
				}
			}()
			out = cs.workflow(store).Execute(context.Background())
			return false
		}()
		if crashed {
			crashes++
			continue
		}
		return crashes, out
	}
	t.Fatalf("run did not converge after %d resumes (possible hang/livelock)", maxResumes)
	return crashes, nil
}

// assertDurableInvariants checks the full battery against the persisted terminal
// state and the returned error, for a Failed-trigger run (never-nil, partition exact).
func (cs *crashSaga) assertDurableInvariants(t *testing.T, store WorkflowStore, resErr error, crashes int) {
	t.Helper()
	persisted, err := store.Load(cs.id)
	require.NoError(t, err)

	// Oracle sets from the ASSIGNED behavior.
	expComp := map[string]bool{}
	expFail := map[string]bool{}
	expSkip := map[string]bool{}
	for _, n := range cs.nodes {
		switch {
		case cs.hasComp[n] && cs.compFails[n]:
			expFail[n] = true
		case cs.hasComp[n]:
			expComp[n] = true
		default:
			expSkip[n] = true
		}
	}

	// (A) STATUS totality: every compensable node ended Compensated XOR CompensationFailed
	//     exactly as assigned; every no-comp node stayed Completed; fail stayed Failed.
	for _, n := range cs.nodes {
		st, ok := persisted.GetNodeStatus(n)
		require.True(t, ok, "node %s has a persisted status", n)
		switch {
		case expComp[n]:
			require.Equalf(t, Compensated, st, "node %s: succeeding comp must end Compensated (seed=%s)", n, cs.id)
		case expFail[n]:
			require.Equalf(t, CompensationFailed, st, "node %s: failing comp must end CompensationFailed (seed=%s)", n, cs.id)
		default:
			require.Equalf(t, Completed, st, "node %s: no-comp Completed node stays Completed (seed=%s)", n, cs.id)
		}
	}
	failSt, _ := persisted.GetNodeStatus("fail")
	require.Equal(t, Failed, failSt, "the trigger node stays Failed")

	// (B) NEVER-NIL: a rolled-back run is never nil (§3 / ph47-F5).
	require.Errorf(t, resErr, "a rolled-back run must never be nil (seed=%s)", cs.id)

	// (C) PARTITION EXACTNESS: the returned error's partition == the persisted statuses.
	if len(expFail) > 0 {
		var se *SagaError
		require.ErrorAsf(t, resErr, &se, "with a failing comp the run must surface a *SagaError (seed=%s)", cs.id)
		require.Truef(t, setEq(setOfNames(se.Compensated), expComp), "Compensated partition exact (seed=%s): got %v want %v", cs.id, se.Compensated, keysOf(expComp))
		require.Truef(t, setEq(namesOfNE(se.FailedToCompensate), expFail), "FailedToCompensate partition exact (seed=%s): got %v want %v", cs.id, failedNames(se.FailedToCompensate), keysOf(expFail))
		require.Truef(t, setEq(setOfNames(se.Skipped), expSkip), "Skipped partition exact (seed=%s): got %v want %v", cs.id, se.Skipped, keysOf(expSkip))
		// totality: union == all non-fail nodes, pairwise disjoint (no missing/phantom/double).
		union := map[string]bool{}
		total := 0
		for _, grp := range [][]string{se.Compensated, se.Skipped, failedNames(se.FailedToCompensate)} {
			for _, n := range grp {
				require.Falsef(t, union[n], "node %s double-counted in the partition (seed=%s)", n, cs.id)
				union[n] = true
				total++
			}
		}
		require.Equal(t, len(cs.nodes), total, "partition covers every Completed node exactly once")
	} else {
		// clean rollback across the crash -> the reconstructed forward *ExecutionError, bare.
		var ee *ExecutionError
		require.ErrorAsf(t, resErr, &ee, "a clean rollback reconstructs the *ExecutionError cause (seed=%s)", cs.id)
		var se *SagaError
		require.Falsef(t, errors.As(resErr, &se), "a clean rollback is NOT a *SagaError (seed=%s)", cs.id)
	}

	// (D) MAJOR-4: the forward action of every compensable node ran EXACTLY ONCE — never
	//     re-driven forward on a rollback-resume.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, n := range cs.nodes {
		require.Equalf(t, 1, cs.forward[n], "MAJOR-4: forward action of %s must run exactly once, never on resume (seed=%s, crashes=%d)", n, cs.id, crashes)
	}

	// (E) AT-LEAST-ONCE, BOUNDED: every declared compensation ran >=1 time; with `crashes`
	//     crashes a node's effect re-runs at most `crashes` extra times (the SetNodeStatus
	//     -> Save window). And the idempotency key is STABLE across every re-invocation
	//     (MAJOR-3): a node ever presents exactly ONE distinct key.
	for _, n := range cs.nodes {
		if !cs.hasComp[n] {
			continue
		}
		require.GreaterOrEqualf(t, cs.effect[n], 1, "comp of %s ran at least once (seed=%s)", n, cs.id)
		require.LessOrEqualf(t, cs.effect[n], 1+crashes, "comp of %s re-ran at most once per crash (seed=%s)", n, cs.id)
		require.LessOrEqualf(t, len(cs.keySeen[n]), 1, "MAJOR-3: comp of %s presented a STABLE idempotency key across resume (seed=%s): %v", n, cs.id, cs.keySeen[n])
	}
}

// ---- small set helpers (uniquely named to avoid sibling-file collisions) ----
func setOfNames(ns []string) map[string]bool {
	m := make(map[string]bool, len(ns))
	for _, n := range ns {
		m[n] = true
	}
	return m
}
func namesOfNE(nes []NodeError) map[string]bool {
	m := make(map[string]bool, len(nes))
	for _, ne := range nes {
		m[ne.NodeName] = true
	}
	return m
}
func setEq(a, b map[string]bool) bool {
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
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ============================================================================
// ANGLE 6 (THE DEFECT) — a caller-cancel-triggered rollback that CRASHES and
// RESUMES returns nil, indistinguishable from success.
//
// DEC-M12-TRIGGER: a caller cancel triggers rollback but surfaces a ctx error, NOT
// an *ExecutionError, so NO node is persisted Failed. The marker save records
// rolling_back=true over the forward statuses (no Failed). A crash there and a
// resume lands in finishRollback(data, nil): the cause is reconstructed from
// persisted Failed nodes — but there are NONE — so reconstructCause returns nil,
// and a CLEAN rollback returns that nil.
//
// §3 CLAIM (saga_rollback.go / workflow.go): "finishRollback returns the
// reconstructed *SagaError or bare cause — NEVER nil on a rolled-back run."
// This test REPRODUCES a nil return on a genuinely rolled-back run.
// ============================================================================
func TestSagaDurableCrash_CancelTrigger_CleanResume_ReturnsNil(t *testing.T) {
	store := NewInMemoryStore()
	comp := func(context.Context, *WorkflowData) error { return nil } // clean compensation

	b := NewWorkflowBuilder().WithWorkflowID("cancel-crash")
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(comp)
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(comp).DependsOn("a")
	b.AddNode("fail").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).DependsOn("b")
	dag, err := b.Build()
	require.NoError(t, err)

	// A snapshot as a crash right after the cancel-triggered marker save would leave
	// it: rolling_back set, a/b Completed, NO node Failed (cancel leaves none).
	seed := NewWorkflowData("cancel-crash")
	seed.SetNodeStatus("a", Completed)
	seed.SetNodeStatus("b", Completed)
	seed.SetRollingBack(true)
	require.NoError(t, store.Save(seed))

	w := &Workflow{DAG: dag, WorkflowID: "cancel-crash", Store: store}
	resErr := w.Execute(context.Background())

	// The run DID roll back: statuses are Compensated, marker still set.
	got, lerr := store.Load("cancel-crash")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
	assertStatus(t, got, "b", Compensated)
	require.True(t, got.IsRollingBack(), "the run knows it rolled back (state honest)")

	// §3: a rolled-back run must NEVER be nil. It is. Indistinguishable from success.
	require.Error(t, resErr,
		"DEFECT (ph48 §3 / ph47-F5): a cancel-triggered, cleanly-compensated rollback that crashes "+
			"and resumes returns nil — a rolled-back run is reported as success")
}

// Control: the SAME snapshot but WITH a Failed node (the fail-fast trigger) correctly
// reconstructs the cause -> non-nil. Proves the nil is specific to the missing-Failed
// (cancel) trigger, not a general resume defect.
func TestSagaDurableCrash_FailedTrigger_CleanResume_NonNil_Control(t *testing.T) {
	store := NewInMemoryStore()
	comp := func(context.Context, *WorkflowData) error { return nil }
	b := NewWorkflowBuilder().WithWorkflowID("fail-crash")
	b.AddNode("a").WithAction(benchNoopAction()).WithCompensation(comp)
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(comp).DependsOn("a")
	b.AddNode("fail").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).DependsOn("b")
	dag, err := b.Build()
	require.NoError(t, err)

	seed := NewWorkflowData("fail-crash")
	seed.SetNodeStatus("a", Completed)
	seed.SetNodeStatus("b", Completed)
	seed.SetNodeStatus("fail", Failed) // the difference: a Failed node to reconstruct from
	seed.SetRollingBack(true)
	require.NoError(t, store.Save(seed))

	err = (&Workflow{DAG: dag, WorkflowID: "fail-crash", Store: store}).Execute(context.Background())
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "the Failed node reconstructs a non-nil cause on resume")
}

// ============================================================================
// ANGLE 1 + 3 + 5 — CRASH AT EVERY ROLLBACK-SAVE POSITION over a rich fixed DAG.
// For each single crash position the resumed run must satisfy the full battery:
// status exactly-once, partition exact, forward-never-re-runs, never-nil. Failed
// trigger, so never-nil is via the reconstructed cause.
// ============================================================================
func TestSagaDurableCrash_EveryPosition_RichDAG(t *testing.T) {
	// A rich shape: chain + diamond + wide fan + a no-comp node, a failing comp.
	// seed chosen so the generator yields >=1 failing comp and a multi-level shape.
	for pos := 1; pos <= 8; pos++ {
		pos := pos
		t.Run(fmt.Sprintf("crash_at_rollback_save_%d", pos), func(t *testing.T) {
			cs := buildCrashSaga(1234567, 4, 3, true /*forceFail*/)
			store := newRBCrashStore(pos)
			crashes, resErr := cs.runToCompletion(t, store, 12)
			cs.assertDurableInvariants(t, store, resErr, crashes)
		})
	}
}

// ============================================================================
// ANGLE 1 (PROPERTY) — CRASH AT A RANDOM POSITION over RANDOM saga-DAGs.
// The generator randomizes shape (chains/diamonds/fans/deep), per-node comp
// behavior, and concurrency; the crash position is seeded too. For EVERY case the
// full durable battery must hold across the crash-resume.
// ============================================================================
func TestSagaDurableCrash_Property_RandomDAGs(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 250
	props := gopter.NewProperties(params)

	props.Property("crash at any rollback position resumes to an exact, honest, forward-frozen partition", prop.ForAll(
		func(seed int64, layers, maxWidth, crashPos int) bool {
			cs := buildCrashSaga(seed, 2+layers, 1+maxWidth, true /*forceFail*/)
			store := newRBCrashStore(1 + crashPos) // 1..8; 1 = marker save
			crashes, resErr := cs.runToCompletion(t, store, 16)

			// Re-run the assertion battery as booleans (gopter wants a bool).
			persisted, err := store.Load(cs.id)
			if err != nil {
				t.Logf("seed=%d load failed: %v", seed, err)
				return false
			}
			if resErr == nil {
				t.Logf("seed=%d crashPos=%d: NIL on a rolled-back run", seed, crashPos)
				return false
			}
			for _, n := range cs.nodes {
				st, _ := persisted.GetNodeStatus(n)
				want := Completed
				if cs.hasComp[n] && cs.compFails[n] {
					want = CompensationFailed
				} else if cs.hasComp[n] {
					want = Compensated
				}
				if st != want {
					t.Logf("seed=%d node=%s status=%v want=%v", seed, n, st, want)
					return false
				}
			}
			cs.mu.Lock()
			for _, n := range cs.nodes {
				if cs.forward[n] != 1 {
					t.Logf("seed=%d MAJOR-4 broken: forward[%s]=%d crashes=%d", seed, n, cs.forward[n], crashes)
					cs.mu.Unlock()
					return false
				}
			}
			cs.mu.Unlock()

			// partition exactness via the *SagaError (>=1 failing comp by construction).
			var se *SagaError
			if !errors.As(resErr, &se) {
				t.Logf("seed=%d: expected *SagaError, got %T", seed, resErr)
				return false
			}
			exp := map[string]NodeStatus{}
			for _, n := range cs.nodes {
				if cs.hasComp[n] && cs.compFails[n] {
					exp[n] = CompensationFailed
				} else if cs.hasComp[n] {
					exp[n] = Compensated
				} else {
					exp[n] = Completed
				}
			}
			for _, n := range se.Compensated {
				if exp[n] != Compensated {
					return false
				}
			}
			for _, ne := range se.FailedToCompensate {
				if exp[ne.NodeName] != CompensationFailed {
					return false
				}
			}
			for _, n := range se.Skipped {
				if exp[n] != Completed {
					return false
				}
			}
			return len(se.Compensated)+len(se.FailedToCompensate)+len(se.Skipped) == len(cs.nodes)
		},
		gen.Int64Range(1, 1_000_000),
		gen.IntRange(0, 3), // extra layers -> 2..5
		gen.IntRange(0, 3), // extra width  -> 1..4
		gen.IntRange(0, 7), // crash position 1..8
	))

	props.TestingRun(t)
}

// ============================================================================
// ANGLE 2 + 4 — MULTI-CRASH resume chain, and CRASH AFTER CompensationFailed.
// Crash on TWO distinct rollback positions across resumes; the run must still
// converge to the exact honest partition, forward frozen, bounded at-least-once,
// and a node that reached CompensationFailed before a crash stays failed.
// ============================================================================
func TestSagaDurableCrash_MultiCrash_ResumeChain(t *testing.T) {
	cs := buildCrashSaga(987654321, 4, 3, true /*forceFail -> guarantees a CompensationFailed*/)
	store := newRBCrashStore(2, 4) // crash on the 2nd and 4th rollback saves across resumes
	crashes, resErr := cs.runToCompletion(t, store, 24)
	require.GreaterOrEqual(t, crashes, 1, "at least one crash fired")
	cs.assertDurableInvariants(t, store, resErr, crashes)
}

// ============================================================================
// ANGLE 4 (focused) — a compensation that FAILED (CompensationFailed, durable)
// before a crash still counts failed after the resume, reconstructed from the
// durable status, never re-attempted-clean; and its effect does NOT re-run.
// ============================================================================
func TestSagaDurableCrash_AfterCompensationFailed_StaysFailed(t *testing.T) {
	store := NewInMemoryStore()

	// Seed a mid-rollback snapshot: fail Failed (trigger), x already CompensationFailed
	// (a prior comp failure, durable), y still Completed with a comp pending.
	comp := func(context.Context, *WorkflowData) error { return nil }
	failing := 0
	b := NewWorkflowBuilder().WithWorkflowID("after-cf")
	b.AddNode("x").WithAction(benchNoopAction()).
		WithCompensation(func(context.Context, *WorkflowData) error { failing++; return errors.New("x re-attempted!") })
	b.AddNode("y").WithAction(benchNoopAction()).WithCompensation(comp).DependsOn("x")
	b.AddNode("fail").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).DependsOn("y")
	dag, err := b.Build()
	require.NoError(t, err)

	seed := NewWorkflowData("after-cf")
	seed.SetNodeStatus("x", CompensationFailed) // already durably failed before the crash
	seed.SetNodeStatus("y", Completed)          // undo still pending
	seed.SetNodeStatus("fail", Failed)
	seed.SetRollingBack(true)
	require.NoError(t, store.Save(seed))

	resErr := (&Workflow{DAG: dag, WorkflowID: "after-cf", Store: store}).Execute(context.Background())

	var se *SagaError
	require.ErrorAs(t, resErr, &se, "a run with a CompensationFailed node surfaces a *SagaError on resume")
	require.Equal(t, []string{"x"}, failedNames(se.FailedToCompensate), "the pre-crash CompensationFailed is reconstructed as failed, not re-attempted-clean")
	require.Equal(t, 0, failing, "the already-failed compensation must NOT re-run on resume (status was terminal)")
	require.Equal(t, []string{"y"}, se.Compensated, "the still-pending node is compensated on resume")

	got, loadErr := store.Load("after-cf")
	require.NoError(t, loadErr)
	assertStatus(t, got, "x", CompensationFailed)
	assertStatus(t, got, "y", Compensated)
}
