# 02 · Errors and retries

Failure handling, end to end: transient failures that recover on retry, a non-critical node
whose failure must not sink the run, and a genuinely fatal failure that does — plus the two
error domains the library keeps distinct.

## What it shows

- **`WithRetries(n)`** — the builder's built-in retry. `flaky-retry` fails its first attempt
  and is Completed after recovering on attempt 2.
- **`RetryableAction` with capped backoff + jitter** — `flaky-backoff` wraps its action in
  `NewRetryableAction(a, maxRetries, delay).WithBackoff(2).WithMaxDelay(20ms).WithJitter(0.3)`
  and recovers on attempt 3. Use *either* `WithRetries` *or* a wrapped `RetryableAction` on a
  node, never both (they are two front-ends to the same retry loop).
- **`WithContinueOnError()`** — `optional-metrics` always fails, but its failure does not
  abort the run: a continue-on-error node that Failed still *resolves* its dependents, so the
  downstream `summarize` node runs anyway.
- **A fatal failure** — `charge-card` needs an `amount` input that was never set, returns
  `ErrInputNotFound`, and (not being continue-on-error) fail-fasts the run: `ship` is Skipped
  and `Execute` returns an `*ExecutionError`.
- **The two error domains** — action sentinels (`ErrInputNotFound` / `ErrInvalidInput` /
  `ErrExecutionFailed`) reachable through the run's error with `errors.Is`, versus store
  sentinels (`ErrNotFound` / `ErrValidation` / `ErrCorruptData` / `ErrIO`). A missing
  workflow is `ErrNotFound`, never `ErrInputNotFound` — the sets are intentionally not aliased.

## Run it

```bash
GOTOOLCHAIN=local go run ./examples/02-errors-and-retries
GOTOOLCHAIN=local go test ./examples/02-errors-and-retries/ -count=1
```

## Key API calls

```go
b.AddNode("flaky-retry").WithActionFunc(fn).WithRetries(3).DependsOn("seed")

backoff := workflow.NewRetryableAction(fn, 4, 2*time.Millisecond).
    WithBackoff(2).WithMaxDelay(20 * time.Millisecond).WithJitter(0.3)
b.AddNode("flaky-backoff").WithAction(backoff).DependsOn("seed")

b.AddNode("optional-metrics").WithActionFunc(fn).WithContinueOnError().DependsOn("seed")

err := wf.Execute(ctx)
errors.Is(err, workflow.ErrInputNotFound)   // action domain
var execErr *workflow.ExecutionError
errors.As(err, &execErr)                     // execErr.FailedNodes
errors.Is(loadErr, workflow.ErrNotFound)     // store domain
```

## Expected output

The resilient pipeline reports each flaky node's failed attempts then its winning attempt
(`flaky-retry` on 2, `flaky-backoff` on 3), and `summary_written=true` — proof the
continue-on-error failure did not abort the run. The fatal pipeline prints the failed run's
error, `errors.Is(err, ErrInputNotFound) = true`, the failed node (`charge-card`), and the
store-domain check showing a missing workflow is `ErrNotFound` and not `ErrInputNotFound`.
