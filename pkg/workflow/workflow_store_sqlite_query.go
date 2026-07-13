package workflow

// WorkflowQuery (M15 ph70, SQL-05) — additive optional indexed visibility. A store that
// implements it answers the operability questions a durable engine needs — "which workflows
// have a Waiting/Failed/… node" + "which are rolling back" + an updated_at recency filter —
// CHEAPLY via indexes (ph70 added idx_nodes_status + idx_workflows_rolling_back), NOT the
// composition-only ListWorkflows→Load-each-and-decode fallback (which deserializes every run).
// The updated_at recency filter is a RESIDUAL on the rows those two indexes already fetch (it
// does NOT get its own index — that would be write-amplification for no read gain; ph70-F1).
// Type-asserted like Checkpointer/Syncer/IncrementalCheckpointer; a store WITHOUT it is
// unaffected (callers use the fallback).
//
// DESIGN (DEC-ph70, Option A): the interface exposes honest PRIMITIVES over the real data
// model (workflow status is DERIVED — there is no workflows.status column; status lives
// per-node), NOT contested run-level buckets. "which are Waiting" = ListByNodeStatus(Waiting);
// "which have a Failed node" = ListByNodeStatus(Failed); "terminally failed" (Failed ∧
// ¬RollingBack) or "Completed" (a negative) are the CALLER's composition from these
// primitives + ListRollingBack — the interface does not force a taxonomy.

import (
	"context"
	"fmt"
)

// WorkflowQuery is the additive optional indexed-visibility interface (ph70, SQL-05).
type WorkflowQuery interface {
	// ListByNodeStatus returns the DISTINCT workflow IDs that have at least one node in the
	// given status (e.g. Waiting → parked runs; Failed → runs with a failed node). since>0
	// additionally filters to workflows whose updated_at >= since (unix-nanos); since<=0
	// means no recency filter. Indexed (idx_nodes_status); does NOT decode any workflow.
	ListByNodeStatus(status NodeStatus, since int64) ([]string, error)
	// ListRollingBack returns the workflow IDs currently rolling back (M12). since>0 filters
	// to updated_at >= since. Indexed (idx_workflows_rolling_back); no decode.
	ListRollingBack(since int64) ([]string, error)
}

// ListByNodeStatus implements WorkflowQuery. It rejects an unknown status (corrupt input
// guard) up front, then runs an indexed DISTINCT scan of nodes, joined to workflows only
// when a since filter is set (nodes has no updated_at — recency is the run's updated_at).
func (s *SQLiteStore) ListByNodeStatus(status NodeStatus, since int64) ([]string, error) {
	if !isKnownStatus(status) {
		return nil, fmt.Errorf("%w: unknown node status %q", ErrValidation, status)
	}
	ctx := context.Background()
	if since > 0 {
		// Join to workflows for the updated_at recency filter (the run's, not the node's).
		return s.queryIDs(ctx,
			`SELECT DISTINCT n.workflow_id FROM nodes n
			 JOIN workflows w ON w.id = n.workflow_id
			 WHERE n.status = ? AND w.updated_at >= ?
			 ORDER BY n.workflow_id`,
			string(status), since)
	}
	return s.queryIDs(ctx,
		`SELECT DISTINCT workflow_id FROM nodes WHERE status = ? ORDER BY workflow_id`,
		string(status))
}

// ListRollingBack implements WorkflowQuery — the rolling_back runs, optionally recency-filtered.
func (s *SQLiteStore) ListRollingBack(since int64) ([]string, error) {
	ctx := context.Background()
	if since > 0 {
		return s.queryIDs(ctx,
			`SELECT id FROM workflows WHERE rolling_back = 1 AND updated_at >= ? ORDER BY id`, since)
	}
	return s.queryIDs(ctx,
		`SELECT id FROM workflows WHERE rolling_back = 1 ORDER BY id`)
}

// queryIDs runs a single-column workflow-ID query and collects the results. A scan/rows
// error surfaces as ErrCorruptData (never a panic), matching Load's error taxonomy.
func (s *SQLiteStore) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: query: %w", ErrIO, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: scan id: %w", ErrCorruptData, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows: %w", ErrCorruptData, err)
	}
	return ids, nil
}
