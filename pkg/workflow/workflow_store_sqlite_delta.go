package workflow

// SaveDeltaCheckpoint (M15 ph69) — the O(Δ)-COMPUTE fast path. Where SaveCheckpoint
// (ph67) must scan+re-encode all N nodes to FIND the delta (O(N)/level via shadowFromData
// — the F-M15-P68-1 O(N²) tail), this receives the executor's per-level changed-set and
// re-reads ONLY those keys from d → O(Δ) compute AND O(Δ) writes = genuine O(N) forward
// drive. One crash-atomic transaction, same encoders as Save/saveIncremental so the
// persisted rows are byte-identical.
//
// ABSENCE ⇒ DELETE: a reported key not present in d (a consumed wait via ClearWait, a
// removed node) is deleted from its table — no tombstone needed, the changed-set carries
// the "touched" signal and the absence carries the "gone" signal.
//
// SHADOW INTERACTION: this path does NOT diff against the shadow, but it DOES keep it
// coherent so a subsequent full SaveCheckpoint (a fallback level, or M12 rollback's full
// Save) diffs against the correct frontier. To preserve the ph67-F1 post-commit-advance
// invariant WITHOUT reintroducing an O(N) cost, it STAGES the O(Δ) shadow edits (shadowEdit)
// during the txn and applies them to the live shadow ONLY after a durable COMMIT — a
// rolled-back delta never mutates the shadow. Batched(K) fires the same
// wal_checkpoint(TRUNCATE) cadence as saveIncremental post-commit (ph69-F2).

import (
	"context"
	"fmt"
)

// shadowEdit is one staged O(Δ) mutation to apply to the shadow post-commit (ph69-F1).
type shadowEdit struct {
	dom  deltaDomain
	key  string
	del  bool       // true = delete the key from its domain map
	node encNodeRow // dom==deltaNode, !del
	kv   encKVRow   // dom==deltaData, !del
	wait int64      // dom==deltaWait, !del
}

// applyTo applies this staged edit to a shadowState (called post-commit only).
func (e shadowEdit) applyTo(sh *shadowState) {
	switch e.dom {
	case deltaNode:
		if e.del {
			delete(sh.nodes, e.key)
		} else {
			sh.nodes[e.key] = e.node
		}
	case deltaData:
		if e.del {
			delete(sh.data, e.key)
		} else {
			sh.data[e.key] = e.kv
		}
	case deltaWait:
		if e.del {
			delete(sh.waits, e.key)
		} else {
			sh.waits[e.key] = e.wait
		}
	}
}

