package workflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLease_RegistryReclaimsEntries (ph37 review F4): the in-process lock registry
// is bounded — an entry is reclaimed on the last release (refcount), so driving many
// distinct WorkflowIDs does not grow the map without bound. Acquire+release 200
// distinct ids and assert the registry is empty afterward.
func TestLease_RegistryReclaimsEntries(t *testing.T) {
	l, ok := NewInProcessLocker().(*inProcessLocker)
	require.True(t, ok)
	for i := 0; i < 200; i++ {
		release, err := l.Acquire(context.Background(), fmt.Sprintf("wf-%d", i))
		require.NoError(t, err)
		release()
	}
	l.mu.Lock()
	n := len(l.locks)
	l.mu.Unlock()
	assert.Equal(t, 0, n, "the lock registry reclaims entries on last release (bounded; ph37 F4)")
}

// noOpLocker is a Locker that never serializes — the first-class "drop the lease"
// mutation for the serialization bite. Injecting it must reproduce the
// concurrent-drive overlap the real lease prevents.
type noOpLocker struct{}

func (noOpLocker) Acquire(_ context.Context, _ string) (func(), error) {
	return func() {}, nil
}

// overlapProbe is an Action that detects whether two drives ever execute it
// concurrently: it tracks the live in-flight count and records the maximum seen.
// A widen-the-window sleep makes a real overlap observable.
type overlapProbe struct {
	inFlight    int32
	maxInFlight int32
}

func (p *overlapProbe) Execute(_ context.Context, _ *WorkflowData) error {
	n := atomic.AddInt32(&p.inFlight, 1)
	for {
		m := atomic.LoadInt32(&p.maxInFlight)
		if n <= m || atomic.CompareAndSwapInt32(&p.maxInFlight, m, n) {
			break
		}
	}
	time.Sleep(3 * time.Millisecond) // widen the overlap window
	atomic.AddInt32(&p.inFlight, -1)
	return nil
}

// driveTwiceConcurrently runs two concurrent Execute on one *Workflow and returns
// the maximum number of probe executions seen in flight at once.
func driveTwiceConcurrently(t *testing.T, locker Locker) int32 {
	t.Helper()
	probe := &overlapProbe{}
	w := NewWorkflow(NewInMemoryStore())
	w.WorkflowID = "wf-lease"
	if locker != nil {
		w.WithLocker(locker)
	}
	require.NoError(t, w.AddNode(NewNode("n", probe)))

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = w.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		}()
	}
	wg.Wait()
	return atomic.LoadInt32(&probe.maxInFlight)
}

// TestLease_SerializesConcurrentDrives (acceptance #4): two concurrent drives of
// one WorkflowID under the default in-process lease never overlap — the lease
// makes them take turns, so the first drive completes + saves the node and the
// second loads it Completed and skips it. The probe is therefore in flight at most
// once.
func TestLease_SerializesConcurrentDrives(t *testing.T) {
	maxInFlight := driveTwiceConcurrently(t, nil) // nil → default in-process lease
	assert.Equal(t, int32(1), maxInFlight,
		"the in-process lease must serialize concurrent same-ID drives (probe never overlaps)")
}

// TestLease_NoOpLockerReproducesOverlap is the load-bearing BITE: dropping the
// lease (injecting a no-op Locker) reproduces the concurrent-drive overlap — both
// drives load the node Pending at once and both run the probe, so it is in flight
// twice. This proves the serialization test has teeth (the assertion above would
// be vacuous if a no-op locker also yielded max==1).
func TestLease_NoOpLockerReproducesOverlap(t *testing.T) {
	maxInFlight := driveTwiceConcurrently(t, noOpLocker{})
	assert.Equal(t, int32(2), maxInFlight,
		"without the lease, concurrent same-ID drives overlap (the bite: the lease is load-bearing)")
}
