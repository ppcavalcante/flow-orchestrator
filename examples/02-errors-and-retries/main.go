// Command 02-errors-and-retries teaches failure handling. It runs two workflows on one
// in-memory store:
//
//   - a RESILIENT pipeline that survives transient failures — a node that fails its first
//     attempts and RECOVERS on retry (both the builder's WithRetries and an explicitly
//     wrapped RetryableAction with capped backoff + jitter), alongside a non-critical node
//     marked WithContinueOnError whose failure does NOT abort the run; and
//
//   - a FATAL pipeline where a critical node fails for good, so the whole run surfaces an
//     error and its downstream never runs.
//
//     ┌─▶ flaky-retry ────────┐            (resilient)
//     seed ───┼─▶ flaky-backoff ───────┼─▶ summarize
//     └─▶ optional-metrics ────┘
//     (continue-on-error)
//
//     seed ───▶ charge-card ───▶ ship        (fatal: charge-card needs an input that
//     (needs "amount", absent)      seed never set → ErrInputNotFound → ship
//     is Skipped, Execute returns an error)
//
// It also shows the TWO error domains the library keeps distinct:
//   - action sentinels (ErrInputNotFound / ErrInvalidInput / ErrExecutionFailed) — what an
//     action's runtime behaviour reports; reachable through Execute's error with errors.Is;
//   - store sentinels (ErrNotFound / ErrValidation / ErrCorruptData / ErrIO) — what a
//     WorkflowStore reports. A missing workflow is ErrNotFound, NOT ErrInputNotFound.
//
// Run it:
//
//	go run ./examples/02-errors-and-retries
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// WorkflowData keys. Each flaky node publishes the attempt on which it finally succeeded
// so a reader (and the test) can confirm it really retried rather than passed first try.
const (
	resilientID = "resilient-pipeline"
	fatalID     = "fatal-pipeline"

	keyRetryAttempts   = "flaky-retry_attempts"
	keyBackoffAttempts = "flaky-backoff_attempts"
	keySummary         = "summary_written"
)

// Node names, shared by the builders and the test so a rename can't drift the two apart.
const (
	nodeSeed      = "seed"
	nodeRetry     = "flaky-retry"
	nodeBackoff   = "flaky-backoff"
	nodeOptional  = "optional-metrics"
	nodeSummarize = "summarize"

	nodeChargeCard = "charge-card"
	nodeShip       = "ship"
)

// flakyAction returns an action that FAILS on its first (succeedOnAttempt-1) attempts and
// SUCCEEDS on attempt number succeedOnAttempt. The attempt counter lives in the closure —
// not in WorkflowData — so it survives regardless of whether a failed attempt's data
// writes are kept, making the recovery demonstration robust. On success it records the
// winning attempt number into data under attemptsKey for the reader and the test.
//
// The transient errors wrap ErrExecutionFailed: a retry loop should recover from a
// genuine execution failure, and wrapping the sentinel lets a caller classify it with
// errors.Is without string-matching.
func flakyAction(name, attemptsKey string, succeedOnAttempt int) workflow.ActionFunc {
	attempts := 0
	return func(_ context.Context, data *workflow.WorkflowData) error {
		attempts++
		if attempts < succeedOnAttempt {
			fmt.Printf("  %s: attempt %d failed (transient)\n", name, attempts)
			return fmt.Errorf("%w: %s transient failure on attempt %d", workflow.ErrExecutionFailed, name, attempts)
		}
		data.Set(attemptsKey, int64(attempts))
		fmt.Printf("  %s: succeeded on attempt %d\n", name, attempts)
		return nil
	}
}

// buildResilientWorkflow constructs the pipeline that survives transient and non-critical
// failures. It is a standalone helper so the test builds the identical graph.
func buildResilientWorkflow(store workflow.WorkflowStore) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(resilientID).
		WithStore(store)

	b.AddStartNode(nodeSeed).WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set("order_id", "ord-42")
		fmt.Println("  seed: order_id=ord-42")
		return nil
	})

	// flaky-retry uses the builder's own WithRetries: fail attempt 1, succeed attempt 2.
	// WithRetries(3) allows up to 3 retries (4 total attempts), so recovering on attempt 2
	// leaves the node Completed.
	b.AddNode(nodeRetry).
		WithActionFunc(flakyAction(nodeRetry, keyRetryAttempts, 2)).
		WithRetries(3).
		DependsOn(nodeSeed)

	// flaky-backoff wraps the action in a RetryableAction with CAPPED exponential backoff
	// and jitter, then hands it to WithAction. Delays are tiny so the example stays fast:
	// base 2ms, ×2 each attempt (2ms, 4ms, 8ms…), capped at 20ms, jittered down by up to
	// 30%. It recovers on attempt 3. (Use EITHER WithRetries OR a wrapped RetryableAction
	// on a node, not both — they are two front-ends to the same retry behaviour, and
	// stacking them would nest two retry loops.)
	backoff := workflow.NewRetryableAction(
		flakyAction(nodeBackoff, keyBackoffAttempts, 3),
		4,                  // maxRetries: up to 4 retries
		2*time.Millisecond, // base delay
	).WithBackoff(2).WithMaxDelay(20 * time.Millisecond).WithJitter(0.3)
	b.AddNode(nodeBackoff).
		WithAction(backoff).
		DependsOn(nodeSeed)

	// optional-metrics is non-critical: it always fails, but WithContinueOnError means its
	// failure does NOT abort the run. A continue-on-error node that Failed still RESOLVES
	// its dependents, so summarize (below) runs anyway. This is how you model best-effort
	// side work — telemetry, cache warming — that must never sink the pipeline.
	b.AddNode(nodeOptional).
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
			fmt.Println("  optional-metrics: failed (non-critical, run continues)")
			return fmt.Errorf("%w: metrics backend unreachable", workflow.ErrExecutionFailed)
		}).
		WithContinueOnError().
		DependsOn(nodeSeed)

	// summarize depends on all three. It runs because the two flaky nodes recovered to
	// Completed and the continue-on-error node's Failed status still resolves the edge.
	b.AddNode(nodeSummarize).
		WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
			opt, _ := data.GetNodeStatus(nodeOptional)
			fmt.Printf("  summarize: proceeding (optional-metrics=%s)\n", opt)
			data.Set(keySummary, true)
			return nil
		}).
		DependsOn(nodeRetry, nodeBackoff, nodeOptional)

	return workflow.FromBuilder(b)
}

