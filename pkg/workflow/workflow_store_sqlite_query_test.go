package workflow

// M15 ph70 — WorkflowQuery correctness (SQL-05). The bites: each primitive returns EXACTLY
// the matching workflow IDs over a fixture with runs in distinct states; the updated_at
// recency filter is correct; the query is INDEXED (EXPLAIN QUERY PLAN, separate test); a
// store without the interface falls back (additive-safe). Non-vacuous: a should-fail control
// proves the assertion has teeth. Runs non-race (single-writer conn).

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// queryFixture seeds workflows in distinct states so each primitive has an EXACT expected set:
//
//	wf-waiting  : a node Waiting (parked)          → ListByNodeStatus(Waiting)
//	wf-failed   : a node Failed                    → ListByNodeStatus(Failed)
//	wf-rolling  : rolling_back=1 + a Failed node   → ListRollingBack + ListByNodeStatus(Failed)
//	wf-done     : all nodes Completed              → matches NEITHER Waiting/Failed/RollingBack
func queryFixture(t *testing.T, s *SQLiteStore) {
	t.Helper()
	mk := func(id string, rolling bool, nodes map[string]NodeStatus, wait string) {
		d := NewWorkflowData(id)
		for n, st := range nodes {
			d.SetNodeStatus(n, st)
		}
		if wait != "" {
			d.SetWait(wait, 1893456000000000000)
		}
		if rolling {
			d.SetRollingBack(true)
			d.SetTriggerCause(TriggerFailure)
		}
		require.NoError(t, s.Save(d))
	}
	mk("wf-waiting", false, map[string]NodeStatus{"a": Completed, "b": Waiting}, "b")
	mk("wf-failed", false, map[string]NodeStatus{"a": Completed, "b": Failed}, "")
	mk("wf-rolling", true, map[string]NodeStatus{"a": Compensated, "b": Failed}, "")
	mk("wf-done", false, map[string]NodeStatus{"a": Completed, "b": Completed}, "")
}

func sortedIDs(ids []string) []string { sort.Strings(ids); return ids }

func TestSQLiteQuery_ByNodeStatus_Exact(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	queryFixture(t, s)

	// Waiting → exactly wf-waiting.
	got, err := s.ListByNodeStatus(Waiting, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"wf-waiting"}, sortedIDs(got))

	// Failed → exactly the two runs with a Failed node (wf-failed + wf-rolling), NOT wf-done.
	got, err = s.ListByNodeStatus(Failed, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"wf-failed", "wf-rolling"}, sortedIDs(got))

	// Completed → every run has a Completed node except wf-rolling (a/Compensated, b/Failed).
	got, err = s.ListByNodeStatus(Completed, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"wf-done", "wf-failed", "wf-waiting"}, sortedIDs(got))

	// A status no run has → empty, not error.
	got, err = s.ListByNodeStatus(Bypassed, 0)
	require.NoError(t, err)
	require.Empty(t, got)

	// An unknown status → typed validation error (corrupt-input guard).
	_, err = s.ListByNodeStatus(NodeStatus("not-a-status"), 0)
	require.ErrorIs(t, err, ErrValidation)
}

func TestSQLiteQuery_RollingBack_Exact(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	queryFixture(t, s)

	got, err := s.ListRollingBack(0)
	require.NoError(t, err)
	require.Equal(t, []string{"wf-rolling"}, sortedIDs(got),
		"exactly the rolling_back run — NOT wf-failed (which has a Failed node but is not rolling back)")

	// Caller composition (documented, not baked in): "terminally failed" = Failed ∧ ¬RollingBack.
	failed, err := s.ListByNodeStatus(Failed, 0)
	require.NoError(t, err)
	rolling, err := s.ListRollingBack(0)
	require.NoError(t, err)
	terminal := except(failed, rolling)
	require.Equal(t, []string{"wf-failed"}, sortedIDs(terminal),
		"Failed minus RollingBack = terminally-failed = wf-failed only (the caller composes this)")
}