// SaveDeltaCheckpoint implements IncrementalCheckpointer (ph69). changed carries the
// per-level touched keys; d is the current WorkflowData (the store re-reads each key's
// value). One transaction; shadow advanced post-commit for cross-path coherence.
func (s *SQLiteStore) SaveDeltaCheckpoint(changed ChangeSet, d *WorkflowData) error {
	ctx := context.Background()
	if d == nil {
		return fmt.Errorf("%w: cannot checkpoint nil workflow data", ErrValidation)
	}
	id := d.GetWorkflowID()
	if err := validateWorkflowID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// The shadow must reflect the committed frontier for cross-path coherence. If it's cold
	// (first delta after a reopen without Load), hydrate it from the DB so a later full
	// SaveCheckpoint diffs correctly (ph67-AF1 parity).
	base := s.shadow[id]
	if base == nil {
		hydrated, herr := s.hydrateShadowFromDB(ctx, id)
		if herr != nil {
			return herr
		}
		if hydrated == nil {
			hydrated = emptyShadow()
		}
		base = hydrated
	}
	// ph69-F1: do NOT mutate the live shadow during the txn. A rolled-back (crash) delta must
	// leave s.shadow[id] at the pre-crash committed frontier (the ph67-F1 post-commit-advance
	// invariant). Stage the O(Δ) shadow edits in a small pending list and apply them to `base`
	// ONLY after a durable COMMIT. (Staging is O(Δ), NOT an O(N) clone-the-whole-shadow —
	// that would reintroduce the O(N²)/level cost this phase exists to kill.)
	var pending []shadowEdit

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin: %w", ErrIO, err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	// M16 (ph74): the FENCING CAS rides this IMMEDIATE txn (no-op single-process / non-claimed).
	if err := s.checkFencingLocked(ctx, tx, id); err != nil {
		return err
	}

	// workflows row — unconditional idempotent UPSERT (one row, cheap; keeps the anchor
	// current incl. rolling_back/trigger_cause without a scalar-change gate — ph68 shape).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflows (id, rolling_back, trigger_cause, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET rolling_back=excluded.rolling_back, trigger_cause=excluded.trigger_cause, updated_at=excluded.updated_at`,
		id, boolToInt(d.IsRollingBack()), int64(d.TriggerCause()), unixNanoNow(),
	); err != nil {
		return fmt.Errorf("%w: upsert workflow: %w", ErrIO, err)
	}

	// nodes: for each reported node, re-read its status + output from d. A node with NEITHER
	// a status entry NOR an output is gone ⇒ DELETE; otherwise UPSERT the encoded row
	// (mirroring shadowFromData's status/output/'' -sentinel rules exactly).
	// ORDER IS LOAD-BEARING (ph69-fixtail-F1): nodes are processed BEFORE data_kv/waits below.
	// TestSQLiteDelta_FailedTxnDoesNotAdvanceShadow's mid-txn-failure bite relies on this order
	// (a node UPSERTs, then a later data row aborts). Reordering would weaken that guard — keep
	// nodes first (also the F2 DB-rollback assertion backstops it).
	for _, node := range changed.Nodes {
		row, present := encNodeFromData(d, node)
		if !present {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM nodes WHERE workflow_id=? AND node_name=?`, id, node); err != nil {
				return fmt.Errorf("%w: delete node %q: %w", ErrIO, node, err)
			}
			pending = append(pending, shadowEdit{dom: deltaNode, key: node, del: true})
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET status=excluded.status, output=excluded.output, has_output=excluded.has_output`,
			id, node, row.status, row.output, boolToInt(row.hasOutput),
		); err != nil {
			return fmt.Errorf("%w: upsert node %q: %w", ErrIO, node, err)
		}
		pending = append(pending, shadowEdit{dom: deltaNode, key: node, node: row})
	}

	// data_kv: re-read each reported key; absent ⇒ DELETE, else UPSERT the typed encoding.
	for _, key := range changed.DataKeys {
		value, ok := d.Get(key)
		if !ok {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM data_kv WHERE workflow_id=? AND key=?`, id, key); err != nil {
				return fmt.Errorf("%w: delete data %q: %w", ErrIO, key, err)
			}
			pending = append(pending, shadowEdit{dom: deltaData, key: key, del: true})
			continue
		}
		kind, iv, fv, sv := encodeKV(value)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO data_kv (workflow_id, key, kind, i_val, f_val, s_val) VALUES (?,?,?,?,?,?)
			 ON CONFLICT(workflow_id, key) DO UPDATE SET kind=excluded.kind, i_val=excluded.i_val, f_val=excluded.f_val, s_val=excluded.s_val`,
			id, key, kind, iv, fv, sv,
		); err != nil {
			return fmt.Errorf("%w: upsert data %q: %w", ErrIO, key, err)
		}
		pending = append(pending, shadowEdit{dom: deltaData, key: key, kv: encKVRow{kind: kind, iv: iv, fv: fv, sv: sv}})
	}

	// waits: re-read each reported wait node; absent ⇒ DELETE (consumed via ClearWait), else UPSERT.
	for _, node := range changed.WaitKeys {
		fireAt, ok := d.GetWait(node)
		if !ok {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM waits WHERE workflow_id=? AND node_name=?`, id, node); err != nil {
				return fmt.Errorf("%w: delete wait %q: %w", ErrIO, node, err)
			}
			pending = append(pending, shadowEdit{dom: deltaWait, key: node, del: true})
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO waits (workflow_id, node_name, fire_at) VALUES (?,?,?)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET fire_at=excluded.fire_at`,
			id, node, fireAt,
		); err != nil {
			return fmt.Errorf("%w: upsert wait %q: %w", ErrIO, node, err)
		}
		pending = append(pending, shadowEdit{dom: deltaWait, key: node, wait: fireAt})
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit: %w", ErrIO, err)
	}
	committed = true
	// ph69-F1: apply the staged O(Δ) edits to the shadow only NOW, after a durable COMMIT. A
	// rolled-back txn returns above (never reaching here), so `base` — which for a warm shadow
	// IS s.shadow[id] — is only ever mutated post-commit → it stays at the last-committed
	// frontier on failure. Applying the pending list is O(Δ), not an O(N) clone.
	for _, e := range pending {
		e.applyTo(base)
	}
	base.rollingBack = d.IsRollingBack()
	base.triggerCause = int64(d.TriggerCause())
	s.shadow[id] = base

	// ph69-F2: the Batched(K) durability cadence — same as saveIncremental (incremental.go).
	// Without it a SQLiteBatched store driving forward through the fast path would NEVER fire
	// the every-Kth wal_checkpoint(TRUNCATE), breaking the ≤K power-loss bound + growing the
	// WAL unbounded (the ph68 fsync-relapse class recurring in a sibling path). Strict is
	// per-commit-durable already (synchronous=FULL), so this is Batched-only.
	if !s.dur.strict {
		s.ckptCount[id]++
		if s.dur.batchK <= 1 || s.ckptCount[id]%s.dur.batchK == 0 {
			if err := s.walCheckpointFullLocked(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// encNodeFromData re-reads a single node's status+output from d and encodes it EXACTLY as
// shadowFromData/saveIncremental would (so a delta row is byte-identical to a full-write
// row). present is false when the node has NEITHER a status entry NOR an output (gone).
// The ” sentinel (ph66-F1): a node with an output but no status → status "".
func encNodeFromData(d *WorkflowData, node string) (encNodeRow, bool) {
	status, hasStatus := d.GetNodeStatus(node)
	out, hasOut := d.GetOutput(node)
	if !hasStatus && !hasOut {
		return encNodeRow{}, false
	}
	enc := ""
	if hasOut {
		enc = encodeOutput(out)
	}
	st := ""
	if hasStatus {
		st = string(status)
	}
	return encNodeRow{status: st, output: enc, hasOutput: hasOut}, true
}
