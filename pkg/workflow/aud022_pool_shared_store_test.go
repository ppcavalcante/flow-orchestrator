package workflow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-022 / P-13: the Pool's per-worker tokenState isolation (the thing that makes
// in-process fencing bite -- see the workflow_pool.go header) REQUIRES each factory
// call to return a DISTINCT *SQLiteStore. A factory that hands the SAME instance to
// every worker collapses that isolation (a reclaim clobbers the shared
// tokenState[workflowID] key) and Closes the one store N times. That contract was
// enforced only by prose. Reject a shared instance at startup, before any work begins.
func TestAUD022_PoolRejectsSharedStoreInstance(t *testing.T) {
	dir := t.TempDir()
	shared, err := NewSQLiteStore(filepath.Join(dir, "wf.db"), WithMultiProcess())
	require.NoError(t, err)
	// The pool owns closing what its factory returns; a second Close here is a no-op
	// on *sql.DB, but we let the pool's rollback close be the one that runs.

	factory := func() (*SQLiteStore, error) {
		return shared, nil // VIOLATION: the SAME pointer to every worker.
	}
	reg := NewRegistry()
	pool, err := NewPool(factory, reg, "owner", WithPoolSize(2))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = pool.Run(ctx)
	require.Error(t, err, "Run must reject a factory that returns a shared *SQLiteStore, not silently collapse fencing isolation")
	require.ErrorIs(t, err, ErrValidation)
}

// A well-behaved factory (distinct MP store per call) must still start cleanly and
// drain on ctx cancellation -- the distinctness guard must not reject the good case.
func TestAUD022_PoolAcceptsDistinctStores(t *testing.T) {
	dir := t.TempDir()
	factory := func() (*SQLiteStore, error) {
		return NewSQLiteStore(filepath.Join(dir, "wf.db"), WithMultiProcess())
	}
	reg := NewRegistry()
	pool, err := NewPool(factory, reg, "owner", WithPoolSize(2))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Empty registry -> no work; a clean drain on ctx timeout returns nil.
	require.NoError(t, pool.Run(ctx))
}
