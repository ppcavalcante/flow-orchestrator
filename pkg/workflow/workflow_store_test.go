package workflow

import (
	"os"
	"path/filepath"
	"testing"

	fb "github.com/ppcavalcante/flow-orchestrator/internal/workflow/fb/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestWorkflow(t *testing.T, store WorkflowStore) *Workflow {
	workflow := NewWorkflow(store)

	startAction := NewCompositeAction()
	endAction := NewCompositeAction()

	startNode := NewNode("start", startAction)
	endNode := NewNode("end", endAction)

	err := workflow.AddNode(startNode)
	require.NoError(t, err)

	err = workflow.AddNode(endNode)
	require.NoError(t, err)

	err = workflow.AddDependency("end", "start")
	require.NoError(t, err)

	return workflow
}

func TestInMemoryStore(t *testing.T) {
	// Create a new in-memory store
	store := NewInMemoryStore()

	// Create test workflow data
	data := NewWorkflowData("test-workflow")
	data.Set("key1", "value1")
	data.SetNodeStatus("node1", Pending)
	data.SetOutput("node1", "output1")

	// Test Save
	err := store.Save(data)
	require.NoError(t, err)

	// Test Load
	loaded, err := store.Load("test-workflow")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify loaded data
	val, exists := loaded.Get("key1")
	require.True(t, exists)
	assert.Equal(t, "value1", val)

	status, exists := loaded.GetNodeStatus("node1")
	require.True(t, exists)
	assert.Equal(t, Pending, status)

	output, exists := loaded.GetOutput("node1")
	require.True(t, exists)
	assert.Equal(t, "output1", output)

	// Test ListWorkflows
	workflows, err := store.ListWorkflows()
	require.NoError(t, err)
	assert.Equal(t, []string{"test-workflow"}, workflows)

	// Test Delete
	err = store.Delete("test-workflow")
	require.NoError(t, err)

	// Verify deletion
	_, err = store.Load("test-workflow")
	assert.Error(t, err)
}

func TestJSONFileStore(t *testing.T) {
	// Create a temporary directory for testing (t.TempDir auto-removes at test
	// end — no manual os.RemoveAll, which errcheck check-blank would flag).
	tempDir := t.TempDir()

	// Create a new JSON file store
	store, err := NewJSONFileStore(tempDir)
	require.NoError(t, err)

	// Create test workflow data
	data := NewWorkflowData("test-workflow")
	data.Set("key1", "value1")
	data.SetNodeStatus("node1", Pending)
	data.SetOutput("node1", "output1")

	// Test Save
	err = store.Save(data)
	require.NoError(t, err)

	// Test Load
	loaded, err := store.Load("test-workflow")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify loaded data
	val, exists := loaded.Get("key1")
	require.True(t, exists)
	assert.Equal(t, "value1", val)

	status, exists := loaded.GetNodeStatus("node1")
	require.True(t, exists)
	assert.Equal(t, Pending, status)

	output, exists := loaded.GetOutput("node1")
	require.True(t, exists)
	assert.Equal(t, "output1", output)

	// Test ListWorkflows
	workflows, err := store.ListWorkflows()
	require.NoError(t, err)
	assert.Equal(t, []string{"test-workflow"}, workflows)

	// Test Delete
	err = store.Delete("test-workflow")
	require.NoError(t, err)

	// Verify deletion
	_, err = store.Load("test-workflow")
	assert.Error(t, err)
}

func TestFlatBuffersStore(t *testing.T) {
	// Create a temporary directory for testing (t.TempDir auto-removes at test
	// end — no manual os.RemoveAll, which errcheck check-blank would flag).
	tempDir := t.TempDir()

	// Create a new FlatBuffers store
	store, err := NewFlatBuffersStore(tempDir)
	require.NoError(t, err)

	// Create test workflow data
	data := NewWorkflowData("test-workflow")
	data.Set("key1", "value1")
	data.SetNodeStatus("node1", Pending)
	data.SetOutput("node1", "output1")

	// Test Save
	err = store.Save(data)
	require.NoError(t, err)

	// Test Load
	loaded, err := store.Load("test-workflow")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify loaded data
	val, exists := loaded.Get("key1")
	require.True(t, exists)
	assert.Equal(t, "value1", val)

	status, exists := loaded.GetNodeStatus("node1")
	require.True(t, exists)
	assert.Equal(t, Pending, status)

	output, exists := loaded.GetOutput("node1")
	require.True(t, exists)
	assert.Equal(t, "output1", output)

	// Test ListWorkflows
	workflows, err := store.ListWorkflows()
	require.NoError(t, err)
	assert.Equal(t, []string{"test-workflow"}, workflows)

	// Test Delete
	err = store.Delete("test-workflow")
	require.NoError(t, err)

	// Verify deletion
	_, err = store.Load("test-workflow")
	assert.Error(t, err)
}

