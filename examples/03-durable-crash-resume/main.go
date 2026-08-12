// Command 03-durable-crash-resume is the flagship: it proves the library's whole thesis —
// a workflow is DATA, so a crash-killed run RESUMES off the durable store and re-runs no
// completed work. No replay, no re-execution of side effects that already happened.
//
//	fetch ──▶ validate ──▶ transform ──▶ load
//	  │          │            │            │
//	  └── each node appends its name to a shared effects file exactly once ──┘
//
// The demonstration:
//
//   - PHASE 1 runs the pipeline in a CHILD process (a re-exec of this binary) that is
//     killed — via os.Exit, mimicking a `kill -9` — the instant it reaches `transform`,
//     AFTER `fetch` and `validate` have completed and been checkpointed to SQLite.
//   - PHASE 2 RESUMES on the SAME SQLite store from fresh process state. The engine loads
//     the checkpoint, sees `fetch` and `validate` are already terminal, SKIPS them, and
//     runs only `transform` and `load`.
//   - PHASE 3 reads the effects file and proves every node's side effect appears EXACTLY
//     ONCE — the crash-durable, exactly-once property that is the point.
//
// WHY the crash lands at the START of `transform`, before its side effect. Completion is
// made durable at the LEVEL BARRIER — the checkpoint the engine writes AFTER a node's
// action returns. So the safe crash window is BETWEEN nodes: after `validate`'s checkpoint,
// before `transform` does anything observable. A kill in the MIDDLE of a side effect (after
// the append, before the checkpoint) would re-run that one node on resume — for that window
// you make the action idempotent (an idempotency key / upsert), which is what the durable
// dispatch path and the chaos rig do. This example keeps the boundary clean so the
// exactly-once proof is honest rather than hand-waved.
//
// Run it:
//
//	go run ./examples/03-durable-crash-resume
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

const workflowID = "durable-pipeline"

// The pipeline's nodes, in dependency order. Each is its own level, so the checkpoint
// between them is where crash-durability lives.
const (
	nodeFetch     = "fetch"
	nodeValidate  = "validate"
	nodeTransform = "transform"
	nodeLoad      = "load"
)

// pipelineNodes lists the four in execution order — the exact multiset the effects file
// must contain after a successful (possibly resumed) run.
var pipelineNodes = []string{nodeFetch, nodeValidate, nodeTransform, nodeLoad}

// Env contract for the re-exec'd worker child. The parent (demo or test) sets these; the
// child reads them at startup and becomes a crashing worker.
const (
	envRole    = "CRASH_RESUME_ROLE"     // "worker" => this process is the crashing child
	envDB      = "CRASH_RESUME_DB"       // shared SQLite path
	envEffects = "CRASH_RESUME_EFFECTS"  // shared effects-log path
	envCrashAt = "CRASH_RESUME_CRASH_AT" // node name at whose START the child self-kills
)

// crashAtNode arms the crash. It is set ONLY in a worker process (from envCrashAt); in the
// resuming/normal process it stays "", so no node ever self-kills there. A package var is
// the honest model of "this process was told to die at node X" — a fresh resume process
// carries no such instruction.
var crashAtNode string

// maybeCrash self-kills the process at the START of the armed node, BEFORE any side effect.
// os.Exit(137) mimics SIGKILL (128+9): no deferred Close runs, exactly as a real kill.
func maybeCrash(node string) {
	if crashAtNode != "" && node == crashAtNode {
		fmt.Printf("  [pid %d] %s: SIMULATING A CRASH (os.Exit) before its side effect\n", os.Getpid(), node)
		os.Exit(137)
	}
}

