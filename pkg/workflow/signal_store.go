package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	flatbuffers "github.com/google/flatbuffers/go"
	fb "github.com/ppcavalcante/flow-orchestrator/internal/workflow/fb/workflow"
)

// Signal is one durable mailbox entry (M10 phase 37, wait-for-signal). The host
// delivers a Signal to a workflow; a WaitForSignalNode waiting on Name consumes
// it and applies Payload. ID is a host-supplied stable, unique-per-logical-event
// identifier (the inbound analog of IdempotencyKey): re-delivering the same ID is
// idempotent (one mailbox entry), and a consuming node's apply keyed off the
// node — not the signal — means a crash-resume re-applies byte-identically
// (D37-05, D37-06).
type Signal struct {
	ID      string // stable unique-per-logical-event id (host-supplied; dedupe key)
	Name    string // the signal name a WaitForSignalNode waits on
	Payload any    // arbitrary payload (JSON-encoded in the durable stores)
}

// SignalStore is an OPTIONAL interface a WorkflowStore MAY implement to carry a
// durable signal mailbox (M10 phase 37). It is additive and type-asserted exactly
// like Checkpointer: a Store that does not implement it simply offers no
// wait-for-signal capability. The mailbox is an INDEPENDENT channel — it lives
// OUTSIDE the WorkflowData snapshot (MH37-1) so an external deliverer's write can
// never clobber a running instance's checkpoint (the snapshot is rewritten
// wholesale at every checkpoint; a load-mutate-save deliverer would race it).
//
// Delivery splits from wake (D37-03): DeliverSignal is a durable enqueue that
// always succeeds regardless of process topology — the instance need not be
// loaded, running, or even exist yet (an early signal is buffered). Waking the
// workflow is the host re-invoking Workflow.Execute; there is no background
// scheduler.
//
// Consume ordering is the caller's (the executor's) responsibility and is the
// correctness core (D37-04): take (non-destructive) → idempotent apply → node
// Completed → checkpoint → THEN ack. TakeSignals is therefore non-destructive;
// AckSignals is the separate, after-durability drain.
//
// Host contract — mailbox bound (F37-LOW-1): a single workflow's mailbox holds at
// most defaultMaxElements (2^20) un-acked entries. The host is responsible for
// acking consumed signals promptly (the consume ordering above); a backlog that
// exceeds the bound is a host-contract violation and TakeSignals rejects it with
// ErrCorruptData rather than driving an unbounded allocation. This mirrors the
// element-count cap the snapshot decode enforces on its FlatBuffers vectors — the
// store defends the read path; the host must not over-deliver.
type SignalStore interface {
	// DeliverSignal durably enqueues sig for workflowID. It is idempotent by
	// sig.ID (re-delivering the same ID leaves one entry) and rejects an empty
	// sig.ID. It succeeds with no process running and whether or not the instance
	// exists (early-signal buffering).
	DeliverSignal(workflowID string, sig Signal) error

	// TakeSignals returns the currently-buffered signals for workflowID WITHOUT
	// removing them (non-destructive — removal is AckSignals, after the consuming
	// completion is durable). An empty mailbox returns an empty slice, not an
	// error.
	TakeSignals(workflowID string) ([]Signal, error)

	// AckSignals removes the named signals (by ID) for workflowID. It is
	// best-effort and idempotent: acking an absent ID is not an error. Called
	// ONLY after the consuming node's Completed status is durably checkpointed.
	AckSignals(workflowID string, ids []string) error
}

// signalDirSuffix is the sibling-directory suffix for a workflow's durable
// mailbox in the file stores: <baseDir>/<workflowID>.signals/. Keeping signals in
// a sibling directory (not inside the <id>.json / <id>.fb snapshot file) is the
// on-disk realization of mailbox-outside-snapshot (MH37-1).
const signalDirSuffix = ".signals"

// signalFileSuffix is the per-signal entry filename suffix inside the mailbox dir.
const signalFileSuffix = ".sig"

// signalMailboxCap bounds the number of un-acked entries one workflow's durable
// mailbox may hold before a read rejects it (F37-LOW-1). It mirrors the snapshot
// decode's defaultMaxElements vector cap (workflow_store.go): the mailbox is an
// external-writable channel, so an unbounded deliverer of distinct sig.IDs could
// otherwise drive TakeSignals into an arbitrarily large alloc/iterate exactly like
// an over-long FlatBuffers vector. It is a package var (initialized to the same
// bound) ONLY so tests can lower it deterministically without materializing ~1M
// entries per store; production behavior is defaultMaxElements, unchanged.
var signalMailboxCap = defaultMaxElements

