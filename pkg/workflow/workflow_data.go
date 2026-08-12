package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// WorkflowData is the central data store for workflow execution.
// It maintains the state of the workflow, including node statuses and outputs.
//
// # Concurrency (AUD-038)
//
// The accessor methods (Set/Get and the typed getters, SetNodeStatus/GetNodeStatus,
// SetOutput/GetOutput, SetWait/GetWait/ClearWait, the IsRollingBack/TriggerCause markers)
// are safe for concurrent use — every one guards a single RWMutex.
//
// Synchronization is SLOT-LEVEL, not deep. Set/Get lock the map SLOT for a key, NOT the
// contents of a map, slice, or pointer stored through it as `any`. If you Set a
// reference-typed value and then mutate that same object — or mutate a value you got back
// from Get — you race the object's contents even though the slot access was synchronized.
// Treat stored values as immutable, or clone before mutating. (Clone deep-copies the CANONICAL
// value algebra — scalars, map[string]interface{}, and []interface{}, nested and cycle-safe —
// so mutating those in the source never affects the clone; NON-canonical values (typed maps,
// pointers, custom structs) are retained by reference and stay shared. See Clone.)
//
// The ForEach iterators (ForEach, ForEachNodeStatus, ForEachOutput, ForEachWait) SNAPSHOT
// their entries under the read lock and invoke the callback AFTER releasing it, so a callback
// may safely read from — or write back into — the same WorkflowData without deadlocking
// (AUD-029). The callback sees a consistent point-in-time view; writes made after the snapshot
// are not observed by that iteration.
type WorkflowData struct {
	// Single mutex for data access. A POINTER (not a value) so a sealed per-node view
	// (sealedViewFor, M24 DEC-M24-MEDIATION) can be a struct copy that SHARES this one
	// lock and the backing maps, rather than copying a lock value (copylocks; split lock
	// state over shared maps). Set by NewWorkflowDataWithConfig and Clone (each its own),
	// copied-by-pointer into a sealed view (shared). See mediation.go.
	mu *sync.RWMutex

	// sealed marks a per-node action view (M24 AUD-019): the executor hands each
	// consumer action a sealedViewFor(node) instead of the raw data, so the action
	// cannot forge the engine journal. When sealed, the engine-authority mutators
	// (SetNodeStatus/SetWait/ClearWait/SetRollingBack/SetTriggerCause, and SetOutput for
	// a node OTHER than sealedNode) record sealedViolation and no-op instead of mutating.
	// The unsealed instance the engine/host holds is unaffected. Reads are never sealed.
	sealed          bool
	sealedNode      string
	sealedViolation error

	// Simple maps for data storage
	data       map[string]interface{}
	nodeStatus map[string]NodeStatus
	outputs    map[string]interface{} // Renamed from nodeOutput for compatibility

	// waits holds the durable wake metadata of parked timer nodes (M10 chunk 2,
	// D36-04): nodeName -> fireAt, an ABSOLUTE wall-clock instant in unix
	// nanoseconds (int64). It is the persisted source of truth for a durable
	// timer — the live time.Timer is a disposable optimization re-derived from
	// this on every boot. fireAt rides the same WorkflowData snapshot as node
	// statuses/outputs, so one atomic write commits state+timer with no torn
	// write, and the int64 magnitude round-trips losslessly through both the
	// FlatBuffers (value_long-style long) and JSON (UseNumber) paths.
	waits map[string]int64

	// rollingBack is the run-level saga rollback marker (M12): once a hard failure
	// triggers compensation, the drive sets this true and persists it in the SAME
	// snapshot as node statuses, so a crash mid-rollback re-loads a rolling_back run
	// and re-enters the rollback drive (never the forward path). A single bool —
	// the whole run is either rolling forward or rolling back — mirrors the `waits`
	// durable-field lifecycle but is not per-node (DEC-M12-STATE: no per-node
	// Compensating status). Rides the atomic WorkflowData write across all 3 stores.
	rollingBack bool

	// triggerCause is the durable discriminator of WHY the run is rolling back (M12
	// ph49, resolves ph48-F2): TriggerFailure / TriggerCanceled / TriggerDeadlineExceeded,
	// or TriggerNone when not rolling back. Journaled in the SAME snapshot as
	// rollingBack, so a resumed rollback recovers the TRUE cause across a crash — a
	// cancel stays a cancel, never a spurious node-failure reconstructed from an
	// incidental Failed node. Written only at the rollback trigger.
	triggerCause TriggerCause

	// Keep the ID and metrics configuration
	ID      string
	metrics *metrics.MetricsCollector

	// String interning for efficiency
	stringInterner *stringInterner

	// capture is the per-Execute delta accumulator (M15 ph69). NIL on the non-durable /
	// plain-Checkpointer hot path (recordDelta is then a single branch, zero alloc — det-tax
	// stays EXACT). Non-nil only while an IncrementalCheckpointer run drives forward, set by
	// beginDeltaCapture. Guarded by mu (see workflow_data_delta.go).
	capture *deltaCapture
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
	stringInterner := newStringInterner()

	// Create the workflow data with simple maps
	return &WorkflowData{
		mu:             &sync.RWMutex{}, // M24: pointer lock (see the struct doc + sealedViewFor)
		ID:             id,
		data:           make(map[string]interface{}, config.ExpectedData),
		nodeStatus:     make(map[string]NodeStatus, config.ExpectedNodes),
		outputs:        make(map[string]interface{}, config.ExpectedNodes),
		waits:          make(map[string]int64),
		metrics:        metricsCollector,
		stringInterner: stringInterner,
	}
}

