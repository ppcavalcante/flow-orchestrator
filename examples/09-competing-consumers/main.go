// Command 09-competing-consumers shows the dispatch model: the workflow is DATA on a
// shared durable queue, and N interchangeable workers drain it — each item processed
// EXACTLY ONCE across the fleet, guaranteed by leases (liveness) + fencing (safety).
//
//	┌──────────── shared SQLite work_queue ────────────┐
//	│  order:00  order:01  order:02 … order:11         │
//	└───┬─────────────┬─────────────┬─────────────┬────┘
//	 worker-0      worker-1      worker-2      worker-3      (each RunNext-drains)
//
// The unit of work is a registered DAGFactory keyed by a work TYPE ("process-order").
// A worker calls RunNext: it atomically Claims the oldest pending item (one BEGIN
// IMMEDIATE txn — exactly one worker wins any row), seeds the item's JSON input onto a
// fresh run, builds the factory's DAG, and Executes it to a durable terminal state.
//
// The distribution is structural, not luck:
//   - CLAIM is atomic, so no two workers ever hold the same item at once.
//   - A LEASE means a dead/slow worker's claim lapses and a sibling RE-CLAIMS the item —
//     nothing is lost when a worker dies mid-drive (liveness).
//   - A monotonic FENCING TOKEN means the superseded worker's late checkpoint is rejected
//     (ErrFencedOut), so the journal is never corrupted by a stale writer (safety).
//
// Durable dispatch is AT-LEAST-ONCE EXECUTION: a reclaim after a mid-drive lease lapse
// can re-run an action whose Completed status was not yet checkpointed. So an EXACTLY-ONCE
// outcome comes from making the effect IDEMPOTENT — here, charging is keyed on the order id,
// so a re-execution is a no-op on the effect. That is the real contract, and this example
// models it honestly: execution attempts may exceed the order count; the applied effect does
// not. The command exercises the happy path; the fencing reclaim is proven in dispatch_test.go.
//
// Run it:
//
//	go run ./examples/09-competing-consumers
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// The work TYPE registered in the dispatch Registry. A worker claims only types it has
// registered a factory for; every worker in this fleet registers this one.
const typeProcessOrder = "process-order"

// Data keys threaded through a run. The order id + amount are seeded from the item's
// JSON input by dispatch (seedInput turns a JSON object into KV Sets); `charged` is the
// terminal fact the charge node writes. Named constants keep producer and consumer honest.
const (
	keyOrderID = "order_id"
	keyAmount  = "amount_cents"
	keyCharged = "charged"
)

// Fleet sizing. More items than workers so every worker gets several; a fresh queue on
// each run so the example is self-contained.
const (
	numOrders  = 12
	numWorkers = 4
)

// leaseTTL sits comfortably above a healthy drive so the happy-path fleet rarely reclaims a
// live sibling's in-flight item. A reclaim is still safe (fencing + the idempotent effect),
// but keeping it rare makes the happy-path output clean. The fencing test opens its OWN
// store with a deliberately short TTL to make a lapse happen fast.
const leaseTTL = 2 * time.Second

// idleGrace is how long a worker keeps re-scanning an empty queue before quitting, so it
// sticks around to reclaim a lapsed sibling claim rather than quitting while an item is
// still in flight.
const idleGrace = 300 * time.Millisecond

// chargeLedger is the process-shared, concurrency-safe record of the real side effect. The
// DAGFactory captures it, so every worker's charge node records into the SAME ledger. It
// separates the two facts the durable contract distinguishes:
//   - charged: the IDEMPOTENT effect, keyed on the order id — a re-execution is a no-op, so
//     each order is charged exactly once regardless of how many times it is driven.
//   - attempts: the raw EXECUTION count per order — at-least-once, so a reclaim after a
//     mid-drive lease lapse legitimately bumps this above 1. This is what a non-idempotent
//     effect would double-apply, which is exactly why real effects carry an idempotency key.
type chargeLedger struct {
	mu       sync.Mutex
	charged  map[string]bool // idempotent effect: orderID → charged (the dedup key)
	attempts map[string]int  // raw execution attempts per order (at-least-once)
}

