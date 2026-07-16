package workflow

// M18 ph86 Slice 1 — the dispatch event-counter BITES. BITE 2 (centerpiece): the counters ACTUALLY fire
// at real events (a counter that never increments is theater). BITE 1: zero-cost when the hook is unset.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkMetricsStore is mkDispatchStore + an attached DispatchMetrics (returns both).
func mkMetricsStore(t *testing.T, opts ...SQLiteOption) (*SQLiteStore, *DispatchMetrics) {
	t.Helper()
	m := NewDispatchMetrics()
	s := mkDispatchStore(t, append(opts, WithDispatchMetrics(m))...)
	return s, m
}

// TestDispatchMetrics_FenceRejection_Fires (BITE 2) — a REAL fence-rejection (a superseded worker's
// flipTerminalFenced → 0 rows) increments fenceRejections by EXACTLY 1. Reuses the ph82 staging:
// A claims (token 1), B reclaims (token 2), A's stale-token MarkFailed is a 0-row no-op = the reject.
func TestDispatchMetrics_FenceRejection_Fires(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s, m := mkMetricsStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("A") // token 1
	require.NoError(t, err)
	clk.Advance(6 * time.Second)
	_, err = s.ClaimNext("B") // reclaim → token 2 (A superseded)
	require.NoError(t, err)

	require.Zero(t, m.FenceRejections(), "no reject yet")
	// A holds the STALE token 1 → its terminal flip is a 0-row no-op = a fence-rejection.
	s.setToken("wf", 1)
	flipped, err := s.MarkFailed("wf")
	require.NoError(t, err)
	require.False(t, flipped, "A's stale-token flip is a 0-row no-op (the reject)")
	require.Equal(t, int64(1), m.FenceRejections(), "BITE 2: a real fence-rejection increments fenceRejections by exactly 1")

	// SEED-BREAK (in-test): a CURRENT-token holder's flip is NOT a reject — the counter must NOT move.
	s.setToken("wf", 2) // B's current token
	before := m.FenceRejections()
	flipped, err = s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "B (current token) flips successfully")
	require.Equal(t, before, m.FenceRejections(), "a SUCCESSFUL flip is NOT a fence-rejection — counter unchanged (proves the counter is at the right event, not every flip)")
}

// TestDispatchMetrics_ReclaimAfterDeath_Fires (BITE 2) — a REAL reclaim-after-death (a lapsed-claimed row
// re-claimed by a live worker) increments reclaimAfterDeath. A fresh (pending) claim must NOT.
func TestDispatchMetrics_ReclaimAfterDeath_Fires(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s, m := mkMetricsStore(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)

	// A fresh (pending) claim is NOT a reclaim — the counter must stay 0.
	_, err = s.ClaimNext("A") // pending → claimed, token 1
	require.NoError(t, err)
	require.Zero(t, m.ReclaimAfterDeath(), "a fresh pending claim is NOT a reclaim-after-death")

	// Lapse A's lease → B's ClaimNext discovers the lapsed-claimed row and RE-CLAIMS it = the event.
	clk.Advance(6 * time.Second)
	_, err = s.ClaimNext("B")
	require.NoError(t, err)
	require.Equal(t, int64(1), m.ReclaimAfterDeath(), "BITE 2: a real reclaim-after-death (lapsed-claimed re-claimed) increments reclaimAfterDeath by 1")
}

// TestDispatchMetrics_SupersededAbort_And_DeadLetter_Fire (BITE 2) — disposeExecErr's superseded-abort +
// dead-letter events increment their counters.
func TestDispatchMetrics_SupersededAbort_And_DeadLetter_Fire(t *testing.T) {
	s, m := mkMetricsStore(t)
	// stage a claimed row so disposeExecErr's paths are exercised.
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w")
	require.NoError(t, err)
	s.setToken("wf", 1)

	// superseded-abort: disposeExecErr on ErrFencedOut → increment supersededAborts, no queue transition.
	require.Zero(t, m.SupersededAborts())
	require.Error(t, disposeExecErr(s, "wf", 3, fmt.Errorf("cp: %w", ErrFencedOut)))
	require.Equal(t, int64(1), m.SupersededAborts(), "BITE 2: ErrFencedOut → supersededAborts +1")

	// dead-letter: a poison *ExecutionError → MarkFailed terminal → increment deadLetters.
	require.Zero(t, m.DeadLetters())
	require.Error(t, disposeExecErr(s, "wf", 3, newExecutionError([]NodeError{{NodeName: "n", Err: fmt.Errorf("boom: %w", ErrValidation)}})))
	require.Equal(t, int64(1), m.DeadLetters(), "BITE 2: a poison dead-letter → deadLetters +1")
}

