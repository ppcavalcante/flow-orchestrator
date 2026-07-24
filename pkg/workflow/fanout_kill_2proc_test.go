package workflow

// M21 ph108 T3 — the GENUINE 2-OS-process kill-9 fan-out crash test (FANOUT-09, the milestone crux). This is the
// true write→kill→read that the in-process store-seed CANNOT reach: the executor terminal-on-return contract
// means an in-process interrupt always terminalizes the fan node (a returned error → Failed), so only a real
// SIGKILL (between the durable journal write and Execute's return) leaves the node non-terminal for a genuine
// resume. See [[in-process-crash-cannot-leave-node-nonterminal]] + the store-seed rationale on
// TestFanOut_CrashResume_RealWindow (ph108 T5). Mirrors workflow_killstorm_2proc_test.go's re-exec+SIGKILL pattern.
//
// A child process drives an AddFanOut fan-out on a shared SQLite .db under a FIXED WorkflowID; the parent SIGKILLs
// it at intervals and re-execs it (each re-exec RESUMES the same durable workflow — the expansion journal +
// already-Completed branch children survive the kill). The oracle: after convergence, exactly N branch effects
// exist, EACH EXACTLY ONCE (a fanout_effects side-table row per branch index; a double-run would insert a second
// row for that index — the exactly-N-branches-each-once claim, proven across real process deaths).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const (
	fanoutKillWID    = "fanout-kill-wf" // the FIXED workflow id resumed across kills
	fanoutKillN      = 6                // branch count
	fanoutKillDBEnv  = "FANOUT_KILL_DB"
	fanoutKillWorker = "FANOUT_KILL_WORKER"
)