// SetWait records the durable wake metadata for a parked timer node: fireAt, an
// absolute wall-clock instant in unix nanoseconds (M10 chunk 2, D36-04). It is
// stored in the same snapshot as node statuses, so a single atomic checkpoint
// write commits "this node is Waiting until fireAt".
func (w *WorkflowData) SetWait(nodeName string, fireAt int64) {
	if w.sealed { // M24: only the timer/signal engine actions arm waits
		w.recordSealViolation("SetWait")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.internKey(nodeName)
	w.waits[k] = fireAt
	w.recordDelta(deltaWait, k)
}

// GetWait returns the persisted fireAt for a node and whether the node has an
// armed timer. A non-ok result means the node is not (or no longer) an armed
// timer — the timer action treats that as "arm me" on first encounter.
func (w *WorkflowData) GetWait(nodeName string) (int64, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	// No interning on the read path (see Get).
	fireAt, ok := w.waits[nodeName]
	return fireAt, ok
}

// ClearWait removes a node's armed-timer metadata. Called when a timer fires (the
// node transitions out of Waiting), so a completed timer carries no stale fireAt.
func (w *WorkflowData) ClearWait(nodeName string) {
	if w.sealed { // M24: only the timer/signal engine actions disarm waits
		w.recordSealViolation("ClearWait")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// No interning on the read path; delete is keyed by string value.
	delete(w.waits, nodeName)
	// Record the touch: the fast path re-reads d, finds the wait ABSENT ⇒ DELETEs the row.
	w.recordDelta(deltaWait, nodeName)
}

// ForEachWait iterates over every armed timer's (nodeName, fireAt). Used by the
// host-driven Tick/DueTimers wake API to find due timers.
//
// AUD-029 / C-14: the callback runs AFTER the read lock is released, over a snapshot
// taken under it. The RWMutex is non-reentrant, so the previous hold-lock-across-callback
// form deadlocked any callback that called back into WorkflowData (GetWait/SetWait/
// ClearWait/GetNodeStatus/…). It is now safe to access WorkflowData from the callback;
// the snapshot is a consistent point-in-time view (Review F5 caveat retired).
func (w *WorkflowData) ForEachWait(fn func(nodeName string, fireAt int64)) {
	w.mu.RLock()
	names := make([]string, 0, len(w.waits))
	fireAts := make([]int64, 0, len(w.waits))
	for k, v := range w.waits {
		names = append(names, k)
		fireAts = append(fireAts, v)
	}
	w.mu.RUnlock()

	for i, k := range names {
		fn(k, fireAts[i])
	}
}

// SetRollingBack sets the run-level saga rollback marker (M12). The saga trigger
// calls SetRollingBack(true) after a hard failure and persists the snapshot; on a
// later resume, executeLocked reads IsRollingBack to switch to the rollback drive
// instead of the forward DAG.Execute.
func (w *WorkflowData) SetRollingBack(rollingBack bool) {
	if w.sealed { // M24: rollback is an engine decision, never an action's
		w.recordSealViolation("SetRollingBack")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rollingBack = rollingBack
}

// IsRollingBack reports whether this run is in saga rollback (M12) — i.e. a hard
// failure has triggered compensation. Persisted in the snapshot, so it survives a
// crash and drives the forward-vs-rollback switch on resume.
func (w *WorkflowData) IsRollingBack() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.rollingBack
}

// SetTriggerCause journals WHY the run rolled back (M12 ph49). The saga trigger calls
// it alongside SetRollingBack(true), in the same persisted snapshot, so a resumed
// rollback recovers the true cause across a crash (resolves ph48-F2).
func (w *WorkflowData) SetTriggerCause(cause TriggerCause) {
	if w.sealed { // M24: the rollback cause is journaled by the engine, never by an action
		w.recordSealViolation("SetTriggerCause")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.triggerCause = cause
}

// TriggerCause reports the journaled rollback trigger cause (M12 ph49), or
// TriggerNone when the run is not rolling back (or a pre-ph49 snapshot). reconstructCause
// reads it on resume to return the honest cause (a cancel as a cancel, a deadline as a
// deadline) instead of inferring a node-failure from an incidental Failed node.
func (w *WorkflowData) TriggerCause() TriggerCause {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.triggerCause
}

// Set stores a value in the workflow data.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Set(key string, value interface{}) {
	if w.sealed && isReservedKey(key) { // M24 AUD-018: a consumer action cannot write engine-reserved keys
		w.recordSealViolation("Set on an engine-reserved key")
		return
	}
	w.setValue(key, value)
}

// setReserved writes an engine-reserved (__-prefixed) key even through a sealed view.
// In-package plumbing only — the fan-out branch wrapper uses it to pass the per-branch
// item (__fanout_item__). A consumer, in another package, cannot reach it and is refused
// by Set above. (M24 AUD-018.)
func (w *WorkflowData) setReserved(key string, value interface{}) {
	w.setValue(key, value)
}

// setValue is the shared write core of Set/setReserved (under the lock, with metrics).
func (w *WorkflowData) setValue(key string, value interface{}) {
	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(key)
		w.data[k] = value
		w.recordDelta(deltaData, k)
		return
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSet, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(key)
		w.data[k] = value
		w.recordDelta(deltaData, k)
	})
}

// Get retrieves a value from the workflow data.
// Returns the value and a boolean indicating if the key exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Get(key string) (interface{}, bool) {
	var result interface{}
	var exists bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		// No interning on the read path: Go maps key by string value, so a raw
		// key matches a stored interned key. Interning would only take the
		// interner's lock and (for arenas) pollute the pool with read-only keys.
		result, exists = w.data[key]
		return result, exists
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGet, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, exists = w.data[key]
	})

	return result, exists
}

// Delete removes a key-value pair from the workflow data.
// Returns true if the key existed and was deleted.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) Delete(key string) bool {
	var existed bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.Lock()
		defer w.mu.Unlock()
		internedKey := w.internKey(key)
		_, existed = w.data[internedKey]
		if existed {
			delete(w.data, internedKey)
		}
		// ph69-AF1: Delete is a 6th data mutator — record the touch so the delta fast path
		// re-reads d, finds the key ABSENT, and emits the DELETE (else the row survives and
		// the fast path diverges from a full SaveCheckpoint). Recorded unconditionally: a
		// no-op delete re-reads absent and DELETEs 0 rows, still byte-identical.
		w.recordDelta(deltaData, internedKey)
		return existed
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpDelete, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		internedKey := w.internKey(key)
		_, existed = w.data[internedKey]
		if existed {
			delete(w.data, internedKey)
		}
		w.recordDelta(deltaData, internedKey) // ph69-AF1 (see fast-path note above)
	})

	return existed
}

// SetNodeStatus updates the status of a node in the workflow.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) SetNodeStatus(nodeName string, status NodeStatus) {
	if w.sealed { // M24: a consumer action cannot forge node status
		w.recordSealViolation("SetNodeStatus")
		return
	}
	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(nodeName)
		w.nodeStatus[k] = status
		w.recordDelta(deltaNode, k)
		return
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSetStatus, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(nodeName)
		w.nodeStatus[k] = status
		w.recordDelta(deltaNode, k)
	})
}

// GetNodeStatus retrieves the status of a node in the workflow.
// Returns the status and a boolean indicating if the node exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) GetNodeStatus(nodeName string) (NodeStatus, bool) {
	var status NodeStatus
	var exists bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		// No interning on the read path (see Get).
		status, exists = w.nodeStatus[nodeName]
		return status, exists
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetStatus, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		status, exists = w.nodeStatus[nodeName]
	})

	return status, exists
}

// SetOutput stores the output of a node.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) SetOutput(nodeName string, output interface{}) {
	if w.sealed && nodeName != w.sealedNode { // M24: an action may record its OWN output, never another node's
		w.recordSealViolation("SetOutput for a node other than the running node")
		return
	}
	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(nodeName)
		w.outputs[k] = output
		w.recordDelta(deltaNode, k)
		return
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpSetOutput, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		k := w.internKey(nodeName)
		w.outputs[k] = output
		w.recordDelta(deltaNode, k)
	})
}

// GetOutput retrieves the output of a node.
// Returns the output and a boolean indicating if the output exists.
// This method is thread-safe and can be called concurrently.
func (w *WorkflowData) GetOutput(nodeName string) (interface{}, bool) {
	var output interface{}
	var exists bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		// No interning on the read path (see Get).
		output, exists = w.outputs[nodeName]
		return output, exists
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetOutput, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		output, exists = w.outputs[nodeName]
	})

	return output, exists
}

