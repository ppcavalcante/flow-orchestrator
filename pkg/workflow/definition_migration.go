package workflow

import (
	"errors"
	"fmt"
)

// AUD-070 / O-03 — a typed definition-mismatch + a migration hook. The AUD-010 digest
// guard rejects a resume onto a graph whose definition changed, but it did so with an
// anonymous ErrValidation: a host could neither classify the failure nor do anything
// about it but restart from scratch. This gives the mismatch a TYPE (so a host can read
// the two digests and fork/transform) and an opt-in HANDLER (so a host can accept a
// compatible change or migrate the persisted state in place).

// ErrDefinitionChanged is the sentinel a resume returns when the graph DEFINITION digest
// differs from the checkpoint's and no migration handler accepted the change. It is
// matched by errors.Is on a *DefinitionMismatchError, alongside ErrValidation (so both a
// specific handler and an existing generic classifier keep working).
var ErrDefinitionChanged = errors.New("workflow definition changed since checkpoint")

// DefinitionMismatch describes a resume onto a graph whose DefinitionDigest differs from
// the checkpoint's. It is handed to a DefinitionMigration so a host can decide what to do.
type DefinitionMismatch struct {
	WorkflowID      string
	PersistedDigest string // the full digest stamped into the checkpoint
	CurrentDigest   string // the full digest of the graph being resumed onto
}

// DefinitionMigration decides what happens on a definition-digest mismatch, replacing the
// default hard reject. It receives the mismatch and the LOADED state, and may:
//   - transform data IN PLACE and return nil to ACCEPT the resume onto the new graph
//     (the current digest is then re-stamped, so the next resume matches);
//   - return nil WITHOUT transforming to accept a compatible/additive change as-is;
//   - return a non-nil error to REJECT — that error is what Execute returns verbatim.
//
// The handler runs BEFORE any node executes, holding no user code but the host's own, so
// a transform here rehydrates state to match the new graph before the drive begins.
type DefinitionMigration func(mismatch DefinitionMismatch, data *WorkflowData) error

// DefinitionMismatchError is the typed error a resume returns when the graph definition
// changed and no DefinitionMigration accepted it. errors.As it to read the digests and
// build a migration (fork the WorkflowID, clear+restart, or wire WithDefinitionMigration).
// It satisfies errors.Is for both ErrDefinitionChanged and ErrValidation.
type DefinitionMismatchError struct {
	WorkflowID      string
	PersistedDigest string
	CurrentDigest   string
}

func (e *DefinitionMismatchError) Error() string {
	return fmt.Sprintf(
		"cannot resume workflow %q: the graph DEFINITION changed since the checkpoint "+
			"(persisted digest %s, current %s) — a node, edge, retry/timeout/continue-on-error policy, "+
			"compensation, boundary, action kind, or suspendability differs; resuming would rehydrate "+
			"state that no longer matches the graph. Fork the WorkflowID, clear the persisted state, or "+
			"supply Workflow.WithDefinitionMigration to accept/transform the change",
		e.WorkflowID, shortDigest(e.PersistedDigest), shortDigest(e.CurrentDigest))
}

// Is reports a match for the typed sentinel AND ErrValidation, so both a specific
// classifier (ErrDefinitionChanged / errors.As) and the pre-existing generic one
// (ErrValidation) keep working across this change.
func (e *DefinitionMismatchError) Is(target error) bool {
	return target == ErrDefinitionChanged || target == ErrValidation
}

// WithDefinitionMigration installs a handler consulted on a definition-digest mismatch
// instead of the default hard reject (AUD-070). Returns the workflow for chaining.
func (w *Workflow) WithDefinitionMigration(fn DefinitionMigration) *Workflow {
	w.migration = fn
	return w
}
