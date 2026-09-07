package workflow

// M25 input-aware registered DAG factories — §14 E-01 COMPLETION witnesses (A1..A8). These extend the
// Phase-B witnesses (dispatch_input_aware_test.go) to exercise the input-aware NEW PATH directly for the
// scenarios the independent recheck (§13) left as shared-regression-only: end-to-end nested decode + a node
// reading the seeded value (A1), early failure + missing-data integrity (A2), settings/progressed-KV survival
// on reclaim (A3), input-aware completion reconciliation (A4), concurrent config isolation + capped admission
// (A5), lease loss on the new path (A6), the input-aware CHILD itself parking/resuming (A7), and cancellation/
// drain/bounded-retry on the new path (A8). All run over real SQLite dispatch.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkSharedStores opens two independent *SQLiteStore handles on the SAME db file (two "processes" over one
// shared queue), each with the given options, and returns both. Used by the multi-worker witnesses (A5/A6).
func mkSharedStores(t *testing.T, opts ...SQLiteOption) (*SQLiteStore, *SQLiteStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.db")
	all := append([]SQLiteOption{WithMultiProcess()}, opts...)
	s1, err := NewSQLiteStore(path, all...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s1.Close() }) //nolint:errcheck // cleanup
	s2, err := NewSQLiteStore(path, all...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() }) //nolint:errcheck // cleanup
	return s1, s2
}

// leaseToken reads the durable fencing token for a workflow's lease (0 when no lease row).
func leaseToken(t *testing.T, s *SQLiteStore, wf string) int64 {
	t.Helper()
	var tok int64
	err := s.db.QueryRow(`SELECT fencing_token FROM leases WHERE workflow_id=?`, wf).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return tok
}

// A1 — W3 end-to-end payload ownership + decoding, with a NODE that reads the seeded value after reload.
// The constructor sees the byte-identical noncanonical payload, DECODES the nested JSON-string config
// (preserving a >2^53 integer via json.Number), and builds a node that at RUN time reads the seeded values
// out of WorkflowData and records what it observed. After a real store reload the recorded observations show
// the node saw the ORIGINAL seeded scalar and the exact large integer; the durable queue bytes are unchanged.
func TestInputAware_A1_W3_NestedDecodeNodeReadsSeed(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()

	const bigInt = "9007199254740993" // 2^53 + 1 — lost as a float64 number
	submitted := []byte(`{  "note" :  "hello-world" ,   "config": "{\"test_id\":` + bigInt + `,\"max_width\":8}"  }`)

	var (
		ctorDecodedTestID string
		ctorDecodedWidth  int64
	)
	require.NoError(t, reg.RegisterWithInput("nested", func(input []byte) (*DAG, error) {
		// Byte identity: the constructor owns the ORIGINAL submitted bytes.
		if string(input) != string(submitted) {
			return nil, fmt.Errorf("%w: constructor received normalized/altered bytes", ErrValidation)
		}
		// Decode the OUTER object, then the nested JSON-STRING config, preserving the big int via json.Number.
		var outer struct {
			Config string `json:"config"`
		}
		if err := json.Unmarshal(input, &outer); err != nil {
			return nil, fmt.Errorf("%w: outer decode: %w", ErrValidation, err)
		}
		dec := json.NewDecoder(strings.NewReader(outer.Config))
		dec.UseNumber()
		var cfg struct {
			TestID   json.Number `json:"test_id"`
			MaxWidth json.Number `json:"max_width"`
		}
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("%w: nested config decode: %w", ErrValidation, err)
		}
		ctorDecodedTestID = cfg.TestID.String()
		w, err := cfg.MaxWidth.Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: max_width: %w", ErrValidation, err)
		}
		ctorDecodedWidth = w
		// A real node reads the SEEDED values at run time and records what it observed, so the observation
		// survives to the durable journal (not just a constructor-side callback assertion).
		return oneNode(t, "reader", func(d *WorkflowData) error {
			note, ok := d.Get("note")
			if !ok {
				return errors.New("node did not observe the seeded note")
			}
			d.Set("node_saw_note", note)
			cfgStr, ok := d.Get("config")
			if !ok {
				return errors.New("node did not observe the seeded config")
			}
			cfgText, ok := cfgStr.(string)
			if !ok {
				return errors.New("seeded config was not a string")
			}
			// The node decodes the nested config the same way and records the big int it read.
			ndec := json.NewDecoder(strings.NewReader(cfgText))
			ndec.UseNumber()
			var ncfg struct {
				TestID json.Number `json:"test_id"`
			}
			if err := ndec.Decode(&ncfg); err != nil {
				return err
			}
			d.Set("node_saw_test_id", ncfg.TestID.String())
			return nil
		}), nil
	}))

	_, err := s.Enqueue("w-nested", "nested", submitted)
	require.NoError(t, err)
	ran, rerr := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.NoError(t, rerr)
	require.Equal(t, wqDone, wqState(t, s, "w-nested"))

	// The constructor decoded the nested config, preserving the >2^53 integer and the width.
	require.Equal(t, bigInt, ctorDecodedTestID, "constructor decoded the nested test_id without float64 coercion")
	require.EqualValues(t, 8, ctorDecodedWidth, "constructor decoded the nested max_width")

	// After a REAL store reload, the node's recorded observations prove it read the ORIGINAL seeded values.
	loaded, err := s.Load("w-nested")
	require.NoError(t, err)
	sawNote, ok := loaded.Get("node_saw_note")
	require.True(t, ok)
	require.Equal(t, "hello-world", sawNote, "the node observed the seeded scalar")
	sawID, ok := loaded.Get("node_saw_test_id")
	require.True(t, ok)
	require.Equal(t, bigInt, sawID, "the node read the exact large integer out of the reloaded seed")

	// The durable queue bytes are the original submission (the defensive copy protected them).
	require.Equal(t, submitted, workQueueInput(t, s, "w-nested"), "the persisted queue input is unchanged")
}