func TestMigration(t *testing.T) {
	// Create a temporary directory for testing (t.TempDir auto-removes at test
	// end — no manual os.RemoveAll, which errcheck check-blank would flag).
	tempDir := t.TempDir()

	// Create a new JSON file store
	jsonStore, err := NewJSONFileStore(tempDir)
	require.NoError(t, err)

	// Create test workflow data
	data := NewWorkflowData("test-workflow")
	data.Set("key1", "value1")
	data.SetNodeStatus("node1", Pending)
	data.SetOutput("node1", "output1")

	// Save to JSON store
	err = jsonStore.Save(data)
	require.NoError(t, err)

	// Migrate to FlatBuffers store
	fbStore, err := jsonStore.MigrateToFlatBuffers(false)
	require.NoError(t, err)

	// Load from FlatBuffers store
	loaded, err := fbStore.Load("test-workflow")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify loaded data
	val, exists := loaded.Get("key1")
	require.True(t, exists)
	assert.Equal(t, "value1", val)

	status, exists := loaded.GetNodeStatus("node1")
	require.True(t, exists)
	assert.Equal(t, Pending, status)

	output, exists := loaded.GetOutput("node1")
	require.True(t, exists)
	assert.Equal(t, "output1", output)

	// Test cleanup: only the side effect (JSON deletion) + err matter here, so
	// the returned store is intentionally discarded (SA4006 — the prior fbStore
	// value was never read again).
	_, err = jsonStore.MigrateToFlatBuffers(true)
	require.NoError(t, err)

	// Verify JSON file was deleted
	_, err = os.Stat(filepath.Join(tempDir, "test-workflow.json"))
	assert.True(t, os.IsNotExist(err))
}

func TestStatusToFBStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   NodeStatus
		expected fb.NodeStatus
	}{
		{
			name:     "Pending status",
			status:   Pending,
			expected: fb.NodeStatusPending,
		},
		{
			name:     "Running status",
			status:   Running,
			expected: fb.NodeStatusRunning,
		},
		{
			name:     "Completed status",
			status:   Completed,
			expected: fb.NodeStatusCompleted,
		},
		{
			name:     "Failed status",
			status:   Failed,
			expected: fb.NodeStatusFailed,
		},
		{
			name:     "Skipped status",
			status:   Skipped,
			expected: fb.NodeStatusSkipped,
		},
		{
			name:     "NotStarted status",
			status:   NotStarted,
			expected: fb.NodeStatusPending,
		},
		{
			name:     "Unknown status",
			status:   "unknown",
			expected: fb.NodeStatusPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := statusToFBStatus(tt.status)
			assert.Equal(t, tt.expected, result, "statusToFBStatus(%s) returned unexpected value", tt.status)
		})
	}
}

func TestFBStatusToNodeStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   fb.NodeStatus
		expected NodeStatus
	}{
		{
			name:     "Pending status",
			status:   fb.NodeStatusPending,
			expected: Pending,
		},
		{
			name:     "Running status",
			status:   fb.NodeStatusRunning,
			expected: Running,
		},
		{
			name:     "Completed status",
			status:   fb.NodeStatusCompleted,
			expected: Completed,
		},
		{
			name:     "Failed status",
			status:   fb.NodeStatusFailed,
			expected: Failed,
		},
		{
			name:     "Skipped status",
			status:   fb.NodeStatusSkipped,
			expected: Skipped,
		},
		{
			name:     "Unknown status",
			status:   fb.NodeStatus(127), // out-of-range value
			expected: Pending,            // should default to pending
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fbStatusToNodeStatus(tt.status)
			assert.Equal(t, tt.expected, result, "fbStatusToNodeStatus(%d) returned unexpected value", tt.status)
		})
	}
}

