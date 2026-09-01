package workflow

// M16 ph77 [MAJOR-5]: the sustained-contention ErrBusy bite — proof the typed ErrBusy is NOT dead
// code. A lock-holder opens a SECOND raw connection to the same DB and holds a BEGIN IMMEDIATE write
// lock (SetMaxOpenConns(1) per handle → a distinct SQLite connection contending on the same file
// write-lock, exactly what a 2nd PROCESS contends on — the ph73 busy_test.go finding). A production
// SQLiteStore.Save then tries to write, waits out busy_timeout=5000ms, and gets SQLITE_BUSY →
// classified as ErrBusy at the Save/Execute boundary. Asserts: errors.Is(err, ErrBusy) reaches the
// caller, the drive aborts CLEANLY (no partial write — the txn never acquired the lock), and ErrBusy
// stays errors.Is-DISTINCT from ErrFencedOut (retry-vs-abort). In-process lock-holder (the tighter
// deterministic instrument for forcing the >busy_timeout starvation tail; the real-OS-proc write
// contention is covered by TestMPTwoProc_* + the ph77 execute-2proc test).

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMPSave_BusyStarvation_ErrBusy — the MAJOR-5 bite. Skipped under -short (it waits out the full
// busy_timeout, ~5s, by design — that IS the starvation tail).
func TestMPSave_BusyStarvation_ErrBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out busy_timeout (~5s) to force the SQLITE_BUSY starvation tail; skipped under -short")
	}
	dbPath := filepath.Join(t.TempDir(), "busy.db")
	// The production store under test (mp mode → _txlock=immediate + busy_timeout=5000).
	store, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

	// The lock-holder: a distinct raw connection that grabs + HOLDS the write lock past busy_timeout.
	// _txlock=immediate so BEGIN takes the write lock immediately (like the production store).
	holder, err := sql.Open("sqlite", dbPath+"?_txlock=immediate")
	require.NoError(t, err)
	defer func() { _ = holder.Close() }() //nolint:errcheck // cleanup
	holder.SetMaxOpenConns(1)
	_, err = holder.Exec("PRAGMA busy_timeout=5000")
	require.NoError(t, err)

	ctx := context.Background()
	htx, err := holder.BeginTx(ctx, nil) // acquires the write lock
	require.NoError(t, err)
	// Do a write inside the held txn so the lock is definitely a WRITE lock, then keep it open.
	_, err = htx.ExecContext(ctx,
		`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES ('holder','h',0,1)`)
	require.NoError(t, err)

	// Release the holder's write lock ONLY AFTER the contending Save has returned — so the Save
	// reliably waits out busy_timeout and gets SQLITE_BUSY, never racing the holder for the lock.
	// A fixed-duration hold is a flake in BOTH directions: modernc's busy_timeout wall-clock varies
	// with runner load (observed 3.5s–5s+ for a 5s timeout), so too-short a hold lets the Save
	// acquire the lock (no ErrBusy) and too-long a hold slows every run. The channel makes the hold
	// last exactly as long as the Save's attempt.
	release, released := make(chan struct{}), make(chan struct{})
	go func() {
		<-release
		_ = htx.Rollback() //nolint:errcheck // release the lock; the assertions already ran
		close(released)
	}()

	// The production Save contends for the write lock the holder is sitting on. It waits out
	// busy_timeout=5000ms, then SQLITE_BUSY → classifyTxErr → ErrBusy.
	d := NewWorkflowData("wf")
	d.SetNodeStatus("n0", Completed)
	start := time.Now()
	saveErr := store.Save(d)
	waited := time.Since(start)
	close(release) // the Save has given up; let the holder release now
	<-released     // and wait for the rollback to land before the Load below

	require.Error(t, saveErr, "a Save contending with a held write lock past busy_timeout must error, not silently succeed")
	require.ErrorIs(t, saveErr, ErrBusy, "SQLITE_BUSY past busy_timeout MUST classify as ErrBusy at the Save boundary (the typed error is reachable, not dead code)")
	require.False(t, errors.Is(saveErr, ErrFencedOut), "ErrBusy (transient, retry) must be errors.Is-DISTINCT from ErrFencedOut (superseded, abort)")
	// Floor proves the Save waited out a SUBSTANTIAL fraction of busy_timeout (the starvation tail),
	// not an instant lock failure (which returns in <50ms). Loosened from 4s: modernc's busy_timeout
	// wall-clock is nondeterministic under load and was observed at 3.48s for a 5000ms timeout, so a
	// 4s floor false-fails on a loaded runner; 2s still separates the tail from an instant failure by ~40x.
	require.GreaterOrEqual(t, waited, 2*time.Second, "the Save WAITED out a substantial part of busy_timeout before BUSY — the starvation tail, not an instant lock")

	// Clean abort: the failed Save wrote NOTHING (its txn never acquired the lock). The holder has
	// already released (<-released above), so a fresh Load finds no 'wf' rows — no partial write leaked.
	_, loadErr := store.Load("wf")
	require.ErrorIs(t, loadErr, ErrNotFound, "clean abort: the busy-failed Save left NO partial 'wf' state")
}

