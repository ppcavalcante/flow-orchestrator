package workflow

// M17 ph81 — the dispatch mechanism proven: a registered WorkItem rebuilds + runs + terminalizes;
// the type filter claims only registered types; and the DETERMINISTIC reconciliation-seam (BLOCKER-1)
// proves the two-row (queue+journal) seam idempotent under a re-claim of a complete run — the whole
// reason it's owned here, before ph83's kill-storm. Deterministic (FakeClock lease-lapse, no kill-storm).

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func mkDispatchStore(t *testing.T, opts ...SQLiteOption) *SQLiteStore {
	t.Helper()
	all := append([]SQLiteOption{WithMultiProcess()}, opts...)
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "disp.db"), all...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

func jsonInput(t *testing.T, m map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// TestRegistry_RegisterGuards — dup / empty / nil-factory are rejected loud.
func TestRegistry_RegisterGuards(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register("A", func() (*DAG, error) { return NewDAG("a"), nil }))
	require.ErrorIs(t, r.Register("A", func() (*DAG, error) { return NewDAG("a2"), nil }), ErrValidation, "dup type rejected")
	require.ErrorIs(t, r.Register("", func() (*DAG, error) { return NewDAG("x"), nil }), ErrValidation, "empty type rejected")
	require.ErrorIs(t, r.Register("B", nil), ErrValidation, "nil factory rejected")
	require.ElementsMatch(t, []string{"A"}, r.Types())
}

// TestRunNext_EmptyRegistry_NoLeak (review ph81-F1) — a worker with NOTHING registered must NOT claim
// (an empty type filter would claim any type). RunNext short-circuits to ran=false, and the pending row
// stays pending (never stolen + stranded).
func TestRunNext_EmptyRegistry_NoLeak(t *testing.T) {
	s := mkDispatchStore(t)
	_, err := s.Enqueue("wf", "someType", nil)
	require.NoError(t, err)

	ran, err := RunNext(context.Background(), s, NewRegistry(), "worker")
	require.NoError(t, err)
	require.False(t, ran, "an empty registry claims NOTHING (F1: no unfiltered claim)")
	require.Equal(t, wqPending, wqState(t, s, "wf"), "the row stays pending — not stolen + stranded claimed")
}

// TestRunNext_SeedSaveError_Terminalizes (review ph81-F2) — a seed-Save failure MUST MarkFailed the row,
// never leak it `claimed`. We force the failure by closing the store's DB before RunNext so Save errors.
func TestRunNext_SeedSaveError_Terminalizes(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	var g string
	require.NoError(t, reg.Register("A", oneNodeReadingKey("k", &g)))
	_, err := s.Enqueue("wf", "A", jsonInput(t, map[string]interface{}{"k": "v"}))
	require.NoError(t, err)

	// Claim happens inside RunNext; to force the SEED Save (not the claim) to fail, we can't close the DB
	// (that fails the claim too). Instead: pre-create a trigger that RAISEs on the seed's data_kv insert.
	_, err = s.db.Exec(`CREATE TRIGGER seedboom BEFORE INSERT ON data_kv WHEN NEW.key='k'
	                    BEGIN SELECT RAISE(ABORT, 'seeded seed-Save failure'); END`)
	require.NoError(t, err)

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.Error(t, err, "the seed-Save failure surfaces")
	require.Contains(t, err.Error(), "seed save")
	require.Equal(t, wqFailed, wqState(t, s, "wf"), "F2: a seed-Save failure TERMINALIZES the row (MarkFailed), never leaks it claimed")
}

// oneNodeReadingKey builds a 1-node DAG whose action reads a seeded KV key and records it into `got`.
func oneNodeReadingKey(key string, got *string) DAGFactory {
	return func() (*DAG, error) {
		d := NewDAG("read")
		err := d.AddNode(NewNode("n0", ActionFunc(func(_ context.Context, data *WorkflowData) error {
			if v, ok := data.Get(key); ok {
				if s, ok := v.(string); ok {
					*got = s
				}
			}
			return nil
		})))
		return d, err
	}
}

