package workflow

// M17 ph82 (D4) — the store-per-worker WORKER POOL. N in-process worker goroutines share ONE .db
// file, but EACH worker opens its OWN *SQLiteStore instance (its own in-memory tokenState map + its
// own *sql.DB handle). This is the M16 real-multi-process model applied in-process: the durable
// `leases` table (in the one DB file) is the cross-worker fencing arbiter, while per-worker
// tokenState isolation is what makes the fencing CAS bite in-process.
//
// WHY store-per-worker is LOAD-BEARING (safety, not ergonomics): the fencing token a worker holds
// lives in that store instance's in-memory tokenState, keyed by workflowID. If N workers SHARED one
// store, a reclaim would clobber the shared tokenState[workflowID] key — worker B reclaiming workflow
// X calls (inside ClaimNext) s.tokenState[X]=N+1, overwriting worker A's tokenState[X]=N. A's
// checkpoint CAS would then read B's token and a superseded A's STALE write would land UNFENCED (a
// double-commit). Per-worker stores isolate tokenState exactly as separate OS processes do → A-store
// keeps tokenState[X]=N, A's checkpoint reads held=N < durable current=N+1 → FENCED. The fencing
// wiring is the C3 tokenState bridge: runNext builds &Workflow{DAG,WorkflowID,Store} WITHOUT a Locker
// and relies on the fact that a worker's ClaimNext set the token on THAT store instance and Execute's
// checkpoint reads it on the SAME instance — we deliberately do NOT add WithMultiProcessLocker here
// (ClaimNext already claimed; a Locker.Acquire would double-claim).

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// StoreFactory opens a worker's OWN *SQLiteStore on the shared DB file. The Pool calls it once per
// worker (at worker start); the caller owns the path + durability + clock options and MUST open the
// store WithMultiProcess (ClaimNext/Claim require mp mode). Returning a NEW instance on every call is
// the load-bearing contract — a factory that returns one shared instance collapses the per-worker
// tokenState isolation and un-fences superseded writes (see the file header).
type StoreFactory func() (*SQLiteStore, error)

// defaultPollInterval bounds the idle claim rate: a worker that finds no claimable work sleeps this
// long (ctx-cancellable) before scanning again, so an empty queue can never hot-spin (red-team
// MAJOR-5). Off the hot path — a worker that DID claim loops immediately for the next item.
const defaultPollInterval = 100 * time.Millisecond

// Pool runs N store-per-worker goroutines that each ClaimNext + drive registered work items off the
// shared queue. Constructed via NewPool; started with Run. Immutable after construction (the option
// setters run only inside NewPool), so its methods are safe to call once and it carries no per-run
// mutable state.
type Pool struct {
	factory      StoreFactory                     // opens each worker's OWN store (per-worker tokenState isolation)
	reg          *Registry                        // the type→DAGFactory registry every worker claims + rebuilds through
	ownerID      string                           // the ownerID PREFIX; worker i derives ownerID = prefix+"-"+i (distinct)
	size         int                              // number of worker goroutines
	pollInterval time.Duration                    // idle backoff between empty claims (ctx-cancellable)
	maxAttempts  int                              // retry budget passed down to runNext (RunNext freezes at defaultMaxAttempts)
	observer     func(workerIndex int, err error) // AUD-035: optional; surfaces otherwise-swallowed runNext errors
}

// PoolOption configures a Pool at construction (variadic on NewPool).
type PoolOption func(*Pool)

// WithPoolSize sets the number of worker goroutines. Values < 1 are clamped to 1 in NewPool. There is no
// upper cap (review ph82r-L2): the caller owns capacity policy — each worker holds one *sql.DB handle on
// the shared file, so an absurd size costs fds + goroutines but is not a correctness risk (the IMMEDIATE
// write lock + busy_timeout serialize contention regardless). Size to your throughput, not past it.
func WithPoolSize(n int) PoolOption {
	return func(p *Pool) { p.size = n }
}

// WithMaxAttempts sets the DF-4 retry budget the Pool passes down to runNext (the per-item attempt
// bound before dead-lettering). Values < 1 are clamped to 1 in NewPool. This is the knob RunNext
// freezes at defaultMaxAttempts — the Pool drives runNext directly with this budget instead.
func WithMaxAttempts(n int) PoolOption {
	return func(p *Pool) { p.maxAttempts = n }
}

// WithPollInterval sets the idle backoff between empty claims. Values <= 0 fall back to
// defaultPollInterval in NewPool.
func WithPollInterval(d time.Duration) PoolOption {
	return func(p *Pool) { p.pollInterval = d }
}

