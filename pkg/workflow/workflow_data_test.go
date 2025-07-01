package workflow

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NodeStatus constants for testing
const (
	NodeStatusPending   = NodeStatus("pending")
	NodeStatusRunning   = NodeStatus("running")
	NodeStatusCompleted = NodeStatus("completed")
)

func TestWorkflowData(t *testing.T) {
	t.Run("BasicOperations", func(t *testing.T) {
		// Create test data
		data := NewWorkflowData("test")
		data.Set("string1", "value1")
		data.Set("int1", 42)
		data.SetNodeStatus("node1", Running)
		data.SetOutput("node1", "output1")

		// Test Get operations
		val, exists := data.Get("string1")
		require.True(t, exists)
		require.Equal(t, "value1", val)

		val, exists = data.Get("int1")
		require.True(t, exists)
		require.Equal(t, 42, val)

		// Test node status
		status, exists := data.GetNodeStatus("node1")
		require.True(t, exists)
		require.Equal(t, Running, status)

		// Test node output
		output, exists := data.GetOutput("node1")
		require.True(t, exists)
		require.Equal(t, "output1", output)

		// Test non-existent keys
		_, exists = data.Get("nonexistent")
		require.False(t, exists)

		_, exists = data.GetNodeStatus("nonexistent")
		require.False(t, exists)

		_, exists = data.GetOutput("nonexistent")
		require.False(t, exists)
	})

	t.Run("NodeRunnable", func(t *testing.T) {
		data := NewWorkflowData("test")

		// Test when node doesn't exist
		require.True(t, data.IsNodeRunnable("nonexistent"))

		// Test when node exists but has no dependencies
		data.SetNodeStatus("node1", Completed)
		require.False(t, data.IsNodeRunnable("node1"))

		// Test when node has completed dependencies
		data.SetNodeStatus("dep1", Completed)
		data.SetNodeStatus("dep2", Completed)
		data.SetNodeStatus("node2", Pending)
		data.SetNodeStatus("node2:depends:dep1", Pending)
		data.SetNodeStatus("node2:depends:dep2", Pending)
		require.True(t, data.IsNodeRunnable("node2"))

		// Test when node has incomplete dependencies
		data.SetNodeStatus("dep3", Running)
		data.SetNodeStatus("node3", Pending)
		data.SetNodeStatus("node3:depends:dep3", Pending)
		require.False(t, data.IsNodeRunnable("node3"))
	})

	t.Run("GettersAndStats", func(t *testing.T) {
		data := NewWorkflowData("test")

		// Test GetMetrics
		metrics := data.GetMetrics()
		require.NotNil(t, metrics)

		// Test GetAllNodeStatuses and ListNodeStatuses
		data.SetNodeStatus("node1", Running)
		data.SetNodeStatus("node2", Completed)

		allStatuses := data.GetAllNodeStatuses()
		require.Equal(t, 2, len(allStatuses))
		require.Equal(t, Running, allStatuses["node1"])
		require.Equal(t, Completed, allStatuses["node2"])

		listedStatuses := data.ListNodeStatuses()
		require.Equal(t, allStatuses, listedStatuses)
	})

	t.Run("JSONPersistence", func(t *testing.T) {
		data := NewWorkflowData("test")
		data.Set("key1", "value1")
		data.SetNodeStatus("node1", Running)
		data.SetOutput("node1", "output1")

		// Create a temporary file
		tmpfile, err := os.CreateTemp("", "workflow_*.json")
		require.NoError(t, err)
		defer os.Remove(tmpfile.Name())

		// Test SaveToJSON
		err = data.SaveToJSON(tmpfile.Name())
		require.NoError(t, err)

		// Test LoadFromJSON
		newData := NewWorkflowData("test")
		err = newData.LoadFromJSON(tmpfile.Name())
		require.NoError(t, err)

		// Verify loaded data
		val, exists := newData.Get("key1")
		require.True(t, exists)
		require.Equal(t, "value1", val)

		status, exists := newData.GetNodeStatus("node1")
		require.True(t, exists)
		require.Equal(t, Running, status)

		output, exists := newData.GetOutput("node1")
		require.True(t, exists)
		require.Equal(t, "output1", output)
	})

	t.Run("ArenaStats", func(t *testing.T) {
		data := NewWorkflowDataWithArena("test", DefaultWorkflowDataConfig())

		// Test ResetArena
		data.Set("key1", "value1")
		data.ResetArena()
		_, exists := data.Get("key1")
		require.False(t, exists)

		// Test GetArenaStats
		stats := data.GetArenaStats()
		require.NotNil(t, stats)
		require.NotNil(t, stats["arena"])
		require.NotNil(t, stats["stringPool"])

		// Test with custom block size
		customData := NewWorkflowDataWithArenaBlockSize("test", DefaultWorkflowDataConfig(), 512)
		stats = customData.GetArenaStats()
		require.Equal(t, int64(512), stats["arena"]["blockSize"])
	})

	t.Run("FlatBufferOperations", func(t *testing.T) {
		// Create temporary file
		tmpFile := filepath.Join(t.TempDir(), "workflow.fb")

		// Create test data
		data := NewWorkflowData("test-flatbuffer")
		data.Set("string1", "value1")
		data.Set("int1", 42)
		data.SetNodeStatus("node1", Running)
		data.SetOutput("node1", "output1")

		// Save to FlatBuffer
		err := data.SaveToFlatBuffer(tmpFile)
		require.NoError(t, err)

		// Load into new workflow data
		newData := NewWorkflowData("test-flatbuffer")
		err = newData.LoadFromFlatBuffer(tmpFile)
		require.NoError(t, err)

		// Verify loaded data
		val, exists := newData.Get("string1")
		require.True(t, exists)
		require.Equal(t, "value1", val)

		val, exists = newData.Get("int1")
		require.True(t, exists)
		require.Equal(t, 42, val)

		status, exists := newData.GetNodeStatus("node1")
		require.True(t, exists)
		require.Equal(t, Running, status)

		output, exists := newData.GetOutput("node1")
		require.True(t, exists)
		require.Equal(t, "output1", output)

		// Test error cases
		err = data.SaveToFlatBuffer("/invalid/path/workflow.fb")
		require.Error(t, err)

		err = data.LoadFromFlatBuffer("nonexistent.fb")
		require.Error(t, err)

		invalidFile := filepath.Join(t.TempDir(), "invalid.fb")
		err = os.WriteFile(invalidFile, []byte("invalid data"), 0600)
		require.NoError(t, err)
		err = data.LoadFromFlatBuffer(invalidFile)
		require.Error(t, err)
	})

	t.Run("Arena Operations", func(t *testing.T) {
		data := NewWorkflowDataWithArena("test-workflow", DefaultWorkflowDataConfig(), 512)

		// Test arena allocation through normal operations
		data.Set("key1", "value1")
		data.Set("key2", "value2")
		data.SetNodeStatus("node1", NodeStatusPending)
		data.SetOutput("node1", "output1")

		// Get arena stats
		stats := data.GetArenaStats()
		assert.NotNil(t, stats)
		assert.Contains(t, stats, "arena")
		assert.Contains(t, stats, "stringPool")

		// Test arena reset
		data.ResetArena()
		stats = data.GetArenaStats()
		assert.NotNil(t, stats["arena"])
		assert.Equal(t, int64(512), stats["arena"]["blockSize"])
	})
}

