package workflow

// M25 §14.B1 — E-READ witnesses: authoritative by-ID submission inspection + bounded, resumable discovery
// that includes terminal queue-only records. All over real SQLite dispatch.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// B1 — InspectSubmission recovers the authoritative type/input/lifecycle BY ID for every state, INCLUDING a
// terminal record that never obtained a journal (a construction/seed failure or a cancellation before any
// action). Original type/input come from the durable queue row, independent of any journal.
func TestERead_InspectSubmission_ByIDAllStatesIncludingNoJournal(t *testing.T) {
	s := mkQueueStore(t)

	// pending
	_, err := s.Enqueue("p", "typeP", []byte(`{"k":"p"}`))
	require.NoError(t, err)
	// claimed (in-flight)
	_, err = s.Enqueue("c", "typeC", []byte(`{"k":"c"}`))
	require.NoError(t, err)
	_, err = s.ClaimNext("worker", "typeC")
	require.NoError(t, err)
	// terminal FAILED with NO journal (a construction/seed failure shape)
	_, err = s.Enqueue("f", "typeF", []byte(`{"k":"f"}`))
	require.NoError(t, err)
	_, err = s.ClaimNext("worker", "typeF")
	require.NoError(t, err)
	ok, err := s.MarkFailed("f")
	require.NoError(t, err)
	require.True(t, ok)
	_, lerr := s.Load("f")
	require.ErrorIs(t, lerr, ErrNotFound, "the failed record has NO journal")
	// terminal CANCELLED (cancel before any action) with no journal
	_, err = s.Enqueue("x", "typeX", []byte(`{"k":"x"}`))
	require.NoError(t, err)
	okc, err := s.CancelPending("x")
	require.NoError(t, err)
	require.True(t, okc)

	cases := []struct{ id, typ, state, k string }{
		{"p", "typeP", wqPending, "p"},
		{"c", "typeC", wqClaimed, "c"},
		{"f", "typeF", wqFailed, "f"},
		{"x", "typeX", wqCancelled, "x"},
	}
	for _, tc := range cases {
		sub, err := s.InspectSubmission(tc.id)
		require.NoError(t, err, tc.id)
		require.Equal(t, tc.id, sub.WorkflowID)
		require.Equal(t, tc.typ, sub.Type, "%s: original queued type recovered from the durable row", tc.id)
		require.Equal(t, tc.state, sub.State, tc.id)
		require.JSONEq(t, fmt.Sprintf(`{"k":%q}`, tc.k), string(sub.Input), "%s: exact original input recovered", tc.id)
		require.Positive(t, sub.EnqueuedAt)
	}
}

// B1 — a missing id is ErrNotFound, DISTINGUISHABLE from a storage failure (which is NOT ErrNotFound).
func TestERead_InspectSubmission_NotFoundVsStorageFailure(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.InspectSubmission("ghost")
	require.ErrorIs(t, err, ErrNotFound, "a missing submission is ErrNotFound")

	// Storage failure: drop the table → the read faults with a storage-domain error, NOT ErrNotFound.
	_, derr := s.db.Exec("DROP TABLE work_queue")
	require.NoError(t, derr)
	_, serr := s.InspectSubmission("anything")
	require.Error(t, serr)
	require.NotErrorIs(t, serr, ErrNotFound, "a storage failure is distinguishable from a missing id")
}

// B1 — the recovered type/input come from the durable QUEUE row, independent of any mutable journal value.
func TestERead_InspectSubmission_IndependentOfJournal(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.Enqueue("indep", "origType", []byte(`{"a":1}`))
	require.NoError(t, err)
	// A journal with a DIFFERENT value under the same id must not change the authoritative queued input.
	j := NewWorkflowData("indep")
	j.Set("a", 999)
	require.NoError(t, s.Save(j))

	sub, err := s.InspectSubmission("indep")
	require.NoError(t, err)
	require.Equal(t, "origType", sub.Type)
	require.JSONEq(t, `{"a":1}`, string(sub.Input), "the authoritative input is the queued bytes, not the journal")
}

// B1 — the returned input is a DEFENSIVE COPY: mutating it cannot corrupt a subsequent read.
func TestERead_InspectSubmission_InputDefensiveCopy(t *testing.T) {
	s := mkQueueStore(t)
	_, err := s.Enqueue("cp", "t", []byte(`{"v":"orig"}`))
	require.NoError(t, err)
	sub, err := s.InspectSubmission("cp")
	require.NoError(t, err)
	for i := range sub.Input {
		sub.Input[i] = 'Z'
	}
	again, err := s.InspectSubmission("cp")
	require.NoError(t, err)
	require.JSONEq(t, `{"v":"orig"}`, string(again.Input), "a caller mutation did not reach the store")
}

// B1 — bounded, resumable discovery paginates a dataset LARGER than the page bound, resumes the cursor, and
// discovers ALL qualifying records (including terminal queue-only ones) with no omissions or duplicates.
func TestERead_ListSubmissions_PaginationResumeNoOmissions(t *testing.T) {
	s := mkQueueStore(t)
	const n = 25
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("wf-%03d", i)
		want[id] = true
		_, err := s.Enqueue(id, "t", []byte(`{}`))
		require.NoError(t, err)
	}
	// Terminalize a spread of them so the dataset spans pending + terminal queue-only rows.
	for _, id := range []string{"wf-002", "wf-010", "wf-024"} {
		_, err := s.ClaimNext("w", "t")
		require.NoError(t, err)
		_ = id
	}
	// (ClaimNext takes the oldest; terminalize whatever got claimed so some rows are terminal.)
	for _, id := range []string{"wf-000", "wf-001", "wf-002"} {
		_, _ = s.MarkDone(id) //nolint:errcheck // best-effort: only claimed rows flip; the rest stay pending
	}

	seen := make(map[string]int)
	var cursor *SubmissionCursor
	pages := 0
	for {
		page, err := s.ListSubmissions(nil, 10, cursor) // nil states = ALL states, incl terminal
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 10, "a page never exceeds the bound")
		for _, it := range page.Items {
			seen[it.WorkflowID]++
		}
		pages++
		if page.Next == nil {
			break
		}
		cursor = page.Next
		require.Less(t, pages, 100, "pagination terminates")
	}
	require.GreaterOrEqual(t, pages, 3, "a >bound dataset spans multiple pages")
	require.Len(t, seen, n, "every submission discovered")
	for id := range want {
		require.Equal(t, 1, seen[id], "%s discovered exactly once (no omission, no duplicate)", id)
	}
}

