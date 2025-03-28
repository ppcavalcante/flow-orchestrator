package workflow

import (
	"os"
	"path/filepath"
	"testing"

	fb "github.com/pparaujo/flow-orchestrator/pkg/workflow/fb/workflow"
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
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "workflow-store-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

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
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "workflow-store-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

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
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "workflow-store-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

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

	// Test cleanup
	fbStore, err = jsonStore.MigrateToFlatBuffers(true)
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
		status   byte
		expected NodeStatus
	}{
		{
			name:     "Pending status",
			status:   byte(fb.NodeStatusPending),
			expected: Pending,
		},
		{
			name:     "Running status",
			status:   byte(fb.NodeStatusRunning),
			expected: Running,
		},
		{
			name:     "Completed status",
			status:   byte(fb.NodeStatusCompleted),
			expected: Completed,
		},
		{
			name:     "Failed status",
			status:   byte(fb.NodeStatusFailed),
			expected: Failed,
		},
		{
			name:     "Skipped status",
			status:   byte(fb.NodeStatusSkipped),
			expected: Skipped,
		},
		{
			name:     "Unknown status",
			status:   byte(255), // Invalid status value
			expected: Pending,   // Should default to pending
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
			fbStatus := StatusToFBStatus(status)
			roundTrip := fbStatusToNodeStatus(byte(fbStatus))

			// Should get the same status back
			assert.Equal(t, status, roundTrip, "Status conversion round trip failed for %s", status)
		})
	}
}