// validateSignalID rejects a sig.ID that is not a single safe path segment. The
// ID is host-supplied and becomes a FILENAME inside the mailbox directory, so the
// SAME path-traversal guard validateWorkflowID applies to workflow IDs must apply
// here — otherwise a sig.ID of "../../escape" would write outside the store root.
// (Hardening beyond D37-09, which named only workflowID: sig.ID is the other
// caller-supplied component joined onto a path.)
func validateSignalID(sigID string) error {
	if sigID == "" {
		return fmt.Errorf("%w: signal ID cannot be empty", ErrValidation)
	}
	if strings.ContainsRune(sigID, '/') ||
		strings.ContainsRune(sigID, os.PathSeparator) ||
		!filepath.IsLocal(sigID) ||
		filepath.Base(sigID) != sigID {
		return fmt.Errorf("%w: invalid signal ID %q: must be a single path segment with no separators or traversal", ErrValidation, sigID)
	}
	return nil
}

// marshalSignalPayload encodes a signal payload to a JSON string (the same
// convention NodeOutputEntry.output uses for node outputs). A nil payload encodes
// to the empty string.
func marshalSignalPayload(p any) (string, error) {
	if p == nil {
		return "", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode signal payload: %w", ErrValidation, err)
	}
	return string(b), nil
}

// unmarshalSignalPayload decodes a JSON-string payload back to any, using
// UseNumber so an int64 payload round-trips at full magnitude (the same fidelity
// guard the snapshot load path uses — see workflow_data.go). The empty string
// decodes to nil.
func unmarshalSignalPayload(s string) (any, error) {
	if s == "" {
		return nil, nil //nolint:nilnil // empty payload legitimately decodes to (nil payload, no error); a sentinel would be incorrect
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: corrupt signal payload: %w", ErrCorruptData, err)
	}
	return v, nil
}

// encodeSignalFB serializes a Signal as a standalone FlatBuffers buffer (the
// Signal table as its own root). payload is JSON-encoded into the FB string field
// (mirroring NodeOutputEntry.output), so the FB envelope carries id + name + a
// JSON payload — exercising the additive FB Signal type (MH37-9).
func encodeSignalFB(sig Signal) ([]byte, error) {
	payloadStr, err := marshalSignalPayload(sig.Payload)
	if err != nil {
		return nil, err
	}
	b := flatbuffers.NewBuilder(256)
	idOff := b.CreateString(sig.ID)
	nameOff := b.CreateString(sig.Name)
	payloadOff := b.CreateString(payloadStr)
	fb.SignalStart(b)
	fb.SignalAddId(b, idOff)
	fb.SignalAddName(b, nameOff)
	fb.SignalAddPayload(b, payloadOff)
	b.Finish(fb.SignalEnd(b))
	return b.FinishedBytes(), nil
}

// decodeSignalFB deserializes a Signal from its standalone FlatBuffers buffer.
//
// Totality hardening (ph37 review F1/AF1): FlatBuffers accessors index into the
// buffer using offsets read FROM the buffer with no bounds validation, so a
// corrupt / truncated / foreign .sig entry would otherwise PANIC — and the mailbox
// dir is external-writable by design, while the executor runs nodes in goroutines
// with no recover(), so an unguarded panic crashes the host drive. This mirrors the
// snapshot Load hardening (workflow_store.go): a min-length + root-offset pre-check
// rejects the common short/truncated shapes as a typed ErrCorruptData, and a
// recover() backstop covers the deep-offset cases the pre-check cannot — so a
// corrupt mailbox entry degrades to a clean error exactly like the JSON codec, never
// a host crash. (The Go flatbuffers runtime ships no Verifier.)
func decodeSignalFB(buf []byte) (sig Signal, err error) {
	defer func() {
		if r := recover(); r != nil {
			sig = Signal{}
			err = fmt.Errorf("%w: malformed FlatBuffers signal", ErrCorruptData)
		}
	}()
	if len(buf) < flatbuffers.SizeUOffsetT {
		return Signal{}, fmt.Errorf("%w: malformed FlatBuffers signal", ErrCorruptData)
	}
	if rootOffset := flatbuffers.GetUOffsetT(buf); uint64(rootOffset) >= uint64(len(buf)) {
		return Signal{}, fmt.Errorf("%w: malformed FlatBuffers signal", ErrCorruptData)
	}
	s := fb.GetRootAsSignal(buf, 0)
	id := string(s.Id())
	if id == "" {
		// A well-formed signal ALWAYS carries a non-empty ID (validateSignalID
		// rejects empty on deliver). An empty-ID decode means a malformed/forged
		// buffer that passed the offset pre-check but reads zero-value fields (e.g.
		// all-nul bytes with root offset 0) — reject as corrupt rather than
		// surfacing a phantom empty signal. (ph37 review F1 — the nul-bytes
		// silent-empty symptom the JSON codec already rejects via decode error.)
		return Signal{}, fmt.Errorf("%w: malformed FlatBuffers signal (empty id)", ErrCorruptData)
	}
	payload, perr := unmarshalSignalPayload(string(s.Payload()))
	if perr != nil {
		return Signal{}, perr
	}
	return Signal{ID: id, Name: string(s.Name()), Payload: payload}, nil
}