func newChargeLedger() *chargeLedger {
	return &chargeLedger{charged: map[string]bool{}, attempts: map[string]int{}}
}

// charge applies the effect idempotently, keyed on orderID: a re-drive counts as another
// execution attempt but leaves the effect applied exactly once.
func (l *chargeLedger) charge(orderID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[orderID]++
	l.charged[orderID] = true
}

// snapshot returns copies so a caller can inspect the ledger without holding the lock.
func (l *chargeLedger) snapshot() (charged map[string]bool, attempts map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	charged = make(map[string]bool, len(l.charged))
	for k, v := range l.charged {
		charged[k] = v
	}
	attempts = make(map[string]int, len(l.attempts))
	for k, v := range l.attempts {
		attempts[k] = v
	}
	return charged, attempts
}

// buildProcessOrder is the "process-order" DAGFactory. It is store-free by design: RunNext
// supplies the store and the claimed WorkflowID when it drives the run, so the factory
// returns a bare *DAG via Build() (Build refuses a store-configured builder). The captured
// ledger is shared across every worker building this DAG.
//
//	validate ──▶ charge   (charge is the real, at-most-once effect)
func buildProcessOrder(led *chargeLedger) workflow.DAGFactory {
	return func() (*workflow.DAG, error) {
		b := workflow.NewWorkflowBuilder()

		// validate: confirm the item's input reached the run (dispatch seeded it as KV).
		b.AddStartNode("validate").WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
			if _, ok := d.GetString(keyOrderID); !ok {
				return fmt.Errorf("process-order: no %q seeded on the run", keyOrderID)
			}
			return nil
		})

		// charge: the real-world effect, applied IDEMPOTENTLY (keyed on the order id). Under a
		// reclaim after a mid-drive lease lapse the engine may re-run this action, so a
		// non-idempotent effect would double-apply. Keying on the order id makes the effect
		// exactly-once even under at-least-once execution — the durable-execution contract.
		b.AddNode("charge").DependsOn("validate").WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
			orderID, _ := d.GetString(keyOrderID)
			led.charge(orderID)
			d.Set(keyCharged, true)
			return nil
		})

		return b.Build()
	}
}

// openStore opens a worker's OWN handle to the shared multi-process SQLite DB with the
// default fleet lease TTL.
func openStore(dbPath string) (*workflow.SQLiteStore, error) {
	return openStoreTTL(dbPath, leaseTTL)
}

// openStoreTTL opens a worker's OWN handle to the shared multi-process SQLite DB. Each
// worker (and the enqueuer) opens a distinct handle to the same file — the store-per-worker
// discipline the fencing model requires (a shared handle would blur per-owner token state).
// The lease TTL is a parameter so the fencing test can force a fast lapse.
func openStoreTTL(dbPath string, ttl time.Duration) (*workflow.SQLiteStore, error) {
	return workflow.NewSQLiteStore(dbPath,
		workflow.WithMultiProcess(), // WAL + busy_timeout: many handles/processes on one file
		workflow.WithLeaseTTL(ttl))  // a lapsed claim becomes reclaimable after this
}

// buildRegistry wires the type→factory Registry every worker drives. One registry is safe
// to share across workers (registration happens up front; lookups are read-only).
func buildRegistry(led *chargeLedger) (*workflow.Registry, error) {
	reg := workflow.NewRegistry()
	if err := reg.Register(typeProcessOrder, buildProcessOrder(led)); err != nil {
		return nil, err
	}
	return reg, nil
}

// enqueueOrders puts numOrders ProcessOrder jobs on the shared queue. Each job's DocInput
// is the JSON dispatch seeds onto its run. Returns the enqueued workflow ids.
func enqueueOrders(store *workflow.SQLiteStore, n int) ([]string, error) {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		orderID := fmt.Sprintf("order-%02d", i)
		wid := "order:" + orderID
		input, err := json.Marshal(map[string]any{
			keyOrderID: orderID,
			keyAmount:  int64((i + 1) * 100),
		})
		if err != nil {
			return nil, fmt.Errorf("marshal input for %s: %w", wid, err)
		}
		if _, err := store.Enqueue(wid, typeProcessOrder, input); err != nil {
			return nil, fmt.Errorf("enqueue %s: %w", wid, err)
		}
		ids = append(ids, wid)
	}
	return ids, nil
}

