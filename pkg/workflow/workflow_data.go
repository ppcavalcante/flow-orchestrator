package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow/arena"
	"github.com/pparaujo/flow-orchestrator/pkg/workflow/metrics"
	"github.com/pparaujo/flow-orchestrator/pkg/workflow/utils"
)

// WorkflowData is the central data store for workflow execution.
// It maintains the state of the workflow, including node statuses and outputs.
type WorkflowData struct {
	// Single mutex for data access - simpler and more reliable
	mu sync.RWMutex

	// Simple maps for data storage
	data       map[string]interface{}
	nodeStatus map[string]NodeStatus
	outputs    map[string]interface{} // Renamed from nodeOutput for compatibility

	// Keep the ID and metrics configuration
	ID      string
	metrics *metrics.MetricsCollector

	// String interning for efficiency
	stringInterner *StringInterner

	// Add arena field to the WorkflowData struct
	arena      *arena.Arena      // Arena for memory management
	stringPool *arena.StringPool // String pool for string interning
}

// NewWorkflowData creates a new workflow data instance with the given ID.
func NewWorkflowData(id string) *WorkflowData {
	return NewWorkflowDataWithConfig(id, DefaultWorkflowDataConfig())
}

// NewWorkflowDataWithConfig creates a new workflow data instance with the specified configuration.
// This allows customizing memory usage, string interning, and metrics collection.
func NewWorkflowDataWithConfig(id string, config WorkflowDataConfig) *WorkflowData {
	// Create a metrics collector with the specified configuration
	var metricsCollector *metrics.MetricsCollector
	if config.MetricsConfig != nil {
		metricsCollector = metrics.NewMetricsCollectorWithConfig(config.MetricsConfig.GetInternalConfig())
	} else {
		metricsCollector = metrics.NewMetricsCollector()
	}

	// Create a string interner
	stringInterner := NewStringInterner()

	// Create the workflow data with simple maps
	return &WorkflowData{
		ID:             id,
		data:           make(map[string]interface{}, config.ExpectedData),
		nodeStatus:     make(map[string]NodeStatus, config.ExpectedNodes),
		outputs:        make(map[string]interface{}, config.ExpectedNodes),
		metrics:        metricsCollector,
		stringInterner: stringInterner,
	}
}

// NewWorkflowDataWithArena creates a new workflow data instance with an arena allocator.
// If blockSize is not provided, a default block size will be used.
func NewWorkflowDataWithArena(id string, config WorkflowDataConfig, blockSize ...int) *WorkflowData {
	// Create a metrics collector with the specified configuration
	var metricsCollector *metrics.MetricsCollector
	if config.MetricsConfig != nil {
		metricsCollector = metrics.NewMetricsCollectorWithConfig(config.MetricsConfig.GetInternalConfig())
	} else {
		metricsCollector = metrics.NewMetricsCollector()
	}

	// Create an arena allocator with the specified block size
	var a *arena.Arena
	if len(blockSize) > 0 && blockSize[0] > 0 {
		a = arena.NewArenaWithBlockSize(blockSize[0])
	} else {
		a = arena.NewArena()
	}

	// Create a string pool for interning strings
	stringPool := arena.NewStringPool(a)

	// Create the workflow data with arena-backed maps
	return &WorkflowData{
		ID:             id,
		data:           make(map[string]interface{}, config.ExpectedData),
		nodeStatus:     make(map[string]NodeStatus, config.ExpectedNodes),
		outputs:        make(map[string]interface{}, config.ExpectedNodes),
		metrics:        metricsCollector,
		stringInterner: nil, // Not used with arena
		arena:          a,
		stringPool:     stringPool,
	}
}

// NewWorkflowDataWithArenaBlockSize creates a new workflow data instance with arena allocation and a specific block size.
// This function is maintained for backward compatibility.
func NewWorkflowDataWithArenaBlockSize(id string, config WorkflowDataConfig, blockSize int) *WorkflowData {
	return NewWorkflowDataWithArena(id, config, blockSize)
}

// Set stores a value in the workflow data.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Set(key string, value interface{}) {
	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSet, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.data[w.internKey(key)] = value
	})
}

// Get retrieves a value from the workflow data.
// Returns the value and a boolean indicating if the key exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Get(key string) (interface{}, bool) {
	var result interface{}
	var exists bool

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGet, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, exists = w.data[w.internKey(key)]
	})

	return result, exists
}