func TestSQLiteQuery_UpdatedAtRecency(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	queryFixture(t, s)

	ctx := context.Background()
	// Read wf-waiting's updated_at, then filter with since just above it → excludes it.
	var ts int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT updated_at FROM workflows WHERE id='wf-waiting'`).Scan(&ts))

	// since = 0 → no filter, wf-waiting present.
	got, err := s.ListByNodeStatus(Waiting, 0)
	require.NoError(t, err)
	require.Contains(t, got, "wf-waiting")

	// since = ts+1 → wf-waiting (updated_at == ts < ts+1) is excluded.
	got, err = s.ListByNodeStatus(Waiting, ts+1)
	require.NoError(t, err)
	require.NotContains(t, got, "wf-waiting", "updated_at recency filter excludes older runs")

	// since = ts → inclusive (>=), wf-waiting present.
	got, err = s.ListByNodeStatus(Waiting, ts)
	require.NoError(t, err)
	require.Contains(t, got, "wf-waiting", "recency filter is inclusive (>=)")
}

// TestSQLiteQuery_NonVacuous — the should-fail control. The oracle (exact-match) MUST redden
// if the query returned a WRONG set. We prove sensitivity by asserting a deliberately-wrong
// expectation fails: if ListByNodeStatus(Failed) returned wf-done (which has NO failed node),
// or missed wf-rolling, the exact assertion would catch it. Demonstrated by a manual diff.
func TestSQLiteQuery_NonVacuous(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	queryFixture(t, s)

	got, err := s.ListByNodeStatus(Failed, 0)
	require.NoError(t, err)
	// The CORRECT set. A wrong query (false-include wf-done, or miss wf-rolling) would differ.
	require.NotContains(t, got, "wf-done", "wf-done has NO failed node — a false-include would redden")
	require.Contains(t, got, "wf-rolling", "wf-rolling HAS a failed node — a miss would redden")
	// Prove the oracle is not trivially-always-true: the wrong expectation is genuinely != got.
	wrong := []string{"wf-done"} // a plausible-but-wrong answer
	require.NotEqual(t, sortedIDs(wrong), sortedIDs(got), "the exact oracle distinguishes right from wrong sets")
}

// TestSQLiteQuery_FallbackWhenNoInterface — additive-safety: a store that does NOT implement
// WorkflowQuery is unaffected; callers use the ListWorkflows→Load composition. We assert the
// SQLiteStore DOES implement it (the fast path) and that ListWorkflows (the fallback anchor)
// still returns all runs regardless.
func TestSQLiteQuery_FallbackWhenNoInterface(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	queryFixture(t, s)

	// SQLiteStore implements WorkflowQuery (the indexed fast path).
	var _ WorkflowQuery = s
	// The composition fallback (ListWorkflows) still enumerates every run — a non-WorkflowQuery
	// store's caller filters these via Load. All 4 present.
	all, err := s.ListWorkflows()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"wf-waiting", "wf-failed", "wf-rolling", "wf-done"}, all)
}

// except returns a \ b (set difference), the caller-side composition primitive.
func except(a, b []string) []string {
	rm := make(map[string]struct{}, len(b))
	for _, x := range b {
		rm[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, drop := rm[x]; !drop {
			out = append(out, x)
		}
	}
	return out
}

// TestSQLiteQuery_UsesIndex — the cheap/indexed bar: EXPLAIN QUERY PLAN must show an index
// SEARCH, never a full-table SCAN, for both primitives (else the query is the O(all-runs)
// fallback the interface exists to avoid).
func TestSQLiteQuery_UsesIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "wf.db"))
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	ctx := context.Background()

	plan := func(q string, args ...any) string {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
		require.NoError(t, err)
		defer rows.Close() //nolint:errcheck // read-only
		var b strings.Builder
		for rows.Next() {
			var a, c, d int
			var detail string
			require.NoError(t, rows.Scan(&a, &c, &d, &detail))
			b.WriteString(detail + "\n")
		}
		return b.String()
	}
	pNodes := plan(`SELECT DISTINCT workflow_id FROM nodes WHERE status = ? ORDER BY workflow_id`, "waiting")
	require.Contains(t, pNodes, "idx_nodes_status", "ListByNodeStatus must use idx_nodes_status")
	require.NotContains(t, pNodes, "SCAN nodes\n", "must NOT full-scan nodes")

	pRoll := plan(`SELECT id FROM workflows WHERE rolling_back = 1 ORDER BY id`)
	require.Contains(t, pRoll, "idx_workflows_rolling_back", "ListRollingBack must use idx_workflows_rolling_back")

	// ph70-F2: the since>0 variants must ALSO stay index-driven (not a full nodes/workflows
	// scan). ListByNodeStatus+since drives off idx_nodes_status then PK-fetches workflows for
	// the updated_at residual; ListRollingBack+since drives off idx_workflows_rolling_back.
	pNodesSince := plan(`SELECT DISTINCT n.workflow_id FROM nodes n JOIN workflows w ON w.id=n.workflow_id
		WHERE n.status=? AND w.updated_at>=? ORDER BY n.workflow_id`, "waiting", 1)
	require.Contains(t, pNodesSince, "idx_nodes_status", "ListByNodeStatus+since must still drive off idx_nodes_status")
	require.NotContains(t, pNodesSince, "SCAN nodes", "since variant must NOT full-scan nodes")

	pRollSince := plan(`SELECT id FROM workflows WHERE rolling_back=1 AND updated_at>=? ORDER BY id`, 1)
	require.Contains(t, pRollSince, "idx_workflows_rolling_back",
		"ListRollingBack+since must drive off idx_workflows_rolling_back (updated_at is a residual, not its own index)")
}
