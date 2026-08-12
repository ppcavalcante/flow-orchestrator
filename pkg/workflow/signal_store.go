package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	// EnqueuedAt is the unix-nanos delivery time the durable store recorded for this signal,
	// exposed on the READ path (TakeSignals) so a consumer can reject a STALE buffered signal —
	// a freshness check on approvals, where an old queued decision must not satisfy a new wait
	// (AUD-025 / P-16). The SQLite store populates it from its signals.enqueued_at column; stores
	// that do not persist a delivery time (InMemory / file mailboxes) leave it 0 (= unknown).
	// It is READ-ONLY output: DeliverSignal ignores any value set here and stamps its own time.
	// (The full approval generation/nonce policy is a separate, larger security piece.)
	EnqueuedAt int64
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
//
// DeliverSignal enforces the same bound on the WRITE, so the mailbox cannot be driven
// over it through this interface. RESIDUAL, stated because it is a real cost and not a
// theoretical one: a mailbox that is ALREADY over the bound — state persisted before that
// write guard existed, state admitted by concurrent delivery before the guard was made to
// hold under it (fixed, but shipped briefly), an external writer to the mailbox directory or
// signals table which the M9 threat model says exist, or a concurrent delivery on a non-unix
// build where the file-store lock is a no-op — is recoverable **out of band only**.
// The cap is unexported, so a consumer cannot raise it; and AckSignals takes the IDs to
// drain, which the caller cannot enumerate, because TakeSignals is the only enumeration
// path and it is the call that is failing. Draining such a mailbox means going at the
// backing store directly: delete entry FILES under <baseDir>/<workflowID>.signals/ for
// the file stores, or rows from the signals table for SQLite.
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

// signalLockSuffix names the per-workflow mailbox lock file,
// <baseDir>/<workflowID>.signals.lock — a SIBLING of the mailbox dir, deliberately not a
// file inside it.
//
// WHY IT IS OUTSIDE THE DIRECTORY IT PROTECTS, and this is the whole point: flock(2) binds to
// an INODE, not to a path. removeSignalDir's os.RemoveAll destroys the mailbox directory's
// inode, so locking the directory itself let a Delete unlink the very object a parked
// deliverer held. The next deliverer MkdirAll'd a NEW inode and flocked that — two writers
// holding "the" lock on two different objects, writing through one path. Reproduced: a parked
// re-delivery, a Delete, a refill to cap, then the parked rename landing on top — 5 entries
// against a cap of 4, TakeSignals ErrCorruptData, and the deliverer reported success.
//
// A sibling file is not reachable by os.RemoveAll(<id>.signals), so its inode is stable for
// the lifetime of the store directory.
//
// NAME COLLISION IS UNREPRESENTABLE, and this needed checking rather than assuming because
// validateWorkflowID PERMITS '.' — it rejects only separators, traversal and non-local paths,
// so a workflow may legally be named "foo.signals". Both directions were worked through:
//
//	lock file aliases another mailbox dir?  needs id2+".signals" == id+".signals.lock",
//	  i.e. ".signals.lock" to end in ".signals" — it ends in ".lock".
//	mailbox dir aliases another lock file?  needs id+".signals" == id2+".signals.lock" with
//	  len(id) == len(id2)+5, which forces the 5-char bridge to be ".sign" and then
//	  ".sign"+".signals" == ".sign.signals" != ".signals.lock".
//
// Neither holds, so a third suffix adds no collision the first two did not already have.
//
// THE LOCK FILE IS NEVER DELETED, and that is load-bearing rather than an oversight. Removing
// it is precisely the inode-destruction bug again: a Delete that unlinked it while another
// process held it would hand the next deliverer a fresh inode, restoring the class this
// constant exists to close. It cannot be removed safely even under the lock, because proving
// nobody holds it requires holding it.
//
// WHO CREATES IT, stated exactly because the first version of this sentence was FALSE and the
// error was mine. It said "one per workflow that ever received a signal". At the time,
// removeSignalDir also took the lock with O_CREATE, so it was also one per workflow ever
// DELETED and per FAILED delete — five Deletes of nonexistent ids left five files, and a Delete
// of a signal-less workflow turned [wf.json] into [wf.signals.lock]. Unbounded in the ids a
// caller ever passes to Delete, on the one API whose job is reclamation. Fixed: only the
// DELIVERY path creates (lockMailboxDir's create parameter), and Delete skips when absent.
//
// So, precisely: one zero-byte file per workflow for which a delivery was ever ATTEMPTED past
// validation — INCLUDING a delivery later refused for exceeding the cap, because the lock is
// taken before the count and that ordering is what makes the count correct. Delete does not
// reclaim it. That is the whole population; nothing else in the package creates one.
//
// It is invisible to the stores' own listing, which globs *.json and *.fb (workflow_store.go),
// so it cannot appear as a phantom workflow.
const signalLockSuffix = ".signals.lock"

