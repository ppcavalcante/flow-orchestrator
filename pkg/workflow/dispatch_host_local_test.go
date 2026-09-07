package workflow

// M25 §14.B2 — targeted host-local submit/admit/drive witnesses. A designated-host run is admitted atomically
// (EnqueueForHost), driven ONLY by its host (RunNextForHost / ClaimNextForHost), never stolen by a generic
// worker, and shares the engine's caps + fencing + lifecycle. All over real SQLite dispatch.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkHostReg registers a single legacy type "job" whose node records the host that ran it (via a per-run marker
// set by the constructor closure is not possible; instead the node stamps a fixed key and the test reads state).
func mkHostReg(t *testing.T, ran *int32Recorder) *Registry {
	t.Helper()
	reg := NewRegistry()
	require.NoError(t, reg.Register("job", func() (*DAG, error) {
		return oneNode(t, "n", func(d *WorkflowData) error {
			if ran != nil {
				ran.inc()
			}
			d.Set("done", true)
			return nil
		}), nil
	}))
	return reg
}

type int32Recorder struct{ n int32 }

func (r *int32Recorder) inc() { r.n++ }

// B2 — the theft-window is closed: a generic worker can NEVER claim a host-bound row, and the designated host
// drives ONLY its own bound run (no substitution of other queued work).
func TestHostLocal_GenericWorkerCannotStealHostBound(t *testing.T) {
	s := mkQueueStore(t)
	reg := mkHostReg(t, nil)

	// A host-bound run + a generic run in the SAME queue.
	_, err := s.EnqueueForHost("host-run", "job", nil, "host-A")
	require.NoError(t, err)
	_, err = s.Enqueue("generic-run", "job", nil)
	require.NoError(t, err)

	// A GENERIC worker drives: it may take ONLY the generic run — never the host-bound one, even though the
	// host-bound row is older (enqueued first).
	ran, rerr := RunNext(context.Background(), s, reg, "generic-worker")
	require.NoError(t, rerr)
	require.True(t, ran)
	require.Equal(t, wqDone, wqState(t, s, "generic-run"), "the generic worker took the generic run")
	require.Equal(t, wqPending, wqState(t, s, "host-run"), "the host-bound run is NOT stealable by a generic worker")

	// A second generic drive finds no more generic work (the host-bound row is invisible to it) → ran=false.
	ran, rerr = RunNext(context.Background(), s, reg, "generic-worker")
	require.NoError(t, rerr)
	require.False(t, ran, "no generic work remains; the host-bound row is not claimable generically")
	require.Equal(t, wqPending, wqState(t, s, "host-run"))

	// The DESIGNATED host drives its own run — and ONLY its own (it does not substitute other work).
	ran, rerr = RunNextForHost(context.Background(), s, reg, "host-A-worker", "host-A")
	require.NoError(t, rerr)
	require.True(t, ran)
	require.Equal(t, wqDone, wqState(t, s, "host-run"), "the designated host drove its bound run")

	// A DIFFERENT host cannot claim host-A's work.
	_, err = s.EnqueueForHost("host-run-2", "job", nil, "host-A")
	require.NoError(t, err)
	ran, rerr = RunNextForHost(context.Background(), s, reg, "host-B-worker", "host-B")
	require.NoError(t, rerr)
	require.False(t, ran, "host-B cannot claim host-A's bound work")
	require.Equal(t, wqPending, wqState(t, s, "host-run-2"))
}

// B2 — race: a designated host-local admission and a generic worker contend over the SAME database; only the
// designated host may execute the requested run, and the generic worker is left to take only generic work.
func TestHostLocal_RaceDesignatedVsGeneric(t *testing.T) {
	s1, s2 := mkSharedStores(t)
	reg := mkHostReg(t, nil)

	_, err := s1.EnqueueForHost("targeted", "job", nil, "the-host")
	require.NoError(t, err)

	// Both a generic worker (s2) and the designated host (s1) try, concurrently.
	type res struct {
		ran bool
		err error
	}
	genCh := make(chan res, 1)
	hostCh := make(chan res, 1)
	start := make(chan struct{})
	go func() { <-start; r, e := RunNext(context.Background(), s2, reg, "generic"); genCh <- res{r, e} }()
	go func() {
		<-start
		r, e := RunNextForHost(context.Background(), s1, reg, "host", "the-host")
		hostCh <- res{r, e}
	}()
	close(start)
	gen := <-genCh
	host := <-hostCh
	require.NoError(t, gen.err)
	require.NoError(t, host.err)

	require.False(t, gen.ran, "the generic worker did NOT execute the host-bound run")
	require.True(t, host.ran, "only the designated host executed the requested run")
	require.Equal(t, wqDone, wqState(t, s1, "targeted"))
}