// signalWire is the JSON on-disk shape for JSONFileStore mailbox entries (the
// JSON store keeps signals human-readable, consistent with its snapshot format).
type signalWire struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Payload any    `json:"payload"`
}

// encodeSignalJSON serializes a Signal as a JSON object.
func encodeSignalJSON(sig Signal) ([]byte, error) {
	b, err := json.Marshal(signalWire(sig))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode signal: %w", ErrValidation, err)
	}
	return b, nil
}

// decodeSignalJSON deserializes a Signal from its JSON object, using UseNumber for
// int64 payload fidelity.
func decodeSignalJSON(buf []byte) (Signal, error) {
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.UseNumber()
	var w signalWire
	if err := dec.Decode(&w); err != nil {
		return Signal{}, fmt.Errorf("%w: corrupt signal: %w", ErrCorruptData, err)
	}
	return Signal(w), nil
}

// --- file-store shared mailbox helpers (codec injected) ---

// deliverSignalToDir is the shared file-store DeliverSignal: it guards both IDs,
// creates the mailbox dir, and atomically writes the (codec-encoded) entry. The
// filename is the sig.ID, so a re-delivery of the same ID overwrites the same file
// with identical bytes — idempotent by construction.
func deliverSignalToDir(baseDir, workflowID string, sig Signal, encode func(Signal) ([]byte, error)) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	if err := validateSignalID(sig.ID); err != nil {
		return err
	}
	encoded, err := encode(sig)
	if err != nil {
		return err
	}
	dir := filepath.Join(baseDir, workflowID+signalDirSuffix)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("%w: cannot create signal mailbox dir: %w", ErrIO, err)
	}
	path := filepath.Join(dir, sig.ID+signalFileSuffix)
	if err := writeFileAtomic(path, encoded, 0600); err != nil {
		return fmt.Errorf("%w: cannot persist signal: %w", ErrIO, err)
	}
	return nil
}

// takeSignalsFromDir is the shared file-store TakeSignals: non-destructive read of
// every entry in the mailbox dir, decoded via the injected codec, returned sorted
// by ID for deterministic iteration. A missing dir (no signals) returns empty.
func takeSignalsFromDir(baseDir, workflowID string, decode func([]byte) (Signal, error)) ([]Signal, error) {
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	dir := filepath.Join(baseDir, workflowID+signalDirSuffix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Signal{}, nil
		}
		return nil, fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, err)
	}
	// Entry-count cap (F37-LOW-1): reject an oversized mailbox BEFORE the alloc/
	// iterate below, mirroring the snapshot decode's defaultMaxElements guard. The
	// total dir-entry count is a conservative upper bound (a handful of crash-left
	// temp files can only inflate it by a tiny constant, far under the cap).
	if len(entries) > signalMailboxCap {
		return nil, fmt.Errorf("%w: signal mailbox entry count exceeds max", ErrCorruptData)
	}
	signals := make([]Signal, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), signalFileSuffix) {
			continue
		}
		raw, rerr := readBoundedFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("%w: cannot read signal entry %q: %w", ErrIO, e.Name(), rerr)
		}
		sig, derr := decode(raw)
		if derr != nil {
			return nil, derr
		}
		signals = append(signals, sig)
	}
	sort.Slice(signals, func(i, j int) bool { return signals[i].ID < signals[j].ID })
	return signals, nil
}