// metricsDisabled reports whether the metrics closure should be skipped for this
// operation — either because metrics are disabled outright, or because this
// operation falls outside the sampling rate. When true, callers take a
// metrics-free fast path (direct lock + map op) that avoids the TrackOperation
// closure, the per-op time.Now()/time.Since() pair, and the atomic bookkeeping.
// This mirrors the existing branches in IsNodeRunnable/Snapshot/LoadSnapshot.
func (w *WorkflowData) metricsDisabled() bool {
	return !w.metrics.IsEnabled() ||
		(w.metrics.GetSamplingRate() < 1.0 && utils.SecureRandomFloat64() > w.metrics.GetSamplingRate())
}

// internKey interns a string key to reduce memory usage.
// This is an internal helper method.
func (w *WorkflowData) internKey(key string) string {
	// Use the global string interner when present.
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

// isNodeRunnableInternal is the internal implementation of IsNodeRunnable.
// Caller must hold the read lock.
func (w *WorkflowData) isNodeRunnableInternal(nodeName string) bool {
	// No interning on the read path (see Get).
	// If the node is already running, completed, failed, or skipped, it's not runnable
	if status, ok := w.nodeStatus[nodeName]; ok {
		if status == Running || status == Completed || status == Failed || status == Skipped {
			return false
		}
	}

	// Node is considered runnable from the data perspective (status is pending/not_started).
	// Actual dependency checking requires DAG structure and is handled by the executor.
	return true
}

// IsNodeRunnableWithDeps checks if a node is runnable by verifying its status
// and that all specified dependencies have completed.
func (w *WorkflowData) IsNodeRunnableWithDeps(nodeName string, depNames []string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// No interning on the read path (see Get).
	// Check own status first
	if status, ok := w.nodeStatus[nodeName]; ok {
		if status == Running || status == Completed || status == Failed || status == Skipped {
			return false
		}
	}

	// Check all dependencies are completed
	for _, depName := range depNames {
		depStatus, exists := w.nodeStatus[depName]
		if !exists || depStatus != Completed {
			return false
		}
	}

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

// maxSectionCount returns the largest per-section entry count across the four sections
// LoadSnapshot caps (data, nodeStatus, outputs, waits). It is the write-side twin of
// that read-side guard: the sections here map 1:1 to the JSON keys createSnapshot
// emits, so counting them is exactly the quantity Load will check.
//
// Max, not total: LoadSnapshot rejects if ANY single section exceeds the cap, so a
// total would falsely reject state that loads fine.
func (w *WorkflowData) maxSectionCount() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.maxSectionCountLocked()
}

// maxSectionCountLocked is maxSectionCount for callers already holding the read lock.
func (w *WorkflowData) maxSectionCountLocked() int {
	n := len(w.data)
	if len(w.nodeStatus) > n {
		n = len(w.nodeStatus)
	}
	if len(w.outputs) > n {
		n = len(w.outputs)
	}
	if len(w.waits) > n {
		n = len(w.waits)
	}
	return n
}

// createSnapshot creates a snapshot of the workflow data
// Caller must hold the read lock
func (w *WorkflowData) createSnapshot() ([]byte, error) {
	// Create a snapshot structure. `waits` (durable timer fireAt, M10) rides the
	// same snapshot; omitted when empty so a workflow with no timers serializes
	// byte-identically to its pre-M10 form (additive, backward-compatible). int64
	// fireAt values are marshalled as JSON number literals and rehydrated through
	// the UseNumber() path in loadSnapshotInternal, preserving full int64
	// magnitude (the v0.7.1 int64-via-float64 lesson applied to fireAt).
	snapshot := map[string]interface{}{
		"id":         w.ID,
		"data":       w.data,
		"nodeStatus": w.nodeStatus,
		"outputs":    w.outputs,
	}
	if len(w.waits) > 0 {
		snapshot["waits"] = w.waits
	}
	// M12: run-level saga rollback marker. Omitted when false so a non-saga run (or
	// a forward run) serializes byte-identically to its pre-M12 form (additive,
	// backward-compatible — the same discipline as `waits`).
	if w.rollingBack {
		snapshot["rolling_back"] = w.rollingBack
	}
	// M12 ph49: the durable rollback trigger cause. Omitted when TriggerNone (not
	// rolling back) so a non-saga/forward run stays byte-identical (additive).
	if w.triggerCause != TriggerNone {
		snapshot["trigger_cause"] = uint8(w.triggerCause)
	}

	// AF2: the CRASH axis, and it must run BEFORE the marshal, not after it. json.Marshal
	// is recursive with no depth limit; the checkJSONDepth below measures bytes that only
	// exist if the encoder survived, so it cannot see this vector at all. `snapshot` here
	// holds w.data and w.outputs — host values straight from Set/SetOutput — so this is a
	// live path, not a defensive one. Refused before the encoder is entered.
	if err := checkValueDepth(snapshot, fmt.Sprintf("snapshot of workflow %q", w.ID)); err != nil {
		return nil, err
	}
	// Serialize to JSON
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	// Depth cap HERE, not at the callers. createSnapshot is the marshal site and it is
	// already fallible, so this is a check rather than new plumbing — and putting it here
	// covers every caller at once, including the EXPORTED WorkflowData.Snapshot(), which
	// carried no cap at all.
	//
	// That gap is worth naming because of how it hid: SaveToJSON has a checkJSONDepth,
	// and it is easy to read the four existing checks as covering the four marshal sites.
	// They do not pair up — SaveToJSON's check guards ITS OWN bytes, not createSnapshot's,
	// and Snapshot() reaches this marshal without passing either. A count of guards is not
	// a count of guarded sites.
	//
	// Snapshot() is JSONFileStore's serializer, so an uncapped document here is one the
	// store's own reader can refuse on load: the write-accepts/read-refuses wedge, the
	// same shape as the fan-out journal and WithInput.
	if derr := checkJSONDepth(b, w.GetWorkflowID()); derr != nil {
		return nil, derr
	}
	return b, nil
}

// LoadSnapshot loads a snapshot into the workflow data, bounding each section at the
// package default element ceiling. A caller with its own ceiling (a store's
// maxElements) uses loadSnapshotBounded instead, so both sides of that store agree.
func (w *WorkflowData) LoadSnapshot(data []byte) error {
	return w.loadSnapshotBounded(data, defaultMaxElements)
}

// loadSnapshotBounded is LoadSnapshot with an explicit per-section element ceiling.
func (w *WorkflowData) loadSnapshotBounded(data []byte, maxElements int) error {
	// Skip metrics if disabled or sampling
	if !w.metrics.IsEnabled() || (w.metrics.GetSamplingRate() < 1.0 && utils.SecureRandomFloat64() > w.metrics.GetSamplingRate()) {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.loadSnapshotInternal(data, maxElements)
	}

	// With metrics
	var err error
	w.metrics.TrackOperation(metrics.OpLoadSnapshot, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		err = w.loadSnapshotInternal(data, maxElements)
	})

	return err
}

