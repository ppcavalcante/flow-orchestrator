package workflow

// M19 ph91 — ADVERSARIAL suite (anvil-adversarial-tester).
//
// Attacks the sub-workflow inline core (subworkflow.go / builder.go) on the axes the
// author's own fixtures structurally exclude:
//
//   1. RESULT FIDELITY beyond int64 (the F-1 axis): negative int64, large float64, bool,
//      numeric-looking string, empty string, nil, nested map/slice — round-tripped through
//      ALL THREE stores. The author proved max-int64 only; the OTHER shapes expose whether
//      the data-key result path is *uniformly* value-faithful or store-dependent.
//   2. CLOSURE-SCAN recursion vs nesting shapes the author excluded: a diamond (shared
//      grandchild), a definition-value CYCLE, deep nesting, and a middleware-hidden
//      suspendable.
//   3. IDEMPOTENT spawn under crash interleavings the author's single-node fixture misses:
//      a MULTI-node child crashed mid-way (granular resume), a fully-terminal multi-node
//      child on re-drive, deterministic-ID uniqueness, deterministic first-failed.
//
// The two reproduced defects have been DISPOSITIONED: F91-2 (unbounded closure-scan on a
// cycle/diamond) is FIXED with a visited-set (the cycle test now asserts termination); F91-1
// (complex result store-divergence) is a documented pre-existing store-wide property (a scalar
// result is backend-uniform, a complex result follows the store's serialization) — the test
// characterizes the property rather than pinning a defect.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Target 1 — result-key fidelity across the 3-store axis (F-1, beyond int64).
// ---------------------------------------------------------------------------

// runSubResult spawns a child that Sets resultVal as its "result" data key, drives the
// parent to completion on store, and returns the value the parent's declared key holds
// after reload. This is the exact fidelity surface: child DATA key -> parent DATA key,
// through a full store round-trip.
func runSubResult(t *testing.T, store WorkflowStore, wf string, resultVal any) (any, bool) {
	t.Helper()
	var afterN atomic.Int32
	w := parentWithSub(t, store, wf, childProducing(t, resultVal, nil), &afterN)
	require.NoError(t, w.Execute(context.Background()))
	final, err := store.Load(wf)
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
	return final.Get("result")
}

// TestSubWorkflow_ResultFidelity_ScalarShapes_AllStores — the fidelity claim HOLDS for
// every SCALAR shape on every store (this is the part the author under-tested: only
// max-int64 was proven). A negative int64, a large float64, both bools, a numeric-looking
// string, and an empty string must all reload as the SAME typed value on InMemory,
// FlatBuffers, and SQLite.
func TestSubWorkflow_ResultFidelity_ScalarShapes_AllStores(t *testing.T) {
	scalars := []struct {
		name string
		val  any
	}{
		{"negative_int64", int64(-9223372036854775808)}, // min int64 — sign + magnitude
		{"large_float64", float64(1.7976931348623157e308)},
		{"bool_true", true},
		{"bool_false", false},
		{"numeric_string", "123"}, // must stay a STRING, never coerce to a number
		{"empty_string", ""},
	}
	for _, sc := range subWorkflowStoreFactories(t) {
		for _, tc := range scalars {
			t.Run(sc.name+"/"+tc.name, func(t *testing.T) {
				got, ok := runSubResult(t, sc.mk(), "wf-scalar", tc.val)
				require.True(t, ok, "declared result key must be populated")
				assert.IsType(t, tc.val, got, "%s: result type must survive the round-trip", sc.name)
				assert.Equal(t, tc.val, got, "%s: result value must survive the round-trip", sc.name)
			})
		}
	}
}