// signalMailboxCap bounds the number of un-acked entries one workflow's durable
// mailbox may hold before a read rejects it (F37-LOW-1). It mirrors the snapshot
// decode's defaultMaxElements vector cap (workflow_store.go): the mailbox is an
// external-writable channel, so an unbounded deliverer of distinct sig.IDs could
// otherwise drive TakeSignals into an arbitrarily large alloc/iterate exactly like
// an over-long FlatBuffers vector. It is a package var (initialized to the same
// bound) ONLY so tests can lower it deterministically without materializing ~1M
// entries per store; production behavior is defaultMaxElements, unchanged.
//
// It is the SINGLE source of truth for this axis, read by SIX sites — three reads
// (takeSignalsFromDir, InMemoryStore.TakeSignals, SQLiteStore.TakeSignals) and three
// writes (deliverSignalToDir, InMemoryStore.DeliverSignal, SQLiteStore.DeliverSignal).
// One variable rather than a per-store field is deliberate: the bound is a HOST
// CONTRACT declared on the SignalStore interface above, uniform across every
// implementation, so drift between a writer and its reader is unrepresentable here
// rather than merely tested against (contrast the byte axis, which needed a per-store
// field plus a symmetry test because the ceiling is per-store format state).
var signalMailboxCap = defaultMaxElements

// checkMailboxEntries rejects a delivery that would leave a workflow's mailbox
// holding more entries than a read will accept — the element-count twin of
// checkWriteSize on the signal channel (F1).
//
// n is the count the mailbox would hold AFTER the delivery. Before this, only the
// READ side enforced signalMailboxCap: every DeliverSignal returned nil, and then
// TakeSignals rejected the whole mailbox with ErrCorruptData, permanently. The
// mailbox read is all-or-nothing, so a single over-cap backlog fails a WaitForSignal
// run's take on every re-drive until the mailbox is drained out of band, and no knob
// reached this quantity to rescue it.
//
// ErrValidation, not ErrCorruptData, matching checkWriteSize/checkWriteElements:
// nothing is corrupt, the host over-delivered against the contract it was given. The
// message names the resulting count AND the ceiling so an operator can see how far
// over the bound the mailbox is without guessing.
//
// There is no knob to raise this ceiling. The decision has been re-derived twice, because
// TWO of the reasons first given for it turned out to be false, and it is worth recording
// which survived:
//
//	DEAD - "AckSignals drains an over-cap mailbox, so no knob is needed." False: AckSignals
//	  takes the ids to remove, and TakeSignals - the only enumeration path - is the call that
//	  is failing. A consumer restarting on top of one holds no ids.
//	DEAD - "once this guard exists the over-cap state is unreachable through the API at all."
//	  False: the guard did not hold under concurrent delivery. It does now, but "unreachable"
//	  was never a property of the guard's existence, only of its correctness, and it is not
//	  the kind of claim that should carry a design decision.
//	HOLDS - signalMailboxCap is defaultMaxElements = 2^20: an absurdity ceiling, not a tuning
//	  parameter a legitimate consumer reaches. This is the real disanalogy with the 64 MiB
//	  byte ceiling, which an ordinary large snapshot DOES reach - which is why that axis
//	  needed WithJSONMaxFileSize and this one does not.
//	HOLDS - the bound is an interface-level host contract, uniform across implementations, so
//	  splitting it into per-store options would fragment one guarantee into four.
//
// Two of four reasons were false and the conclusion survived on the other two. That is the
// shape this project banked as "a TRUE conclusion defended by a FALSE premise survives review
// on the conclusion's strength" - twice in one phase, in this file. The two that hold are
// stated positively above so a future reader inherits the argument, not the corpse.
func checkMailboxEntries(n, ceiling int, workflowID string) error {
	if n > ceiling {
		return fmt.Errorf("%w: workflow %q signal mailbox would hold %d entries, exceeds the %d-entry max mailbox size; ack consumed signals to drain it",
			ErrValidation, workflowID, n, ceiling)
	}
	return nil
}

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
	// AF2: the CRASH axis, BEFORE the marshal. The checkJSONDepth below runs on bytes
	// that only exist if json.Marshal returned — and on a deep enough payload it does not
	// return, it kills the process. DeliverSignal is a public API taking `any`, so this
	// is the shortest path from a host value to the encoder in the whole package.
	if err := checkValueDepth(p, "signal payload"); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode signal payload: %w", ErrValidation, err)
	}
	// Nesting-depth axis (F2): this string is decoded back by unmarshalSignalPayload
	// through json.Decoder, whose scanner caps nesting; json.Marshal does not. Guard
	// here rather than at each caller so the FlatBuffers store (encodeSignalFB) and
	// the SQLite store (DeliverSignal), which share this one encoder, cannot diverge.
	if err := checkJSONDepth(b, "signal payload"); err != nil {
		return "", err
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
	fb.SignalAddEnqueuedAt(b, sig.EnqueuedAt) // CUR-001/AUD-025: preserve the store-stamped delivery time
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
	// EnqueuedAt() reads 0 for a pre-CUR-001 buffer (the field was appended last), which
	// decodes as "unknown" — a freshness-enforcing consumer treats 0 as reject, matching the
	// documented contract for the other backends.
	return Signal{ID: id, Name: string(s.Name()), Payload: payload, EnqueuedAt: s.EnqueuedAt()}, nil
}