// loadSnapshotInternal loads a snapshot into the workflow data
// Caller must hold the write lock
func (w *WorkflowData) loadSnapshotInternal(data []byte, maxElements int) error {
	// Deserialize from JSON. UseNumber so integer values round-trip exactly:
	// decoding into interface{} otherwise turns every JSON number into a float64,
	// which silently loses precision for int64 magnitudes above 2^53 (e.g. MaxInt64
	// rounds to 2^63, and int64(2^63) then overflows — platform-defined, so it can
	// pass on one arch and corrupt on another). json.Number keeps the original
	// literal so it can be parsed losslessly below.
	var snapshot map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&snapshot); err != nil {
		return err
	}

	// Bounds guard: element-count caps, symmetric with the FlatBuffers Load path
	// (defaultMaxElements per vector). A small JSON document can still decode into
	// maps of millions of entries; reject any section over the cap as ErrCorruptData
	// before populating the maps, so a malformed/abusive payload cannot drive a huge
	// allocation. (The byte-size cap upstream bounds the document; this bounds the
	// decoded entry count typed and early.)
	for _, section := range []string{"data", "nodeStatus", "outputs", "waits"} {
		if m, ok := snapshot[section].(map[string]interface{}); ok && len(m) > maxElements {
			return fmt.Errorf("%w: element count exceeds max", ErrCorruptData)
		}
	}

	// Update ID
	if id, ok := snapshot["id"].(string); ok {
		w.ID = id
	}

	// Update data, canonicalized (AUD-026). canonicalDataValue is the single source
	// of truth for the cross-store contract: json.Number normalizes to int64-if-exact
	// else float64 (the full int64 range faithful, matching the FlatBuffers value_long
	// path, instead of a lossy float64), scalars keep their type, and a COMPLEX value
	// (a nested map/slice) collapses to the SAME canonical JSON string the FB/SQLite
	// stores yield — so a value round-trips identically on every store. A depth-refused
	// value (which a valid persisted document cannot contain, but a forged one might)
	// surfaces as ErrCorruptData rather than a silent mis-decode.
	if data, ok := snapshot["data"].(map[string]interface{}); ok {
		w.data = make(map[string]interface{})
		for k, v := range data {
			cv, cerr := canonicalDataValue(k, v)
			if cerr != nil {
				return fmt.Errorf("%w: data key %q: %w", ErrCorruptData, k, cerr)
			}
			w.data[w.internKey(k)] = cv
		}
	}

	// Update node status. AUD-036 / P-04: reject an unknown status string as ErrCorruptData
	// rather than accepting ANY string into NodeStatus. A forged/bit-rotted snapshot carrying
	// a bogus terminal status would otherwise load and, being unrecognized by the executor,
	// let a node rerun — the shared strict isKnownStatus policy (SQLite's) fails closed here too.
	if nodeStatus, ok := snapshot["nodeStatus"].(map[string]interface{}); ok {
		w.nodeStatus = make(map[string]NodeStatus)
		for k, v := range nodeStatus {
			if status, ok := v.(string); ok {
				ns := NodeStatus(status)
				if !isKnownStatus(ns) {
					return fmt.Errorf("%w: node %q has unknown status %q", ErrCorruptData, k, status)
				}
				w.nodeStatus[w.internKey(k)] = ns
			}
		}
	}

	// Update outputs, canonicalized (AUD-026). FB/SQLite store outputs string-on-wire,
	// so the canonical form of every output is its string form: a string passes
	// through, anything else (a scalar, a map) becomes the canonical JSON string. This
	// is what makes a JSON-store output reload identically to an FB/SQLite one instead
	// of as a json.Number or a live map.
	if outputs, ok := snapshot["outputs"].(map[string]interface{}); ok {
		w.outputs = make(map[string]interface{})
		for k, v := range outputs {
			cv, cerr := canonicalOutputValue(k, v)
			if cerr != nil {
				return fmt.Errorf("%w: output %q: %w", ErrCorruptData, k, cerr)
			}
			w.outputs[w.internKey(k)] = cv
		}
	}

	// Update durable timer waits (M10). fireAt is an absolute unix-nanos int64 and
	// MUST rehydrate at full magnitude — with UseNumber every JSON number arrives
	// as json.Number (the original literal), so Int64() recovers it exactly,
	// matching the FlatBuffers long path. A value that does not parse as an int64
	// (a malformed/forged snapshot) is rejected as ErrCorruptData rather than
	// silently coerced through a lossy float64 (the v0.7.1 int64-via-float64 trap).
	// Always reset the map so a resume that carries no waits clears any stale ones
	// (symmetric with the data/nodeStatus/outputs resets above).
	w.waits = make(map[string]int64)
	if waits, ok := snapshot["waits"].(map[string]interface{}); ok {
		for k, v := range waits {
			num, ok := v.(json.Number)
			if !ok {
				return fmt.Errorf("%w: timer fireAt for %q is not a number", ErrCorruptData, k)
			}
			fireAt, err := num.Int64()
			if err != nil {
				return fmt.Errorf("%w: timer fireAt for %q is not an int64: %w", ErrCorruptData, k, err)
			}
			w.waits[w.internKey(k)] = fireAt
		}
	}

	// M12: run-level saga rollback marker. Always reset first so a resume that is no
	// longer rolling back clears any stale marker (symmetric with the waits reset);
	// absent in a pre-M12 or forward snapshot -> stays false.
	w.rollingBack = false
	if rb, ok := snapshot["rolling_back"].(bool); ok {
		w.rollingBack = rb
	}

	// M12 ph49: the durable rollback trigger cause. Reset first (a resume that is no
	// longer rolling back clears it); absent in a pre-ph49 snapshot -> TriggerNone. With
	// UseNumber the value arrives as a json.Number.
	w.triggerCause = TriggerNone
	if tc, ok := snapshot["trigger_cause"].(json.Number); ok {
		// Bounds-guard the conversion: a valid cause is 0..TriggerDeadlineExceeded; a
		// malformed/forged value stays TriggerNone (reconstructCause then infers). This
		// also satisfies gosec G115 (int64->uint8) with a real range check.
		if n, err := tc.Int64(); err == nil && n >= 0 && n <= int64(TriggerDeadlineExceeded) {
			w.triggerCause = TriggerCause(n)
		}
	}

	return nil
}

// GetWorkflowID returns the unique identifier for this workflow
func (w *WorkflowData) GetWorkflowID() string {
	return w.ID
}

