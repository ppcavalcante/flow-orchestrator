package workflow

// ph76 hard-bar: the MP Locker wired into Execute end-to-end — re-claim-after-death resumes from
// the COMMITTED frontier (exactly-once), abort-on-supersession, MINOR-6 at the drive level.
// Deterministic (FakeClock lease-lapse); MP store + MP Locker; no real subprocesses (that's ph77).

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runCounter counts, per node name, how many times an action actually executed — the exactly-once
// oracle (a re-run of a committed level bumps its count above 1).
type runCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func newRunCounter() *runCounter { return &runCounter{n: map[string]int{}} }
func (c *runCounter) inc(name string) {
	c.mu.Lock()
	c.n[name]++
	c.mu.Unlock()
}
func (c *runCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

// mkMPWorkflow builds a Workflow on the given MP store with an MP Locker for `owner`, driving a
// 2-level chain n0 → n1 whose actions bump the counter. Shares the FakeClock for lease-lapse.
func mkMPWorkflow(t *testing.T, store *SQLiteStore, owner string, ctr *runCounter) *Workflow {
	t.Helper()
	d := NewDAG("mp")
	require.NoError(t, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n0")
		return nil
	}))))
	require.NoError(t, d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n1")
		return nil
	}))))
	require.NoError(t, d.AddDependency("n0", "n1"))
	w := &Workflow{DAG: d, WorkflowID: "wf", Store: store}
	return w.WithLocker(newMultiProcessLocker(store, owner))
}

// TestMPLocker_ReClaimAfterDeath_ExactlyOnce — MAJOR-4 end-to-end. Worker A (MP Locker, token N)
// runs the workflow but "dies" after committing level 0 (n0). The lease lapses (FakeClock). Worker
// B (MP Locker, token N+1) claims + Executes → RESUMES from n0's committed state (n0 NOT re-run),
// runs n1, completes. Exactly-once: n0 ran exactly once (by A), n1 exactly once (by B).
func TestMPLocker_ReClaimAfterDeath_ExactlyOnce(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "mp.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
		return s
	}
	ctr := newRunCounter()

	// Worker A models a REAL DEATH (reviewer m16-ph76-F2): it CLAIMs (token 1), commits level 0's
	// state durably, then "crashes" — it does NOT Release, so the lease ROW SURVIVES (a clean
	// Execute would Release and delete the row, making the later re-claim a fresh token-1 INSERT,
	// not a lapse-bump; that was the F2 defect). We drive A's L0 commit directly (claim + a Save of
	// n0=Completed) to leave the lease row present + the token stale.
	sA := open()
	tokA, err := sA.Claim("wf", "A")
	require.NoError(t, err)
	require.Equal(t, FencingToken(1), tokA)
	dL0 := NewWorkflowData("wf")
	ctr.inc("n0") // A's action ran once
	dL0.SetNodeStatus("n0", Completed)
	require.NoError(t, sA.Save(dL0), "A durably commits level 0 under token 1")
	// A crashes here — NO Release. The lease row (owner A, token 1) stays.

	// A "dies": advance the clock past the TTL → A's lease LAPSES (re-claimable).
	clk.Advance(6 * time.Second)

	// Worker B: the FULL n0→n1 chain, MP Locker DERIVED from its own store (F1-safe). Execute →
	// Acquire re-claims the LAPSED lease → token 2 (bump); Store.Load returns n0's committed
	// Completed state → n0 is SKIPPED (not re-run), n1 runs, completes.
	//
	// We capture the live lease token FROM INSIDE n1's action (mid-drive, while the lease row is
	// still present): a clean Execute Releases (DELETEs) the lease row at drive end, so a
	// post-Execute SELECT finds NO row (that is correct handoff behavior — the successor need not
	// wait for expiry). Observing token 2 inside n1 is the STRONGER witness anyway: n1 only runs
	// because B resumed from n0's committed frontier (n0 skipped), so this single read proves BOTH
	// the lapse-reclaim bump (token 2, not a fresh token-1 INSERT) AND the committed-frontier resume.
	var tokenDuringDrive int64
	sB := open()
	dB := NewDAG("mp")
	require.NoError(t, dB.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n0")
		return nil
	}))))
	require.NoError(t, dB.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n1")
		require.NoError(t, sB.db.QueryRow(`SELECT fencing_token FROM leases WHERE workflow_id='wf'`).Scan(&tokenDuringDrive))
		return nil
	}))))
	require.NoError(t, dB.AddDependency("n0", "n1"))
	wB := (&Workflow{DAG: dB, WorkflowID: "wf", Store: sB}).WithMultiProcessLocker("B")
	require.NoError(t, wB.Execute(context.Background()))

	// The re-claim really BUMPED the token (proving the lapse-reclaim path ran, not a fresh INSERT).
	// Captured mid-drive inside n1 — which itself only runs on a committed-frontier resume.
	require.Equal(t, int64(2), tokenDuringDrive, "B re-claimed the LAPSED lease → token bumped to 2 (not a fresh token-1 INSERT)")

	// EXACTLY-ONCE across the death→bump→resume path: n0 ran once (by A, resumed-skipped by B), n1
	// ran once (by B).
	require.Equal(t, 1, ctr.get("n0"), "MAJOR-4: n0 must NOT re-run on re-claim — resumed from the committed frontier")
	require.Equal(t, 1, ctr.get("n1"), "n1 runs once on the re-claimer's completion")
}

