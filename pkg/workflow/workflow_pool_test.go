package workflow

// M17 ph82 (D4) — the store-per-worker WORKER POOL, proven. The load-bearing test is
// TestPool_StorePerWorker_FencingSafety: a stalled worker A whose superseded checkpoint is FENCED
// (rejected) exactly because each worker opens its OWN *SQLiteStore (own tokenState map) via the
// factory — collapse the factory to one SHARED store and worker B's reclaim clobbers A's token, A's
// stale write LANDS, and the two [BITE] asserts redden (the seed-break in the fencing test's header).
// The rest cover the pool mechanics: happy-path drain (each item exactly once), bounded empty-queue
// backoff (no hot-spin, counted via a driver seam), and prompt graceful drain on ctx cancel.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sqlited "modernc.org/sqlite"
)

// countState returns the number of work_queue rows in `state`. Error-tolerant so it can be polled from
// require.Eventually while the pool writes concurrently (a transient SQLITE_BUSY read is retried).
func countState(s *SQLiteStore, state string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE state=?`, state).Scan(&n)
	return n, err
}

// TestNewPool_Guards — the constructor rejects a nil factory / nil registry / empty ownerID loud, and
// clamps unsafe option values to safe minimums (so a mis-set option degrades, never breaks the pool).
func TestNewPool_Guards(t *testing.T) {
	reg := NewRegistry()
	factory := func() (*SQLiteStore, error) { return nil, nil }

	_, err := NewPool(nil, reg, "w")
	require.ErrorIs(t, err, ErrValidation, "nil factory rejected")
	_, err = NewPool(factory, nil, "w")
	require.ErrorIs(t, err, ErrValidation, "nil registry rejected")
	_, err = NewPool(factory, reg, "")
	require.ErrorIs(t, err, ErrValidation, "empty ownerID rejected")

	p, err := NewPool(factory, reg, "w", WithPoolSize(0), WithPollInterval(-1), WithMaxAttempts(0))
	require.NoError(t, err)
	require.Equal(t, 1, p.size, "size < 1 clamped to 1")
	require.Equal(t, defaultPollInterval, p.pollInterval, "pollInterval <= 0 falls back to default")
	require.Equal(t, 1, p.maxAttempts, "maxAttempts < 1 clamped to 1")
}

// TestPool_StorePerWorker_FencingSafety (LOAD-BEARING) — the whole reason store-per-worker is the
// locked design. A stalled worker A (parked mid-drive, holding token 1) is superseded by worker B's
// reclaim (token 2); when A resumes, its level-0 checkpoint is a STALE-token write that the fencing
// CAS must REJECT, and nothing A wrote may land (exactly-once). Deterministic: FakeClock lapses the
// lease + a barrier stages the schedule (no kill-storm), mirroring TestRunNext_ReconciliationSeam_*.
//
// SEED-BREAK is documented inline (collapse the factory to a shared store). It must redden the [BITE]
// asserts — a non-biting fencing test is worse than none.
func TestPool_StorePerWorker_FencingSafety(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fence.db")
	clk := NewFakeClock(time.Unix(1000, 0))
	const ttl = 5 * time.Second

	// The per-worker store factory the Pool uses: each call opens a NEW *SQLiteStore on the SAME db
	// file (its OWN tokenState map + OWN *sql.DB). Per-worker isolation is the safety property tested.
	//
	// >>> SEED-BREAK — collapse to a SHARED store to prove per-worker isolation is load-bearing:
	//        shared, serr := NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
	//        require.NoError(t, serr)
	//        factory := func() (*SQLiteStore, error) { return shared, nil }
	//     Then worker B's reclaim (ClaimNext) writes shared.tokenState["wf"]=2, CLOBBERING worker A's
	//     tokenState["wf"]=1 → A's superseded checkpoint is NO LONGER fenced (held==current==2) → A's
	//     stale write LANDS → the two [BITE] asserts below REDDEN. (Revert to re-green.) <<<
	factory := func() (*SQLiteStore, error) {
		return NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteClock(clk), withSQLiteLeaseTTL(ttl))
	}

	// Enqueue "wf" (a throwaway store; the workers open their own). Closed at test end via t.Cleanup —
	// NOT inline — so the documented SEED-BREAK (a factory returning ONE shared store) does not close
	// the shared store out from under the workers.
	enq, err := factory()
	require.NoError(t, err)
	t.Cleanup(func() { _ = enq.Close() }) //nolint:errcheck // cleanup
	_, err = enq.Enqueue("wf", "chain", nil)
	require.NoError(t, err)

	// Barrier channels stage a DETERMINISTIC stalled-worker-then-reclaimer schedule.
	aInN0 := make(chan struct{})    // A signals it is parked mid-drive (inside n0, before its checkpoint)
	releaseA := make(chan struct{}) // main releases A after B has reclaimed
	aDone := make(chan error, 1)    // A's Execute result

	// Worker A: opens its OWN store, claims "wf" (token 1), drives it. n0 records a durable side-effect
	// then PARKS — modelling a worker stalled mid-drive BEFORE its level-0 checkpoint. When released,
	// that checkpoint is a STALE-token write.
	sA, err := factory()
	require.NoError(t, err)
	defer sA.Close() //nolint:errcheck // cleanup
	go func() {
		itemA, cerr := sA.ClaimNext("A")
		if cerr != nil {
			aDone <- fmt.Errorf("A ClaimNext: %w", cerr)
			return
		}
		if itemA.Token != 1 {
			aDone <- fmt.Errorf("A expected token 1, got %d", itemA.Token)
			return
		}
		d := NewDAG("chain")
		if e := d.AddNode(NewNode("n0", ActionFunc(func(_ context.Context, data *WorkflowData) error {
			data.Set("aWrote", "A") // A's side-effect — its checkpoint must be fenced (rejected)
			close(aInN0)            // parked: A holds token 1 and has NOT yet checkpointed level 0
			<-releaseA
			return nil
		}))); e != nil {
			aDone <- e
			return
		}
		if e := d.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))); e != nil {
			aDone <- e
			return
		}
		if e := d.AddDependency("n0", "n1"); e != nil {
			aDone <- e
			return
		}
		wA := &Workflow{DAG: d, WorkflowID: "wf", Store: sA}
		aDone <- wA.Execute(context.Background())
	}()

	<-aInN0              // A holds token 1 and is parked mid-drive
	clk.Advance(2 * ttl) // A's lease lapses (deterministic liveness lapse — no kill-storm)

	// Worker B: opens its OWN store and RECLAIMS "wf" (token 2). This is the exact operation whose
	// in-memory effect (sB.tokenState["wf"]=2) a SHARED store would let clobber A's token 1.
	sB, err := factory()
	require.NoError(t, err)
	defer sB.Close() //nolint:errcheck // cleanup
	itemB, err := sB.ClaimNext("B")
	require.NoError(t, err)
	require.Equal(t, "wf", itemB.WorkflowID, "B's broadened ClaimNext discovers the lapsed-claimed row")
	require.Equal(t, FencingToken(2), itemB.Token, "B's reclaim bumped the durable token (A superseded)")

	close(releaseA) // release A → its level-0 checkpoint fires under the now-STALE token 1
	aErr := <-aDone

	// [BITE 1] A's superseded checkpoint is REJECTED. Per-worker stores keep A's tokenState["wf"]=1 <
	// durable 2 → ErrFencedOut. With a SHARED store, B's reclaim clobbered A's token to 2 → A is NOT
	// fenced → aErr is nil and A's write LANDS (double-commit) → THIS REDDENS.
	require.ErrorIs(t, aErr, ErrFencedOut, "store-per-worker: worker A's superseded checkpoint is FENCED")

	// [BITE 2] ...and nothing A wrote is durable (exactly-once): a fresh reader finds NO journal for
	// "wf" (A's checkpoint + failed-state save were both fenced; B has not driven yet). With a shared
	// store A's stale write landed → Load returns A's data → THIS REDDENS.
	reader, err := factory()
	require.NoError(t, err)
	defer reader.Close() //nolint:errcheck // cleanup
	_, lerr := reader.Load("wf")
	require.ErrorIs(t, lerr, ErrNotFound, "the fenced write did NOT land durably — no double-commit")

	// Exactly-once capstone (reached only when the fencing above HELD): B, the current-token owner,
	// drives "wf" to completion on its OWN store and terminalizes the row once. A has returned, so the
	// process-wide in-process drive lock for "wf" is free.
	dB := NewDAG("chain")
	require.NoError(t, dB.AddNode(NewNode("n0", ActionFunc(func(_ context.Context, data *WorkflowData) error {
		data.Set("winner", "B")
		return nil
	}))))
	require.NoError(t, dB.AddNode(NewNode("n1", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))))
	require.NoError(t, dB.AddDependency("n0", "n1"))
	wB := &Workflow{DAG: dB, WorkflowID: "wf", Store: sB}
	require.NoError(t, wB.Execute(context.Background()), "B (current token) drives to completion, unfenced")
	flipped, err := sB.MarkDone("wf")
	require.NoError(t, err)
	require.True(t, flipped, "B terminalizes the row exactly once")

	got, err := reader.Load("wf")
	require.NoError(t, err)
	winner, _ := got.GetString("winner")
	require.Equal(t, "B", winner, "the durable journal reflects ONLY the current-token owner B (exactly-once)")
}

// TestPool_HappyPath_DrainsQueue — N store-per-worker goroutines drain a queue of M items; every item
// runs EXACTLY once (a shared atomic run-counter == M) and every queue row is terminal `done`.
func TestPool_HappyPath_DrainsQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "happy.db")
	factory := func() (*SQLiteStore, error) { return NewSQLiteStore(dbPath, WithMultiProcess()) }

	const items = 24
	var ran atomic.Int64
	reg := NewRegistry()
	require.NoError(t, reg.Register("job", func() (*DAG, error) {
		d := NewDAG("job")
		return d, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error {
			ran.Add(1)
			return nil
		})))
	}))

	enq, err := factory()
	require.NoError(t, err)
	defer enq.Close() //nolint:errcheck // cleanup
	for i := range items {
		_, eerr := enq.Enqueue(fmt.Sprintf("wf-%d", i), "job", nil)
		require.NoError(t, eerr)
	}

	pool, err := NewPool(factory, reg, "happy", WithPoolSize(4), WithPollInterval(10*time.Millisecond))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	require.Eventually(t, func() bool {
		n, cerr := countState(enq, wqDone)
		return cerr == nil && n == items
	}, 15*time.Second, 5*time.Millisecond, "all %d items drain to done", items)

	cancel()
	require.NoError(t, <-done, "graceful drain returns nil")

	require.Equal(t, int64(items), ran.Load(), "each workflow ran EXACTLY once (no reclaim double-run)")
	n, err := countState(enq, wqDone)
	require.NoError(t, err)
	require.Equal(t, items, n, "every queue row is terminal `done`")
}

// TestPool_EmptyQueue_NoHotSpin (red-team MAJOR-5) — an idle pool must NOT hot-spin. We count the
// work_queue claim-scans (via a counting driver seam) over a fixed window: correct backoff does
// ~workers*(window/pollInterval) scans; a hot-spin (no ctx-cancellable sleep) does THOUSANDS. The
// generous upper bound bites the bug without flaking on scheduler jitter.
func TestPool_EmptyQueue_NoHotSpin(t *testing.T) {
	drv := registerCountDriver()
	dbPath := filepath.Join(t.TempDir(), "idle.db")

	// A registered type so runNext actually calls ClaimNext (an empty registry short-circuits BEFORE
	// the scan and would count zero, hiding a hot-spin).
	reg := NewRegistry()
	require.NoError(t, reg.Register("job", func() (*DAG, error) {
		d := NewDAG("job")
		return d, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	}))

	factory := func() (*SQLiteStore, error) {
		return NewSQLiteStore(dbPath, WithMultiProcess(), withSQLiteDriverName(drv))
	}
	// Warm the schema once so the pool workers' own opens (migrations) don't skew the count.
	warm, err := factory()
	require.NoError(t, err)
	require.NoError(t, warm.Close())
	claimScans.Store(0)

	const (
		workers = 2
		poll    = 40 * time.Millisecond
		window  = 200 * time.Millisecond
	)
	pool, err := NewPool(factory, reg, "idle", WithPoolSize(workers), WithPollInterval(poll))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	time.Sleep(window)
	cancel()
	require.NoError(t, <-done)

	scans := claimScans.Load()
	// Correct backoff: ~workers*(window/poll+1) ≈ 2*6 = 12 scans. Bound = that × 4 (jitter margin);
	// a hot-spin makes THOUSANDS in the same 200ms window, so the bound bites hard.
	upper := int64(workers) * (int64(window/poll) + 2) * 4
	require.Positive(t, scans, "the idle pool polled at least once")
	require.Less(t, scans, upper, "empty-queue backoff is bounded — no hot-spin (got %d scans, bound %d)", scans, upper)
}

// TestPool_GracefulDrain_PromptReturn — with a LARGE poll interval the workers, once the queue drains,
// park in a long idle sleep. On ctx cancel a correct pool returns in ~ms (the sleep is ctx-cancellable);
// a non-cancellable time.Sleep would make Run take up to the poll interval → the bound below reddens.
// Also asserts no item is lost or stranded on drain.
func TestPool_GracefulDrain_PromptReturn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "drain.db")
	factory := func() (*SQLiteStore, error) { return NewSQLiteStore(dbPath, WithMultiProcess()) }

	const items = 6
	reg := NewRegistry()
	require.NoError(t, reg.Register("job", func() (*DAG, error) {
		d := NewDAG("job")
		return d, d.AddNode(NewNode("n0", ActionFunc(func(context.Context, *WorkflowData) error { return nil })))
	}))
	enq, err := factory()
	require.NoError(t, err)
	defer enq.Close() //nolint:errcheck // cleanup
	for i := range items {
		_, eerr := enq.Enqueue(fmt.Sprintf("wf-%d", i), "job", nil)
		require.NoError(t, eerr)
	}

	// 3s poll interval: after the queue drains, each worker enters a 3s idle sleep.
	pool, err := NewPool(factory, reg, "drain", WithPoolSize(3), WithPollInterval(3*time.Second))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	require.Eventually(t, func() bool {
		n, cerr := countState(enq, wqDone)
		return cerr == nil && n == items
	}, 15*time.Second, 5*time.Millisecond, "queue drains")

	time.Sleep(50 * time.Millisecond) // let the workers actually ENTER the idle sleep

	start := time.Now()
	cancel()
	select {
	case rerr := <-done:
		require.NoError(t, rerr, "graceful drain returns nil")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after cancel — idle backoff is not ctx-cancellable")
	}
	require.Less(t, time.Since(start), 2*time.Second, "prompt drain: workers return from the idle sleep on cancel")

	n, err := countState(enq, wqDone)
	require.NoError(t, err)
	require.Equal(t, items, n, "no work was lost or stranded on drain")
}

// -----------------------------------------------------------------------------
// counting driver — a database/sql driver wrapping modernc that counts work_queue claim-scans, so the
// no-hot-spin test can bound the empty-queue poll rate. TEST-ONLY; registered once under its own name.
// -----------------------------------------------------------------------------

const countDriverName = "sqlite-poolcount"

var (
	countDriverOnce sync.Once
	claimScans      atomic.Int64 // # of QueryContext calls touching work_queue (== ClaimNext scans)
)

func registerCountDriver() string {
	countDriverOnce.Do(func() {
		sql.Register(countDriverName, &countDriver{real: &sqlited.Driver{}})
	})
	return countDriverName
}

type countDriver struct{ real driver.Driver }

func (d *countDriver) Open(name string) (driver.Conn, error) {
	c, err := d.real.Open(name)
	if err != nil {
		return nil, err
	}
	return &countConn{real: c}, nil
}

type countConn struct{ real driver.Conn }

func (c *countConn) Prepare(query string) (driver.Stmt, error) { return c.real.Prepare(query) }
func (c *countConn) Close() error                              { return c.real.Close() }
func (c *countConn) Begin() (driver.Tx, error) {
	return c.beginCtx(context.Background(), driver.TxOptions{})
}

func (c *countConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.beginCtx(ctx, opts)
}

func (c *countConn) beginCtx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.real.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("countdriver: wrapped driver lacks ConnBeginTx")
	}
	return bt.BeginTx(ctx, opts)
}

func (c *countConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := c.real.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *countConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "work_queue") {
		claimScans.Add(1) // one per ClaimNext scan (the only work_queue SELECT in the idle path)
	}
	if qc, ok := c.real.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

var (
	_ driver.Driver         = (*countDriver)(nil)
	_ driver.Conn           = (*countConn)(nil)
	_ driver.ConnBeginTx    = (*countConn)(nil)
	_ driver.ExecerContext  = (*countConn)(nil)
	_ driver.QueryerContext = (*countConn)(nil)
)
