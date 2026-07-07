package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TriggerCause is the durable discriminator of WHY a saga rollback was triggered (M12
// ph49, resolves ph48-F2). Journaled at the trigger in the same snapshot as the
// rolling_back marker, so a resumed rollback recovers the TRUE cause across a crash: a
// caller-cancel stays a cancel, a deadline a deadline — never a spurious node-failure
// inferred from an incidental Failed node. TriggerNone is a run that is not rolling back
// (or a pre-ph49 snapshot, which reconstructCause handles via the existing inference).
type TriggerCause uint8

const (
	// TriggerNone is a run that is not rolling back (or a pre-ph49 durable snapshot).
	TriggerNone TriggerCause = iota
	// TriggerFailure is a rollback triggered by a hard *ExecutionError (a node failed).
	TriggerFailure
	// TriggerCanceled is a rollback triggered by the caller's context being canceled.
	TriggerCanceled
	// TriggerDeadlineExceeded is a rollback triggered by the caller's context deadline.
	TriggerDeadlineExceeded
)

// errRecoveredCompensationFailure is the placeholder error for a CompensationFailed
// node reconstructed from durable status on resume: only the STATUS is journaled, not
// the transient compensation error string (DEC-M12-P48-RECONSTRUCT). The status — the
// load-bearing fact that the effect is NOT undone — is exact; the original error string
// is a documented, honest loss, not a silent one.
var errRecoveredCompensationFailure = errors.New(
	"compensation failed (recovered from durable status; original error not journaled)")

// errRecoveredForwardFailure is the placeholder for the forward failure of a node
// reconstructed from a persisted Failed status on resume — the in-hand trigger
// *ExecutionError belonged to a prior (pre-crash) run.
var errRecoveredForwardFailure = errors.New(
	"node failed (recovered from durable status; original error not journaled)")

// DefaultRollbackTimeout bounds a saga rollback when the workflow sets no explicit
// RollbackTimeout (ph46-F2 / DEC-M12-P47-DEADLINE): best-effort must be bounded, not
// eternal, so a hung compensation cannot hang the run. A generous default (undoing
// side-effects may be slow) that is still finite; override via WithRollbackTimeout, or
// set a negative value for an explicitly unbounded rollback.
const DefaultRollbackTimeout = 5 * time.Minute

// compIdempotencyKeyCtxKey is the context key under which the saga rollback drive
// stashes the per-node idempotency handle a compensation reads.
type compIdempotencyKeyCtxKey struct{}

// withCompensationKey injects the stable per-node idempotency handle a compensation
// reads via CompensationIdempotencyKey. The rollback drive sets it before invoking
// each node's compensation (M12, red-team MAJOR-3): DEC-M12-STATE's no-Compensating-
// status choice leans on at-least-once + idempotency for the crash-resume (ph48), so
// the handle must be available to the compensation NOW even though ph46 never crashes.
func withCompensationKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, compIdempotencyKeyCtxKey{}, key)
}

// CompensationIdempotencyKey returns the stable idempotency handle for the
// compensation currently running under a saga rollback, and whether one is present
// (it is present only while a compensation runs). A compensation uses it to
// deduplicate its effect on a downstream system across an at-least-once
// re-invocation — a crash mid-rollback re-runs the compensation (ph48), and the key
// is byte-identical across that re-run (it is IdempotencyKey(data, nodeName), derived
// only from WorkflowID+nodeName), so the downstream dedupes the re-invocation as one
// logical undo.
func CompensationIdempotencyKey(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(compIdempotencyKeyCtxKey{}).(string)
	return key, ok
}

// hasCompensations reports whether the DAG declares any compensating action. When
// false the whole run is a NON-SAGA run and the rollback trigger stays inert, so a
// failing run is byte-for-byte the pre-M12 path (the moat guarantee: no compensation
// declared => exactly the old behavior — no rolling_back marker, a single failed-state
// save). Scanned only on the failure path, never the hot path.
func (w *Workflow) hasCompensations() bool {
	for _, n := range w.DAG.Nodes {
		if n.Compensation != nil {
			return true
		}
	}
	return false
}