// TestRunNext_EndToEnd_InputReachesDAG — a queued REGISTERED workflow is rebuilt + run to completion +
// MarkDone; the seeded Input reaches the DAG (the node reads it). Seed-break (differential): an item
// with NO input → the node reads absent → `got` stays "" (the input-seed is load-bearing).
func TestRunNext_EndToEnd_InputReachesDAG(t *testing.T) {
	s := mkDispatchStore(t)
	var got string
	reg := NewRegistry()
	require.NoError(t, reg.Register("greet", oneNodeReadingKey("name", &got)))

	// enqueue with input {"name":"Ada"} → the node must read "Ada".
	q, err := s.Enqueue("wf1", "greet", jsonInput(t, map[string]interface{}{"name": "Ada"}))
	require.NoError(t, err)
	require.True(t, q)

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran, "a claimable registered item ran")
	require.Equal(t, "Ada", got, "the seeded Input reached the DAG (node read name=Ada)")
	require.Equal(t, wqDone, wqState(t, s, "wf1"), "run completed → MarkDone (state=done)")

	// SEED-BREAK (differential): a fresh item with NO input → the node reads absent → got resets to "".
	got = ""
	q, err = s.Enqueue("wf2", "greet", nil)
	require.NoError(t, err)
	require.True(t, q)
	ran, err = RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, "", got, "SEED-BREAK: with no input seeded, the node reads absent — proving the input-seed is what makes the real path read the value")
}

// TestRunNext_TypeFilter_UnregisteredStaysPending — a worker registering only {A} leaves a queued {B}
// pending+visible; RunNext claims A. Seed-break note: dropping reg.Types() from the ClaimNext call would
// let {B} be claimed → factory lookup miss → the drift the filter prevents.
func TestRunNext_TypeFilter_UnregisteredStaysPending(t *testing.T) {
	s := mkDispatchStore(t)
	var gotA string
	reg := NewRegistry()
	require.NoError(t, reg.Register("A", oneNodeReadingKey("k", &gotA)))

	_, err := s.Enqueue("wfB", "B", nil) // an UNregistered type
	require.NoError(t, err)
	_, err = s.Enqueue("wfA", "A", jsonInput(t, map[string]interface{}{"k": "v"}))
	require.NoError(t, err)

	// RunNext claims only A (the registered type); B is skipped.
	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, "v", gotA)
	require.Equal(t, wqDone, wqState(t, s, "wfA"))

	// B is still pending + visible — NOT claimed (the type filter), no claim-fail-release loop.
	require.Equal(t, wqPending, wqState(t, s, "wfB"), "unregistered {B} stays pending+visible")
	// a second RunNext finds no MORE registered work → ran=false (B is filtered out → ErrNoWork).
	ran, err = RunNext(context.Background(), s, reg, "worker")
	require.NoError(t, err)
	require.False(t, ran, "no more registered work → ran=false (B not claimed)")
}

