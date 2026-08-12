# 03 · Durable crash-resume ⭐

The flagship. It proves the library's whole thesis: **a workflow is data, not replay**. A
run is killed mid-flight; a fresh process resumes off the durable SQLite store and re-runs
no completed work — every side effect happens **exactly once, across the crash**.

```
fetch ──▶ validate ──▶ transform ──▶ load
  each node appends its name to a shared effects file exactly once
```

## What it shows

- **`NewSQLiteStore(path, WithMultiProcess())`** — the durable, cross-process store. Each
  level's checkpoint is fenced and power-loss durable.
- **Crash-durable resume** — a child process (a re-exec of the binary) is killed with
  `os.Exit(137)` (mimicking `kill -9`) the instant it reaches `transform`, *after* `fetch`
  and `validate` have completed and checkpointed. A second process then calls `Execute` on
  the same store; the engine loads the checkpoint, sees `fetch`/`validate` are terminal,
  **skips them**, and runs only `transform` and `load`.
- **Exactly-once, proven not asserted** — each node's side effect is a line appended to a
  shared file. The test proves every node appears exactly once (a replayed node would leave
  a second line) and that the run reaches terminal completion.

## Why the crash lands *between* nodes

A node's completion becomes durable at the **level barrier** — the checkpoint the engine
writes *after* the action returns. The safe crash window is therefore between nodes: after
`validate`'s checkpoint, before `transform` does anything observable. This example kills at
the *start* of `transform`, before its side effect, so the exactly-once proof is honest.

A kill in the *middle* of a side effect (after the append, before the checkpoint) would
re-run that one node on resume. For that window you make the action idempotent (an
idempotency key / upsert) — which is exactly what the durable dispatch path and the chaos
rig (`_local/proving-ground/chaos/killstorm_test.go`) do. Keeping the boundary clean here is
a teaching choice, not a limitation of the engine.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/03-durable-crash-resume
GOTOOLCHAIN=local go test ./examples/03-durable-crash-resume/ -count=1
```

The default `go test` exercises the real crash+resume. `-short` skips the subprocess path
and runs only the fast in-process smoke test.

## Key API calls

```go
store, err := workflow.NewSQLiteStore(dbPath, workflow.WithMultiProcess())

// same builder, run by both the crashed process and the resuming one:
b := workflow.NewWorkflowBuilder().WithWorkflowID("durable-pipeline").WithStore(store)
b.AddStartNode("fetch").WithActionFunc(fn)
b.AddNode("validate").WithActionFunc(fn).DependsOn("fetch")
// ...
wf, err := workflow.FromBuilder(b)

err = wf.Execute(ctx) // on a fresh process this RESUMES: completed nodes are skipped

data, _ := store.Load("durable-pipeline")
data.GetNodeStatus("fetch") // == workflow.Completed
```

## Expected output

```
PHASE 1 — run the pipeline in a child process, killed the moment it reaches transform
  fetch: ran, side effect recorded
  validate: ran, side effect recorded
  transform: SIMULATING A CRASH (os.Exit) before its side effect
  child exited: exit status 137  (a crash is expected)
  effects recorded before the crash: [fetch validate]

PHASE 2 — resume on the SAME store from fresh process state
  transform: ran, side effect recorded
  load: ran, side effect recorded

PHASE 3 — verify exactly-once
  full effects ledger: [fetch validate transform load]
  fetch/validate/transform/load each ran 1 time(s)
  every node Completed; every side effect happened exactly once — across a crash.
```
