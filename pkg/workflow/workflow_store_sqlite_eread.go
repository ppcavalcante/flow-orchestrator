package workflow

// M25 §14.B1 — E-READ: authoritative submission inspection + bounded, resumable discovery. A read-only
// public surface over the durable work_queue row, so an application can recover the ORIGINAL queued type
// and exact input for any submission BY ID — including a record that never obtained a journal (a
// constructor/seed failure or a cancellation before any action) — and can ENUMERATE submissions in a
// bounded, cursor-resumable way that includes terminal queue-only rows, not just pending ones.
//
// Why this is not covered by the existing surface:
//   - WorkflowStatus (OBS-RM-04) reports lifecycle + a journal node tally, but NOT the original queued
//     input, and its node tally reads the MUTABLE journal — it cannot answer "what type/input was this
//     submitted with" for a record that has no journal.
//   - ListPending (DEC-M17-STUCKVIS) enumerates ONLY pending rows and is unbounded — it cannot discover a
//     terminal (done/failed/cancelled) queue-only submission, and it has no cursor.
//   - queueChildTypeInput reads type+input but is unexported and single-id (no lifecycle, no enumeration).
//
// All methods are read-only, require mp mode (the work_queue table only exists on an mp store), and
// classify failures into the store error domain so a caller can DISTINGUISH a missing id (ErrNotFound)
// from a storage/corruption failure (ErrBusy/ErrIO/ErrCorruptData).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// QueuedSubmission is the AUTHORITATIVE durable record of one dispatch submission, read straight from its
// work_queue row — independent of any journal. Input is the EXACT original queued bytes (a defensive copy;
// mutating it cannot alter the persisted input). Every field is engine-durable: the control-plane columns
// (ParentID/ParentSignal/Depth) are engine-set (never the input BLOB), and CancelRequested reflects the
// durable operator-cancel INTENT flag.
type QueuedSubmission struct {
	WorkflowID      string
	Type            string
	Input           []byte
	State           string // pending | claimed | done | failed | cancelled
	Attempts        int
	EnqueuedAt      int64  // unix-nanos submit time (the FIFO ordering key)
	UpdatedAt       int64  // unix-nanos of the last lifecycle transition
	ParentID        string // "" when not a sub-workflow child
	ParentSignal    string // "" when not a sub-workflow child
	Depth           int    // sub-workflow nesting depth (0 for a plain dispatch)
	CancelRequested bool   // the durable operator-cancel intent flag is set
	OwnerHost       string // §14.B2 designated-host binding ("" = generic, any worker); lets a caller
	// validate/fail-closed on a run's durable host identity before driving it.
}

