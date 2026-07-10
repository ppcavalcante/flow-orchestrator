package workflow

// M14 ph61 — group-commit (batched-fsync) durability for FlatBuffersStore
// (DEC-M14-GROUPCOMMIT / B1). The deep-durable cost is per-level FSYNC (~10ms fixed,
// measured), not serialized bytes — so batching the fsync (every K levels, not every
// level) is the durable-time fix. Strategy (d): in Batched(K), SaveCheckpoint writes
// + fsyncs only every Kth call; the K-1 intervening levels are NOT written to disk
// (state stays live in memory), so there is NEVER a partial/un-fsync'd file on disk
// → torn-read-safe TRIVIALLY (the only writes are complete fsync'd ones via
// writeFileAtomic). A crash loses ≤K un-checkpointed levels, which resume re-runs
// idempotently (the M9/M12 IdempotencyKey contract). Strict (K=1) is bit-identical
// to the pre-M14 every-level fsync.
//
// The suspend/completion durability FLOOR: a park (D-10/D-11) and run-completion MUST
// be fsync-durable regardless of mode. The frozen Checkpointer.SaveCheckpoint can't
// carry a "durable-now" flag, so the store exposes an additive Sync() the floor sites
// force — it flushes+fsyncs the last checkpointed state immediately.

import (
	"fmt"
	"path/filepath"
)

// Syncer is the additive optional capability a WorkflowStore MAY implement to force
// its durability floor (M14 ph61). The suspend/completion sites call Sync() after
// their checkpoint so a parked/completed run is fsync-durable even under Batched(K).
// A Strict store's Sync() is a cheap no-op (every checkpoint already fsync'd). This
// is ADDITIVE — NOT part of the frozen Checkpointer interface.
type Syncer interface {
	// Sync forces any batched-but-un-fsync'd checkpoint for workflowID to disk,
	// durably. A no-op when nothing is pending (Strict, or already flushed).
	Sync(workflowID string) error
}

// SaveCheckpoint persists the current workflow state mid-run (M9 crash-resume).
// M14 ph61: in Batched(K) this writes+fsyncs only every Kth call (group-commit);
// Strict (K=1) fsyncs every call, bit-identical to pre-M14. Strategy (d): skipped
// levels are not written at all (live-in-memory), retained for a forced Sync() (the
// suspend/completion floor). Each actual write is writeFileAtomic (torn-safe).
func (s *FlatBuffersStore) SaveCheckpoint(data *WorkflowData) error {
	if data == nil {
		return fmt.Errorf("%w: cannot save nil workflow data", ErrValidation)
	}
	workflowID := data.GetWorkflowID()
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ckptCount == nil {
		s.ckptCount = make(map[string]uint)
	}
	k := s.batchK
	if k < 1 {
		k = 1 // defensive: a struct-literal store defaults to Strict
	}
	s.ckptCount[workflowID]++
	count := s.ckptCount[workflowID]

	// Strict (k==1) → always flush. Batched → flush on every kth checkpoint.
	if k == 1 || count%k == 0 {
		return s.writeFullSnapshotLocked(data, workflowID)
	}
	// Batched, non-boundary level: retain the live state for a forced Sync() (the
	// park/completion floor) and defer the disk write to the next boundary. NOT
	// writing here is what makes strategy (d) torn-safe — no partial file exists.
	s.setPendingCheckpoint(workflowID, data)
	return nil
}

// Sync forces the last batched-but-un-fsync'd checkpoint for workflowID to disk
// (M14 ph61 durability floor). Called by the suspend + completion sites so a parked/
// completed run is fsync-durable even under Batched(K). No-op when nothing pending.
func (s *FlatBuffersStore) Sync(workflowID string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.takePendingCheckpoint(workflowID)
	if pending == nil {
		return nil // nothing deferred (Strict, or already flushed at a boundary)
	}
	return s.writeFullSnapshotLocked(pending, workflowID)
}

// writeFullSnapshotLocked serializes + atomically (temp+fsync+rename) writes data as
// the canonical workflowID.fb, then clears any pending deferred checkpoint (this
// write supersedes it). Caller holds s.mu.
func (s *FlatBuffersStore) writeFullSnapshotLocked(data *WorkflowData, workflowID string) error {
	buf := buildFullStateBuffer(data)
	filePath := filepath.Join(s.baseDir, workflowID+".fb")
	if err := writeFileAtomic(filePath, buf, 0600); err != nil {
		return newIOError("write", workflowID, err)
	}
	// A boundary write supersedes the deferred pending — clear it. Do NOT reset
	// ckptCount here: the cadence is driven by the ABSOLUTE checkpoint count, and a
	// forced Sync() (the park floor) also routes through this write; resetting the
	// counter mid-run would shift the batch boundaries. ckptCount is reclaimed at run
	// termination (Save) + Delete (F1) — the counter is bounded by the run length
	// while live, and a run is a bounded number of levels.
	s.clearPendingCheckpoint(workflowID)
	return nil
}

// clearBatchState drops BOTH the deferred pending checkpoint AND the cadence counter
// for workflowID (M14 ph61 F1). Called at run TERMINATION — a full Save
// (completion/failure/rollback) and Delete — so neither map leaks an entry keyed by
// a caller-supplied ID after the run is done. NOT called at a mid-run boundary write
// (that would shift the batch cadence). Caller holds s.mu.
func (s *FlatBuffersStore) clearBatchState(workflowID string) {
	s.clearPendingCheckpoint(workflowID)
	if s.ckptCount != nil {
		delete(s.ckptCount, workflowID)
	}
}

// --- pending-checkpoint retention (strategy (d): the last deferred state, held so
// a forced Sync() can flush it). A Clone keeps the store's clone-on-save discipline
// (a caller mutating data after a deferred checkpoint can't corrupt the pending
// flush). Guarded by s.mu. ---

func (s *FlatBuffersStore) setPendingCheckpoint(workflowID string, data *WorkflowData) {
	if s.pending == nil {
		s.pending = make(map[string]*WorkflowData)
	}
	s.pending[workflowID] = data.Clone()
}

func (s *FlatBuffersStore) takePendingCheckpoint(workflowID string) *WorkflowData {
	d := s.pending[workflowID]
	if d != nil {
		delete(s.pending, workflowID)
	}
	return d
}

func (s *FlatBuffersStore) clearPendingCheckpoint(workflowID string) {
	if s.pending != nil {
		delete(s.pending, workflowID)
	}
}