// drain runs one worker's loop: claim-and-execute the next item until the queue has been
// idle for idleGrace. Returns how many items THIS worker drove — the fleet's total across
// workers is numOrders, but the split between workers is nondeterministic (whoever claims
// first wins), which is the whole point of interchangeable consumers.
func drain(ctx context.Context, store *workflow.SQLiteStore, reg *workflow.Registry, ownerID string) (int, error) {
	drained := 0
	lastWork := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return drained, err
		}
		ran, err := workflow.RunNext(ctx, store, reg, ownerID)
		switch {
		case ran:
			drained++
			lastWork = time.Now()
		default:
			// No progress this round — EITHER ErrNoWork (nothing claimable; a slow sibling
			// may still hold a not-yet-lapsed claim) OR a transient claim/busy fault that
			// dispatch already terminalized/retried. Back off and re-scan; quit only after a
			// sustained idle window so a persistent fault can never spin the loop forever.
			if time.Since(lastWork) > idleGrace {
				return drained, nil
			}
			_ = err // transient/ErrNoWork are both handled by backing off; the ledger is the source of truth
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// runFleet launches numWorkers goroutine-workers, each with its own store handle, draining
// the shared queue concurrently. Returns each worker's drained count.
func runFleet(ctx context.Context, dbPath string, led *chargeLedger, nWorkers int) ([]int, error) {
	reg, err := buildRegistry(led)
	if err != nil {
		return nil, err
	}
	counts := make([]int, nWorkers)
	errs := make([]error, nWorkers)
	var wg sync.WaitGroup
	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			store, err := openStore(dbPath)
			if err != nil {
				errs[idx] = fmt.Errorf("worker-%d open: %w", idx, err)
				return
			}
			defer store.Close()
			n, err := drain(ctx, store, reg, fmt.Sprintf("worker-%d", idx))
			counts[idx] = n
			if err != nil && ctx.Err() == nil {
				errs[idx] = fmt.Errorf("worker-%d drain: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return counts, e
		}
	}
	return counts, nil
}

func run() error {
	dir, err := os.MkdirTemp("", "competing-consumers")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "queue.db")

	led := newChargeLedger()

	// Enqueue on a dedicated handle, then close it — the workers open their own.
	enq, err := openStore(dbPath)
	if err != nil {
		return fmt.Errorf("open enqueue store: %w", err)
	}
	ids, err := enqueueOrders(enq, numOrders)
	closeErr := enq.Close()
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close enqueue store: %w", closeErr)
	}
	fmt.Printf("enqueued %d orders onto the shared queue\n", len(ids))

	counts, err := runFleet(context.Background(), dbPath, led, numWorkers)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}

	total := 0
	for i, n := range counts {
		fmt.Printf("worker-%d drove %d orders\n", i, n)
		total += n
	}
	// Dispatch is at-least-once EXECUTION: a reclaim of a lapsed claim can re-drive an
	// already-Completed item (a no-op resume), so drives can exceed the order count. The
	// exactly-once guarantee lives in the EFFECT below, not the drive count.
	fmt.Printf("fleet performed %d drives across %d workers for %d orders\n", total, numWorkers, len(ids))

	// Verify the durable outcome: each order's idempotent effect applied exactly once (none
	// lost), and the raw execution attempts (>= the order count, since a reclaim can re-run).
	charged, attempts := led.snapshot()
	missing, totalAttempts := 0, 0
	for _, n := range attempts {
		totalAttempts += n
	}
	for _, id := range ids {
		if !charged[id[len("order:"):]] {
			missing++
		}
	}
	fmt.Printf("\nresult: %d orders, effect-applied-exactly-once=%d lost=%d; execution-attempts=%d (>= orders: at-least-once)\n",
		len(ids), len(charged), missing, totalAttempts)
	if missing != 0 {
		return fmt.Errorf("lost work: %d orders never charged", missing)
	}
	if len(charged) != len(ids) {
		return fmt.Errorf("effect count = %d, want %d distinct orders charged", len(charged), len(ids))
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("09-competing-consumers: %v", err)
	}
}