// driveRollback runs the saga compensation pass for a run that failed with a hard
// error (its rolling_back marker is set). It invokes the compensation of every
// Completed compensable node in REVERSE-topological order — the forward levels
// (DAG.GetLevels) peeled back-to-front, within-level concurrent and bounded by
// MaxConcurrency — and marks each Compensated on success.
//
// Reverse-topological is the ONLY crash-durable order: the journal persists no
// per-node completion order (workflow_store.go), so "undo in the inverse of the
// dependency order" is the only ordering reconstructable after a crash (DEC-M12-ORDER).
//
// FRESH context (DEC-M12-CTX): the caller's ctx may already be cancelled — a caller
// cancel TRIGGERS rollback (DEC-M12-TRIGGER) but must NOT abort it — so the rollback
// runs under context.Background() (with the workflow Clock injected, parity with
// executeLocked), never the cancelled forward ctx.
//
// Each compensation receives the live post-failure WorkflowData (its forward node's
// output is present, M9) and, on ctx, a stable IdempotencyKey handle (MAJOR-3).
//
// SCOPE (SAGA-04 / DEC-M12-SUSPEND): only a Completed node that DECLARES a
// compensation is compensated. A Completed node with no compensation is a rollback
// no-op (nothing to undo). Bypassed / Skipped / Waiting / Failed / never-run nodes
// are NEVER compensated — they have no successful effect to undo.
//
// Returns the sagaOutcome — the EXACT partition of the run's Completed nodes across
// {compensated, failedToCompensate, skipped} — which executeLocked turns into a typed
// *SagaError when any compensation failed (ph47). Single-shot; crash-resume is ph48.
func (w *Workflow) driveRollback(data *WorkflowData) *sagaOutcome {
	// Fresh context — NOT the (possibly cancelled) caller ctx. Inject the workflow
	// Clock so a compensation reads "now" through it (no-determinism-tax parity with
	// executeLocked), defaulting to the system clock.
	ctx := context.Background()
	clock := w.Clock
	if clock == nil {
		clock = systemClock{}
	}
	ctx = withClock(ctx, clock)

	// §4 rollback-scoped deadline (ph46-F2 fold, DEC-M12-P47-DEADLINE). Dropping the
	// caller's Cancel (DEC-M12-CTX) so a caller-cancel does not ABORT the rollback does
	// NOT mean unbounded — this scoped deadline is the bound. Past it the ctx cancels
	// and any still-blocked compensation is recorded CompensationFailed, so a hung
	// compensation can never hang the run. A non-positive timeout means no deadline.
	if timeout := w.rollbackTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxConc := w.DAG.config.MaxConcurrency
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}

	out := &sagaOutcome{}
	// §3 / review ph48-F4: a fully-rolled-back run (no Completed compensable node
	// remains — "rollback complete") needs no drive. Skip the reverse loop and its
	// redundant per-level Saves; reconstructOutcome rebuilds the honest outcome from
	// durable statuses regardless. A re-drive is then a true no-op (only the final Save).
	if !w.hasPendingCompensation(data) {
		return out
	}
	levels := w.DAG.GetLevels()
	for i := len(levels) - 1; i >= 0; i-- {
		compensateLevel(ctx, levels[i], data, maxConc, out)
		// §1 per-reverse-level checkpoint (DEC-M12-P48-CHECKPOINT): persist progress
		// after each level so a crash mid-rollback leaves the completed-so-far
		// compensations durably Compensated/CompensationFailed. On resume, the switch
		// re-enters here and compensateLevel skips every already-terminal node (only
		// Completed is processed), re-running ONLY the still-Completed compensable
		// nodes. The sole re-run window is SetNodeStatus->Save, covered by the
		// IdempotencyKey handle (at-least-once + idempotent effect). Mirrors the M9
		// per-level checkpoint. A checkpoint Save error is folded into the outcome
		// (ph47-F2 pattern) — finishRollback's final Save stays authoritative.
		if w.Store != nil {
			if err := w.Store.Save(data); err != nil {
				out.mu.Lock()
				out.saveErr = err
				out.mu.Unlock()
			}
		}
	}
	return out
}

// hasPendingCompensation reports whether any node still needs compensating — a
// Completed node that declares a compensation. Used to fast-path a re-drive of a
// fully-rolled-back run (§3 rollback-complete = this returns false).
func (w *Workflow) hasPendingCompensation(data *WorkflowData) bool {
	for name, n := range w.DAG.Nodes {
		if n.Compensation == nil {
			continue
		}
		if st, _ := data.GetNodeStatus(name); st == Completed {
			return true
		}
	}
	return false
}

