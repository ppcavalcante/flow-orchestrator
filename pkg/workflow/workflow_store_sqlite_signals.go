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
	// Idempotent by (workflow_id, sig_id) — one row per logical event. On a re-deliver of the SAME
	// id, LAST-writer-wins (DO UPDATE), matching the other stores exactly: InMemory does
	// box[sig.ID]=sig, and the file stores writeFileAtomic-overwrite the sig_id file — both
	// last-writer. DO NOTHING would make SQLite FIRST-writer-wins, a conformance divergence. The
	// enqueued_at is refreshed so the tiebreak ordering tracks the latest delivery (sig_id stays
	// the primary sort, so this is observationally inert for TakeSignals ordering).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO signals(workflow_id, sig_id, name, payload, enqueued_at)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(workflow_id, sig_id)
		 DO UPDATE SET name=excluded.name, payload=excluded.payload, enqueued_at=excluded.enqueued_at`,
		workflowID, sig.ID, sig.Name, payloadStr, time.Now().UnixNano(),
	); err != nil {
		return fmt.Errorf("%w: cannot persist signal: %w", ErrIO, err)
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
		`SELECT sig_id, name, payload FROM signals WHERE workflow_id = ? ORDER BY sig_id`, workflowID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read signal mailbox: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; a close error cannot corrupt state

	out := make([]Signal, 0, count)
	for rows.Next() {
		var id, name string
		// Scan payload as sql.NullString: DeliverSignal always writes a non-NULL string ('' for a
		// nil payload), but the signals table is an external-writable persisted channel (a corrupt
		// DB or a direct SQL write could set NULL). A plain-string Scan of a NULL would FAIL, and
		// TakeSignals errors for the WHOLE query — so one NULL row would brick every read for the
		// workflow (an availability poison-pill on a security-relevant channel). NULL → "" → nil
		// payload, uniform with the empty-string case. (F-P93-ADV-1.)
		var payloadNS sql.NullString
		if err := rows.Scan(&id, &name, &payloadNS); err != nil {
			return nil, fmt.Errorf("%w: cannot scan signal row: %w", ErrCorruptData, err)
		}
		payload, perr := unmarshalSignalPayload(payloadNS.String) // "" (incl. NULL) → nil; ErrCorruptData on bad JSON
		if perr != nil {
			return nil, perr
		}
		out = append(out, Signal{ID: id, Name: name, Payload: payload})
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