// A2 — W5 early failure and missing-data integrity, on the input-aware queued-child path.
//
//	(a) An input-aware queued child whose constructor SUCCEEDS at the parent's enqueue-resolve but FAILS at
//	    the worker: the worker records a durable terminal failure, delivers the completion trigger, no child
//	    action runs, and the re-driven parent RESOLVES FAILURE (never re-parks).
//	(b) A queue child that is `done` with NO journal is an INTEGRITY error (ErrCorruptData), surfaced on the
//	    parent's re-drive — never a silent success and never an indefinite park.
//
// The pure "constructor accepts, seedInput rejects" observation for the input-aware path is exercised at
// DISPATCH level by W5 (accept-any + a non-object seed → failed, not retried) — a parent-awaited input-aware
// child cannot itself reach a seed rejection, because seedInput rejects only NON-OBJECT input and a parent
// declares its child input as a JSON object (WithInput(map)); an input-aware child then enforces durable ==
// declared input, so a non-object child input can never be presented by a parent. This structural note is
// recorded in the A-receipt.
func TestInputAware_A2_W5_EarlyFailureAndIntegrity(t *testing.T) {
	t.Run("constructor_fails_at_worker_parent_resolves_failure", func(t *testing.T) {
		s := mkQueueStore(t)
		reg := NewRegistry()
		var childActions atomic.Int32
		var calls atomic.Int32
		// The input-aware child factory: succeeds on call 1 (parent enqueue-resolve) and call 3 (parent
		// re-drive verdict), FAILS on call 2 (the worker) — the realistic "dependency present at enqueue,
		// gone at claim" shape that reaches runNext's factory-error terminal path.
		require.NoError(t, reg.RegisterWithInput("awareFlaky", func(input []byte) (*DAG, error) {
			if calls.Add(1) == 2 {
				return nil, fmt.Errorf("%w: factory boom at the worker", ErrValidation)
			}
			return oneNode(t, "n", func(*WorkflowData) error { childActions.Add(1); return nil }), nil
		}))
		pw, childID := mkAwareParent(t, s, reg, "awareFlaky", map[string]any{"k": "v"})

		ran, rerr := RunNext(context.Background(), s, reg, "worker")
		require.True(t, ran, "the worker handled the child")
		require.Error(t, rerr, "the worker's factory failed")
		require.ErrorIs(t, rerr, ErrValidation)
		require.Equal(t, wqFailed, wqState(t, s, childID), "the child row is terminally failed")
		require.EqualValues(t, 0, childActions.Load(), "no child action executed (construction failed first)")

		// The worker WOKE the parent (BUG-1 fix): the completion trigger is in the parent mailbox.
		box, terr := s.TakeSignals("parent-wf")
		require.NoError(t, terr)
		require.Len(t, box, 1, "the worker delivered the completion trigger after the child failed construction")
		// Re-deliver it (TakeSignals consumed it) so the parent's wake path is exercised on re-drive.
		require.NoError(t, s.DeliverSignal("parent-wf", box[0]))

		rerr2 := pw.Execute(context.Background())
		require.Error(t, rerr2, "the parent resolves failure")
		require.NotErrorIs(t, rerr2, ErrSuspended, "the parent must NOT re-park on a terminal-failed child")
		final, err := s.Load("parent-wf")
		require.NoError(t, err)
		assertNodeStatus(t, final, "sub", Failed)
	})

	t.Run("queue_done_without_journal_is_integrity_error", func(t *testing.T) {
		s := mkQueueStore(t)
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("awareDone", func([]byte) (*DAG, error) {
			return oneNode(t, "n", func(*WorkflowData) error { return nil }), nil
		}))
		pw, childID := mkAwareParent(t, s, reg, "awareDone", map[string]any{"k": "v"})

		// Force the child row to `done` with NO journal (the integrity-violation shape): claim then MarkDone
		// without ever running/checkpointing it.
		_, cerr := s.ClaimNext("worker", "awareDone")
		require.NoError(t, cerr)
		ok, err := s.MarkDone(childID)
		require.NoError(t, err)
		require.True(t, ok, "child row flipped claimed->done")
		_, lerr := s.Load(childID)
		require.ErrorIs(t, lerr, ErrNotFound, "the done child has NO journal")

		rerr := pw.Execute(context.Background())
		require.Error(t, rerr, "a done-without-journal child is an integrity error, not a silent success")
		require.ErrorIs(t, rerr, ErrCorruptData, "the missing-journal integrity violation is surfaced")
		require.NotErrorIs(t, rerr, ErrSuspended, "and it is not an indefinite park")
	})
}

