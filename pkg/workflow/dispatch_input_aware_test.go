package workflow

// M25 input-aware registered DAG factories — Phase B witnesses (W1/W3/W4/W5). RegisterWithInput lets
// one registered type build different validated per-run graphs from the queued input, through the SAME
// capped/fenced dispatch lifecycle. All witnesses run over real SQLite dispatch (mkDispatchStore).

import (
	"bytes"
	"context"
	"encoding/json"
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
// graph HONORS its own validated bound. Proven by an atomic active-count that blocks until exactly the
// input-selected number of nodes run concurrently (a constructor that ignored input would let the width
// run and never settle at the requested bound).
func TestInputAware_W1_VariableGraphConcurrency(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	const width = 6
	var active, peak atomic.Int32
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
		for i := 0; i < width; i++ {
			b.AddStartNode(fmt.Sprintf("n%d", i)).WithAction(ActionFunc(func(context.Context, *WorkflowData) error {
				cur := active.Add(1)
				for { // atomic max into peak
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				<-release // hold the slot occupied so the concurrent set is observable
				active.Add(-1)
				return nil
			}))
		}
		return b.Build()
	}))

	runOne := func(wf string, concurrency int) int32 {
		active.Store(0)
		peak.Store(0)
		release = make(chan struct{})
		_, err := s.Enqueue(wf, "variable", jsonInput(t, map[string]interface{}{"concurrency": concurrency}))
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { _, e := RunNext(context.Background(), s, reg, "worker"); done <- e }()
		// The bound is honored iff EXACTLY `concurrency` nodes are simultaneously blocked (the rest wait
		// for a slot). If the constructor ignored input (default bound), active would rise to `width` and
		// never equal `concurrency` → this fails, catching the bug deterministically (no timing guess).
		want := int32(concurrency)
		require.Eventually(t, func() bool { return active.Load() == want }, 3*time.Second, 5*time.Millisecond,
			"exactly %d nodes run concurrently (the input-selected bound)", want)
		observed := peak.Load()
		close(release)
		require.NoError(t, <-done)
		require.Equal(t, wqDone, wqState(t, s, wf))
		return observed
	}

	peak1 := runOne("run-c1", 1)
	require.EqualValues(t, 1, peak1, "concurrency=1 → at most one node runs at a time")
	peak3 := runOne("run-c3", 3)
	require.EqualValues(t, 3, peak3, "concurrency=3 → exactly three nodes run concurrently")
	require.NotEqual(t, peak1, peak3, "a constructor that IGNORED input would produce equal peaks")
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

// workQueueInput reads the durable work_queue.input bytes for a workflow id (identity assertions).
func workQueueInput(t *testing.T, s *SQLiteStore, wf string) []byte {
	t.Helper()
	var in []byte
	require.NoError(t, s.db.QueryRow(`SELECT input FROM work_queue WHERE workflow_id=?`, wf).Scan(&in))
	return in
}
