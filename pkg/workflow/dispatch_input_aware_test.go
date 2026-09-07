package workflow

// M25 input-aware registered DAG factories — Phase B witnesses (W1/W3/W4/W5). RegisterWithInput lets
// one registered type build different validated per-run graphs from the queued input, through the SAME
// capped/fenced dispatch lifecycle. All witnesses run over real SQLite dispatch (mkDispatchStore).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// oneNode builds a trivial single-node DAG whose action runs `fn`.
func oneNode(t *testing.T, name string, fn func(*WorkflowData) error) *DAG {
	t.Helper()
	b := NewWorkflowBuilder()
	b.AddStartNode(name).WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error { return fn(d) }))
	dag, err := b.Build()
	require.NoError(t, err)
	return dag
}

// W4 — legacy + mixed registry: an existing zero-arg factory runs unchanged alongside an input-aware
// type; a cross-method duplicate refuses without replacing the original; an unregistered row stays pending.
func TestInputAware_W4_LegacyAndMixedRegistry(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	var legacyRan, awareRan atomic.Bool
	require.NoError(t, reg.Register("legacy", func() (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { legacyRan.Store(true); return nil }), nil
	}))
	require.NoError(t, reg.RegisterWithInput("aware", func(_ []byte) (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { awareRan.Store(true); return nil }), nil
	}))

	// Cross-method duplicates refuse and do NOT replace the original entry.
	require.ErrorIs(t, reg.RegisterWithInput("legacy", func([]byte) (*DAG, error) { return nil, nil }), ErrValidation,
		"an input-aware dup of a legacy type is refused")
	require.ErrorIs(t, reg.Register("aware", func() (*DAG, error) { return nil, nil }), ErrValidation,
		"a legacy dup of an input-aware type is refused")

	_, err := s.Enqueue("w-legacy", "legacy", nil)
	require.NoError(t, err)
	_, err = s.Enqueue("w-aware", "aware", jsonInput(t, map[string]interface{}{"x": 1}))
	require.NoError(t, err)
	_, err = s.Enqueue("w-unreg", "unregistered", nil)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		ran, rerr := RunNext(context.Background(), s, reg, "worker")
		require.NoError(t, rerr)
		require.True(t, ran)
	}
	require.True(t, legacyRan.Load(), "the legacy factory ran (original registration survived the dup attempt)")
	require.True(t, awareRan.Load(), "the input-aware factory ran (original registration survived the dup attempt)")
	require.Equal(t, wqDone, wqState(t, s, "w-legacy"))
	require.Equal(t, wqDone, wqState(t, s, "w-aware"))
	require.Equal(t, wqPending, wqState(t, s, "w-unreg"), "an unregistered type is not claimable → stays pending")
}

// W1 — variable graph: the SAME registered type carries different concurrency in two runs, and each
// graph HONORS its own validated bound. Proven DETERMINISTICALLY by a start-barrier — each branch signals
// as it starts then holds a slot, so the test blocks until exactly `concurrency` branches are concurrently
// live and then proves no further branch can start while they are held (no timing poll on the positive bound).
func TestInputAware_W1_VariableGraphConcurrency(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	const width = 6
	var peak atomic.Int32
	var started chan struct{} // each branch signals here as it starts, then blocks on release
	var release chan struct{}

	require.NoError(t, reg.RegisterWithInput("variable", func(input []byte) (*DAG, error) {
		var cfg struct {
			Concurrency int `json:"concurrency"`
		}
		if err := json.Unmarshal(input, &cfg); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		if cfg.Concurrency < 1 {
			return nil, fmt.Errorf("%w: concurrency must be >= 1", ErrValidation)
		}
		b := NewWorkflowBuilder().WithExecutionConfig(ExecutionConfig{MaxConcurrency: cfg.Concurrency})
		var live atomic.Int32
		for i := 0; i < width; i++ {
			b.AddStartNode(fmt.Sprintf("n%d", i)).WithAction(ActionFunc(func(context.Context, *WorkflowData) error {
				cur := live.Add(1)
				for { // atomic max into peak
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				started <- struct{}{} // signal AFTER recording peak, then hold the slot occupied
				<-release
				live.Add(-1)
				return nil
			}))
		}
		return b.Build()
	}))

	runOne := func(wf string, concurrency int) int32 {
		peak.Store(0)
		started = make(chan struct{}, width)
		release = make(chan struct{})
		_, err := s.Enqueue(wf, "variable", jsonInput(t, map[string]interface{}{"concurrency": concurrency}))
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { _, e := RunNext(context.Background(), s, reg, "worker"); done <- e }()

		// Block until exactly `concurrency` branches have started concurrently (each signals then holds).
		for i := 0; i < concurrency; i++ {
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatalf("only %d of %d branches started concurrently — the input-selected bound was not honored", i, concurrency)
			}
		}
		// The bound is a CEILING: no (concurrency+1)th branch may start while these are held.
		select {
		case <-started:
			t.Fatalf("more than %d branches ran concurrently — MaxConcurrency=%d not honored", concurrency, concurrency)
		case <-time.After(150 * time.Millisecond):
		}
		observed := peak.Load()
		close(release)
		require.NoError(t, <-done)
		require.Equal(t, wqDone, wqState(t, s, wf))
		return observed
	}

	peak1 := runOne("run-c1", 1)
	require.EqualValues(t, 1, peak1, "concurrency=1 → at most one branch runs at a time")
	peak3 := runOne("run-c3", 3)
	require.EqualValues(t, 3, peak3, "concurrency=3 → exactly three branches run concurrently")
	require.NotEqual(t, peak1, peak3, "a constructor that IGNORED input would produce equal peaks")
}

