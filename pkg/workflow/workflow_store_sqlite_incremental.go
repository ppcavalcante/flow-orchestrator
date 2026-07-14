package workflow

// Incremental checkpoint (M15 ph67) — the true per-node UPSERT path that removes the
// M14 deep-durable O(N²) re-serialization tail. SaveCheckpoint diffs the incoming
// snapshot against the store's in-memory shadow of the last-committed row-set and, in
// ONE transaction, writes ONLY what changed: UPSERT changed/new rows, DELETE vanished
// rows, update the workflow-row scalars if changed. O(Δ) per level, not O(nodes).
//
// CRASH-SAFETY MODEL (the ph65-proven per-level txn boundary): all row writes for a
// level ride one tx.Commit() — SQLite COMMIT is atomic, so a crash mid-checkpoint leaves
// exactly the levels that committed, zero torn. The shadow is updated ONLY after COMMIT
// returns nil, so a crashed (rolled-back) checkpoint leaves the shadow unchanged too —
// DB and shadow stay coherent at the pre-crash committed frontier. On reopen, Load
// rebuilds the shadow from the durable journal (shadowFromData), so the first
// post-resume checkpoint diffs against the real committed frontier.
//
// BYTE-IDENTITY (ph66 hold, bite 1): the shadow stores the SAME encoded strings that
// Save writes (encNodeRow / encoded kv / fireAt), so the union of committed
// UPSERT/DELETEs reconstructs exactly the row-set a full Save of the final state would
// write; Load reads rows order-independently → identical WorkflowData → identical
// Snapshot bytes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// encNodeRow is the encoded form of a nodes row — everything that determines its bytes.
// Two rows with equal encNodeRow produce identical persisted+reloaded state, so the diff
// can skip an unchanged node.
type encNodeRow struct {
	status    string // string(NodeStatus), or "" for the output-only sentinel (ph66-F1)
	output    string // encodeOutput(...) result; "" when !hasOutput
	hasOutput bool
}

// encKVRow is the encoded form of a data_kv row (kind + the one populated typed column).
type encKVRow struct {
	kind int
	iv   *int64
	fv   *float64
	sv   *string
}

func (a encKVRow) equal(b encKVRow) bool {
	if a.kind != b.kind {
		return false
	}
	return ptrIntEq(a.iv, b.iv) && ptrFloatEq(a.fv, b.fv) && ptrStrEq(a.sv, b.sv)
}

func ptrIntEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func ptrFloatEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Compare by bit pattern, not ==: +0.0 == -0.0 is true but they have distinct FB
	// float64 bytes, so a +0.0 → -0.0 transition MUST be detected as changed (else the
	// diff skips the write and the DB keeps the stale sign bit — a strict byte-identity
	// gap, ph67-F2). Float64bits also makes NadN==NaN by pattern (harmless: a real NaN
	// payload change still differs; identical NaN bits legitimately skip).
	return math.Float64bits(*a) == math.Float64bits(*b)
}
func ptrStrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// shadowState is the last-committed encoded row-set for one workflow — the diff baseline.
// rollingBack/triggerCause are carried so the TARGET shadow supplies the workflow-row
// scalars to the (unconditional) UPSERT; they are NOT used as a base skip-decision — the
// one-row workflow write is always issued (ph67-F1/F3), so no scalar-change gate exists.
type shadowState struct {
	rollingBack  bool
	triggerCause int64
	nodes        map[string]encNodeRow
	data         map[string]encKVRow
	waits        map[string]int64
}

// emptyShadow is a baseline with no rows — equivalent to a first write (every target row
// is new, nothing to delete). Returned by hydrateShadowFromDB when the DB has no state.
func emptyShadow() *shadowState {
	return &shadowState{
		nodes: make(map[string]encNodeRow),
		data:  make(map[string]encKVRow),
		waits: make(map[string]int64),
	}
}