// B2 — the host-local run honors the engine's SHARED cap: at capacity the host-bound claim is backpressured
// (stays pending), never bypasses the cap.
func TestHostLocal_SharedCapBackpressure(t *testing.T) {
	caps := WithCaps(Caps{PerType: map[string]int{"job": 1}})
	s := mkDispatchStore(t, caps)
	reg := mkHostReg(t, nil)

	// Occupy the single "job" slot with a generic claim (claimed, not parked = a running slot).
	_, err := s.Enqueue("occupant", "job", nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("occupant-worker", "job")
	require.NoError(t, err)

	// A host-bound run now cannot be admitted (shared cap reached) — backpressure, not a cap bypass.
	_, err = s.EnqueueForHost("host-run", "job", nil, "host-A")
	require.NoError(t, err)
	ran, rerr := RunNextForHost(context.Background(), s, reg, "host-A-worker", "host-A")
	require.NoError(t, rerr)
	require.False(t, ran, "the host-bound run is backpressured by the shared cap (no bypass)")
	require.Equal(t, wqPending, wqState(t, s, "host-run"))

	// Free the slot → the host run is admitted.
	ok, err := s.MarkDone("occupant")
	require.NoError(t, err)
	require.True(t, ok)
	ran, rerr = RunNextForHost(context.Background(), s, reg, "host-A-worker", "host-A")
	require.NoError(t, rerr)
	require.True(t, ran, "once the shared slot frees, the host run is admitted")
	require.Equal(t, wqDone, wqState(t, s, "host-run"))
}

// B2 — recovery: a host-bound run whose host dies (lease lapses) is reclaimable ONLY by the SAME host, never
// silently executed elsewhere; its durable owner_host identity persists. The successor (same host, new worker)
// resumes and the stale owner's late write is fenced.
func TestHostLocal_HostScopedRecoveryAndFencing(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s1, s2 := mkSharedStores(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	reg := mkHostReg(t, nil)

	_, err := s1.EnqueueForHost("hb", "job", nil, "host-A")
	require.NoError(t, err)
	// Host-A worker 1 claims (token 1) then stalls (host "dies").
	_, err = s1.ClaimNextForHost("A-w1", "host-A", "job")
	require.NoError(t, err)
	require.EqualValues(t, 1, leaseToken(t, s1, "hb"))

	// Lease lapses. A GENERIC worker cannot reclaim it (host-bound), and neither can another host.
	clk.Advance(6 * time.Second)
	ranGen, gerr := RunNext(context.Background(), s2, reg, "generic")
	require.NoError(t, gerr)
	require.False(t, ranGen, "a lapsed host-bound run is NOT reclaimable by a generic worker")
	_, berr := s2.ClaimNextForHost("B-w", "host-B", "job")
	require.ErrorIs(t, berr, ErrNoWork, "another host cannot reclaim host-A's lapsed work")
	require.Equal(t, wqClaimed, wqState(t, s1, "hb"), "the row keeps its host-bound identity (still claimed by A)")

	// The SAME host (a restarted worker on the other handle) reclaims it (token 2 — A fenced) and holds it.
	item, err := s2.ClaimNextForHost("A-w2", "host-A", "job")
	require.NoError(t, err)
	require.EqualValues(t, 2, leaseToken(t, s2, "hb"), "the same-host reclaim bumped the token (stale worker fenced)")
	require.EqualValues(t, 2, item.Token)

	// The stale host-A worker 1's late terminalization is fenced (token 1 < durable 2) — cannot flip the row.
	flipped, ferr := s1.MarkFailed("hb")
	require.NoError(t, ferr)
	require.False(t, flipped, "the stale owner's late write is fenced")
	require.Equal(t, wqClaimed, wqState(t, s2, "hb"), "the successor's row is intact")

	// The successor completes the run.
	done, err := s2.MarkDone("hb")
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, wqDone, wqState(t, s2, "hb"))
}

// B2 — cancellation of a host-bound run works through the ordinary operator surface: a pending host-bound run
// is cancelled and never executed by its host.
func TestHostLocal_Cancellation(t *testing.T) {
	s := mkQueueStore(t)
	rec := &int32Recorder{}
	reg := mkHostReg(t, rec)

	_, err := s.EnqueueForHost("hb", "job", nil, "host-A")
	require.NoError(t, err)
	// Operator cancels the pending host-bound run.
	ok, err := s.CancelPending("hb")
	require.NoError(t, err)
	require.True(t, ok)

	ran, rerr := RunNextForHost(context.Background(), s, reg, "host-A-worker", "host-A")
	require.NoError(t, rerr)
	require.False(t, ran, "a cancelled host-bound run is not executed")
	require.Equal(t, wqCancelled, wqState(t, s, "hb"))
	require.EqualValues(t, 0, rec.n, "no action ran")
}

// B2 — guard rails: EnqueueForHost/ClaimNextForHost/RunNextForHost require a non-empty host.
func TestHostLocal_EmptyHostRejected(t *testing.T) {
	s := mkQueueStore(t)
	reg := mkHostReg(t, nil)
	_, err := s.EnqueueForHost("x", "job", nil, "")
	require.ErrorIs(t, err, ErrValidation)
	_, err = s.ClaimNextForHost("w", "", "job")
	require.ErrorIs(t, err, ErrValidation)
	_, err = RunNextForHost(context.Background(), s, reg, "w", "")
	require.ErrorIs(t, err, ErrValidation)
	_, err = s.ClaimSpecificForHost("w", "", "wf")
	require.ErrorIs(t, err, ErrValidation)
	_, err = s.ClaimSpecificForHost("w", "host-A", "")
	require.ErrorIs(t, err, ErrValidation)
	_, err = RunSpecificForHost(context.Background(), s, reg, "w", "", "wf")
	require.ErrorIs(t, err, ErrValidation)
	_, err = RunSpecificForHost(context.Background(), s, reg, "w", "host-A", "")
	require.ErrorIs(t, err, ErrValidation)
}

// R1 / B2-SEC-1 — the consumer's reproducer: with two legitimate same-host/same-type pending rows, driving the
// SPECIFICALLY requested run executes ONLY it, leaving the older row untouched (no host-scoped-FIFO substitution).
func TestB2Security_SpecificRequestedRun(t *testing.T) {
	s := mkDispatchStore(t)
	var executed []string
	reg := NewRegistry()
	require.NoError(t, reg.RegisterWithInput("job", func(input []byte) (*DAG, error) {
		return oneNode(t, "n", func(d *WorkflowData) error { executed = append(executed, d.GetWorkflowID()); return nil }), nil
	}))
	_, err := s.EnqueueForHost("older", "job", nil, "host-A")
	require.NoError(t, err)
	_, err = s.EnqueueForHost("requested", "job", nil, "host-A")
	require.NoError(t, err)

	ran, err := RunSpecificForHost(context.Background(), s, reg, "cli-requested", "host-A", "requested")
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, []string{"requested"}, executed, "only the specifically requested run executed")
	require.Equal(t, wqDone, wqState(t, s, "requested"))
	require.Equal(t, wqPending, wqState(t, s, "older"), "the older same-host row is untouched (no FIFO substitution)")
}