// mkAwareParent builds a parent whose queued node "sub" awaits an input-aware child of childType with the
// given input, drives it once so it enqueues the child + parks, and returns the parent + the childID.
func mkAwareParent(t *testing.T, s *SQLiteStore, reg *Registry, childType string, input map[string]any) (*Workflow, string) {
	t.Helper()
	pb := NewWorkflowBuilder().WithWorkflowID("parent-wf")
	pb.AddSubWorkflowQueued("sub", childType).WithInput(input)
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := newWorkflowForTest(s)
	pw.WorkflowID = "parent-wf"
	pw.dag = pdag
	pw.registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues + parks")
	return pw, SubWorkflowChildID("parent-wf", "sub")
}

// A3 — W6 settings and PROGRESSED DATA survive reclaim. A replacement worker/registry (worker defaults
// deliberately different from the queued configuration) reclaims a lapsed-claimed run: it rebuilds the
// original per-run behavior FROM THE DURABLE INPUT, skips the committed first stage, and resumes the pending
// stage reading PROGRESSED JOURNAL KV (a data value written by the first stage) rather than reseeding the
// initial values. Asserts behavior AND data, not only constructor arguments.
func TestInputAware_A3_W6_SettingsAndProgressedDataSurviveReclaim(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	ctr := newRunCounter()
	var inputsSeen []string

	// The REPLACEMENT worker's registry. Its factory reads the per-run "mode" from the DURABLE input (a
	// worker "default" would differ — the point is the reconstruction honors the queued config, not a
	// worker-local default). n1 records both the input-derived mode AND the progressed KV artifact.
	mkReg := func() *Registry {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("staged", func(input []byte) (*DAG, error) {
			inputsSeen = append(inputsSeen, string(input))
			var cfg struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(input, &cfg); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrValidation, err)
			}
			d := newDAGForTest("staged")
			if err := d.addNode(newNode("n0", ActionFunc(func(_ context.Context, wd *WorkflowData) error {
				ctr.inc("n0")
				wd.Set("artifact", "built-by-n0")
				return nil
			}))); err != nil {
				return nil, err
			}
			if err := d.addNode(newNode("n1", ActionFunc(func(_ context.Context, wd *WorkflowData) error {
				ctr.inc("n1")
				art, _ := wd.Get("artifact") // the PROGRESSED KV from n0 (must survive reclaim, not be reseeded away)
				wd.Set("n1_saw_artifact", art)
				wd.Set("n1_saw_mode", cfg.Mode) // input-derived per-run behavior on the reclaim rebuild
				return nil
			}))); err != nil {
				return nil, err
			}
			return d, d.addDependency("n0", "n1")
		}))
		return reg
	}
	input := jsonInput(t, map[string]interface{}{"mode": "configured-mode"})

	// Worker A claims (token 1) and durably commits a PARTIAL journal: n0 Completed WITH its progressed KV
	// artifact, n1 Pending. Then A "dies" before MarkDone (n0 counter stays 0 — staged as if A ran it).
	_, err := s.Enqueue("wf", "staged", input)
	require.NoError(t, err)
	_, err = s.ClaimNext("A", "staged")
	require.NoError(t, err)
	partial := NewWorkflowData("wf")
	partial.SetNodeStatus("n0", Completed)
	partial.SetNodeStatus("n1", Pending)
	partial.Set("artifact", "built-by-n0") // the committed progressed KV
	require.NoError(t, s.Save(partial))

	// A dies → lapse the lease. A FRESH worker/registry B reclaims via RunNext.
	clk.Advance(6 * time.Second)
	ran, rerr := RunNext(context.Background(), s, mkReg(), "B")
	require.NoError(t, rerr)
	require.True(t, ran, "worker B reclaimed the lapsed-claimed run")
	require.Equal(t, wqDone, wqState(t, s, "wf"), "the reclaimed run resumed to done")

	require.Equal(t, 0, ctr.get("n0"), "n0 was Completed in the journal → NOT re-invoked on reclaim")
	require.Equal(t, 1, ctr.get("n1"), "n1 resumed from the committed frontier")

	loaded, err := s.Load("wf")
	require.NoError(t, err)
	sawArt, ok := loaded.Get("n1_saw_artifact")
	require.True(t, ok)
	require.Equal(t, "built-by-n0", sawArt, "n1 read the PROGRESSED KV — the reclaim did NOT reseed initial values over it")
	sawMode, ok := loaded.Get("n1_saw_mode")
	require.True(t, ok)
	require.Equal(t, "configured-mode", sawMode, "the reclaim rebuild honored the DURABLE per-run config, not a worker default")

	require.NotEmpty(t, inputsSeen)
	for _, in := range inputsSeen {
		require.Equal(t, string(input), in, "every rebuild used the durable queued input")
	}
}