// Delete removes a key-value pair from the workflow data.
// Returns true if the key existed and was deleted.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Delete(key string) bool {
	var existed bool

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpDelete, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		internedKey := w.internKey(key)
		_, existed = w.data[internedKey]
		if existed {
			delete(w.data, internedKey)
		}
	})

	return existed
}

// SetNodeStatus updates the status of a node in the workflow.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) SetNodeStatus(nodeName string, status NodeStatus) {
	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSetStatus, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.nodeStatus[w.internKey(nodeName)] = status
	})
}

// GetNodeStatus retrieves the status of a node in the workflow.
// Returns the status and a boolean indicating if the node exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) GetNodeStatus(nodeName string) (NodeStatus, bool) {
	var status NodeStatus
	var exists bool

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetStatus, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		status, exists = w.nodeStatus[w.internKey(nodeName)]
	})

	return status, exists
}

// SetOutput stores the output of a node.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) SetOutput(nodeName string, output interface{}) {
	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSetOutput, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.outputs[w.internKey(nodeName)] = output
	})
}

// GetOutput retrieves the output of a node.
// Returns the output and a boolean indicating if the output exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) GetOutput(nodeName string) (interface{}, bool) {
	var output interface{}
	var exists bool

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetOutput, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		output, exists = w.outputs[w.internKey(nodeName)]
	})

	return output, exists
}

// internKey interns a string key to reduce memory usage.
// This is an internal helper method.
func (w *WorkflowData) internKey(key string) string {
	// If we're using an arena, use the string pool
	if w.stringPool != nil {
		return w.stringPool.Intern(key)
	}

	// Otherwise use the global string interner
	if w.stringInterner != nil {
		return w.stringInterner.Intern(key)
	}

	// Fallback to the original string
	return key
}

// IsNodeRunnable checks if a node is runnable (all dependencies completed)
func (w *WorkflowData) IsNodeRunnable(nodeName string) bool {
	// Skip metrics if disabled or sampling
	if !w.metrics.IsEnabled() || (w.metrics.GetSamplingRate() < 1.0 && utils.SecureRandomFloat64() > w.metrics.GetSamplingRate()) {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.isNodeRunnableInternal(nodeName)
	}

	// With metrics
	var result bool
	w.metrics.TrackOperation(metrics.OpIsNodeRunnable, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result = w.isNodeRunnableInternal(nodeName)
	})
	return result
}

// isNodeRunnableInternal is the internal implementation of IsNodeRunnable
// Caller must hold the read lock
func (w *WorkflowData) isNodeRunnableInternal(nodeName string) bool {
	// If the node is already running, completed, failed, or skipped, it's not runnable
	if status, ok := w.nodeStatus[w.internKey(nodeName)]; ok {
		if status == Running || status == Completed || status == Failed || status == Skipped {
			return false
		}
	}

	// Check dependencies by looking for keys with the pattern nodeName:depends:*
	prefix := nodeName + ":depends:"
	for key := range w.nodeStatus {
		if strings.HasPrefix(key, prefix) {
			depName := strings.TrimPrefix(key, prefix)
			depStatus, exists := w.nodeStatus[w.internKey(depName)]
			// If dependency doesn't exist or isn't completed, node is not runnable
			if !exists || depStatus != Completed {
				return false
			}
		}
	}

	// Node is runnable if it has no dependencies or all dependencies are completed
	return true
}

// Snapshot creates a snapshot of the workflow data
func (w *WorkflowData) Snapshot() ([]byte, error) {
	// Skip metrics if disabled or sampling
	if !w.metrics.IsEnabled() || (w.metrics.GetSamplingRate() < 1.0 && utils.SecureRandomFloat64() > w.metrics.GetSamplingRate()) {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.createSnapshot()
	}

	// With metrics
	var result []byte
	var err error
	w.metrics.TrackOperation(metrics.OpSnapshot, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, err = w.createSnapshot()
	})

	return result, err
}

// createSnapshot creates a snapshot of the workflow data
// Caller must hold the read lock
func (w *WorkflowData) createSnapshot() ([]byte, error) {
	// Create a snapshot structure
	snapshot := map[string]interface{}{
		"id":         w.ID,
		"data":       w.data,
		"nodeStatus": w.nodeStatus,
		"outputs":    w.outputs,
	}

	// Serialize to JSON
	return json.Marshal(snapshot)
}

// LoadSnapshot loads a snapshot into the workflow data
func (w *WorkflowData) LoadSnapshot(data []byte) error {
	// Skip metrics if disabled or sampling
	if !w.metrics.IsEnabled() || (w.metrics.GetSamplingRate() < 1.0 && utils.SecureRandomFloat64() > w.metrics.GetSamplingRate()) {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.loadSnapshotInternal(data)
	}

	// With metrics
	var err error
	w.metrics.TrackOperation(metrics.OpLoadSnapshot, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		err = w.loadSnapshotInternal(data)
	})

	return err
}