// clone returns a deep copy of the shadow's maps (scalars copied by value). Used by the
// ph69 delta path (ph69-F1): the delta must mutate a WORKING COPY, never the live shadow,
// so a rolled-back (crash) delta txn leaves s.shadow[id] at the pre-crash committed frontier
// — the ph67-F1 post-commit-advance invariant. The copy is swapped into s.shadow ONLY after
// a durable COMMIT.
func (s *shadowState) clone() *shadowState {
	next := &shadowState{
		rollingBack:  s.rollingBack,
		triggerCause: s.triggerCause,
		nodes:        make(map[string]encNodeRow, len(s.nodes)),
		data:         make(map[string]encKVRow, len(s.data)),
		waits:        make(map[string]int64, len(s.waits)),
	}
	for k, v := range s.nodes {
		next.nodes[k] = v
	}
	for k, v := range s.data {
		next.data[k] = v
	}
	for k, v := range s.waits {
		next.waits[k] = v
	}
	return next
}

// shadowFromData builds the encoded row-set that a full Save of data WOULD write. Used
// to (re)seed the shadow after a full Save and after a Load (resume-into). It mirrors
// Save's encoding EXACTLY so the diff baseline equals what is durably on disk.
func shadowFromData(data *WorkflowData) *shadowState {
	st := &shadowState{
		rollingBack:  data.IsRollingBack(),
		triggerCause: int64(data.TriggerCause()),
		nodes:        make(map[string]encNodeRow),
		data:         make(map[string]encKVRow),
		waits:        make(map[string]int64),
	}
	// nodes with a status entry (may also have an output).
	data.ForEachNodeStatus(func(node string, status NodeStatus) {
		out, has := data.GetOutput(node)
		enc := ""
		if has {
			enc = encodeOutput(out)
		}
		st.nodes[node] = encNodeRow{status: string(status), output: enc, hasOutput: has}
	})
	// output-only nodes (no status entry): '' sentinel status (ph66-F1). A node already
	// present above (status + this output) keeps its status — do NOT downgrade it.
	data.ForEachOutput(func(node string, out interface{}) {
		if existing, ok := st.nodes[node]; ok {
			existing.output = encodeOutput(out)
			existing.hasOutput = true
			st.nodes[node] = existing
			return
		}
		st.nodes[node] = encNodeRow{status: "", output: encodeOutput(out), hasOutput: true}
	})
	data.ForEach(func(k string, value interface{}) {
		kind, iv, fv, sv := encodeKV(value)
		st.data[k] = encKVRow{kind: kind, iv: iv, fv: fv, sv: sv}
	})
	data.ForEachWait(func(node string, fireAt int64) {
		st.waits[node] = fireAt
	})
	return st
}