// A4 — W7 input-aware completion reconciliation. A COMPLETE journal + an un-terminalized `claimed` queue row
// + a lapsed lease is reclaimed THROUGH RegisterWithInput: the rebuilt run re-executes as a NO-OP (completed
// action counters do not increase) and the authoritative queue row reaches `done`. Parameterizes the shared
// reconciliation seam onto the input-aware path.
func TestInputAware_A4_W7_CompletionReconciliation(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	ctr := newRunCounter()
	var inputsSeen []string
	mkReg := func() *Registry {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("recon", func(input []byte) (*DAG, error) {
			inputsSeen = append(inputsSeen, string(input))
			d := newDAGForTest("recon")
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
	input := jsonInput(t, map[string]interface{}{"tag": "recon"})

	// Stage the seam: a COMPLETE journal + a `claimed` queue row (token 1) A committed before dying pre-MarkDone.
	_, err := s.Enqueue("wf", "recon", input)
	require.NoError(t, err)
	_, err = s.ClaimNext("A", "recon")
	require.NoError(t, err)
	complete := NewWorkflowData("wf")
	complete.SetNodeStatus("n0", Completed)
	complete.SetNodeStatus("n1", Completed)
	require.NoError(t, s.Save(complete))
	require.Equal(t, wqClaimed, wqState(t, s, "wf"))

	// Lease lapses → RunNext reclaims through the input-aware factory, re-executes the complete journal as a
	// no-op, and terminalizes done.
	clk.Advance(6 * time.Second)
	ran, rerr := RunNext(context.Background(), s, mkReg(), "B")
	require.NoError(t, rerr)
	require.True(t, ran, "worker B reconciled the un-terminalized complete run")
	require.Equal(t, wqDone, wqState(t, s, "wf"), "the authoritative queue row reached done")
	require.Equal(t, 0, ctr.get("n0"), "no completed action re-executed on reconciliation")
	require.Equal(t, 0, ctr.get("n1"), "no completed action re-executed on reconciliation")
	require.NotEmpty(t, inputsSeen)
	for _, in := range inputsSeen {
		require.Equal(t, string(input), in, "the reconciliation rebuild used the durable queued input")
	}
}

// A5 — W8 concurrent CONFIG ISOLATION + CAPPED ADMISSION + barrier. Two store handles over ONE shared queue,
// a per-type cap of 1 on "iso". While worker A is held inside an "iso" constructor (barrier) — proving it
// holds neither the registry lock (it calls Registry.Types) nor the claim txn — a second "iso" candidate is
// at cap and stays PENDING (backpressure), while an eligible "free" candidate WITH capacity progresses on the
// other handle. After A releases, the second "iso" run proceeds. Each run observes only its OWN input (no
// shared mutable graph/input leakage).
func TestInputAware_A5_W8_ConcurrentConfigIsolationAndCap(t *testing.T) {
	caps := WithCaps(Caps{PerType: map[string]int{"iso": 1}})
	s1, s2 := mkSharedStores(t, caps)

	buildStarted := make(chan struct{})
	release := make(chan struct{})
	mkReg := func() *Registry {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("iso", func(input []byte) (*DAG, error) {
			var cfg struct {
				Tag string `json:"tag"`
			}
			if err := json.Unmarshal(input, &cfg); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrValidation, err)
			}
			if cfg.Tag == "A" { // only the first (iso-A) construction blocks at the barrier
				_ = reg.Types() // prove the constructor can inspect the registry without deadlocking (C3/C4)
				close(buildStarted)
				<-release
			}
			return oneNode(t, "n", func(d *WorkflowData) error { d.Set("ran_tag", cfg.Tag); return nil }), nil
		}))
		require.NoError(t, reg.Register("free", func() (*DAG, error) {
			return oneNode(t, "n", func(d *WorkflowData) error { d.Set("ran_tag", "free"); return nil }), nil
		}))
		return reg
	}
	regA, regB := mkReg(), mkReg()

	// Enqueue in a fixed FIFO order: iso-A (oldest), free-1, iso-B.
	_, err := s1.Enqueue("iso-A", "iso", jsonInput(t, map[string]interface{}{"tag": "A"}))
	require.NoError(t, err)
	_, err = s1.Enqueue("free-1", "free", nil)
	require.NoError(t, err)
	_, err = s2.Enqueue("iso-B", "iso", jsonInput(t, map[string]interface{}{"tag": "B"}))
	require.NoError(t, err)

	// Worker A claims iso-A and blocks IN construction.
	aDone := make(chan error, 1)
	go func() { _, e := RunNext(context.Background(), s1, regA, "A"); aDone <- e }()
	select {
	case <-buildStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker A did not enter iso-A construction")
	}

	// While A is held, worker B (other handle) claims + runs the eligible "free" item to completion — capacity
	// is available for "free", and A's construction holds no lock/txn.
	ranFree, errFree := RunNext(context.Background(), s2, regB, "B")
	require.NoError(t, errFree)
	require.True(t, ranFree, "the free item progressed while A was held in iso construction")
	require.Equal(t, wqDone, wqState(t, s2, "free-1"))

	// A second attempt now finds only iso-B, which is AT the iso cap (iso-A is a running slot) → skipped,
	// leaving iso-B PENDING (backpressure, not a failure).
	ranCap, errCap := RunNext(context.Background(), s2, regB, "B")
	require.NoError(t, errCap)
	require.False(t, ranCap, "iso-B is at the per-type cap while iso-A runs → no claim")
	require.Equal(t, wqPending, wqState(t, s2, "iso-B"), "the capped candidate remains pending")

	// Release A → iso-A finishes; then iso-B can run (a slot freed).
	close(release)
	require.NoError(t, <-aDone)
	require.Equal(t, wqDone, wqState(t, s1, "iso-A"))

	ranB, errB := RunNext(context.Background(), s2, regB, "B")
	require.NoError(t, errB)
	require.True(t, ranB, "iso-B runs once the iso slot is free")
	require.Equal(t, wqDone, wqState(t, s2, "iso-B"))

	// Config isolation: each run observed only its OWN input — no shared mutable graph/input leakage.
	la, err := s1.Load("iso-A")
	require.NoError(t, err)
	gotA, _ := la.Get("ran_tag")
	require.Equal(t, "A", gotA, "iso-A ran with its own input")
	lb, err := s2.Load("iso-B")
	require.NoError(t, err)
	gotB, _ := lb.Get("ran_tag")
	require.Equal(t, "B", gotB, "iso-B ran with its own input (no leakage from iso-A)")
}