func TestStatusConversionRoundTrip(t *testing.T) {
	statuses := []NodeStatus{
		Pending,
		Running,
		Completed,
		Failed,
		Skipped,
	}

	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			// Convert to FB status and back
			fbStatus := statusToFBStatus(status)
			roundTrip := fbStatusToNodeStatus(fbStatus)

			// Should get the same status back
			assert.Equal(t, status, roundTrip, "Status conversion round trip failed for %s", status)
		})
	}
}

// TestValidateWorkflowID_RejectsTraversal covers F-2: workflow IDs that are not a
// single safe path segment must be rejected at every file-store path site, and a
// rejected Save/Delete must touch nothing outside baseDir. InMemoryStore has no
// filesystem surface and is intentionally not guarded, so it is not exercised here.
func TestValidateWorkflowID_RejectsTraversal(t *testing.T) {
	bad := []string{
		"../x",
		"../../tmp/evil",
		"a/b",
		"sub/child",
		"..",
		"/abs",
		"",
	}
	good := []string{"wf", "test-workflow", "wf_123", "a.b"}

	newStores := map[string]func(dir string) (WorkflowStore, error){
		"json": func(dir string) (WorkflowStore, error) { return NewJSONFileStore(dir) },
		"fb":   func(dir string) (WorkflowStore, error) { return NewFlatBuffersStore(dir) },
	}

	for name, mk := range newStores {
		t.Run(name, func(t *testing.T) {
			baseDir := t.TempDir()
			// A sibling dir + planted file OUTSIDE baseDir, to prove no escape.
			outside := t.TempDir()
			planted := filepath.Join(outside, "secret.fb")
			require.NoError(t, os.WriteFile(planted, []byte("keep me"), 0600))

			store, err := mk(baseDir)
			require.NoError(t, err)

			for _, id := range bad {
				// Save (ID derived from data) must error and write nothing.
				err := store.Save(NewWorkflowData(id))
				assert.Errorf(t, err, "Save(%q) should be rejected", id)

				// Load must error.
				_, err = store.Load(id)
				assert.Errorf(t, err, "Load(%q) should be rejected", id)

				// Delete must error.
				err = store.Delete(id)
				assert.Errorf(t, err, "Delete(%q) should be rejected", id)
			}

			// The planted file outside baseDir is untouched.
			_, statErr := os.Stat(planted)
			assert.NoError(t, statErr, "traversal must not delete files outside baseDir")

			// baseDir contains no escaped artifacts (only what legit Saves write).
			entries, err := os.ReadDir(baseDir)
			require.NoError(t, err)
			assert.Empty(t, entries, "no files should be written for rejected IDs")

			// Legit single-segment IDs still round-trip.
			for _, id := range good {
				require.NoErrorf(t, store.Save(NewWorkflowData(id)), "Save(%q) should succeed", id)
				_, err := store.Load(id)
				assert.NoErrorf(t, err, "Load(%q) should succeed", id)
			}
		})
	}
}

// TestFlatBuffersStore_LoadMalformed covers F-1: Load on malformed / truncated
// bytes must return an error, never panic the host process.
// assertMalformedRejected is the strengthened HARD-01 oracle: a malformed
// buffer must be rejected as errors.Is(ErrCorruptData), return data == nil, and
// never panic. The earlier oracle asserted only assert.Error — false-clean if
// Load returned ErrIO or recovered into a different category. (No panic escapes
// because store.Load's own recover() converts any residual panic to
// ErrCorruptData; this helper additionally guards against a panic leaking out
// of the test harness itself.)
func assertMalformedRejected(t *testing.T, name string, payload []byte) {
	t.Helper()
	baseDir := t.TempDir()
	store, err := NewFlatBuffersStore(baseDir)
	require.NoError(t, err)

	id := "corrupt"
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, id+".fb"), payload, 0600))

	var data *WorkflowData
	require.NotPanics(t, func() {
		data, err = store.Load(id)
	}, "Load of malformed %q must not panic", name)
	assert.ErrorIs(t, err, ErrCorruptData,
		"Load of malformed %q must return errors.Is(ErrCorruptData)", name)
	assert.Nil(t, data, "Load of malformed %q must return data == nil", name)
}

