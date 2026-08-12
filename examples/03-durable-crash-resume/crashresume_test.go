package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestCrashResumeWorkerEntry is the crashing-child entrypoint, gated on the worker env var
// exactly as the chaos rig's TestKillStormWorkerEntry is. The heavy test below re-execs
// THIS test binary with -test.run pointing here and the env armed; runWorkerProcess then
// os.Exit(137)s mid-pipeline. On a normal `go test` invocation the env is unset and this
// skips, so it is inert.
func TestCrashResumeWorkerEntry(t *testing.T) {
	if os.Getenv(envRole) != "worker" {
		t.Skip("not a worker invocation (CRASH_RESUME_ROLE != worker)")
	}
	runWorkerProcess() // never returns — it self-kills inside transform
	t.Fatal("worker returned without crashing")
}

// TestCrashResume_ExactlyOnce is the flagship assertion. A child process runs the pipeline
// and is killed the instant it reaches `transform` (after `fetch` and `validate` have
// checkpointed); a fresh process then resumes off the same SQLite store. The proof: every
// node's side effect appears EXACTLY ONCE and the run reaches terminal completion — no
// completed work was replayed across the crash.
func TestCrashResume_ExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("re-execs a crashing subprocess against a real SQLite store; skipped under -short")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pipeline.db")
	effectsPath := filepath.Join(dir, "effects.log")

	// PHASE 1: run the pipeline in a child that crashes at `transform`. We re-exec THIS
	// test binary filtered to the worker entrypoint, with the crash armed via env.
	child := exec.Command(os.Args[0], "-test.run", "^TestCrashResumeWorkerEntry$")
	child.Env = append(os.Environ(),
		envRole+"=worker",
		envDB+"="+dbPath,
		envEffects+"="+effectsPath,
		envCrashAt+"="+nodeTransform,
	)
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("child worker exited 0, expected a crash. output:\n%s", out)
	}

	// The child ran fetch and validate (and checkpointed them) but crashed BEFORE
	// transform's side effect. So exactly those two effects exist on disk now.
	afterCrash, err := readEffects(effectsPath)
	if err != nil {
		t.Fatalf("read effects after crash: %v", err)
	}
	if got, want := afterCrash, []string{nodeFetch, nodeValidate}; !equal(got, want) {
		t.Fatalf("effects after crash = %v, want %v (transform must NOT have run pre-crash)\nchild output:\n%s", got, want, out)
	}

	// PHASE 2: resume on the same store from fresh (this-process) state. crashAtNode is ""
	// here, so nothing self-kills; the engine loads the checkpoint and skips fetch+validate.
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("open store for resume: %v", err)
	}
	defer func() { _ = store.Close() }()

	wf, err := buildPipelineWorkflow(store, effectsPath)
	if err != nil {
		t.Fatalf("build for resume: %v", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("resume execute: %v", err)
	}

	// PHASE 3: exactly-once. Every node's side effect appears exactly once across the
	// crash — the two pre-crash nodes were NOT replayed on resume.
	effects, err := readEffects(effectsPath)
	if err != nil {
		t.Fatalf("read effects: %v", err)
	}
	counts := countEach(effects)
	for _, node := range pipelineNodes {
		if counts[node] != 1 {
			t.Errorf("%s ran %d times, want exactly 1 (effects=%v)", node, counts[node], effects)
		}
	}
	if len(effects) != len(pipelineNodes) {
		t.Errorf("total effects = %d (%v), want %d — no extra or missing side effects", len(effects), effects, len(pipelineNodes))
	}

	// And the durable state reached terminal completion for every node.
	data, err := store.Load(workflowID)
	if err != nil {
		t.Fatalf("load final state: %v", err)
	}
	for _, node := range pipelineNodes {
		if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %s final status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
}

// TestPipeline_RunsAllNodesOnce is a fast, always-on smoke test (no crash, no subprocess):
// a single in-process run must record every side effect exactly once and complete. It runs
// even under -short, so the graph is never left entirely unexercised.
func TestPipeline_RunsAllNodesOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pipeline.db")
	effectsPath := filepath.Join(dir, "effects.log")

	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	wf, err := buildPipelineWorkflow(store, effectsPath)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	effects, err := readEffects(effectsPath)
	if err != nil {
		t.Fatalf("read effects: %v", err)
	}
	counts := countEach(effects)
	for _, node := range pipelineNodes {
		if counts[node] != 1 {
			t.Errorf("%s ran %d times, want 1 (effects=%v)", node, counts[node], effects)
		}
	}

	data, err := store.Load(workflowID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, node := range pipelineNodes {
		if st, ok := data.GetNodeStatus(node); !ok || st != workflow.Completed {
			t.Errorf("node %s status = %v (ok=%v), want Completed", node, st, ok)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
