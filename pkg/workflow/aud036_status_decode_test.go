package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fb "github.com/ppcavalcante/flow-orchestrator/internal/workflow/fb/workflow"
	"github.com/stretchr/testify/require"
)

// AUD-036 / P-04: unknown NodeStatus decode diverged by backend -- SQLite failed closed
// (ErrCorruptData, the correct reference), but JSON accepted ANY string into NodeStatus and
// FlatBuffers mapped an unknown enum to Pending. A corrupt/forged terminal status could thus
// make a Completed node rerun. Every durable store must apply the shared strict isKnownStatus
// policy and reject the unknown value as corrupt.

// FB: an out-of-range enum value must be reported as unknown, not silently coerced to Pending.
func TestAUD036_FBUnknownStatusIsRejected(t *testing.T) {
	// Every legal enum decodes with ok=true.
	for _, s := range []fb.NodeStatus{
		fb.NodeStatusPending, fb.NodeStatusRunning, fb.NodeStatusCompleted, fb.NodeStatusFailed,
		fb.NodeStatusSkipped, fb.NodeStatusWaiting, fb.NodeStatusBypassed, fb.NodeStatusCompensated,
		fb.NodeStatusCompensationFailed,
	} {
		_, ok := fbStatusToNodeStatus(s)
		require.True(t, ok, "legal enum %v must decode", s)
	}
	// An unknown enum is NOT silently mapped to Pending.
	_, ok := fbStatusToNodeStatus(fb.NodeStatus(99))
	require.False(t, ok, "AUD-036: an unknown FB enum must be reported unknown, not coerced to Pending")
}

// JSON: a forged snapshot carrying a bogus status string must load as ErrCorruptData, not be
// accepted verbatim into NodeStatus.
func TestAUD036_JSONUnknownStatusIsRejected(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)

	// Persist a legitimate Completed status.
	d := NewWorkflowData("wf-036")
	d.SetNodeStatus("n1", Completed)
	require.NoError(t, store.Save(d))

	// Forge the on-disk status to a bogus value.
	path := filepath.Join(dir, "wf-036.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	forged := strings.Replace(string(raw), `"completed"`, `"totally_bogus_status"`, 1)
	require.NotEqual(t, string(raw), forged, "the forge must actually change the file")
	require.NoError(t, os.WriteFile(path, []byte(forged), 0o600))

	// Load must reject it as corrupt, not accept the bogus string into NodeStatus.
	_, err = store.Load("wf-036")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruptData, "AUD-036: an unknown JSON status must be ErrCorruptData")
}