// ackSignalsInDir is the shared file-store AckSignals: removes the named entries
// (codec-independent). Best-effort and idempotent — an absent entry is not an
// error; only a real removal failure surfaces.
func ackSignalsInDir(baseDir, workflowID string, ids []string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	dir := filepath.Join(baseDir, workflowID+signalDirSuffix)
	for _, id := range ids {
		if err := validateSignalID(id); err != nil {
			return err
		}
		path := filepath.Join(dir, id+signalFileSuffix)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: cannot ack signal %q: %w", ErrIO, id, err)
		}
	}
	return nil
}

// removeSignalDir best-effort removes a workflow's ENTIRE durable mailbox dir
// (<id>.signals/), reclaiming both consumed-but-unacked and never-consumed signal
// entries. The file stores' Delete calls it so deleting a workflow reclaims its
// mailbox too — the mailbox is a sibling channel the snapshot Delete would
// otherwise orphan (ph37 review F2: there is no background GC; reclamation is owned
// by Delete). Idempotent: an absent dir is not an error.
func removeSignalDir(baseDir, workflowID string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(baseDir, workflowID+signalDirSuffix))
}

// --- InMemoryStore SignalStore impl (in-process mailbox) ---

// DeliverSignal durably (in-process) enqueues sig, deduplicated by sig.ID.
func (s *InMemoryStore) DeliverSignal(workflowID string, sig Signal) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	if err := validateSignalID(sig.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	box, ok := s.signals[workflowID]
	if !ok {
		box = make(map[string]Signal)
		s.signals[workflowID] = box
	}
	box[sig.ID] = sig // idempotent by ID
	return nil
}

// TakeSignals returns the buffered signals for workflowID (non-destructive),
// sorted by ID for deterministic iteration.
func (s *InMemoryStore) TakeSignals(workflowID string) ([]Signal, error) {
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	box := s.signals[workflowID]
	// Entry-count cap (F37-LOW-1) — same bound as the file stores, uniform contract.
	if len(box) > signalMailboxCap {
		return nil, fmt.Errorf("%w: signal mailbox entry count exceeds max", ErrCorruptData)
	}
	out := make([]Signal, 0, len(box))
	for _, sig := range box {
		out = append(out, sig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AckSignals removes the named signals for workflowID (idempotent).
func (s *InMemoryStore) AckSignals(workflowID string, ids []string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	box := s.signals[workflowID]
	if box == nil {
		return nil
	}
	for _, id := range ids {
		delete(box, id)
	}
	return nil
}

// --- JSONFileStore SignalStore impl (JSON sidecar mailbox) ---

// DeliverSignal durably enqueues sig as a JSON entry in <id>.signals/.
func (s *JSONFileStore) DeliverSignal(workflowID string, sig Signal) error {
	return deliverSignalToDir(s.baseDir, workflowID, sig, encodeSignalJSON)
}

// TakeSignals non-destructively reads the JSON mailbox for workflowID.
func (s *JSONFileStore) TakeSignals(workflowID string) ([]Signal, error) {
	return takeSignalsFromDir(s.baseDir, workflowID, decodeSignalJSON)
}

// AckSignals removes the named JSON entries for workflowID.
func (s *JSONFileStore) AckSignals(workflowID string, ids []string) error {
	return ackSignalsInDir(s.baseDir, workflowID, ids)
}

// --- FlatBuffersStore SignalStore impl (FB sidecar mailbox) ---

// DeliverSignal durably enqueues sig as an FB Signal entry in <id>.signals/.
func (s *FlatBuffersStore) DeliverSignal(workflowID string, sig Signal) error {
	return deliverSignalToDir(s.baseDir, workflowID, sig, encodeSignalFB)
}

// TakeSignals non-destructively reads the FB mailbox for workflowID.
func (s *FlatBuffersStore) TakeSignals(workflowID string) ([]Signal, error) {
	return takeSignalsFromDir(s.baseDir, workflowID, decodeSignalFB)
}

// AckSignals removes the named FB entries for workflowID.
func (s *FlatBuffersStore) AckSignals(workflowID string, ids []string) error {
	return ackSignalsInDir(s.baseDir, workflowID, ids)
}