// signalWire is the JSON on-disk shape for JSONFileStore mailbox entries (the
// JSON store keeps signals human-readable, consistent with its snapshot format).
type signalWire struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Payload any    `json:"payload"`
	// EnqueuedAt mirrors Signal.EnqueuedAt (AUD-025). Kept field-identical to Signal so the
	// signalWire(sig)/Signal(w) struct conversions stay valid. omitempty + the read tolerating
	// its absence make it backward-compatible: a pre-existing mailbox file without the field
	// decodes to 0 (= unknown), exactly as an un-stamped delivery does.
	EnqueuedAt int64 `json:"enqueued_at,omitempty"`
}

// encodeSignalJSON serializes a Signal as a JSON object.
func encodeSignalJSON(sig Signal) ([]byte, error) {
	// AF2: the CRASH axis, BEFORE the marshal — and in this encoder's own right rather
	// than by way of marshalSignalPayload, because THIS store does not call it. The JSON
	// store marshals the whole signalWire with the payload still a live Go value, so a
	// guard sitting only in the payload encoder covers the FlatBuffers and SQLite stores
	// and leaves this one crashing. That is the "closes on the fourth writer" shape.
	if err := checkValueDepth(sig.Payload, fmt.Sprintf("payload of signal %q", sig.ID)); err != nil {
		return nil, err
	}
	b, err := json.Marshal(signalWire(sig))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode signal: %w", ErrValidation, err)
	}
	// Nesting-depth axis (F2), measured on the WHOLE wire object because that is what
	// decodeSignalJSON hands the decoder. The signalWire envelope costs one level, so
	// this store's usable payload depth is one shallower than the FlatBuffers/SQLite
	// stores', which encode the payload standalone — measuring the payload alone would
	// have been off by exactly that envelope and left the wedge open here.
	if err := checkJSONDepth(b, fmt.Sprintf("signal %q", sig.ID)); err != nil {
		return nil, err
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
// ceiling bounds the encoded entry: TakeSignals reads every entry back through the
// same bound, and ONE over-ceiling entry fails the read of the WHOLE mailbox, so an
// unguarded delivery could permanently strand a WaitForSignal run (HYG-00, F1).
// Signal.Payload is host-supplied `any`, which is what makes the size unbounded.
// G703 DISPOSITION for this function's filesystem calls: gosec's taint tracker follows
// workflowID and sig.ID to the path sinks below and cannot model the validators that sanitize
// them. Both are rejected unless they are a single safe path segment - validateWorkflowID and
// validateSignalID each refuse empty, separators, os.PathSeparator, non-local paths
// (filepath.IsLocal) and anything failing a filepath.Base round-trip - so traversal out of
// baseDir is unrepresentable here. Same confirmed false positive already dispositioned on
// ackSignalsInDir and removeSignalDir. These sites were G703-clean until an extra os.Stat was
// added in the qa-2 round; the analyzer is threshold-sensitive on this chain, so the directives
// are per-site rather than a blanket file exclusion.
func deliverSignalToDir(baseDir, workflowID string, sig Signal, encode func(Signal) ([]byte, error), ceiling int64) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	if err := validateSignalID(sig.ID); err != nil {
		return err
	}
	sig.EnqueuedAt = time.Now().UnixNano() // AUD-025: stamp delivery time (store owns it; re-deliver refreshes)
	encoded, err := encode(sig)
	if err != nil {
		return err
	}
	// Refuse to write an entry that would poison the mailbox read.
	if err := checkWriteSize(int64(len(encoded)), ceiling, workflowID); err != nil {
		return err
	}
	dir := filepath.Join(baseDir, workflowID+signalDirSuffix)
	path := filepath.Join(dir, sig.ID+signalFileSuffix)

	// THERE IS NO LOCK-FREE FAST PATH FOR RE-DELIVERY, and the one that used to be here is
	// worth naming, because its premise reads as obviously true and is not.
	//
	// It was: `if os.Stat(path) == nil { writeFileAtomic; return }` — a re-delivery of an id
	// already in the mailbox overwrites in place, cannot grow it, and so needs neither a count
	// nor the lock. The premise "cannot grow it" holds only if the observed entry STILL EXISTS
	// when the rename lands, and nothing established that: ackSignalsInDir takes no lock and
	// just os.Removes. Reproduced deterministically (pause injected at the createTempFile seam,
	// so this is a schedule and not a timing race), mailbox exactly at cap 4:
	//
	//	1. G1 re-delivers s000, Stat hits, enters writeFileAtomic holding NO lock
	//	2. a consumer acks s000                                        -> 3 entries
	//	3. G2 delivers a NEW id: under the flock it counts 3, 3+1 <= 4 -> 4 entries
	//	4. G1's rename lands                                           -> 5 entries, over cap
	//
	// TakeSignals then returned "corrupt workflow data: signal mailbox entry count exceeds max"
	// — the permanent wedge this whole guard exists to prevent. The re-Stat under the lock below
	// guards the entry-APPEARED direction; nothing guarded entry-DISAPPEARED, and only the lock
	// can, because the check and the rename must be one atomic step against BOTH directions.
	//
	// Deleting it costs almost nothing: the locked path below already skips the ReadDir when the
	// entry is present, so a re-delivery stays O(1) in DIRECTORY work and pays only the
	// open+flock pair. RE-MEASURED after the deletion rather than carried over from the earlier
	// estimate: 26.4/27.0/23.7/22.4us at n=1/100/1000/10^4 — still FLAT in n, and ~0.2% of an
	// ~11.8ms delivery (BenchmarkMailboxGuardCost/redelivery has the table). That is the
	// identical trade this guard already made once for the new-entry path.
	//
	// This is the SECOND blocker on this one property, and both had the same shape: the guard's
	// correctness argument lived in a comment and was never checked against a schedule. A
	// comment asserting a property is a verification obligation — hence the regression test
	// named in the paragraph below, and hence these numbers being measurements.
	//
	// ONE COST THIS DOES CARRY, stated because "0.2%" does not convey it: re-deliveries to the
	// SAME mailbox now SERIALIZE on the flock, where the fast path let them proceed in parallel.
	// New-entry deliveries already serialized, so nothing changed there. Racing re-deliveries of
	// one id are last-writer-wins against a single file anyway, which is why serializing them
	// costs correctness nothing.
	//
	// THE INVARIANT THAT HOLDS, over the writers ENUMERATED MECHANICALLY rather than recalled.
	// An earlier version of this paragraph said "any concurrent AckSignals only REMOVES entries"
	// and named two writers. There are THREE, and the third is the one that broke it — the same
	// shape as round 2, where the defect was a writer nobody enumerated. The enumeration is
	// `grep -n signalDirSuffix` over the non-test sources, which yields exactly four sites:
	//
	//	deliverSignalToDir   WRITER, adds an entry   — takes the lock (here)
	//	ackSignalsInDir      WRITER, removes only    — no lock, and safe: removal cannot
	//	                                               push the mailbox OVER the cap
	//	removeSignalDir      WRITER, removes the DIR — takes the lock; it used to take none,
	//	                                               and it destroyed the lock's own inode
	//	takeSignalsFromDir   reader                  — no lock (see the transient below)
	//
	// Given that set: a writer that ADDS counts and writes under one lock, on a lock object no
	// writer can destroy; removals only ever decrease the count; and this writer's rename re-adds
	// at most the single path it evaluated under that lock. So
	//
	//	post-write entry set  ⊆  (set observed under the lock) ∪ {path}
	//
	// which the check bounded by the cap. Any other writer must take the same lock to count or to
	// write, and the lock file cannot be unlinked, so "the same lock" is the same INODE for every
	// holder — which is the part that was false before signalLockSuffix existed.
	//
	// On non-unix the lock is a no-op and ALL of this is best-effort — see
	// signal_store_lock_other.go, which enumerates both open classes.
	//
	// KNOWN, NOT CLOSED: takeSignalsFromDir takes no lock, and writeFileAtomic's temp file is
	// itself a directory entry, so a read landing inside a delivery's write window on a mailbox
	// at exactly the cap can see cap+1 and return a SPURIOUS ErrCorruptData. Transient (a retry
	// clears it), pre-existing, and filed as F-116-TRANSIENT-01 against M24 rather than fixed
	// here. Deleting the fast path made it LESS reachable, not more: N concurrent re-deliveries
	// used to hold N temp files at once, and now serialize to one.

	// LOCK FIRST — before the count, before MkdirAll, before anything touches the mailbox.
	// The lock lives on a sibling file that does not require the mailbox dir to exist, so the
	// ENTIRE delivery including directory creation is one critical section.
	//
	// THIS ORDERING CARRIES TWO SEPARATE PROPERTIES, and only one of them was written down. The
	// second is the more load-bearing of the pair, so it is stated first:
	//
	//	P1 — ACQUIRE BEFORE MkdirAll  ⟹  "lock file absent implies no mailbox work ever
	//	  happened", which is the ENTIRE SOUNDNESS OF removeSignalDir's SKIP. Delete opens the
	//	  lock file without O_CREATE and, finding it absent, proceeds unlocked. That is safe only
	//	  because a delivery materializes the lock file before it touches any directory, so an
	//	  absent lock file proves no delivery ever created a mailbox and none is in flight. If a
	//	  future change moved the acquire below MkdirAll, a delivery could sit between MkdirAll
	//	  and the acquire with the directory present and the lock file absent — and a concurrent
	//	  Delete would skip locking and os.RemoveAll it mid-write.
	//	P2 — MkdirAll AFTER THE VERDICT  ⟹  "a refused delivery leaves nothing behind", at every
	//	  cap, by construction.
	//
	// P1 IS IMPLIED BY THE OTHER TWO, which is worth knowing before trusting it. Writing the
	// constraints out — acquire < count (the count must be under the lock), count < MkdirAll
	// (P2) — P1 follows by transitivity, and both of those have behavioural tests. So P1 cannot
	// be silently broken while they pass; MEASURED, by moving the acquire below MkdirAll and
	// watching three other tests red. An earlier version of this note claimed P1 was unguarded,
	// which was wrong.
	//
	// What it lacks is not a guard but a DIAGNOSIS: those three tests report a cap breach and
	// say nothing about removeSignalDir's skip, so a reader fixes the count and never learns
	// that Delete can now RemoveAll a mailbox out from under a live delivery.
	// TestDeliverSignal_AcquiresBeforeMkdirAll exists to name that consequence.
	//
	// P2 also RETIRES a branch rather than documenting it. There used to be an absent-mailbox
	// special case here — os.Stat(dir) + checkMailboxEntries(1, cap) — whose only job was to
	// refuse before MkdirAll so a refused delivery left no empty directory behind. It was DEAD
	// IN PRODUCTION (it fires only when cap < 1, and the cap is 2^20) and existed to satisfy one
	// test, under a comment that read as if it closed the residue class. The dead branch is gone
	// and the residue class it half-covered is smaller — see the residue note below.
	//
	// Without the lock the guard does not hold at all: N concurrent deliveries of distinct new
	// ids each observed the same pre-count and all committed. Measured before the fix at cap 8
	// seeded with 7, 16 goroutines: 11-12 entries, and TakeSignals then rejected the whole
	// mailbox. The lock costs ~24us against an ~11.8ms delivery (~0.2%) — see lockMailboxDir.
	//
	// The lock is on the SIBLING lock file, never on dir: os.RemoveAll would otherwise destroy
	// the locked inode out from under a parked holder. See signalLockSuffix.
	//
	// LOCK ORDERING, stated because acquiring here creates the possibility of one. The file
	// stores' Delete takes s.mu and then this flock (via removeSignalDir), so the only ordering
	// that exists is s.mu -> flock. Delivery takes this flock and NO s.mu, so there is no cycle
	// and no hold-and-wait. THE RULE THIS IMPLIES: never take s.mu inside a delivery, and never
	// take this lock while already holding it. Nothing here runs host code — json.Marshal can
	// invoke a caller's MarshalJSON, but encode() completed above, before the acquire.
	//
	// On non-unix builds this lock is a no-op and the guard is best-effort; that residual
	// is stated in signal_store_lock_other.go, which enumerates all three open classes.
	unlock, lerr := lockMailboxDir(filepath.Join(baseDir, workflowID+signalLockSuffix), true)
	if lerr != nil {
		return lerr
	}
	defer unlock()

	// Stat UNDER the lock, and count only if the entry is genuinely new. This is what keeps a
	// re-delivery O(1) in directory work now that the lock-free fast path is gone: an existing
	// id cannot grow the mailbox, so it skips the ReadDir entirely. It must be read under the
	// lock in BOTH directions — a concurrent deliverer of the same id may have created the
	// entry (counting it as new would refuse a delivery that does not grow the mailbox), and a
	// concurrent ack may have removed one (see the fast-path autopsy above).
	//
	// An ABSENT mailbox reads as zero entries through os.ReadDir's IsNotExist, deliberately
	// rather than by short-circuiting: at a ceiling of 0 the first delivery must be refused,
	// because the READ side rejects any entry at that ceiling. Short-circuiting "no dir means
	// nothing to count, let it through" re-arms the write/read asymmetry at exactly one point.
	if _, serr := os.Stat(path); serr != nil { //nolint:gosec // G703 false positive: path = validated workflowID + validated sig.ID
		entries, rerr := os.ReadDir(dir)
		if rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, rerr)
		}
		if verr := checkMailboxEntries(len(entries)+1, signalMailboxCap, workflowID); verr != nil {
			return verr
		}
	}
	// MkdirAll AFTER the verdict, which is what makes "a refused delivery leaves nothing
	// behind" true by construction rather than by a special case.
	//
	// RESIDUE, what remains and what no longer does: a REFUSAL now never creates the directory
	// at any cap, and a lock-acquisition or ReadDir failure returns before this line, so
	// neither leaves residue either. ONE path still can — a writeFileAtomic failure after this
	// MkdirAll leaves an empty <id>.signals/. That residue is benign and deliberately not
	// cleaned up: a later delivery reuses the dir and TakeSignals reads an empty mailbox as an
	// empty slice, not an error (pinned by TestMailbox_EmptyMailboxDirResidueIsBenign).
	// Removing it would need an os.Remove on the error path, which can delete a directory a
	// CONCURRENT deliverer has MkdirAll'd and not yet written into — trading a harmless empty
	// directory for a spurious ErrIO on someone else's live delivery.
	if err := os.MkdirAll(dir, 0750); err != nil { //nolint:gosec // G703 false positive: dir = baseDir + validated workflowID
		return fmt.Errorf("%w: cannot create signal mailbox dir: %w", ErrIO, err)
	}
	if err := writeFileAtomic(path, encoded, 0600); err != nil {
		return fmt.Errorf("%w: cannot persist signal: %w", ErrIO, err)
	}
	return nil
}