// attachMetricsFromConfig rebuilds this data's metrics collector from cfg — used on
// RESUME. File-backed stores (JSON/FlatBuffers) do not persist metrics config, so a
// loaded WorkflowData carries a default (disabled) collector; the Workflow's
// MetricsConfig is the authority and must be re-attached, or an enabled workflow
// silently resumes with metrics OFF (AUD-016 / P-05). A nil cfg is a no-op. Called
// before the drive, so no accumulated stats are discarded.
func (w *WorkflowData) attachMetricsFromConfig(cfg *metrics.Config) {
	if cfg == nil {
		return
	}
	w.metrics = metrics.NewMetricsCollectorWithConfig(cfg.GetInternalConfig())
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

// ForEach iterates over all key-value pairs in the data map.
//
// AUD-029 / C-14: the callback runs AFTER the read lock is released, over a snapshot
// taken under it. The RWMutex is non-reentrant, so invoking the callback while holding
// the lock deadlocked any callback that wrote back (Set/…) and let a slow callback block
// every writer. Snapshotting first makes a re-entrant write safe; the callback sees a
// consistent point-in-time view (concurrent writes after the snapshot are not observed).
func (w *WorkflowData) ForEach(fn func(key string, value interface{})) {
	w.mu.RLock()
	keys := make([]string, 0, len(w.data))
	vals := make([]interface{}, 0, len(w.data))
	for k, v := range w.data {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	w.mu.RUnlock()

	for i, k := range keys {
		fn(k, vals[i])
	}
}

// forEachNodeStatusLocked iterates node statuses UNDER the read lock, without snapshotting.
// For TRUSTED internal engine callers ONLY — ones that collect-then-act and never write
// back into WorkflowData (nor call another locking method) from the callback, so they
// cannot hit the non-reentrant-RWMutex deadlock the public method guards against. Keeping
// these callers allocation-free is load-bearing: clearWaiting runs on EVERY DAG.Execute
// exit, and the public snapshot form (AUD-029) would add heap allocations to the
// non-durable drive, breaching the ratified det-tax alloc ceiling (perf_ceiling_test.go).
// External/untrusted callbacks must use the public ForEachNodeStatus, which snapshots.
func (w *WorkflowData) forEachNodeStatusLocked(fn func(nodeName string, status NodeStatus)) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for k, v := range w.nodeStatus {
		fn(k, v)
	}
}

// ForEachNodeStatus iterates over all node statuses. Like ForEach, the callback runs on
// a snapshot AFTER the lock is released (AUD-029 / C-14), so it may safely write back.
func (w *WorkflowData) ForEachNodeStatus(fn func(nodeName string, status NodeStatus)) {
	w.mu.RLock()
	names := make([]string, 0, len(w.nodeStatus))
	statuses := make([]NodeStatus, 0, len(w.nodeStatus))
	for k, v := range w.nodeStatus {
		names = append(names, k)
		statuses = append(statuses, v)
	}
	w.mu.RUnlock()

	for i, k := range names {
		fn(k, statuses[i])
	}
}

// ForEachOutput iterates over all node outputs. The callback runs on a snapshot AFTER the
// lock is released (AUD-029 / C-14), so it may safely write back. Map-valued outputs are
// deep-copied (under the lock) to prevent comparison issues and external mutation.
func (w *WorkflowData) ForEachOutput(fn func(nodeName string, output interface{})) {
	w.mu.RLock()
	names := make([]string, 0, len(w.outputs))
	outs := make([]interface{}, 0, len(w.outputs))
	for k, v := range w.outputs {
		names = append(names, k)
		// Deep copy maps to prevent comparison issues (snapshot a stable copy under lock).
		if m, ok := v.(map[string]interface{}); ok {
			outs = append(outs, cloneMap(m))
		} else {
			outs = append(outs, v)
		}
	}
	w.mu.RUnlock()

	for i, k := range names {
		fn(k, outs[i])
	}
}

// Clone creates a copy of the WorkflowData whose CANONICAL value graph is fully isolated
// from the source. The clone gets its own metrics collector and string interner to avoid
// shared mutable state.
//
// # Isolation contract (AUD-013/CUR-003)
//
// Clone DEEP-COPIES the canonical value algebra recursively and cycle-safely: scalars
// (string/int64/float64/bool/…), map[string]interface{}, and []interface{} — INCLUDING
// nested slices and slices nested inside maps and vice-versa. Mutating any of those in the
// source after Clone never affects the clone, and identity cycles (a map or slice reachable
// from itself) terminate and close on the CLONE.
//
// NON-canonical values — typed maps (map[string]T), pointers, and custom structs — are
// RETAINED BY REFERENCE (shallow): the clone and source share that object, so mutating it
// through either is observed by both. This is deliberate: those shapes are outside the
// store's canonical value algebra and cannot durably persist (InMemoryStore.Save canonicalizes
// stored values; AUD-026), so a deep copy of them would isolate a value that can never cross
// the store boundary anyway. Treat non-canonical stored values as immutable, or convert them
// to the canonical algebra before relying on Clone isolation.
func (w *WorkflowData) Clone() *WorkflowData {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// The clone's interner is right-sized to the keys this clone actually holds,
	// NOT the eager 10k-capacity default (newStringInterner → make(map,10000) ≈
	// 641 KB/clone). InMemoryStore.Save→Clone() runs this per level, so the eager
	// pre-alloc was the inmem deep-1000 cost center (752 MB; M14 ph60/REM-01). A
	// clone's key universe is bounded by its data+status+output keys; sizing to
	// that keeps per-instance isolation (sound — never shared, internKey mutates
	// its cache on the hot path) while dropping the waste. commonStringsCount is
	// ignored by the underlying interner, so any value is fine.
	internCap := len(w.data) + len(w.nodeStatus) + len(w.outputs)

	// Preserve the SOURCE's metrics enable-STATE (N1). The prior code always built
	// a fresh ENABLED collector (metrics.NewMetricsCollector sets Enabled=true), so
	// a clone of metrics-DISABLED data silently became enabled, and an enabled
	// source's collection state was not carried honestly. Rebuilding from the
	// source's own config makes the clone track the source: disabled→disabled
	// (the frozen fast path stays a no-op, 0 alloc), enabled→enabled. NOTE: only the
	// enabled STATE carries — NewMetricsCollectorWithConfig Reset()s the counters, so
	// the clone starts at zero stats and re-accrues; a resumed enabled run measures
	// its own drive (not cumulative across resumes). That is the property bite #5
	// checks: the resumed run's tracking is not silently skipped.
	clone := &WorkflowData{
		mu:             &sync.RWMutex{}, // M24: a clone is an isolated snapshot -> its OWN lock (never shared)
		ID:             w.ID,
		metrics:        metrics.NewMetricsCollectorWithConfig(w.metrics.GetConfig().GetInternalConfig()),
		nodeStatus:     make(map[string]NodeStatus, len(w.nodeStatus)),
		waits:          make(map[string]int64, len(w.waits)),
		rollingBack:    w.rollingBack,  // M12: run-level saga marker (value type — a full copy)
		triggerCause:   w.triggerCause, // M12 ph49: durable rollback trigger cause (value type)
		stringInterner: newStringInternerWithCapacity(internCap, 0),
	}

	// Copy data and outputs DEEPLY (AUD-013/AUD-014/C-13): a Clone is contracted
	// as a deep copy and InMemoryStore.Save/Load both rely on it for snapshot
	// isolation. A shallow entry copy (clone.data[k] = v) aliases nested
	// maps/slices, so a caller mutating a nested value after Save (or after Load)
	// silently rewrote the stored snapshot. cloneMap is the cycle- and depth-safe
	// deep copier already used by ForEachOutput on this same value shape; reuse it
	// so both the accessor OUT and the snapshot path defend against aliasing
	// identically. cloneMap returns a non-nil map for the (always non-nil) fields
	// here, preserving the "maps are never nil" invariant.
	//
	// Each field is written EXACTLY ONCE (guard the nil case on a LOCAL, then a
	// single field assignment). VB-09's complete-mediation golden counts every
	// write to the outputs field outside the executor; a second write here (e.g.
	// an `if clone.outputs == nil { clone.outputs = make(...) }` guard on the field
	// itself) would add a spurious member to that set and red the golden for a
	// benign copy-construction. cloneMap returns nil only for a nil input, and both
	// fields are always non-nil on a live WorkflowData, so the guard is defensive.
	dataCopy := cloneMap(w.data)
	if dataCopy == nil {
		dataCopy = make(map[string]interface{})
	}
	clone.data = dataCopy

	outputsCopy := cloneMap(w.outputs)
	if outputsCopy == nil {
		outputsCopy = make(map[string]interface{})
	}
	clone.outputs = outputsCopy

	// Copy node statuses (NodeStatus is a value type — a shallow copy is a full copy).
	for k, v := range w.nodeStatus {
		clone.nodeStatus[k] = v
	}

	// Copy durable timer waits (M10) — int64 is a value type, so a shallow copy
	// is a full copy (no shared mutable state, matching the clone contract).
	for k, v := range w.waits {
		clone.waits[k] = v
	}

	return clone
}

// GetBool gets a boolean value from the workflow data
func (w *WorkflowData) GetBool(key string) (bool, bool) {
	var result bool
	var found bool
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		// No interning on the read path (see Get).
		val, ok := w.data[key]
		if !ok {
			return false, false
		}
		boolVal, ok := val.(bool)
		return boolVal, ok
	}
	w.metrics.TrackOperation(metrics.OpGetBool, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[key]
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
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		// No interning on the read path (see Get).
		val, ok := w.data[key]
		if !ok {
			return "", false
		}
		strVal, ok := val.(string)
		return strVal, ok
	}
	w.metrics.TrackOperation(metrics.OpGetString, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		val, ok := w.data[key]
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

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.getFloat64Internal(key)
	}

	// Track the operation with metrics
	w.metrics.TrackOperation(metrics.OpGetFloat64, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, found = w.getFloat64Internal(key)
	})

	return result, found
}