// appendEffect records that a node ran, by appending its name to the shared effects file.
// This is the observable "side effect" whose exactly-once-ness across a crash is the whole
// proof: a re-executed node would leave a second line. O_APPEND makes each write atomic.
func appendEffect(effectsPath, node string) error {
	f, err := os.OpenFile(effectsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintln(f, node)
	return err
}

// readEffects returns the node names recorded so far, in order.
func readEffects(effectsPath string) ([]string, error) {
	f, err := os.Open(effectsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// effectNode builds an action that (1) may crash at its boundary, then (2) records its
// side effect exactly once and marks itself done in the durable data. The crash check is
// the FIRST statement, so an armed node never reaches its append — its side effect simply
// does not happen, and neither does its checkpoint.
func effectNode(node, effectsPath string) workflow.ActionFunc {
	return func(_ context.Context, data *workflow.WorkflowData) error {
		maybeCrash(node) // on an armed worker this never returns
		if err := appendEffect(effectsPath, node); err != nil {
			return fmt.Errorf("%s: record side effect: %w", node, err) // infra failure
		}
		data.Set(node+"_ran", true)
		fmt.Printf("  [pid %d] %s: ran, side effect recorded\n", os.Getpid(), node)
		return nil
	}
}

// buildPipelineWorkflow constructs the durable pipeline on the given store. Standalone so
// the test builds the identical graph the command runs — the anti-rot guarantee.
func buildPipelineWorkflow(store workflow.WorkflowStore, effectsPath string) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(workflowID).
		WithStore(store)

	b.AddStartNode(nodeFetch).WithActionFunc(effectNode(nodeFetch, effectsPath))
	b.AddNode(nodeValidate).WithActionFunc(effectNode(nodeValidate, effectsPath)).DependsOn(nodeFetch)
	b.AddNode(nodeTransform).WithActionFunc(effectNode(nodeTransform, effectsPath)).DependsOn(nodeValidate)
	b.AddNode(nodeLoad).WithActionFunc(effectNode(nodeLoad, effectsPath)).DependsOn(nodeTransform)

	return workflow.FromBuilder(b)
}

// openStore opens the durable, multi-process SQLite store at path. WithMultiProcess is the
// mode a crash-and-resume workload wants: two OS processes (the killed worker, then the
// resuming one) touch the same DB, and each checkpoint is fenced and power-loss durable.
func openStore(path string) (*workflow.SQLiteStore, error) {
	return workflow.NewSQLiteStore(path, workflow.WithMultiProcess())
}

// runWorkerProcess is the crashing child. It opens the shared store, builds the pipeline
// with the crash ARMED (crashAtNode set from env), and executes — the armed node self-kills
// mid-run. If Execute returns at all, the crash injection failed and we exit non-zero so the
// parent notices rather than silently proceeding.
func runWorkerProcess() {
	dbPath := os.Getenv(envDB)
	effectsPath := os.Getenv(envEffects)
	crashAtNode = os.Getenv(envCrashAt) // ARM the crash for this process only

	store, err := openStore(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: open store: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = store.Close() }() // deliberately NOT reached when the crash fires

	wf, err := buildPipelineWorkflow(store, effectsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: build: %v\n", err)
		os.Exit(2)
	}
	_ = wf.Execute(context.Background()) // the armed node os.Exit(137)s before returning
	fmt.Fprintln(os.Stderr, "worker: Execute returned without crashing — crash injection did not fire")
	os.Exit(3)
}

// spawnCrashingWorker re-execs THIS binary as a worker child that dies at nodeTransform. It
// mirrors the chaos rig's re-exec pattern, simplified to one deterministic kill. Returns the
// child's exit error (non-nil is EXPECTED — the child crashed).
func spawnCrashingWorker(dbPath, effectsPath string, wireStdio bool) error {
	// CommandContext (not Command) satisfies the repo's noctx lint and is the right default;
	// we never cancel it here — the child self-terminates via os.Exit(137).
	cmd := exec.CommandContext(context.Background(), os.Args[0])
	cmd.Env = append(os.Environ(),
		envRole+"=worker",
		envDB+"="+dbPath,
		envEffects+"="+effectsPath,
		envCrashAt+"="+nodeTransform,
	)
	if wireStdio {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

// countEach returns a name->count map over the effects.
func countEach(effects []string) map[string]int {
	counts := make(map[string]int, len(effects))
	for _, e := range effects {
		counts[e]++
	}
	return counts
}

func demo() error {
	dir, err := os.MkdirTemp("", "crash-resume-demo-")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dbPath := filepath.Join(dir, "pipeline.db")
	effectsPath := filepath.Join(dir, "effects.log")

	fmt.Println("PHASE 1 — run the pipeline in a child process, killed the moment it reaches transform")
	crashErr := spawnCrashingWorker(dbPath, effectsPath, true)
	fmt.Printf("  child exited: %v  (a crash is expected)\n", crashErr)
	afterCrash, err := readEffects(effectsPath)
	if err != nil {
		return fmt.Errorf("read effects after crash: %w", err)
	}
	fmt.Printf("  effects recorded before the crash: %v\n\n", afterCrash)

	fmt.Println("PHASE 2 — resume on the SAME store from fresh process state")
	store, err := openStore(dbPath)
	if err != nil {
		return fmt.Errorf("open store for resume: %w", err)
	}
	defer func() { _ = store.Close() }()
	wf, err := buildPipelineWorkflow(store, effectsPath)
	if err != nil {
		return fmt.Errorf("build for resume: %w", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("resume execute: %w", err) // a real resume must succeed
	}

	fmt.Println("\nPHASE 3 — verify exactly-once")
	effects, err := readEffects(effectsPath)
	if err != nil {
		return fmt.Errorf("read effects: %w", err)
	}
	fmt.Printf("  full effects ledger: %v\n", effects)
	counts := countEach(effects)
	for _, node := range pipelineNodes {
		fmt.Printf("  %-9s ran %d time(s)\n", node, counts[node])
		if counts[node] != 1 {
			return fmt.Errorf("exactly-once VIOLATED: %s ran %d times, want 1", node, counts[node])
		}
	}
	data, err := store.Load(workflowID)
	if err != nil {
		return fmt.Errorf("load final state: %w", err)
	}
	for _, node := range pipelineNodes {
		if st, _ := data.GetNodeStatus(node); st != workflow.Completed {
			return fmt.Errorf("node %s final status = %v, want Completed", node, st)
		}
	}
	fmt.Println("  every node Completed; every side effect happened exactly once — across a crash.")
	return nil
}

func main() {
	// A re-exec of this binary with CRASH_RESUME_ROLE=worker is the crashing child.
	if os.Getenv(envRole) == "worker" {
		runWorkerProcess() // never returns
	}
	if err := demo(); err != nil {
		log.Fatalf("03-durable-crash-resume: %v", err)
	}
}
