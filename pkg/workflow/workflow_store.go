package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
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
//
// CANONICAL VALUE CONTRACT (AUD-026 / AUD-054). All four bundled stores (InMemory,
// JSONFile, FlatBuffers, SQLite) round-trip a value to the SAME canonical Go form, so
// a workflow tested on one store behaves identically on any other — InMemory is a
// faithful substitute for the durable stores, not an over-faithful one:
//
//	data value  int/int32/int64 -> int64 ; float32/float64 -> float64 ; bool ; string
//	            everything else (a map, a slice, an unsupported scalar kind) reloads as
//	            its canonical JSON STRING, e.g. Set("k", map[string]any{"a":1}) reloads
//	            as the string `{"a":1}`.
//	node output a string stays a string; anything else reloads as its JSON string.
//
// The string collapse of complex values is the HONEST floor of what the durable wire
// formats preserve without a format change: FB/SQLite store complex values as JSON
// strings, and this contract makes InMemory and JSONFile match rather than silently
// over-preserve. A consumer that needs structure back Unmarshal()s the string itself.
// See canonical.go for the transform.
type WorkflowStore interface {
	// Save stores the workflow data.
	// Returns an error if the save operation fails.
	Save(data *WorkflowData) error

	// Load retrieves workflow data by ID.
	// Returns the workflow data and an error if the load operation fails.
	//
	// CONTRACT (AUD-037 / P-06): "no prior state" MUST be signalled by returning
	// ErrNotFound. A (nil, nil) return is ILLEGAL — the run rejects it as ErrCorruptData
	// rather than treat it as fresh, because a silent fresh-start would overwrite real
	// persisted state on the next Save. On success return non-nil data and a nil error.
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
	if err = os.Rename(tmpName, path); err != nil { //nolint:gosec // G703 false positive: callers build path from validated single segments or their own construction input
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
	// maxElements is the element-count ceiling, enforced on BOTH Save and Load — the
	// second axis every Load path checks. Seeded from defaultMaxElements; override
	// with WithJSONMaxElements.
	maxElements int
	// maxFileSize is the ONE ceiling this store enforces, on BOTH Save and Load.
	// Seeded from defaultMaxFileSize at construction; override with
	// WithJSONMaxFileSize. Save and Load reading the same field is what keeps the
	// two sides from drifting apart (HYG-00).
	maxFileSize int64
}

// JSONFileStoreOption configures a JSONFileStore at construction.
type JSONFileStoreOption func(*JSONFileStore)

// WithJSONMaxFileSize sets the size ceiling enforced on both Save and Load.
//
// Raising it is the supported recovery path for a workflow file already on disk
// that exceeds the default ceiling: a Save-side cap alone cannot help state that
// was written before the cap existed. n must be > 0; a non-positive n is ignored
// and the default is retained.
//
// SCOPE — this ceiling also governs the durable SIGNAL MAILBOX, not just the
// workflow snapshot. DeliverSignal refuses an entry above it, and TakeSignals reads
// every entry through it. The mailbox read is all-or-nothing: ONE entry above the
// ceiling fails the read of the WHOLE mailbox for that workflow, so LOWERING this on
// a store whose mailbox already holds larger entries can strand a waiting run.
//
// Every process sharing a baseDir MUST agree on this value. Two processes at
// different ceilings can have one write an entry the other cannot read.
func WithJSONMaxFileSize(n int64) JSONFileStoreOption {
	return func(s *JSONFileStore) {
		if n > 0 {
			s.maxFileSize = clampCeiling(n)
		}
	}
}

// WithJSONMaxElements sets the element-count ceiling enforced on both Save and Load.
//
// Raising it is the supported recovery path for a file already on disk whose largest
// section exceeds the default — the byte ceiling cannot help, because over-count state
// is typically far UNDER the byte limit. n must be > 0; a non-positive n is ignored.
func WithJSONMaxElements(n int) JSONFileStoreOption {
	return func(s *JSONFileStore) {
		if n > 0 {
			s.maxElements = n
		}
	}
}

// NewJSONFileStore creates a new JSON file-based workflow store.
// baseDir is the directory where workflow data will be stored.
// Returns an error if the directory cannot be created or accessed.
// The size ceiling defaults to defaultMaxFileSize; pass WithJSONMaxFileSize to
// change it (needed to read back a file that already exceeds the default).
func NewJSONFileStore(baseDir string, opts ...JSONFileStoreOption) (*JSONFileStore, error) {
	// Create the directory if it doesn't exist
	err := os.MkdirAll(baseDir, 0750) //nolint:gosec // G703 false positive: baseDir is the caller's own store-construction input, not request-derived
	if err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	s := &JSONFileStore{
		baseDir:     baseDir,
		maxFileSize: defaultMaxFileSize,
		maxElements: defaultMaxElements,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
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
	// Nesting-depth axis (F2), BEFORE the decode below rather than beside the two guards
	// further down, because that decode is itself depth-limited and would otherwise reject
	// the state first — with an error in NEITHER error domain. Measured at 264265d: an
	// over-depth Save returned "failed to unmarshal snapshot: invalid character '[' exceeded
	// max depth", for which errors.Is is false for BOTH ErrValidation and ErrCorruptData,
	// while the sibling SaveToJSON returned a clean ErrValidation for identical input. The
	// axis is now closed on all four writers in the same domain rather than on three of four.
	if err := checkJSONDepth(snapshotData, workflowID); err != nil {
		return err
	}

	var snapshot map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(snapshotData))
	dec.UseNumber()
	if err := dec.Decode(&snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	// Add timestamp
	snapshot["__timestamp"] = time.Now().UnixNano() / int64(time.Millisecond)

	// THIS MARSHAL IS DEPTH-COVERED BY THE checkJSONDepth ABOVE, AND THE REASON IS AN
	// ARGUMENT RATHER THAN PROXIMITY. Stated here because a reader counting marshal sites
	// against checkJSONDepth calls will otherwise "find" a gap that does not exist — and
	// because the inverse mistake was already made once on this axis: four checks were
	// read as covering four marshals when they did not pair up (SaveToJSON's check guards
	// its own bytes, not createSnapshot's).
	//
	// The check above ran on snapshotData, the INPUT bytes. What is re-marshaled here is
	// that same document decoded, plus one top-level "__timestamp" scalar. Adding a
	// top-level key cannot deepen a document, so the depth already verified still bounds
	// this output. If anything is ever added to `snapshot` that is NOT a top-level scalar,
	// that argument breaks and this site needs its own check.
	//
	// THE HEADING ABOVE IS THE WEDGE AXIS ONLY, and saying "DEPTH-COVERED" unqualified is
	// the one instance of this phase's axis conflation that SHIPS. The claim is TRUE; the
	// argument given for it is a WEDGE argument that happens to also hold on the crash
	// axis, which is being accidentally right. Both are stated separately below because
	// everywhere else in this package they need separate guards.
	//
	// CRASH AXIS (AF2) — the reason MarshalIndent here cannot overflow the stack: `snapshot`
	// is not a host value, it is the OUTPUT OF A DECODER, and json.Decoder caps nesting at
	// the scanner's limit. Its depth is therefore bounded by construction at ~10^4, far
	// below where json.Marshal exhausts the stack, and it cannot contain a cycle because a
	// decoded document is a tree. That is why there is no checkValueDepth here while
	// createSnapshot, one call earlier, has one.
	//
	// THE PRECONDITION THAT ARGUMENT RESTS ON IS NOT LOCAL, which the wedge argument above
	// is blind to: this function is only reached because a CALLER ALREADY MARSHALLED
	// SUCCESSFULLY to produce snapshotData. Its crash-axis safety is INHERITED FROM ITS
	// CALLER and stated nowhere else. Move the guard, or feed this function bytes from a
	// producer that marshals lazily, and every sentence above still reads as a proof while
	// being false.
	jsonData, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal workflow data: %w", err)
	}

	// Refuse to write state this store could never read back, on BOTH axes Load
	// enforces (HYG-00). Bytes measured on the exact buffer about to be written;
	// elements measured as the largest section, which is exactly what LoadSnapshot
	// caps. A successful Save implies a loadable file on both axes.
	if err := checkWriteSize(int64(len(jsonData)), s.maxFileSize, workflowID); err != nil {
		return err
	}
	if err := checkWriteElements(data.maxSectionCount(), s.maxElements, workflowID); err != nil {
		return err
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
// The returns are NAMED so the deferred Close can actually surface its error. With
// unnamed returns the deferred assignment wrote a local that the already-determined
// return value ignored, silently dropping every Close error despite the comment below
// claiming otherwise. FlatBuffersStore.Load has always had named returns; this matches
// it, which is also why readBoundedFileCapped gets it right.
func (s *JSONFileStore) Load(workflowID string) (data *WorkflowData, err error) {
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
	f, ferr := openForRead(filePath)
	if ferr != nil {
		if errors.Is(ferr, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowID)
		}
		return nil, newIOError("read", workflowID, ferr)
	}
	defer func() {
		// Surface a Close error only if Load was otherwise succeeding; a failed
		// read/parse error takes precedence (errcheck check-blank requires the
		// Close error be consumed). This assignment reaches the CALLER only because
		// the returns above are named.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = newIOError("read", workflowID, cerr)
		}
	}()

	fileCeiling := effectiveFileSize(s.maxFileSize)
	jsonData, rerr := io.ReadAll(io.LimitReader(f, readLimit(fileCeiling)))
	if rerr != nil {
		return nil, newIOError("read", workflowID, rerr)
	}
	if int64(len(jsonData)) > fileCeiling {
		return nil, fmt.Errorf("%w: file exceeds max size", ErrCorruptData)
	}

	// Create new workflow data
	data = NewWorkflowData(workflowID)

	// Load from snapshot
	if err := data.loadSnapshotBounded(jsonData, effectiveElements(s.maxElements)); err != nil {
		// A decode failure (or element-count overflow) means the persisted JSON
		// is malformed/abusive. Keep the boundary message generic (no path / raw
		// detail leak); the underlying error stays reachable via errors.Unwrap.
		return nil, fmt.Errorf("%w: malformed JSON workflow data: %w", ErrCorruptData, err)
	}

	// AUD-015 / P-02: the lookup KEY is authoritative. loadSnapshotInternal set
	// data.ID from the payload's own "id" field; if that disagrees with the key we
	// were asked to Load, the file is a misplaced/copied/forged payload. Returning it
	// would run actions under a mixed identity and, worse, REDIRECT the next
	// Save(data) into the payload's workflow (Save keys on data.GetWorkflowID()).
	// Reject as corruption rather than silently honoring the payload ID.
	if got := data.GetWorkflowID(); got != workflowID {
		return nil, fmt.Errorf("%w: JSON payload workflow ID %q does not match the lookup key %q (misplaced or forged file)",
			ErrCorruptData, got, workflowID)
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

	// Reclaim the durable signal mailbox too (ph37 F2): the <id>.signals/ sibling
	// dir is a separate channel the snapshot Delete would otherwise orphan. Done
	// FIRST + best-effort so it runs even for a mailbox with no snapshot (an early
	// signal delivered to a workflow that never ran/saved).
	//
	// ONE OBJECT IS DELIBERATELY NOT RECLAIMED, and it is a stated exception to this
	// store's "there is no background GC; reclamation is owned by Delete" contract
	// rather than an oversight: the <id>.signals.lock file survives. Unlinking it
	// while another process holds it would hand the next deliverer a fresh inode and
	// re-arm the very race the lock exists to close — and it cannot be removed safely
	// even under the lock, because proving nobody holds it requires holding it. The
	// population and cost are stated ONCE, on signalLockSuffix, and deliberately not
	// restated here — the phrasing has already been wrong once, and this comment is
	// duplicated across both file stores, so a restatement drifts in two places at
	// once. It is invisible to the *.json / *.fb listing
	// globs, so it never surfaces as a phantom workflow, and it is not created at all
	// on non-unix, where the lock is a no-op. Delete does NOT create one for a workflow
	// that never had a delivery — it acquires with create=false and skips when absent,
	// because creating here made Delete mint a permanent artifact for ids that never
	// existed.
	//
	// LOCK HELD ACROSS A CROSS-PROCESS WAIT, stated because the shape deserves a reader's
	// attention: s.mu is held across removeSignalDir, which blocks on flock(LOCK_EX) with
	// no timeout. A delivery in ANOTHER PROCESS holding that flock therefore stalls this
	// process's whole store — every Save/Load/List/Delete queues behind s.mu. It is not a
	// deadlock: the only lock ordering in the package is s.mu -> flock (delivery takes the
	// flock and no s.mu), so no cycle exists, and the kernel releases a flock when its
	// holder dies. The case that can actually stall is a LIVE but stopped holder — a
	// SIGSTOP'd process, or one wedged in a pathological fsync — not a crashed one.
	//nolint:errcheck,gosec // best-effort mailbox reclamation (ph37 F2)
	removeSignalDir(s.baseDir, workflowID)

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

	// Create a new FlatBuffersStore with the same base directory AND the same
	// ceiling. Inheriting it matters in both directions (HYG-00): a store opened
	// with a raised ceiling to rescue an oversized .json must be able to write the
	// converted .fb, and a migration must not silently widen the bound either.
	fbStore, err := NewFlatBuffersStore(s.baseDir,
		WithFlatBuffersMaxFileSize(s.maxFileSize), WithFlatBuffersMaxElements(s.maxElements))
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
		jsonData, err := readBoundedFileCapped(filePath, s.maxFileSize)
		if err != nil {
			s.mu.RUnlock()
			return nil, fmt.Errorf("failed to read workflow %s: %w", workflowID, err)
		}

		data := NewWorkflowData(workflowID)
		if err := data.loadSnapshotBounded(jsonData, effectiveElements(s.maxElements)); err != nil {
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

// checkWriteSize rejects a serialized snapshot that exceeds the ceiling BEFORE it
// reaches the disk (HYG-00). Load has always capped its reads; Save never did, so
// an over-ceiling Save succeeded and the workflow then failed to Load forever —
// the failure surfacing at resume, far from the write that caused it. Refusing the
// write makes the two sides symmetric: a Save that returns nil implies a file the
// same store can read back.
//
// ErrValidation, not ErrCorruptData: nothing is corrupt, the caller handed over
// more state than the configured ceiling allows. The message names the actual size
// AND the ceiling so the operator can size the WithJSONMaxFileSize /
// WithFlatBuffersMaxFileSize override without guessing.
// maxAllowedCeiling is the largest ceiling any option will store. The bounded-read
// idiom is io.LimitReader(ceiling+1) — reading one byte past the ceiling is what
// distinguishes at-cap from over-cap — so a ceiling of exactly math.MaxInt64 would
// overflow that +1 to MinInt64, make LimitReader return EOF immediately, and turn
// EVERY Load into a zero-byte read reported as ErrCorruptData. math.MaxInt64 is the
// natural way to write "no limit", so clamping is what keeps that from being a
// silent, permanent wedge reached through this very API.
const maxAllowedCeiling int64 = math.MaxInt64 - 1

// effectiveFileSize / effectiveElements floor a store's ceiling fields, so a store
// built as a STRUCT LITERAL (bypassing both constructors) does not get a zero ceiling
// that would make checkWriteSize refuse every write and readLimit(0) report every
// non-empty file corrupt. Same defensive shape batchK already uses for a literal store
// (workflow_store_groupcommit.go). Latent today — the constructors are the only
// construction sites — but free to hold.
func effectiveFileSize(n int64) int64 {
	if n <= 0 {
		return defaultMaxFileSize
	}
	return n
}

func effectiveElements(n int) int {
	if n <= 0 {
		return defaultMaxElements
	}
	return n
}

// clampCeiling keeps a caller-supplied ceiling inside the range where ceiling+1 is
// still representable. Shared by every With*MaxFileSize option.
func clampCeiling(n int64) int64 {
	if n > maxAllowedCeiling {
		return maxAllowedCeiling
	}
	return n
}

// readLimit returns the io.LimitReader bound for a ceiling (ceiling+1), saturating
// instead of overflowing. Defence in depth behind clampCeiling: the options clamp
// what they store, and this clamps at the point of use, so no internal caller can
// reintroduce the overflow by passing a raw ceiling.
func readLimit(ceiling int64) int64 {
	if ceiling == math.MaxInt64 {
		return math.MaxInt64
	}
	return ceiling + 1
}

func checkWriteSize(n, ceiling int64, workflowID string) error {
	ceiling = effectiveFileSize(ceiling)
	if n > ceiling {
		return fmt.Errorf("%w: workflow %q state is %d bytes, exceeds the %d-byte max file size",
			ErrValidation, workflowID, n, ceiling)
	}
	return nil
}

// defaultMaxElements caps each FlatBuffers vector length (the six *Length()
// counts) before the load loops allocate/iterate, stopping a tiny header that
// claims billions of elements.
// A var (not const) so tests can shrink it to assert the element bound on the live
// paths without materializing ~1M entries — the same seam discipline as
// defaultMaxFileSize, and what lets an anti-drift test invert the relationship
// (default BELOW the store ceiling) so a read that reverts to the default is caught.
// Production never reassigns it.
var defaultMaxElements int = 1 << 20 // 1,048,576 entries per vector

// checkWriteElements rejects state whose largest section/vector exceeds the element
// ceiling BEFORE it reaches the disk — the element-count twin of checkWriteSize.
//
// Both Load paths enforce TWO caps (bytes AND element count); before this, every write
// path enforced only the first. State of defaultMaxElements+1 short keys serializes to
// ~22 MB of JSON / ~42 MB of FlatBuffers — comfortably UNDER the 64 MiB byte ceiling —
// so Save returned nil and Load then failed permanently with "element count exceeds
// max". Exactly the HYG-00 wedge on a second axis, and worse: the byte ceiling has an
// option, so this one had no recovery path at all until it got one too.
//
// n is the LARGEST per-section (JSON) or per-vector (FlatBuffers) count, because both
// Load paths reject if ANY single section/vector exceeds the cap — max > ceiling is
// exactly equivalent, and a total across sections would falsely reject state that
// loads fine.
//
// ErrValidation for the same reason as checkWriteSize: nothing is corrupt, the caller
// handed over more state than the configured ceiling allows.
func checkWriteElements(n, ceiling int, workflowID string) error {
	ceiling = effectiveElements(ceiling)
	if n > ceiling {
		return fmt.Errorf("%w: workflow %q has a section of %d entries, exceeds the %d-entry max element count",
			ErrValidation, workflowID, n, ceiling)
	}
	return nil
}

// openForRead is the file-open seam used by Load (default os.Open). Tests swap
// it for a byte-counting wrapper to assert the bytes consumed from the fd are
// bounded by cap+1 on the live path. Production never reassigns it.
// nolint:gosec // controlled internal file paths
var openForRead = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// readBoundedFileCapped reads an entire file through io.LimitReader(ceiling+1) —
// the same bounded-read discipline as JSONFileStore.Load / FlatBuffersStore.Load —
// so every reader in the package shares one symmetric size bound. It bounds memory
// regardless of on-disk size and rejects over-ceiling input as ErrCorruptData
// (the +1, via readLimit, is what distinguishes at-cap from over-cap).
//
// The ceiling is always passed explicitly: every reader now has one to supply
// (a store's maxFileSize, or a resolved DataFileOption). openForRead is the same
// test seam Load uses; the open error (incl. fs.ErrNotExist) is returned verbatim
// for the caller to classify/wrap.
func readBoundedFileCapped(path string, ceiling int64) (data []byte, err error) {
	// Floor the ceiling, like every other point of use. A struct-literal store
	// (bypassing both constructors) carries maxFileSize == 0, and without this the
	// readers that route through here — TakeSignals and MigrateToFlatBuffers —
	// report every valid file corrupt. Save/Load floored it already; this path did
	// not, which is why TestZeroCeiling_StructLiteralStoreStillWorks stayed green:
	// neither Save nor Load routes through here.
	ceiling = effectiveFileSize(ceiling)
	f, err := openForRead(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	data, err = io.ReadAll(io.LimitReader(f, readLimit(ceiling)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > ceiling {
		return nil, fmt.Errorf("%w: file exceeds max size", ErrCorruptData)
	}
	return data, nil
}

// FlatBuffersStore is a file-based implementation of WorkflowStore that uses FlatBuffers serialization.
// It provides better performance than JSONFileStore for large workflows.
type FlatBuffersStore struct {
	baseDir string
	mu      sync.RWMutex

	// M14 ph61 group-commit (DEC-M14-GROUPCOMMIT). batchK is the durability batch
	// window: 1 = Strict (fsync every checkpoint, today's contract bit-identical);
	// >1 = Batched(K) (write+fsync only every Kth SaveCheckpoint, strategy (d) — a
	// crash loses ≤K un-checkpointed levels, re-run idempotently). ckptCount is the
	// per-workflowID checkpoint counter driving the batch cadence. Both guarded by mu.
	batchK    uint
	ckptCount map[string]uint
	// pending holds the last deferred (un-fsync'd) checkpoint per workflowID under
	// Batched(K) — strategy (d) retains the live state so a forced Sync() (the
	// suspend/completion floor) can flush it. A Clone (clone-on-save discipline).
	pending map[string]*WorkflowData
	// maxElements is the element-count ceiling, enforced on BOTH the .fb write paths
	// and Load — the second axis every Load path checks. Seeded from
	// defaultMaxElements; override with WithFlatBuffersMaxElements.
	maxElements int
	// maxFileSize is the ONE ceiling this store enforces, on BOTH Load and every
	// .fb write path (Save and the group-commit writeFullSnapshotLocked). Seeded
	// from defaultMaxFileSize; override with WithFlatBuffersMaxFileSize (HYG-00).
	maxFileSize int64
}

// WithFlatBuffersMaxFileSize sets the size ceiling enforced on both the .fb write
// paths and Load.
//
// Raising it is the supported recovery path for a .fb already on disk that exceeds
// the default ceiling: a write-side cap alone cannot help state written before the
// cap existed. n must be > 0; a non-positive n is ignored and the default retained.
//
// SCOPE — this ceiling also governs the durable SIGNAL MAILBOX, not just the
// workflow snapshot. DeliverSignal refuses an entry above it, and TakeSignals reads
// every entry through it. The mailbox read is all-or-nothing: ONE entry above the
// ceiling fails the read of the WHOLE mailbox for that workflow, so LOWERING this on
// a store whose mailbox already holds larger entries can strand a waiting run.
//
// Every process sharing a baseDir MUST agree on this value. Two processes at
// different ceilings can have one write an entry the other cannot read.
func WithFlatBuffersMaxFileSize(n int64) func(*FlatBuffersStore) {
	return func(s *FlatBuffersStore) {
		if n > 0 {
			s.maxFileSize = clampCeiling(n)
		}
	}
}

// WithFlatBuffersMaxElements sets the element-count ceiling enforced on both the .fb
// write paths and Load.
//
// Raising it is the supported recovery path for a .fb already on disk whose largest
// vector exceeds the default — the byte ceiling cannot help, because over-count state
// is typically far UNDER the byte limit. n must be > 0; a non-positive n is ignored.
func WithFlatBuffersMaxElements(n int) func(*FlatBuffersStore) {
	return func(s *FlatBuffersStore) {
		if n > 0 {
			s.maxElements = n
		}
	}
}

// DurabilityOption configures a FlatBuffersStore's durability mode (M14 ph61).
type DurabilityOption func(*FlatBuffersStore)

// WithDurabilityMode sets the store's fsync batching (M14 ph61, DEC-M14-GROUPCOMMIT).
// Pass Strict() (the default — every checkpoint is fsync-durable, bit-identical to
// pre-M14) or Batched(k) (fsync every k-th checkpoint; a power/process crash loses
// ≤k un-fsync'd levels, which resume re-runs idempotently — the deep-durable perf
// mode). The suspend/completion durability floor stays fsync-durable in EITHER mode.
func WithDurabilityMode(opt DurabilityOption) func(*FlatBuffersStore) {
	return opt
}

// Strict is the default durability mode: every SaveCheckpoint is fsync-durable
// (bit-identical to the pre-M14 contract). K=1.
func Strict() DurabilityOption {
	return func(s *FlatBuffersStore) { s.batchK = 1 }
}

// Batched sets group-commit durability: write+fsync only every k-th checkpoint. A
// crash loses ≤k un-fsync'd levels (re-run idempotently via IdempotencyKey). k must
// be ≥1 (k=1 ≡ Strict). Weakens power-loss durability in exchange for ~K× fewer
// fsyncs on deep durable runs — you must type Batched to opt in.
func Batched(k uint) DurabilityOption {
	return func(s *FlatBuffersStore) {
		if k < 1 {
			k = 1
		}
		s.batchK = k
	}
}

// NewFlatBuffersStore creates a new FlatBuffers-based workflow store.
// baseDir is the directory where workflow data will be stored.
// Returns an error if the directory cannot be created or accessed.
// Durability defaults to Strict (every checkpoint fsync'd); pass
// WithDurabilityMode(Batched(k)) to enable group-commit (M14 ph61).
func NewFlatBuffersStore(baseDir string, opts ...func(*FlatBuffersStore)) (*FlatBuffersStore, error) {
	// Create the directory if it doesn't exist. NO nolint here, deliberately: gosec has never
	// flagged this site, so a directive would pre-suppress a FUTURE genuine finding on a
	// MkdirAll that is one line from the store root. Its twin in NewJSONFileStore does carry
	// one because gosec does flag that site. G703 on this chain is threshold-sensitive, so
	// "the analyzer is quiet today" is only evidence across repeated cache-cleared runs — this
	// removal was checked that way, not from a single draw.
	err := os.MkdirAll(baseDir, 0750)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	s := &FlatBuffersStore{
		baseDir:     baseDir,
		batchK:      1, // default Strict
		ckptCount:   make(map[string]uint),
		maxFileSize: defaultMaxFileSize,
		maxElements: defaultMaxElements,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
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

	// Serialize the full snapshot (extracted so the group-commit checkpoint path
	// reuses the EXACT same serialization — M14 ph61) and write it atomically.
	buf, maxVec, err := buildFullStateBuffer(data)
	// The fallible dominator for the two host-value encodes inside the builder (AF2 +
	// F2). Refused before any byte is written, so a value too deep to encode safely
	// never produces a file at all.
	if err != nil {
		return err
	}
	// Refuse to write state this store could never read back, on BOTH axes Load
	// enforces: bytes and element count (HYG-00).
	if err := checkWriteSize(int64(len(buf)), s.maxFileSize, workflowID); err != nil {
		return err
	}
	if err := checkWriteElements(maxVec, s.maxElements, workflowID); err != nil {
		return err
	}
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	if err := writeFileAtomic(filePath, buf, 0600); err != nil {
		return newIOError("write", workflowID, err)
	}
	// A full Save (final/rollback/external) is fsync-durable and supersedes any
	// deferred batched checkpoint AND ends the batch window — clear both the pending
	// state (so a later Sync() can't re-flush a stale snapshot) and the cadence
	// counter (F1: Save is the normal completion path, so this is where the counter
	// must be reclaimed to avoid unbounded growth). (M14 ph61.)
	s.clearBatchState(workflowID)
	return nil
}

// buildFullStateBuffer serializes data as a COMPLETE WorkflowState snapshot (M14
// ph61 extraction of Save's builder body, so the group-commit checkpoint path reuses
// the identical full-snapshot serialization).
// It returns the serialized buffer AND the largest vector length, so the caller can
// apply the element-count guard against the same quantity Load will check.
//
// THE ERROR RETURN IS THE (iii-c) WIRING, and its shape is the point. The two host-value
// encodes below sit inside data.ForEach / data.ForEachOutput callbacks, which return
// NOTHING — so a refusal there has nowhere to go. It is captured into encErr at the site
// of the encode, ADJACENT to the encode of that same value, and surfaced at the first
// fallible frame that dominates both: this function's own return, which both writers
// (Save and writeFullSnapshotLocked) already check alongside checkWriteSize and
// checkWriteElements.
//
// A HOISTED walk over data BEFORE the build was considered and rejected. ForEach holds
// its RLock across the WHOLE loop (workflow_data.go) — an earlier version of this comment
// said "per call" and that was simply wrong — but the conclusion is unchanged and does not
// depend on it: a hoisted walk is a SECOND ForEach, hence a second acquisition, with the
// lock released in between. Two critical sections with a gap is 116-AF1's own shape (a
// read and a use separated by an unlocked interval) reappearing on the other side of the
// phase.
//
// AND EVEN ADJACENCY DOES NOT BUY SAME-VALUE OBSERVATION — stated here so this comment is
// not read as claiming more than it can. Set stores the caller's REFERENCE, and the mutex
// protects the map rather than the objects in it, so a host holding its own alias can
// deepen the structure without taking any lock at all. Checking beside the encode buys the
// narrowest window available, not a guarantee. See checkValueDepth.
//
// Once encErr is set the remaining callbacks short-circuit. They cannot be stopped —
// ForEach has no break — so they return immediately instead, which also guarantees the
// FIRST offending key is the one reported rather than the last.
func buildFullStateBuffer(data *WorkflowData) ([]byte, int, error) {
	workflowID := data.GetWorkflowID()

	// Create FlatBuffer builder
	builder := flatbuffers.NewBuilder(1024)

	// Create the workflow ID string
	fbWorkflowID := builder.CreateString(workflowID)

	// Create typed data vectors — use appropriate typed vector for each value type
	stringDataOffsets := make([]flatbuffers.UOffsetT, 0)
	intDataOffsets := make([]flatbuffers.UOffsetT, 0)
	boolDataOffsets := make([]flatbuffers.UOffsetT, 0)
	doubleDataOffsets := make([]flatbuffers.UOffsetT, 0)

	// The captured refusal — see this function's doc comment for why it is captured here
	// rather than hoisted into a pre-pass.
	var encErr error

	data.ForEach(func(k string, value interface{}) {
		if encErr != nil {
			return
		}
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
			// Complex types: fall back to JSON string, with BOTH depth axes closed
			// around the marshal (AF2 crash + F2 wedge). See encodeHostValue.
			strValue, err := encodeHostValue(v, fmt.Sprintf("data key %q", k))
			if err != nil {
				encErr = err
				return
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
		if encErr != nil {
			return
		}
		// Convert output to JSON string, with BOTH depth axes closed around the marshal
		// (AF2 crash + F2 wedge). See encodeHostValue.
		//
		// A string output passes through UNCHECKED, as it always has. That is not an
		// omission: it is stored verbatim and read back verbatim by decodeOutput (which
		// is the identity), so it never passes a JSON decoder and has no nesting a
		// decoder could refuse. The depth axes bound what the ENCODER must build and what
		// a DECODER must read; a byte string that is neither is bounded by the size axis.
		var outputStr string
		if v, ok := output.(string); ok {
			outputStr = v
		} else {
			s, err := encodeHostValue(output, fmt.Sprintf("output of node %q", nodeName))
			if err != nil {
				encErr = err
				return
			}
			outputStr = s
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

	// Both host-value callbacks are done; surface a captured refusal before finishing a
	// buffer nobody will write. Deliberately AFTER both rather than between them, so one
	// site cannot be added later without the check moving with it.
	if encErr != nil {
		return nil, 0, encErr
	}

	// Create NodeOutputs vector
	var outputsVector flatbuffers.UOffsetT
	if len(outputOffsets) > 0 {
		fb.WorkflowStateStartNodeOutputsVector(builder, len(outputOffsets))
		for i := len(outputOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(outputOffsets[i])
		}
		outputsVector = builder.EndVector(len(outputOffsets))
	}

	// Create durable timer waits vector (M10). fireAt is written to the faithful
	// int64 `fire_at` long, symmetric with the JSON UseNumber() path — no clamp,
	// no float64 detour, so a MaxInt64/MinInt64 fireAt round-trips losslessly.
	waitOffsets := make([]flatbuffers.UOffsetT, 0)
	data.ForEachWait(func(nodeName string, fireAt int64) {
		fbNodeName := builder.CreateString(nodeName)
		fb.TimerWaitEntryStart(builder)
		fb.TimerWaitEntryAddNodeName(builder, fbNodeName)
		fb.TimerWaitEntryAddFireAt(builder, fireAt)
		waitOffsets = append(waitOffsets, fb.TimerWaitEntryEnd(builder))
	})

	var waitsVector flatbuffers.UOffsetT
	if len(waitOffsets) > 0 {
		fb.WorkflowStateStartWaitsVector(builder, len(waitOffsets))
		for i := len(waitOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(waitOffsets[i])
		}
		waitsVector = builder.EndVector(len(waitOffsets))
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

	if len(waitOffsets) > 0 {
		fb.WorkflowStateAddWaits(builder, waitsVector)
	}

	// M12: run-level saga rollback marker. Additive scalar bool (field id 9), rides
	// the SAME atomic snapshot as node statuses — one write commits state + the
	// forward-vs-rollback intent with no torn write. Old buffers read false.
	fb.WorkflowStateAddRollingBack(builder, data.IsRollingBack())

	// M12 ph49: the rollback trigger cause. Additive scalar ubyte (field id 10), rides
	// the same atomic snapshot. Old buffers read 0 = TriggerNone.
	fb.WorkflowStateAddTriggerCause(builder, byte(data.TriggerCause()))

	workflowState := fb.WorkflowStateEnd(builder)

	// Finish the buffer
	builder.Finish(workflowState)

	// The largest of the SEVEN vector lengths — exactly the seven Load checks against
	// its element cap. Computed from the same slices that produced the vectors, so the
	// write-side guard measures precisely what the read-side guard will reject.
	maxVec := len(intDataOffsets)
	for _, n := range []int{
		len(boolDataOffsets), len(doubleDataOffsets), len(stringDataOffsets),
		len(nodeStatusOffsets), len(outputOffsets), len(waitOffsets),
	} {
		if n > maxVec {
			maxVec = n
		}
	}

	// Get the finished buffer
	return builder.FinishedBytes(), maxVec, nil
}

// SaveCheckpoint is defined in workflow_store_groupcommit.go (M14 ph61 group-commit).

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

	fileCeiling := effectiveFileSize(s.maxFileSize)
	buf, err := io.ReadAll(io.LimitReader(f, readLimit(fileCeiling)))
	if err != nil {
		return nil, newIOError("read", workflowID, err)
	}
	if int64(len(buf)) > fileCeiling {
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
	elemCeiling := effectiveElements(s.maxElements)
	if fbState.IntDataLength() > elemCeiling ||
		fbState.BoolDataLength() > elemCeiling ||
		fbState.DoubleDataLength() > elemCeiling ||
		fbState.StringDataLength() > elemCeiling ||
		fbState.NodeStatusesLength() > elemCeiling ||
		fbState.NodeOutputsLength() > elemCeiling ||
		fbState.WaitsLength() > elemCeiling {
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
			// AUD-036: an unknown enum is a corrupt/forged journal — fail closed rather
			// than silently rerun a terminal node as Pending.
			status, ok := fbStatusToNodeStatus(entry.Status())
			if !ok {
				return nil, fmt.Errorf("%w: node %q has unknown FlatBuffers status %d", ErrCorruptData, nodeName, entry.Status())
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

	// Load durable timer waits (M10). The int64 fire_at is read straight from the
	// faithful long — no float64 detour — so a MaxInt64/MinInt64 fireAt survives
	// the FB round-trip, matching the JSON path. Absent in pre-M10 buffers
	// (WaitsLength()==0), so older snapshots load unchanged (additive field id 8).
	for i := 0; i < fbState.WaitsLength(); i++ {
		var entry fb.TimerWaitEntry
		if fbState.Waits(&entry, i) {
			data.SetWait(string(entry.NodeName()), entry.FireAt())
		}
	}

	// M12: run-level saga rollback marker (additive scalar bool, field id 9). Absent
	// in pre-M12 buffers -> RollingBack() returns the false default -> a forward run,
	// loaded unchanged. A rolling_back run re-enters the rollback drive on resume.
	data.SetRollingBack(fbState.RollingBack())

	// M12 ph49: the rollback trigger cause (additive scalar ubyte, field id 10). Absent
	// in pre-ph49 buffers -> 0 = TriggerNone -> reconstructCause falls to inference.
	// Bounds-guarded for decoder symmetry with the JSON path (review ph49-F1): an
	// out-of-range byte from a corrupt/forged buffer leaves TriggerNone.
	if b := fbState.TriggerCause(); b <= byte(TriggerDeadlineExceeded) {
		data.SetTriggerCause(TriggerCause(b))
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

	// Reclaim the durable signal mailbox too (ph37 F2): the <id>.signals/ sibling
	// dir is a separate channel the snapshot Delete would otherwise orphan. Done
	// FIRST + best-effort so it runs even for a mailbox with no snapshot (an early
	// signal delivered to a workflow that never ran/saved).
	//
	// ONE OBJECT IS DELIBERATELY NOT RECLAIMED, and it is a stated exception to this
	// store's "there is no background GC; reclamation is owned by Delete" contract
	// rather than an oversight: the <id>.signals.lock file survives. Unlinking it
	// while another process holds it would hand the next deliverer a fresh inode and
	// re-arm the very race the lock exists to close — and it cannot be removed safely
	// even under the lock, because proving nobody holds it requires holding it. The
	// population and cost are stated ONCE, on signalLockSuffix, and deliberately not
	// restated here — the phrasing has already been wrong once, and this comment is
	// duplicated across both file stores, so a restatement drifts in two places at
	// once. It is invisible to the *.json / *.fb listing
	// globs, so it never surfaces as a phantom workflow, and it is not created at all
	// on non-unix, where the lock is a no-op. Delete does NOT create one for a workflow
	// that never had a delivery — it acquires with create=false and skips when absent,
	// because creating here made Delete mint a permanent artifact for ids that never
	// existed.
	//
	// LOCK HELD ACROSS A CROSS-PROCESS WAIT, stated because the shape deserves a reader's
	// attention: s.mu is held across removeSignalDir, which blocks on flock(LOCK_EX) with
	// no timeout. A delivery in ANOTHER PROCESS holding that flock therefore stalls this
	// process's whole store — every Save/Load/List/Delete queues behind s.mu. It is not a
	// deadlock: the only lock ordering in the package is s.mu -> flock (delivery takes the
	// flock and no s.mu), so no cycle exists, and the kernel releases a flock when its
	// holder dies. The case that can actually stall is a LIVE but stopped holder — a
	// SIGSTOP'd process, or one wedged in a pathological fsync — not a crashed one.
	//nolint:errcheck,gosec // best-effort mailbox reclamation (ph37 F2)
	removeSignalDir(s.baseDir, workflowID)

	// Drop the group-commit batch state (M14 ph61, AF1): a deferred (un-fsync'd)
	// checkpoint held in s.pending, plus the ckptCount cadence counter, must be
	// cleared on Delete — otherwise the workflow is gone from disk but a later
	// public Sync() would re-flush the stale pending snapshot and RESURRECT the
	// deleted workflow.
	s.clearBatchState(workflowID)

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
// fbStatusToNodeStatus returns ok=false for an unknown enum value rather than silently
// coercing it to Pending (AUD-036 / P-04). An unknown enum in a durable buffer is a
// corrupt/forged journal; mapping it to Pending would rerun a node that was terminal. The
// caller rejects !ok as ErrCorruptData, matching SQLite's isKnownStatus fail-closed policy.
func fbStatusToNodeStatus(status fb.NodeStatus) (NodeStatus, bool) {
	switch status {
	case fb.NodeStatusPending:
		return Pending, true
	case fb.NodeStatusRunning:
		return Running, true
	case fb.NodeStatusCompleted:
		return Completed, true
	case fb.NodeStatusFailed:
		return Failed, true
	case fb.NodeStatusSkipped:
		return Skipped, true
	case fb.NodeStatusWaiting:
		return Waiting, true
	case fb.NodeStatusBypassed:
		return Bypassed, true
	case fb.NodeStatusCompensated:
		return Compensated, true
	case fb.NodeStatusCompensationFailed:
		return CompensationFailed, true
	default:
		return "", false
	}
}

// InMemoryStore is an in-memory implementation of WorkflowStore.
// It's useful for testing and workflows that don't need persistence.
type InMemoryStore struct {
	data map[string]*WorkflowData
	// signals is the in-process durable mailbox (M10 phase 37): per-workflow
	// delivered signals, keyed by workflowID, deduplicated by sig.ID. It lives
	// SEPARATE from data (the snapshot) so a DeliverSignal cannot clobber a
	// running instance's checkpoint (MH37-1). Durable only in-process, exactly
	// like this store's checkpoint — honest for an in-memory store.
	signals map[string]map[string]Signal
	mu      sync.RWMutex
}

// NewInMemoryStore creates a new in-memory workflow store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		data:    make(map[string]*WorkflowData),
		signals: make(map[string]map[string]Signal),
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

	// Clone the data to avoid external modification, then canonicalize the ISOLATED
	// clone (AUD-026) so InMemory yields the SAME values the durable stores do — a
	// faithful substitute rather than an over-faithful one. Canonicalizing the clone,
	// never `data`, leaves a live in-flight instance untouched.
	clone := data.Clone()
	clone.canonicalizeForStore()
	s.data[workflowID] = clone
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
	delete(s.signals, workflowID) // reclaim the in-process mailbox too (ph37 F2)
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
	case Waiting:
		return fb.NodeStatusWaiting
	case Bypassed:
		return fb.NodeStatusBypassed
	case Compensated:
		return fb.NodeStatusCompensated
	case CompensationFailed:
		return fb.NodeStatusCompensationFailed
	default:
		return fb.NodeStatusPending
	}
}