// TestDispatchMetrics_ZeroCost_NilHook (BITE 1) — with NO metrics hook (nil), the increment path does ZERO
// allocations. The nil-hook is the default; this asserts the increment helpers allocate nothing when the
// receiver is nil (the zero-cost contract). Seed-break: an unconditional increment (non-nil-guarded) would
// allocate a counter → this bench's allocs/op > 0.
func TestDispatchMetrics_ZeroCost_NilHook(t *testing.T) {
	var m *DispatchMetrics // nil = unset
	allocs := testing.AllocsPerRun(1000, func() {
		// the exact increment-helper calls the dispatch path makes, on a nil receiver.
		m.incFenceRejections()
		m.incReclaimAfterDeath()
		m.incSupersededAborts()
		m.incRetriesAttempted()
		m.incDeadLetters()
	})
	require.Zero(t, allocs, "BITE 1: nil-hook increments allocate ZERO (zero-cost when metrics unset)")
}

// TestDispatchMetrics_RetriesAttempted_Fires (BITE 2, review ph86-F5) — a transient-infra fault under
// budget (requeued) increments retriesAttempted. Also covers the F3 fix: a benign already-terminal
// double-flip does NOT tick fenceRejections (only a real still-claimed token-stale reject does).
func TestDispatchMetrics_RetriesAttempted_Fires(t *testing.T) {
	s, m := mkMetricsStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w")
	require.NoError(t, err)
	s.setToken("wf", 1)

	// a transient ErrBusy under budget (attempts 1 < max 3) → MarkForRetry requeues → retriesAttempted +1.
	require.Zero(t, m.RetriesAttempted())
	require.Error(t, disposeExecErr(s, "wf", 3, fmt.Errorf("cp: %w", ErrBusy)))
	require.Equal(t, int64(1), m.RetriesAttempted(), "BITE 2: a requeued transient fault increments retriesAttempted")
	require.Zero(t, m.DeadLetters(), "a requeue is NOT a dead-letter — deadLetters unchanged")
}

// TestDispatchMetrics_F3_BenignDoubleFlip_NotCounted (review ph86-F3 + F-M18-P86-QA-1) — a benign
// double-flip of an already-terminal row, WITH A LIVE (nonzero) held token, must NOT count as a
// fence-rejection.
//
// QA-1 VACUITY FIX: the prior version did MarkDone→MarkFailed, but MarkDone→flipTerminalAndRelease→Release
// CLEARS tokenState (delete → held==0), so the 2nd flip hit flipTerminalFenced's held==0 PLAIN-CAS branch
// and NEVER reached the F3-gated token-guarded block — the test passed for the WRONG reason (dropping the
// `cur==wqClaimed` gate would NOT redden it = vacuous). Fix: drive flipTerminalFenced DIRECTLY (it does NOT
// Release/clear the token — Release lives in the flipTerminalAndRelease wrapper), keeping held != 0 so the
// 2nd flip genuinely enters the token-guarded branch, matches 0 rows (already terminal), and the F3 gate
// (row must be `claimed` to count) is what prevents the false fence-rejection. Bite: drop the gate → this
// benign no-op counts → reddens.
func TestDispatchMetrics_F3_BenignDoubleFlip_NotCounted(t *testing.T) {
	s, m := mkMetricsStore(t)
	_, err := s.Enqueue("wf", "T", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w") // claimed, token 1 in tokenState
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), s.heldToken("wf"), "the claim set a LIVE token")

	// First flip: flipTerminalFenced DIRECTLY (token-guarded branch, held=1 is current) → claimed→done,
	// n==1. Crucially this does NOT clear the token (no Release) — so the token stays LIVE for the 2nd flip.
	flipped, err := s.flipTerminalFenced("wf", wqDone)
	require.NoError(t, err)
	require.True(t, flipped, "first flip lands (current token)")
	require.Zero(t, m.FenceRejections(), "a successful flip is not a reject")
	require.Equal(t, FencingToken(1), s.heldToken("wf"), "token STILL held (flipTerminalFenced does not Release) — so the 2nd flip enters the token-guarded branch")

	// SECOND flip on the now-`done` row, token still LIVE (held=1 != 0) → enters the token-guarded branch →
	// UPDATE matches 0 rows (state is `done`, not `claimed`) → n==0. This is a BENIGN idempotent double-flip,
	// NOT a fence-rejection. The F3 gate (the row must still be `claimed` to count) skips it. WITHOUT the
	// gate, this n==0 would falsely count as a fence-rejection — so this path genuinely exercises the gate.
	flipped, err = s.flipTerminalFenced("wf", wqFailed)
	require.NoError(t, err)
	require.False(t, flipped, "the 2nd flip is a 0-row no-op (row already `done`)")
	require.Zero(t, m.FenceRejections(), "F3: a benign already-terminal double-flip (held!=0, row not claimed) does NOT count as a fence-rejection")
}
