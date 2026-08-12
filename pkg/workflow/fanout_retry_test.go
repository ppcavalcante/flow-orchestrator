package workflow

// M22 ph112 — per-branch retry hardening (HARD-02 / F-PG-10) adversarial suite. Five bites, each seed-break-proven
// (the comment on every test states what break turns it RED). The mechanism under test: WithBranchRetries wraps a
// fan-out branch's INNER action in a bounded RetryableAction (capped exponential backoff + jitter) BELOW the
// deterministic child-ID journal, so a failed branch re-drives WITHOUT re-expanding the fan-out and WITHOUT
// re-running succeeded siblings, and the result still persists exactly-once under the child's deterministic ID.
//
// Timing bites (a/b/d) use the InMemory store and a base wall-clock budget large enough to swamp scheduling noise
// (even under -race) yet far below the broken-path latency. Bite (e) is the store-seeded mid-retry resume variant
// (stated in its doc) mirroring the ph105/ph106 seed discipline (TestCollectPartial_PartialResume_F1Payoff).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// retryBranch builds a fan-out branch action: it reads its item (a json.Number, the branch's index) and runs
// body(ctx, idx). On a nil body result it writes the TYPED int64 result idx*10 under "out" (the WithResults key);
// a non-nil body error surfaces unchanged (the retry classifier + backoff act on it). A single closure instance is
// shared across a branch's retry attempts, so a body closing over per-index atomic counters observes each attempt.
func retryBranch(body func(ctx context.Context, idx int) error) Action {
	return ActionFunc(func(ctx context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("branch item is not a json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		idx := int(iv)
		if berr := body(ctx, idx); berr != nil {
			return berr
		}
		d.Set("out", int64(idx*10))
		return nil
	})
}

// --- (a) RETRY STORM BOUNDED: total attempts == count+1, and the maxDelay cap clamps the backoff --------------

// TestBranchRetry_StormBounded — an always-failing branch retries a BOUNDED number of times (exactly count+1
// attempts, never a tight loop nor unbounded) and the WithMaxDelay cap CLAMPS the exponential backoff. delay=10ms
// + backoff=10x would grow 10ms→100ms→1000ms (~1.11s total) UNCAPPED; a 15ms cap flattens it to ~40ms. SEED-BREAK:
// (1) drop the count+1 loop bound (attempt cap) → attempts != 4 → RED; (2) ignore r.maxDelay in Execute (:265) →
// elapsed climbs past 1.1s → the <400ms assertion goes RED.
func TestBranchRetry_StormBounded(t *testing.T) {
	const count = 3
	var attempts atomic.Int32
	branch := retryBranch(func(_ context.Context, _ int) error {
		attempts.Add(1)
		return errors.New("branch always fails")
	})

	b := NewWorkflowBuilder().WithWorkflowID("wf-retry-storm")
	b.AddFanOut("fan", intItemsExpander(1), branch).WithResults("r", "out").
		WithBranchRetries(count, 10*time.Millisecond, func(r *RetryableAction) {
			r.WithBackoff(10.0).WithMaxDelay(15 * time.Millisecond)
		})
	dag, err := b.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(NewInMemoryStore())
	w.WorkflowID = "wf-retry-storm"
	w.dag = dag

	start := time.Now()
	err = w.Execute(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err, "an always-failing branch exhausts its retries and fails the fan node (FailFast)")
	require.Equal(t, int32(count+1), attempts.Load(), "BOUNDED: total attempts == count+1, not a tight loop nor unbounded")
	require.Less(t, int64(elapsed), int64(400*time.Millisecond), "maxDelay cap CLAMPS the backoff (uncapped would be >1.1s)")
	require.Greater(t, int64(elapsed), int64(5*time.Millisecond), "real backoff sleeps happened (not a zero-wait tight loop)")
}

// --- (b) NON-RETRYABLE => EXACTLY 1 ATTEMPT: WithRetryIf(false) makes the error terminal, no backoff loop ------

// TestBranchRetry_NonRetryableExactlyOnce — a branch whose error is classified non-retryable (WithRetryIf
// returning false) is invoked EXACTLY ONCE, with NO backoff loop — even though count=5, delay=50ms would otherwise
// drive 6 attempts spanning >1.5s of backoff. SEED-BREAK: drop the `!r.retryIf(err)` break arm in Execute (:256) →
// attempts climbs to 6 and elapsed past a second → both assertions RED.
func TestBranchRetry_NonRetryableExactlyOnce(t *testing.T) {
	var attempts atomic.Int32
	branch := retryBranch(func(_ context.Context, _ int) error {
		attempts.Add(1)
		return errors.New("permanent, non-retryable failure")
	})

	b := NewWorkflowBuilder().WithWorkflowID("wf-retry-nonretryable")
	b.AddFanOut("fan", intItemsExpander(1), branch).WithResults("r", "out").
		WithBranchRetries(5, 50*time.Millisecond, func(r *RetryableAction) {
			r.WithRetryIf(func(error) bool { return false }) // classify EVERY error as terminal
		})
	dag, err := b.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(NewInMemoryStore())
	w.WorkflowID = "wf-retry-nonretryable"
	w.dag = dag

	start := time.Now()
	err = w.Execute(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err, "the non-retryable branch fails the fan node")
	require.Equal(t, int32(1), attempts.Load(), "NON-RETRYABLE => exactly 1 attempt, no backoff loop")
	require.Less(t, int64(elapsed), int64(200*time.Millisecond), "no backoff was entered (a retry loop would span >1.5s)")
}

// --- (c) PER-BRANCH re-drive, siblings untouched -------------------------------------------------------------

// TestBranchRetry_PerBranchRedriveSiblingsUntouched — N branches, only branch failIdx fails once then succeeds on
// retry. Assert: failIdx invoked EXACTLY twice, every sibling EXACTLY once (per-index atomic counters), the
// expander ran EXACTLY once (no fan-out re-expansion), and the discovery-order aggregate is intact + typed.
// SEED-BREAK: a re-drive that re-ran the whole fan-out level would re-invoke succeeded siblings (counter==2) and/or
// re-run the expander (expanderCalls>1) → RED.
func TestBranchRetry_PerBranchRedriveSiblingsUntouched(t *testing.T) {
	const (
		n       = 5
		failIdx = 2
	)
	counts := make([]atomic.Int32, n)
	var expanderCalls atomic.Int32
	branch := retryBranch(func(_ context.Context, idx int) error {
		if counts[idx].Add(1) == 1 && idx == failIdx {
			return fmt.Errorf("branch %d transient failure (first attempt)", idx)
		}
		return nil
	})

	b := NewWorkflowBuilder().WithWorkflowID("wf-retry-perbranch")
	b.AddFanOut("fan", intExpander(n, &expanderCalls), branch).WithResults("r", "out").
		WithBranchRetries(2, time.Millisecond)
	dag, err := b.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(NewInMemoryStore())
	w.WorkflowID = "wf-retry-perbranch"
	w.dag = dag
	require.NoError(t, w.Execute(context.Background()), "branch %d succeeds on retry → the run completes", failIdx)

	require.Equal(t, int32(1), expanderCalls.Load(), "expander ran ONCE — no fan-out re-expansion on a branch retry")
	for i := range n {
		want := int32(1)
		if i == failIdx {
			want = 2 // failed once, re-driven once
		}
		require.Equal(t, want, counts[i].Load(), "branch %d invoked exactly %d time(s)", i, want)
	}

	d, lerr := w.Store.Load("wf-retry-perbranch")
	require.NoError(t, lerr)
	require.Equal(t, n, fanCount(t, d, "r"))
	for i := range n {
		v, ok := d.Get(fanOutResultIndexKey("r", i))
		require.True(t, ok, "discovery-order aggregate carries index %d", i)
		require.Equal(t, int64(i*10), coerceInt64(t, v), "index %d result typed + in discovery order", i)
	}
}

// --- (d) FAILFAST LATENCY: a sibling FailFast cancels an in-flight branch backoff ----------------------------

// TestBranchRetry_FailFastCancelsInFlightBackoff — branch B fails transiently and enters a LONG (5s) retry
// backoff; branch A then fails PERMANENTLY (non-retryable → terminal after 1 attempt), tripping FailFast. The
// FailFast cancel must reach B's in-flight backoff select (RetryableAction Execute :277 select on ctx.Done) so B
// returns immediately — the whole run finishes in well under the 5s backoff. SEED-BREAK: replace that select with a
// plain time.Sleep(delay) (no ctx arm) → B sleeps the full 5s → the <2s assertion goes RED (a FailFast-latency
// regression). A signals off B's first-attempt entry so the cancel provably races an ALREADY in-flight backoff.
func TestBranchRetry_FailFastCancelsInFlightBackoff(t *testing.T) {
	const longDelay = 5 * time.Second
	errTransient := errors.New("branch B transient failure")
	bInBackoff := make(chan struct{})
	var once sync.Once

	branch := retryBranch(func(ctx context.Context, idx int) error {
		switch idx {
		case 1: // B: fail transiently on the first attempt → the RetryableAction enters its long backoff.
			once.Do(func() { close(bInBackoff) })
			return errTransient
		default: // A (index 0): wait until B is in backoff, then fail permanently → trips FailFast.
			select {
			case <-bInBackoff:
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second): // safety net; should never fire
			}
			return errors.New("A-permanent failure")
		}
	})

	b := NewWorkflowBuilder().WithWorkflowID("wf-retry-failfast")
	b.AddFanOut("fan", intItemsExpander(2), branch).WithResults("r", "out").
		WithBranchRetries(3, longDelay, func(r *RetryableAction) {
			// Only B's transient error retries; A's permanent error is terminal-fast (exactly 1 attempt).
			r.WithRetryIf(func(err error) bool { return errors.Is(err, errTransient) })
		})
	dag, err := b.Build()
	require.NoError(t, err)
	w := newWorkflowForTest(NewInMemoryStore())
	w.WorkflowID = "wf-retry-failfast"
	w.dag = dag

	start := time.Now()
	err = w.Execute(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err, "FailFast: A's permanent failure fails the fan node")
	require.Contains(t, err.Error(), "branch 0", "the surfaced root cause is A (branch 0), not B's cancellation")
	require.Contains(t, err.Error(), "A-permanent", "the surfaced root cause is A's real error, not context.Canceled")
	require.Less(t, int64(elapsed), int64(2*time.Second), "FailFast cancelled B's in-flight backoff (returned fast, not after the full 5s)")
}

