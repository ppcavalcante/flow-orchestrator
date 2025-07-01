package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	fb "github.com/ppcavalcante/flow-orchestrator/pkg/workflow/fb/workflow"
)

// WorkflowStore defines the interface for persisting workflow state.
// Implementations can store workflow data in memory, files, databases, etc.
type WorkflowStore interface {
	// Save stores the workflow data.
	// Returns an error if the save operation fails.
	Save(data *WorkflowData) error

	// Load retrieves workflow data by ID.
	// Returns the workflow data and an error if the load operation fails.
	Load(workflowID string) (*WorkflowData, error)

	// ListWorkflows returns all workflow IDs.
	// Returns an error if the list operation fails.
	ListWorkflows() ([]string, error)

	// Delete removes a workflow.
	// Returns an error if the delete operation fails.
	Delete(workflowID string) error
}

// JSONFileStore is a file-based implementation of WorkflowStore that uses JSON serialization.
// This is a temporary implementation until we fully integrate FlatBuffers
//
// Deprecated: Use FlatBuffersStore for better performance
type JSONFileStore struct {
	baseDir string
	mu      sync.RWMutex
}

// NewJSONFileStore creates a new JSON file-based workflow store.
// baseDir is the directory where workflow data will be stored.
// Returns an error if the directory cannot be created or accessed.
func NewJSONFileStore(baseDir string) (*JSONFileStore, error) {
	// Create the directory if it doesn't exist
	err := os.MkdirAll(baseDir, 0750)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	return &JSONFileStore{
		baseDir: baseDir,
	}, nil
}

// Save stores the workflow data as JSON
func (s *JSONFileStore) Save(data *WorkflowData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if data == nil {
		return errors.New("cannot save nil workflow data")
	}

	// Get workflow ID
	workflowID := data.GetWorkflowID()
	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	// Create snapshot
	snapshotData, err := data.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Unmarshal to map for adding timestamp
	var snapshot map[string]interface{}
	if err := json.Unmarshal(snapshotData, &snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	// Add timestamp
	snapshot["__timestamp"] = time.Now().UnixNano() / int64(time.Millisecond)

	// Marshal to JSON
	jsonData, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal workflow data: %w", err)
	}

	// Write to file
	filePath := filepath.Join(s.baseDir, workflowID+".json")
	err = os.WriteFile(filePath, jsonData, 0600)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// Load retrieves workflow data from JSON
func (s *JSONFileStore) Load(workflowID string) (*WorkflowData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if workflowID == "" {
		return nil, errors.New("workflow ID cannot be empty")
	}

	// Construct file path
	filePath := filepath.Join(s.baseDir, workflowID+".json")

	// Read the file
	// nolint:gosec // This is an internal function with controlled file paths
	jsonData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read workflow file: %w", err)
	}

	// Create new workflow data
	data := NewWorkflowData(workflowID)

	// Load from snapshot
	if err := data.LoadSnapshot(jsonData); err != nil {
		return nil, fmt.Errorf("failed to load snapshot: %w", err)
	}

	return data, nil
}

// ListWorkflows returns all workflow IDs
func (s *JSONFileStore) ListWorkflows() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get JSON files in directory
	pattern := filepath.Join(s.baseDir, "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list workflows: %w", err)
	}

	// Extract workflow IDs from filenames
	workflowIDs := make([]string, 0, len(matches))
	for _, match := range matches {
		filename := filepath.Base(match)
		workflowID := filename[:len(filename)-5] // Remove ".json"
		workflowIDs = append(workflowIDs, workflowID)
	}

	return workflowIDs, nil
}

// Delete removes a workflow
func (s *JSONFileStore) Delete(workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	// Delete file
	filePath := filepath.Join(s.baseDir, workflowID+".json")
	err := os.Remove(filePath)
	if err != nil {
		return fmt.Errorf("failed to delete workflow: %w", err)
	}

	return nil
}