// getFloat64Internal performs the float64 coercion read. Caller must hold the
// read lock. No interning on the read path (see Get).
func (w *WorkflowData) getFloat64Internal(key string) (float64, bool) {
	val, ok := w.data[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	default:
		return 0, false
	}
}

// GetInt gets an int value from the workflow data.
//
// The result is the platform int. On 64-bit builds this carries any stored
// integer faithfully. On 32-bit builds, int is 32 bits, so a stored value
// outside the int32 range cannot be represented and the int64/int32 cases
// narrow it. Callers that must read values larger than MaxInt32 portably
// should use GetInt64, which returns the full int64 on every architecture.
func (w *WorkflowData) GetInt(key string) (int, bool) {
	var result int
	var found bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.getIntInternal(key)
	}

	w.metrics.TrackOperation(metrics.OpGetInt, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, found = w.getIntInternal(key)
	})
	return result, found
}

// getIntInternal performs the int coercion read. Caller must hold the read lock.
// No interning on the read path (see Get).
func (w *WorkflowData) getIntInternal(key string) (int, bool) {
	val, ok := w.data[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case int32:
		return int(v), true
	default:
		return 0, false
	}
}

// GetInt64 gets an integer value from the workflow data as an int64.
//
// Unlike GetInt, the result is an int64 on every architecture, so values
// larger than MaxInt32 are returned faithfully on 32-bit builds as well as
// 64-bit. It accepts values that were stored as int, int32, or int64.
func (w *WorkflowData) GetInt64(key string) (int64, bool) {
	var result int64
	var found bool

	// Metrics-free fast path when metrics are disabled or sampled out.
	if w.metricsDisabled() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.getInt64Internal(key)
	}

	w.metrics.TrackOperation(metrics.OpGetInt64, func() {
		w.mu.RLock()
		defer w.mu.RUnlock()
		result, found = w.getInt64Internal(key)
	})
	return result, found
}

// getInt64Internal performs the int64 coercion read. Caller must hold the read lock.
// No interning on the read path (see Get).
func (w *WorkflowData) getInt64Internal(key string) (int64, bool) {
	val, ok := w.data[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	default:
		return 0, false
	}
}

// DataFileOption configures a WorkflowData file operation. It exists so the
// SaveToJSON/LoadFromJSON pair can carry a size ceiling: these are store-less
// calls, so there is no store field for the two halves to share (HYG-00).
type DataFileOption func(*dataFileConfig)

// dataFileConfig carries the ceilings a SaveToJSON/LoadFromJSON pair enforces. Both
// halves resolve it through resolveDataFileConfig, which is what keeps them in step.
type dataFileConfig struct {
	maxFileSize int64
	maxElements int
}

// WithDataFileMaxSize sets the size ceiling for a SaveToJSON or LoadFromJSON call.
//
// Pass the SAME value to both halves. Raising it on LoadFromJSON is the supported
// way to read back a file that already exceeds the default ceiling — without it,
// guarding SaveToJSON would turn a recoverable-with-effort file into an
// unrecoverable one. A non-positive n is ignored and the default retained.
func WithDataFileMaxSize(n int64) DataFileOption {
	return func(c *dataFileConfig) {
		if n > 0 {
			c.maxFileSize = clampCeiling(n)
		}
	}
}

// WithDataFileMaxElements sets the per-section element ceiling for a SaveToJSON or
// LoadFromJSON call.
//
// Pass the SAME value to both halves. Over-count state is typically far UNDER the byte
// ceiling, so WithDataFileMaxSize cannot rescue it — this is its own axis with its own
// recovery path. A non-positive n is ignored and the default retained.
func WithDataFileMaxElements(n int) DataFileOption {
	return func(c *dataFileConfig) {
		if n > 0 {
			c.maxElements = n
		}
	}
}

