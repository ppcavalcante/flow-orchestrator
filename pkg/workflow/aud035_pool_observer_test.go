package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-035 / P-14: the Pool's drive loop discarded every runNext error (`ran, _ :=`), hiding
// per-item terminal failures and persistent claim faults — a worker could churn on a broken
// factory or a failing store forever with nothing observable. WithErrorObserver surfaces those
// otherwise-swallowed errors without changing control flow.
func TestAUD035_PoolObserverSurfacesSwallowedErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pool.db")

	// Enqueue an item whose factory will fail, then close the setup store so the pool's
	// factory can reopen the same DB cleanly.
	setup, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	_, err = setup.Enqueue("wf-boom", "boom", nil)
	require.NoError(t, err)
	require.NoError(t, setup.Close())

	reg := NewRegistry()
	require.NoError(t, reg.Register("boom", func() (*DAG, error) {
		return nil, fmt.Errorf("factory exploded")
	}))

	var mu sync.Mutex
	var seen []error
	factory := func() (*SQLiteStore, error) {
		return NewSQLiteStore(dbPath, WithMultiProcess())
	}
	pool, err := NewPool(factory, reg, "obs", WithPoolSize(1),
		WithErrorObserver(func(_ int, e error) {
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
		}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, pool.Run(ctx), "a per-item factory failure is not worker-fatal; Run drains clean")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seen, "AUD-035: the observer must receive the swallowed factory error, not have it silently dropped")
	require.ErrorIs(t, seen[0], ErrValidation, "a broken factory is a terminal ErrValidation")
}

// Without an observer (the default), the same broken factory is still swallowed and the pool
// drains clean — the observer is purely additive and changes no control flow.
func TestAUD035_NoObserverStillDrainsClean(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pool.db")
	setup, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	_, err = setup.Enqueue("wf-boom", "boom", nil)
	require.NoError(t, err)
	require.NoError(t, setup.Close())

	reg := NewRegistry()
	require.NoError(t, reg.Register("boom", func() (*DAG, error) { return nil, fmt.Errorf("boom") }))

	pool, err := NewPool(
		func() (*SQLiteStore, error) { return NewSQLiteStore(dbPath, WithMultiProcess()) },
		reg, "obs", WithPoolSize(1))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	require.NoError(t, pool.Run(ctx))
}
