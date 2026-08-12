package workflow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-023 / P-09: the MP Locker (claimLocker) does NOT serialize concurrent
// same-(WorkflowID, ownerID) drives WITHIN one process -- a re-entrant Claim returns the
// held token without blocking, so a host that fans one owner across goroutines driving the
// same WorkflowID gets two concurrent drives racing the same state. WithMultiProcessLocker
// now composes the durable claim OVER the process-local in-process Locker (the F4 pattern
// the design already documented), so same-WorkflowID drives serialize locally while the
// durable claim still arbitrates cross-process.
func TestAUD023_CompositeLockerSerializesSameProcess(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "c.db"), WithMultiProcess())
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // test cleanup

	w := &Workflow{Store: store, WorkflowID: "aud023-wf"}
	w.WithMultiProcessLocker("owner")
	lk := w.Locker
	const id = "aud023-wf"

	// First acquire holds the lease.
	rel1, err := lk.Acquire(context.Background(), id)
	require.NoError(t, err)

	// A second acquire of the SAME id must BLOCK until the first releases. Pre-fix the
	// claim is re-entrant for the same owner and returns immediately (no serialization).
	acquired := make(chan func(), 1)
	go func() {
		rel2, aerr := lk.Acquire(context.Background(), id)
		if aerr == nil {
			acquired <- rel2
		}
	}()

	select {
	case <-acquired:
		t.Fatal("AUD-023: the second same-id Acquire did NOT block — same-process same-id drives are not serialized")
	case <-time.After(250 * time.Millisecond):
		// Correct: it is blocked on the process-local lock.
	}

	rel1() // release → the blocked acquire proceeds
	select {
	case rel2 := <-acquired:
		rel2()
	case <-time.After(3 * time.Second):
		t.Fatal("AUD-023: the second Acquire did not proceed after the first released")
	}
}

// A DISTINCT WorkflowID under the same composite locker never contends.
func TestAUD023_CompositeLockerDistinctIDsDoNotContend(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "c.db"), WithMultiProcess())
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // test cleanup

	w := &Workflow{Store: store, WorkflowID: "x"}
	w.WithMultiProcessLocker("owner")
	lk := w.Locker

	ra, err := lk.Acquire(context.Background(), "id-a")
	require.NoError(t, err)
	defer ra()

	done := make(chan struct{})
	go func() {
		rb, err := lk.Acquire(context.Background(), "id-b") // different id -> immediate
		if err == nil {
			rb()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a distinct WorkflowID must not contend under the composite locker")
	}
}