// MigrateToFlatBuffers converts all JSON workflow files to FlatBuffer format
// and returns a new FlatBuffersStore pointing to the same directory.
//
// This is a convenience method to help migrate existing JSON stores to FlatBuffers.
// The original JSON files are left intact unless cleanupJSON is set to true.
func (s *JSONFileStore) MigrateToFlatBuffers(cleanupJSON bool) (*FlatBuffersStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Create a new FlatBuffersStore with the same base directory
	fbStore, err := NewFlatBuffersStore(s.baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create FlatBuffers store: %w", err)
	}

	// Get a list of all JSON files
	pattern := filepath.Join(s.baseDir, "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list JSON files: %w", err)
	}

	// Convert each file
	for _, jsonPath := range matches {
		// Extract workflow ID from filename
		filename := filepath.Base(jsonPath)
		workflowID := filename[:len(filename)-5] // Remove ".json"

		// Load the workflow data from JSON
		data, err := s.Load(workflowID)
		if err != nil {
			return nil, fmt.Errorf("failed to load workflow %s: %w", workflowID, err)
		}

		// Save it in FlatBuffers format
		err = fbStore.Save(data)
		if err != nil {
			return nil, fmt.Errorf("failed to save workflow %s in FlatBuffers format: %w", workflowID, err)
		}

		// Optionally remove the JSON file
		if cleanupJSON {
			err = os.Remove(jsonPath)
			if err != nil {
				return nil, fmt.Errorf("failed to cleanup JSON file %s: %w", jsonPath, err)
			}
		}
	}

	return fbStore, nil
}

// FlatBuffersStore is a file-based implementation of WorkflowStore that uses FlatBuffers serialization.
// It provides better performance than JSONFileStore for large workflows.
type FlatBuffersStore struct {
	baseDir string
	mu      sync.RWMutex
}

// NewFlatBuffersStore creates a new FlatBuffers-based workflow store.
// baseDir is the directory where workflow data will be stored.
// Returns an error if the directory cannot be created or accessed.
func NewFlatBuffersStore(baseDir string) (*FlatBuffersStore, error) {
	// Create the directory if it doesn't exist
	err := os.MkdirAll(baseDir, 0750)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	return &FlatBuffersStore{
		baseDir: baseDir,
	}, nil
}