// A6 — W9 lease loss through the new factory path. Two handles over one queue: A claims (token 1) then stalls;
// the lease lapses; B RECLAIMS through the input-aware path (token 2 — bumped). While the row is still claimed
// under B, A's late failure-disposition (a stale-owner terminalization) is FENCED — refused by the token guard,
// so it cannot flip B's live row. B then rebuilds from the durable input and drives to done. The checkpoint-CAS
// fencing is the shared M16 mechanism, byte-unchanged by this additive feature.
func TestInputAware_A6_W9_LeaseLossOnNewPath(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s1, s2 := mkSharedStores(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	var inputsSeen []string
	reg := NewRegistry()
	require.NoError(t, reg.RegisterWithInput("leased", func(input []byte) (*DAG, error) {
		inputsSeen = append(inputsSeen, string(input))
		return oneNode(t, "n", func(d *WorkflowData) error { d.Set("winner", "B"); return nil }), nil
	}))
	input := jsonInput(t, map[string]interface{}{"tag": "leased"})

	_, err := s1.Enqueue("wf", "leased", input)
	require.NoError(t, err)
	// A claims (token 1) on handle 1 and then stalls (writes no journal).
	_, err = s1.ClaimNext("A", "leased")
	require.NoError(t, err)
	require.EqualValues(t, 1, leaseToken(t, s1, "wf"), "A holds token 1")

	// Lease lapses → B reclaims the lapsed-claimed row on the other handle (token 2 — A fenced). Still claimed.
	clk.Advance(6 * time.Second)
	item, err := s2.ClaimNext("B", "leased")
	require.NoError(t, err)
	require.EqualValues(t, 2, leaseToken(t, s2, "wf"), "the reclaim bumped the fencing token (A fenced)")
	require.EqualValues(t, 2, item.Token, "B holds the bumped token")
	require.Equal(t, wqClaimed, wqState(t, s2, "wf"), "the row is claimed under B")

	// A's LATE failure-disposition under its stale token 1 is FENCED — the row is still `claimed`, yet A's
	// terminalize is refused purely by the token guard (not merely because the row is already terminal).
	flipped, ferr := s1.MarkFailed("wf")
	require.NoError(t, ferr)
	require.False(t, flipped, "A's stale-owner terminalization is fenced (token 1 < durable token 2)")
	require.Equal(t, wqClaimed, wqState(t, s2, "wf"), "the stale owner did NOT flip B's live row")

	// B rebuilds from the DURABLE input (the new path) and drives to done — its outcome stands.
	entry, ok := reg.lookup(item.Type)
	require.True(t, ok)
	dag, ferr2 := entry.build(item.Input)
	require.NoError(t, ferr2)
	wB := &Workflow{dag: dag, WorkflowID: "wf", Store: s2}
	require.NoError(t, wB.Execute(context.Background()))
	done, err := s2.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, done, "B terminalizes done under its live token")
	require.Equal(t, wqDone, wqState(t, s2, "wf"))
	loaded, err := s2.Load("wf")
	require.NoError(t, err)
	winner, _ := loaded.Get("winner")
	require.Equal(t, "B", winner, "the durable journal is the successor's")
	require.NotEmpty(t, inputsSeen, "B rebuilt from the durable input")
}

