package workflow

// M10 phase-35 chunk-1 — ADVERSARIAL suite for the suspend/re-enter spine.
//
// These tests independently attack the suspend spine at the boundaries the
// author's own suite (suspend_test.go / suspend_executor_test.go /
// suspend_property_test.go) structurally misses: MULTIPLE suspend↔resume cycles
// (the existing property does exactly one park→wake), persistence round-trips
// across ALL THREE stores (the existing property uses only FlatBuffers), int64
// workflow DATA written across a park, the precedence corners (cancel/timeout vs
// park, no-checkpointer via Workflow.Execute), the static-topology guard against
// a park hidden inside a CompositeAction or behind middleware, and the
// graph-identity guard on a parked (Waiting) node. The concurrency attacks live
// in suspend_adversarial_concurrency_test.go and the N-cycle property in
// suspend_adversarial_property_test.go.
//
// Oracle: "suspend is a crash you chose" — a parked run must converge to the SAME
// final a never-suspended run reaches, with every node executing exactly once
// after wake; a non-suspend outcome (cancel/fail/config-error) must never leak the
// ErrSuspended sentinel nor persist a stray Waiting frontier; a non-declared node
// can never park.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adversarialStore pairs a store with a label for table-driven all-three-stores
// coverage. Each built-in store implements both WorkflowStore and Checkpointer.
type adversarialStore struct {
	name  string
	store WorkflowStore
}

func adversarialStores(t *testing.T) []adversarialStore {
	t.Helper()
	js, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	fb, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	return []adversarialStore{
		{"InMemory", NewInMemoryStore()},
		{"JSONFile", js},
		{"FlatBuffers", fb},
	}
}

// TestSuspend_Adversarial_MultiCycleConvergence is the contract-1 N-cycle attack
// over all three stores: a node parks, the run is re-entered while STILL parked
// (re-park), N times, and only then woken. The existing property parks exactly
// once; this asserts that N park→re-park cycles still converge with every node
// completing exactly once after wake — and that the already-Completed upstream is
// NEVER re-run across any of the N reloads (the memoization holds per-cycle, not
// just once). Run against JSONFile (UseNumber path) and FlatBuffers (the
// statusToFBStatus switches) as well as InMemory so the Waiting frontier is proven
// to survive every store's serialization on every cycle.
func TestSuspend_Adversarial_MultiCycleConvergence(t *testing.T) {
	const cycles = 5
	const id = "multicycle"

	for _, sc := range adversarialStores(t) {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			gate := newParkGate()
			counter := newExecCounter()
			build := func() *DAG {
				d := NewDAG(id)
				mustAddNode(t, d, countingNode("a", counter))
				mustAddNode(t, d, completingSuspendNode("park", gate, counter))
				mustAddNode(t, d, countingNode("c", counter))
				mustAddDep(t, d, "a", "park")
				mustAddDep(t, d, "park", "c")
				return d
			}

			// N re-entries while the gate stays parked: each must re-park.
			for i := 0; i < cycles; i++ {
				w := &Workflow{DAG: build(), WorkflowID: id, Store: sc.store}
				err := w.Execute(context.Background())
				require.ErrorIsf(t, err, ErrSuspended, "cycle %d must re-park", i)

				persisted, lerr := sc.store.Load(id)
				require.NoError(t, lerr, "cycle %d must have flushed a durable checkpoint", i)
				assertStatus(t, persisted, "a", Completed)
				assertStatus(t, persisted, "park", Waiting)
				assertStatus(t, persisted, "c", Pending)
				// Upstream output must survive every reload (FB stringifies; JSON
				// UseNumber; InMemory clones).
				out, ok := persisted.GetOutput("a")
				require.Truef(t, ok, "cycle %d: upstream output must persist", i)
				assert.Equalf(t, "out-a", out, "cycle %d: upstream output fidelity", i)
			}

			// The already-Completed upstream ran exactly once despite N reloads;
			// the parked node never completed; the downstream never ran.
			assert.Equal(t, 1, counter.get("a"), "upstream must not re-run across N cycles")
			assert.Equal(t, 0, counter.get("park"), "park has not completed while gate is parked")
			assert.Equal(t, 0, counter.get("c"), "downstream of a parked node must not run")

			// The event arrives; the re-entry converges.
			gate.wake()
			w := &Workflow{DAG: build(), WorkflowID: id, Store: sc.store}
			require.NoError(t, w.Execute(context.Background()), "re-entry after wake converges")

			final, lerr := sc.store.Load(id)
			require.NoError(t, lerr)
			assertStatus(t, final, "a", Completed)
			assertStatus(t, final, "park", Completed)
			assertStatus(t, final, "c", Completed)
			assert.Equal(t, 1, counter.get("a"), "upstream still exactly once after wake")
			assert.Equal(t, 1, counter.get("park"), "park completes exactly once after N re-parks")
			assert.Equal(t, 1, counter.get("c"), "downstream runs exactly once after wake")
		})
	}
}

