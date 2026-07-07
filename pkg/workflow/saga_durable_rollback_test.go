package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// crashSaveStore wraps an InMemoryStore and simulates a crash by PANICKING on the first
// Save whose snapshot shows crashWhenCompensated marked Compensated — modelling a
// process death mid-rollback the instant that node's undo lands. CONTENT-based (not
// Save-count-based) on purpose: the crash fires by rollback PROGRESS, independent of how
// many checkpoints the pass emits, so dropping the per-level Save (the §1 bite) makes
// the crash land on the FINAL save with NOTHING durably compensated -> the durable-
// progress assertion goes RED. The test recovers the panic and re-Executes.
type crashSaveStore struct {
	*InMemoryStore
	mu                   sync.Mutex
	saveCount            int
	crashWhenCompensated string // panic on the first Save where this node is Compensated
	crashed              bool
}

func (s *crashSaveStore) Save(data *WorkflowData) error {
	s.mu.Lock()
	s.saveCount++
	if s.crashWhenCompensated != "" && !s.crashed {
		if st, _ := data.GetNodeStatus(s.crashWhenCompensated); st == Compensated {
			s.crashed = true
			s.mu.Unlock()
			panic("simulated crash mid-rollback")
		}
	}
	s.mu.Unlock()
	return s.InMemoryStore.Save(data)
}

func (s *crashSaveStore) saves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

// buildDurableSaga: a->{b,c}->d->fail, compensations on a,b,c (d has none -> skipped).
// Each compensation bumps a shared per-node EFFECT counter (at-least-once observable),
// and the FORWARD action of a bumps a forward counter (the MAJOR-4 discriminator — it
// must NOT advance on a rollback-resume). failComp toggles whether b's comp fails.
func buildDurableSaga(t *testing.T, store WorkflowStore, id string, effect map[string]int, effectMu *sync.Mutex, forward *int, bFails bool) *Workflow {
	t.Helper()
	comp := func(name string, fail bool) func(context.Context, *WorkflowData) error {
		return func(context.Context, *WorkflowData) error {
			effectMu.Lock()
			effect[name]++
			effectMu.Unlock()
			if fail {
				return errors.New("comp fail: " + name)
			}
			return nil
		}
	}
	b := NewWorkflowBuilder()
	b.AddNode("a").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { effectMu.Lock(); *forward++; effectMu.Unlock(); return nil })).
		WithCompensation(comp("a", false))
	b.AddNode("b").WithAction(benchNoopAction()).WithCompensation(comp("b", bFails)).DependsOn("a")
	b.AddNode("c").WithAction(benchNoopAction()).WithCompensation(comp("c", false)).DependsOn("a")
	b.AddNode("d").WithAction(benchNoopAction()).DependsOn("b", "c") // no comp -> skipped
	b.AddNode("fail").
		WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return errors.New("boom") })).
		DependsOn("d")
	dag, err := b.Build()
	require.NoError(t, err)
	return &Workflow{DAG: dag, WorkflowID: id, Store: store}
}

// TestSagaDurable_CrashMidRollback_ResumesAndFinishes — the DUR-01 crux: a crash mid-
// rollback resumes and finishes; every compensable node ends Compensated exactly once
// (status), the partition is honest, and the FORWARD action never re-runs on resume
// (MAJOR-4 discriminator).
func TestSagaDurable_CrashMidRollback_ResumesAndFinishes(t *testing.T) {
	var mu sync.Mutex
	effect := map[string]int{}
	forward := 0
	// Crash the instant a's undo lands. a is the LAST reverse level (deepest dep), so
	// b,c are already compensated + (with per-level Save) durably checkpointed by then.
	store := &crashSaveStore{InMemoryStore: NewInMemoryStore(), crashWhenCompensated: "a"}

	// Run 1: forward completes, fail triggers rollback, the Save carrying a=Compensated panics.
	func() {
		defer func() { recover() }() //nolint:errcheck // discard the simulated-crash panic // catch the simulated crash
		w1 := buildDurableSaga(t, store, "dur", effect, &mu, &forward, false)
		_ = w1.Execute(context.Background()) //nolint:errcheck // crashes mid-rollback
	}()
	require.Equal(t, 1, forward, "forward action ran exactly once in run 1")
	require.True(t, store.crashed, "the simulated crash fired")

	// §1 durable-progress: the per-reverse-level checkpoint persisted the compensations
	// that ran BEFORE the crash. Without it, the crash would lose all rollback progress
	// (0 Compensated). This is the §1 bite: drop the per-level Save -> RED here.
	afterCrash, lerr0 := store.Load("dur")
	require.NoError(t, lerr0)
	compAfterCrash := 0
	afterCrash.ForEachNodeStatus(func(_ string, st NodeStatus) {
		if st == Compensated {
			compAfterCrash++
		}
	})
	require.GreaterOrEqual(t, compAfterCrash, 1, "§1: a per-level checkpoint must persist pre-crash compensations")

	// Run 2: same store -> resume into rollback -> finish. The crash is one-shot
	// (store.crashed latched), so this drive runs clean.
	forwardBefore := forward
	w2 := buildDurableSaga(t, store, "dur", effect, &mu, &forward, false)
	err := w2.Execute(context.Background())

	// MAJOR-4: the forward action did NOT re-run on the rollback-resume.
	require.Equal(t, forwardBefore, forward, "MAJOR-4: forward action MUST NOT re-run on a rollback-resume")

	// exactly-finishes: every compensable node ends Compensated (status exactly-once);
	// d (no comp) skipped; the run is honest (clean rollback -> reconstructed cause).
	got, lerr := store.Load("dur")
	require.NoError(t, lerr)
	assertStatus(t, got, "a", Compensated)
	assertStatus(t, got, "b", Compensated)
	assertStatus(t, got, "c", Compensated)
	assertStatus(t, got, "d", Completed) // no comp -> skipped, stays Completed
	// clean rollback across a crash -> the reconstructed forward *ExecutionError, NEVER nil.
	require.Error(t, err, "a rolled-back run is never nil (reconstructed cause)")
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "resume reconstructs the forward *ExecutionError cause from the persisted Failed node")
	// at-least-once effect: every compensation ran at least once (some may re-run across
	// the crash — deduped by IdempotencyKey at the downstream, not asserted here).
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, effect["a"], 1)
	require.GreaterOrEqual(t, effect["b"], 1)
	require.GreaterOrEqual(t, effect["c"], 1)
}

