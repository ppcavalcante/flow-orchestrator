# 05 · Dynamic fan-out

Map a branch action over **N items discovered at runtime** — the width is not known when the
graph is built. A single `AddFanOut` node expands into N parallel branches; each branch sees
only its own item and records a per-item result; the node collects the N typed results back
into the parent data under indexed keys.

## What it shows

- **`AddFanOut(name, expander, branchAction)`** — the expander runs inside the parent
  `Execute` and returns the ordered list of per-branch inputs (`len == N`), derived from the
  run data at run time. Here it reads a seeded page count and emits one item per page.
- **`workflow.FanOutItemKey`** — a branch reads its own item under this key. Items are
  JSON-journaled, so an integer item arrives as `json.Number` (int64-faithful) — call
  `.Int64()` for a concrete int.
- **`WithResults(base, branchKey)`** — each branch's `branchKey` scalar is collected TYPED
  into parent data under `base[i]` in discovery order, plus `base.__count__ = N`. An `int64`
  result reloads as an `int64` on every store, not a lossy float64/JSON-string.
- **`WithMaxWidth(n)`** — a guardrail: an expansion wider than `n` is refused at run time
  (`ErrFanOutMaxWidth`), before any branch work.

Fan-out requires a `Checkpointer` store because the expander runs **exactly once** even
across a crash+resume — its result is journaled, so a resume rebuilds the exact branches
without re-calling the expander. (`WithCollectPartial()` — not used here — flips a failed
branch from fail-fast to a partition of succeeded/failed indices under `base.__failed__`.)

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/05-dynamic-fanout
GOTOOLCHAIN=local go test ./examples/05-dynamic-fanout/ -count=1
```

## Key API calls

```go
b.AddFanOut("render", expandPages, workflow.ActionFunc(ocrBranch)).
    WithResults("ocr_sizes", "size").
    WithMaxWidth(64).
    DependsOn("seed")

// expander: discover N at run time
func expandPages(_ context.Context, data *workflow.WorkflowData) ([]interface{}, error) {
    n, _ := data.GetInt64("page_count")
    items := make([]interface{}, n)
    for i := range items { items[i] = i }
    return items, nil
}

// branch: read its item, set a typed result
raw, _ := data.Get(workflow.FanOutItemKey)
page, _ := raw.(json.Number).Int64()
data.Set("size", (page+1)*100)

// parent: read the collected results
count, _ := data.GetInt64("ocr_sizes.__count__")
v, _ := data.GetInt64("ocr_sizes[0]")
```

## Expected output

The expander fans out over 5 discovered pages; the five branches run in parallel (their print
order varies), each producing `size = (page+1)*100`. The final line reports `5 branches, total
size 1500` — proof every branch effect landed exactly once, collected typed into
`ocr_sizes[0..4]` with `ocr_sizes.__count__ = 5`.
