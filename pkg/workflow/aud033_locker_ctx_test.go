package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-033 / C-19 dispatch: the in-process Locker blocked on a sync.Mutex Lock and could
// not honor ctx cancellation while waiting for a held lease -- a caller whose context was
// cancelled (shutdown, deadline) stayed parked until the holder released. The per-WorkflowID
// lock is now a channel, so Acquire aborts on ctx.Done() and returns ctx.Err().
func TestAUD033_InProcessLockerHonorsContext(t *testing.T) {
	l := NewInProcessLocker()

	// Hold the lease for "wf" so the second Acquire must wait.
	release, err := l.Acquire(context.Background(), "wf")
	require.NoError(t, err)

	// A second Acquire under a short-deadline ctx must give up, not block forever.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		r, aerr := l.Acquire(ctx, "wf")
		if aerr == nil {
			r() // must not happen; release if it somehow acquired
		}
		done <- aerr
	}()

	select {
	case aerr := <-done:
		require.Error(t, aerr, "AUD-033: a blocked Acquire must abort when its ctx is cancelled")
		require.ErrorIs(t, aerr, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Fatal("AUD-033: Acquire ignored ctx and blocked past the deadline")
	}

	// The holder can still release cleanly, and a fresh Acquire then succeeds.
	release()
	r2, err := l.Acquire(context.Background(), "wf")
	require.NoError(t, err)
	r2()
}

// A distinct WorkflowID never contends, ctx or not.
func TestAUD033_DistinctIDsDoNotContend(t *testing.T) {
	l := NewInProcessLocker()
	ra, err := l.Acquire(context.Background(), "a")
	require.NoError(t, err)
	defer ra()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rb, err := l.Acquire(ctx, "b") // different ID -> immediate
	require.NoError(t, err)
	rb()
}