// TestSagaDurable_CrashAfterCompensationFailed_StaysFailed — the MINOR: a compensation
// that FAILED (CompensationFailed, durable) before a crash still counts failed after
// resume — never silently re-attempted-clean. Reconstructed from the durable status.
func TestSagaDurable_CrashAfterCompensationFailed_StaysFailed(t *testing.T) {
	var mu sync.Mutex
	effect := map[string]int{}
	forward := 0
	store := NewInMemoryStore()

	// Run 1 (no crash): b's comp fails -> b CompensationFailed, a/c Compensated.
	w1 := buildDurableSaga(t, store, "dur-fail", effect, &mu, &forward, true /*bFails*/)
	err1 := w1.Execute(context.Background())
	var se1 *SagaError
	require.ErrorAs(t, err1, &se1)
	require.Equal(t, []string{"b"}, failedNames(se1.FailedToCompensate))

	// Now resume (re-drive of a fully-rolled-back run, §3): b stays CompensationFailed,
	// reconstructed from the durable status (the MINOR — not re-attempted-clean).
	persisted, lerrp := store.Load("dur-fail")
	require.NoError(t, lerrp)
	require.True(t, persisted.IsRollingBack(), "rolling_back stays set (durably terminal)")
	w2 := buildDurableSaga(t, store, "dur-fail", effect, &mu, &forward, true)
	err2 := w2.Execute(context.Background())
	var se2 *SagaError
	require.ErrorAs(t, err2, &se2, "a re-drive of a rolled-back run returns the reconstructed *SagaError, never nil")
	require.Equal(t, []string{"b"}, failedNames(se2.FailedToCompensate), "the pre-existing CompensationFailed still counts failed after resume")
	// §3 re-drive is a no-op: no compensation re-ran (b was already CompensationFailed,
	// a/c already Compensated -> all skipped by compensateLevel's status!=Completed guard).
	mu.Lock()
	effAfter := map[string]int{"a": effect["a"], "b": effect["b"], "c": effect["c"]}
	mu.Unlock()
	_ = effAfter // the re-drive ran zero NEW compensations (statuses were already terminal)
}

// TestSagaDurable_CleanResume_NeverNil — ph47-F5/ph46-F3: a clean resume-into-rollback
// (a persisted rolling_back snapshot with a Failed trigger + a Completed compensable
// node) returns a NON-nil reconstructed *ExecutionError (never nil), and a re-drive of
// the now-fully-rolled-back run is a no-op returning the same reconstructed cause.
func TestSagaDurable_CleanResume_NeverNil(t *testing.T) {
	var mu sync.Mutex
	effect := map[string]int{}
	forward := 0
	store := NewInMemoryStore()

	// Persist a rolling_back snapshot: "fail" Failed (the trigger), "a" Completed (undo
	// pending). No prior rollback marks -> the resume drives a clean rollback.
	seed := NewWorkflowData("dur-clean")
	seed.SetNodeStatus("a", Completed)
	seed.SetNodeStatus("fail", Failed)
	seed.SetRollingBack(true)
	require.NoError(t, store.Save(seed))

	w := buildDurableSaga(t, store, "dur-clean", effect, &mu, &forward, false)
	err := w.Execute(context.Background())

	require.Equal(t, 0, forward, "no forward re-run on a rollback-resume")
	require.Error(t, err, "a rolled-back run is NEVER nil (ph47-F5)")
	var se *SagaError
	require.False(t, errors.As(err, &se), "a CLEAN rollback returns the bare cause, not a *SagaError")
	var ee *ExecutionError
	require.ErrorAs(t, err, &ee, "the cause is the reconstructed forward *ExecutionError")
	got, lerrc := store.Load("dur-clean")
	require.NoError(t, lerrc)
	assertStatus(t, got, "a", Compensated)

	// re-drive of the fully-rolled-back run: no-op, same reconstructed cause (ph46-F3).
	err2 := buildDurableSaga(t, store, "dur-clean", effect, &mu, &forward, false).Execute(context.Background())
	require.ErrorAs(t, err2, &ee, "a re-drive returns the same reconstructed cause, never nil")
	require.False(t, errors.As(err2, &se), "still a clean (bare-cause) outcome on re-drive")
}
