# 08 — Sub-workflows

Composition: a **parent** workflow runs a whole **child** graph as a single node.

```
parent:  prepare ──▶ [index]  ──▶ finalize
                        │
                        └─ child:  do-index   (its own DAG, its own journal)
```

## What it shows

- **`AddSubWorkflow(name, childDAG)`** — spawns and awaits a definition-value child DAG
  **in-process** under a deterministic child `WorkflowID` (`f(parentID, nodeName)`) with the
  child's **own journal**. Parent and child are *distinct* durable workflows (one-writer per
  workflow is preserved). The child is built with a bare builder — `Build()` returns a
  `*DAG`, exactly what `AddSubWorkflow` consumes; it inherits the parent's store at spawn.
- **`WithResult(parentKey, childDataKey)`** — on child success, copies a child data key up
  into parent data. A **scalar** (int64/string/bool/float) round-trips type-faithfully across
  every store. `finalize` then consumes it as ordinary parent data.
- **Distinct durable identity** — the child's own record is loadable independently via
  `SubWorkflowChildID(parentID, nodeName)`, proving it ran as its own run, not inline code.
- **Depth bound** — nesting is capped by `MaxSubWorkflowDepth` (default **8**), enforced as a
  loud `ErrSubWorkflowMaxDepth`, never a silent truncation. An **inline** child may not
  contain a suspendable node (Build scans the closure and refuses one) — route a parking
  child to `AddSubWorkflowParked` / `AddSubWorkflowQueued`.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/08-sub-workflows
GOTOOLCHAIN=local go test ./examples/08-sub-workflows/ -count=1
```

## Key API

| Call | Role |
|---|---|
| `WorkflowBuilder.Build()` → `*DAG` | build the child graph (bare, no store) |
| `WorkflowBuilder.AddSubWorkflow(name, child)` | run the child DAG as one parent node |
| `NodeBuilder.WithResult(parentKey, childDataKey)` | copy the child's scalar result up |
| `SubWorkflowChildID(parentID, nodeName)` | recompute the child's deterministic id |
| `FromBuilder(b)` | store-backed parent `*Workflow` |

## Expected output

```
prepare: 3 documents to index
child do-index: indexed 3 documents (in the child's own journal)
finalize: parent sees indexed_total=3 from the child

result: parent indexed_total=3, child(sub:…) done=true
```

(The `sub:…` id is the deterministic SHA-256 child id — stable per parent+node.)