// COST of the file-store entry-count guard — measured, and the band restated, because the
// first version of this note attached correct numbers to the WRONG band.
//
// Delivery was O(1) before this guard: the entry is written to a path built from sig.ID, so
// idempotency falls out of the filename and nothing ever scanned the directory. The scan
// makes a new-entry delivery O(N) and an N-fill O(N^2). On darwin/arm64 APFS:
//
//	mailbox n:      1       1000     10^4      10^5
//	the scan:      31us     799us    6.8ms     85.9ms
//	one delivery: ~9-12ms, n-independent (writeFileAtomic's fsync dominates)
//
// At 10^5 entries the scan is ~9x the rest of the delivery, and filling that mailbox is
// roughly 72 minutes of directory scanning against ~17 minutes of fsync.
//
// THE BAND THIS APPLIES TO IS INSIDE THE CONTRACT, NOT OUTSIDE IT. An earlier version of
// this comment called the quadratic band "sizes that are already a host-contract violation,
// which is what the guard is refusing". That was false: signalMailboxCap is 2^20, so
// 10^4-10^6 entries are EXPLICITLY LEGAL and this guard admits every one of them. The cap
// does not protect against the cost; it permits it. Accepted as a residual rather than
// fixed, because the fix is a cached count and that is not free — it would need invalidating
// from AckSignals and from every external writer to the dir, which the M9 threat model says
// exist. A host holding >10^4 un-acked signals for one workflow should ack them.
//
// Two cheap things ARE done. The Stat-under-the-lock keeps re-delivery O(1) in DIRECTORY work
// — an existing id skips the ReadDir. (It does NOT skip the lock. An earlier version skipped
// both, and that lock-free fast path was breachable by an ack racing it; the autopsy is in
// deliverSignalToDir. A re-delivery therefore pays the open+flock pair, ~20us, 0.17% of a
// delivery.) And os.ReadDir is kept over the ~20-30% faster Readdirnames, because the reader
// counts with os.ReadDir and counting the identical way is worth more than a sub-2%
// end-to-end gain — the writer must bound the same quantity the reader bounds.
//
// THAT SHARED QUANTITY HAS A CONSEQUENCE WORTH STATING, because both sides must say it and
// only the reader did (the write-side sentence was dropped in b315997 and is restored here):
// the count is ALL directory entries, so a crash-left temp file from an interrupted
// writeFileAtomic permanently consumes a cap slot. Measured: a mailbox holding 4 real entries
// plus one .tmp stub refuses the 5th delivery with "would hold 5 entries, exceeds the 4-entry
// max". TakeSignals skips non-.sig entries when decoding, so the read is unaffected and no
// wedge results — this is a slot-accounting conservatism, not a hazard. It is deliberate: the
// alternative is a writer that counts differently from its reader, which is the entire defect
// this guard exists to close.