// TestSuspend_Adversarial_SequentialParksAcrossLevels attacks two DISTINCT
// suspension nodes at DIFFERENT levels (pA at level 0, pB at level 1, pB depends
// on pA), woken one at a time. The existing coverage only ever has one park node.
// This proves: pA must not re-run when pB later parks (it is persisted Completed
// between the two suspensions), and the run converges only after BOTH events fire.
func TestSuspend_Adversarial_SequentialParksAcrossLevels(t *testing.T) {
	store := NewInMemoryStore()
	const id = "seq-parks"
	gateA := newParkGate()
	gateB := newParkGate()
	counter := newExecCounter()

	build := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, completingSuspendNode("pA", gateA, counter))
		mustAddNode(t, d, completingSuspendNode("pB", gateB, counter))
		mustAddDep(t, d, "pA", "pB")
		return d
	}

	// Run 1: pA parks at level 0; pB is never reached.
	w1 := &Workflow{DAG: build(), WorkflowID: id, Store: store}
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)
	p1, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p1, "pA", Waiting)
	assertStatus(t, p1, "pB", Pending)

	// Wake only pA. Run 2: pA completes, pB now parks at level 1.
	gateA.wake()
	w2 := &Workflow{DAG: build(), WorkflowID: id, Store: store}
	require.ErrorIs(t, w2.Execute(context.Background()), ErrSuspended)
	p2, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p2, "pA", Completed)
	assertStatus(t, p2, "pB", Waiting)
	assert.Equal(t, 1, counter.get("pA"), "pA completed exactly once")
	assert.Equal(t, 0, counter.get("pB"), "pB has not completed yet")

	// Wake pB. Run 3: converge; pA must NOT re-run.
	gateB.wake()
	w3 := &Workflow{DAG: build(), WorkflowID: id, Store: store}
	require.NoError(t, w3.Execute(context.Background()))
	p3, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p3, "pA", Completed)
	assertStatus(t, p3, "pB", Completed)
	assert.Equal(t, 1, counter.get("pA"), "pA must not re-run when pB wakes")
	assert.Equal(t, 1, counter.get("pB"), "pB completes exactly once")
}

// TestSuspend_Adversarial_Int64DataSurvivesParkResume is the contract-3 fidelity
// attack: an upstream node writes int64 KILLER magnitudes (the values a float64
// codec corrupts — the v0.7.1 first-CI-run saga) into workflow DATA before a
// downstream node parks. After park→checkpoint→Load→resume, every int64 must come
// back an int64 at full magnitude on every store. The existing checkpoint-fidelity
// property covers a static Save→Load; this exercises the value crossing the
// park→resume→re-execute boundary.
func TestSuspend_Adversarial_Int64DataSurvivesParkResume(t *testing.T) {
	killers := map[string]int64{
		"k_max": math.MaxInt64,
		"k_min": math.MinInt64,
		"k_53p": (1 << 53) + 1,
		"k_53n": -((1 << 53) + 1),
	}
	const id = "int64-susp"

	for _, sc := range adversarialStores(t) {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			gate := newParkGate()
			build := func() *DAG {
				d := NewDAG(id)
				writer := NewNode("writer", ActionFunc(func(_ context.Context, data *WorkflowData) error {
					for k, v := range killers {
						data.Set(k, v)
					}
					return nil
				}))
				mustAddNode(t, d, writer)
				mustAddNode(t, d, newSuspendingNode("park", gate))
				mustAddDep(t, d, "writer", "park")
				return d
			}

			w1 := &Workflow{DAG: build(), WorkflowID: id, Store: sc.store}
			require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)

			assertInt64s := func(stage string, data *WorkflowData) {
				for k, want := range killers {
					raw, ok := data.Get(k)
					require.Truef(t, ok, "%s: key %s missing", stage, k)
					got, isInt := raw.(int64)
					require.Truef(t, isInt, "%s: key %s came back %T, not int64", stage, k, raw)
					assert.Equalf(t, want, got, "%s: key %s magnitude", stage, k)
				}
			}

			parked, lerr := sc.store.Load(id)
			require.NoError(t, lerr)
			assertInt64s("parked", parked)

			gate.wake()
			w2 := &Workflow{DAG: build(), WorkflowID: id, Store: sc.store}
			require.NoError(t, w2.Execute(context.Background()))
			final, lerr := sc.store.Load(id)
			require.NoError(t, lerr)
			assertInt64s("resumed", final)
			assertStatus(t, final, "park", Completed)
		})
	}
}

