package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	fb "github.com/ppcavalcante/flow-orchestrator/internal/workflow/fb/workflow"
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

// Checkpointer is an OPTIONAL interface a WorkflowStore MAY implement to support
// durable mid-run checkpointing (M9 crash-resume). It is additive: a Store that
// does not implement Checkpointer keeps the prior "save at run boundaries only"
// behavior with zero change. When a Store DOES implement it, Workflow.Execute
// wires the executor to flush the workflow's state at each completed level
// barrier, so a process crash mid-run can resume from the last completed level
// (skipping already-completed nodes) instead of restarting from scratch.
//
// SaveCheckpoint must persist data ATOMICALLY and durably: a crash during the
// call must leave either the prior checkpoint or the new one fully intact, never
// a torn mix. For the file stores this is the temp+fsync+rename of
// writeFileAtomic; for InMemoryStore it is the lock-guarded clone. Because the
// snapshot a Store already writes carries the full per-node {status, output}, a
// checkpoint is simply an atomic whole-snapshot Save performed mid-run — no new
// serialization format is involved.
type Checkpointer interface {
	// SaveCheckpoint atomically and durably persists the current workflow state.
	SaveCheckpoint(data *WorkflowData) error
}

// validateWorkflowID rejects any workflow ID that is not a single safe path
// segment, preventing path traversal when the ID is joined onto a store's
// baseDir. An ID is rejected if it is empty, contains a path separator, is not
// local (per filepath.IsLocal — catches "..", absolute paths, and volume names),
// or does not survive a filepath.Base round-trip. Callers that build a filesystem
// path from a caller-supplied ID must call this first.
func validateWorkflowID(workflowID string) error {
	if workflowID == "" {
		return fmt.Errorf("%w: workflow ID cannot be empty", ErrValidation)
	}
	if strings.ContainsRune(workflowID, '/') ||
		strings.ContainsRune(workflowID, os.PathSeparator) ||
		!filepath.IsLocal(workflowID) ||
		filepath.Base(workflowID) != workflowID {
		return fmt.Errorf("%w: invalid workflow ID %q: must be a single path segment with no separators or traversal", ErrValidation, workflowID)
	}
	return nil
}

// atomicTempFile is the subset of *os.File that writeFileAtomic uses. It exists
// so a test can inject a failure on the Write / Sync / Chmod / Close steps — the
// torn-write-guard error branches that are otherwise impossible to trigger with a
// real on-disk file (a write to a freshly-created temp file does not fail on
// demand). *os.File satisfies it directly; production never substitutes anything.
type atomicTempFile interface {
	Write(p []byte) (int, error)
	Sync() error
	Chmod(mode os.FileMode) error
	Close() error
	Name() string
}

// createTempFile is the temp-file-creation seam used by writeFileAtomic (default
// os.CreateTemp). It is an unexported test seam — the same discipline as
// openForRead — adding no public surface; tests swap it for a wrapper that returns
// a real temp file failing on a chosen method. Production never reassigns it.
var createTempFile = func(dir, pattern string) (atomicTempFile, error) {
	return os.CreateTemp(dir, pattern)
}

