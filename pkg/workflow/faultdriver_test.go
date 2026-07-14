package workflow

// M16 ph78 — the fault-injection driver seam (task #126, class A). A database/sql/driver that wraps
// the real modernc "sqlite" driver and delegates EVERYTHING, except it consults a per-open fault plan
// to return a chosen error on the Nth Exec/Query/Commit. This lets a store test drive its DB-error
// and fsync-failure branches (every `if err != nil { return ErrIO }` the real driver never trips)
// without contortions. Registered ONCE under a distinct name; the store opens through it via the
// test-only withSQLiteDriverName option. TEST-ONLY — nothing here is reachable in production.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"

	sqlited "modernc.org/sqlite"
)

// faultPlan decides when the wrapper injects an error. It is matched by a SUBSTRING of the SQL
// (empty = any statement), and fires after `skip` matching calls have passed (so a test can let the
// setup run and fail, say, the 1st checkpoint write). Guarded because database/sql may touch the
// driver from its own goroutines.
type faultPlan struct {
	mu        sync.Mutex
	sqlSubstr string // match statements containing this (case-insensitive); "" = match any
	kind      string // "exec" | "query" | "commit"
	skip      int    // let this many matching calls succeed first
	err       error  // the error to inject once armed
	fired     bool   // set true once injected (one-shot)
}

func (p *faultPlan) shouldFire(kind, query string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil || p.fired || p.kind != kind {
		return false
	}
	if p.sqlSubstr != "" && !strings.Contains(strings.ToUpper(query), strings.ToUpper(p.sqlSubstr)) {
		return false
	}
	if p.skip > 0 {
		p.skip--
		return false
	}
	p.fired = true
	return true
}

// the single registered fault driver + its currently-armed plan (one test at a time; the suite is
// sequential per-package by default and these tests do not run in parallel).
var (
	faultDriverOnce sync.Once
	activePlan      *faultPlan
	activePlanMu    sync.Mutex
)

const faultDriverName = "sqlite-fault"

// registerFaultDriver registers the wrapper once and returns its name.
func registerFaultDriver() string {
	faultDriverOnce.Do(func() {
		sql.Register(faultDriverName, &faultDriver{real: &sqlited.Driver{}})
	})
	return faultDriverName
}

// armFault installs the plan the wrapper consults, and returns a disarm func.
func armFault(p *faultPlan) func() {
	activePlanMu.Lock()
	activePlan = p
	activePlanMu.Unlock()
	return func() {
		activePlanMu.Lock()
		activePlan = nil
		activePlanMu.Unlock()
	}
}

func currentPlan() *faultPlan {
	activePlanMu.Lock()
	defer activePlanMu.Unlock()
	return activePlan
}

// --- the driver wrapper ---

type faultDriver struct{ real driver.Driver }

func (d *faultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.real.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{real: c}, nil
}

type faultConn struct{ real driver.Conn }

// Prepare returns the REAL stmt unwrapped. We do NOT wrap it in a fault-injecting *faultStmt because
// database/sql routes exec/query through faultConn's ExecerContext/QueryerContext (which modernc's conn
// implements) — the store never reaches a prepared-stmt exec, so a stmt wrapper would be dead code that
// only existed to re-implement driver.Stmt's DEPRECATED Exec/Query (SA1019). Fault injection lives at the
// conn level; the stmt path is unused for this driver (reviewer ph78-F5).
func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	return c.real.Prepare(query)
}

func (c *faultConn) Close() error { return c.real.Close() }
func (c *faultConn) Begin() (driver.Tx, error) {
	return c.beginCtx(context.Background(), driver.TxOptions{})
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.beginCtx(ctx, opts)
}

func (c *faultConn) beginCtx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	// Begin injection (review ph78-F3): modernc starts a txn via ConnBeginTx (NOT an Exec("BEGIN")),
	// so the "begin" fault kind must be consulted HERE — matched by "" query — to exercise Save's
	// classifyTxErr("begin", err) branch (incl. the ErrBusy-at-begin classification).
	if p := currentPlan(); p != nil && p.shouldFire("begin", "") {
		return nil, p.err
	}
	// modernc (the only driver this test wrapper wraps) implements ConnBeginTx; require it rather than
	// fall back to the deprecated driver.Conn.Begin() (SA1019). If a future driver lacked it, fail loud.
	bt, ok := c.real.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("faultdriver: wrapped driver lacks ConnBeginTx")
	}
	tx, err := bt.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{real: tx}, nil
}

// ExecerContext / QueryerContext: modernc's conn implements these; delegate + inject.
func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if p := currentPlan(); p != nil && p.shouldFire("exec", query) {
		return nil, p.err
	}
	if ec, ok := c.real.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if p := currentPlan(); p != nil && p.shouldFire("query", query) {
		return nil, p.err
	}
	if qc, ok := c.real.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

type faultTx struct{ real driver.Tx }

func (t *faultTx) Commit() error {
	if p := currentPlan(); p != nil && p.shouldFire("commit", "") {
		return p.err
	}
	return t.real.Commit()
}
func (t *faultTx) Rollback() error { return t.real.Rollback() }

// ensure the interfaces are satisfied at compile time.
var (
	_ driver.Driver         = (*faultDriver)(nil)
	_ driver.Conn           = (*faultConn)(nil)
	_ driver.ConnBeginTx    = (*faultConn)(nil)
	_ driver.ExecerContext  = (*faultConn)(nil)
	_ driver.QueryerContext = (*faultConn)(nil)
	_ io.Closer             = (*faultConn)(nil)
)