// takeSignalsFromDir is the shared file-store TakeSignals: non-destructive read of
// every entry in the mailbox dir, decoded via the injected codec, returned sorted
// by ID for deterministic iteration. A missing dir (no signals) returns empty.
func takeSignalsFromDir(baseDir, workflowID string, decode func([]byte) (Signal, error), ceiling int64) ([]Signal, error) {
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
	//
	// THIS CHECK STAYS HERE, BEFORE THE LOOP, AND THAT POSITION IS LOAD-BEARING (AF1).
	// It is computed from the ReadDir snapshot, so a concurrent AckSignals removing
	// entries makes it stale-HIGH: it can refuse a mailbox that has since shrunk under
	// the cap. That is the conservative direction and it is deliberate. Now that a
	// vanished entry is tolerated below, the tempting cleanup is to count survivors
	// instead — which makes it stale-LOW and lets a genuinely over-cap mailbox through,
	// turning a refusal into an admission. Do not move it into the loop.
	if len(entries) > signalMailboxCap {
		return nil, fmt.Errorf("%w: signal mailbox entry count exceeds max", ErrCorruptData)
	}
	signals := make([]Signal, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), signalFileSuffix) {
			continue
		}
		// Same ceiling the delivery guard enforced - the mailbox pair is symmetric.
		raw, rerr := readBoundedFileCapped(filepath.Join(dir, e.Name()), ceiling)
		if rerr != nil {
			// AF1: ReadDir above and the open here are UNLOCKED, so a concurrent and
			// entirely legal AckSignals can remove an entry between the two. Before this,
			// that window failed the WHOLE take with ErrIO and lost every other signal in
			// the mailbox — a take is non-destructive, so the caller had no way to
			// recover the rest. ackSignalsInDir already swallows os.IsNotExist on the
			// removal side; the read treated the identical condition as storage failure.
			//
			// TOLERATE NOT-EXIST AND NOTHING ELSE. A blanket `continue` here is the
			// tempting fix and it is wrong: readBoundedFileCapped returns three distinct
			// things — the open error verbatim (incl. ENOENT), real I/O errors, and
			// ErrCorruptData for a file over the byte ceiling, which is HYG-00's read
			// half. Skipping that last one would silently drop an over-sized signal and
			// convert a loud guard into data loss. A dropped signal is a park that never
			// wakes.
			//
			// errors.Is over os.IsNotExist deliberately: the open error is returned
			// verbatim today, but errors.Is survives a future wrap.
			if errors.Is(rerr, fs.ErrNotExist) {
				continue
			}
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
		// gosec FALSE POSITIVE: path is built from two components that are
		// BOTH validated single path segments — validateWorkflowID above and
		// validateSignalID in this loop each reject empty, separators, non-local paths
		// (filepath.IsLocal) and anything failing a filepath.Base round-trip, so
		// traversal out of baseDir is unrepresentable here. gosec does not model those
		// validators and flags the variable path regardless. Confirmed false positive;
		// this directive is what stops the ~25% intermittent CI-lint failure.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) { //nolint:gosec // validated path segments; see comment above
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
//
// IT TAKES THE MAILBOX LOCK, and it is the third writer — the one the entry-count invariant
// used to omit. Unlocked, os.RemoveAll destroyed the inode a parked deliverer's flock was bound
// to; the next deliverer then MkdirAll'd a fresh inode and locked THAT, so two writers held
// "the" lock on two different objects and wrote through one path. Reproduced at bc6cb3b: park a
// re-delivery at the createTempFile seam, Delete, refill to cap, release — 5 entries against a
// cap of 4, TakeSignals ErrCorruptData, and the parked delivery reported SUCCESS.
//
// WHAT THIS ACQUISITION ACTUALLY BUYS, corrected because the paragraph above credits it with
// something the SIBLING LOCK FILE already does. An independent reviewer deleted these five
// lines, left the sibling file in place, and the entire signal suite still passed — including
// both regression tests. So the inode-destruction class is closed by moving the lock OFF the
// directory, not by locking here. What locking here buys is separate and real: a Delete that
// runs during an in-flight delivery would otherwise os.RemoveAll the mailbox between that
// delivery's MkdirAll and its rename, and the delivery fails with a spurious ErrIO. With the
// lock, Delete waits. That is now pinned by TestMailboxDeleteLock_DeleteWaitsForAnInFlight-
// Delivery, which reds when these lines are removed — an untested guard is a guard the next
// tidy deletes, exactly as the reviewer deleted it.
//
// It does NOT create the lock file (create=false): see lockMailboxDir. Creating here made
// Delete mint a permanent artifact for workflows that never existed.
//
// Locking the mailbox dir itself does not fix it on its own: the object being locked is the
// object being destroyed, so an unlocked destroyer unlinks it out from under a holder, and
// os.ReadDir/os.Rename resolve by PATH.
//
// A PREVIOUS VERSION OF THIS NOTE OVERSTATED THAT, and the correction matters because phase 117
// reasons from it. It said revalidating the inode after acquiring "cannot fix it, because the
// destruction can happen after the revalidation". That is true only of revalidation WITHOUT a
// locking destroyer. Revalidate-after-acquire COMBINED with a destroyer that takes the same
// lock does close the race: if a writer holds the flock on inode I and has verified
// stat(path) == I, then a second writer would need path's inode to differ from I, which needs
// someone to unlink I, which needs I's flock — which the first writer holds. So the
// combination is correct, and "no amount of checking fixes it" was wrong.
//
// The sibling lock file is still the better design, on reasons that are actually true rather
// than on a false impossibility: revalidation needs a RETRY LOOP (and therefore a liveness
// argument) on every acquisition, it must be repeated at EVERY acquisition site or the weakest
// one decides the guarantee, and it leaves the lock's identity dependent on a directory that
// other code is free to delete. A lock object nobody unlinks needs none of that.
// See signalLockSuffix.
//
// COST, stated because Delete is a reclamation API and this is the one thing it does NOT
// reclaim: the <id>.signals.lock file survives. Removing it would re-arm the exact class above.
// Its population is stated ONCE, on signalLockSuffix, and deliberately not restated here: the
// phrasing has already been wrong once and a restated fact drifts. Invisible to the *.json /
// *.fb listing globs.
//
// A sentence was deleted here (F-116-R3-01, qa). It claimed a Delete on a workflow with no mailbox
// creates a lock file "as a side effect of taking the lock", and called skip-locking-when-absent a
// rejected alternative. Both clauses were the exact inverse of the shipped code: this function
// acquires with create=false, and skipping when absent is what it DOES. It was pre-eee7a0f text that
// survived the fix, contradicting line ~712 of this same block twenty-seven lines above it.
// Recorded rather than silently removed because 116-F4-01's own root cause was "two comments in-tree
// disagreed and the optimistic one got propagated" — the same condition, the same file, the same fact.
// The true content lives once, at :712 and on lockMailboxDir. Do not restate it here.
func removeSignalDir(baseDir, workflowID string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	unlock, lerr := lockMailboxDir(filepath.Join(baseDir, workflowID+signalLockSuffix), false)
	if lerr != nil {
		return lerr
	}
	defer unlock()
	// gosec FALSE POSITIVE, same basis as ackSignalsInDir: workflowID is a
	// validated single path segment (validateWorkflowID above rejects separators,
	// non-local paths and traversal), so the joined path cannot escape baseDir. The
	// suffix is a package constant. gosec does not model the validator.
	return os.RemoveAll(filepath.Join(baseDir, workflowID+signalDirSuffix)) //nolint:gosec // validated path segment; see comment above
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
	box := s.signals[workflowID]
	// Entry-count axis (F1) — the same bound TakeSignals enforces below, checked
	// under the SAME lock so there is no TOCTOU. A re-delivery of an existing ID
	// replaces its map entry and does not grow the mailbox, so it stays legal at the
	// cap. Checked BEFORE the box is created, so a refused delivery leaves no
	// empty-mailbox residue behind.
	after := len(box) + 1
	if _, exists := box[sig.ID]; exists {
		after = len(box)
	}
	if err := checkMailboxEntries(after, signalMailboxCap, workflowID); err != nil {
		return err
	}
	if box == nil {
		box = make(map[string]Signal)
		s.signals[workflowID] = box
	}
	sig.EnqueuedAt = time.Now().UnixNano() // AUD-025: stamp delivery time (store owns it; re-deliver refreshes)
	box[sig.ID] = sig                      // idempotent by ID
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
	return deliverSignalToDir(s.baseDir, workflowID, sig, encodeSignalJSON, s.maxFileSize)
}

// TakeSignals non-destructively reads the JSON mailbox for workflowID.
func (s *JSONFileStore) TakeSignals(workflowID string) ([]Signal, error) {
	return takeSignalsFromDir(s.baseDir, workflowID, decodeSignalJSON, s.maxFileSize)
}

// AckSignals removes the named JSON entries for workflowID.
func (s *JSONFileStore) AckSignals(workflowID string, ids []string) error {
	return ackSignalsInDir(s.baseDir, workflowID, ids)
}

// --- FlatBuffersStore SignalStore impl (FB sidecar mailbox) ---

// DeliverSignal durably enqueues sig as an FB Signal entry in <id>.signals/.
func (s *FlatBuffersStore) DeliverSignal(workflowID string, sig Signal) error {
	return deliverSignalToDir(s.baseDir, workflowID, sig, encodeSignalFB, s.maxFileSize)
}

// TakeSignals non-destructively reads the FB mailbox for workflowID.
func (s *FlatBuffersStore) TakeSignals(workflowID string) ([]Signal, error) {
	return takeSignalsFromDir(s.baseDir, workflowID, decodeSignalFB, s.maxFileSize)
}

// AckSignals removes the named FB entries for workflowID.
func (s *FlatBuffersStore) AckSignals(workflowID string, ids []string) error {
	return ackSignalsInDir(s.baseDir, workflowID, ids)
}