// saveIncremental is the ph67 SaveCheckpoint body. It diffs data against the shadow and
// writes only the delta in one crash-atomic transaction, then (post-commit) advances the
// shadow. With no shadow yet, the target is built from an empty baseline → every row is
// "new" → a correct full write.
func (s *SQLiteStore) saveIncremental(data *WorkflowData) error {
	ctx := context.Background()
	if data == nil {
		return fmt.Errorf("%w: cannot checkpoint nil workflow data", ErrValidation)
	}
	id := data.GetWorkflowID()
	if err := validateWorkflowID(id); err != nil {
		return err
	}

	// The target encoded row-set (what the DB must equal after this checkpoint).
	target := shadowFromData(data)

	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.shadow[id]

	// p67-AF1 fix: an empty shadow does NOT mean an empty DB. If we diffed a checkpoint
	// against a nil baseline while the durable DB already held rows for this id (a reopen
	// WITHOUT a prior Load, or a recycled id after a process restart), the diff would
	// UPSERT the new rows but its DELETE-vanished loops — iterating an empty base — would
	// delete NOTHING, leaking stale prior-process rows and diverging from a full Save
	// (BITE 1). So when there is no in-memory baseline, hydrate it from the DB's current
	// committed frontier, so the diff sees (and deletes) the rows the new state dropped.
	if base == nil {
		hydrated, herr := s.hydrateShadowFromDB(ctx, id)
		if herr != nil {
			return herr
		}
		base = hydrated // nil when the workflow has no rows yet → true first-write full path
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		// The executor's per-level checkpoint drives this path; BEGIN IMMEDIATE acquires the write
		// lock here, so a contended writer past busy_timeout surfaces SQLITE_BUSY → ErrBusy (transient),
		// errors.Is-reachable at the Execute return; clean abort (no row written). (MAJOR-5)
		return classifyTxErr("begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	// M16 (ph74): the FENCING CAS rides this IMMEDIATE txn — reject a stale-token write BEFORE
	// any row lands (mu is held; the CAS reads tokenState + re-reads leases.fencing_token). No-op
	// in single-process mode or for a non-claimed drive. renew-on-checkpoint happens inside it.
	if err := s.checkFencingLocked(ctx, tx, id); err != nil {
		return err
	}

	// workflows row: an unconditional idempotent UPSERT. It's exactly ONE row, so the
	// "skip if unchanged" micro-optimization isn't worth an INSERT-vs-UPDATE branch keyed
	// on whether the row exists — a fragile distinction (base is never nil here after
	// hydration, and an UPDATE on an absent row would silently no-op the anchor). The
	// UPSERT is correct on the first write, a reopen-without-Load, and a normal level alike.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflows (id, rolling_back, trigger_cause, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET rolling_back=excluded.rolling_back, trigger_cause=excluded.trigger_cause, updated_at=excluded.updated_at`,
		id, boolToInt(target.rollingBack), target.triggerCause, unixNanoNow(),
	); err != nil {
		return fmt.Errorf("%w: upsert workflow: %w", ErrIO, err)
	}

	if err := diffNodes(ctx, tx, id, baseNodes(base), target.nodes); err != nil {
		return err
	}
	if err := diffData(ctx, tx, id, baseData(base), target.data); err != nil {
		return err
	}
	if err := diffWaits(ctx, tx, id, baseWaits(base), target.waits); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return classifyTxErr("commit", err) // SQLITE_BUSY at commit → ErrBusy (transient); else ErrIO.
	}
	committed = true
	// Advance the shadow ONLY after a durable COMMIT (crash-safety ordering).
	s.shadow[id] = target

	// ph68 durability cadence. In Strict (synchronous=FULL + fullfsync=1) the COMMIT above
	// is already power-loss-durable per level — nothing more to do. In Batched(K)
	// (synchronous=NORMAL + WAL) the COMMIT only appended WAL frames; force a durable
	// wal_checkpoint(TRUNCATE) every Kth checkpoint so a power-loss loses ≤K un-checkpointed
	// levels. (s.mu is held; caller of saveIncremental holds it for the whole body.)
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

// hydrateShadowFromDB reconstructs the diff baseline from the workflow's CURRENT durable
// rows (the committed frontier), for the empty-shadow case (reopen without Load). It
// returns nil when the workflow has no `workflows` row (genuinely never written → the
// diff correctly treats every target row as new). Reuses the same scan helpers as Load so
// the baseline equals exactly what Load would reconstruct. Caller holds s.mu.
func (s *SQLiteStore) hydrateShadowFromDB(ctx context.Context, id string) (*shadowState, error) {
	var rollingBack, triggerCause int64
	err := s.db.QueryRowContext(ctx,
		`SELECT rolling_back, trigger_cause FROM workflows WHERE id = ?`, id,
	).Scan(&rollingBack, &triggerCause)
	if errors.Is(err, sql.ErrNoRows) {
		// No durable state yet → an EMPTY (non-nil) baseline: the diff treats every target
		// row as new and has nothing to delete — exactly the true-first-write path, with no
		// nil ambiguity.
		return emptyShadow(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: hydrate workflow: %w", ErrCorruptData, err)
	}

	tmp := NewWorkflowData(id)
	drows, err := s.db.QueryContext(ctx, `SELECT key, kind, i_val, f_val, s_val FROM data_kv WHERE workflow_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("%w: hydrate data: %w", ErrCorruptData, err)
	}
	if err := scanDataKV(drows, tmp); err != nil {
		return nil, err
	}
	nrows, err := s.db.QueryContext(ctx, `SELECT node_name, status, output, has_output FROM nodes WHERE workflow_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("%w: hydrate nodes: %w", ErrCorruptData, err)
	}
	if err := scanNodes(nrows, tmp); err != nil {
		return nil, err
	}
	wrows, err := s.db.QueryContext(ctx, `SELECT node_name, fire_at FROM waits WHERE workflow_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("%w: hydrate waits: %w", ErrCorruptData, err)
	}
	if err := scanWaits(wrows, tmp); err != nil {
		return nil, err
	}
	tmp.SetRollingBack(rollingBack != 0)
	if triggerCause >= 0 && triggerCause <= int64(TriggerDeadlineExceeded) {
		tmp.SetTriggerCause(TriggerCause(triggerCause))
	}
	return shadowFromData(tmp), nil
}

// baseNodes/baseData/baseWaits return the base maps or empty maps when base is nil
// (first checkpoint) so the diff treats every target row as new.
func baseNodes(b *shadowState) map[string]encNodeRow {
	if b == nil {
		return nil
	}
	return b.nodes
}
func baseData(b *shadowState) map[string]encKVRow {
	if b == nil {
		return nil
	}
	return b.data
}
func baseWaits(b *shadowState) map[string]int64 {
	if b == nil {
		return nil
	}
	return b.waits
}

// diffNodes UPSERTs changed/new node rows and DELETEs rows that vanished from target.
func diffNodes(ctx context.Context, tx *sql.Tx, id string, base, target map[string]encNodeRow) error {
	for node, row := range target {
		if prev, ok := base[node]; ok && prev == row {
			continue // unchanged — skip (the O(Δ) win)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (workflow_id, node_name, status, output, has_output) VALUES (?,?,?,?,?)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET status=excluded.status, output=excluded.output, has_output=excluded.has_output`,
			id, node, row.status, row.output, boolToInt(row.hasOutput),
		); err != nil {
			return fmt.Errorf("%w: upsert node %q: %w", ErrIO, node, err)
		}
	}
	for node := range base {
		if _, ok := target[node]; !ok {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM nodes WHERE workflow_id=? AND node_name=?`, id, node); err != nil {
				return fmt.Errorf("%w: delete node %q: %w", ErrIO, node, err)
			}
		}
	}
	return nil
}

// diffData UPSERTs changed/new data_kv rows and DELETEs vanished keys.
func diffData(ctx context.Context, tx *sql.Tx, id string, base, target map[string]encKVRow) error {
	for k, row := range target {
		if prev, ok := base[k]; ok && prev.equal(row) {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO data_kv (workflow_id, key, kind, i_val, f_val, s_val) VALUES (?,?,?,?,?,?)
			 ON CONFLICT(workflow_id, key) DO UPDATE SET kind=excluded.kind, i_val=excluded.i_val, f_val=excluded.f_val, s_val=excluded.s_val`,
			id, k, row.kind, row.iv, row.fv, row.sv,
		); err != nil {
			return fmt.Errorf("%w: upsert data %q: %w", ErrIO, k, err)
		}
	}
	for k := range base {
		if _, ok := target[k]; !ok {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM data_kv WHERE workflow_id=? AND key=?`, id, k); err != nil {
				return fmt.Errorf("%w: delete data %q: %w", ErrIO, k, err)
			}
		}
	}
	return nil
}

// diffWaits UPSERTs changed/new wait rows and DELETEs vanished (consumed) waits.
func diffWaits(ctx context.Context, tx *sql.Tx, id string, base, target map[string]int64) error {
	for node, fireAt := range target {
		if prev, ok := base[node]; ok && prev == fireAt {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO waits (workflow_id, node_name, fire_at) VALUES (?,?,?)
			 ON CONFLICT(workflow_id, node_name) DO UPDATE SET fire_at=excluded.fire_at`,
			id, node, fireAt,
		); err != nil {
			return fmt.Errorf("%w: upsert wait %q: %w", ErrIO, node, err)
		}
	}
	for node := range base {
		if _, ok := target[node]; !ok {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM waits WHERE workflow_id=? AND node_name=?`, id, node); err != nil {
				return fmt.Errorf("%w: delete wait %q: %w", ErrIO, node, err)
			}
		}
	}
	return nil
}