// TestRunNext_ReconciliationSeam_Idempotent — [BLOCKER-1] the deterministic two-row (queue+journal) seam:
// a COMPLETE journal + a `claimed` queue row + a lapsed lease → re-claim (token N+1) → re-Execute is a
// NO-OP (terminal nodes skipped), MarkDone flips EXACTLY ONCE (ph80 CAS), and a second re-claim of the
// now-`done` row is REFUSED by C2. Mirrors the M16 token-seam (FakeClock lapse, NO kill-storm).
func TestRunNext_ReconciliationSeam_Idempotent(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))

	// A registered 2-node chain n0->n1; a run-counter proves the seam does NOT re-execute.
	ctr := newRunCounter()
	reg := NewRegistry()
	require.NoError(t, reg.Register("chain", func() (*DAG, error) {
		d := NewDAG("chain")
		if err := d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n0"); return nil }))); err != nil {
			return nil, err
		}
		if err := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n1"); return nil }))); err != nil {
			return nil, err
		}
		return d, d.AddDependency("n0", "n1")
	}))

	// Manually stage the seam: a COMPLETE journal (both nodes Completed) + a `claimed` queue row + a
	// claim (token 1) held by a "dead" worker A — modeling: A ran the whole workflow + committed the
	// journal, flipped the queue to claimed, then died BEFORE MarkDone (the crash window the seam heals).
	_, err := s.Enqueue("wf", "chain", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("A") // A claims → queue row `claimed`, lease token 1
	require.NoError(t, err)
	complete := NewWorkflowData("wf")
	complete.SetNodeStatus("n0", Completed)
	complete.SetNodeStatus("n1", Completed)
	require.NoError(t, s.Save(complete), "A durably committed the COMPLETE journal before dying")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"))

	// A "dies": lapse the lease. The queue row is still `claimed` (a claimed row is NOT re-claimable via
	// ClaimNext in ph80 — that scan-broadening for lapsed-claimed rows is ph82/DISP-05). The RECONCILIATION
	// the seam heals is: B re-claims the workflow's LEASE (lapsed → token 2), rebuilds + re-Executes the
	// already-complete journal, and terminalizes the row — proving the drive is idempotent + the CAS flip
	// fires exactly once, WITHOUT needing ph82's queue-reclaim. This mirrors the M16 token-seam structure.
	clk.Advance(6 * time.Second)
	tokB, err := s.Claim("wf", "B") // B re-claims the lapsed LEASE → token 2 (fences A)
	require.NoError(t, err)
	require.Equal(t, FencingToken(2), tokB, "the lapsed lease re-claim bumped the token (A fenced)")

	// B rebuilds the DAG (same registry) + re-Executes on the SAME store (checkpoint CAS reads token 2).
	dag, ferr := reg.factories["chain"]()
	require.NoError(t, ferr)
	wB := &Workflow{DAG: dag, WorkflowID: "wf", Store: s}
	require.NoError(t, wB.Execute(context.Background()), "re-Execute of a complete journal is a clean NO-OP")

	// (i) Execute NO-OPS: neither node re-executed (both were terminal in the loaded journal → skipped).
	require.Equal(t, 0, ctr.get("n0"), "seam: n0 NOT re-executed (terminal in the journal)")
	require.Equal(t, 0, ctr.get("n1"), "seam: n1 NOT re-executed")

	// (ii) MarkDone flips claimed->done EXACTLY ONCE (ph80 CAS-guard). First flip: true.
	flipped, err := s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "the first claimed->done flip lands")
	require.Equal(t, wqDone, wqState(t, s, "wf"))
	// A SECOND MarkDone (a re-reconciliation) is a detectable 0-row no-op — the flip fired exactly once.
	flipped, err = s.MarkDone("wf")
	require.NoError(t, err)
	require.False(t, flipped, "the CAS guard makes a second claimed->done a 0-row no-op — flipped exactly once")

	// (iii) a second re-claim of the now-`done` row is REFUSED by C2 (never returned by ClaimNext).
	_, err = s.ClaimNext("B")
	require.ErrorIs(t, err, ErrNoWork, "C2: the done row is NEVER re-claimed")
}

// TestRunNext_InputBearingReClaim_NoReSeed (architect-pinned condition; review ph81-F3) — the input
// pre-Save is CONDITIONAL ON A FRESH RUN. An item that already has a journal (partial progress from a
// first worker that seeded + ran + died) MUST NOT be re-seeded on re-run — re-seeding would blind-overwrite
// the journal → lost work / double-apply. This proves the conditional-fresh gate is load-bearing.
func TestRunNext_InputBearingReClaim_NoReSeed(t *testing.T) {
	s := mkDispatchStore(t)
	ctr := newRunCounter()
	reg := NewRegistry()
	// A 2-node chain n0->n1; n0 records that it ran + reads the seeded input key.
	var sawInput string
	require.NoError(t, reg.Register("chain", func() (*DAG, error) {
		d := NewDAG("chain")
		if err := d.AddNode(NewNode("n0", ActionFunc(func(_ context.Context, data *WorkflowData) error {
			ctr.inc("n0")
			if v, ok := data.Get("k"); ok {
				if sv, ok := v.(string); ok {
					sawInput = sv
				}
			}
			return nil
		}))); err != nil {
			return nil, err
		}
		if err := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { ctr.inc("n1"); return nil }))); err != nil {
			return nil, err
		}
		return d, d.AddDependency("n0", "n1")
	}))

	// Construct a PENDING queue row that ALREADY has a journal with partial progress (n0 Completed + input +
	// a side-effect marker a blind re-seed would erase). This is the state RunNext's seed gate must respect:
	// its Load finds a journal → it must SKIP the seed. (In ph80 a pending row doesn't normally co-exist with
	// a journal — we stage it manually to isolate RunNext's gate, the exact scenario ph82's reclaim creates.)
	partial := NewWorkflowData("wf")
	partial.Set("k", "v-ORIGINAL")         // the ORIGINAL seeded input in the journal
	partial.Set("n0_ran", "yes")           // a side-effect marker a blind re-seed would wipe
	partial.SetNodeStatus("n0", Completed) // n0 already ran under the prior worker (committed frontier)
	require.NoError(t, s.Save(partial), "prior partial journal: n0 Completed + input + a side-effect marker")
	// The pending queue row carries a DIFFERENT input — if RunNext wrongly re-seeded, it'd overwrite the
	// journal's "v-ORIGINAL" with "v-RESEED" AND wipe the marker + n0's Completed status.
	_, err := s.Enqueue("wf", "chain", jsonInput(t, map[string]interface{}{"k": "v-RESEED"}))
	require.NoError(t, err)

	// RunNext claims the pending row → Load finds the journal → the conditional-fresh gate SKIPS the seed →
	// n0 (terminal) is NOT re-run, only n1 runs, and the ORIGINAL journal (input + marker) survives.
	ran, err := RunNext(context.Background(), s, reg, "B")
	require.NoError(t, err)
	require.True(t, ran)

	require.Equal(t, 0, ctr.get("n0"), "n0 NOT re-run (terminal in the committed frontier) — RunNext did not reset progress")
	require.Equal(t, 1, ctr.get("n1"), "n1 runs (the remaining work)")

	got, err := s.Load("wf")
	require.NoError(t, err)
	marker, ok := got.GetString("n0_ran")
	require.True(t, ok, "the side-effect marker SURVIVED — RunNext did NOT re-seed / blind-overwrite the journal")
	require.Equal(t, "yes", marker)
	k, _ := got.GetString("k")
	require.Equal(t, "v-ORIGINAL", k, "the ORIGINAL input survived — RunNext did NOT overwrite it with the re-claim's v-RESEED")
	_ = sawInput
}