// TestMPLocker_ReClaimAfterDeath_SeedBreak — reset-to-empty resume (skip Store.Load) → n0 RE-RUNS
// → the exactly-once oracle reddens. Proves the resume-from-committed-frontier is load-bearing.
func TestMPLocker_ReClaimAfterDeath_SeedBreak(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "mp.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
		return s
	}
	ctr := newRunCounter()

	sA := open()
	dA := NewDAG("mp")
	require.NoError(t, dA.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n0")
		return nil
	}))))
	wA := (&Workflow{DAG: dA, WorkflowID: "wf", Store: sA}).WithLocker(newMultiProcessLocker(sA, "A"))
	require.NoError(t, wA.Execute(context.Background()))
	require.Equal(t, 1, ctr.get("n0"))

	clk.Advance(6 * time.Second)

	// SEED-BREAK (differential — no production reset-toggle to ship): the reset-to-empty resume is
	// modeled by driving a workflow with NO committed state (a fresh WorkflowID so Store.Load finds
	// nothing). That is exactly what a re-claimer that IGNORED the committed frontier would see —
	// and it RE-RUNS n0. The real re-claim (TestMPLocker_ReClaimAfterDeath_ExactlyOnce, same store,
	// committed frontier present) does NOT re-run n0. The differential proves the committed-frontier
	// resume (Store.Load returning the durable state) is the load-bearing thing preventing double-apply.
	sB := open()
	dB := NewDAG("mp")
	require.NoError(t, dB.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
		ctr.inc("n0")
		return nil
	}))))
	// wfFresh = a workflow with NO committed state → resume-from-empty → n0 re-runs.
	wB := (&Workflow{DAG: dB, WorkflowID: "wfFresh", Store: sB}).WithLocker(newMultiProcessLocker(sB, "B"))
	require.NoError(t, wB.Execute(context.Background()))

	// The reset-to-empty path RE-RAN n0 → count 2. This is the double-apply the committed-frontier
	// resume prevents; the seed-break reproduces it, proving the real path's resume is load-bearing.
	require.Equal(t, 2, ctr.get("n0"), "seed-break: a reset-to-empty resume RE-RUNS the committed level (double-apply) — the committed-frontier resume prevents this")
}

