package workflow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-021 / P-12: the Pool's StoreFactory contract requires every worker store to
// use multi-process mode, but runWorker never validated it. A non-MP store's
// ClaimNext returns a PERMANENT ErrValidation; runWorker discards that error and
// backs off, so the worker spins forever -- alive, healthy-looking, processing
// zero work. Validate at startup and fail loudly.
func TestAUD021_PoolRejectsNonMPStoreFactory(t *testing.T) {
	dir := t.TempDir()
	factory := func() (*SQLiteStore, error) {
		// NOTE: no WithMultiProcess() -> a non-MP store, violating the factory contract.
		return NewSQLiteStore(filepath.Join(dir, "wf.db"))
	}
	reg := NewRegistry()
	pool, err := NewPool(factory, reg, "owner", WithPoolSize(1))
	require.NoError(t, err)

	// A short deadline: pre-fix, Run would spin until this fires and then return nil
	// (drain), failing the require.Error. Post-fix, Run returns the startup error
	// immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = pool.Run(ctx)
	require.Error(t, err, "Run must fail fast on a non-MP store factory, not spin forever processing nothing")
	require.ErrorIs(t, err, ErrValidation)
}