// writeFileAtomic writes data to path atomically: it writes to a temp file in
// the SAME directory, fsyncs it, then renames it over path. A crash (or an error)
// at any point leaves either the prior file fully intact or the new file fully
// written — never a torn/partial file. This is the torn-write guard the durable
// checkpoint path (M9) depends on, and it also hardens the existing Save paths.
//
// The temp file is created in path's directory (not the system temp dir) so the
// final os.Rename is a same-filesystem rename, which POSIX guarantees is atomic;
// a cross-filesystem rename would fall back to a non-atomic copy. On any error
// the temp file is removed so a failed write leaves no leftover. The parent
// directory is fsynced after the rename so the rename itself is durable across a
// power loss (on POSIX the rename is atomic but its persistence is only
// guaranteed after the directory entry is synced); a dir-sync failure is not
// fatal — the rename already succeeded and the data file was fsynced.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)

	tmp, err := createTempFile(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// On any failure after creation, remove the temp file so a failed write
	// never leaves a leftover next to the real file. Both calls are best-effort
	// cleanup on the error path — there is nothing useful to do with their errors
	// (the real error is already being returned), so they are intentionally
	// ignored. nolint:errcheck // best-effort cleanup on the error path
	defer func() {
		if err != nil {
			tmp.Close()        //nolint:errcheck,gosec // may already be closed; cleanup only
			os.Remove(tmpName) //nolint:errcheck,gosec // best-effort temp cleanup
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	// fsync the data to stable storage BEFORE the rename, so the renamed file is
	// guaranteed complete on disk (not just in the page cache).
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Atomic replace (same-filesystem rename).
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	// Best-effort durability of the rename itself: fsync the parent directory.
	// The rename already succeeded, so a dir-sync error does not corrupt anything
	// and must not fail the write — the calls are intentionally best-effort.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // controlled internal directory path
		d.Sync()  //nolint:errcheck,gosec // best-effort directory fsync; rename already succeeded
		d.Close() //nolint:errcheck,gosec // best-effort
	}

	return nil
}

// JSONFileStore is a file-based implementation of WorkflowStore that uses JSON
// serialization. It is a first-class, supported store: JSON is the human-readable,
// recovery-friendly persistence format. Use FlatBuffersStore instead when you want
// the faster binary format; the two are interchangeable behind the WorkflowStore
// interface. Both Load paths are bounded (io.LimitReader) against oversized input.
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
		return fmt.Errorf("%w: cannot save nil workflow data", ErrValidation)
	}

	// Get workflow ID
	workflowID := data.GetWorkflowID()
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}

	// Create snapshot
	snapshotData, err := data.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Unmarshal to map for adding timestamp. UseNumber so integer values survive
	// the decode/re-encode round-trip exactly — decoding into interface{} would
	// turn numbers into float64 and silently corrupt int64 magnitudes above 2^53
	// (json.Number re-marshals back to the original literal verbatim).
	var snapshot map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(snapshotData))
	dec.UseNumber()
	if err := dec.Decode(&snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	// Add timestamp
	snapshot["__timestamp"] = time.Now().UnixNano() / int64(time.Millisecond)

	// Marshal to JSON
	jsonData, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal workflow data: %w", err)
	}

	// Write to file atomically (temp + fsync + rename) so a crash mid-write
	// cannot leave a torn/partial file (the durable-checkpoint torn-write guard).
	filePath := filepath.Join(s.baseDir, workflowID+".json")
	if err := writeFileAtomic(filePath, jsonData, 0600); err != nil {
		return newIOError("write", workflowID, err)
	}

	return nil
}

// SaveCheckpoint persists the current workflow state mid-run (M9 crash-resume).
// A checkpoint is an atomic whole-snapshot Save: JSONFileStore.Save already
// writes atomically (writeFileAtomic), so the durability contract is satisfied
// by delegating to it. This makes *JSONFileStore implement Checkpointer.
func (s *JSONFileStore) SaveCheckpoint(data *WorkflowData) error {
	return s.Save(data)
}