// --- (e) CRASH MID-RETRY exactly-once-persisted (store-seeded resume variant) --------------------------------

// TestBranchRetry_CrashMidRetry_ExactlyOncePersisted — the store-seeded mid-retry resume variant (stated: NOT the
// 2-proc kill; the ph105/ph106 seed discipline, mirroring TestCollectPartial_PartialResume_F1Payoff). Seeds the
// durable expansion journal (so the expander MUST NOT re-run) + a sibling branch already durably Completed under
// its deterministic child-ID, with the fan node left NON-terminal. On resume the "crashed mid-retry" branch
// re-drives (fails once, succeeds on retry) and its result persists EXACTLY ONCE under the SAME deterministic
// child-ID; the at-least-once EXECUTION envelope stays bounded (count+1); the fan-out does NOT re-expand and the
// already-Completed sibling is NOT re-run. SEED-BREAK: a resume that re-expands (expanderCalls>0), re-runs a
// completed sibling (counts[doneIdx]>0), or double-persists under a fresh (non-deterministic) child-ID (aggregate
// or child result wrong) → RED.
func TestBranchRetry_CrashMidRetry_ExactlyOncePersisted(t *testing.T) {
	const (
		n        = 4
		crashIdx = 2 // "crashed mid-retry" — re-drives on resume
		doneIdx  = 0 // already durably Completed pre-crash — must NOT re-run
	)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // test cleanup, fire-and-forget
	parentID := "wf-crash-midretry"

	// Seed the durable expansion journal (resume reads it → the expander must NOT re-run) + sibling doneIdx's
	// already-flushed result. The fan node itself is left Pending (non-terminal) → a genuine resume.
	items := make([]json.RawMessage, n)
	for i := range items {
		items[i] = json.RawMessage(fmt.Sprintf("%d", i))
	}
	journal, merr := json.Marshal(fanOutJournal{N: n, Items: items})
	require.NoError(t, merr)
	seed := NewWorkflowData(parentID)
	seed.Set(fanOutItemsKey("fan"), string(journal))
	seed.Set(fanOutResultIndexKey("r", doneIdx), int64(doneIdx*10))
	require.NoError(t, store.Save(seed))

	// Pre-seed sibling doneIdx's child as Completed under its DETERMINISTIC id (driveBranch's terminal-fast-path).
	seedBranch := fanBranch(func(_ context.Context, idx int, _ interface{}) (interface{}, error) { return int64(idx * 10), nil })
	childDone := &Workflow{dag: seedBranch(doneIdx, doneIdx), WorkflowID: FanOutChildID(parentID, "fan", doneIdx), Store: store}
	require.NoError(t, childDone.Execute(context.Background()))

	// Resume: counting expander (proves no re-expansion) + per-index counters + the crashed branch failing once.
	counts := make([]atomic.Int32, n)
	var expanderCalls atomic.Int32
	branch := retryBranch(func(_ context.Context, idx int) error {
		if counts[idx].Add(1) == 1 && idx == crashIdx {
			return fmt.Errorf("branch %d mid-retry failure", idx)
		}
		return nil
	})
	b := NewWorkflowBuilder().WithWorkflowID(parentID)
	b.AddFanOut("fan", intExpander(n, &expanderCalls), branch).WithResults("r", "out").
		WithBranchRetries(2, time.Millisecond)
	dag, berr := b.Build()
	require.NoError(t, berr)
	w := newWorkflowForTest(store)
	w.WorkflowID = parentID
	w.dag = dag
	require.NoError(t, w.Execute(context.Background()), "resume re-drives the crashed branch and the fan-out completes")

	require.Equal(t, int32(0), expanderCalls.Load(), "expander NOT called on resume (journal read) — no re-expansion")
	require.Equal(t, int32(0), counts[doneIdx].Load(), "already-Completed sibling NOT re-run (deterministic-id terminal-fast-path)")
	require.Equal(t, int32(2), counts[crashIdx].Load(), "crashed branch re-drove within the bounded at-least-once envelope (count+1)")

	d, lerr := store.Load(parentID)
	require.NoError(t, lerr)
	assertNodeStatus(t, d, "fan", Completed)
	require.Equal(t, n, fanCount(t, d, "r"))
	for i := range n {
		v, ok := d.Get(fanOutResultIndexKey("r", i))
		require.True(t, ok, "index %d result persisted", i)
		require.Equal(t, int64(i*10), coerceInt64(t, v), "index %d result typed + exactly-once", i)
	}

	// EXACTLY-ONCE PERSISTENCE: the crashed branch's result lives under the SAME deterministic child-ID, once.
	childID := FanOutChildID(parentID, "fan", crashIdx)
	childData, cerr := store.Load(childID)
	require.NoError(t, cerr)
	require.True(t, childUnambiguouslyComplete(childData), "crashed branch durably Complete under its deterministic child-id")
	cv, ok := childData.Get("out")
	require.True(t, ok, "the crashed branch persisted its result under the deterministic child-id")
	require.Equal(t, int64(crashIdx*10), coerceInt64(t, cv), "single persisted result under the deterministic child-id")
}