func TestFlatBuffersStore_LoadMalformed(t *testing.T) {
	// Named hand-cases kept as explicit regressions.
	cases := map[string][]byte{
		"empty":          {},
		"one-byte":       {0x01},
		"three-byte":     {0x01, 0x02, 0x03},
		"small-garbage":  {0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"oversized-root": {0xf0, 0xff, 0xff, 0x0f}, // 4-byte file, huge root offset
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			assertMalformedRejected(t, name, payload)
		})
	}

	// Generated corpus families over a real valid buffer — these earn the
	// ∀-claim (every malformed shape → ErrCorruptData, data==nil, no panic).
	valid, err := os.ReadFile(filepath.Join("testdata", "m1_format_int.fb"))
	require.NoError(t, err, "golden fixture must exist as the corpus base")
	require.Greater(t, len(valid), 8, "fixture must be non-trivial")

	// (a) truncated-at-every-prefix: buf[:n] for every n shorter than the whole.
	// HONEST SCOPE (matches CONTEXT's disclosed residual): the layered guard is
	// not a structural verifier. FlatBuffers places the root/vtable near the
	// front, so a NEAR-complete truncation can still decode the in-range scalar
	// fields and Load clean — that is in-bounds data, not a crash, and the guard
	// never promised to reject it. So the ∀-claim here is the real guarantee:
	// every prefix → NO PANIC, and (if err != nil) it is errors.Is(ErrCorruptData)
	// with data == nil. A nil-error Load of a near-complete prefix is allowed.
	t.Run("truncated-prefixes-no-panic", func(t *testing.T) {
		baseDir := t.TempDir()
		store, serr := NewFlatBuffersStore(baseDir)
		require.NoError(t, serr)
		for n := 0; n < len(valid); n++ {
			var data *WorkflowData
			var lerr error
			require.NotPanics(t, func() {
				require.NoError(t, os.WriteFile(filepath.Join(baseDir, "t.fb"), valid[:n], 0600))
				data, lerr = store.Load("t")
			}, "Load of trunc[:%d] must not panic", n)
			if lerr != nil {
				assert.ErrorIs(t, lerr, ErrCorruptData,
					"trunc[:%d] error must be ErrCorruptData", n)
				assert.Nil(t, data, "trunc[:%d] with error must return data == nil", n)
			}
		}
	})

	// (b) sampled single-bit-flips across the buffer. A flip may yield a still-
	// valid buffer (Load returns nil err) — that is NOT malformed, so only assert
	// the no-panic invariant here; the rejection oracle applies to the families
	// that are definitionally malformed (a,c,d,e). This is the fuzz-shaped
	// "never panic" property exercised deterministically.
	t.Run("bit-flips-no-panic", func(t *testing.T) {
		baseDir := t.TempDir()
		store, serr := NewFlatBuffersStore(baseDir)
		require.NoError(t, serr)
		for i := 0; i < len(valid); i += max(1, len(valid)/64) {
			for _, bit := range []byte{0x01, 0x80} {
				flipped := append([]byte(nil), valid...)
				flipped[i] ^= bit
				require.NoError(t, os.WriteFile(filepath.Join(baseDir, "f.fb"), flipped, 0600))
				require.NotPanics(t, func() {
					// err/data intentionally ignored: this asserts ONLY that a
					// bit-flipped file never panics Load (it may error or not).
					_, _ = store.Load("f") //nolint:errcheck // no-panic is the sole property here
				}, "Load of bit-flipped[%d^%#x] must not panic", i, bit)
			}
		}
	})

	// (c) oversized length-prefix / huge vector count (the alloc-bomb): a 4-byte
	// root offset claiming a vector far past the buffer. Exercises T2/T3.
	t.Run("alloc-bomb-root", func(t *testing.T) {
		assertMalformedRejected(t, "huge-root", []byte{0xff, 0xff, 0xff, 0x7f})
	})

	// (d) root-offset > len: a minimal 4-byte buffer whose root lands past EOF.
	t.Run("root-past-eof", func(t *testing.T) {
		assertMalformedRejected(t, "root-past-eof", []byte{0x10, 0x00, 0x00, 0x00})
	})

	// (e) valid-header + truncated-body: keep the valid root offset region but
	// cut the body so accessor offsets run off the end.
	t.Run("valid-header-truncated-body", func(t *testing.T) {
		if len(valid) > 12 {
			assertMalformedRejected(t, "header+trunc-body", valid[:len(valid)/2])
		}
	})
}