// reconstructOutcome builds the FULL saga partition from DURABLE node statuses, merged
// with the fresh in-pass outcome (§2, DEC-M12-P48-RECONSTRUCT — resolves ph47-F5). A
// resumed rollback's fresh sagaOutcome only covers the nodes it re-processed AFTER the
// crash; the honest partition of the WHOLE run must also account for the nodes marked
// Compensated/CompensationFailed BEFORE the crash. So scan every node's durable status:
//
//	Compensated        -> compensated
//	CompensationFailed -> failed (this drive's real error if present, else "recovered")
//	Completed + no comp -> skipped (nothing to undo)
//	Completed + has comp -> failed (defensive: unreachable after a COMPLETE pass — the
//	                        reverse loop reaches every level; a residual one is un-undone)
//
// Non-Completed nodes (Failed/Bypassed/Skipped/Pending) are NOT in the partition. Lists
// are sorted for deterministic output (the DAG.Nodes map iterates in random order).
func (w *Workflow) reconstructOutcome(data *WorkflowData, fresh *sagaOutcome) *sagaOutcome {
	freshFailed := make(map[string]error, len(fresh.failedToCompensate))
	for _, ne := range fresh.failedToCompensate {
		freshFailed[ne.NodeName] = ne.Err
	}
	out := &sagaOutcome{saveErr: fresh.saveErr}
	for name, n := range w.DAG.Nodes {
		status, _ := data.GetNodeStatus(name)
		switch {
		case status == Compensated:
			out.compensated = append(out.compensated, name)
		case status == CompensationFailed:
			err := freshFailed[name]
			if err == nil {
				err = errRecoveredCompensationFailure // pre-crash failure; string not journaled
			}
			out.failedToCompensate = append(out.failedToCompensate, NodeError{NodeName: name, Err: err})
		case status == Completed && n.Compensation == nil:
			out.skipped = append(out.skipped, name)
		case status == Completed: // has a compensation but still Completed — defensive
			out.failedToCompensate = append(out.failedToCompensate,
				NodeError{NodeName: name, Err: errRecoveredCompensationFailure})
		}
	}
	sort.Strings(out.compensated)
	sort.Strings(out.skipped)
	sort.Slice(out.failedToCompensate, func(i, j int) bool {
		return out.failedToCompensate[i].NodeName < out.failedToCompensate[j].NodeName
	})
	return out
}

// reconstructCause rebuilds the forward *ExecutionError from the persisted Failed
// node(s) (§2). A resumed rollback lost the in-hand trigger error (a prior run), so the
// cause is derived from the durable Failed statuses — the fail-fast node(s) that
// triggered the rollback. Returns nil when no node is Failed (a caller-cancel-triggered
// rollback leaves no Failed node; the reconstructed cause is then nil and the *SagaError
// partition alone carries the outcome).
func reconstructCause(data *WorkflowData, dag *DAG) error {
	// M12 ph49 (resolves ph48-F2): route on the JOURNALED trigger cause. A cancel
	// stays a cancel and a deadline a deadline — errors.Is-matchable — instead of being
	// mis-reconstructed as a node failure from an incidental Failed node (a mid-level
	// cancel marks its in-flight node Failed). The sentinel is wrapped so the message is
	// honest AND errors.Is(err, context.Canceled/DeadlineExceeded) matches.
	switch data.TriggerCause() {
	case TriggerCanceled:
		return fmt.Errorf("saga rolled back: %w", context.Canceled)
	case TriggerDeadlineExceeded:
		return fmt.Errorf("saga rolled back: %w", context.DeadlineExceeded)
	case TriggerFailure, TriggerNone:
		// Failure: reconstruct the *ExecutionError from the Failed node(s) — the durable
		// witness. TriggerNone (a pre-ph49 snapshot) falls to the SAME inference for
		// backward compatibility; the ErrRolledBack floor covers a cause-less resume.
	}

	var failures []NodeError
	for name := range dag.Nodes {
		if status, _ := data.GetNodeStatus(name); status == Failed {
			failures = append(failures, NodeError{NodeName: name, Err: errRecoveredForwardFailure})
		}
	}
	if len(failures) == 0 {
		return nil // a TRUE nil error — NOT a typed-nil *ExecutionError in an interface
	}
	return newExecutionError(failures) // sorts by name
}

// rollbackTimeout resolves the scoped rollback deadline: the workflow's RollbackTimeout
// when set, else DefaultRollbackTimeout. A caller who explicitly wants an UNBOUNDED
// rollback sets a negative RollbackTimeout (a non-positive resolved value → no deadline).
func (w *Workflow) rollbackTimeout() time.Duration {
	if w.RollbackTimeout != 0 {
		return w.RollbackTimeout
	}
	return DefaultRollbackTimeout
}

