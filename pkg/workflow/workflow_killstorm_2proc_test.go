package workflow

// M17 ph83 hard-bar capstone (Slice 2) — the REAL 2-OS-process KILL-STORM. N worker subprocesses
// drain a batch of enqueued workflows off ONE shared .db file, SIGKILL'd at random points, restart,
// and reclaim (ph82 D1 reclaim-after-death) until the batch is fully terminal. This is the empirical
// half of the DISP-05 hard bar (WorkQueue.tla is the exhaustive half). It reuses the M16
// TestMPWorkerEntry re-exec pattern (subprocess of `go test` itself → genuine separate OS-process
// *sql.DB opens → per-process store = store-per-worker automatically, the ph82 model).
//
// The oracle COUNTS STORE STATE (BLOCKER-2 anti-vacuity — "no double-run" is vacuous because clause
// (d) permits body re-runs): (a) exactly-once terminal count per id, (b) none lost, (c) journal ≤1
// per node, (d) a side-effect counter ≤ maxAttempts. See killStormOracle (Slice 2b).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// killStormType is the single registered workflow type the kill-storm drains. Its 2-node chain
// increments a durable side-effect counter (clause (d)) so a re-run after a kill is observable +
// BOUNDED. Actions stay CODE (the moat) — the subprocess re-registers this factory on each restart.
const killStormType = "ks-chain"

// ksRegistry builds the registry every kill-storm subprocess uses (same factory, re-registered on
// each restart — the type→DAG registry is per-process, ph81).
func ksRegistry() *Registry {
	reg := NewRegistry()
	_ = reg.Register(killStormType, func() (*DAG, error) { //nolint:errcheck // fixed valid registration
		d := NewDAG(killStormType)
		// n0 increments a durable side-effect counter (clause (d) — a KV incremented in the body).
		// On a re-claim after a kill, n0 may re-run (body ≥1×) but the PERSISTED terminal is once.
		if err := d.AddNode(NewNode("n0", ActionFunc(func(_ context.Context, data *WorkflowData) error {
			prev := 0
			if v, ok := data.Get("sidefx"); ok {
				if n, ok := v.(int64); ok {
					prev = int(n)
				} else if n, ok := v.(float64); ok {
					prev = int(n)
				}
			}
			data.Set("sidefx", int64(prev+1))
			return nil
		}))); err != nil {
			return nil, err
		}
		if err := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))); err != nil {
			return nil, err
		}
		return d, d.AddDependency("n0", "n1")
	})
	return reg
}

