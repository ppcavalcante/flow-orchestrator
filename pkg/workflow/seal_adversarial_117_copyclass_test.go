package workflow

// M23 ph117 / AUD-002 — the COPY CLASS, now FIXED and in the DEFAULT build.
//
// HISTORY. This file was born behind the `adversarial_copyclass` build tag and REPRODUCED the
// defect. DAG embedded a sync.RWMutex BY VALUE, so `cp := *builtDAG` copied the lock's internal
// state. A copy taken while a drive held the write lock — Validate takes it on EVERY drive — inherited
// a mutex locked with no owner and no unlocker, and blocked FOREVER inside Validate. A stamped,
// seal-admitted graph that hangs: a violation of the no-input-hangs hard bar, reachable with EXPORTED
// API ONLY (Workflow.DAG() hands out the live pointer, a value copy is one line, Validate is exported).
// Those `cp := *dag` copies also tripped `go vet`'s copylocks analyser, which is why the file was
// tagged out of the default build (to keep the `go vet ./...` = 0 gate meaningful) and run via
// scripts/testing/run_tagged_adversarial.sh.
//
// THE FIX (AUD-002, dag.go): DAG.mu is now a *sync.RWMutex. A value copy copies the POINTER, so the
// copy SHARES the one mutex instead of duplicating its locked state — no copy can be born wedged.
// That ALSO removes the copylocks diagnostic (a pointer is not a lock value), which retired the build
// tag AND its script: with no diagnostic left to quarantine, these tests join the default build where
// every gate — including `-race` — can see them. The tag, run_tagged_adversarial.sh, the Makefile
// `test-tagged` target and its CI step were removed with the fix.
//
// WHAT STAYS TRUE: a value copy of a built DAG still carries a TRUE stamp (the token is provenance,
// never identity) and executes. WHAT CHANGED: the copy no longer wedges.
//
// 🔴 A RESIDUAL, NAMED so silence does not imply it is closed. `cp := *liveDAG` taken WHILE a drive
// is concurrently mutating that DAG (Validate writes d.cycleNodes under the write lock) is still an
// unsynchronised read of the struct's value fields — a data RACE, which `go test -race` flags at the
// copy site. The pointer-mutex fix removes the HANG, not the race: copying a graph another goroutine
// is driving was never safe and is not made safe here. The tests below therefore copy only a
// QUIESCENT DAG, which is race-clean. Do NOT reintroduce a copy-during-drive arm into this
// default (-race) build; that was the old TestSeal117Copy_ConsumerReachableWithExportedAPIOnly hunt,
// which is dropped precisely because it races by construction and its target (the wedge) is now gone.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func copyClassDAG(t *testing.T, id string) *DAG {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID(id)
	b.AddNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	b.AddNode("b").DependsOn("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	d, err := b.Build()
	require.NoError(t, err)
	require.True(t, d.built, "precondition: the builder output is stamped")
	return d
}

// TestSeal117Copy_StampIsBytesNotIdentity pins the property the phase recorded: a value copy of a
// built DAG carries a TRUE stamp and executes. Not a defect (the token is provenance, never identity)
// — pinned so M24's sealed-envelope design cannot silently assume an endorsement is unique to an
// object. If M24 makes the stamp identity-bearing, this FAILS and forces the change to be deliberate.
func TestSeal117Copy_StampIsBytesNotIdentity(t *testing.T) {
	orig := copyClassDAG(t, "copyclass")

	cp := *orig
	assert.True(t, cp.built,
		"a value copy carries a TRUE stamp — 'these bytes came from build()', never 'this object did'")
	assert.NoError(t, cp.Execute(context.Background(), NewWorkflowData("copyclass")),
		"and the copy therefore executes; this is the recorded, bounded residual")
}

// TestAUD002_CopiedMutexDoesNotDeadlock is the AUD-002 regression, deterministic and race-clean.
//
// RED BEFORE THE FIX: with a value mutex, `cp := *orig` taken while the write lock is held copies a
// locked, ownerless mutex; unlocking the ORIGINAL does not unlock the COPY, so cp.Validate() blocks
// on cp.mu.Lock() forever and this test times out and fails.
//
// GREEN AFTER THE FIX: with `mu *sync.RWMutex`, cp.mu IS orig.mu, so unlocking the original releases
// the one shared lock and cp.Validate() acquires it and completes.
//
// Oracle: TOTALITY under a hard deadline. A stamped graph that blocks forever inside Validate is a
// hang, not a refusal — and the seal cannot see it, because the copy's token is genuinely true. The
// copy is taken while the lock is held but NOTHING is mutating the DAG, and only the copy's own
// goroutine touches it afterwards, so there is no unsynchronised access — this arm stays clean
// under `-race` (unlike the copy-during-drive reachability hunt this file used to carry).
func TestAUD002_CopiedMutexDoesNotDeadlock(t *testing.T) {
	orig := copyClassDAG(t, "aud002-nowedge")

	// Seed the exact state a drive holds the lock in (Validate takes the WRITE lock), then copy.
	// With the old value mutex this planted a locked, ownerless lock into the copy.
	orig.mu.Lock()
	cp := *orig
	orig.mu.Unlock()

	// The original is healthy again; the copy is stamped, so the seal admits it.
	require.NoError(t, orig.Validate(), "the ORIGINAL must be unaffected")
	require.True(t, cp.built, "the copy is stamped — the seal will admit it")

	done := make(chan error, 1)
	go func() { done <- cp.Validate() }() // takes cp.mu.Lock(); a SHARED, released lock after the fix

	select {
	case err := <-done:
		require.NoError(t, err, "a value copy of a built DAG must validate cleanly, not wedge")
	case <-time.After(10 * time.Second):
		t.Fatal("BUG (AUD-002 regressed): Validate on a copied DAG WEDGED — the mutex was inherited " +
			"in a locked state with no unlocker. DAG.mu must be a *sync.RWMutex so a copy SHARES the " +
			"lock instead of duplicating its locked value.")
	}
}
