package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// M19 ph93 — SQLite SignalStore: the durable signal mailbox that makes *SQLiteStore implement
// SignalStore (previously only InMemory/FB/JSON did). Closes the ph90 approvals-on-SQLite gap
// and unblocks the ph94 queue-path wake. Backed by the `signals` table (workflow_store_sqlite.go),
// a SEPARATE table from the WorkflowData snapshot (mailbox-outside-snapshot, MH37-1) with NO FK to
// workflows (early-signal buffering). Semantics mirror the other stores EXACTLY — the cross-store
// conformance suite (signal_store_test.go) is the spec.

// compile-time conformance: *SQLiteStore is a SignalStore.
var _ SignalStore = (*SQLiteStore)(nil)

// DeliverSignal durably enqueues sig for workflowID, idempotent by sig.ID (a re-delivered sig_id
// updates the single row — last-writer-wins, uniform with the other stores). It rejects an empty
// sig.ID and succeeds regardless of whether the workflow instance exists (no FK — early-signal
// buffering). The payload is JSON-encoded (the same marshal the file stores use), so the ph90
// defensive decode tolerates the round-trip.
func (s *SQLiteStore) DeliverSignal(workflowID string, sig Signal) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	if err := validateSignalID(sig.ID); err != nil {
		return err
	}
	payloadStr, err := marshalSignalPayload(sig.Payload)
	if err != nil {
		return err
	}

	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	// Entry-count axis (F1): refuse a delivery that would leave the mailbox holding more rows than
	// TakeSignals will accept. Before this the write side had no bound at all, so every DeliverSignal
	// returned nil and TakeSignals then rejected the WHOLE mailbox with ErrCorruptData — permanently,
	// and the read is all-or-nothing, so one over-cap backlog fails a WaitForSignal run's take on
	// every re-drive until the mailbox is drained out of band.
	//
	// THE COUNT AND THE INSERT ARE ONE STATEMENT, and that is the whole correctness argument.
	// The first version of this guard did a COUNT query and then a separate INSERT, both under
	// s.mu, with a comment claiming "under the SAME lock as the insert, so there is no TOCTOU".
	// That comment was FALSE in the case this store exists for: s.mu is process-local, and
	// SQLiteStore is the multi-process store (M16 competing consumers). Two handles on one DB file
	// hold two different mutexes. Measured at 264265d, cap 8 seeded with 7, eight handles racing:
	// 8 accepted, ZERO refusals, 15 rows — the guard admitted every single delivery.
	//
	// A subquery inside the INSERT is evaluated within that statement's own implicit transaction,
	// so no other writer can commit between the count and the write. This is the same lesson M20
	// already banked for dispatch caps (workflow_store_sqlite_caps.go): a count taken OUTSIDE the
	// write lock is stale under concurrency.
	//
	// One statement rather than an explicit BEGIN IMMEDIATE, and the reason is AVAILABILITY, not
	// correctness — an earlier version of this comment overstated it. _txlock=immediate is set only
	// in mp mode (verified: openSQLiteDB is the single DSN site), so on the single-process path a
	// transaction would be DEFERRED and would read-then-upgrade. That does NOT commit a stale read:
	// SQLite fails the upgrade with SQLITE_BUSY rather than admitting it. So a DEFERRED transaction
	// would be CORRECT and merely liable to spurious busy-failures under contention. One statement
	// avoids that failure mode and needs no mode-dependent reasoning at all, which is why it is
	// still the right shape — but it is not the case that the alternative was unsafe.
	//
	// The EXISTS arm is the idempotency contract: re-delivering a sig_id already present replaces
	// its row and does not grow the mailbox, so it must still succeed AT the cap.
	//
	// Idempotent by (workflow_id, sig_id) — one row per logical event. On a re-deliver of the SAME
	// id, LAST-writer-wins (DO UPDATE), matching the other stores exactly: InMemory does
	// box[sig.ID]=sig, and the file stores writeFileAtomic-overwrite the sig_id file — both
	// last-writer. DO NOTHING would make SQLite FIRST-writer-wins, a conformance divergence. The
	// enqueued_at is refreshed so the tiebreak ordering tracks the latest delivery (sig_id stays
	// the primary sort, so this is observationally inert for TakeSignals ordering).
	//
	// The SELECT carries a WHERE clause deliberately: SQLite cannot parse an UPSERT attached to an
	// INSERT...SELECT without one (the "ON" would be ambiguous with a join's ON).
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO signals(workflow_id, sig_id, name, payload, enqueued_at)
		 SELECT ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM signals WHERE workflow_id = ? AND sig_id = ?)
		    OR (SELECT COUNT(*) FROM signals WHERE workflow_id = ?) < ?
		 ON CONFLICT(workflow_id, sig_id)
		 DO UPDATE SET name=excluded.name, payload=excluded.payload, enqueued_at=excluded.enqueued_at`,
		workflowID, sig.ID, sig.Name, payloadStr, time.Now().UnixNano(),
		workflowID, sig.ID,
		workflowID, signalMailboxCap,
	)
	if err != nil {
		return fmt.Errorf("%w: cannot persist signal: %w", ErrIO, err)
	}
	// Zero rows affected means the WHERE refused it: not an existing id, and the mailbox is already
	// at or above the cap. Re-read the count only to name it in the error — the refusal already
	// happened atomically above, so this read cannot re-open the race it reports on.
	//
	// RowsAffected's own error is surfaced rather than folded into the zero-check. Unreachable
	// with the current driver (mattn/go-sqlite3 returns a cached value and a nil error), but the
	// shape `rerr == nil && n == 0` falls through to `return nil` when rerr is non-nil, which
	// reports a delivery that may have been REFUSED as a success — a silently dropped signal is
	// a park that never wakes. One line so the failure is loud instead of inverted.
	n, rerr := res.RowsAffected()
	if rerr != nil {
		return fmt.Errorf("%w: cannot confirm signal delivery: %w", ErrIO, rerr)
	}
	if n == 0 {
		var count int
		if qerr := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM signals WHERE workflow_id = ?`, workflowID,
		).Scan(&count); qerr != nil {
			return fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, qerr)
		}
		if verr := checkMailboxEntries(count+1, signalMailboxCap, workflowID); verr != nil {
			return verr
		}
		// The re-read came back UNDER the cap — a concurrent AckSignals drained the mailbox
		// between the refusal and this read. Nothing was written either way, so returning nil
		// here would report a delivery that did not happen. Report the refusal on its own terms.
		return fmt.Errorf("%w: workflow %q signal mailbox was at the %d-entry max when the delivery was evaluated; retry",
			ErrValidation, workflowID, signalMailboxCap)
	}
	return nil
}

