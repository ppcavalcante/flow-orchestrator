package workflow

// M18 ph85 Slice 2 — the Snapshot() MUTUAL-CONSISTENCY hard bar (OBS-RM-CONSISTENCY). The crux, red-team-
// corrected: because SetMaxOpenConns(1) + s.mu serialize a SAME-INSTANCE writer behind the reader, the
// concurrent writer MUST be a SEPARATE *SQLiteStore instance on the same file, so it genuinely contends at
// the SQLite/WAL layer. The invariant: Snapshot()'s per-state counts ALWAYS sum to N (a claimed->done flip
// is either fully before or fully after the read snapshot, never torn). Seed-break: an unwrapped snapshot
// (no BEGIN DEFERRED) can tear -> sums to N±1 -> reddens.

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// snapshotCountsTorn is the SEED-BREAK variant: it issues the per-state count query TWICE as SEPARATE
// auto-commit reads (no BEGIN DEFERRED enclosing txn), summing claimed + done across the two reads. With a
// separate-instance writer flipping claimed->done between the two reads, the two counts reflect DIFFERENT
// instants -> the sum tears to N±1. This is what Snapshot()'s single DEFERRED txn prevents.
func snapshotCountsTorn(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	one := func(state string) int {
		var n int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM work_queue WHERE state=?`, state).Scan(&n))
		return n
	}
	c := one("claimed") // read 1 (its own snapshot)
	d := one("done")    // read 2 (a LATER snapshot — the writer may have flipped between them)
	return c + d
}

// snapshotCountsConsistent uses the real Snapshot() (one BEGIN DEFERRED txn) and sums claimed+done from its
// mutually-consistent Counts.
// snapshotClaimedAgree returns whether the snapshot's TWO independent views of "claimed" agree: the
// per-state COUNT (from the GROUP BY query) vs. the length of the in-flight LIST (from the claimed-JOIN-
// leases query). These are DIFFERENT component queries, so agreement is only guaranteed when both read the
// SAME instant — i.e. only when Snapshot() encloses them in ONE BEGIN DEFERRED txn. This is the genuine
// mutual-consistency property (a single GROUP BY is atomic on its own; the txn's load-bearing value is
// making SEPARATE queries agree). Returns (countClaimed, lenInFlight).
func snapshotClaimedAgree(t *testing.T, s *SQLiteStore) (int, int) {
	t.Helper()
	snap, err := s.Snapshot(0, nil)
	require.NoError(t, err)
	return snap.Counts["claimed"], len(snap.InFlight)
}

// TestSnapshot_MutualConsistency — the real Snapshot()'s TWO independent claimed-views (the per-state
// count vs. the in-flight list length) ALWAYS AGREE under a separate-instance concurrent writer, because
// the BEGIN DEFERRED txn reads both at one instant. A count-vs-list disagreement would mean a claimed->done
// flip landed BETWEEN the two component queries — which the txn prevents.
func TestSnapshot_MutualConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a 2nd store instance + a tight concurrent write loop")
	}
	dbPath := filepath.Join(t.TempDir(), "snap.db")
	const N = 40

	// Store A = the reader. Enqueue N + claim them all (so every row is `claimed` at the start; the writer
	// flips claimed->done). WAL journal mode so the separate reader/writer genuinely use WAL snapshots.
	a, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer a.Close() //nolint:errcheck // cleanup
	for i := 0; i < N; i++ {
		id := "s" + itoa(i)
		_, err := a.Enqueue(id, "T", nil)
		require.NoError(t, err)
	}
	for i := 0; i < N; i++ {
		_, err := a.ClaimNext("wA")
		require.NoError(t, err)
	}

	// Store B = a SEPARATE instance on the same file (own conn/tokenState). It flips claimed->done in a
	// tight loop — genuine WAL contention (not same-instance, which s.mu would serialize into vacuity).
	b, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer b.Close() //nolint:errcheck // cleanup

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			// flip one row claimed<->done directly (B is a separate instance; raw UPDATE to bypass B's
			// tokenState fencing — we only want WAL-level write contention). Bidirectional so the claimed
			// count keeps MOVING (a monotone drain would settle at 0 claimed and stop exercising the race).
			id := "s" + itoa(i%N)
			_, _ = b.db.Exec(`UPDATE work_queue SET state=CASE state WHEN 'claimed' THEN 'done' ELSE 'claimed' END WHERE workflow_id=? AND state IN ('claimed','done')`, id) //nolint:errcheck // contention
			i++
		}
	}()

	// The real Snapshot() (BEGIN DEFERRED): its TWO independent claimed-views ALWAYS AGREE across many
	// reads — the per-state count == the in-flight list length. Disagreement would mean a flip landed
	// between the two component queries; the DEFERRED txn reads both at ONE instant, so they can't diverge.
	for r := 0; r < 300; r++ {
		cnt, list := snapshotClaimedAgree(t, a)
		require.Equal(t, cnt, list,
			"OBS-RM-CONSISTENCY: Snapshot()'s claimed COUNT (%d) == its in-flight LIST length (%d) — the two component queries agree at one instant (BEGIN DEFERRED)", cnt, list)
	}

	close(stop)
	wg.Wait()
}

// TestSnapshot_SeedBreak_TornReadReddens — proves the DEFERRED txn is LOAD-BEARING: the unwrapped
// two-read variant (no enclosing txn) DOES tear to N±1 under the same separate-instance writer, so a
// green consistency test without the txn would be theater. This test ASSERTS the tear is observable
// (it must find at least one non-N sum), which is what the real Snapshot() prevents.
func TestSnapshot_SeedBreak_TornReadReddens(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a 2nd store instance + a tight concurrent write loop")
	}
	dbPath := filepath.Join(t.TempDir(), "snaptear.db")
	const N = 40
	a, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer a.Close() //nolint:errcheck // cleanup
	for i := 0; i < N; i++ {
		_, err := a.Enqueue("s"+itoa(i), "T", nil)
		require.NoError(t, err)
	}
	for i := 0; i < N; i++ {
		_, err := a.ClaimNext("wA")
		require.NoError(t, err)
	}
	b, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer b.Close() //nolint:errcheck // cleanup

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			id := "s" + itoa(i%N)
			// alternate flip done<->claimed so the count keeps changing (a monotone drain would settle at N
			// once all are done; we want a CONTINUOUSLY moving target so the torn read has something to tear).
			_, _ = b.db.Exec(`UPDATE work_queue SET state=CASE state WHEN 'claimed' THEN 'done' ELSE 'claimed' END WHERE workflow_id=?`, id) //nolint:errcheck // contention
			i++
		}
	}()

	// The UNWRAPPED (seed-break) read tears: across many reads, at least one sum != N (the writer flipped
	// between the claimed-count read and the done-count read). This proves the DEFERRED txn is load-bearing.
	sawTear := false
	for r := 0; r < 5000 && !sawTear; r++ {
		if snapshotCountsTorn(t, a) != N {
			sawTear = true
		}
	}
	close(stop)
	wg.Wait()
	require.True(t, sawTear,
		"SEED-BREAK: the unwrapped two-read count MUST tear to != N under a separate-instance writer — proving Snapshot()'s BEGIN DEFERRED txn is load-bearing (a green test without it is theater)")
}