// B1 — a state filter discovers terminal queue-only records; ordering is stable (enqueued_at, workflow_id).
func TestERead_ListSubmissions_StateFilterAndOrdering(t *testing.T) {
	s := mkQueueStore(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		_, err := s.Enqueue(id, "t", []byte(`{}`))
		require.NoError(t, err)
	}
	// Claim + fail the two oldest (a, b) → terminal queue-only failed rows.
	for i := 0; i < 2; i++ {
		it, err := s.ClaimNext("w", "t")
		require.NoError(t, err)
		_, err = s.MarkFailed(it.WorkflowID)
		require.NoError(t, err)
	}
	// Filter to terminal 'failed' → exactly a, b, in FIFO order.
	page, err := s.ListSubmissions([]string{wqFailed}, 100, nil)
	require.NoError(t, err)
	require.Nil(t, page.Next)
	ids := make([]string, len(page.Items))
	for i, it := range page.Items {
		ids[i] = it.WorkflowID
		require.Equal(t, wqFailed, it.State)
	}
	require.Equal(t, []string{"a", "b"}, ids, "terminal queue-only rows discoverable, FIFO ordered")

	// Filter to pending → c, d.
	page, err = s.ListSubmissions([]string{wqPending}, 100, nil)
	require.NoError(t, err)
	pids := make([]string, len(page.Items))
	for i, it := range page.Items {
		pids[i] = it.WorkflowID
	}
	require.Equal(t, []string{"c", "d"}, pids)
}

// B1 — concurrent-change semantics: a row APPENDED after the cursor position is observed on continuation, and a
// state change between pages is reflected in the later page (point-in-time per page, keyset-stable).
func TestERead_ListSubmissions_ConcurrentChangeSemantics(t *testing.T) {
	s := mkQueueStore(t)
	for _, id := range []string{"r1", "r2", "r3"} {
		_, err := s.Enqueue(id, "t", []byte(`{}`))
		require.NoError(t, err)
	}
	// Page 1 (bound 2) → r1, r2; Next set.
	p1, err := s.ListSubmissions(nil, 2, nil)
	require.NoError(t, err)
	require.Len(t, p1.Items, 2)
	require.Equal(t, []string{"r1", "r2"}, []string{p1.Items[0].WorkflowID, p1.Items[1].WorkflowID})
	require.NotNil(t, p1.Next)

	// Between pages: APPEND r4 (a later enqueued_at) and change r3's state (claim the three oldest in FIFO
	// order — r1, r2, r3 — so r3 is now claimed; r1/r2 are already behind the cursor).
	_, err = s.Enqueue("r4", "t", []byte(`{}`))
	require.NoError(t, err)
	var lastClaimed string
	for i := 0; i < 3; i++ {
		it, cerr := s.ClaimNext("w", "t")
		require.NoError(t, cerr)
		lastClaimed = it.WorkflowID
	}
	require.Equal(t, "r3", lastClaimed, "the third-oldest claimed is r3")

	// Continue: the appended r4 IS observed, and r3 shows its NEW state.
	p2, err := s.ListSubmissions(nil, 10, p1.Next)
	require.NoError(t, err)
	got := map[string]string{}
	for _, x := range p2.Items {
		got[x.WorkflowID] = x.State
	}
	require.Contains(t, got, "r3", "the row after the cursor is discovered")
	require.Equal(t, wqClaimed, got["r3"], "its state reflects the change made between pages")
	require.Contains(t, got, "r4", "a row appended after the cursor is observed on continuation")

	// No row is seen twice for the fixed cursor sequence: r1/r2 (page 1) are absent from page 2.
	require.NotContains(t, got, "r1")
	require.NotContains(t, got, "r2")
}

// B1 — the limit is clamped so a call can never be an unbounded dump; a 0/negative limit uses the default.
func TestERead_ListSubmissions_LimitBounds(t *testing.T) {
	s := mkQueueStore(t)
	total := DefaultSubmissionPageLimit + 5
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("z-%04d", i)
		ids = append(ids, id)
		_, err := s.Enqueue(id, "t", []byte(`{}`))
		require.NoError(t, err)
	}
	// limit 0 → clamped to the default page size (NOT the whole table), with a continuation cursor.
	page, err := s.ListSubmissions(nil, 0, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, DefaultSubmissionPageLimit, "a 0 limit clamps to the default page bound")
	require.NotNil(t, page.Next, "more remains → a cursor is returned (no unbounded dump)")

	// Walk the rest and confirm the union is the full set (sorted, no omission).
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.WorkflowID] = true
	}
	for page.Next != nil {
		page, err = s.ListSubmissions(nil, 0, page.Next)
		require.NoError(t, err)
		for _, it := range page.Items {
			seen[it.WorkflowID] = true
		}
	}
	got := make([]string, 0, len(seen))
	for id := range seen {
		got = append(got, id)
	}
	sort.Strings(got)
	require.Equal(t, ids, got, "the full dataset is discoverable across bounded pages")
}