// resolveDataFileMaxSize is the SINGLE resolution point for the ceiling, and both
// halves of the SaveToJSON/LoadFromJSON pair MUST route through it.
//
// The stores keep the two sides in agreement structurally — one field, read by
// both — but that argument does not transfer here: these are two independent
// calls with no store to hold a field. This helper is the equivalent guarantee,
// and TestSizeCap_DataFileSymmetry_BothHalvesHonourOption is what makes it hold:
// if either half stops calling this, that test goes red.
func resolveDataFileConfig(opts []DataFileOption) dataFileConfig {
	cfg := dataFileConfig{maxFileSize: defaultMaxFileSize, maxElements: defaultMaxElements}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// SaveToJSON saves the workflow data to a JSON file.
//
// The write is atomic (temp file + fsync + rename), so a crash mid-write leaves the
// previous file intact rather than a truncated one. Two consequences of that, both
// intended: an existing file's permission bits are PRESERVED (a new file gets 0600),
// and if the path is a SYMLINK it is REPLACED rather than written through. The latter
// is deliberate hardening — writing through a symlink is a classic escalation vector,
// since whoever controls the link controls the destination.
//
// Refuses to write more than the resolved ceiling (default: defaultMaxFileSize),
// because LoadFromJSON has always enforced that same ceiling on the way back in —
// writing past it produced a file this package could never read again (HYG-00).
// Pass WithDataFileMaxSize to raise it, and pass the same value to LoadFromJSON.
func (w *WorkflowData) SaveToJSON(filePath string, opts ...DataFileOption) error {
	// Create a snapshot of the data
	data, err := w.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Refuse to write state LoadFromJSON could never read back, on BOTH axes it
	// enforces. Same shared resolution both halves use.
	cfg := resolveDataFileConfig(opts)
	if err := checkWriteSize(int64(len(data)), cfg.maxFileSize, w.GetWorkflowID()); err != nil {
		return err
	}
	if err := checkWriteElements(w.maxSectionCount(), cfg.maxElements, w.GetWorkflowID()); err != nil {
		return err
	}
	// Third axis: nesting depth. Measured on the ENCODED document, not on any value
	// inside it — the encoding is what LoadFromJSON's decoder walks. Unlike the other
	// two this ceiling has no option, because it is encoding/json's decoder limit
	// rather than ours; a deeper document is unreadable by any configuration.
	if err := checkJSONDepth(data, w.GetWorkflowID()); err != nil {
		return err
	}

	// Write atomically (temp + fsync + rename), the same torn-write guard the four
	// other snapshot writers use. This was the ONE write path still on a plain
	// os.WriteFile, so a crash mid-write could leave a truncated file that
	// LoadFromJSON then rejects — the same "durable state you cannot read back"
	// family this guard exists to close, arriving by a different route.
	//
	// PRESERVE an existing file's mode. The atomic write creates a fresh temp file
	// and renames over the target, so without this a consumer's deliberate chmod
	// would be silently reset to 0600 — and this format has a known out-of-tree
	// consumer who would get no signal that it happened. Absent file → 0600, the
	// unchanged default for the create case. The mode is carried through verbatim,
	// never widened beyond what was observed.
	//
	// The Stat→rename gap is a benign TOCTOU: the worst case is that we write the
	// mode the file had a moment earlier. We never compute a broader mode than we
	// observed, so the gap cannot escalate permissions.
	perm := os.FileMode(0600)
	if fi, serr := os.Stat(filePath); serr == nil {
		perm = fi.Mode().Perm()
	}
	if err := writeFileAtomic(filePath, data, perm); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// LoadFromJSON loads the workflow data from a JSON file
// Pass WithDataFileMaxSize to raise the ceiling — that is how a file already on
// disk above the default is read back (the recovery path for anything written
// before SaveToJSON refused to exceed it). Use the same value on both halves.
func (w *WorkflowData) LoadFromJSON(filePath string, opts ...DataFileOption) error {
	// Bounds guard: read through io.LimitReader(cap+1) and reject over-cap input
	// as ErrCorruptData, symmetric with the FlatBuffers/store Load paths. Bounds
	// memory regardless of on-disk size; cap+1 distinguishes at-cap from over-cap.
	// Same resolution helper SaveToJSON uses — that shared call is what keeps the
	// two halves from drifting apart.
	cfg := resolveDataFileConfig(opts)
	data, err := readBoundedFileCapped(filePath, cfg.maxFileSize)
	if err != nil {
		return fmt.Errorf("failed to read JSON file: %w", err)
	}

	// Load snapshot
	err = w.loadSnapshotBounded(data, cfg.maxElements)
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

	// No interning on the read path (see Get).
	_, exists := w.data[key]
	return exists
}

// WorkflowDataConfig represents the configuration for workflow data
// ... existing code ...

// cloneMap deep-copies a value map. 116-AF4b: it used to self-recurse with no cycle
// handling, so an ordinary self-referential output —
//
//	m := map[string]any{}; m["self"] = m
//	data.SetOutput("n", m); store.Save(data)
//
// — died with `fatal error: stack overflow`, unrecoverable, through the PUBLIC API in
// two lines. Every frame in the measured crash stack was cloneMap; json.Marshal and
// fmt were never reached, which is why fixing the four %v fallback sites (AF4a) did
// not close this. ForEachOutput deep-copies every map-valued output through here.
//
// THE FIX IS TO HANDLE THE CYCLE, NOT TO REFUSE IT, and that is not a shortcut — it is
// what a deep copy MEANS. A faithful copy of a cyclic structure is itself cyclic. The
// standard algorithm is a path-local visited map from ORIGINAL map identity to its
// clone, which:
//
//   - terminates by construction, with no depth bound and no arbitrary constant;
//   - preserves semantics exactly — the clone's "self" points at the CLONE, which is
//     what a caller asking for a deep copy means;
//   - keeps this function returning a map, so ForEachOutput stays infallible and no
//     caller up to Save needs a new error path.
//
// A depth bound would have been the wrong verb here. With no error return, tripping it
// could only truncate or panic: silent data loss, or a different crash. Refusal is for
// guards; this is a transformation.
func cloneMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}

	root := make(map[string]interface{}, len(m))

	// ITERATIVE, with explicit worklists of (original, clone) pairs — one for maps, one
	// for slices. The visited maps below kill the CYCLE vector; they do nothing about the
	// DEPTH one. A 4,000,000-deep ACYCLIC chain still overflowed the stack with a recursive
	// clone (measured: survives 1.5M, dies at 4M on darwin/arm64, go1.25.1 — at 512 MiB of
	// USABLE stack, not the "1 GB" a prior comment said: Go grows stacks by DOUBLING, so the
	// usable limit is the largest power of two <= the configured one, and the 1e9 default
	// cannot reach 1 GiB. The band is right; only its stated condition was wrong), and no
	// upstream guard fires for it — ForEachOutput runs before and independently of any
	// save-time check, so the clone happens first. Going iterative makes depth cost heap
	// instead of stack and the vector disappears rather than moving. A slice chain
	// ([]any{[]any{[]any{...}}}) has the SAME depth axis and shares the same iterative drain.
	//
	// The shell-first shape is what makes this work: a clone container is created and
	// registered BEFORE its contents are filled, so a value reachable from its own subtree
	// links to the in-progress clone. That is the recursive version's "register before
	// descending", expressed as explicit queues.
	//
	// SCOPE (AUD-013/CUR-003): this deep-copies the CANONICAL value algebra fully and
	// cycle-safely — scalars, map[string]interface{}, and []interface{} (INCLUDING nested
	// slices, keyed on slice identity). NON-canonical values (typed maps map[string]T,
	// pointers, custom structs) hit the default arm and are RETAINED BY REFERENCE: they are
	// outside the store's canonical algebra and cannot cross the durable boundary anyway
	// (InMemoryStore.Save canonicalizes; AUD-026). See the Clone godoc for the honest
	// contract.
	type mapPair struct {
		src map[string]interface{}
		dst map[string]interface{}
	}
	type slicePair struct {
		src []interface{}
		dst []interface{}
	}

	// All allocated LAZILY. seenMap/mapWork on the first nested MAP; seenSlice/sliceWork on
	// the first slice that actually CONTAINS a container (a flat slice — all scalars — is
	// copied inline and never registers). A flat map, or a map holding only flat slices, is
	// the overwhelmingly common case and ForEachOutput runs this per output, so an
	// unconditional make() would be a per-call allocation on the hot path — precisely what
	// this package's CI-blocking alloc ceiling (TestPerfCeiling_DetTax) exists to catch.
	// Measured: a flat map is unchanged at 2 allocs/op.
	var seenMap map[uintptr]map[string]interface{}
	var seenSlice map[sliceKey][]interface{}
	var mapWork []mapPair
	var sliceWork []slicePair

	// ensureMap registers `dst` as the clone of a nested `src` map and queues it for
	// filling. Returns the clone to link, creating it only if this src has not been seen.
	ensureMap := func(src map[string]interface{}) map[string]interface{} {
		if seenMap == nil {
			seenMap = make(map[uintptr]map[string]interface{}, 4)
			seenMap[mapIdentity(m)] = root
		}
		if prior, ok := seenMap[mapIdentity(src)]; ok {
			return prior // cycle, or a shared subtree: link, do not re-clone
		}
		dst := make(map[string]interface{}, len(src))
		seenMap[mapIdentity(src)] = dst
		mapWork = append(mapWork, mapPair{src: src, dst: dst})
		return dst
	}

	// cloneSlice deep-copies a []interface{} value.
	//
	// A slice with NO container elements cannot participate in a cycle (nothing reachable
	// from it points back at it) and is copied INLINE without touching seenSlice — this is
	// what keeps "a map holding only flat slices" off the slice-seen allocation and protects
	// the det-tax ceiling. Only a slice that actually holds a nested map/slice is registered
	// (lazy seenSlice) and queued, giving it the SAME shell-first cycle safety the map path
	// has: a self-referential slice or a map->slice->map cycle links to the in-progress
	// clone. The empty-slice case is guarded and never registered — it has no elements to
	// alias and, worse, every empty non-nil slice shares one backing pointer
	// (runtime.zerobase), so keying them by identity would wrongly collapse unrelated empties.
	cloneSlice := func(src []interface{}) []interface{} {
		if len(src) == 0 {
			return make([]interface{}, len(src))
		}
		hasContainer := false
		for _, item := range src {
			switch item.(type) {
			case map[string]interface{}, []interface{}:
				hasContainer = true
			}
			if hasContainer {
				break
			}
		}
		if !hasContainer {
			dst := make([]interface{}, len(src))
			copy(dst, src) // all scalars: a value copy is a full deep copy
			return dst
		}
		if seenSlice == nil {
			seenSlice = make(map[sliceKey][]interface{}, 4)
		}
		// Key on (backing-array pointer, LEN), not the pointer alone: two overlapping
		// same-start sub-slices of DIFFERENT lengths (s[:1] and s[:2]) share a backing
		// pointer but are distinct slices — keying on the pointer alone collapses them to
		// one clone, producing a wrong-length, wrongly-aliased result that Save would
		// persist. (ptr,len) uniquely identifies a slice's element view, and a genuine
		// cycle re-encounters an identical (ptr,len), so cycle-safety is preserved.
		id := sliceKey{ptr: sliceIdentity(src), n: len(src)}
		if prior, ok := seenSlice[id]; ok {
			return prior // cycle, or a shared container slice: link, do not re-clone
		}
		dst := make([]interface{}, len(src))
		seenSlice[id] = dst
		sliceWork = append(sliceWork, slicePair{src: src, dst: dst})
		return dst
	}

	// Fill the root inline, then drain both worklists. Draining maps before slices is
	// arbitrary — correctness needs only that every queued job runs, since each container's
	// clone shell is registered at enqueue time.
	mapCur := mapPair{src: m, dst: root}
	fillMap := true
	for {
		if fillMap {
			for k, v := range mapCur.src {
				switch val := v.(type) {
				case map[string]interface{}:
					mapCur.dst[k] = ensureMap(val)
				case []interface{}:
					mapCur.dst[k] = cloneSlice(val)
				default:
					mapCur.dst[k] = v
				}
			}
			fillMap = false
		}
		if n := len(mapWork); n > 0 {
			mapCur = mapWork[n-1]
			mapWork = mapWork[:n-1]
			fillMap = true
			continue
		}
		if n := len(sliceWork); n > 0 {
			sc := sliceWork[n-1]
			sliceWork = sliceWork[:n-1]
			for i, v := range sc.src {
				switch val := v.(type) {
				case map[string]interface{}:
					sc.dst[i] = ensureMap(val)
				case []interface{}:
					sc.dst[i] = cloneSlice(val)
				default:
					sc.dst[i] = v
				}
			}
			continue
		}
		break
	}

	return root
}