// R1 / B2-SEC-1 — specific-run selection FAILS CLOSED (ran=false, no fallback to other work) when the requested
// run is absent, terminal, bound to a different host, or over a shared cap; and it validates the durable host
// binding (a run bound to host-A is not drivable as host-B).
func TestB2Security_SpecificRun_FailsClosed(t *testing.T) {
	t.Run("absent_requested_id", func(t *testing.T) {
		s := mkDispatchStore(t)
		var executed []string
		reg := recordingReg(t, &executed)
		_, err := s.EnqueueForHost("other", "job", nil, "host-A")
		require.NoError(t, err)
		ran, err := RunSpecificForHost(context.Background(), s, reg, "cli", "host-A", "ghost")
		require.NoError(t, err)
		require.False(t, ran, "an absent requested id drives nothing")
		require.Empty(t, executed, "no fallback to other host work")
		require.Equal(t, wqPending, wqState(t, s, "other"))
	})

	t.Run("wrong_host_binding", func(t *testing.T) {
		s := mkDispatchStore(t)
		var executed []string
		reg := recordingReg(t, &executed)
		_, err := s.EnqueueForHost("bound-A", "job", nil, "host-A")
		require.NoError(t, err)
		// Inspect proves the binding; driving it as host-B fails closed.
		sub, err := s.InspectSubmission("bound-A")
		require.NoError(t, err)
		require.Equal(t, "host-A", sub.OwnerHost, "the durable host binding is inspectable")
		ran, err := RunSpecificForHost(context.Background(), s, reg, "cli", "host-B", "bound-A")
		require.NoError(t, err)
		require.False(t, ran, "a run bound to host-A is not drivable as host-B")
		require.Empty(t, executed)
		require.Equal(t, wqPending, wqState(t, s, "bound-A"), "the row stays bound + pending")
	})

	t.Run("terminal_requested_id", func(t *testing.T) {
		s := mkDispatchStore(t)
		var executed []string
		reg := recordingReg(t, &executed)
		_, err := s.EnqueueForHost("term", "job", nil, "host-A")
		require.NoError(t, err)
		_, err = s.ClaimSpecificForHost("w", "host-A", "term")
		require.NoError(t, err)
		_, err = s.MarkDone("term")
		require.NoError(t, err)
		ran, err := RunSpecificForHost(context.Background(), s, reg, "cli", "host-A", "term")
		require.NoError(t, err)
		require.False(t, ran, "a terminal requested run is not re-executed")
		require.Empty(t, executed)
	})

	t.Run("over_shared_cap", func(t *testing.T) {
		s := mkDispatchStore(t, WithCaps(Caps{PerType: map[string]int{"job": 1}}))
		var executed []string
		reg := recordingReg(t, &executed)
		// Occupy the single job slot generically.
		_, err := s.Enqueue("occupant", "job", nil)
		require.NoError(t, err)
		_, err = s.ClaimNext("occupant-w", "job")
		require.NoError(t, err)
		// The requested host-bound run is over the shared cap → fails closed (no bypass, no fallback).
		_, err = s.EnqueueForHost("req", "job", nil, "host-A")
		require.NoError(t, err)
		ran, err := RunSpecificForHost(context.Background(), s, reg, "cli", "host-A", "req")
		require.NoError(t, err)
		require.False(t, ran, "a requested run over the shared cap is backpressured, not bypassed")
		require.Empty(t, executed)
		require.Equal(t, wqPending, wqState(t, s, "req"))
	})
}