// Load retrieves workflow data from JSON
func (s *JSONFileStore) Load(workflowID string) (*WorkflowData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}

	// Construct file path
	filePath := filepath.Join(s.baseDir, workflowID+".json")

	// Bounds guard: cap input size ATOMICALLY with the read, symmetric with the
	// FlatBuffers Load path. Reading through io.LimitReader(cap+1) eliminates any
	// os.Stat -> os.ReadFile TOCTOU and bounds memory regardless of on-disk size;
	// cap+1 lets us distinguish "exactly at cap" (accepted) from "over cap"
	// (rejected). openForRead is the same test seam used by FB Load (default
	// os.Open). A missing file surfaces as ErrNotFound.
	f, err := openForRead(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowID)
		}
		return nil, newIOError("read", workflowID, err)
	}
	defer func() {
		// Surface a Close error only if Load was otherwise succeeding; a failed
		// read/parse error takes precedence (errcheck check-blank requires the
		// Close error be consumed).
		if cerr := f.Close(); cerr != nil && err == nil {
			err = newIOError("read", workflowID, cerr)
		}
	}()

	jsonData, err := io.ReadAll(io.LimitReader(f, defaultMaxFileSize+1))
	if err != nil {
		return nil, newIOError("read", workflowID, err)
	}
	if int64(len(jsonData)) > defaultMaxFileSize {
		return nil, fmt.Errorf("%w: file exceeds max size", ErrCorruptData)
	}

	// Create new workflow data
	data := NewWorkflowData(workflowID)

	// Load from snapshot
	if err := data.LoadSnapshot(jsonData); err != nil {
		// A decode failure (or element-count overflow) means the persisted JSON
		// is malformed/abusive. Keep the boundary message generic (no path / raw
		// detail leak); the underlying error stays reachable via errors.Unwrap.
		return nil, fmt.Errorf("%w: malformed JSON workflow data: %w", ErrCorruptData, err)
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

	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}

	// Delete file
	filePath := filepath.Join(s.baseDir, workflowID+".json")
	err := os.Remove(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, workflowID)
		}
		return newIOError("delete", workflowID, err)
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

	// Create a new FlatBuffersStore with the same base directory
	fbStore, err := NewFlatBuffersStore(s.baseDir)
	if err != nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("failed to create FlatBuffers store: %w", err)
	}

	// Get a list of all JSON files
	pattern := filepath.Join(s.baseDir, "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("failed to list JSON files: %w", err)
	}

	// Collect paths to delete and convert each file
	var toDelete []string
	for _, jsonPath := range matches {
		// Extract workflow ID from filename
		filename := filepath.Base(jsonPath)
		workflowID := filename[:len(filename)-5] // Remove ".json"

		// Load the workflow data from JSON (note: Load also takes RLock, so
		// we read the file directly here to avoid double-locking). Use the same
		// bounded read (io.LimitReader(cap+1)) as Load so a migration cannot be
		// driven to unbounded allocation by an oversized .json file.
		filePath := filepath.Join(s.baseDir, workflowID+".json")
		jsonData, err := readBoundedFile(filePath)
		if err != nil {
			s.mu.RUnlock()
			return nil, fmt.Errorf("failed to read workflow %s: %w", workflowID, err)
		}

		data := NewWorkflowData(workflowID)
		if err := data.LoadSnapshot(jsonData); err != nil {
			s.mu.RUnlock()
			return nil, fmt.Errorf("failed to load workflow %s: %w", workflowID, err)
		}

		// Save it in FlatBuffers format
		err = fbStore.Save(data)
		if err != nil {
			s.mu.RUnlock()
			return nil, fmt.Errorf("failed to save workflow %s in FlatBuffers format: %w", workflowID, err)
		}

		if cleanupJSON {
			toDelete = append(toDelete, jsonPath)
		}
	}

	// Release the read lock before deleting files
	s.mu.RUnlock()

	// Delete JSON files outside the lock
	for _, jsonPath := range toDelete {
		if err := os.Remove(jsonPath); err != nil {
			return nil, fmt.Errorf("failed to cleanup JSON file %s: %w", jsonPath, err)
		}
	}

	return fbStore, nil
}

// Layered bounds-guard limits for FlatBuffersStore.Load. The Go flatbuffers
// runtime ships no Verifier, so these hand-rolled caps reject malformed,
// truncated, oversized, or absurd-count buffers before they reach the
// (unbounded) accessor offset-deref. They are internal defaults — no public
// surface — bounding the availability ceiling DEC-M1-trust-contract declares:
// Load won't panic and won't unbounded-allocate. They do NOT make Load a
// structural verifier (a well-formed-but-forged buffer still loads).

// defaultMaxFileSize caps the bytes Load reads from a .fb (enforced atomically
// via io.LimitReader(cap+1), not a separate os.Stat — see Load). 64 MiB is far
// above any realistic snapshot yet bounds a single Load's memory to a fixed
// ceiling. A var (not const) so tests can shrink it to assert the read bound on
// the live Load path without materializing a 64 MiB file; production never
// reassigns it.
var defaultMaxFileSize int64 = 64 << 20 // 64 MiB

// defaultMaxElements caps each FlatBuffers vector length (the six *Length()
// counts) before the load loops allocate/iterate, stopping a tiny header that
// claims billions of elements.
const defaultMaxElements int = 1 << 20 // 1,048,576 entries per vector

