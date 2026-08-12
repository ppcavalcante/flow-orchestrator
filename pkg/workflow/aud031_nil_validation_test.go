package workflow

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// AUD-031 / C-19: public entry points must return a typed ErrValidation on nil inputs,
// not panic through a nil dereference. A panic in a library constructor takes the HOST
// process down; the named offenders were FromBuilder(nil) and RunNext with a nil store
// or registry. This mirrors the P0 policy: make invalid input a typed error, never a crash.
func TestAUD031_FromBuilderNilReturnsValidationError(t *testing.T) {
	w, err := FromBuilder(nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrValidation)
	require.Nil(t, w)
}

func TestAUD031_RunNextNilInputsReturnValidationError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "wf.db"), WithMultiProcess())
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // test cleanup
	reg := NewRegistry()

	t.Run("nil store", func(t *testing.T) {
		ran, err := RunNext(context.Background(), nil, reg, "owner")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.False(t, ran)
	})

	t.Run("nil registry", func(t *testing.T) {
		ran, err := RunNext(context.Background(), store, nil, "owner")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.False(t, ran)
	})
}
