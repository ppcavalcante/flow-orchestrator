package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBypassed_IsTerminal guards that Bypassed is a terminal status: a bypassed
// node never runs and is never re-armed (contrast Waiting, which is NON-terminal
// and re-runnable). isTerminalStatus is consulted at the top of the launch loop
// (parallel_execution.go:88) to skip already-settled nodes.
func TestBypassed_IsTerminal(t *testing.T) {
	assert.True(t, isTerminalStatus(Bypassed), "Bypassed must be terminal")
	// Sanity: the M10 Waiting status stays NON-terminal (a park re-runs).
	assert.False(t, isTerminalStatus(Waiting), "Waiting must stay non-terminal")
}

// TestBypassed_FBNoClobberRoundTrip round-trips a Bypassed node through BOTH
// FlatBuffers conversion switches (statusToFBStatus on Save / fbStatusToNodeStatus
// on Load) alongside every other status. A missed switch arm silently downgrades
// Bypassed->Pending (both switches default to Pending) — the AF1/N2 clobber class.
// BITE (T4 Property-A mutation #3): delete the `case Bypassed` from
// statusToFBStatus and this assertion FAILS (Bypassed reloads as Pending).
func TestBypassed_FBNoClobberRoundTrip(t *testing.T) {
	store, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	const id = "bypassed-fb-roundtrip"

	d := NewWorkflowData(id)
	d.SetNodeStatus("done", Completed)
	d.SetNodeStatus("fail", Failed)
	d.SetNodeStatus("skip", Skipped)
	d.SetNodeStatus("run", Running)
	d.SetNodeStatus("pend", Pending)
	d.SetNodeStatus("park", Waiting)
	d.SetNodeStatus("bypass", Bypassed)
	require.NoError(t, store.Save(d))

	got, err := store.Load(id)
	require.NoError(t, err)
	for node, want := range map[string]NodeStatus{
		"done": Completed, "fail": Failed, "skip": Skipped,
		"run": Running, "pend": Pending, "park": Waiting, "bypass": Bypassed,
	} {
		st, ok := got.GetNodeStatus(node)
		require.True(t, ok, "node %s missing after FB round-trip", node)
		assert.Equal(t, want, st, "node %s status clobbered across FB round-trip", node)
	}
}

// TestBypassed_JSONNoClobberRoundTrip round-trips a Bypassed node through
// JSONFileStore, which serializes NodeStatus as its raw string (via Snapshot,
// NOT the FB switch) — so Bypassed must survive as "bypassed".
func TestBypassed_JSONNoClobberRoundTrip(t *testing.T) {
	store, err := NewJSONFileStore(t.TempDir())
	require.NoError(t, err)
	const id = "bypassed-json-roundtrip"

	d := NewWorkflowData(id)
	d.SetNodeStatus("bypass", Bypassed)
	d.SetNodeStatus("done", Completed)
	require.NoError(t, store.Save(d))

	got, err := store.Load(id)
	require.NoError(t, err)
	st, ok := got.GetNodeStatus("bypass")
	require.True(t, ok, "bypassed node missing after JSON round-trip")
	assert.Equal(t, Bypassed, st, "Bypassed clobbered across JSON round-trip")
}

// TestBypassed_InMemoryRoundTrip covers the third store: the in-memory store
// holds *WorkflowData directly, so Bypassed must be preserved verbatim.
func TestBypassed_InMemoryRoundTrip(t *testing.T) {
	store := NewInMemoryStore()
	const id = "bypassed-inmem"

	d := NewWorkflowData(id)
	d.SetNodeStatus("bypass", Bypassed)
	require.NoError(t, store.Save(d))

	got, err := store.Load(id)
	require.NoError(t, err)
	st, ok := got.GetNodeStatus("bypass")
	require.True(t, ok)
	assert.Equal(t, Bypassed, st)
}