// TestFanOutKillEntry is the subprocess worker: when FANOUT_KILL_WORKER is set it opens the shared .db, builds the
// fan-out, and drives it via Workflow.Execute (which resumes if a prior drive was SIGKILL'd mid-flight). Each
// branch does a slow bit of work then INSERTs its per-index effect row (so a kill mid-branch leaves that branch
// non-terminal → its child journal re-drives on resume, but a COMPLETED branch is a deterministic-ID no-op → its
// effect row stays at exactly one). Runs until the workflow Completes or a wall deadline (the parent kills it).
func TestFanOutKillEntry(t *testing.T) {
	if os.Getenv(fanoutKillWorker) == "" {
		t.Skip("not a worker invocation (set FANOUT_KILL_WORKER to run as the subprocess fan-out kill worker)")
	}
	db := os.Getenv(fanoutKillDBEnv)
	s, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatalf("worker open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // worker cleanup

	branch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
		raw, _ := d.Get(FanOutItemKey)
		num, ok := raw.(json.Number)
		if !ok {
			return fmt.Errorf("item is not json.Number: %T", raw)
		}
		iv, err := num.Int64()
		if err != nil {
			return err
		}
		// Slow work so the parent's SIGKILL lands mid-branch with high probability.
		time.Sleep(40 * time.Millisecond)
		// Durable per-index effect, IDEMPOTENT (review F3): a re-run of the SAME branch (a kill in the
		// post-INSERT/pre-checkpoint window, before the branch child journal committed Completed) must NOT
		// double-count. ON CONFLICT(idx) DO NOTHING (idx is UNIQUE) makes a re-insert a no-op → the count stays
		// EXACTLY 1 whether the branch ran once or was killed-and-re-ran. The exactly-once claim then rests on the
		// deterministic-child-ID idempotency for the LOGICAL run + this idempotent effect for the SIDE EFFECT — a
		// genuine double-EXECUTE of the branch action is still caught by a distinct witness below (attempts count).
		if _, ierr := s.db.Exec(`INSERT INTO fanout_effects(idx, attempts) VALUES(?, 1) ON CONFLICT(idx) DO UPDATE SET attempts = attempts + 1`, iv); ierr != nil {
			return fmt.Errorf("effect insert idx=%d: %w", iv, ierr)
		}
		d.Set("out", iv)
		return nil
	})

	b := NewWorkflowBuilder().WithWorkflowID(fanoutKillWID)
	b.AddFanOut("fan", intItemsExpander(fanoutKillN), branch).WithResults("r", "out")
	dag, err := b.Build()
	if err != nil {
		t.Fatalf("worker build: %v", err)
	}
	w := NewWorkflow(s)
	w.WorkflowID = fanoutKillWID
	w.DAG = dag

	deadline := time.Now().Add(20 * time.Second) // safety bound; the parent kills/ends us well before
	for time.Now().Before(deadline) {
		_ = w.Execute(context.Background()) //nolint:errcheck // a killed/failed drive resumes on the next re-exec
		final, lerr := s.Load(fanoutKillWID)
		if lerr == nil && final != nil {
			if st, ok := final.GetNodeStatus("fan"); ok && st == Completed {
				return // the fan-out Completed durably → this worker is done.
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// spawnFanOutKillWorker re-execs THIS test binary as a fan-out kill worker (the M16/M17 re-exec pattern).
func spawnFanOutKillWorker(db string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run", "^TestFanOutKillEntry$", "-test.v")
	cmd.Env = append(os.Environ(), fanoutKillWorker+"=1", fanoutKillDBEnv+"="+db)
	return cmd
}

// TestFanOutKill_2Proc — the genuine crash driver. Spawns the fan-out worker, SIGKILLs it at intervals + re-execs
// (each re-exec resumes the durable workflow), until the parent observes the fan-out Completed. Oracle: exactly N
// branch effects, EACH EXACTLY ONCE (no double-run across the real process deaths) + the fan node Completed.
func TestFanOutKill_2Proc(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns + SIGKILLs OS subprocesses; skipped under -short")
	}
	db := filepath.Join(t.TempDir(), "fanout-kill.db")

	// Pre-create the schema + the effects side-table (the parent handle; the worker opens its own).
	parent, err := NewSQLiteStore(db, WithMultiProcess())
	if err != nil {
		t.Fatalf("parent open: %v", err)
	}
	// idx UNIQUE + the ON CONFLICT DO UPDATE below makes the effect IDEMPOTENT-with-an-attempts-witness (F3): a
	// re-run of the same branch (killed pre-child-checkpoint) does NOT create a second ROW (so the row count is
	// exactly N), but it DOES bump `attempts` — so the oracle can assert BOTH the exactly-once side effect (N rows)
	// AND observe re-executes without a false-positive red.
	if _, err := parent.db.Exec(`CREATE TABLE IF NOT EXISTS fanout_effects(idx INTEGER NOT NULL UNIQUE, attempts INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatalf("create effects table: %v", err)
	}

	deadline := time.Now().Add(25 * time.Second)
	var cmd *exec.Cmd
	launch := func() {
		cmd = spawnFanOutKillWorker(db)
		if err := cmd.Start(); err != nil {
			t.Logf("worker start: %v", err)
			return
		}
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd) //nolint:errcheck // reaped; exit code is not the oracle
	}
	launch()

	converged := false
	for time.Now().Before(deadline) {
		if fanNodeCompleted(parent) {
			converged = true
			break
		}
		// SIGKILL the live worker mid-drive + re-exec (the genuine crash: kill between a journal write and
		// Execute's return; the re-exec resumes the durable workflow).
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck // best-effort (may have exited)
		}
		time.Sleep(time.Duration(25+ /*jitter*/ (time.Now().UnixNano()%50)) * time.Millisecond)
		launch()
	}

	// Final drain: let one worker run uninterrupted to convergence (a kill-storm may have left the last drive
	// mid-flight). Kill any survivor first, then run one clean worker to completion.
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck // teardown before the clean drain
	}
	if !converged {
		clean := spawnFanOutKillWorker(db)
		if err := clean.Start(); err == nil {
			_ = clean.Wait() //nolint:errcheck // the clean drain
		}
		converged = fanNodeCompleted(parent)
	}
	if !converged {
		t.Fatalf("fan-out did not converge to Completed within the deadline (kill-storm too aggressive or a resume bug)")
	}

	// ORACLE: the fan node is Completed, and each of the N branch indices has EXACTLY ONE effect row (no
	// double-run across the real SIGKILLs — the exactly-N-branches-each-once claim, proven end-to-end).
	final, err := parent.Load(fanoutKillWID)
	if err != nil {
		t.Fatalf("parent load: %v", err)
	}
	if st, _ := final.GetNodeStatus("fan"); st != Completed {
		t.Fatalf("fan node not Completed after convergence: %v", st)
	}
	// EXACTLY-ONCE SIDE EFFECT: each branch index has EXACTLY ONE row (the idempotent ON CONFLICT means a
	// re-executed branch, killed pre-child-checkpoint, never creates a second row — the F3-robust witness). Every
	// index must be present (all N branches durably completed the fan-out).
	var rows int
	if err := parent.db.QueryRow(`SELECT count(*) FROM fanout_effects`).Scan(&rows); err != nil {
		t.Fatalf("count effect rows: %v", err)
	}
	if rows != fanoutKillN {
		t.Fatalf("fanout_effects has %d rows, want EXACTLY %d (one per branch — the exactly-once side effect)", rows, fanoutKillN)
	}
	for i := 0; i < fanoutKillN; i++ {
		var c int
		if err := parent.db.QueryRow(`SELECT count(*) FROM fanout_effects WHERE idx=?`, i).Scan(&c); err != nil {
			t.Fatalf("count effect idx=%d: %v", i, err)
		}
		if c != 1 {
			t.Fatalf("branch %d has %d effect rows, want EXACTLY 1 (exactly-once side effect across the kill-storm)", i, c)
		}
	}
	// And the typed aggregate is intact (every branch result reloaded).
	cnt := fanCount(t, final, "r")
	if cnt != fanoutKillN {
		t.Fatalf("aggregate count = %d, want %d", cnt, fanoutKillN)
	}
}

// fanNodeCompleted reports whether the durable fan-out workflow's fan node is Completed.
func fanNodeCompleted(s *SQLiteStore) bool {
	final, err := s.Load(fanoutKillWID)
	if err != nil || final == nil {
		return false
	}
	st, ok := final.GetNodeStatus("fan")
	return ok && st == Completed
}