// loadSnapshotInternal loads a snapshot into the workflow data
// Caller must hold the write lock
func (w *WorkflowData) loadSnapshotInternal(data []byte) error {
	// Deserialize from JSON
	var snapshot map[string]interface{}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}

	// Update ID
	if id, ok := snapshot["id"].(string); ok {
		w.ID = id
	}

	// Update data with type conversion
	if data, ok := snapshot["data"].(map[string]interface{}); ok {
		w.data = make(map[string]interface{})
		for k, v := range data {
			// Convert numbers to appropriate types
			switch val := v.(type) {
			case float64:
				// Check if it's actually an integer
				if float64(int(val)) == val {
					w.data[w.internKey(k)] = int(val)
				} else {
					w.data[w.internKey(k)] = val
				}
			default:
				w.data[w.internKey(k)] = v
			}
		}
	}

	// Update node status
	if nodeStatus, ok := snapshot["nodeStatus"].(map[string]interface{}); ok {
		w.nodeStatus = make(map[string]NodeStatus)
		for k, v := range nodeStatus {
			if status, ok := v.(string); ok {
				w.nodeStatus[w.internKey(k)] = NodeStatus(status)
			}
		}
	}

	// Update outputs
	if outputs, ok := snapshot["outputs"].(map[string]interface{}); ok {
		w.outputs = make(map[string]interface{})
		for k, v := range outputs {
			w.outputs[w.internKey(k)] = v
		}
	}

	return nil
}

// GetWorkflowID returns the unique identifier for this workflow
func (w *WorkflowData) GetWorkflowID() string {
	return w.ID
}

// GetMetrics returns the metrics collector
func (w *WorkflowData) GetMetrics() *metrics.MetricsCollector {
	return w.metrics
}

// GetAllNodeStatuses returns a copy of all node statuses
func (w *WorkflowData) GetAllNodeStatuses() map[string]NodeStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()

	result := make(map[string]NodeStatus)

	for k, v := range w.nodeStatus {
		result[k] = v
	}

	return result
}

// ListNodeStatuses is an alias for GetAllNodeStatuses for backward compatibility
func (w *WorkflowData) ListNodeStatuses() map[string]NodeStatus {
	return w.GetAllNodeStatuses()
}

// ForEach iterates over all key-value pairs in the data map
func (w *WorkflowData) ForEach(fn func(key string, value interface{})) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for k, v := range w.data {
		fn(k, v)
	}
}

// ForEachNodeStatus iterates over all node statuses
func (w *WorkflowData) ForEachNodeStatus(fn func(nodeName string, status NodeStatus)) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for k, v := range w.nodeStatus {
		fn(k, v)
	}
}

// ForEachOutput iterates over all node outputs
func (w *WorkflowData) ForEachOutput(fn func(nodeName string, output interface{})) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for k, v := range w.outputs {
		// Deep copy maps to prevent comparison issues
		if m, ok := v.(map[string]interface{}); ok {
			fn(k, cloneMap(m))
		} else {
			fn(k, v)
		}
	}
}

// Clone creates a deep copy of the WorkflowData
func (w *WorkflowData) Clone() *WorkflowData {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// Create a new WorkflowData with the same ID and configuration
	clone := &WorkflowData{
		ID:         w.ID,
		metrics:    w.metrics,
		data:       make(map[string]interface{}, len(w.data)),
		nodeStatus: make(map[string]NodeStatus, len(w.nodeStatus)),
		outputs:    make(map[string]interface{}, len(w.outputs)),
		arena:      w.arena,
		stringPool: w.stringPool,
	}

	// Copy data
	for k, v := range w.data {
		clone.data[k] = v
	}

	// Copy node statuses
	for k, v := range w.nodeStatus {
		clone.nodeStatus[k] = v
	}

	// Copy outputs
	for k, v := range w.outputs {
		clone.outputs[k] = v
	}

	return clone
}

// GetBool gets a boolean value from the workflow data
func (w *WorkflowData) GetBool(key string) (bool, bool) {
	var result bool
	var found bool
	w.metrics.TrackOperation(metrics.OpGetBool, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[w.internKey(key)]
		if !ok {
			found = false
			return
		}
		boolVal, ok := val.(bool)
		result = boolVal
		found = ok
	})
	return result, found
}