// Helper function to test map cloning
func TestCloneMap(t *testing.T) {
	original := map[string]interface{}{
		"string": "value",
		"int":    42,
		"bool":   true,
		"map":    map[string]interface{}{"nested": "value"},
		"slice":  []interface{}{1, 2, 3},
	}

	cloned := cloneMap(original)

	// Verify all values are copied
	require.Equal(t, original["string"], cloned["string"])
	require.Equal(t, original["int"], cloned["int"])
	require.Equal(t, original["bool"], cloned["bool"])

	// Verify nested structures are deep copied
	originalMap := original["map"].(map[string]interface{})
	clonedMap := cloned["map"].(map[string]interface{})
	require.Equal(t, originalMap["nested"], clonedMap["nested"])

	originalSlice := original["slice"].([]interface{})
	clonedSlice := cloned["slice"].([]interface{})
	require.Equal(t, originalSlice, clonedSlice)

	// Verify modifying clone doesn't affect original
	clonedMap["nested"] = "new value"
	require.NotEqual(t, originalMap["nested"], clonedMap["nested"])
	require.Equal(t, "value", originalMap["nested"])
}

func TestWorkflowDataCoreOperations(t *testing.T) {
	t.Run("Basic Data Operations", func(t *testing.T) {
		data := NewWorkflowData("test-workflow")

		// Test Set and Get
		data.Set("string", "value")
		data.Set("int", 42)
		data.Set("float", 3.14)
		data.Set("bool", true)

		// Test GetString
		str, ok := data.GetString("string")
		assert.True(t, ok)
		assert.Equal(t, "value", str)

		// Test GetInt
		num, ok := data.GetInt("int")
		assert.True(t, ok)
		assert.Equal(t, 42, num)

		// Test GetFloat64
		f, ok := data.GetFloat64("float")
		assert.True(t, ok)
		assert.Equal(t, 3.14, f)

		// Test GetBool
		b, ok := data.GetBool("bool")
		assert.True(t, ok)
		assert.True(t, b)

		// Test non-existent keys
		_, ok = data.GetString("nonexistent")
		assert.False(t, ok)
		_, ok = data.GetInt("nonexistent")
		assert.False(t, ok)
		_, ok = data.GetFloat64("nonexistent")
		assert.False(t, ok)
		_, ok = data.GetBool("nonexistent")
		assert.False(t, ok)

		// Test type mismatches
		_, ok = data.GetString("int")
		assert.False(t, ok)
		_, ok = data.GetInt("string")
		assert.False(t, ok)
		_, ok = data.GetFloat64("bool")
		assert.False(t, ok)
		_, ok = data.GetBool("float")
		assert.False(t, ok)

		// Test Delete
		assert.True(t, data.Delete("string"))
		assert.False(t, data.Delete("nonexistent"))

		// Test HasKey
		assert.True(t, data.HasKey("int"))
		assert.False(t, data.HasKey("string"))

		// Test Keys
		keys := data.Keys()
		assert.Equal(t, 3, len(keys))
		assert.Contains(t, keys, "int")
		assert.Contains(t, keys, "float")
		assert.Contains(t, keys, "bool")
	})

	t.Run("Node Status Operations", func(t *testing.T) {
		data := NewWorkflowData("test-workflow")

		// Test SetNodeStatus and GetNodeStatus
		data.SetNodeStatus("node1", NodeStatusPending)
		status, exists := data.GetNodeStatus("node1")
		assert.True(t, exists)
		assert.Equal(t, NodeStatusPending, status)

		// Test non-existent node
		_, exists = data.GetNodeStatus("nonexistent")
		assert.False(t, exists)

		// Test GetAllNodeStatuses
		data.SetNodeStatus("node2", NodeStatusRunning)
		data.SetNodeStatus("node3", NodeStatusCompleted)

		statuses := data.GetAllNodeStatuses()
		assert.Equal(t, 3, len(statuses))
		assert.Equal(t, NodeStatusPending, statuses["node1"])
		assert.Equal(t, NodeStatusRunning, statuses["node2"])
		assert.Equal(t, NodeStatusCompleted, statuses["node3"])

		// Test ForEachNodeStatus
		count := 0
		data.ForEachNodeStatus(func(nodeName string, status NodeStatus) {
			count++
			assert.Contains(t, statuses, nodeName)
			assert.Equal(t, statuses[nodeName], status)
		})
		assert.Equal(t, 3, count)
	})

	t.Run("Node Output Operations", func(t *testing.T) {
		data := NewWorkflowData("test-workflow")

		// Test SetOutput and GetOutput
		data.SetOutput("node1", "output1")
		output, exists := data.GetOutput("node1")
		assert.True(t, exists)
		assert.Equal(t, "output1", output)

		// Test non-existent output
		_, exists = data.GetOutput("nonexistent")
		assert.False(t, exists)

		// Test ForEachOutput
		data.SetOutput("node2", 42)
		data.SetOutput("node3", true)

		count := 0
		data.ForEachOutput(func(nodeName string, output interface{}) {
			count++
			actual, exists := data.GetOutput(nodeName)
			assert.True(t, exists)
			assert.Equal(t, output, actual)
		})
		assert.Equal(t, 3, count)
	})

	t.Run("Arena Operations", func(t *testing.T) {
		data := NewWorkflowDataWithArena("test-workflow", DefaultWorkflowDataConfig(), 512)

		// Test arena allocation through normal operations
		data.Set("key1", "value1")
		data.Set("key2", "value2")
		data.SetNodeStatus("node1", NodeStatusPending)
		data.SetOutput("node1", "output1")

		// Get arena stats
		stats := data.GetArenaStats()
		assert.NotNil(t, stats)
		assert.Contains(t, stats, "arena")
		assert.Contains(t, stats, "stringPool")

		// Test arena reset
		data.ResetArena()
		stats = data.GetArenaStats()
		assert.NotNil(t, stats["arena"])
		assert.Equal(t, int64(512), stats["arena"]["blockSize"])
	})
}