// TestClassifyTxErr_isBusy_seedbreak — the bite that the ErrBusy CLASSIFICATION (not just the raw
// wait) is load-bearing: if classifyTxErr mapped a busy error to ErrIO instead, errors.Is(_, ErrBusy)
// would fail. This asserts the mapping directly (fast, no 5s wait), complementing the starvation test.
func TestClassifyTxErr_isBusy_seedbreak(t *testing.T) {
	// The classifier maps an unambiguous SQLITE_BUSY error to ErrBusy...
	require.ErrorIs(t, classifyTxErr("commit", errors.New("SQLITE_BUSY: database is locked")), ErrBusy)
	// ...and a hypothetical mis-map (wrapping the same error as ErrIO — what a swallow-the-class
	// regression would do) is NOT errors.Is ErrBusy → the caller could not distinguish retry-vs-abort.
	mismapped := errors.New("some IO wrap: SQLITE_BUSY")
	require.False(t, errors.Is(mismapped, ErrBusy), "a raw/mis-wrapped busy error is NOT errors.Is-ErrBusy — classifyTxErr's wrap is what makes it reachable")
}

// TestMPClaim_BusyStarvation_ErrBusy — review ph77-F1: Claim is the busiest cross-process contention
// point, and it too must surface ErrBusy (not opaque ErrIO) when it loses the write-lock race past
// busy_timeout — so a host can RETRY a merely-contended claim instead of treating it as fatal. Same
// lock-holder instrument as the Save bite. -short-skipped (waits out busy_timeout ~5s).
func TestMPClaim_BusyStarvation_ErrBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out busy_timeout (~5s) to force the SQLITE_BUSY starvation tail; skipped under -short")
	}
	dbPath := filepath.Join(t.TempDir(), "claimbusy.db")
	store, err := NewSQLiteStore(dbPath, WithMultiProcess())
	require.NoError(t, err)
	defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

	holder, err := sql.Open("sqlite", dbPath+"?_txlock=immediate")
	require.NoError(t, err)
	defer func() { _ = holder.Close() }() //nolint:errcheck // cleanup
	holder.SetMaxOpenConns(1)
	_, err = holder.Exec("PRAGMA busy_timeout=5000")
	require.NoError(t, err)

	ctx := context.Background()
	htx, err := holder.BeginTx(ctx, nil) // hold the write lock
	require.NoError(t, err)
	_, err = htx.ExecContext(ctx,
		`INSERT INTO leases(workflow_id, owner_id, expiry, fencing_token) VALUES ('holder','h',0,1)`)
	require.NoError(t, err)
	// Release only after the Claim has returned (same deterministic hold as the Save bite above —
	// a fixed sleep flakes if the busy wait ever outlasts it under load).
	release, released := make(chan struct{}), make(chan struct{})
	go func() {
		<-release
		_ = htx.Rollback() //nolint:errcheck // release after the assertion window
		close(released)
	}()

	// Claim contends for the held write lock, waits out busy_timeout → SQLITE_BUSY → ErrBusy.
	_, claimErr := store.Claim(context.Background(), "wf", "owner")
	close(release)
	<-released
	require.Error(t, claimErr, "a Claim contending with a held write lock past busy_timeout must error")
	require.ErrorIs(t, claimErr, ErrBusy, "ph77-F1: a contended Claim past busy_timeout classifies as ErrBusy (retryable), NOT opaque ErrIO")
	require.False(t, errors.Is(claimErr, ErrFencedOut), "a busy Claim is transient (retry), distinct from ErrFencedOut (abort)")
}
