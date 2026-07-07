package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrRolledBack is returned by a rolled-back run whose trigger cause cannot be
// reconstructed from durable state on resume (M12 ph48, review ph48-F1). A
// caller-cancel / deadline trigger leaves NO persisted Failed node (unlike a hard
// *ExecutionError), and the trigger cause itself is not journaled — so after a crash
// the resume path has no durable witness of WHY the run rolled back, only THAT it did
// (the durable rolling_back marker). This sentinel is the never-nil floor: a rolled-back
// run is NEVER reported as success. (Faithfully recovering the cancel-vs-failure cause
// across a crash needs a journaled trigger cause — routed UP as ph48-F2.)
var ErrRolledBack = errors.New("workflow rolled back (trigger cause not journaled)")

// SagaError is the honest outcome of a saga rollback in which AT LEAST ONE
// compensation failed (M12 ph47). It reports the EXACT partition of the run's
// Completed nodes across three disjoint sets — every Completed node appears in
// exactly one:
//
//   - Compensated:        its compensation ran and succeeded (effect undone).
//   - FailedToCompensate: its compensation was attempted (after RetryCount) and
//     FAILED — the node's effect is NOT undone and needs operator attention.
//   - Skipped:            it Completed but declared no compensation (nothing to undo).
//
// A saga that half-rolls-back must SAY so: a rollback where every compensation
// succeeded returns the original trigger failure (its Cause), NOT a SagaError — so
// a caller can distinguish a clean rollback from a partial one. A SagaError is
// returned ONLY when FailedToCompensate is non-empty.
//
// Cause is the original hard failure that triggered the rollback (a *ExecutionError,
// or a context cancellation). Unwrap returns it, so a caller can errors.As BOTH the
// *SagaError outcome AND the original *ExecutionError cause off the same error.
type SagaError struct {
	// Cause is the hard failure that triggered the rollback (the forward
	// *ExecutionError or a caller-cancel). Nil for a rollback resumed from a
	// persisted rolling_back snapshot (the original cause was a prior run).
	Cause error

	// Compensated holds the names of nodes whose compensation succeeded.
	Compensated []string

	// FailedToCompensate holds one entry per node whose compensation did NOT succeed —
	// the node name paired with the error. Its effect is NOT undone and needs operator
	// attention. Most entries are "attempted (after RetryCount) and failed" (the error
	// is the compensation's own); an entry carrying context.DeadlineExceeded /
	// context.Canceled may NOT have been invoked at all — the shared rollback deadline
	// fired before its turn (review ph47-F4). Either way the effect is un-undone.
	FailedToCompensate []NodeError

	// Skipped holds the names of Completed nodes that declared no compensation
	// (nothing to undo).
	Skipped []string
}

// Error renders a deterministic summary: how many compensations failed, and the
// failed node names + their errors. Like ExecutionError it contains ONLY node names
// and the errors the compensations themselves returned — never WorkflowData values.
func (e *SagaError) Error() string {
	failed := make([]string, len(e.FailedToCompensate))
	for i, ne := range e.FailedToCompensate {
		failed[i] = ne.Error()
	}
	sort.Strings(failed)
	summary := fmt.Sprintf(
		"saga rollback: %d compensated, %d failed to compensate, %d skipped",
		len(e.Compensated), len(e.FailedToCompensate), len(e.Skipped))
	if len(failed) > 0 {
		summary += ": " + strings.Join(failed, "; ")
	}
	if e.Cause != nil {
		summary += fmt.Sprintf(" (triggered by: %s)", e.Cause.Error())
	}
	return summary
}

// Unwrap returns the original trigger failure so errors.Is / errors.As reach the
// *ExecutionError cause through the *SagaError outcome.
func (e *SagaError) Unwrap() error {
	return e.Cause
}