// openForRead is the file-open seam used by Load (default os.Open). Tests swap
// it for a byte-counting wrapper to assert the bytes consumed from the fd are
// bounded by cap+1 on the live path. Production never reassigns it.
// nolint:gosec // controlled internal file paths
var openForRead = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// readBoundedFile reads an entire file through io.LimitReader(cap+1) — the same
// bounded-read discipline as JSONFileStore.Load / FlatBuffersStore.Load — so
// WorkflowData.LoadFromJSON and JSONFileStore.MigrateToFlatBuffers share one
// symmetric size bound. It bounds memory regardless of on-disk size and rejects
// over-cap input as ErrCorruptData (cap+1 distinguishes at-cap from over-cap).
// openForRead is the same test seam used by Load; the open error (incl.
// fs.ErrNotExist) is returned verbatim for the caller to classify/wrap.
func readBoundedFile(path string) (data []byte, err error) {
	f, err := openForRead(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	data, err = io.ReadAll(io.LimitReader(f, defaultMaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > defaultMaxFileSize {
		return nil, fmt.Errorf("%w: file exceeds max size", ErrCorruptData)
	}
	return data, nil
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
		return fmt.Errorf("%w: cannot save nil workflow data", ErrValidation)
	}

	// Get workflow ID
	workflowID := data.GetWorkflowID()
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}

	// Create FlatBuffer builder
	builder := flatbuffers.NewBuilder(1024)

	// Create the workflow ID string
	fbWorkflowID := builder.CreateString(workflowID)

	// Create typed data vectors — use appropriate typed vector for each value type
	stringDataOffsets := make([]flatbuffers.UOffsetT, 0)
	intDataOffsets := make([]flatbuffers.UOffsetT, 0)
	boolDataOffsets := make([]flatbuffers.UOffsetT, 0)
	doubleDataOffsets := make([]flatbuffers.UOffsetT, 0)

	data.ForEach(func(k string, value interface{}) {
		switch v := value.(type) {
		case int:
			// M2: write the full int64 magnitude to value_long (no clamp). The
			// legacy value:int field is left at its default; Load reads value_long
			// first and only falls back to value for pre-M2 (M1-format) buffers.
			fbKey := builder.CreateString(k)
			fb.KeyValueIntStart(builder)
			fb.KeyValueIntAddKey(builder, fbKey)
			fb.KeyValueIntAddValueLong(builder, int64(v))
			intDataOffsets = append(intDataOffsets, fb.KeyValueIntEnd(builder))
		case int32:
			fbKey := builder.CreateString(k)
			fb.KeyValueIntStart(builder)
			fb.KeyValueIntAddKey(builder, fbKey)
			fb.KeyValueIntAddValueLong(builder, int64(v))
			intDataOffsets = append(intDataOffsets, fb.KeyValueIntEnd(builder))
		case int64:
			fbKey := builder.CreateString(k)
			fb.KeyValueIntStart(builder)
			fb.KeyValueIntAddKey(builder, fbKey)
			fb.KeyValueIntAddValueLong(builder, v)
			intDataOffsets = append(intDataOffsets, fb.KeyValueIntEnd(builder))
		case bool:
			fbKey := builder.CreateString(k)
			fb.KeyValueBoolStart(builder)
			fb.KeyValueBoolAddKey(builder, fbKey)
			fb.KeyValueBoolAddValue(builder, v)
			boolDataOffsets = append(boolDataOffsets, fb.KeyValueBoolEnd(builder))
		case float64:
			fbKey := builder.CreateString(k)
			fb.KeyValueDoubleStart(builder)
			fb.KeyValueDoubleAddKey(builder, fbKey)
			fb.KeyValueDoubleAddValue(builder, v)
			doubleDataOffsets = append(doubleDataOffsets, fb.KeyValueDoubleEnd(builder))
		case float32:
			fbKey := builder.CreateString(k)
			fb.KeyValueDoubleStart(builder)
			fb.KeyValueDoubleAddKey(builder, fbKey)
			fb.KeyValueDoubleAddValue(builder, float64(v))
			doubleDataOffsets = append(doubleDataOffsets, fb.KeyValueDoubleEnd(builder))
		case string:
			fbKey := builder.CreateString(k)
			fbValue := builder.CreateString(v)
			fb.KeyValueStringStart(builder)
			fb.KeyValueStringAddKey(builder, fbKey)
			fb.KeyValueStringAddValue(builder, fbValue)
			stringDataOffsets = append(stringDataOffsets, fb.KeyValueStringEnd(builder))
		default:
			// Complex types: fall back to JSON string
			jsonBytes, err := json.Marshal(v)
			var strValue string
			if err != nil {
				strValue = fmt.Sprintf("%v", v)
			} else {
				strValue = string(jsonBytes)
			}
			fbKey := builder.CreateString(k)
			fbValue := builder.CreateString(strValue)
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

	// Create IntData vector
	var intDataVector flatbuffers.UOffsetT
	if len(intDataOffsets) > 0 {
		fb.WorkflowStateStartIntDataVector(builder, len(intDataOffsets))
		for i := len(intDataOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(intDataOffsets[i])
		}
		intDataVector = builder.EndVector(len(intDataOffsets))
	}

	// Create BoolData vector
	var boolDataVector flatbuffers.UOffsetT
	if len(boolDataOffsets) > 0 {
		fb.WorkflowStateStartBoolDataVector(builder, len(boolDataOffsets))
		for i := len(boolDataOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(boolDataOffsets[i])
		}
		boolDataVector = builder.EndVector(len(boolDataOffsets))
	}

	// Create DoubleData vector
	var doubleDataVector flatbuffers.UOffsetT
	if len(doubleDataOffsets) > 0 {
		fb.WorkflowStateStartDoubleDataVector(builder, len(doubleDataOffsets))
		for i := len(doubleDataOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(doubleDataOffsets[i])
		}
		doubleDataVector = builder.EndVector(len(doubleDataOffsets))
	}

	// Create node status vector
	nodeStatusOffsets := make([]flatbuffers.UOffsetT, 0)
	data.ForEachNodeStatus(func(nodeName string, status NodeStatus) {
		// Create node name string
		fbNodeName := builder.CreateString(nodeName)

		// Create NodeStatusEntry table
		fb.NodeStatusEntryStart(builder)
		fb.NodeStatusEntryAddNodeName(builder, fbNodeName)
		fb.NodeStatusEntryAddStatus(builder, statusToFBStatus(status))
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

	if len(intDataOffsets) > 0 {
		fb.WorkflowStateAddIntData(builder, intDataVector)
	}

	if len(boolDataOffsets) > 0 {
		fb.WorkflowStateAddBoolData(builder, boolDataVector)
	}

	if len(doubleDataOffsets) > 0 {
		fb.WorkflowStateAddDoubleData(builder, doubleDataVector)
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

	// Write to file atomically (temp + fsync + rename) so a crash mid-write
	// cannot leave a torn/partial file (the durable-checkpoint torn-write guard).
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	if err := writeFileAtomic(filePath, buf, 0600); err != nil {
		return newIOError("write", workflowID, err)
	}

	return nil
}

// SaveCheckpoint persists the current workflow state mid-run (M9 crash-resume).
// FlatBuffersStore.Save writes atomically (writeFileAtomic), so delegating to it
// satisfies the durability contract. This makes *FlatBuffersStore implement
// Checkpointer.
func (s *FlatBuffersStore) SaveCheckpoint(data *WorkflowData) error {
	return s.Save(data)
}

// Load retrieves workflow data using FlatBuffers
func (s *FlatBuffersStore) Load(workflowID string) (data *WorkflowData, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}

	// Construct file path
	filePath := filepath.Join(s.baseDir, workflowID+".fb")

	// Bounds guard (1/3): cap input size ATOMICALLY with the read. Opening the
	// file once and reading through an io.LimitReader(cap+1) eliminates the
	// os.Stat -> os.ReadFile TOCTOU (M4-SEC-02): a file cannot grow between a
	// size check and the read because there is no separate check — we simply
	// never read more than cap+1 bytes regardless of the on-disk size. Reading
	// cap+1 (one past the limit) lets us distinguish "exactly at cap" (accepted)
	// from "over cap" (rejected). A missing file surfaces as ErrNotFound from
	// os.Open (the single not-exist path).
	// openForRead is a test seam (default os.Open). Tests swap it for a
	// byte-counting wrapper to assert the bytes Load actually consumes from the
	// file descriptor are bounded — the property that discriminates this atomic
	// LimitReader read from a stat-then-ReadFile (which consults size separately).
	f, err := openForRead(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowID)
		}
		return nil, newIOError("read", workflowID, err)
	}
	defer func() {
		// Surface a Close error only if Load was otherwise succeeding — a failed
		// read/parse error takes precedence. errcheck (check-blank: true) requires
		// the Close error be consumed, not blank-assigned.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = newIOError("read", workflowID, cerr)
		}
	}()

	buf, err := io.ReadAll(io.LimitReader(f, defaultMaxFileSize+1))
	if err != nil {
		return nil, newIOError("read", workflowID, err)
	}
	if int64(len(buf)) > defaultMaxFileSize {
		return nil, fmt.Errorf("%w: file exceeds max size", ErrCorruptData)
	}

	// FlatBuffers accessors index into the buffer using offsets read from the
	// buffer itself, with no bounds validation; a malformed, truncated, or
	// version-skewed file makes them panic. The layered bounds guard below
	// (atomic size cap @io.LimitReader, root/offset min-length sanity pre-check,
	// element-count caps) rejects the common malformed shapes deterministically and typed,
	// BEFORE the decode. This recover() is the RESIDUAL backstop behind that
	// guard — for deep-offset cases the cheap pre-walk cannot cover — so Load
	// never crashes the host process. (The Go flatbuffers runtime ships no
	// Verifier; this layered guard is the hardening, not a structural verifier:
	// a well-formed-but-forged buffer can still load in-bounds garbage.)
	defer func() {
		if r := recover(); r != nil {
			data = nil
			// Generic boundary message: do not leak the raw panic internals or
			// path. The category is ErrCorruptData; recovered detail is dropped
			// (a panic value is not an error to wrap, and may contain internals).
			err = fmt.Errorf("%w: malformed FlatBuffers data", ErrCorruptData)
		}
	}()

	// Bounds guard (2/3): root-offset + min-length sanity pre-check. The
	// generated GetRootAsWorkflowState reads a 4-byte root UOffsetT from buf[0:]
	// then derefs at that offset with no validation — a buffer shorter than the
	// offset width, or a root offset pointing past the buffer, is the most common
	// truncation/short-file panic. Reject both deterministically as ErrCorruptData
	// here, BEFORE the decode, rather than relying on the recover() net below.
	// Generic message — no path or buffer internals leak.
	if len(buf) < flatbuffers.SizeUOffsetT {
		return nil, fmt.Errorf("%w: malformed FlatBuffers data", ErrCorruptData)
	}
	if rootOffset := flatbuffers.GetUOffsetT(buf); uint64(rootOffset) >= uint64(len(buf)) {
		return nil, fmt.Errorf("%w: malformed FlatBuffers data", ErrCorruptData)
	}

	// Get the root
	fbState := fb.GetRootAsWorkflowState(buf, 0)

	// Create new workflow data
	data = NewWorkflowData(workflowID)

	// Bounds guard (3/3): element-count caps. Each *Length() is read from the
	// (now root-sanity-checked) buffer; a small header can still claim a vector
	// of billions of elements, driving the loops below into a huge alloc/iterate.
	// Reject any vector length over defaultMaxElements before the loops run.
	// (Hand-rolled — the Go runtime has no Verifier MaxTables to lean on.)
	if fbState.IntDataLength() > defaultMaxElements ||
		fbState.BoolDataLength() > defaultMaxElements ||
		fbState.DoubleDataLength() > defaultMaxElements ||
		fbState.StringDataLength() > defaultMaxElements ||
		fbState.NodeStatusesLength() > defaultMaxElements ||
		fbState.NodeOutputsLength() > defaultMaxElements {
		return nil, fmt.Errorf("%w: element count exceeds max", ErrCorruptData)
	}

	// Load int data. M2 buffers carry the faithful magnitude in value_long;
	// M1-format buffers wrote only value:int, so value_long is absent and its
	// accessor returns the FlatBuffers default (0) — in that case fall back to
	// the legacy value:int. The v==0 fallback is sound, NOT ambiguous: FlatBuffers
	// ELIDES default-valued scalars (PrependInt64Slot skips a value equal to the
	// field default 0), so a genuine M2-stored 0 writes no value_long either — it
	// is indistinguishable on the wire from an absent field, and in BOTH cases the
	// fallback reads the legacy value, which is also 0 (M2 leaves value at default;
	// M1 stored 0). So every path that yields v==0 here is correct. The value is
	// stored as int64 — matching the JSON and InMemory backends, and avoiding the
	// 32-bit truncation the old int(kv.Value()) cast caused.
	for i := 0; i < fbState.IntDataLength(); i++ {
		var kv fb.KeyValueInt
		if fbState.IntData(&kv, i) {
			v := kv.ValueLong()
			if v == 0 {
				v = int64(kv.Value())
			}
			data.Set(string(kv.Key()), v)
		}
	}

	// Load bool data
	for i := 0; i < fbState.BoolDataLength(); i++ {
		var kv fb.KeyValueBool
		if fbState.BoolData(&kv, i) {
			data.Set(string(kv.Key()), kv.Value())
		}
	}

	// Load double data
	for i := 0; i < fbState.DoubleDataLength(); i++ {
		var kv fb.KeyValueDouble
		if fbState.DoubleData(&kv, i) {
			data.Set(string(kv.Key()), kv.Value())
		}
	}

	// Load string data (fallback for strings and complex JSON-serialized types)
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

			// Convert fb.NodeStatus to our NodeStatus via the shared helper
			// (symmetric with Save's statusToFBStatus; was previously inlined here,
			// leaving the helper dead — T3 makes it live, removing the duplication).
			status := fbStatusToNodeStatus(entry.Status())

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

	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}

	// Delete file
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	err := os.Remove(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, workflowID)
		}
		return newIOError("delete", workflowID, err)
	}

	return nil
}