// W2 — actual width bound: the input selects the fan-out width cap; an at-bound fan-out runs every
// branch and an over-bound fan-out fails LOUD (ErrFanOutMaxWidth) rather than silently truncating.
// Setting a width without enforcing it would let the over-bound case pass.
func TestInputAware_W2_FanOutWidthBound(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	var branches atomic.Int32
	require.NoError(t, reg.RegisterWithInput("fanout", func(input []byte) (*DAG, error) {
		var cfg struct {
			MaxWidth int `json:"max_width"`
		}
		if err := json.Unmarshal(input, &cfg); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		expander := func(context.Context, *WorkflowData) ([]interface{}, error) {
			return []interface{}{1, 2, 3, 4}, nil // the expander always resolves 4 branches
		}
		branchAction := ActionFunc(func(context.Context, *WorkflowData) error { branches.Add(1); return nil })
		b := NewWorkflowBuilder()
		b.AddFanOut("fan", expander, branchAction).WithMaxWidth(cfg.MaxWidth)
		return b.Build()
	}))

	// At-bound: cap 4, 4 items → every branch runs, run done.
	branches.Store(0)
	_, err := s.Enqueue("at-bound", "fanout", jsonInput(t, map[string]interface{}{"max_width": 4}))
	require.NoError(t, err)
	ran, rerr := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.NoError(t, rerr, "4 items <= width 4 → all branches run")
	require.Equal(t, wqDone, wqState(t, s, "at-bound"))
	require.EqualValues(t, 4, branches.Load(), "every branch executed (no silent truncation)")

	// Over-bound: cap 2, 4 items → the input-selected bound is enforced (loud), never truncated to 2.
	branches.Store(0)
	_, err = s.Enqueue("over-bound", "fanout", jsonInput(t, map[string]interface{}{"max_width": 2}))
	require.NoError(t, err)
	ran, rerr = RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.Error(t, rerr, "4 items > width 2 → the fan-out must fail, not truncate")
	require.ErrorIs(t, rerr, ErrFanOutMaxWidth, "over-width fan-out fails loud (ErrFanOutMaxWidth)")
	require.Equal(t, wqFailed, wqState(t, s, "over-bound"))
	require.EqualValues(t, 0, branches.Load(), "no branch ran — the bound was enforced before expansion, not silently truncated")
}