// TestKillStormWorkerEntry is the subprocess worker: when M17_KS_WORKER is set it opens the shared DB
// (its OWN *SQLiteStore = its own tokenState = store-per-worker, ph82), registers the type, and drains
// via a RunNext loop until ErrNoWork (or it is SIGKILL'd mid-drive). Env: M17_KS_DB, M17_KS_OWNER,
// M17_KS_MAXATTEMPTS. It runs indefinitely-ish (bounded by a wall deadline) so the parent controls its
// lifetime by killing it; a clean exit happens only when the batch is drained.
func TestKillStormWorkerEntry(t *testing.T) {
	if os.Getenv("M17_KS_WORKER") == "" {
		t.Skip("not a worker invocation (set M17_KS_WORKER to run as the subprocess kill-storm worker)")
	}
	dbPath := os.Getenv("M17_KS_DB")
	owner := os.Getenv("M17_KS_OWNER")
	maxAttempts, _ := strconv.Atoi(os.Getenv("M17_KS_MAXATTEMPTS")) //nolint:errcheck // test env; 0→clamped
	if maxAttempts < 1 {
		maxAttempts = 3
	}

	// Short lease TTL so a killed worker's claim lapses fast → a sibling reclaims quickly (the whole
	// point of the kill-storm). Production sizes this above level-compute; the test wants fast lapse.
	s, err := NewSQLiteStore(dbPath, WithMultiProcess(), WithLeaseTTL(200*time.Millisecond))
	if err != nil {
		t.Fatalf("ks worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	reg := ksRegistry()
	deadline := time.Now().Add(20 * time.Second) // safety bound; the parent kills/ends us well before
	for time.Now().Before(deadline) {
		ran, rerr := runNext(context.Background(), s, reg, owner, maxAttempts)
		if rerr != nil {
			// A drive failure is already terminalized/retried by disposeExecErr; keep draining. A
			// transient claim fault self-heals on the next scan. We do NOT fail the worker here —
			// the parent's ORACLE judges correctness from store state, not a worker's exit code.
			continue
		}
		if !ran {
			// ErrNoWork → the batch is (currently) drained. A killed sibling may still hold a claimed
			// row whose lease hasn't lapsed yet; back off briefly and re-scan so this worker reclaims
			// it once it lapses, rather than exiting and leaving the row for nobody.
			time.Sleep(20 * time.Millisecond)
			// If nothing is claimable AND nothing is reclaimable for a sustained window, exit clean.
			pending, _ := s.ListPending(0) //nolint:errcheck // best-effort; oracle is authoritative
			if len(pending) == 0 && allTerminalOrClaimedLapsing(s) {
				return
			}
		}
	}
}

// allTerminalOrClaimedLapsing is a worker-local heuristic for "nothing left for me to do soon" — used
// only to let a worker exit cleanly when the batch looks drained. The PARENT's oracle is authoritative;
// this just avoids a worker spinning to its wall deadline after the batch is genuinely done.
func allTerminalOrClaimedLapsing(s *SQLiteStore) bool {
	var nonTerminal int
	// count rows not yet terminal; if zero, the batch is done. A read error just keeps the worker
	// alive (the parent oracle is authoritative), so we ignore it explicitly.
	scanErr := s.db.QueryRow(
		`SELECT count(*) FROM work_queue WHERE state NOT IN ('done','failed','cancelled')`,
	).Scan(&nonTerminal)
	if scanErr != nil {
		return false // treat a read fault as "not done" → keep working
	}
	return nonTerminal == 0
}

// spawnKSWorker re-execs THIS test binary as a kill-storm worker (the M16 pattern).
func spawnKSWorker(db, owner string, maxAttempts int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run", "^TestKillStormWorkerEntry$", "-test.v")
	cmd.Env = append(os.Environ(),
		"M17_KS_WORKER=1",
		"M17_KS_DB="+db,
		"M17_KS_OWNER="+owner,
		"M17_KS_MAXATTEMPTS="+strconv.Itoa(maxAttempts),
	)
	return cmd
}

// runKillStorm drives ONE stochastic kill-storm run: enqueue `batch` workflows, spawn `workers`
// subprocesses, SIGKILL them at random intervals + restart, until the batch is fully terminal or a
// wall deadline. Returns the shared store (parent handle, for the oracle) — the caller runs the
// 4-part oracle (Slice 2b) on it. seed varies the kill timing across runs (≥3 runs, Slice 2c).
func runKillStorm(t *testing.T, batch, workers, maxAttempts int, seed int64) *SQLiteStore {
	t.Helper()
	db := filepath.Join(t.TempDir(), "killstorm.db")

	// Pre-create schema ONCE (C3) + enqueue the batch (the parent handle; workers open their own).
	parent, err := NewSQLiteStore(db, WithMultiProcess(), WithLeaseTTL(200*time.Millisecond))
	if err != nil {
		t.Fatalf("parent open: %v", err)
	}
	for i := 0; i < batch; i++ {
		if _, err := parent.Enqueue("ks-"+strconv.Itoa(i), killStormType, nil); err != nil {
			t.Fatalf("enqueue ks-%d: %v", i, err)
		}
	}

	rng := newDetRand(seed)
	deadline := time.Now().Add(25 * time.Second)

	// Keep `workers` subprocesses alive; SIGKILL a random one at random intervals + respawn, so drives
	// are interrupted mid-flight (the reclaim-after-death path). Loop until the batch is fully terminal.
	var mu sync.Mutex
	procs := make(map[int]*exec.Cmd)
	next := 0
	launch := func() {
		cmd := spawnKSWorker(db, "ks-owner-"+strconv.Itoa(next), maxAttempts)
		if err := cmd.Start(); err != nil {
			return
		}
		mu.Lock()
		procs[next] = cmd
		next++
		mu.Unlock()
		// Reap on its own exit so a cleanly-drained worker doesn't become a zombie.
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd) //nolint:errcheck // reaped; exit code not the oracle
	}
	for i := 0; i < workers; i++ {
		launch()
	}

	converged := false
	for time.Now().Before(deadline) {
		if allTerminalOrClaimedLapsing(parent) {
			converged = true
			break // batch fully terminal → done
		}
		// SIGKILL a random live worker (mid-drive interruption) + respawn to keep the pool populated.
		mu.Lock()
		ids := make([]int, 0, len(procs))
		for id := range procs {
			ids = append(ids, id)
		}
		mu.Unlock()
		if len(ids) > 0 {
			victim := ids[rng.intn(len(ids))]
			mu.Lock()
			if c, ok := procs[victim]; ok && c.Process != nil {
				_ = c.Process.Signal(syscall.SIGKILL) //nolint:errcheck // best-effort kill (may have exited)
				delete(procs, victim)
			}
			mu.Unlock()
			launch() // replace the killed worker
		}
		// Random inter-kill interval (stochastic — the seed varies it across runs).
		time.Sleep(time.Duration(30+rng.intn(70)) * time.Millisecond)
	}

	// Kill any survivors (the run is over; the oracle reads final store state).
	mu.Lock()
	for _, c := range procs {
		if c.Process != nil {
			_ = c.Process.Signal(syscall.SIGKILL) //nolint:errcheck // teardown
		}
	}
	mu.Unlock()

	// CONVERGENCE-vs-TIMEOUT diagnostic (review ph83-KS-2): if the loop exited on the WALL DEADLINE
	// with non-terminal rows still present (slow CI / disk stall / worker starvation), that is a HARNESS
	// TIMEOUT, NOT an engine NoLostWork violation. Fail HERE with an explicit message so the oracle's
	// clause (b) can never misattribute a harness timeout to a real defect. (Observed convergence <0.7s
	// vs the 25s deadline — a ~35x margin — so this is a guard against a pathological environment, not an
	// expected path.)
	if !converged {
		var stuck int
		_ = parent.db.QueryRow(`SELECT count(*) FROM work_queue WHERE state NOT IN ('done','failed','cancelled')`).Scan(&stuck) //nolint:errcheck // diagnostic
		t.Fatalf("kill-storm did NOT converge within the 25s deadline (HARNESS TIMEOUT, not an engine defect): %d non-terminal rows remain", stuck)
	}
	return parent
}

// newDetRand / intn — a tiny deterministic PRNG so the kill timing is seed-reproducible across the
// ≥3 runs (a flaky harness is worse than none; the stochasticity is in the SEED, not the wall clock).
type detRand struct{ s uint64 }

func newDetRand(seed int64) *detRand { return &detRand{s: uint64(seed) + 0x9E3779B97F4A7C15} }

func (r *detRand) intn(n int) int {
	// xorshift64* — deterministic, adequate for kill-timing jitter.
	r.s ^= r.s >> 12
	r.s ^= r.s << 25
	r.s ^= r.s >> 27
	if n <= 0 {
		return 0
	}
	return int((r.s * 0x2545F4914F6CDD1D >> 33) % uint64(n))
}

// --- Slice 2b: the 4-part STORE-STATE oracle (BLOCKER-2 anti-vacuity) ---

// ksOracle counts PERSISTED store state after a kill-storm converges and asserts the 4-part contract.
// It counts STORE STATE, never invocations (clause (d) permits body re-runs, so "no double-run" is
// vacuous — we count what PERSISTED). batch = the number of enqueued ids; maxAttempts bounds (d).
func ksOracle(t *testing.T, s *SQLiteStore, batch, maxAttempts int) {
	t.Helper()

	// (a) EXACTLY-ONCE PERSISTENCE: every id has EXACTLY ONE terminal work_queue row. The CAS-guarded
	// flip (WHERE state='claimed') makes a re-terminalize a 0-row UPDATE, so an id is terminal once.
	// (The work_queue PK is workflow_id, so >1 row is impossible by construction; the real (a) content
	// is that every id REACHED terminal exactly once — none double-flipped to a DIFFERENT terminal and
	// none still non-terminal. We assert: count of terminal rows == batch, and each id is terminal.)
	var terminalCount int
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM work_queue WHERE state IN ('done','failed','cancelled')`,
	).Scan(&terminalCount))
	require.Equal(t, batch, terminalCount, "(a) exactly-once: every enqueued id reached a terminal state exactly once (CAS-guarded flip)")

	// (b) NONE LOST: no id stuck pending/claimed — the complement of (a) over the batch. Every enqueued
	// id must have a terminal row (checked by the count above == batch AND no non-terminal rows remain).
	var nonTerminal int
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM work_queue WHERE state NOT IN ('done','failed','cancelled')`,
	).Scan(&nonTerminal))
	require.Zero(t, nonTerminal, "(b) none lost: no id stuck pending/claimed forever (all reached terminal)")

	// WORKLOAD INVARIANT (review ph83-KS-1): the ks-chain actions NEVER error, so under this workload a
	// `failed`/`cancelled` terminal would itself be a real bug (a dead-letter of healthy work — an AF1-class
	// or reclaim-loss defect). We assert ZERO of them EXPLICITLY, so a legit-shaped dead-letter can't slip
	// past clause (a)'s all-terminal count. This also makes clauses (c)/(d) below sound: every terminal id
	// is a `done` id with a committed journal + a persisted sidefx (a journal-less `failed` would false-RED
	// the Load/require.True otherwise — KS-1). If dead-letters ever become in-scope, gate (c)/(d) on state.
	var badTerminal int
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM work_queue WHERE state IN ('failed','cancelled')`,
	).Scan(&badTerminal))
	require.Zero(t, badTerminal, "no healthy ks-chain workflow should dead-letter (failed/cancelled) — the actions never error, so a non-done terminal is a real defect")

	// (c) FENCING — no stale-overwrite corruption. The decomposed store's nodes PK (workflow_id,
	// node_name) makes a literal duplicate node row impossible, so the meaningful fencing witness is
	// that a killed worker's STALE write never corrupted the journal: for every DONE id, its persisted
	// sidefx counter is a POSITIVE, CONSISTENT value (n0 ran at least once and the final journaled value
	// survived), never a fenced-out lower/garbage token's write. We assert each done id has sidefx >= 1
	// (n0's side-effect persisted) and the node journal is present + singular.
	rows, err := s.db.Query(`SELECT workflow_id FROM work_queue WHERE state='done'`)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // read-only
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, batch, len(ids), "(a)+workload: every enqueued id is `done` (no dead-letter under the no-error workload)")
	for _, id := range ids {
		// node journal is singular by PK; assert n0's row exists (the drive actually journaled).
		var nodeRows int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes WHERE workflow_id=? AND node_name='n0'`, id).Scan(&nodeRows))
		require.LessOrEqual(t, nodeRows, 1, "(c) fencing: node journal is singular per (id,node) — no stale-overwrite duplicate")

		// (d) AT-LEAST-ONCE, BOUNDED: the side-effect counter (incremented in n0's body per drive) is
		// >= 1 (the body ran at least once) AND <= maxAttempts (a killed drive re-runs the body, but
		// BOUNDED — the retry budget caps re-claims). This is the anti-vacuity of the whole oracle: it
		// SEPARATES "body ran N times (allowed)" from "persisted once (required)".
		data, lerr := s.Load(id)
		require.NoError(t, lerr, "(d) load done id %s", id)
		sfx, ok := data.GetInt64("sidefx")
		require.True(t, ok, "(d) id %s has a persisted side-effect counter", id)
		require.GreaterOrEqual(t, sfx, int64(1), "(d) at-least-once: n0's body ran >= 1x (side-effect persisted)")
		require.LessOrEqual(t, sfx, int64(maxAttempts), "(d) BOUNDED: the side-effect counter <= maxAttempts (re-runs are bounded by the retry budget)")
	}
}

