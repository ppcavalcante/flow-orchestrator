package workflow

// M10 ph37 WaitForConditionNode ("await") ADVERSARIAL suite (independent of the
// author's tests). The author's signal_test.go covers exactly one condition case:
// park-while-false → set the key across a re-drive → converge. This file attacks
// the gaps that structurally misses, against the hard bar (no panic, no hang, no
// corruption, no busy-loop):
//
//   - predicate reading ABSENT keys: a defined park, never a panic (totality)
//   - predicate satisfied by an UPSTREAM node in the SAME run: converges in one
//     Execute (the in-run-flip case the cross-drive author test never reaches)
//   - predicate NEVER true: parks forever as a CLEAN REST — each drive evaluates
//     the predicate exactly once (no busy-loop within a drive), persists Waiting,
//     and the snapshot is byte-stable across repeated re-parks (no accumulation)
//   - predicate that PANICS: documents that the engine does NOT recover an action/
//     predicate panic (a totality observation, demonstrated safely at the action
//     boundary so it cannot abort the test binary)
//
// Oracle for each is stated inline.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdvCond_AbsentKeys_ParksCleanly: a predicate that reads keys never set must
// observe (nil,false), return false, and park — never panic on the missing data.
// Repeated drives keep re-parking cleanly.
//
// Oracle (totality): a predicate over absent data is a defined park, not a crash.
func TestAdvCond_AbsentKeys_ParksCleanly(t *testing.T) {
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-cond-absent"
	require.NoError(t, w.AddNode(NewWaitForConditionNode("await", func(d *WorkflowData) bool {
		// Reads several keys that were never written; must not panic.
		a, aok := d.Get("missing-a")
		b, bok := d.Get("missing-b")
		out, ook := d.GetOutput("never-ran")
		return aok && bok && ook && a == b && out != nil
	})))

	for i := 0; i < 5; i++ {
		err := w.Execute(context.Background())
		require.ErrorIs(t, err, ErrSuspended, "predicate over absent keys parks cleanly (drive %d)", i)
	}
	p, _ := store.Load("wf-cond-absent") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := p.GetNodeStatus("await")
	assert.Equal(t, Waiting, st, "the node stays Waiting across repeated re-parks")
}

// TestAdvCond_UpstreamSetsKey_SameRunConverges: a condition node downstream of a
// node that SETS the awaited key. The predicate becomes true within the SAME
// Execute (the upstream level ran first), so the run converges in ONE drive with no
// park at all — the in-run-flip case the author's cross-drive test never reaches.
//
// Oracle (data visibility): a condition sees data written by an already-completed
// upstream node in the same run.
func TestAdvCond_UpstreamSetsKey_SameRunConverges(t *testing.T) {
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-cond-upstream"

	require.NoError(t, w.AddNode(NewNode("setter", ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("ready", true)
		return nil
	}))))
	require.NoError(t, w.AddNode(NewWaitForConditionNode("await", func(d *WorkflowData) bool {
		v, ok := d.Get("ready")
		return ok && v == true
	})))
	require.NoError(t, w.AddDependency("setter", "await"))

	require.NoError(t, w.Execute(context.Background()),
		"the condition sees the upstream-set key and converges in one run (no park)")
	final, _ := store.Load("wf-cond-upstream") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("await")
	assert.Equal(t, Completed, st)
	stSetter, _ := final.GetNodeStatus("setter")
	assert.Equal(t, Completed, stSetter)
}