// TakeSignals returns the buffered signals for workflowID NON-DESTRUCTIVELY (removal is
// AckSignals), sorted by sig_id for deterministic iteration (the conformance contract). It
// enforces the F37 mailbox bound BEFORE materializing rows: an over-cap backlog is rejected with
// ErrCorruptData rather than driving an unbounded alloc (the store defends the read path — the
// mailbox is an external-writable channel, M9 threat model). A missing/empty mailbox returns an
// empty slice, not an error.
func (s *SQLiteStore) TakeSignals(workflowID string) ([]Signal, error) {
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}

	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	// F37 cap FIRST (under the same lock — no TOCTOU): count the un-acked entries; reject over-cap
	// before allocating/iterating. Mirrors the file/InMemory stores' element-count guard.
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signals WHERE workflow_id = ?`, workflowID,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, err)
	}
	if count > signalMailboxCap {
		return nil, fmt.Errorf("%w: signal mailbox entry count exceeds max", ErrCorruptData)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT sig_id, name, payload, enqueued_at FROM signals WHERE workflow_id = ? ORDER BY sig_id`, workflowID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; a close error cannot corrupt state

	out := make([]Signal, 0, count)
	for rows.Next() {
		var id, name string
		var enqueuedAt int64 // AUD-025: the durable delivery time, exposed for freshness checks
		// Scan payload as sql.NullString: DeliverSignal always writes a non-NULL string ('' for a
		// nil payload), but the signals table is an external-writable persisted channel (a corrupt
		// DB or a direct SQL write could set NULL). A plain-string Scan of a NULL would FAIL, and
		// TakeSignals errors for the WHOLE query — so one NULL row would brick every read for the
		// workflow (an availability poison-pill on a security-relevant channel). NULL → "" → nil
		// payload, uniform with the empty-string case. (F-P93-ADV-1.)
		var payloadNS sql.NullString
		if err := rows.Scan(&id, &name, &payloadNS, &enqueuedAt); err != nil {
			return nil, fmt.Errorf("%w: cannot scan signal row: %w", ErrCorruptData, err)
		}
		payload, perr := unmarshalSignalPayload(payloadNS.String) // "" (incl. NULL) → nil; ErrCorruptData on bad JSON
		if perr != nil {
			return nil, perr
		}
		out = append(out, Signal{ID: id, Name: name, Payload: payload, EnqueuedAt: enqueuedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, err)
	}
	return out, nil
}

// AckSignals removes the named signals (by ID) for workflowID. Best-effort and idempotent: acking
// an absent ID is a 0-row delete, not an error. Called ONLY after the consuming node's Completed
// status is durably checkpointed (the take→apply→Completed→checkpoint→ack ordering, D37-04).
func (s *SQLiteStore) AckSignals(workflowID string, ids []string) error {
	if err := validateWorkflowID(workflowID); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if err := validateSignalID(id); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM signals WHERE workflow_id = ? AND sig_id = ?`, workflowID, id,
		); err != nil {
			return fmt.Errorf("%w: cannot ack signal %q: %w", ErrIO, id, err)
		}
	}
	return nil
}