// Add this test to improve coverage of the Clone function
func TestWorkflowDataClone(t *testing.T) {
	original := NewWorkflowData("test-workflow")

	// Add various types of data
	original.Set("string", "value")
	original.Set("int", 42)
	original.Set("map", map[string]interface{}{"key": "value"})
	original.Set("slice", []interface{}{1, 2, 3})
	original.SetNodeStatus("node1", NodeStatusPending)
	original.SetOutput("node1", "output1")

	// Clone the data
	clone := original.Clone()

	// Verify all data was copied correctly
	assert.Equal(t, original.ID, clone.ID)

	// Verify basic values
	val, exists := clone.Get("string")
	assert.True(t, exists)
	assert.Equal(t, "value", val)

	val, exists = clone.Get("int")
	assert.True(t, exists)
	assert.Equal(t, 42, val)

	// Verify map was deep copied
	val, exists = clone.Get("map")
	assert.True(t, exists)
	m, ok := val.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "value", m["key"])

	// Verify slice was deep copied
	val, exists = clone.Get("slice")
	assert.True(t, exists)
	s, ok := val.([]interface{})
	assert.True(t, ok)
	assert.Equal(t, []interface{}{1, 2, 3}, s)

	// Verify node status was copied
	status, exists := clone.GetNodeStatus("node1")
	assert.True(t, exists)
	assert.Equal(t, NodeStatusPending, status)

	// Verify output was copied
	output, exists := clone.GetOutput("node1")
	assert.True(t, exists)
	assert.Equal(t, "output1", output)

	// Modify clone and verify original is unchanged
	clone.Set("string", "new-value")
	val, _ = original.Get("string")
	assert.Equal(t, "value", val)

	clone.SetNodeStatus("node1", NodeStatusCompleted)
	status, _ = original.GetNodeStatus("node1")
	assert.Equal(t, NodeStatusPending, status)
}