// W5 — invalid and absent input: a constructor refusal (even wrapping ErrIO — it must NOT be requeued as
// infrastructure), a nil DAG, a malformed seed payload, and zero-length input with a valid absent-input
// constructor. No action runs on failure; durable outcomes are honest.
func TestInputAware_W5_InvalidAndAbsentInput(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	var ran atomic.Bool

	// A constructor whose error wraps ErrIO must still DEAD-LETTER (a bad payload is not infra retry, C7).
	require.NoError(t, reg.RegisterWithInput("refuse-io", func([]byte) (*DAG, error) {
		return nil, fmt.Errorf("%w: transient-looking but a payload defect", ErrIO)
	}))
	// A constructor that returns a nil DAG with no error.
	require.NoError(t, reg.RegisterWithInput("nil-dag", func([]byte) (*DAG, error) { return nil, nil }))
	// A constructor that accepts ANY input (so seedInput, not the constructor, rejects a non-object).
	require.NoError(t, reg.RegisterWithInput("accept-any", func([]byte) (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { ran.Store(true); return nil }), nil
	}))
	// A constructor that INTENTIONALLY accepts absent (zero-length) input → builds a default graph.
	require.NoError(t, reg.RegisterWithInput("absent-ok", func(input []byte) (*DAG, error) {
		if len(input) != 0 {
			return nil, fmt.Errorf("%w: this type expects no input", ErrValidation)
		}
		return oneNode(t, "n", func(*WorkflowData) error { ran.Store(true); return nil }), nil
	}))

	cases := []struct {
		wf, typ   string
		input     []byte
		wantState string
	}{
		{"w-io", "refuse-io", jsonInput(t, map[string]interface{}{"x": 1}), wqFailed}, // constructor error → failed, NOT requeued
		{"w-nil", "nil-dag", jsonInput(t, map[string]interface{}{"x": 1}), wqFailed},  // nil DAG → failed before any action
		{"w-badseed", "accept-any", []byte(`[1,2,3]`), wqFailed},                      // constructor OK, seedInput rejects non-object
		{"w-absent", "absent-ok", nil, wqDone},                                        // zero-length input, valid absent constructor → done
	}
	for _, c := range cases {
		_, err := s.Enqueue(c.wf, c.typ, c.input)
		require.NoError(t, err)
		ran, rerr := RunNext(context.Background(), s, reg, "worker")
		require.True(t, ran, "%s: the worker handled the item", c.wf)
		if c.wantState == wqFailed {
			require.Error(t, rerr, "%s: a construction/seed failure surfaces an error", c.wf)
			require.NotErrorIs(t, rerr, ErrBusy, "%s: a payload defect is not an infra-retry", c.wf)
		} else {
			require.NoError(t, rerr, "%s", c.wf)
		}
		require.Equal(t, c.wantState, wqState(t, s, c.wf), "%s: durable outcome", c.wf)
	}
	require.True(t, ran.Load(), "the absent-input constructor actually ran its node")
}