// sagaOutcome collects the per-node rollback outcomes across the whole reverse pass
// (concurrent within a level — hence mutex-guarded). It is the EXACT partition of the
// run's Completed nodes: every Completed node lands in exactly one slice.
type sagaOutcome struct {
	mu                 sync.Mutex
	compensated        []string
	failedToCompensate []NodeError
	skipped            []string
	saveErr            error // §1: the last per-reverse-level checkpoint Save error, if any
}

func (o *sagaOutcome) markCompensated(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.compensated = append(o.compensated, name)
}

func (o *sagaOutcome) markFailed(name string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failedToCompensate = append(o.failedToCompensate, NodeError{NodeName: name, Err: err})
}

func (o *sagaOutcome) markSkipped(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skipped = append(o.skipped, name)
}

// compensateLevel runs the compensations of one forward level concurrently (bounded by
// maxConc), reusing the semaphore pattern from executeNodesInLevel. BEST-EFFORT (ph47):
// a compensation that fails after RetryCount does NOT abort the rollback — the node is
// marked CompensationFailed and recorded, and every other eligible compensation still
// runs (all levels, all within-level siblings). A Completed node with no compensation
// is recorded `skipped` (nothing to undo; its status stays Completed); a non-Completed
// node is not part of the partition.
func compensateLevel(ctx context.Context, level []*Node, data *WorkflowData, maxConc int, out *sagaOutcome) {
	// Defensive coercion, parity with executeNodesInLevel: a non-positive limit would
	// make an UNBUFFERED semaphore and deadlock the first send (review ph46-F1).
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup
	for _, node := range level {
		status, _ := data.GetNodeStatus(node.Name)
		if status != Completed {
			continue // never-run / Failed / Bypassed / Skipped — not in the completed partition
		}
		if node.Compensation == nil {
			out.markSkipped(node.Name) // Completed, nothing to undo; status stays Completed
			continue
		}
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Present the stable dedup handle to the compensation (MAJOR-3). The key
			// is derived from WorkflowID+nodeName, so it is identical across an
			// at-least-once re-invocation on resume (ph48).
			compCtx := withCompensationKey(ctx, IdempotencyKey(data, n.Name))
			if err := runCompensationWithRetry(compCtx, n, data); err != nil {
				// BEST-EFFORT: record the failure, mark the node, and RETURN (do not
				// abort the wave). The node's effect is NOT undone — the *SagaError
				// surfaces it to the caller.
				data.SetNodeStatus(n.Name, CompensationFailed)
				out.markFailed(n.Name, err)
				return
			}
			data.SetNodeStatus(n.Name, Compensated)
			out.markCompensated(n.Name)
		}(node)
	}
	wg.Wait()
}

// compensationRetryBackoff is the delay between compensation retry attempts, mirroring
// the forward RetryableAction's 1s inter-retry budget (DEC-M12-RETRY, review ph47-F3):
// a transient fault (contention, rate limit) is far more likely to clear across a
// spaced retry than a tight loop. The backoff is ctx-aware, so the rollback deadline
// still bounds it.
const compensationRetryBackoff = time.Second

// runCompensationWithRetry invokes a node's compensation, retrying up to RetryCount
// times on error (DEC-M12-RETRY), with a ctx-aware backoff between attempts.
//
// It BOUNDS the compensation against the rollback deadline even if the compensation
// IGNORES ctx (review ph47-F1): each attempt runs Execute in a child goroutine and
// selects it against ctx.Done(), so a compensation that blocks forever does NOT hang
// the run — the deadline (or shutdown) fires, the node is recorded CompensationFailed,
// and the rollback proceeds. The abandoned goroutine LEAKS (a ctx-ignoring compensation
// is a user bug); such a compensation MUST NOT mutate shared state after ctx is done.
// Compensations do not park (DEC-M12-SUSPEND); an ErrSuspended is a plain error.
func runCompensationWithRetry(ctx context.Context, n *Node, data *WorkflowData) error {
	var lastErr error
	for attempt := 0; attempt <= n.RetryCount; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Bound Execute itself against ctx: context.WithTimeout cannot preempt a
		// goroutine, so a ctx-ignoring blocking Execute would otherwise hang wg.Wait()
		// forever. Run it in a child goroutine and race it against ctx.Done().
		errCh := make(chan error, 1)
		go func() { errCh <- n.Compensation.Execute(ctx, data) }()
		select {
		case err := <-errCh:
			if err == nil {
				return nil
			}
			lastErr = err
		case <-ctx.Done():
			return ctx.Err()
		}
		// ctx-aware backoff before the next attempt (skip after the last).
		if attempt < n.RetryCount {
			select {
			case <-time.After(compensationRetryBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}