// TestRunNext_TopologyDrift_SurfacesErrValidation — a claimed workflow whose rebuilt DAG's node-set
// drifted from the persisted journal → checkGraphIdentity returns ErrValidation at Execute (SURFACED
// here; its attempt-consuming dead-letter is ph82). RunNext propagates it + MarkFailed.
func TestRunNext_TopologyDrift_SurfacesErrValidation(t *testing.T) {
	s := mkDispatchStore(t)
	reg := NewRegistry()
	// the factory builds a DAG with node "nX" ONLY.
	require.NoError(t, reg.Register("drift", func() (*DAG, error) {
		d := NewDAG("drift")
		return d, d.AddNode(NewNode("nX", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	}))

	_, err := s.Enqueue("wf", "drift", nil)
	require.NoError(t, err)
	// Pre-seed a journal referencing a node ("nGONE") NOT in the rebuilt DAG → identity drift.
	drifted := NewWorkflowData("wf")
	drifted.SetNodeStatus("nGONE", Completed)
	require.NoError(t, s.Save(drifted))

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.ErrorIs(t, err, ErrValidation, "topology drift surfaces ErrValidation at Execute (checkGraphIdentity)")
	require.Equal(t, wqFailed, wqState(t, s, "wf"), "the drift terminalizes the row (MarkFailed); its dead-letter is ph82")
}

// TestRunNext_CorruptLoad_Terminalizes (F-M17-P81-QA-1, ph82 deliverable 6) — the MIDDLE arm of RunNext's
// 3-armed seed-freshness gate (ErrNotFound→seed / CORRUPT→MarkFailed / else→skip) gets a STANDING test.
// qa proved it reachable-by-construction in ph81 but noted the arm had no in-tree test. The reachability
// SUBTLETY (why this needs a corrupt workflows-ANCHOR, not a nodes-only forge): Load returns ErrNotFound
// when the workflows anchor row is ABSENT — so a bare child-row forge yields ErrNotFound (→ the seed arm),
// NOT ErrCorruptData. We must forge a PRESENT anchor + a corrupt child so Load reaches the codec's
// unknown-kind path and returns ErrCorruptData → RunNext MarkFailed's WITHOUT seeding over the bad read.
func TestRunNext_CorruptLoad_Terminalizes(t *testing.T) {
	s := mkDispatchStore(t)
	var got string
	reg := NewRegistry()
	require.NoError(t, reg.Register("A", oneNodeReadingKey("k", &got)))

	// Enqueue WITH input so RunNext performs the freshness Load (the gate only Loads when len(Input)>0).
	_, err := s.Enqueue("wf", "A", jsonInput(t, map[string]interface{}{"k": "v"}))
	require.NoError(t, err)

	// Forge a CORRUPT journal for "wf": a PRESENT workflows anchor (so Load ≠ ErrNotFound) + a data_kv row
	// with an INVALID kind (999 ∉ {kvInt,kvBool,kvFloat,kvString}) → Load hits the codec unknown-kind path
	// → ErrCorruptData. This is the reachability the QA-1 note calls out: anchor present, child corrupt.
	_, err = s.db.Exec(`INSERT INTO workflows (id, rolling_back, trigger_cause, updated_at) VALUES ('wf',0,0,0)`)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO data_kv (workflow_id, key, kind, i_val, f_val, s_val) VALUES ('wf','bad',999,0,0,'')`)
	require.NoError(t, err)

	// Sanity: the forge really produces ErrCorruptData on Load (pins the arm's precondition).
	_, lerr := s.Load("wf")
	require.ErrorIs(t, lerr, ErrCorruptData, "forge precondition: a present anchor + bad-kind child → ErrCorruptData (not ErrNotFound)")

	ran, err := RunNext(context.Background(), s, reg, "worker")
	require.True(t, ran)
	require.ErrorIs(t, err, ErrCorruptData, "the corrupt-Load freshness check surfaces ErrCorruptData (the middle arm)")
	require.Contains(t, err.Error(), "seed freshness-check load")
	require.Equal(t, wqFailed, wqState(t, s, "wf"), "the corrupt-Load arm TERMINALIZES the row (MarkFailed) — never seeds over a bad read")
	require.Equal(t, "", got, "the node never ran — RunNext bailed at the freshness check BEFORE Execute")
}

// fenceReclaimSetup stages the ph82-F1 two-owner window: A claims "wf" (token 1, row claimed), stalls past
// TTL, B reclaims via the D1 broadened scan (token 2, row stays claimed). Returns the store + FakeClock.
// Shared by the two F1 guard tests so each asserts ONE guard in isolation (guard-must-bite discipline).
func fenceReclaimSetup(t *testing.T) *SQLiteStore {
	t.Helper()
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("wf", "chain", nil)
	require.NoError(t, err)
	itemA, err := s.ClaimNext("A")
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), itemA.Token, "A holds token 1")
	clk.Advance(6 * time.Second) // A stalls past TTL
	itemB, err := s.ClaimNext("B")
	require.NoError(t, err)
	require.Equal(t, "wf", itemB.WorkflowID, "B's broadened ClaimNext DISCOVERS the lapsed-claimed row (D1)")
	require.Equal(t, FencingToken(2), itemB.Token, "the reclaim bumped the token (A fenced)")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"), "the reclaimed row stays claimed under B (D1)")
	return s
}

// TestFencedWorker_StructuralTokenGuard (review ph82-F1, guard (i) IN ISOLATION) — a SUPERSEDED worker A's
// terminal/retry flips under its STALE token are 0-row no-ops (the `fencing_token <= held` EXISTS subquery),
// so A can NEVER clobber the live reclaimer B's queue row. Isolates the STRUCTURAL guard: it drives the flip
// primitives DIRECTLY (not via disposeExecErr), so removing the EXISTS guard reddens THIS test alone.
// Seed-break: drop the `AND EXISTS(… fencing_token <= ?)` from flipTerminalFenced/MarkForRetry → A's flip
// lands → reddens.
func TestFencedWorker_StructuralTokenGuard(t *testing.T) {
	s := fenceReclaimSetup(t)

	// A holds the STALE token 1 (B's reclaim bumped durable current to 2). Model A's view + attempt the flips.
	s.setToken("wf", 1)
	flipped, err := s.MarkFailed("wf")
	require.NoError(t, err)
	require.False(t, flipped, "guard(i): A's stale-token MarkFailed is a 0-row no-op — cannot flip B's row to failed")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"), "row STILL claimed (B's) — not clobbered")

	requeued, err := s.MarkForRetry("wf", defaultMaxAttempts)
	require.NoError(t, err)
	require.False(t, requeued, "guard(i): A's stale-token MarkForRetry is a 0-row no-op — cannot requeue B's row")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"), "row STILL claimed (B's) — not requeued out from under B")

	// B (current token 2) terminalizes its OWN row normally — proving the guard blocks ONLY the stale worker.
	s.setToken("wf", 2)
	flipped, err = s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "B (current token) terminalizes its own row → done")
	require.Equal(t, wqDone, wqState(t, s, "wf"))
}

// TestFencedWorker_ClassifierAborts (review ph82-F1, guard (ii) IN ISOLATION) — disposeExecErr on
// ErrFencedOut aborts with NO queue transition. Isolated from guard (i) by holding the CURRENT token, so a
// bare MarkFailed WOULD flip the row — proving the CLASSIFIER (not the structural guard) is what prevents
// the transition. Seed-break: remove the isSupersededError(execErr) early-return in disposeExecErr → it
// falls to the fail-closed MarkFailed → the row flips → reddens.
func TestFencedWorker_ClassifierAborts(t *testing.T) {
	s := fenceReclaimSetup(t)

	// Hold the CURRENT token (2) — so the structural guard (i) would ALLOW a flip. This isolates guard (ii):
	// the ONLY thing that must stop the transition here is disposeExecErr's ErrFencedOut early-return. (In the
	// real reclaim A holds the stale token; we hold current here precisely to DISARM guard (i) and prove the
	// classifier stands on its own — otherwise guard (i) would mask a broken classifier, the defense-in-depth
	// vacuity trap, [[guard-must-bite-seed-the-break]].)
	s.setToken("wf", 2)
	derr := disposeExecErr(s, "wf", defaultMaxAttempts, fmt.Errorf("checkpoint: %w", ErrFencedOut))
	require.ErrorIs(t, derr, ErrFencedOut, "the superseded error is surfaced")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"),
		"guard(ii): ErrFencedOut → disposeExecErr aborts, NO queue transition — even though the current token would ALLOW a flip")

	// Control: with the SAME current token, a NON-superseded error (poison) DOES flip (proving the guard is
	// specific to ErrFencedOut, not blocking all transitions — the assert above isn't vacuously true).
	control := disposeExecErr(s, "wf", defaultMaxAttempts, newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("boom")}}))
	require.Error(t, control)
	require.Equal(t, wqFailed, wqState(t, s, "wf"), "control: a poison *ExecutionError under the SAME token DOES MarkFailed — the abort is ErrFencedOut-specific")
}

// TestMarkForRetry_Boundaries (review ph82-F2) — the DF-4 requeue primitive's decision surface: it requeues
// claimed→pending IFF the row is claimed AND attempts<maxAttempts AND the caller holds the current token.
func TestMarkForRetry_Boundaries(t *testing.T) {
	t.Run("under_budget_requeues", func(t *testing.T) {
		s := mkDispatchStore(t)
		_, err := s.Enqueue("wf", "T", nil)
		require.NoError(t, err)
		_, err = s.ClaimNext("w") // claimed, attempts=1, token 1
		require.NoError(t, err)
		requeued, err := s.MarkForRetry("wf", 3) // 1 < 3 → requeue
		require.NoError(t, err)
		require.True(t, requeued, "attempts(1) < max(3) → requeued claimed→pending")
		require.Equal(t, wqPending, wqState(t, s, "wf"))
	})
	t.Run("budget_exhausted_noop", func(t *testing.T) {
		s := mkDispatchStore(t)
		_, err := s.Enqueue("wf", "T", nil)
		require.NoError(t, err)
		_, err = s.ClaimNext("w") // attempts=1
		require.NoError(t, err)
		requeued, err := s.MarkForRetry("wf", 1) // 1 < 1 false → no requeue (budget spent)
		require.NoError(t, err)
		require.False(t, requeued, "attempts(1) >= max(1) → NOT requeued (caller dead-letters)")
		require.Equal(t, wqClaimed, wqState(t, s, "wf"), "row stays claimed — caller MarkFailed's next")
	})
	t.Run("not_claimed_noop", func(t *testing.T) {
		s := mkDispatchStore(t)
		_, err := s.Enqueue("wf", "T", nil) // pending, never claimed
		require.NoError(t, err)
		requeued, err := s.MarkForRetry("wf", 3)
		require.NoError(t, err)
		require.False(t, requeued, "a non-claimed row is never requeued")
		require.Equal(t, wqPending, wqState(t, s, "wf"))
	})
	t.Run("stale_token_noop", func(t *testing.T) {
		s := fenceReclaimSetup(t) // A(token1) fenced, B(token2) owns the claimed row
		s.setToken("wf", 1)       // model stale A
		requeued, err := s.MarkForRetry("wf", 99)
		require.NoError(t, err)
		require.False(t, requeued, "guard: a stale-token requeue is a no-op — cannot requeue B's row")
		require.Equal(t, wqClaimed, wqState(t, s, "wf"))
	})
}

// TestDisposeExecErr_Classification (review ph82-F2, DF-4) — the retry classifier's full decision table on a
// freshly-claimed row (attempts=1, budget 3): retryable infra → requeue; poison/drift/unclassified → fail;
// superseded → abort-no-transition. Each class asserted independently (guard-must-bite: a mis-routed class
// would land the wrong terminal state).
func TestDisposeExecErr_Classification(t *testing.T) {
	claimFresh := func(t *testing.T) *SQLiteStore {
		t.Helper()
		s := mkDispatchStore(t)
		_, err := s.Enqueue("wf", "T", nil)
		require.NoError(t, err)
		_, err = s.ClaimNext("w") // claimed, attempts=1, token 1 (current)
		require.NoError(t, err)
		return s
	}
	cases := []struct {
		name  string
		err   error
		final string // expected work_queue state after disposeExecErr
	}{
		{"ErrBusy_retries", fmt.Errorf("cp: %w", ErrBusy), wqPending},                                                    // infra → requeue
		{"ErrIO_retries", fmt.Errorf("cp: %w", ErrIO), wqPending},                                                        // infra → requeue
		{"ExecutionError_poison_fails", newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("x")}}), wqFailed}, // poison → fail
		{"ErrValidation_drift_fails", fmt.Errorf("drift: %w", ErrValidation), wqFailed},                                  // drift → fail (MAJOR-3)
		{"unclassified_fails", fmt.Errorf("some unknown fault"), wqFailed},                                               // fail-closed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := claimFresh(t)
			require.Error(t, disposeExecErr(s, "wf", 3, tc.err), "disposeExecErr always surfaces the run failure")
			require.Equal(t, tc.final, wqState(t, s, "wf"), tc.name)
		})
	}

	// Poison-precedence: an *ExecutionError WRAPPING an ErrIO cause must still be POISON (fail), not retried —
	// ExecutionError.Unwrap exposes the cause to errors.Is, so this pins the errors.As-first ordering.
	t.Run("ExecutionError_wrapping_ErrIO_is_poison_not_retry", func(t *testing.T) {
		s := claimFresh(t)
		require.Error(t, disposeExecErr(s, "wf", 3, newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("node io: %w", ErrIO)}})))
		require.Equal(t, wqFailed, wqState(t, s, "wf"), "a node failing with an ErrIO cause is STILL poison (errors.As(*ExecutionError) first) — NOT requeued")
	})

	// Poison-precedence for the AF1 cancel-guard (review ph82-AF1-TAIL-F1, HIGH regression): a NODE returning
	// context.Canceled/DeadlineExceeded from its OWN internal timeout while the PARENT ctx is LIVE surfaces as
	// an *ExecutionError (dag.go drops the ExecutionError only on a PARENT-ctx cancel). ExecutionError.Unwrap
	// exposes that cause to errors.Is, so a BARE errors.Is(context.DeadlineExceeded) in the AF1 guard would
	// TRUE-positive and wrongly LEAVE-CLAIMED genuine node poison → an UNBOUNDED TTL-reclaim loop. The
	// errors.As(*ExecutionError)-FIRST gate keeps it POISON → dead-lettered. Both the Canceled + Deadline arms.
	// Seed-break: drop the `!errors.As(&xe)` gate on the cancel-guard → these two flip to wqClaimed → redden.
	t.Run("ExecutionError_wrapping_DeadlineExceeded_is_poison_not_shutdown", func(t *testing.T) {
		s := claimFresh(t)
		require.Error(t, disposeExecErr(s, "wf", 3, newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("node deadline: %w", context.DeadlineExceeded)}})))
		require.Equal(t, wqFailed, wqState(t, s, "wf"), "a node failing via its OWN deadline is POISON (errors.As first), NOT a shutdown — dead-lettered, never left-claimed (would loop forever)")
	})
	t.Run("ExecutionError_wrapping_Canceled_is_poison_not_shutdown", func(t *testing.T) {
		s := claimFresh(t)
		require.Error(t, disposeExecErr(s, "wf", 3, newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("node cancel: %w", context.Canceled)}})))
		require.Equal(t, wqFailed, wqState(t, s, "wf"), "a node failing via its OWN cancel is POISON (errors.As first), NOT a shutdown — dead-lettered")
	})

	// The intended AF1 case: a BARE (non-*ExecutionError) parent-ctx shutdown → leave-claimed (NOT failed).
	// This is what dag.go returns when the PARENT ctx is cancelled. Confirms the guard still fires for its
	// real target after the poison-first gate.
	t.Run("bare_Canceled_is_shutdown_leaves_claimed", func(t *testing.T) {
		s := claimFresh(t)
		require.Error(t, disposeExecErr(s, "wf", 3, fmt.Errorf("workflow cancelled during level 0: %w", context.Canceled)))
		require.Equal(t, wqClaimed, wqState(t, s, "wf"), "a bare parent-ctx shutdown leaves the row claimed for TTL-reclaim (at-least-once), NOT failed")
	})
}

// TestClaimNext_ReclaimBroadening_DiscoversLapsedClaimed (review ph82-F2, D1) — the store-level reclaim
// crux: ClaimNext's broadened scan discovers a lapsed-`claimed` row (not just pending), re-claims it under a
// bumped token, keeps state='claimed', bumps attempts. Seed-break: narrow the scan to state='pending' → the
// lapsed row is never discovered → ErrNoWork → reddens.
func TestClaimNext_ReclaimBroadening_DiscoversLapsedClaimed(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s := mkDispatchStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	itemA, err := s.ClaimNext("A") // token 1, claimed
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), itemA.Token)

	// Before lapse: the claimed row is NOT re-claimable (live lease).
	_, err = s.ClaimNext("B")
	require.ErrorIs(t, err, ErrNoWork, "a LIVE-lease claimed row is never offered (fencing candidate filter)")

	// After lapse: B's broadened scan DISCOVERS it → reclaim, token 2, still claimed.
	clk.Advance(6 * time.Second)
	itemB, err := s.ClaimNext("B")
	require.NoError(t, err)
	require.Equal(t, "wf", itemB.WorkflowID, "the lapsed-claimed row is discovered by the broadened scan (D1)")
	require.Equal(t, FencingToken(2), itemB.Token, "reclaim bumped the token")
	require.Equal(t, wqClaimed, wqState(t, s, "wf"), "the reclaimed row stays claimed")
}

// TestListPending_And_Cancel (D5, DEC-M17-STUCKVIS + DEC-M17-CANCEL) — the stuck-work visibility query +
// cancel-pending. ListPending returns pending rows FIFO (all / age-filtered), excludes claimed/terminal
// rows, and surfaces an unregistered (stuck) type. CancelPending flips pending→cancelled; a claimed row
// is REJECTED (never a mid-flight interrupt).
func TestListPending_And_Cancel(t *testing.T) {
	s := mkDispatchStore(t)
	// Three pending items at increasing enqueue times; a helper stamps enqueued_at deterministically.
	stamp := func(id, typ string, at int64) {
		_, err := s.db.Exec(
			`INSERT INTO work_queue(workflow_id,type,input,enqueued_at,state,attempts,updated_at) VALUES (?,?,NULL,?, 'pending',0,?)`,
			id, typ, at, at)
		require.NoError(t, err)
	}
	stamp("wfA", "A", 100)
	stamp("wfB", "B", 200) // "B" is an UNREGISTERED (stuck) type — still visible via ListPending
	stamp("wfC", "A", 300)

	// All pending, FIFO by enqueued_at.
	all, err := s.ListPending(0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []string{"wfA", "wfB", "wfC"}, []string{all[0].WorkflowID, all[1].WorkflowID, all[2].WorkflowID}, "FIFO by enqueued_at")
	require.Equal(t, "B", all[1].Type, "the stuck unregistered type is surfaced (operator can spot it)")

	// Age filter: only items enqueued at/before 200 → wfA, wfB.
	old, err := s.ListPending(200)
	require.NoError(t, err)
	require.Len(t, old, 2)
	require.Equal(t, []string{"wfA", "wfB"}, []string{old[0].WorkflowID, old[1].WorkflowID}, "age filter keeps the older two")

	// A CLAIMED row is NOT pending → excluded from ListPending.
	_, err = s.ClaimNext("w", "A") // claims wfA (oldest A-type)
	require.NoError(t, err)
	afterClaim, err := s.ListPending(0)
	require.NoError(t, err)
	require.Equal(t, []string{"wfB", "wfC"}, idsOf(afterClaim), "a claimed row drops out of ListPending")

	// CancelPending: a PENDING row → cancelled (removed from ListPending).
	cancelled, err := s.CancelPending("wfB")
	require.NoError(t, err)
	require.True(t, cancelled, "a pending row is cancellable")
	require.Equal(t, wqCancelled, wqState(t, s, "wfB"))
	// A CLAIMED row → cancel REJECTED (DEC-M17-CANCEL — no mid-flight interrupt).
	cancelled, err = s.CancelPending("wfA")
	require.NoError(t, err)
	require.False(t, cancelled, "a claimed row is NOT cancellable (0-row no-op)")
	require.Equal(t, wqClaimed, wqState(t, s, "wfA"), "the claimed row is untouched")

	final, err := s.ListPending(0)
	require.NoError(t, err)
	require.Equal(t, []string{"wfC"}, idsOf(final), "only wfC remains pending (wfA claimed, wfB cancelled)")
}

// idsOf projects the workflow IDs of a PendingItem slice (test helper).
func idsOf(items []PendingItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.WorkflowID
	}
	return out
}