// Save stores the workflow data using FlatBuffers
func (s *FlatBuffersStore) Save(data *WorkflowData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if data == nil {
		return errors.New("cannot save nil workflow data")
	}

	// Get workflow ID
	workflowID := data.GetWorkflowID()
	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	// Create FlatBuffer builder
	builder := flatbuffers.NewBuilder(1024)

	// Create the workflow ID string
	fbWorkflowID := builder.CreateString(workflowID)

	// Create string data vector
	stringDataOffsets := make([]flatbuffers.UOffsetT, 0)
	data.ForEach(func(k string, value interface{}) {
		if v, ok := value.(string); ok {
			// Create key and value strings
			fbKey := builder.CreateString(k)
			fbValue := builder.CreateString(v)

			// Create KeyValueString table
			fb.KeyValueStringStart(builder)
			fb.KeyValueStringAddKey(builder, fbKey)
			fb.KeyValueStringAddValue(builder, fbValue)
			stringDataOffsets = append(stringDataOffsets, fb.KeyValueStringEnd(builder))
		}
	})

	// Create StringData vector
	var stringDataVector flatbuffers.UOffsetT
	if len(stringDataOffsets) > 0 {
		fb.WorkflowStateStartStringDataVector(builder, len(stringDataOffsets))
		for i := len(stringDataOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(stringDataOffsets[i])
		}
		stringDataVector = builder.EndVector(len(stringDataOffsets))
	}

	// Create node status vector
	nodeStatusOffsets := make([]flatbuffers.UOffsetT, 0)
	data.ForEachNodeStatus(func(nodeName string, status NodeStatus) {
		// Create node name string
		fbNodeName := builder.CreateString(nodeName)

		// Create NodeStatusEntry table
		fb.NodeStatusEntryStart(builder)
		fb.NodeStatusEntryAddNodeName(builder, fbNodeName)
		fb.NodeStatusEntryAddStatus(builder, nodeStatusToFB(status))
		nodeStatusOffsets = append(nodeStatusOffsets, fb.NodeStatusEntryEnd(builder))
	})

	// Create NodeStatuses vector
	var statusesVector flatbuffers.UOffsetT
	if len(nodeStatusOffsets) > 0 {
		fb.WorkflowStateStartNodeStatusesVector(builder, len(nodeStatusOffsets))
		for i := len(nodeStatusOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(nodeStatusOffsets[i])
		}
		statusesVector = builder.EndVector(len(nodeStatusOffsets))
	}

	// Create outputs vector
	outputOffsets := make([]flatbuffers.UOffsetT, 0)
	data.ForEachOutput(func(nodeName string, output interface{}) {
		// Convert output to JSON string
		var outputStr string
		if v, ok := output.(string); ok {
			outputStr = v
		} else {
			jsonBytes, err := json.Marshal(output)
			if err == nil {
				outputStr = string(jsonBytes)
			} else {
				outputStr = fmt.Sprintf("%v", output)
			}
		}

		// Create node name string and output string
		fbNodeName := builder.CreateString(nodeName)
		fbOutput := builder.CreateString(outputStr)

		// Create NodeOutputEntry table
		fb.NodeOutputEntryStart(builder)
		fb.NodeOutputEntryAddNodeName(builder, fbNodeName)
		fb.NodeOutputEntryAddOutput(builder, fbOutput)
		outputOffsets = append(outputOffsets, fb.NodeOutputEntryEnd(builder))
	})

	// Create NodeOutputs vector
	var outputsVector flatbuffers.UOffsetT
	if len(outputOffsets) > 0 {
		fb.WorkflowStateStartNodeOutputsVector(builder, len(outputOffsets))
		for i := len(outputOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(outputOffsets[i])
		}
		outputsVector = builder.EndVector(len(outputOffsets))
	}

	// Create WorkflowState table
	fb.WorkflowStateStart(builder)
	fb.WorkflowStateAddWorkflowId(builder, fbWorkflowID)

	if len(stringDataOffsets) > 0 {
		fb.WorkflowStateAddStringData(builder, stringDataVector)
	}

	if len(nodeStatusOffsets) > 0 {
		fb.WorkflowStateAddNodeStatuses(builder, statusesVector)
	}

	if len(outputOffsets) > 0 {
		fb.WorkflowStateAddNodeOutputs(builder, outputsVector)
	}

	workflowState := fb.WorkflowStateEnd(builder)

	// Finish the buffer
	builder.Finish(workflowState)

	// Get the finished buffer
	buf := builder.FinishedBytes()

	// Write to file
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	err := os.WriteFile(filePath, buf, 0600)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// Load retrieves workflow data using FlatBuffers
func (s *FlatBuffersStore) Load(workflowID string) (*WorkflowData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if workflowID == "" {
		return nil, errors.New("workflow ID cannot be empty")
	}

	// Construct file path
	filePath := filepath.Join(s.baseDir, workflowID+".fb")

	// Read the file
	// nolint:gosec // This is an internal function with controlled file paths
	buf, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read workflow file: %w", err)
	}

	// Get the root
	fbState := fb.GetRootAsWorkflowState(buf, 0)

	// Create new workflow data
	data := NewWorkflowData(workflowID)

	// Load string data
	for i := 0; i < fbState.StringDataLength(); i++ {
		var kv fb.KeyValueString
		if fbState.StringData(&kv, i) {
			key := string(kv.Key())
			value := string(kv.Value())
			data.Set(key, value)
		}
	}

	// Load node statuses
	for i := 0; i < fbState.NodeStatusesLength(); i++ {
		var entry fb.NodeStatusEntry
		if fbState.NodeStatuses(&entry, i) {
			nodeName := string(entry.NodeName())

			// Convert fb.NodeStatus to our NodeStatus
			var status NodeStatus
			switch entry.Status() {
			case fb.NodeStatusPending:
				status = Pending
			case fb.NodeStatusRunning:
				status = Running
			case fb.NodeStatusCompleted:
				status = Completed
			case fb.NodeStatusFailed:
				status = Failed
			case fb.NodeStatusSkipped:
				status = Skipped
			default:
				status = Pending
			}

			data.SetNodeStatus(nodeName, status)
		}
	}

	// Load node outputs
	for i := 0; i < fbState.NodeOutputsLength(); i++ {
		var entry fb.NodeOutputEntry
		if fbState.NodeOutputs(&entry, i) {
			nodeName := string(entry.NodeName())
			output := string(entry.Output())
			data.SetOutput(nodeName, output)
		}
	}

	return data, nil
}