// fbStatusToNodeStatus converts an fb.NodeStatus to our NodeStatus.
// The type-symmetric inverse of statusToFBStatus (which Save uses); called by
// Load. Taking fb.NodeStatus directly avoids a lossy int8->byte conversion.
func fbStatusToNodeStatus(status fb.NodeStatus) NodeStatus {
	switch status {
	case fb.NodeStatusPending:
		return Pending
	case fb.NodeStatusRunning:
		return Running
	case fb.NodeStatusCompleted:
		return Completed
	case fb.NodeStatusFailed:
		return Failed
	case fb.NodeStatusSkipped:
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
		return fmt.Errorf("%w: cannot save nil workflow data", ErrValidation)
	}

	workflowID := data.GetWorkflowID()
	if workflowID == "" {
		return fmt.Errorf("%w: workflow ID cannot be empty", ErrValidation)
	}

	// Clone the data to avoid external modification
	s.data[workflowID] = data.Clone()
	return nil
}

// SaveCheckpoint persists the current workflow state mid-run (M9 crash-resume).
// InMemoryStore is not durable across process death, but it implements
// Checkpointer (delegating to the lock-guarded, cloning Save) so it can drive the
// in-process crash-resume tests and so callers get uniform behavior across
// stores. This makes *InMemoryStore implement Checkpointer.
func (s *InMemoryStore) SaveCheckpoint(data *WorkflowData) error {
	return s.Save(data)
}

// Load retrieves workflow data from memory
func (s *InMemoryStore) Load(workflowID string) (*WorkflowData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if workflowID == "" {
		return nil, fmt.Errorf("%w: workflow ID cannot be empty", ErrValidation)
	}

	data, ok := s.data[workflowID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowID)
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
		return fmt.Errorf("%w: workflow ID cannot be empty", ErrValidation)
	}

	delete(s.data, workflowID)
	return nil
}

// statusToFBStatus converts our NodeStatus to fb.NodeStatus.
func statusToFBStatus(status NodeStatus) fb.NodeStatus {
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
	default:
		return fb.NodeStatusPending
	}
}