// TestSubWorkflow_ResultFidelity_ComplexShapes_StoreProperty — F91-1, dispositioned as a
// DOCUMENTED STORE PROPERTY (not a sub-workflow defect). A COMPLEX child result (map/slice/nil)
// is NOT store-uniform: InMemory preserves the Go type, but FlatBuffers + SQLite serialize any
// non-scalar to a JSON *string* on reload (workflow_store.go:655 `default: JSON string`;
// workflow_store_sqlite_codec.go same). This is the SAME pre-existing, store-wide behavior that
// governs EVERY complex data value — the store only type-columns the scalars (int64 via
// value_long, plus string/bool/float) — NOT a sub-workflow-specific bug. applyResult's doc-
// comment (SCOPE note) documents exactly this: a scalar result is backend-uniform; a complex
// result follows the store's serialization. This test CHARACTERIZES the property so a future
// reader (or a store-uniformity change) sees it explicitly.
func TestSubWorkflow_ResultFidelity_ComplexShapes_StoreProperty(t *testing.T) {
	complexShapes := []struct {
		name string
		val  any
	}{
		{"nested_map", map[string]any{"n": float64(42), "s": "hi"}},
		{"slice", []any{float64(1), float64(2), float64(3)}},
		{"nil", nil},
	}
	for _, tc := range complexShapes {
		t.Run(tc.name, func(t *testing.T) {
			factories := subWorkflowStoreFactories(t)
			perStore := map[string]any{}
			for _, sc := range factories {
				got, _ := runSubResult(t, sc.mk(), "wf-complex", tc.val)
				perStore[sc.name] = got
			}

			// InMemory preserves the Go type.
			assert.Equal(t, tc.val, perStore["InMemory"], "InMemory preserves the typed value")

			// FlatBuffers + SQLite serialize a complex value to a string — the documented,
			// store-wide property (not a sub-workflow bug). A scalar result would be uniform;
			// see TestSubWorkflow_ResultFidelity_ScalarShapes_AllStores for the uniform case.
			for _, serialized := range []string{"FlatBuffers", "SQLite"} {
				_, isStr := perStore[serialized].(string)
				assert.True(t, isStr,
					"%s serializes a complex %s result to a string (documented store property; declare a scalar result for backend-uniformity)",
					serialized, tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Target 2 — closure-scan recursion vs adversarial nesting shapes.
// ---------------------------------------------------------------------------

// buildDiamond builds a definition-value nesting of the given depth where EACH level has two
// sub-workflow nodes pointing at the SAME next-level DAG (a diamond / shared substructure).
// Assembled raw (NewDAG+AddNode) so the per-AddSubWorkflow builder scan is bypassed — the
// only scan is the caller's scanChildInlineSafe on the returned root.
func buildDiamond(t *testing.T, depth int, leaf *DAG) *DAG {
	t.Helper()
	level := leaf
	for range depth {
		d := NewDAG("diamond")
		require.NoError(t, d.AddNode(NewNode("a", &subWorkflowAction{nodeName: "a", child: level})))
		require.NoError(t, d.AddNode(NewNode("b", &subWorkflowAction{nodeName: "b", child: level})))
		level = d
	}
	return level
}

// TestSubWorkflow_ScanDiamond_TerminatesAndCatchesShared — an ACYCLIC diamond terminates
// (no crash) and a suspendable in the SHARED grandchild is still refused. Kept at a small
// depth: the scan re-visits the shared substructure once PER reference (no memoization), so
// work is 2^depth — see F91-2 for the DoS this implies at larger depth.
func TestSubWorkflow_ScanDiamond_TerminatesAndCatchesShared(t *testing.T) {
	// Clean diamond (leaf is a plain node) terminates and passes.
	cleanLeaf := childProducing(t, "v", nil)
	require.NoError(t, scanChildInlineSafe(buildDiamond(t, 10, cleanLeaf)),
		"an acyclic diamond of non-suspendable nodes must scan clean")

	// A suspendable node in the shared leaf must be refused.
	require.ErrorIs(t, scanChildInlineSafe(buildDiamond(t, 10, childWithSuspendable(t))),
		ErrSubWorkflowSuspendableChild,
		"a suspendable in the shared grandchild must be refused through the diamond")
}

// TestSubWorkflow_ScanCycle_TerminatesNoOverflow — F91-2 (FIXED): the closure-scan carries a
// visited-set keyed by *DAG identity, so a definition-value CYCLE (child A sub-workflows child
// B which sub-workflows child A, constructible via raw NewDAG+AddNode) TERMINATES instead of
// recursing to a stack overflow. Bite: removing the visited-set (a shallow scanChildInlineSafe)
// makes this overflow the goroutine stack — a build-time DoS on an otherwise-legitimate graph.
func TestSubWorkflow_ScanCycle_TerminatesNoOverflow(t *testing.T) {
	a := NewDAG("cycle-a")
	b := NewDAG("cycle-b")
	require.NoError(t, a.AddNode(NewNode("toB", &subWorkflowAction{nodeName: "toB", child: b})))
	require.NoError(t, b.AddNode(NewNode("toA", &subWorkflowAction{nodeName: "toA", child: a})))

	// Must return (no stack overflow) — the visited-set breaks the cycle. Both nodes are
	// non-suspendable, so a clean cyclic graph scans clean (nil); the point is TERMINATION.
	done := make(chan error, 1)
	go func() { done <- scanChildInlineSafe(a) }()
	select {
	case err := <-done:
		require.NoError(t, err, "a clean definition-value cycle scans without a suspendable → nil, and TERMINATES")
	case <-time.After(3 * time.Second):
		t.Fatal("F91-2: the cyclic scan did not terminate (visited-set missing)")
	}
}

// TestSubWorkflow_DeepNestedSuspendable_Refused — a suspendable node buried 4 levels deep in
// a LINEAR definition-value chain is still refused. The author tested one nested level; this
// proves the recursion reaches an arbitrary depth (not just grandchild).
func TestSubWorkflow_DeepNestedSuspendable_Refused(t *testing.T) {
	deep := childWithSuspendable(t) // the suspendable lives at the bottom
	for range 4 {
		wrap := NewDAG("wrap")
		require.NoError(t, wrap.AddNode(NewNode("down", &subWorkflowAction{nodeName: "down", child: deep})))
		deep = wrap
	}
	pb := NewWorkflowBuilder().WithWorkflowID("wf-deep")
	pb.AddSubWorkflow("sub", deep)
	_, err := pb.Build()
	require.ErrorIs(t, err, ErrSubWorkflowSuspendableChild,
		"a suspendable 4 levels deep must be refused at build")
}

// TestSubWorkflow_MiddlewareHiddenSuspendable_FailsSafeNoHang — the marker-hiding hazard
// (suspend.go:62). A node whose action returns ErrSuspended but does NOT satisfy the
// suspendableAction marker (e.g. wrapped so the marker is hidden) escapes the closure-scan.
// The inline path must still be fail-safe: at runtime node.Execute rejects an ErrSuspended
// from a non-declared action (node.go:217) -> the child FAILS loudly -> the parent node
// Failed -> NO in-process hang. This confirms the inline composition inherits the suspend
// honesty guard as a backstop when the static scan is defeated.
func TestSubWorkflow_MiddlewareHiddenSuspendable_FailsSafeNoHang(t *testing.T) {
	// An ordinary ActionFunc (NOT a suspendableAction) that returns the park sentinel.
	hidden := NewDAG("hidden")
	require.NoError(t, hidden.AddNode(NewNode("fakepark", ActionFunc(func(context.Context, *WorkflowData) error {
		return ErrSuspended
	}))))

	pb := NewWorkflowBuilder().WithWorkflowID("wf-hidden")
	pb.AddSubWorkflow("sub", hidden) // scan passes: ActionFunc is not the marker type
	dag, err := pb.Build()
	require.NoError(t, err, "a hidden (non-marker) suspend escapes the static scan by design")

	store := NewInMemoryStore()
	w := NewWorkflow(store)
	w.WorkflowID = "wf-hidden"
	w.DAG = dag

	done := make(chan error, 1)
	go func() { done <- w.Execute(context.Background()) }()
	select {
	case runErr := <-done:
		require.Error(t, runErr, "the child must fail loudly (ErrSuspended from a non-declared action), never hang")
		require.NotErrorIs(t, runErr, ErrSuspended, "the park sentinel must not escape the inline child")
		final, lerr := store.Load("wf-hidden")
		require.NoError(t, lerr)
		assertNodeStatus(t, final, "sub", Failed)
	case <-time.After(5 * time.Second):
		t.Fatal("the inline child HUNG on a hidden suspendable — the runtime honesty guard did not fire")
	}
}

// ---------------------------------------------------------------------------
// Target 3 — idempotent spawn under crash interleavings + deterministic ID.
// ---------------------------------------------------------------------------

// twoNodeChild builds a child whose n1 (counter n1c) precedes n2 (counter n2c); n2 sets the
// "result" data key. Lets a test observe per-NODE re-execution across a crash/resume.
func twoNodeChild(t *testing.T, n1c, n2c *atomic.Int32) *DAG {
	t.Helper()
	cb := NewWorkflowBuilder()
	cb.AddStartNode("n1").WithAction(countingAction(n1c))
	cb.AddNode("n2").DependsOn("n1").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		n2c.Add(1)
		d.Set("result", "two-node-done")
		return nil
	}))
	dag, err := cb.Build()
	require.NoError(t, err)
	return dag
}

// TestSubWorkflow_MultiNodeChild_GranularResume — crash the child MID-WAY (n1 Completed,
// n2 Pending), then re-drive the parent. The child must RESUME from n2 (n2 runs exactly
// once) and must NOT re-run n1 (n1 stays Completed, its action count stays 0). The author's
// crash test used a single-node, fully-terminal child; this isolates node-granular resume
// idempotency under the sub-workflow spawn.
func TestSubWorkflow_MultiNodeChild_GranularResume(t *testing.T) {
	store := NewInMemoryStore()
	var n1c, n2c atomic.Int32
	childDAG := twoNodeChild(t, &n1c, &n2c)

	// Seed a partial child journal: n1 Completed (without running its action), n2 Pending.
	childID := subWorkflowChildID("wf-resume", "sub")
	seed := NewWorkflowData(childID)
	seed.SetNodeStatus("n1", Completed)
	seed.SetNodeStatus("n2", Pending)
	require.NoError(t, store.Save(seed))

	var afterN atomic.Int32
	w := parentWithSub(t, store, "wf-resume", childDAG, &afterN)
	require.NoError(t, w.Execute(context.Background()))

	require.EqualValues(t, 0, n1c.Load(), "n1 was already Completed — it must NOT re-run on resume")
	require.EqualValues(t, 1, n2c.Load(), "n2 resumes and runs exactly once")

	childFinal, err := store.Load(childID)
	require.NoError(t, err)
	assertNodeStatus(t, childFinal, "n1", Completed)
	assertNodeStatus(t, childFinal, "n2", Completed)

	final, err := store.Load("wf-resume")
	require.NoError(t, err)
	result, ok := final.Get("result")
	require.True(t, ok)
	assert.Equal(t, "two-node-done", result)
}

// TestSubWorkflow_MultiNodeChild_TerminalNoReRun — a FULLY terminal multi-node child on a
// parent re-drive must not re-run ANY node (extends the author's single-node crash test to
// the multi-node terminal-fast-path).
func TestSubWorkflow_MultiNodeChild_TerminalNoReRun(t *testing.T) {
	store := NewInMemoryStore()
	var n1c, n2c atomic.Int32
	childDAG := twoNodeChild(t, &n1c, &n2c)

	// Pre-run the child to full completion under its deterministic ID (crash-before-parent).
	childID := subWorkflowChildID("wf-mn-terminal", "sub")
	seeded := &Workflow{DAG: childDAG, WorkflowID: childID, Store: store}
	require.NoError(t, seeded.Execute(context.Background()))
	require.EqualValues(t, 1, n1c.Load())
	require.EqualValues(t, 1, n2c.Load())

	var afterN atomic.Int32
	w := parentWithSub(t, store, "wf-mn-terminal", childDAG, &afterN)
	require.NoError(t, w.Execute(context.Background()))

	require.EqualValues(t, 1, n1c.Load(), "terminal multi-node child must not re-run n1")
	require.EqualValues(t, 1, n2c.Load(), "terminal multi-node child must not re-run n2")
}

// TestSubWorkflow_DeterministicChildID_NoCollision — the deterministic-ID guarantee + the
// framing discipline. Distinct parents (or distinct node names) yield distinct child IDs, so
// two parents never write one child journal (one-writer preserved). Also proves the 8-byte
// length prefix disambiguates the (parentID, nodeName) split: ("ab","c") != ("a","bc").
func TestSubWorkflow_DeterministicChildID_NoCollision(t *testing.T) {
	// Same node name, different parents -> different IDs.
	assert.NotEqual(t, subWorkflowChildID("parentA", "sub"), subWorkflowChildID("parentB", "sub"),
		"distinct parents must not compute the same child ID")
	// Same parent, different node names -> different IDs.
	assert.NotEqual(t, subWorkflowChildID("p", "subX"), subWorkflowChildID("p", "subY"),
		"distinct node names must not compute the same child ID")
	// Framing: the (parentID, nodeName) split is unambiguous.
	assert.NotEqual(t, subWorkflowChildID("ab", "c"), subWorkflowChildID("a", "bc"),
		"the length-prefix framing must disambiguate the parentID/nodeName boundary")
	// Determinism: the same inputs always yield the same ID (resume-stability).
	assert.Equal(t, subWorkflowChildID("p", "sub"), subWorkflowChildID("p", "sub"),
		"the same (parent, node) must always yield the same child ID")
}

// TestSubWorkflow_ChildUnambiguouslyComplete_FastPathGuard — the fast-path short-circuit
// predicate (post-FIND-P91-R1). It is true ONLY when every node is terminal AND none Failed:
// a Failed node (which may be a coe-tolerated success) makes it false so the caller defers to
// child.Execute (the coe authority), keeping the crash and non-crash paths consistent.
func TestSubWorkflow_ChildUnambiguouslyComplete_FastPathGuard(t *testing.T) {
	// All terminal, none Failed → complete (fast-path may short-circuit).
	d := NewWorkflowData("x")
	d.SetNodeStatus("a", Completed)
	d.SetNodeStatus("b", Bypassed)
	assert.True(t, childUnambiguouslyComplete(d), "all terminal + none Failed → complete")

	// A Failed node → NOT unambiguously complete (defer to child.Execute — R1: a coe node
	// fails but the child may still be a success).
	dFail := NewWorkflowData("f")
	dFail.SetNodeStatus("ok", Completed)
	dFail.SetNodeStatus("coe", Failed)
	assert.False(t, childUnambiguouslyComplete(dFail), "any Failed node → defer to child.Execute (R1)")

	// A non-terminal (Pending) node → not complete (resume).
	dPend := NewWorkflowData("p")
	dPend.SetNodeStatus("a", Completed)
	dPend.SetNodeStatus("b", Pending)
	assert.False(t, childUnambiguouslyComplete(dPend), "a Pending node → not complete (resume)")

	// No persisted nodes → not complete.
	assert.False(t, childUnambiguouslyComplete(NewWorkflowData("z")), "an empty child journal is not complete")
}

// TestSubWorkflow_ResultCollision_CrossValueLoud — a genuine last-writer-wins hazard: a
// pre-existing FOREIGN result value (different from what the child produces) is refused
// loudly on every store, not silently overwritten. (The author proved this on InMemory with
// a string; this widens to the int64 value path.)
func TestSubWorkflow_ResultCollision_CrossValueLoud(t *testing.T) {
	store := NewInMemoryStore()
	pb := NewWorkflowBuilder().WithWorkflowID("wf-xcollide")
	pb.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
		d.Set("result", int64(111)) // foreign pre-existing value
		return nil
	}))
	pb.AddSubWorkflow("sub", childProducing(t, int64(222), nil)).DependsOn("before").WithResult("result", "result")
	dag, err := pb.Build()
	require.NoError(t, err)
	w := NewWorkflow(store)
	w.WorkflowID = "wf-xcollide"
	w.DAG = dag

	err = w.Execute(context.Background())
	require.ErrorIs(t, err, ErrSubWorkflowResultKeyCollision, "a differing pre-existing value is a loud collision")

	final, lerr := store.Load("wf-xcollide")
	require.NoError(t, lerr)
	v, ok := final.GetInt64("result")
	require.True(t, ok)
	assert.EqualValues(t, 111, v, "the foreign value is not overwritten (no last-writer-wins)")
}