// A7 — W10 the input-aware CHILD itself parks and resumes. Distinct parent/child payloads, an input-selected
// child gate. The CHILD (not just its parent) runs a first stage, durably PARKS on a wait-for-signal keyed by
// its input, then RESUMES through the input-aware path on reclaim after the signal arrives — preserving its
// progressed data, reaching the correct terminal queue outcome, and resolving the parent from that outcome.
func TestInputAware_A7_W10_InputAwareChildParksResumes(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	reg := NewRegistry()
	beforeRuns := newRunCounter()
	require.NoError(t, reg.RegisterWithInput("parkingChild", func(input []byte) (*DAG, error) {
		var cfg struct {
			Gate string `json:"gate"`
			Tag  string `json:"tag"`
		}
		if err := json.Unmarshal(input, &cfg); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		b := NewWorkflowBuilder()
		b.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			beforeRuns.inc("before")
			d.Set("child_progress", "ran-before:"+cfg.Tag)
			return nil
		}))
		b.AddWaitForSignal("gate", cfg.Gate).DependsOn("before")
		return b.Build()
	}))

	// Parent awaits the input-aware child (distinct child payload); parent parks.
	pb := NewWorkflowBuilder().WithWorkflowID("parent-wf")
	pb.AddSubWorkflowQueued("sub", "parkingChild").WithInput(map[string]any{"gate": "g1", "tag": "child-X"})
	pdag, err := pb.Build()
	require.NoError(t, err)
	pw := newWorkflowForTest(s)
	pw.WorkflowID = "parent-wf"
	pw.dag = pdag
	pw.registry = reg
	require.ErrorIs(t, pw.Execute(context.Background()), ErrSuspended, "parent enqueues child + parks")
	childID := SubWorkflowChildID("parent-wf", "sub")

	// The worker drives the child: it runs "before", then PARKS on the gate (ErrSuspended → row stays claimed).
	ran, rerr := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.NoError(t, rerr, "a park is not a failure")
	require.Equal(t, wqClaimed, wqState(t, s, childID), "the parked child row stays claimed")
	childData, err := s.Load(childID)
	require.NoError(t, err)
	assertNodeStatus(t, childData, "before", Completed)
	prog, ok := childData.Get("child_progress")
	require.True(t, ok)
	require.Equal(t, "ran-before:child-X", prog, "the child committed its first-stage progress before parking")
	require.Equal(t, 1, beforeRuns.get("before"))

	// Deliver the gate signal to the child, lapse the lease, and RECLAIM: the child resumes THROUGH the
	// input-aware path, consumes the signal, preserves its progress, and reaches done.
	require.NoError(t, s.DeliverSignal(childID, Signal{ID: "sig-g1", Name: "g1"}))
	clk.Advance(6 * time.Second)
	ran, rerr = RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.NoError(t, rerr, "the child resumed and completed")
	require.Equal(t, wqDone, wqState(t, s, childID), "the input-aware child reached its terminal queue outcome")
	require.Equal(t, 1, beforeRuns.get("before"), "the committed first stage was NOT re-run on resume")
	resumed, err := s.Load(childID)
	require.NoError(t, err)
	prog2, ok := resumed.Get("child_progress")
	require.True(t, ok)
	require.Equal(t, "ran-before:child-X", prog2, "the child's progressed data survived the park/resume")

	// The parent resolves from the child's DONE queue outcome (the worker delivered completion on child-done).
	perr := pw.Execute(context.Background())
	require.NoError(t, perr, "the parent resolves success from the child's done outcome")
	require.NotErrorIs(t, perr, ErrSuspended)
	final, err := s.Load("parent-wf")
	require.NoError(t, err)
	assertNodeStatus(t, final, "sub", Completed)
}