// TestMPLocker_AbortOnSupersession — a drive whose checkpoint is fenced (a re-claim bumped the
// token mid-run) ABORTS at the Execute return with ErrFencedOut, no partial write. Here: A holds
// token 1, B re-claims (token 2) BEFORE A's drive, then A Executes → its Save is fenced → Execute
// returns ErrFencedOut (the drive aborted). Distinguishable from a transient error.
func TestMPLocker_AbortOnSupersession(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "mp.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
		return s
	}
	ctr := newRunCounter()

	// A claims token 1 (via the Locker Acquire path — but we drive its Execute below). To make A
	// superseded MID-DRIVE deterministically, we pre-arrange: A's store holds token 1, B re-claims
	// token 2 first, THEN A drives → A's checkpoint/Save fences.
	sA := open()
	_, err := sA.Claim("wf", "A") // A holds token 1
	require.NoError(t, err)
	clk.Advance(6 * time.Second) // lapse so B can re-claim
	sB := open()
	_, err = sB.Claim("wf", "B") // B re-claims → token 2; A (sA) is now stale
	require.NoError(t, err)

	// A drives with its (now-stale) token-1 store. Its final Save is the fencing point.
	wA := mkMPWorkflow(t, sA, "A", ctr)
	// A's Locker Acquire would RE-CLAIM (lease lapsed) and bump to token 3 — which would UN-fence
	// A. To model a superseded ZOMBIE that does NOT re-claim (it thinks it still holds token 1),
	// drive executeLocked-equivalent WITHOUT the Locker re-claim: use the default in-proc locker
	// on wA so Acquire does NOT touch the lease, and A keeps its stale tokenState (token 1).
	wA = wA.WithLocker(NewInProcessLocker())
	sA.setToken("wf", 1) // A's stale in-process token
	execErr := wA.Execute(context.Background())

	require.Error(t, execErr, "a superseded drive must return an error, not a false-clean")
	require.ErrorIs(t, execErr, ErrFencedOut, "abort-on-supersession: the fenced checkpoint's ErrFencedOut reaches Execute's return")
	require.True(t, isSupersededError(execErr), "the caller can classify it as superseded (abort, do NOT retry)")
}

// TestMPLocker_AbortOnSupersession_Differential — the abort's load-bearing bite, done as a
// DIFFERENTIAL (no production swallow-toggle to ship): the IDENTICAL drive returns ErrFencedOut
// when SUPERSEDED but nil when NOT superseded. So the fence is what makes the superseded drive
// abort — remove the supersession (the only difference) and the drive completes clean. This
// proves the abort is caused by the fence, not by some unrelated failure, AND that a drive which
// (hypothetically) swallowed the fence would return the same nil the non-superseded drive does.
func TestMPLocker_AbortOnSupersession_Differential(t *testing.T) {
	drive := func(t *testing.T, supersede bool) error {
		t.Helper()
		clk := NewFakeClock(time.Unix(1000, 0))
		dbPath := filepath.Join(t.TempDir(), "mp.db")
		open := func() *SQLiteStore {
			s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(5*time.Second))
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
			return s
		}
		ctr := newRunCounter()
		sA := open()
		_, err := sA.Claim("wf", "A") // A holds token 1
		require.NoError(t, err)
		if supersede {
			clk.Advance(6 * time.Second)
			sB := open()
			_, err = sB.Claim("wf", "B") // B re-claims → token 2; A (token 1) is now stale
			require.NoError(t, err)
		}
		// A drives with its token-1 store, NOT re-claiming (in-proc Locker so Acquire doesn't touch
		// the lease). Its final Save fences ONLY if A was superseded.
		wA := mkMPWorkflow(t, sA, "A", ctr).WithLocker(NewInProcessLocker())
		sA.setToken("wf", 1)
		return wA.Execute(context.Background())
	}

	// SUPERSEDED: the fence aborts the drive → ErrFencedOut.
	errSuperseded := drive(t, true)
	require.ErrorIs(t, errSuperseded, ErrFencedOut, "superseded drive aborts with ErrFencedOut")

	// NOT superseded (the ONLY difference): the same drive completes clean → nil. This is the
	// baseline a swallowed-fence would wrongly report even when superseded — the seed-break shape.
	errClean := drive(t, false)
	require.NoError(t, errClean, "the identical drive completes clean when NOT superseded — the fence is the sole cause of the abort")
}