// W3 — payload/seed identity: a deliberately NONCANONICAL outer payload; the constructor sees the
// submitted bytes verbatim (no normalization), decodes the nested config, and tries to mutate its
// argument; a real node reads the ORIGINAL seeded value after a store reload, the queue input is
// unchanged, and a large integer inside the nested JSON string survives (no float64 coercion).
func TestInputAware_W3_PayloadSeedIdentity(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()

	// Noncanonical: irregular whitespace + a nested JSON STRING holding a large int. seedInput sees the
	// OUTER object; "note" is a plain string a node reads back, "config" is an opaque JSON string.
	const bigInt = "9007199254740993" // 2^53 + 1 — would lose precision as a float64 number
	submitted := []byte(`{  "note" :  "hello-world" ,   "config": "{\"test_id\":` + bigInt + `,\"max_width\":8}"  }`)

	var sawBytes []byte
	require.NoError(t, reg.RegisterWithInput("identity", func(input []byte) (*DAG, error) {
		sawBytes = append([]byte(nil), input...) // record what the constructor received
		if !bytes.Equal(input, submitted) {
			return nil, fmt.Errorf("%w: constructor received normalized/altered bytes", ErrValidation)
		}
		// Attempt to mutate the argument — the defensive copy must protect the persisted input + seed.
		for i := range input {
			input[i] = 'X'
		}
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))

	_, err := s.Enqueue("w-identity", "identity", submitted)
	require.NoError(t, err)
	ran, rerr := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.NoError(t, rerr, "the constructor accepted the byte-identical payload and the run completed")
	require.Equal(t, wqDone, wqState(t, s, "w-identity"))
	require.True(t, bytes.Equal(sawBytes, submitted), "the constructor saw the submitted bytes verbatim (no normalization)")

	// A real node reads the ORIGINAL seeded values after a store reload (the constructor's mutation of its
	// copy did not touch the seed).
	loaded, err := s.Load("w-identity")
	require.NoError(t, err)
	note, ok := loaded.Get("note")
	require.True(t, ok)
	require.Equal(t, "hello-world", note, "the seeded scalar survived (constructor mutation did not corrupt it)")
	config, ok := loaded.Get("config")
	require.True(t, ok)
	require.Contains(t, config, bigInt, "the large int inside the JSON-string config survived (no float64 coercion)")

	// The DURABLE queue input is byte-unchanged (the constructor mutated only its private copy).
	require.Equal(t, submitted, workQueueInput(t, s, "w-identity"), "the persisted queue input is the original bytes")
}

// W10 — queued-child correctness: the child's INPUT selects its tolerated-failure policy; the child's
// execution decides the queue outcome; the parent HONORS that queue outcome (no graph reclassification);
// and a re-drive with a conflicting input-aware child candidate is REFUSED, not silently reinterpreted.
func TestInputAware_W10_QueuedChildInputSelectsPolicy(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	// An input-aware CHILD: a node "work" that always fails; input.tolerate decides continue-on-error
	// (child succeeds → done) vs not (child fails → failed).
	require.NoError(t, reg.RegisterWithInput("policyChild", func(input []byte) (*DAG, error) {
		var cfg struct {
			Tolerate bool `json:"tolerate"`
		}
		if err := json.Unmarshal(input, &cfg); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		b := NewWorkflowBuilder()
		nb := b.AddStartNode("work").WithAction(ActionFunc(func(context.Context, *WorkflowData) error {
			return errors.New("work always fails")
		}))
		if cfg.Tolerate {
			nb.WithContinueOnError()
		}
		return b.Build()
	}))

	run := func(parentWF string, tolerate bool) (childState string, parentErr error) {
		pb := NewWorkflowBuilder().WithWorkflowID(parentWF)
		pb.AddSubWorkflowQueued("sub", "policyChild").WithInput(map[string]interface{}{"tolerate": tolerate})
		pdag, err := pb.Build()
		require.NoError(t, err)
		pw := newWorkflowForTest(s)
		pw.WorkflowID = parentWF
		pw.dag = pdag
		pw.registry = reg
		require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")
		ran, _ := RunNext(context.Background(), s, reg, "worker") //nolint:errcheck // the child's own failure when !tolerate is expected; the queue state + parent outcome below are the assertions
		require.True(t, ran, "worker ran the child")
		perr := pw.Execute(context.Background()) // wake the parent on the child's terminal queue outcome
		return wqState(t, s, SubWorkflowChildID(parentWF, "sub")), perr
	}

	cs, pe := run("parent-tol", true)
	require.Equal(t, wqDone, cs, "input tolerate=true → the child's failure is tolerated → child done")
	require.NoError(t, pe, "the parent honors the child's done outcome")

	cs2, pe2 := run("parent-notol", false)
	require.Equal(t, wqFailed, cs2, "input tolerate=false → the child fails")
	require.Error(t, pe2, "the parent honors the child's failed queue outcome (INV-01)")
	require.NotErrorIs(t, pe2, ErrSuspended, "a terminal child resolves the parent, not a park")

	// Replay conflict (C9): a parent parks having enqueued the child with input X; a re-drive that
	// declares a DIFFERENT child input is refused — the durable queued input is authoritative.
	pb := NewWorkflowBuilder().WithWorkflowID("parent-conflict")
	pb.AddSubWorkflowQueued("sub", "policyChild").WithInput(map[string]interface{}{"tolerate": true})
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := newWorkflowForTest(s)
	pw.WorkflowID = "parent-conflict"
	pw.dag = pdag
	pw.registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "enqueues child (tolerate=true) + parks")

	pb2 := NewWorkflowBuilder().WithWorkflowID("parent-conflict")
	pb2.AddSubWorkflowQueued("sub", "policyChild").WithInput(map[string]interface{}{"tolerate": false})
	pdag2, err := pb2.Build()
	require.NoError(t, err)
	pw2 := newWorkflowForTest(s)
	pw2.WorkflowID = "parent-conflict"
	pw2.dag = pdag2
	pw2.registry = reg
	cerr := pw2.Execute(context.Background())
	require.Error(t, cerr, "a conflicting child redefinition must not silently proceed")
	require.ErrorIs(t, cerr, ErrValidation, "the conflicting input-aware child redefinition is refused")
}

