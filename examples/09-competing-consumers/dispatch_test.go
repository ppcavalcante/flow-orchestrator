package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// drainFleet enqueues n orders, drains them with a fleet of nWorkers, and returns the
// shared side-effect ledger plus the enqueued ids — the shape both exactly-once tests need.
func drainFleet(t *testing.T, n, nWorkers int) (*chargeLedger, []string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "queue.db")

	enq, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("open enqueue store: %v", err)
	}
	ids, err := enqueueOrders(enq, n)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := enq.Close(); err != nil {
		t.Fatalf("close enqueue store: %v", err)
	}

	led := newChargeLedger()
	counts, err := runFleet(context.Background(), dbPath, led, nWorkers)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	// At-least-once EXECUTION: every item is driven at least once; a reclaim may re-drive an
	// already-Completed item (a no-op resume), so total >= n. Losing an item (total < n) is
	// the real failure. Exactly-once is asserted on the EFFECT (the ledger) by the caller.
	if total < n {
		t.Errorf("fleet performed %d drives for %d items — an item was lost (want >= %d)", total, n, n)
	}
	return led, ids, dbPath
}

// assertEachChargedOnce checks the durable outcome: every enqueued order's idempotent effect
// applied exactly once (present in the charged set, none lost), and execution was at-least-once
// (every order attempted, total attempts >= the order count — a reclaim may re-run an action).
func assertEachChargedOnce(t *testing.T, led *chargeLedger, ids []string) {
	t.Helper()
	charged, attempts := led.snapshot()
	totalAttempts := 0
	for _, n := range attempts {
		totalAttempts += n
	}
	for _, id := range ids {
		orderID := id[len("order:"):]
		if !charged[orderID] {
			t.Errorf("order %q was never charged (lost work)", orderID)
		}
		if attempts[orderID] < 1 {
			t.Errorf("order %q executed %d times, want at least 1", orderID, attempts[orderID])
		}
	}
	if len(charged) != len(ids) {
		t.Errorf("effect applied to %d distinct orders, want %d (none lost, no phantom charges)", len(charged), len(ids))
	}
	if totalAttempts < len(ids) {
		t.Errorf("total execution attempts = %d, want >= %d (every order driven at least once)", totalAttempts, len(ids))
	}
}

// TestCompetingConsumers_ExactlyOnce is the core proof: across N interchangeable workers
// draining one queue, every enqueued order's idempotent charge applied EXACTLY once (none
// lost, none phantom) even though execution is at-least-once, and every run reached a
// durable Completed state.
func TestCompetingConsumers_ExactlyOnce(t *testing.T) {
	led, ids, dbPath := drainFleet(t, numOrders, numWorkers)

	assertEachChargedOnce(t, led, ids)

	// The durable side of the same fact: each run terminalized Completed in the store.
	verify, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("open verify store: %v", err)
	}
	defer verify.Close()
	for _, id := range ids {
		data, err := verify.Load(id)
		if err != nil {
			t.Errorf("load %q: %v", id, err)
			continue
		}
		if st, ok := data.GetNodeStatus("charge"); !ok || st != workflow.Completed {
			t.Errorf("%q charge node status = %v (ok=%v), want Completed", id, st, ok)
		}
		if charged, ok := data.GetBool(keyCharged); !ok || !charged {
			t.Errorf("%q charged = %v (ok=%v), want true", id, charged, ok)
		}
	}
}

// TestCompetingConsumers_ExactlyOnce_Stress cranks up contention (many more items and
// workers) to stress the atomic-claim serialization and the reclaim path. Guarded behind
// -short because it is heavier; the default test above already exercises the real behavior.
func TestCompetingConsumers_ExactlyOnce_Stress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavier contention run in -short mode")
	}
	const items, workers = 60, 8
	led, ids, _ := drainFleet(t, items, workers)
	assertEachChargedOnce(t, led, ids)
}

// TestFencing_ReclaimSupersedesStaleToken proves the safety half of the guarantee against
// the public Claim/Renew API: when a worker's lease lapses and a sibling RE-CLAIMS the
// item, the reclaim issues a strictly greater monotonic fencing token, and the original
// worker's token is FENCED OUT — its late write is rejected, so a slow/dead worker can
// never double-process an item a sibling has taken over.
func TestFencing_ReclaimSupersedesStaleToken(t *testing.T) {
	// A deliberately short TTL so A's lease lapses fast and B can reclaim it.
	const fenceTTL = 150 * time.Millisecond
	dbPath := filepath.Join(t.TempDir(), "fence.db")
	store, err := openStoreTTL(dbPath, fenceTTL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	const wid = "order:fence-target"

	// Worker A claims the item (a never-run workflow — the initial-claim case).
	tokenA, err := store.Claim(ctx, wid, "worker-A")
	if err != nil {
		t.Fatalf("A claim: %v", err)
	}
	if tokenA <= 0 {
		t.Fatalf("A token = %d, want a positive monotonic token", tokenA)
	}

	// A goes slow/dead — it stops driving. Wait for the lease to lapse so the item
	// becomes reclaimable (liveness). A generous margin over fenceTTL keeps this robust.
	time.Sleep(3 * fenceTTL)

	// Worker B reclaims the lapsed lease. The token must be STRICTLY greater (monotonic):
	// that ordering is the sole cross-process safety arbiter.
	tokenB, err := store.Claim(ctx, wid, "worker-B")
	if err != nil {
		t.Fatalf("B reclaim: %v", err)
	}
	if tokenB <= tokenA {
		t.Fatalf("B token = %d, want strictly greater than A token %d (monotonic fencing)", tokenB, tokenA)
	}

	// A wakes up and tries to keep its lease alive under its now-stale token → FENCED OUT.
	// This is what makes a double-process structurally impossible: A's writes are rejected.
	if err := store.Renew(wid, tokenA); !errors.Is(err, workflow.ErrFencedOut) {
		t.Errorf("A renew under stale token: err = %v, want ErrFencedOut", err)
	}

	// B still holds the live lease, so its renew succeeds.
	if err := store.Renew(wid, tokenB); err != nil {
		t.Errorf("B renew under live token: %v", err)
	}
}