// InspectSubmission returns the authoritative durable record for workflowID BY ID, or ErrNotFound when there
// is no work_queue row for it (DISTINCT from a storage failure, which returns an ErrBusy/ErrIO/ErrCorruptData
// -classed error). It works for a record that NEVER obtained a journal — a constructor/seed failure or a
// cancellation before any action leaves a terminal queue row and no journal, and this still recovers its
// original type + exact input. The type + input come from the durable queue row, so they are independent of
// any mutable journal value.
func (s *SQLiteStore) InspectSubmission(workflowID string) (*QueuedSubmission, error) {
	if !s.dur.mp {
		return nil, fmt.Errorf("%w: InspectSubmission requires a multi-process store", ErrValidation)
	}
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		sub                    = &QueuedSubmission{WorkflowID: workflowID}
		parentID, parentSignal sql.NullString
		ownerHost              sql.NullString
		depth                  sql.NullInt64
		cancelReq              sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT type, input, state, attempts, enqueued_at, updated_at, parent_id, parent_signal, depth, cancel_requested, owner_host
		 FROM work_queue WHERE workflow_id=?`, workflowID).
		Scan(&sub.Type, &sub.Input, &sub.State, &sub.Attempts, &sub.EnqueuedAt, &sub.UpdatedAt,
			&parentID, &parentSignal, &depth, &cancelReq, &ownerHost)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: no submission for %q", ErrNotFound, workflowID)
	case err != nil:
		return nil, classifyTxErr("inspectsubmission", err) // SQLITE_BUSY → ErrBusy; else ErrIO.
	}
	sub.Input = copyBytes(sub.Input) // defensive copy — the caller cannot mutate our read buffer into the row.
	sub.ParentID = parentID.String
	sub.ParentSignal = parentSignal.String
	sub.Depth = int(depth.Int64)
	sub.CancelRequested = cancelReq.Valid
	sub.OwnerHost = ownerHost.String
	return sub, nil
}

// Submission-discovery paging bounds. A caller may request any limit; it is clamped to [1, MaxSubmissionPageLimit]
// (0 or negative → DefaultSubmissionPageLimit) so a single call can NEVER become an unbounded database dump.
const (
	DefaultSubmissionPageLimit = 100
	MaxSubmissionPageLimit     = 1000
)

// SubmissionCursor is the opaque continuation token for ListSubmissions. It is a KEYSET position over the
// stable ordering key (EnqueuedAt, WorkflowID) — both immutable for a row (enqueued_at never changes,
// workflow_id is the primary key), which is what makes pagination safe under concurrent writes (see
// ListSubmissions). Pass a page's Next back verbatim to continue; a nil cursor starts at the beginning.
type SubmissionCursor struct {
	EnqueuedAt int64
	WorkflowID string
}

// SubmissionPage is one bounded page of discovery results plus the continuation cursor (Next==nil ⇒ the last
// page). Items are ordered by (EnqueuedAt ASC, WorkflowID ASC).
type SubmissionPage struct {
	Items []QueuedSubmission
	Next  *SubmissionCursor
}

// ListSubmissions returns a BOUNDED, cursor-resumable page of submissions ordered by (enqueued_at, workflow_id),
// INCLUDING terminal queue-only rows (done/failed/cancelled) — not just pending ones. `states` filters to the
// given lifecycle states (nil/empty = ALL states); `limit` is clamped to [1, MaxSubmissionPageLimit]; `after`
// continues a prior page (nil = from the beginning).
//
// ORDERING + CURSOR: rows are ordered by the immutable keyset (enqueued_at, workflow_id) and paged by
// `(enqueued_at, workflow_id) > (after…)`. The ordering key never mutates and work_queue rows are never deleted.
//
// SCOPE — a FINITE PER-PAGE SWEEP, NOT a lossless change feed (the honest contract; §15 R3). Each page is a
// point-in-time read of the rows that exist and match `states` WHEN THAT PAGE IS FETCHED. Within one forward
// sweep a row that was already BEHIND the cursor is not revisited, so:
//   - a row APPENDED after the cursor position (a new Enqueue) IS observed on continuation (keyset advance);
//   - but a row whose FILTER MEMBERSHIP changes BEHIND the cursor is NOT caught by continuing that sweep — e.g.
//     with a `cancelled`-only filter, if a row earlier in the key order transitions pending→cancelled AFTER the
//     cursor has passed its position, this sweep will not return it (a later insertion ordered behind the cursor
//     is likewise not caught). This is expected filtered-keyset behavior, not a cross-page snapshot guarantee.
//
// For completeness where membership changes concurrently, run a FRESH sweep (a new cursor from the beginning),
// or track ids and re-InspectSubmission them, rather than treating a filtered continuation as an event stream.
// A single forward sweep with a FIXED cursor sequence never skips or duplicates a row that stays in the filter,
// because the key is immutable and rows are never deleted.
//
// A caller needing a mutually-consistent multi-page view should quiesce writes or snapshot externally; this
// method deliberately trades that for O(1) memory and unbounded-dataset resumability.
func (s *SQLiteStore) ListSubmissions(states []string, limit int, after *SubmissionCursor) (SubmissionPage, error) {
	if !s.dur.mp {
		return SubmissionPage{}, fmt.Errorf("%w: ListSubmissions requires a multi-process store", ErrValidation)
	}
	switch {
	case limit <= 0:
		limit = DefaultSubmissionPageLimit
	case limit > MaxSubmissionPageLimit:
		limit = MaxSubmissionPageLimit
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `SELECT workflow_id, type, input, state, attempts, enqueued_at, updated_at, parent_id, parent_signal, depth, cancel_requested, owner_host
	          FROM work_queue`
	var (
		conds []string
		args  []interface{}
	)
	if clause, stateArgs := buildStateFilter(states); clause != "" {
		conds = append(conds, clause)
		args = append(args, stateArgs...)
	}
	if after != nil {
		// Keyset: strictly after the cursor position in (enqueued_at, workflow_id) order.
		conds = append(conds, "(enqueued_at > ? OR (enqueued_at = ? AND workflow_id > ?))")
		args = append(args, after.EnqueuedAt, after.EnqueuedAt, after.WorkflowID)
	}
	if len(conds) > 0 {
		// The joined fragments are LITERAL SQL with ?-placeholders only — every value is bound via args,
		// never concatenated — so this is not an injection surface.
		query += " WHERE " + joinAnd(conds) //nolint:gosec // G202: placeholder-only fragments, values are ?-bound
	}
	// Fetch limit+1 to detect whether a further page exists without a second COUNT query.
	query += " ORDER BY enqueued_at, workflow_id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return SubmissionPage{}, classifyTxErr("listsubmissions", err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var page SubmissionPage
	for rows.Next() {
		var (
			sub                    QueuedSubmission
			parentID, parentSignal sql.NullString
			ownerHost              sql.NullString
			depth                  sql.NullInt64
			cancelReq              sql.NullInt64
		)
		if err := rows.Scan(&sub.WorkflowID, &sub.Type, &sub.Input, &sub.State, &sub.Attempts,
			&sub.EnqueuedAt, &sub.UpdatedAt, &parentID, &parentSignal, &depth, &cancelReq, &ownerHost); err != nil {
			return SubmissionPage{}, fmt.Errorf("%w: scan submission row: %w", ErrCorruptData, err)
		}
		sub.Input = copyBytes(sub.Input)
		sub.ParentID = parentID.String
		sub.ParentSignal = parentSignal.String
		sub.Depth = int(depth.Int64)
		sub.CancelRequested = cancelReq.Valid
		sub.OwnerHost = ownerHost.String
		page.Items = append(page.Items, sub)
	}
	if err := rows.Err(); err != nil {
		return SubmissionPage{}, fmt.Errorf("%w: submission rows: %w", ErrCorruptData, err)
	}

	if len(page.Items) > limit {
		// The (limit+1)th row proves there is more: drop it and hand back a cursor at the LAST returned row.
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = &SubmissionCursor{EnqueuedAt: last.EnqueuedAt, WorkflowID: last.WorkflowID}
	}
	return page, nil
}

// buildStateFilter renders `state IN (?,…)` for the given states (empty → no clause). Values are ?-bound.
func buildStateFilter(states []string) (clause string, args []interface{}) {
	if len(states) == 0 {
		return "", nil
	}
	ph := make([]string, len(states))
	args = make([]interface{}, len(states))
	for i, st := range states {
		ph[i] = "?"
		args[i] = st
	}
	return "state IN (" + joinComma(ph) + ")", args
}

// joinAnd / joinComma are tiny string joiners kept local so the query builder reads top-to-bottom without
// pulling strings.Join into a hot read path's import set (mirrors buildTypeFilter's style).
func joinAnd(parts []string) string   { return joinWith(parts, " AND ") }
func joinComma(parts []string) string { return joinWith(parts, ",") }
func joinWith(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// copyBytes returns a defensive copy of b (nil stays nil).
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