// recordingReg registers an input-aware "job" type whose node appends its workflow id to `executed`.
func recordingReg(t *testing.T, executed *[]string) *Registry {
	t.Helper()
	reg := NewRegistry()
	require.NoError(t, reg.RegisterWithInput("job", func([]byte) (*DAG, error) {
		return oneNode(t, "n", func(d *WorkflowData) error { *executed = append(*executed, d.GetWorkflowID()); return nil }), nil
	}))
	return reg
}

// R1 — a lapsed host-bound requested run is reclaimable by ID by the same host (specific-run reclaim).
func TestB2Security_SpecificRun_ReclaimByID(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	s1, s2 := mkSharedStores(t, withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
	var executed []string
	reg := recordingReg(t, &executed)
	_, err := s1.EnqueueForHost("req", "job", nil, "host-A")
	require.NoError(t, err)
	// Host-A worker 1 claims the specific run then stalls.
	_, err = s1.ClaimSpecificForHost("A-w1", "host-A", "req")
	require.NoError(t, err)
	// Lease lapses → the SAME host reclaims the specific run by ID and drives it.
	clk.Advance(6 * time.Second)
	ran, err := RunSpecificForHost(context.Background(), s2, reg, "A-w2", "host-A", "req")
	require.NoError(t, err)
	require.True(t, ran, "the same host reclaims the specific lapsed run by id")
	require.Equal(t, wqDone, wqState(t, s2, "req"))
	require.Equal(t, []string{"req"}, executed)
}