// A8 — W13 cancellation, drain and bounded retry on the new path.
//
//	(1) Operator cancellation BEFORE any action begins → the input-aware run terminalizes `cancelled`; no
//	    action runs and the constructor is not even reached (the post-claim cancel re-read precedes the build).
//	(2) A graceful drain leaves committed progress `claimed` (not dead-lettered); a later reclaim RESUMES it
//	    to done through the input-aware path.
//	(3) A constructor error wrapping an infrastructure class (ErrIO/ErrBusy) stays a TERMINAL validation
//	    failure — it is NOT requeued as retryable work. (The genuine bare-infra retry budget in disposeExecErr
//	    is registration-form-independent and covered by the shared MarkForRetry suites — cited in the receipt.)
func TestInputAware_A8_W13_CancelDrainRetryOnNewPath(t *testing.T) {
	t.Run("operator_cancel_before_action", func(t *testing.T) {
		s := mkDispatchStore(t)
		reg := NewRegistry()
		var built, ran atomic.Int32
		require.NoError(t, reg.RegisterWithInput("cancelAware", func([]byte) (*DAG, error) {
			built.Add(1)
			return oneNode(t, "n", func(*WorkflowData) error { ran.Add(1); return nil }), nil
		}))
		_, err := s.Enqueue("wf", "cancelAware", jsonInput(t, map[string]interface{}{"x": 1}))
		require.NoError(t, err)
		reqd, err := s.CancelRunning("wf")
		require.NoError(t, err)
		require.True(t, reqd, "operator cancel requested while pending")

		got, rerr := RunNext(context.Background(), s, reg, "worker")
		require.True(t, got, "the worker handled (terminalized) the item")
		require.NoError(t, rerr, "a cooperative cancel is not an error disposition")
		require.Equal(t, wqCancelled, wqState(t, s, "wf"), "the run terminalized cancelled")
		require.EqualValues(t, 0, ran.Load(), "no action ran")
		require.EqualValues(t, 0, built.Load(), "the constructor was not reached (cancel re-read precedes build)")
	})

	t.Run("drain_leaves_claimed_then_reclaim_resumes", func(t *testing.T) {
		clk := NewFakeClock(time.Unix(1000, 0))
		s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
		ctr := newRunCounter()
		mkReg := func() *Registry {
			reg := NewRegistry()
			require.NoError(t, reg.RegisterWithInput("drainAware", func([]byte) (*DAG, error) {
				d := newDAGForTest("drainAware")
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
		_, err := s.Enqueue("wf", "drainAware", jsonInput(t, map[string]interface{}{"x": 1}))
		require.NoError(t, err)
		// Model a graceful drain: A claims, commits partial progress (n0 done), and the row is LEFT claimed
		// (a drain leaves it claimed for later reclaim — NOT dead-lettered).
		_, err = s.ClaimNext("A", "drainAware")
		require.NoError(t, err)
		partial := NewWorkflowData("wf")
		partial.SetNodeStatus("n0", Completed)
		partial.SetNodeStatus("n1", Pending)
		require.NoError(t, s.Save(partial))
		require.Equal(t, wqClaimed, wqState(t, s, "wf"), "a drain leaves the row claimed, not failed")

		clk.Advance(6 * time.Second)
		ran, rerr := RunNext(context.Background(), s, mkReg(), "B")
		require.NoError(t, rerr)
		require.True(t, ran, "the drained run is reclaimed and resumed")
		require.Equal(t, wqDone, wqState(t, s, "wf"), "committed progress resumed to done (never dead-lettered)")
		require.Equal(t, 0, ctr.get("n0"), "the committed stage was not re-run")
		require.Equal(t, 1, ctr.get("n1"), "the pending stage resumed")
	})

	t.Run("constructor_error_wrapping_infra_stays_terminal", func(t *testing.T) {
		s := mkDispatchStore(t)
		reg := NewRegistry()
		require.NoError(t, reg.RegisterWithInput("ioAware", func([]byte) (*DAG, error) {
			return nil, fmt.Errorf("%w: transient-looking but a payload defect", ErrBusy)
		}))
		_, err := s.Enqueue("wf", "ioAware", jsonInput(t, map[string]interface{}{"x": 1}))
		require.NoError(t, err)
		ran, rerr := RunNext(context.Background(), s, reg, "worker")
		require.True(t, ran)
		require.Error(t, rerr)
		require.ErrorIs(t, rerr, ErrValidation, "a bad-payload constructor error is a terminal validation failure")
		require.Equal(t, wqFailed, wqState(t, s, "wf"), "terminal failed — NOT requeued as infra-retry")
		require.NotEqual(t, wqPending, wqState(t, s, "wf"), "the row was not requeued to pending")
	})
}