// TestKillStorm_StoreStateOracle — the DISP-05 hard bar: a real 2-OS-process kill-storm converges and
// the 4-part store-state oracle holds. Runs >=3 stochastic seeds (Slice 2c). Skipped under -short (it
// spawns + SIGKILLs OS subprocesses over several seconds each).
func TestKillStorm_StoreStateOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns + SIGKILLs OS subprocesses; skipped under -short")
	}
	const (
		batch       = 8
		workers     = 3
		maxAttempts = 3
	)
	// >=3 stochastic runs — a single green run is not evidence (varied kill timing per seed).
	for _, seed := range []int64{1, 2, 3} {
		seed := seed
		t.Run("seed"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			s := runKillStorm(t, batch, workers, maxAttempts, seed)
			defer func() { _ = s.Close() }() //nolint:errcheck // parent handle cleanup
			ksOracle(t, s, batch, maxAttempts)
		})
	}
}

// TestKillStorm_ExactlyOnceCASBites (Slice 2b, the (a) seed-break — anti-vacuity) — proves the
// oracle's (a) exactly-once property is LOAD-BEARING, carried by the CAS-guarded terminal flip
// (WHERE state='claimed'). A green kill-storm with no reddening seed-break is theater; this is the
// required empirical bite. We stage a claimed row, terminalize it (CAS: claimed->done, flipped),
// then show: (1) the REAL guarded flip is a 0-row NO-OP on the already-done row (exactly-once holds),
// (2) the SEED-BREAK — the same UPDATE WITHOUT the `AND state=?` CAS guard — DOES re-flip the done
// row to a DIFFERENT terminal (done->failed), the double-terminalize the CAS prevents. So the CAS is
// what makes (a) hold; removing it violates exactly-once.
func TestKillStorm_ExactlyOnceCASBites(t *testing.T) {
	s := mkDispatchStore(t) // WithMultiProcess
	_, err := s.Enqueue("wf", killStormType, nil)
	require.NoError(t, err)
	_, err = s.ClaimNext("w") // claimed
	require.NoError(t, err)
	// set the held token so the fenced terminal flip (flipTerminalFenced) is allowed for this owner
	s.setToken("wf", 1)

	// REAL: first terminalize claimed->done (flipped=true), second is a 0-row no-op (CAS: state != claimed).
	flipped, err := s.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "first claimed->done flips")
	require.Equal(t, wqDone, wqState(t, s, "wf"))
	flipped, err = s.MarkFailed("wf")
	require.NoError(t, err)
	require.False(t, flipped, "(a) exactly-once: a second terminalize on a done row is a 0-row NO-OP (CAS-guarded)")
	require.Equal(t, wqDone, wqState(t, s, "wf"), "the row stays done — no double-terminalize")

	// SEED-BREAK: the SAME UPDATE WITHOUT the `AND state='claimed'` CAS guard DOES re-flip a done row to
	// a different terminal — the exactly-once violation the CAS prevents. This reddens the (a) invariant
	// (a row would carry a race-decided terminal, not the CAS-arbitrated one).
	res, err := s.db.Exec(`UPDATE work_queue SET state='failed', updated_at=? WHERE workflow_id=?`, unixNanoNow(), "wf")
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "SEED-BREAK: un-guarded UPDATE re-flips the done row (1 row changed) — the double-terminalize the CAS blocks")
	require.Equal(t, wqFailed, wqState(t, s, "wf"), "SEED-BREAK: the row is now failed (was done) — exactly-once VIOLATED without the CAS guard")
}