// mapIdentity returns a stable per-map key for the visited set. Go maps are not
// comparable and cannot be map keys themselves, so identity is the map's underlying
// pointer: two references to the SAME map yield the same value, two structurally
// identical but distinct maps do not. That is exactly the identity a cycle check
// needs — structural equality would collapse legitimate repeated subtrees.
//
// The uintptr is used only for comparison within one synchronous clone; every map it
// names is reachable from the traversal for the whole call, so nothing can be moved or
// collected out from under it.
func mapIdentity(m map[string]interface{}) uintptr {
	return reflect.ValueOf(m).Pointer()
}

// sliceIdentity returns a stable per-slice key for the slice visited set: the address of
// the backing array's first element (reflect Pointer semantics for a slice). Two references
// to the SAME slice yield the same value; two structurally identical but distinct slices do
// not — exactly the identity a cycle check needs.
//
// It must NOT be called on an empty slice: every empty non-nil slice shares one backing
// pointer (runtime.zerobase), so keying empties by identity would collapse unrelated empty
// slices into one clone. cloneSlice guards this by handling len == 0 before ever reaching
// here — an empty slice has no elements to alias, so it needs no cycle tracking.
func sliceIdentity(s []interface{}) uintptr {
	return reflect.ValueOf(s).Pointer()
}

// sliceKey identifies a slice's element VIEW for cycle/dedup tracking: the backing-array
// start pointer AND the length. The length is load-bearing — sliceIdentity alone (the raw
// pointer) collapses overlapping same-start reslices of different lengths (s[:1], s[:2])
// onto one clone; (ptr,len) keeps them distinct while still re-identifying a genuine cycle.
type sliceKey struct {
	ptr uintptr
	n   int
}