// TestSuspend_Adversarial_CompositeHidingSuspendDoesNotPark is the contract-5
// static-topology guard: a CompositeAction wrapping a suspending action is NOT a
// declared suspension node (the marker is on the composite's type, which lacks
// suspendable()). An ErrSuspended bubbling out of it must be FAILED loudly and the
// sentinel must NOT escape — both at node.Execute and end-to-end through
// Workflow.Execute (where it must surface as a failure, never ErrSuspended).
func TestSuspend_Adversarial_CompositeHidingSuspendDoesNotPark(t *testing.T) {
	t.Run("node.Execute fails loudly", func(t *testing.T) {
		gate := newParkGate()
		composite := NewCompositeAction(&suspendingAction{gate: gate})
		node := NewNode("composite", composite)
		data := NewWorkflowData("wf")

		err := node.Execute(context.Background(), data)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrSuspended),
			"a park hidden inside a CompositeAction must not escape as the suspend sentinel")
		st, _ := data.GetNodeStatus("composite")
		assert.Equal(t, Failed, st)
	})

	t.Run("Workflow.Execute surfaces a failure, not a park", func(t *testing.T) {
		store := NewInMemoryStore()
		const id = "composite-wf"
		gate := newParkGate()
		d := NewDAG(id)
		mustAddNode(t, d, NewNode("composite", NewCompositeAction(&suspendingAction{gate: gate})))

		w := &Workflow{DAG: d, WorkflowID: id, Store: store}
		err := w.Execute(context.Background())
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrSuspended), "a hidden park must not suspend the run")
		var execErr *ExecutionError
		assert.True(t, errors.As(err, &execErr), "it is a real failure (*ExecutionError)")
		persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
		assertStatus(t, persisted, "composite", Failed)
	})
}

// TestSuspend_Adversarial_MiddlewareWrappedSuspendDoesNotPark is the companion
// contract-5 guard for the F-note forward constraint: wrapping a suspending action
// in middleware re-wraps it in an ActionFunc, hiding the marker, so the node is
// NOT a declared suspension node and must NOT park. A pass-through middleware that
// faithfully propagates the sentinel is the worst case — node.Execute must still
// fail loudly rather than honor the park.
func TestSuspend_Adversarial_MiddlewareWrappedSuspendDoesNotPark(t *testing.T) {
	gate := newParkGate()
	passthrough := Middleware(func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			return next.Execute(ctx, data)
		})
	})
	wrapped := passthrough(&suspendingAction{gate: gate})
	// Sanity: the wrapper has erased the marker.
	_, stillMarked := wrapped.(suspendableAction)
	require.False(t, stillMarked, "middleware must hide the suspendableAction marker")

	node := NewNode("mw", wrapped)
	data := NewWorkflowData("wf")
	err := node.Execute(context.Background(), data)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrSuspended),
		"a middleware-wrapped suspend action must not park (marker hidden)")
	st, _ := data.GetNodeStatus("mw")
	assert.Equal(t, Failed, st)
}

// TestSuspend_Adversarial_NonCheckpointerViaWorkflowExecute is the contract-6
// attack via the public surface: a Store that does NOT implement Checkpointer,
// driven through Workflow.Execute (not raw DAG.Execute), must refuse a park with
// ErrSuspendRequiresCheckpointer and NEVER the silently-non-durable ErrSuspended.
//
// It ALSO probes the F1 invariant on this path: a run that did NOT suspend (it
// returned a configuration error) must not persist a stray Waiting frontier — the
// same rule clearParked enforces on the cancel and fail-fast return paths. The
// final Save in Workflow.Execute persists whatever status the parked node carries;
// the ErrSuspendRequiresCheckpointer return in DAG.Execute does NOT clearParked, so
// this asserts the expected (F1-consistent) outcome and FAILS if a Waiting leaks.
func TestSuspend_Adversarial_NonCheckpointerViaWorkflowExecute(t *testing.T) {
	st := &nonCheckpointerStore{inner: NewInMemoryStore()}
	_, isCP := WorkflowStore(st).(Checkpointer)
	require.False(t, isCP, "the test store must not implement Checkpointer")

	const id = "nocp-wf"
	gate := newParkGate()
	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))

	w := &Workflow{DAG: d, WorkflowID: id, Store: st}
	err := w.Execute(context.Background())

	require.ErrorIs(t, err, ErrSuspendRequiresCheckpointer,
		"a park with no Checkpointer wired must be a config error")
	require.False(t, errors.Is(err, ErrSuspended),
		"a non-durable park must never surface as the success suspend arm")

	persisted, lerr := st.Load(id)
	require.NoError(t, lerr)
	got, ok := persisted.GetNodeStatus("park")
	require.True(t, ok)
	assert.NotEqualf(t, Waiting, got,
		"a run that returned a config error (did NOT suspend) must not persist a stray Waiting frontier (F1 invariant); got %q", got)
}