// Independent-review defect: a queued child under this parent+node is enqueued as type A; a re-drive
// declaring a DIFFERENT type B (even with identical input) must be REFUSED — the childID collides with a
// different-type durable child. The original C9 guard compared only the input, so a type-only conflict
// slipped through and the parent re-suspended instead of refusing.
func TestInputAware_QueuedChildTypeConflict_Refused(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.RegisterWithInput("typeA", func([]byte) (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))
	require.NoError(t, reg.RegisterWithInput("typeB", func([]byte) (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))

	pbA := NewWorkflowBuilder().WithWorkflowID("p")
	pbA.AddSubWorkflowQueued("sub", "typeA").WithInput(map[string]interface{}{"k": "v"})
	dagA, err := pbA.Build()
	require.NoError(t, err)
	pwA := newWorkflowForTest(s)
	pwA.WorkflowID = "p"
	pwA.dag = dagA
	pwA.registry = reg
	require.ErrorIs(t, pwA.Execute(context.Background()), ErrSuspended, "parent enqueues child as typeA + parks")

	// Re-drive the SAME parent+node with a different TYPE and identical input → durable-type conflict.
	pbB := NewWorkflowBuilder().WithWorkflowID("p")
	pbB.AddSubWorkflowQueued("sub", "typeB").WithInput(map[string]interface{}{"k": "v"})
	dagB, err := pbB.Build()
	require.NoError(t, err)
	pwB := newWorkflowForTest(s)
	pwB.WorkflowID = "p"
	pwB.dag = dagB
	pwB.registry = reg
	rerr := pwB.Execute(context.Background())
	require.Error(t, rerr, "a queued child re-declared with a different type must not silently proceed")
	require.ErrorIs(t, rerr, ErrValidation, "a durable-type conflict is refused")
	require.NotErrorIs(t, rerr, ErrSuspended, "must not re-suspend on a type conflict")
}

// Independent-review defect: when RECORDING a factory failure faults (the store rejects MarkFailed),
// RunNext must SURFACE the persistence error, not silently return only the factory's validation error
// while the row stays claimed and the store fault is discarded (§5A).
func TestInputAware_FactoryFailure_RecordingFaultSurfaced(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	// The factory drops work_queue during construction (after the claim), so the subsequent MarkFailed
	// UPDATE faults — modeling a store rejection of the failure record.
	require.NoError(t, reg.RegisterWithInput("self-fault", func([]byte) (*DAG, error) {
		_, _ = s.db.Exec("DROP TABLE work_queue") //nolint:errcheck // deliberate fault injection
		return nil, fmt.Errorf("%w: factory boom", ErrValidation)
	}))
	_, err := s.Enqueue("wf", "self-fault", jsonInput(t, map[string]interface{}{"x": 1}))
	require.NoError(t, err)

	ran, rerr := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.Error(t, rerr)
	require.ErrorIs(t, rerr, ErrValidation, "the factory cause is still surfaced")
	require.ErrorContains(t, rerr, "failed to record terminal failure", "the persistence fault is SURFACED, not discarded")
}

// A9 (compatibility declaration): the durable-type conflict guard is UNCONDITIONAL — it applies to
// LEGACY (zero-arg) child registrations too. Ordinary same-type legacy replay is unaffected (a re-drive
// of the same parent DAG presents the same type → proceeds); only a different-type reuse of the same
// deterministic child ID is refused. This distinguishes the two.
func TestQueuedChild_LegacyReplay_SameTypeOK_DifferentTypeRefused(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.Register("legA", func() (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))
	require.NoError(t, reg.Register("legB", func() (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))

	mkParent := func(childType string) *Workflow {
		pb := NewWorkflowBuilder().WithWorkflowID("p")
		pb.AddSubWorkflowQueued("sub", childType)
		dag, err := pb.Build()
		require.NoError(t, err)
		pw := newWorkflowForTest(s)
		pw.WorkflowID = "p"
		pw.dag = dag
		pw.registry = reg
		return pw
	}

	// Enqueue child as legA + park.
	require.ErrorIs(t, mkParent("legA").Execute(context.Background()), ErrSuspended, "enqueue legA + park")
	// Ordinary same-type legacy replay → still parks (NOT refused) — unchanged behavior.
	require.ErrorIs(t, mkParent("legA").Execute(context.Background()), ErrSuspended, "same-type legacy replay is unaffected")
	// Different-type reuse of the same child ID → refused (the durable-type integrity guard).
	rerr := mkParent("legB").Execute(context.Background())
	require.Error(t, rerr)
	require.ErrorIs(t, rerr, ErrValidation, "a different-type reuse of the same child ID is refused, even for legacy")
	require.NotErrorIs(t, rerr, ErrSuspended)
}

// W11 — control-plane separation: forged parent/signal/depth-looking KEYS in the child payload cannot
// change the engine-derived child identity, completion destination, or runtime depth (those live in
// trusted control columns set by EnqueueSubWorkflow, never the input BLOB).
func TestInputAware_W11_ControlPlaneSeparation(t *testing.T) {
	s := mkQueueStore(t)
	reg := NewRegistry()
	require.NoError(t, reg.RegisterWithInput("cpChild", func([]byte) (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))
	forged := map[string]interface{}{
		"parent_id": "attacker-wf", "parent_signal": "attacker-sig", "depth": 0, "result": "x",
	}
	pb := NewWorkflowBuilder().WithWorkflowID("real-parent")
	pb.AddSubWorkflowQueued("sub", "cpChild").WithInput(forged)
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := newWorkflowForTest(s)
	pw.WorkflowID = "real-parent"
	pw.dag = pdag
	pw.registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")

	// Claim the child as a worker would; the TRUSTED control columns are engine-derived, not the payload.
	item, err := s.ClaimNext("worker", "cpChild")
	require.NoError(t, err)
	require.Equal(t, SubWorkflowChildID("real-parent", "sub"), item.WorkflowID, "child id is engine-derived")
	require.Equal(t, "real-parent", item.ParentID, "parent address is the trusted column, not the forged payload key")
	require.Equal(t, completionSignalName("sub"), item.ParentSignal, "completion destination is engine-derived")
	require.Equal(t, 1, item.Depth, "depth is engine-derived (parent depth + 1), not the forged 0")
}

// W12 — static-inspection honesty: legacy cycle detection still finds a declared legacy cycle; a
// registry containing ANY input-aware factory returns the specified unsupported-inspection refusal
// WITHOUT invoking an invented-input factory (and not a false cycle claim).
func TestInputAware_W12_CycleHelperRefusesInputAware(t *testing.T) {
	// Legacy-only: A queues B queues A → the cycle is still detected.
	reg := NewRegistry()
	require.NoError(t, reg.Register("A", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("toB", "B")
		return b.Build()
	}))
	require.NoError(t, reg.Register("B", func() (*DAG, error) {
		b := NewWorkflowBuilder()
		b.AddSubWorkflowQueued("toA", "A")
		return b.Build()
	}))
	require.ErrorIs(t, reg.ValidateNoTypeCycles(), ErrSubWorkflowTypeCycle, "a declared legacy cycle is still detected")

	// Any input-aware factory → explicit unsupported-inspection refusal, not a cycle error, and the
	// input-aware factory is never invoked with invented input.
	reg2 := NewRegistry()
	require.NoError(t, reg2.Register("legacy", func() (*DAG, error) { return newDAGForTest("x"), nil }))
	var awareCalled atomic.Bool
	require.NoError(t, reg2.RegisterWithInput("aware", func([]byte) (*DAG, error) {
		awareCalled.Store(true)
		return newDAGForTest("y"), nil
	}))
	err := reg2.ValidateNoTypeCycles()
	require.ErrorIs(t, err, ErrValidation, "an input-aware registry gets the unsupported-inspection refusal")
	require.NotErrorIs(t, err, ErrSubWorkflowTypeCycle, "the refusal is NOT a false cycle claim")
	require.False(t, awareCalled.Load(), "the input-aware factory was NOT invoked with invented input")
}

// W6 — checkpoint reclaim: a partially-progressed input-aware run, its lease lapsed, is reclaimed by a
// FRESH worker/registry and REBUILT FROM THE DURABLE INPUT (C5); completed work is not re-invoked and
// the progressed journal survives (C6). The constructor sees the same durable bytes on every rebuild.
func TestInputAware_W6_ReclaimRebuildsFromDurableInput(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	ctr := newRunCounter()
	var inputsSeen []string
	mkReg := func() *Registry {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("staged", func(input []byte) (*DAG, error) {
			inputsSeen = append(inputsSeen, string(input))
			d := newDAGForTest("staged")
			if err := d.addNode(newNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n0"); return nil }))); err != nil {
				return nil, err
			}
			if err := d.addNode(newNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n1"); return nil }))); err != nil {
				return nil, err
			}
			return d, d.addDependency("n0", "n1")
		}))
		return reg
	}
	input := jsonInput(t, map[string]interface{}{"tag": "durable-X"})

	// Worker A claims (token 1) and durably commits a PARTIAL journal (n0 done, n1 pending) — then "dies"
	// before MarkDone (n0 counter stays 0: staged manually, as if A ran + checkpointed it).
	_, err := s.Enqueue("wf", "staged", input)
	require.NoError(t, err)
	_, err = s.ClaimNext("A", "staged")
	require.NoError(t, err)
	partial := NewWorkflowData("wf")
	partial.SetNodeStatus("n0", Completed)
	partial.SetNodeStatus("n1", Pending)
	require.NoError(t, s.Save(partial))

	// A dies → lapse the lease. A FRESH worker B (fresh registry) reclaims via RunNext: it rebuilds from
	// the DURABLE input, skips the seed (a journal exists), resumes n1, and terminalizes done.
	clk.Advance(6 * time.Second)
	ran, rerr := RunNext(context.Background(), s, mkReg(), "B")
	require.NoError(t, rerr)
	require.True(t, ran, "worker B reclaimed the lapsed-claimed run")
	require.Equal(t, wqDone, wqState(t, s, "wf"), "the reclaimed run resumed to done")

	require.Equal(t, 0, ctr.get("n0"), "n0 was Completed in the durable journal → NOT re-invoked on reclaim")
	require.Equal(t, 1, ctr.get("n1"), "n1 resumed from the committed frontier")
	require.NotEmpty(t, inputsSeen)
	for _, in := range inputsSeen {
		require.Equal(t, string(input), in, "every rebuild (incl. the reclaim) used the DURABLE queued input")
	}
}

// W8 — construction holds neither the registry lock nor the claim transaction (C3/C4): while worker A is
// blocked INSIDE an input-aware constructor (which itself calls Registry.Types without deadlocking),
// worker B claims and runs a different item to completion on the same store.
func TestInputAware_W8_ConstructionHoldsNoLockOrTxn(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	buildStarted := make(chan struct{})
	release := make(chan struct{})
	var barrierUsed atomic.Bool
	require.NoError(t, reg.RegisterWithInput("barrier", func([]byte) (*DAG, error) {
		if !barrierUsed.Swap(true) { // only the FIRST build blocks at the barrier
			_ = reg.Types() // prove the constructor can inspect the registry without deadlocking
			close(buildStarted)
			<-release
		}
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))
	require.NoError(t, reg.Register("free", func() (*DAG, error) {
		return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
	}))

	_, err := s.Enqueue("w-barrier", "barrier", jsonInput(t, map[string]interface{}{"x": 1}))
	require.NoError(t, err)
	_, err = s.Enqueue("w-free", "free", nil)
	require.NoError(t, err)

	// Worker A claims w-barrier and blocks IN construction.
	aDone := make(chan error, 1)
	go func() { _, e := RunNext(context.Background(), s, reg, "workerA"); aDone <- e }()
	select {
	case <-buildStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("worker A did not enter construction")
	}

	// While A is held in construction, worker B claims + runs w-free to completion — proving A's
	// construction holds neither the registry lock (B calls reg.lookup) nor a claim txn (B's ClaimNext).
	ranB, errB := RunNext(context.Background(), s, reg, "workerB")
	require.NoError(t, errB)
	require.True(t, ranB, "worker B progressed while A was held in construction")
	require.Equal(t, wqDone, wqState(t, s, "w-free"))

	close(release) // let A finish
	require.NoError(t, <-aDone)
	require.Equal(t, wqDone, wqState(t, s, "w-barrier"))
}

// workQueueInput reads the durable work_queue.input bytes for a workflow id (identity assertions).
func workQueueInput(t *testing.T, s *SQLiteStore, wf string) []byte {
	t.Helper()
	var in []byte
	require.NoError(t, s.db.QueryRow(`SELECT input FROM work_queue WHERE workflow_id=?`, wf).Scan(&in))
	return in
}