// ListWorkflows returns all workflow IDs from FlatBuffers files
func (s *FlatBuffersStore) ListWorkflows() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get FlatBuffer files in directory
	pattern := filepath.Join(s.baseDir, "*.fb")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list workflows: %w", err)
	}

	// Extract workflow IDs from filenames
	workflowIDs := make([]string, 0, len(matches))
	for _, match := range matches {
		filename := filepath.Base(match)
		workflowID := filename[:len(filename)-3] // Remove ".fb"
		workflowIDs = append(workflowIDs, workflowID)
	}

	return workflowIDs, nil
}

// Delete removes a workflow stored with FlatBuffers
func (s *FlatBuffersStore) Delete(workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	// Delete file
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	err := os.Remove(filePath)
	if err != nil {
		return fmt.Errorf("failed to delete workflow: %w", err)
	}

	return nil
}

// statusToFBStatus converts our NodeStatus to fb.NodeStatus
// nolint:unused // This function is kept for future use
func statusToFBStatus(status NodeStatus) fb.NodeStatus {
	return StatusToFBStatus(status)
}

// fbStatusToNodeStatus converts fb.NodeStatus to NodeStatus
// nolint:unused // This function is kept for future use
func fbStatusToNodeStatus(status byte) NodeStatus {
	switch status {
	case byte(fb.NodeStatusPending):
		return Pending
	case byte(fb.NodeStatusRunning):
		return Running
	case byte(fb.NodeStatusCompleted):
		return Completed
	case byte(fb.NodeStatusFailed):
		return Failed
	case byte(fb.NodeStatusSkipped):
		return Skipped
	default:
		return Pending
	}
}

// InMemoryStore is an in-memory implementation of WorkflowStore.
// It's useful for testing and workflows that don't need persistence.
type InMemoryStore struct {
	data map[string]*WorkflowData
	mu   sync.RWMutex
}

// NewInMemoryStore creates a new in-memory workflow store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		data: make(map[string]*WorkflowData),
	}
}

// Save stores the workflow data in memory
func (s *InMemoryStore) Save(data *WorkflowData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if data == nil {
		return errors.New("cannot save nil workflow data")
	}

	workflowID := data.GetWorkflowID()
	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	// Clone the data to avoid external modification
	s.data[workflowID] = data.Clone()
	return nil
}

// Load retrieves workflow data from memory
func (s *InMemoryStore) Load(workflowID string) (*WorkflowData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if workflowID == "" {
		return nil, errors.New("workflow ID cannot be empty")
	}

	data, ok := s.data[workflowID]
	if !ok {
		return nil, fmt.Errorf("workflow not found: %s", workflowID)
	}

	// Return a clone to avoid external modification
	return data.Clone(), nil
}

// ListWorkflows returns all workflow IDs in memory
func (s *InMemoryStore) ListWorkflows() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	workflowIDs := make([]string, 0, len(s.data))
	for id := range s.data {
		workflowIDs = append(workflowIDs, id)
	}

	return workflowIDs, nil
}

// Delete removes a workflow from memory
func (s *InMemoryStore) Delete(workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workflowID == "" {
		return errors.New("workflow ID cannot be empty")
	}

	delete(s.data, workflowID)
	return nil
}

// nodeStatusToFB converts our NodeStatus to fb.NodeStatus
func nodeStatusToFB(status NodeStatus) fb.NodeStatus {
	return StatusToFBStatus(status)
}

// StatusToFBStatus converts NodeStatus to fb.NodeStatus
// This function is exported for use by other files in the package
func StatusToFBStatus(status NodeStatus) fb.NodeStatus {
	switch status {
	case Pending:
		return fb.NodeStatusPending
	case Running:
		return fb.NodeStatusRunning
	case Completed:
		return fb.NodeStatusCompleted
	case Failed:
		return fb.NodeStatusFailed
	case Skipped:
		return fb.NodeStatusSkipped
	case NotStarted:
		return fb.NodeStatusPending // Map NotStarted to Pending for FB compatibility
	default:
		return fb.NodeStatusPending
	}
}