// GetString gets a string value from the workflow data
func (w *WorkflowData) GetString(key string) (string, bool) {
	var result string
	var found bool
	w.metrics.TrackOperation(metrics.OpGetString, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[w.internKey(key)]
		if !ok {
			found = false
			return
		}
		strVal, ok := val.(string)
		result = strVal
		found = ok
	})
	return result, found
}

// GetFloat64 gets a float64 value from the workflow data
func (w *WorkflowData) GetFloat64(key string) (float64, bool) {
	var result float64
	var found bool

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetFloat64, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[w.internKey(key)]
		if !ok {
			found = false
			return
		}
		switch v := val.(type) {
		case float64:
			result, found = v, true
		case float32:
			result, found = float64(v), true
		case int:
			result, found = float64(v), true
		case int64:
			result, found = float64(v), true
		case int32:
			result, found = float64(v), true
		default:
			found = false
		}
	})

	return result, found
}

// GetInt gets an int value from the workflow data
func (w *WorkflowData) GetInt(key string) (int, bool) {
	var result int
	var found bool
	w.metrics.TrackOperation(metrics.OpGetInt, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[w.internKey(key)]
		if !ok {
			found = false
			return
		}
		intVal, ok := val.(int)
		result = intVal
		found = ok
	})
	return result, found
}

// SaveToJSON saves the workflow data to a JSON file
func (w *WorkflowData) SaveToJSON(filePath string) error {
	// Create a snapshot of the data
	data, err := w.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Write the file
	err = os.WriteFile(filePath, data, 0600)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// LoadFromJSON loads the workflow data from a JSON file
func (w *WorkflowData) LoadFromJSON(filePath string) error {
	// Read the file
	// nolint:gosec // This is an internal function with controlled file paths
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read JSON file: %w", err)
	}

	// Load snapshot
	err = w.LoadSnapshot(data)
	if err != nil {
		return fmt.Errorf("failed to load snapshot: %w", err)
	}

	return nil
}

// SaveToFlatBuffer saves the workflow data to a FlatBuffer file
func (w *WorkflowData) SaveToFlatBuffer(filePath string) error {
	// Create a snapshot of the data
	data, err := w.createSnapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Write the file
	err = os.WriteFile(filePath, data, 0600)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// LoadFromFlatBuffer loads the workflow data from a FlatBuffer file
func (w *WorkflowData) LoadFromFlatBuffer(filePath string) error {
	// Read the file
	// nolint:gosec // This is an internal function with controlled file paths
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read FlatBuffer file: %w", err)
	}

	// Load snapshot (just using JSON for now)
	err = w.LoadSnapshot(data)
	if err != nil {
		return fmt.Errorf("failed to load snapshot: %w", err)
	}

	return nil
}

// Keys returns all keys in the data map
func (w *WorkflowData) Keys() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	keys := make([]string, 0, len(w.data))
	for k := range w.data {
		keys = append(keys, k)
	}
	return keys
}

// HasKey checks if a key exists in the data map
func (w *WorkflowData) HasKey(key string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	_, exists := w.data[key]
	return exists
}

// ResetArena resets the arena and clears all data
func (w *WorkflowData) ResetArena() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.arena != nil {
		w.arena.Reset()
	}
	if w.stringPool != nil {
		w.stringPool.Reset()
	}

	// Clear all maps
	w.data = make(map[string]interface{})
	w.nodeStatus = make(map[string]NodeStatus)
	w.outputs = make(map[string]interface{})
}

// GetArenaStats returns statistics about the arena and string pool if they are being used
func (w *WorkflowData) GetArenaStats() map[string]map[string]int64 {
	stats := make(map[string]map[string]int64)

	if w.arena != nil {
		w.mu.RLock()
		defer w.mu.RUnlock()

		// Get arena stats
		stats["arena"] = w.arena.Stats()

		// Get string pool stats
		stats["stringPool"] = w.stringPool.Stats()
	}

	return stats
}

// WorkflowDataConfig represents the configuration for workflow data
// ... existing code ...

func cloneMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	dataCopy := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			dataCopy[k] = cloneMap(val)
		case []interface{}:
			sliceCopy := make([]interface{}, len(val))
			for i, item := range val {
				if mapItem, ok := item.(map[string]interface{}); ok {
					sliceCopy[i] = cloneMap(mapItem)
				} else {
					sliceCopy[i] = item
				}
			}
			dataCopy[k] = sliceCopy
		default:
			dataCopy[k] = v
		}
	}
	return dataCopy
}