// TestClassifyTxErr_BusyMapping — the ph77 unit bite for the SQLITE_BUSY → ErrBusy classifier. The
// SOUND classifier uses the modernc typed error (errors.As + Code()&0xFF == SQLITE_BUSY) with a
// string fallback keyed ONLY on the unambiguous "SQLITE_BUSY" token (review ph77-F2 — a bare "(5)"
// substring false-positives on a real IO fault like "disk I/O error (5)"). This pins: the token
// fallback → ErrBusy; a non-busy error (even one containing "(5)") stays ErrIO (do NOT tell a caller
// to retry a real fault). The full typed-error path is covered end-to-end by TestMPSave_BusyStarvation.
func TestClassifyTxErr_BusyMapping(t *testing.T) {
	// The unambiguous token fallback classifies as ErrBusy (transient, retryable).
	busyErr := classifyTxErr("commit", errors.New("SQLITE_BUSY: the database file is locked"))
	require.ErrorIs(t, busyErr, ErrBusy, "a SQLITE_BUSY token → ErrBusy (transient, retryable)")
	require.False(t, errors.Is(busyErr, ErrIO), "a busy error must NOT also read as opaque ErrIO")
	require.Contains(t, busyErr.Error(), "SQLITE_BUSY", "the raw driver error is preserved in the %w chain")

	// F2 FIX: a real IO fault whose text merely CONTAINS "(5)" must NOT be misclassified as retryable.
	ioWith5 := classifyTxErr("commit", errors.New("disk I/O error (5)"))
	require.ErrorIs(t, ioWith5, ErrIO, `an IO fault containing "(5)" stays ErrIO — not a false-positive ErrBusy`)
	require.False(t, errors.Is(ioWith5, ErrBusy), `"(5)" alone must NOT classify as ErrBusy (ph77-F2)`)

	// A plain non-busy fault stays ErrIO.
	ioErr := classifyTxErr("begin", errors.New("no such table: leases"))
	require.ErrorIs(t, ioErr, ErrIO, "a non-busy error stays ErrIO")
	require.False(t, errors.Is(ioErr, ErrBusy), "a non-busy error must NOT be misclassified as ErrBusy")
	require.False(t, isBusy(nil), "isBusy(nil) is false")
}

// TestMPLocker_ErrFencedOut_vs_ErrBusy — MINOR-6 at the drive level: the two error classes a
// competing consumer must distinguish. ErrFencedOut = superseded (abort, do NOT retry); ErrBusy =
// transient contention (MAY retry). isSupersededError separates them.

func TestMPLocker_ErrFencedOut_vs_ErrBusy(t *testing.T) {
	require.True(t, isSupersededError(ErrFencedOut), "ErrFencedOut classifies as superseded → abort")
	// ph77: the REAL ErrBusy sentinel (transient) must NOT classify as superseded → a caller MAY retry.
	require.False(t, isSupersededError(ErrBusy), "ErrBusy is transient (write-lock contention) → NOT superseded → may retry")
	require.False(t, isSupersededError(errors.New("some transient IO error")), "a transient error is NOT superseded → may retry")
	// The two competing-consumer error classes are errors.Is-DISTINCT (the abort-vs-retry discriminator).
	require.False(t, errors.Is(ErrBusy, ErrFencedOut), "ErrBusy (retry) is distinct from ErrFencedOut (abort)")
	require.False(t, errors.Is(ErrFencedOut, ErrBusy), "ErrFencedOut (abort) is distinct from ErrBusy (retry)")
	require.False(t, errors.Is(ErrFencedOut, ErrClaimLost), "ErrFencedOut (superseded) is distinct from ErrClaimLost (lost the initial claim)")
}

// TestMPLocker_AcquireOnLiveForeignLease_ClaimLost — a drive whose workflow is owned by a LIVE
// lease held by ANOTHER worker gets ErrClaimLost from Acquire → Execute returns it (does not run).
func TestMPLocker_AcquireOnLiveForeignLease_ClaimLost(t *testing.T) {
	clk := NewFakeClock(time.Unix(1000, 0))
	dbPath := filepath.Join(t.TempDir(), "mp.db")
	open := func() *SQLiteStore {
		s, err := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(60*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup
		return s
	}
	ctr := newRunCounter()

	sA := open()
	_, err := sA.Claim("wf", "A") // A holds a LIVE lease (60s TTL, not advanced)
	require.NoError(t, err)

	// B tries to drive the same workflow while A's lease is live → Acquire → Claim → ErrClaimLost.
	sB := open()
	wB := mkMPWorkflow(t, sB, "B", ctr)
	execErr := wB.Execute(context.Background())
	require.ErrorIs(t, execErr, ErrClaimLost, "a live foreign lease → the competing consumer does not run (ErrClaimLost)")
	require.Equal(t, 0, ctr.get("n0"), "B ran nothing — the workflow is owned by A")
}