// WithErrorObserver registers an optional callback invoked with (workerIndex, err) whenever a
// worker's runNext returns a non-nil error that the drive loop otherwise SWALLOWS (AUD-035 / P-14):
// a per-item terminal failure (an unregistered/broken/refused factory or bad input — the row is
// already dead-lettered) or a transient claim-path fault (ErrBusy/ErrIO — the queue is untouched and
// the worker backs off). It is for OBSERVABILITY and health tracking, never control flow: the
// callback cannot change the worker's disposition. It runs ON the worker goroutine, so it must not
// block or panic. Permanent worker-config errors (a non-MP or shared store) are rejected at startup
// by openStores and never reach here. nil observer (the default) preserves the prior swallow.
func WithErrorObserver(fn func(workerIndex int, err error)) PoolOption {
	return func(p *Pool) { p.observer = fn }
}

// NewPool builds a Pool. factory (non-nil) opens each worker's own store, reg (non-nil) is the shared
// registry, ownerID (non-empty) is the ownerID PREFIX from which each worker derives a distinct
// ownerID. Options tune size / retry budget / poll interval; unsafe values are clamped to safe
// minimums so a mis-set option degrades rather than breaking the pool.
func NewPool(factory StoreFactory, reg *Registry, ownerID string, opts ...PoolOption) (*Pool, error) {
	if factory == nil {
		return nil, fmt.Errorf("%w: NewPool requires a non-nil StoreFactory", ErrValidation)
	}
	if reg == nil {
		return nil, fmt.Errorf("%w: NewPool requires a non-nil Registry", ErrValidation)
	}
	if ownerID == "" {
		return nil, fmt.Errorf("%w: NewPool requires a non-empty ownerID prefix", ErrValidation)
	}
	p := &Pool{
		factory:      factory,
		reg:          reg,
		ownerID:      ownerID,
		size:         runtime.NumCPU(),
		pollInterval: defaultPollInterval,
		maxAttempts:  defaultMaxAttempts,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.size < 1 {
		p.size = 1
	}
	if p.pollInterval <= 0 {
		p.pollInterval = defaultPollInterval
	}
	if p.maxAttempts < 1 {
		p.maxAttempts = 1
	}
	return p, nil
}

// Run starts p.size worker goroutines and blocks until ctx is cancelled AND every worker has drained
// (finished its in-flight runNext, closed its store, released any held lease) and returned. It
// returns the joined non-nil worker errors — in practice only a store-open failure, since the drive
// loop swallows per-item faults (disposeExecErr already resolved the queue state) and treats a
// cancelled ctx as a clean drain. A normal drain returns nil.
func (p *Pool) Run(ctx context.Context) error {
	// Open every worker's store UP FRONT, before any drive loop starts. This is where the
	// two Pool safety contracts a bare StoreFactory cannot self-enforce are checked (AUD-021
	// non-MP, AUD-022 distinct-instance) — both are configuration errors, so we fail the
	// whole pool at startup rather than spin or alias. openStores closes anything it already
	// opened on failure; on success WE own closing them all once the workers drain.
	stores, err := p.openStores()
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range stores {
			_ = s.Close() //nolint:errcheck // best-effort close on drain; the DB is being torn down
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, p.size)
	for i := range p.size {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = p.runWorker(ctx, idx, stores[idx])
		}(i)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// openStores opens one store per worker and enforces the two safety contracts a bare
// StoreFactory leaves to prose. It runs sequentially, before any work begins:
//
//   - AUD-021/P-12 — each store MUST be in multi-process mode. A non-MP store's ClaimNext
//     returns a PERMANENT ErrValidation; runWorker discards that error and backs off, so a
//     misconfigured worker would spin forever looking healthy while processing ZERO work.
//   - AUD-022/P-13 — each factory call MUST return a DISTINCT *SQLiteStore. The per-worker
//     tokenState isolation that makes in-process fencing bite (see the file header) lives in
//     each store instance's in-memory tokenState map; a factory that hands the SAME instance
//     to two workers collapses that isolation (a reclaim clobbers the shared
//     tokenState[workflowID] key, un-fencing a superseded write) and would Close the one store
//     N times. The contract was previously prose-only and unverifiable.
//
// Both are configuration, not transient contention, so a violation fails the whole pool at
// startup. Anything already opened is closed before returning the error.
func (p *Pool) openStores() ([]*SQLiteStore, error) {
	stores := make([]*SQLiteStore, 0, p.size)
	seen := make(map[*SQLiteStore]int, p.size)
	closeOpened := func() {
		for _, s := range stores {
			_ = s.Close() //nolint:errcheck // best-effort rollback of a partially-opened pool
		}
	}
	for i := range p.size {
		store, err := p.factory()
		if err != nil {
			closeOpened()
			return nil, fmt.Errorf("pool worker %d: open store: %w", i, err)
		}
		if store == nil {
			closeOpened()
			return nil, fmt.Errorf("%w: pool worker %d: StoreFactory returned a nil store", ErrValidation, i)
		}
		if prev, dup := seen[store]; dup {
			// `store` is already tracked at stores[prev]; closeOpened closes it exactly once.
			closeOpened()
			return nil, fmt.Errorf("%w: pool workers %d and %d received the SAME *SQLiteStore instance; the "+
				"StoreFactory MUST return a DISTINCT store per call — per-worker tokenState isolation is "+
				"load-bearing for fencing (see workflow_pool.go header), and a shared instance would also be "+
				"Closed multiple times", ErrValidation, prev, i)
		}
		if !store.dur.mp {
			_ = store.Close() //nolint:errcheck // this store is not yet tracked; roll it back explicitly
			closeOpened()
			return nil, fmt.Errorf("%w: pool worker %d: StoreFactory returned a store not in multi-process mode; "+
				"open each worker store with NewSQLiteStore(path, WithMultiProcess()) — a non-MP store's "+
				"ClaimNext fails permanently and the worker would process nothing forever", ErrValidation, i)
		}
		seen[store] = i
		stores = append(stores, store)
	}
	return stores, nil
}

// runWorker is one worker's drive lifecycle over its pre-opened store (opened + safety-checked
// by openStores, closed by Run): loop { runNext → on a drive back to the top; on no-work / a claim
// fault back off } until ctx is cancelled, at which point it drains (returns to Run). Every runtime
// fault is handled and the loop continues; the fatal configuration faults (non-MP, shared instance)
// were already rejected at startup, so runWorker never opens or closes a store itself.
func (p *Pool) runWorker(ctx context.Context, idx int, store *SQLiteStore) error {
	ownerID := fmt.Sprintf("%s-%d", p.ownerID, idx)

	for {
		// Drain point: a cancelled ctx stops us claiming NEW work. Checked at the loop top so an
		// in-flight runNext (below) always completes first. On a normal drive it terminalizes via
		// MarkDone/MarkFailed (releasing the lease). On a MID-DRIVE cancel (this drain cancels the ctx
		// the in-flight Execute holds), DAG.Execute returns context.Canceled → disposeExecErr recognizes
		// the shutdown and leaves the item `claimed` WITHOUT terminalizing or releasing the lease
		// (review ph82-AF1): the lease lapses on TTL, D1's reclaim-broadening rediscovers it, and a
		// successor resumes from the committed frontier (at-least-once). A cancel is a shutdown signal,
		// NOT a poison failure — dead-lettering healthy in-flight work would lose it. Returning here hands
		// control back to Run, which closes every worker store once all workers have drained.
		if ctx.Err() != nil {
			return nil
		}

		// SAME-INSTANCE fencing (C3): runNext claims + drives on THIS worker's store, so the token
		// ClaimNext set in this store's tokenState is the one Execute's checkpoint CAS reads. We pass
		// the Pool's maxAttempts budget (not RunNext's frozen defaultMaxAttempts). We deliberately do
		// NOT wrap the drive in WithMultiProcessLocker — ClaimNext already claimed; a second Acquire
		// would double-claim.
		ran, rerr := runNext(ctx, store, p.reg, ownerID, p.maxAttempts)
		if rerr != nil && p.observer != nil {
			// AUD-035: surface the error the loop otherwise swallows. runNext has already
			// resolved the queue state (ran=true → the row was terminalized; ran=false → a
			// transient claim fault left the queue untouched). The observer is for visibility
			// and health only — it does NOT change the disposition below.
			p.observer(idx, rerr)
		}
		if ran {
			// A real drive happened. On success the item is done; on a handled failure disposeExecErr
			// already set the queue state (retry / dead-letter / superseded-abort) and an ErrFencedOut
			// simply means a reclaimer owns it now. Either way, loop immediately for the next item —
			// NO idle backoff (there may be more work). (Prompt: "on ErrFencedOut/other errors →
			// continue".)
			continue
		}
		// ran == false: no item was obtained — ErrNoWork (empty queue / all contended away / nothing
		// registered) OR a transient claim-path fault (ErrBusy/ErrIO from the claim txn). BOTH back
		// off for pollInterval, ctx-cancellable, so neither an idle queue nor a persistent claim fault
		// can hot-spin (MAJOR-5). A claim fault is transient and self-heals on the next scan; we do
		// not surface it (the queue state is untouched — nothing was claimed).
		if !p.backoff(ctx) {
			return nil // ctx cancelled during backoff → drain.
		}
	}
}

// backoff sleeps for pollInterval or until ctx is cancelled, whichever comes first. Returns true if
// the full interval elapsed (keep polling), false if ctx was cancelled (drain and return). The timer
// is the single mechanism that bounds the idle claim rate.
func (p *Pool) backoff(ctx context.Context) bool {
	t := time.NewTimer(p.pollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