// TestAdvCond_NeverTrue_CleanRestNoBusyLoop is the "parks forever" totality bite.
// A predicate that is ALWAYS false must:
//   - park on every drive (ErrSuspended, node Waiting), never hang or error,
//   - evaluate the predicate EXACTLY ONCE per drive (no busy-loop / spin inside a
//     single Execute — the executor evaluates once and parks at the barrier), and
//   - leave the persisted snapshot BYTE-STABLE across repeated re-parks (a forever-
//     park accumulates no state, corrupts nothing).
//
// Oracle (clean rest): a never-satisfiable wait is a bounded, idempotent rest — the
// liveness analog of the M10 WakeReady design (a legitimately-resting Waiting node,
// not a stuck/spinning one).
func TestAdvCond_NeverTrue_CleanRestNoBusyLoop(t *testing.T) {
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-cond-never"

	var evals int
	require.NoError(t, w.AddNode(NewWaitForConditionNode("await", func(_ *WorkflowData) bool {
		evals++
		return false // never satisfiable
	})))

	const drives = 8
	var prevSnap []byte
	for i := 0; i < drives; i++ {
		evalsBefore := evals
		err := w.Execute(context.Background())
		require.ErrorIs(t, err, ErrSuspended, "a never-true predicate parks on every drive (drive %d)", i)

		// No busy-loop WITHIN a drive: the predicate is evaluated exactly once per
		// Execute (the executor checks once and parks, it does not spin).
		assert.Equal(t, 1, evals-evalsBefore,
			"the predicate is evaluated exactly once per drive — no busy-loop/spin (drive %d)", i)

		// The persisted snapshot is byte-stable across re-parks (no accumulation).
		p, lerr := store.Load("wf-cond-never")
		require.NoError(t, lerr)
		st, _ := p.GetNodeStatus("await")
		require.Equal(t, Waiting, st)
		snap, serr := p.Snapshot()
		require.NoError(t, serr)
		if i > 0 {
			assert.Equal(t, prevSnap, snap,
				"a forever-parked condition accumulates no state — snapshot byte-stable across re-parks (drive %d)", i)
		}
		prevSnap = snap
	}
	assert.Equal(t, drives, evals, "exactly one predicate evaluation per drive across all drives")
}

// TestAdvCond_PredicatePanics_NotRecoveredAtActionBoundary documents the totality
// boundary for a buggy predicate. The engine runs node actions in goroutines with
// NO recover() (verified: parallel_execution.go), so a panicking predicate (like any
// panicking Action) propagates and would crash the host process — it is NOT
// converted to a typed error. This is a consistent, engine-wide property (a
// panicking predicate is caller error), not a signal/condition-specific defect.
//
// We demonstrate it SAFELY at the action boundary — calling the action's Execute
// directly under a recover — rather than through Workflow.Execute (whose worker
// goroutine panic would abort the whole test binary). The assertion: the action
// does NOT internally recover, so callers must guarantee their predicates are total.
//
// Oracle (documented totality contract): waitForConditionAction.Execute surfaces a
// predicate panic to its caller; it adds no internal recover. If a future change
// DID add recovery (panic→typed error), this test flags the contract change.
func TestAdvCond_PredicatePanics_NotRecoveredAtActionBoundary(t *testing.T) {
	node := NewWaitForConditionNode("await", func(_ *WorkflowData) bool {
		panic("predicate blew up")
	})

	// Pull the action out of the node and call it directly under a recover so the
	// panic cannot escape this goroutine and abort the binary.
	act, ok := node.Action.(*waitForConditionAction)
	require.True(t, ok, "NewWaitForConditionNode wires a *waitForConditionAction directly")

	recovered := func() (didPanic bool) {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
			}
		}()
		_ = act.Execute(context.Background(), NewWorkflowData("wf-cond-panic")) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		return false
	}()

	assert.True(t, recovered,
		"the condition action does NOT internally recover a predicate panic — it propagates to the caller "+
			"(engine-wide property: actions run with no recover; a panicking predicate is caller error). "+
			"If recovery is ever added, update this contract.")
}

// TestAdvCond_PredicateValueTypeChange_NoPanic: a predicate that compares a key
// whose stored value can be of varying dynamic type (bool / string / int) must not
// panic on a type-assertion mismatch — it returns false and parks. Totality on the
// predicate's own robustness path through the engine (the engine passes whatever is
// stored; the predicate must tolerate it).
//
// Oracle (totality): heterogeneous stored values never crash the wait machinery.
func TestAdvCond_PredicateValueTypeChange_NoPanic(t *testing.T) {
	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-cond-types"
	require.NoError(t, w.AddNode(NewWaitForConditionNode("await", func(d *WorkflowData) bool {
		v, ok := d.Get("flag")
		if !ok {
			return false
		}
		b, isBool := v.(bool) // tolerant assertion — must not panic when v is not a bool
		return isBool && b
	})))

	// Park (no key).
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// Set the key to a WRONG type (string, not bool): predicate tolerates it, parks.
	data, _ := store.Load("wf-cond-types") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	data.Set("flag", "true-ish-string")
	require.NoError(t, store.Save(data))
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended,
		"a wrong-typed value is tolerated (predicate returns false), not a panic")

	// Set the RIGHT type/value: converges.
	data, _ = store.Load("wf-cond-types") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	data.Set("flag", true)
	require.NoError(t, store.Save(data))
	require.NoError(t, w.Execute(context.Background()))
	final, _ := store.Load("wf-cond-types") //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("await")
	assert.Equal(t, Completed, st)
}