func mockSecureRandomFloat64(mockValue float64) func() {
	originalRandom := utils.SecureRandomFloat64
	utils.SecureRandomFloat64 = func() float64 {
		return mockValue
	}
	return func() {
		utils.SecureRandomFloat64 = originalRandom
	}
}

func TestGetFloat64(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		value         interface{}
		expectedValue float64
		expectedFound bool
		metricsConfig WorkflowDataConfig
		mockRandom    float64
	}{
		{
			name:          "Get float64 value",
			key:           "float64_key",
			value:         float64(123.456),
			expectedValue: 123.456,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Non-existent key",
			key:           "non_existent",
			expectedValue: 0,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Invalid type (bool)",
			key:           "bool_key",
			value:         true,
			expectedValue: 0,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "With metrics disabled",
			key:           "float64_key",
			value:         float64(123.456),
			expectedValue: 123.456,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(false, 1.0)},
		},
		{
			name:          "With metrics sampling - below threshold",
			key:           "float64_key",
			value:         float64(42),
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.4, // Below sampling rate, should collect metrics
		},
		{
			name:          "With metrics sampling - above threshold",
			key:           "float64_key",
			value:         float64(42),
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.6, // Above sampling rate, should NOT collect metrics
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock SecureRandomFloat64 for this test BEFORE creating any metrics collectors
			restore := mockSecureRandomFloat64(tt.mockRandom)
			defer restore()

			// Reset the default metrics collector before each test
			metrics.Reset()

			// Create workflow data with the test config
			data := NewWorkflowDataWithConfig("test", tt.metricsConfig)

			// Set test value if not testing non-existent key
			if tt.value != nil {
				data.Set(tt.key, tt.value)
			}

			// Test single access first to verify metrics sampling
			value, found := data.GetFloat64(tt.key)
			assert.Equal(t, tt.expectedFound, found)
			if found {
				assert.Equal(t, tt.expectedValue, value)
			}

			// Verify metrics if enabled
			if tt.metricsConfig.MetricsConfig.GetInternalConfig().Enabled {
				m := data.GetMetrics()
				stats, exists := m.GetOperationStats(metrics.OpGetFloat64)

				if tt.mockRandom == 0 || tt.mockRandom <= tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate {
					// For cases where metrics should be collected
					assert.True(t, exists, "Expected metrics to exist when random value (%v) is less than or equal to sampling rate (%v)",
						tt.mockRandom, tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate)
					assert.Equal(t, int64(1), stats.Count, "Expected exactly one metric to be collected")
				} else {
					// For cases where metrics should be skipped
					if exists {
						assert.Zero(t, stats.Count, "Expected no metrics to be collected when random value (%v) is greater than sampling rate (%v)",
							tt.mockRandom, tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate)
					}
				}
			}

			// Now test concurrent access (after metrics verification)
			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					value, found := data.GetFloat64(tt.key)
					assert.Equal(t, tt.expectedFound, found)
					if found {
						assert.Equal(t, tt.expectedValue, value)
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestGetBool(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		value         interface{}
		expectedValue bool
		expectedFound bool
		metricsConfig WorkflowDataConfig
		mockRandom    float64
	}{
		{
			name:          "Get bool value - true",
			key:           "bool_key",
			value:         true,
			expectedValue: true,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Get bool value - false",
			key:           "bool_key",
			value:         false,
			expectedValue: false,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Non-existent key",
			key:           "non_existent",
			expectedValue: false,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Invalid type (string)",
			key:           "string_key",
			value:         "true",
			expectedValue: false,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "With metrics disabled",
			key:           "bool_key",
			value:         true,
			expectedValue: true,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(false, 1.0)},
		},
		{
			name:          "With metrics sampling - below threshold",
			key:           "bool_key",
			value:         true,
			expectedValue: true,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.4, // Below sampling rate, should collect metrics
		},
		{
			name:          "With metrics sampling - above threshold",
			key:           "bool_key",
			value:         true,
			expectedValue: true,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.6, // Above sampling rate, should NOT collect metrics
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock SecureRandomFloat64 for this test BEFORE creating any metrics collectors
			restore := mockSecureRandomFloat64(tt.mockRandom)
			defer restore()

			// Reset the default metrics collector before each test
			metrics.Reset()

			// Create workflow data with the test config
			data := NewWorkflowDataWithConfig("test", tt.metricsConfig)

			// Set test value if not testing non-existent key
			if tt.value != nil {
				data.Set(tt.key, tt.value)
			}

			// Test single access first to verify metrics sampling
			value, found := data.GetBool(tt.key)
			assert.Equal(t, tt.expectedFound, found)
			if found {
				assert.Equal(t, tt.expectedValue, value)
			}

			// Verify metrics if enabled
			if tt.metricsConfig.MetricsConfig.GetInternalConfig().Enabled {
				m := data.GetMetrics()
				stats, exists := m.GetOperationStats(metrics.OpGetBool)

				if tt.mockRandom == 0 || tt.mockRandom <= tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate {
					assert.True(t, exists)
					assert.Equal(t, int64(1), stats.Count)
				} else {
					if exists {
						assert.Zero(t, stats.Count)
					}
				}
			}

			// Test concurrent access
			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					value, found := data.GetBool(tt.key)
					assert.Equal(t, tt.expectedFound, found)
					if found {
						assert.Equal(t, tt.expectedValue, value)
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestGetString(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		value         interface{}
		expectedValue string
		expectedFound bool
		metricsConfig WorkflowDataConfig
		mockRandom    float64
	}{
		{
			name:          "Get string value",
			key:           "string_key",
			value:         "test value",
			expectedValue: "test value",
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Get empty string",
			key:           "empty_key",
			value:         "",
			expectedValue: "",
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Non-existent key",
			key:           "non_existent",
			expectedValue: "",
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Invalid type (bool)",
			key:           "bool_key",
			value:         true,
			expectedValue: "",
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "With metrics disabled",
			key:           "string_key",
			value:         "test value",
			expectedValue: "test value",
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(false, 1.0)},
		},
		{
			name:          "With metrics sampling - below threshold",
			key:           "string_key",
			value:         "test value",
			expectedValue: "test value",
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.4,
		},
		{
			name:          "With metrics sampling - above threshold",
			key:           "string_key",
			value:         "test value",
			expectedValue: "test value",
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := mockSecureRandomFloat64(tt.mockRandom)
			defer restore()

			metrics.Reset()

			data := NewWorkflowDataWithConfig("test", tt.metricsConfig)

			if tt.value != nil {
				data.Set(tt.key, tt.value)
			}

			value, found := data.GetString(tt.key)
			assert.Equal(t, tt.expectedFound, found)
			if found {
				assert.Equal(t, tt.expectedValue, value)
			}

			if tt.metricsConfig.MetricsConfig.GetInternalConfig().Enabled {
				m := data.GetMetrics()
				stats, exists := m.GetOperationStats(metrics.OpGetString)

				if tt.mockRandom == 0 || tt.mockRandom <= tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate {
					assert.True(t, exists)
					assert.Equal(t, int64(1), stats.Count)
				} else {
					if exists {
						assert.Zero(t, stats.Count)
					}
				}
			}

			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					value, found := data.GetString(tt.key)
					assert.Equal(t, tt.expectedFound, found)
					if found {
						assert.Equal(t, tt.expectedValue, value)
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestGetInt(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		value         interface{}
		expectedValue int
		expectedFound bool
		metricsConfig WorkflowDataConfig
		mockRandom    float64
	}{
		{
			name:          "Get int value",
			key:           "int_key",
			value:         42,
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Get zero value",
			key:           "zero_key",
			value:         0,
			expectedValue: 0,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Get negative value",
			key:           "negative_key",
			value:         -42,
			expectedValue: -42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Non-existent key",
			key:           "non_existent",
			expectedValue: 0,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "Invalid type (string)",
			key:           "string_key",
			value:         "42",
			expectedValue: 0,
			expectedFound: false,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 1.0)},
		},
		{
			name:          "With metrics disabled",
			key:           "int_key",
			value:         42,
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(false, 1.0)},
		},
		{
			name:          "With metrics sampling - below threshold",
			key:           "int_key",
			value:         42,
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.4,
		},
		{
			name:          "With metrics sampling - above threshold",
			key:           "int_key",
			value:         42,
			expectedValue: 42,
			expectedFound: true,
			metricsConfig: WorkflowDataConfig{MetricsConfig: metrics.ConfigFromLiteral(true, 0.5)},
			mockRandom:    0.6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := mockSecureRandomFloat64(tt.mockRandom)
			defer restore()

			metrics.Reset()

			data := NewWorkflowDataWithConfig("test", tt.metricsConfig)

			if tt.value != nil {
				data.Set(tt.key, tt.value)
			}

			value, found := data.GetInt(tt.key)
			assert.Equal(t, tt.expectedFound, found)
			if found {
				assert.Equal(t, tt.expectedValue, value)
			}

			if tt.metricsConfig.MetricsConfig.GetInternalConfig().Enabled {
				m := data.GetMetrics()
				stats, exists := m.GetOperationStats(metrics.OpGetInt)

				if tt.mockRandom == 0 || tt.mockRandom <= tt.metricsConfig.MetricsConfig.GetInternalConfig().SamplingRate {
					assert.True(t, exists)
					assert.Equal(t, int64(1), stats.Count)
				} else {
					if exists {
						assert.Zero(t, stats.Count)
					}
				}
			}

			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					value, found := data.GetInt(tt.key)
					assert.Equal(t, tt.expectedFound, found)
					if found {
						assert.Equal(t, tt.expectedValue, value)
					}
				}()
			}
			wg.Wait()
		})
	}
}
