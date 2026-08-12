# 04 · Choice and merge

Conditional branching, end to end: a choice routes the run down exactly one arm based on
its data, the arms it did not pick end **Bypassed**, and a merge re-converges the branches
into a single downstream result. The same graph is run twice — once premium, once standard —
so both arms are exercised.

## What it shows

- **`AddChoice(name).When(pred, arm).Otherwise(arm)`** — the routing decision *is* the
  choice's action, so a `ChoiceNode` takes `When` / `Otherwise` arms, not `WithActionFunc`.
  `DependsOn("seed")` makes it evaluate only after the routing input is set.
- **`Bypassed`** — the two arms the choice did not take end in the `Bypassed` terminal
  status: an explicit, durable "deliberately not run", distinct from `Skipped` and `Pending`.
  The one taken arm ends `Completed`.
- **`AddMerge(name).From(arms...)`** — the M11 OR-join. A plain node that
  `DependsOn`ed several choice arms directly would be rejected at **Build** as an
  unstructured reconvergence; the merge is the only legal way to re-converge choice arms.
  Here every arm writes the same key, so the merge reads one key regardless of which won.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/04-choice-and-merge
GOTOOLCHAIN=local go test ./examples/04-choice-and-merge/ -count=1
```

## Key API calls

```go
b.AddChoice("route").
    DependsOn("seed").
    When(func(d *workflow.WorkflowData) bool { k, _ := d.GetString(keyKind); return k == "premium" }, "premium-price").
    When(func(d *workflow.WorkflowData) bool { k, _ := d.GetString(keyKind); return k == "standard" }, "standard-price").
    Otherwise("reject")

b.AddNode("premium-price").DependsOn("route").WithActionFunc(fn)

b.AddMerge("total").
    From("premium-price", "standard-price", "reject").
    WithActionFunc(fn)

data.GetNodeStatus("standard-price") // == workflow.Bypassed when premium was taken
```

## Expected output

Two runs. For `kind="premium"` the premium arm prices `3 × 2000¢` and the merge publishes
`final_total=6000¢`; for `kind="standard"` the standard arm prices `3 × 1000¢` and the merge
publishes `3000¢`. In each run the two arms that were not chosen end `Bypassed`, and the
choice, the taken arm, and the merge all end `Completed`.