// buildFatalWorkflow constructs a pipeline whose critical node fails for good. charge-card
// requires an "amount" input that seed deliberately never sets, so it returns
// ErrInputNotFound — a real action-domain failure. It is NOT continue-on-error and does
// not retry, so the run fail-fasts: ship is Skipped and Execute returns an ExecutionError.
func buildFatalWorkflow(store workflow.WorkflowStore) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(fatalID).
		WithStore(store)

	b.AddStartNode(nodeSeed).WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set("order_id", "ord-99") // note: no "amount" — that omission is the failure
		return nil
	})

	b.AddNode(nodeChargeCard).
		WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
			if _, ok := data.GetInt64("amount"); !ok {
				return fmt.Errorf("%w: charge-card needs %q", workflow.ErrInputNotFound, "amount")
			}
			return nil
		}).
		DependsOn(nodeSeed)

	b.AddNode(nodeShip).
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
			fmt.Println("  ship: THIS SHOULD NEVER PRINT")
			return nil
		}).
		DependsOn(nodeChargeCard)

	return workflow.FromBuilder(b)
}

func runResilient(store workflow.WorkflowStore) error {
	fmt.Println("=== resilient pipeline: retries + continue-on-error ===")
	wf, err := buildResilientWorkflow(store)
	if err != nil {
		return fmt.Errorf("build resilient: %w", err) // infra error — the wiring is broken
	}
	// A nil error is the EXPECTED outcome here: the flaky nodes recover and the only
	// failure is a continue-on-error node. A non-nil error would be a real defect.
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("resilient execute (unexpected — this pipeline should survive): %w", err)
	}
	data, err := store.Load(resilientID)
	if err != nil {
		return fmt.Errorf("load resilient: %w", err)
	}
	ra, _ := data.GetInt64(keyRetryAttempts)
	ba, _ := data.GetInt64(keyBackoffAttempts)
	sum, _ := data.GetBool(keySummary)
	fmt.Printf("result: flaky-retry won on attempt %d, flaky-backoff on attempt %d, summary_written=%v\n\n", ra, ba, sum)
	return nil
}

func runFatal(store workflow.WorkflowStore) error {
	fmt.Println("=== fatal pipeline: a critical failure surfaces as an error ===")
	wf, err := buildFatalWorkflow(store)
	if err != nil {
		return fmt.Errorf("build fatal: %w", err) // infra error
	}
	// This Execute is DELIBERATELY expected to fail — a demonstrated outcome, not infra.
	// We report it and keep going; we do NOT return it as the command's error.
	err = wf.Execute(context.Background())
	if err == nil {
		return fmt.Errorf("fatal execute returned nil — the demonstration is broken")
	}
	fmt.Printf("  charge-card failed the run as designed: %v\n", err)
	fmt.Printf("  errors.Is(err, ErrInputNotFound) = %v  (action domain)\n", errors.Is(err, workflow.ErrInputNotFound))

	var execErr *workflow.ExecutionError
	if errors.As(err, &execErr) {
		for _, ne := range execErr.FailedNodes {
			fmt.Printf("  failed node: %s\n", ne.NodeName)
		}
	}

	// The two-domain distinction, made concrete: loading a workflow that does not exist is
	// a STORE failure (ErrNotFound), a different sentinel from any action error.
	if _, loadErr := store.Load("no-such-workflow"); loadErr != nil {
		fmt.Printf("  store.Load(missing): errors.Is(err, ErrNotFound)=%v, errors.Is(err, ErrInputNotFound)=%v  (store domain, NOT action domain)\n",
			errors.Is(loadErr, workflow.ErrNotFound), errors.Is(loadErr, workflow.ErrInputNotFound))
	}
	fmt.Println()
	return nil
}

func run() error {
	store := workflow.NewInMemoryStore()
	if err := runResilient(store); err != nil {
		return err
	}
	return runFatal(store)
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("02-errors-and-retries: %v", err)
	}
}