// TestSuspend_Adversarial_GraphIdentityRejectsRenamedParkNode is the contract-7
// attack: park a real run (persisting a Waiting node), then resume against a DAG in
// which the parked node has been RENAMED. The graph-identity guard must reject the
// resume loudly (ErrValidation naming the missing node) rather than mis-resume —
// and the renamed node must never execute. The existing graph-identity test seeds
// a Completed ghost; this drives a genuine park→rename→resume.
func TestSuspend_Adversarial_GraphIdentityRejectsRenamedParkNode(t *testing.T) {
	store := NewInMemoryStore()
	const id = "graph-park"
	gate := newParkGate()
	counter := newExecCounter()

	build1 := func() *DAG {
		d := NewDAG(id)
		mustAddNode(t, d, countingNode("a", counter))
		mustAddNode(t, d, completingSuspendNode("park", gate, counter))
		mustAddDep(t, d, "a", "park")
		return d
	}
	w1 := &Workflow{DAG: build1(), WorkflowID: id, Store: store}
	require.ErrorIs(t, w1.Execute(context.Background()), ErrSuspended)
	p1, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	assertStatus(t, p1, "park", Waiting)

	// Resume against a graph where "park" was renamed to "park2".
	d2 := NewDAG(id)
	mustAddNode(t, d2, countingNode("a", counter))
	mustAddNode(t, d2, completingSuspendNode("park2", gate, counter))
	mustAddDep(t, d2, "a", "park2")

	gate.wake() // even with the event fired, the resume must be rejected on identity
	w2 := &Workflow{DAG: d2, WorkflowID: id, Store: store}
	err := w2.Execute(context.Background())
	require.Error(t, err, "resume against a renamed parked node must be rejected")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Contains(t, err.Error(), "park", "the rejection must name the missing node")
	assert.Equal(t, 0, counter.get("park2"), "the renamed node must not execute on a rejected resume")
}

// TestSuspend_Adversarial_PreCancelledContextDoesNotPark is a contract-4 precedence
// corner: a context already cancelled before Execute must stop the run with a
// cancellation error BEFORE the parkable level runs — never a park. The park action
// must not even be invoked, and no Waiting may be persisted.
func TestSuspend_Adversarial_PreCancelledContextDoesNotPark(t *testing.T) {
	store := NewInMemoryStore()
	const id = "precancel"
	gate := newParkGate()
	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Execute

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(ctx)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "the run stops with the cancellation")
	assert.False(t, errors.Is(err, ErrSuspended), "a cancelled run must not suspend")
	parkRuns, _ := gate.stats()
	assert.Equal(t, 0, parkRuns, "the park action must not run when the ctx is pre-cancelled")
	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	got, _ := persisted.GetNodeStatus("park")
	assert.NotEqual(t, Waiting, got, "a cancelled run must not persist a Waiting frontier")
}

// TestSuspend_Adversarial_TimeoutDuringParkLevelClearsWaiting is a contract-4
// precedence corner combining a TIMEOUT firing in the same level as a park.
// A park node (returns immediately) shares level 0 with a sibling that blocks
// until the context deadline. When the deadline fires, cancellation OUTRANKS the
// park: the run returns a deadline error (never ErrSuspended, never *ExecutionError)
// and the parked node must be cleared off Waiting (clearParked on the cancel path).
func TestSuspend_Adversarial_TimeoutDuringParkLevelClearsWaiting(t *testing.T) {
	store := NewInMemoryStore()
	const id = "timeout-park"
	gate := newParkGate()

	d := NewDAG(id)
	mustAddNode(t, d, newSuspendingNode("park", gate))
	mustAddNode(t, d, NewNode("slow", ActionFunc(func(ctx context.Context, _ *WorkflowData) error {
		<-ctx.Done() // block until the run's deadline fires
		return ctx.Err()
	})))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	w := &Workflow{DAG: d, WorkflowID: id, Store: store}
	err := w.Execute(ctx)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the deadline is the stop reason")
	assert.False(t, errors.Is(err, ErrSuspended), "a timed-out run must not suspend")
	var execErr *ExecutionError
	assert.False(t, errors.As(err, &execErr), "cancellation outranks any incidental node failure")

	persisted, _ := store.Load(id) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	got, _ := persisted.GetNodeStatus("park")
	assert.NotEqualf(t, Waiting, got,
		"a timed-out run must not persist a stray Waiting frontier (clearParked); got %q", got)
}
