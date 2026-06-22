package workflow

import (
	"errors"
	"fmt"
)

// Sentinel errors for the persistence and validation boundaries.
//
// These categorize failures returned by WorkflowStore implementations (and the
// validation guards that feed them) so callers can branch with errors.Is
// without string-matching. Each is wrapped with %w at its origin, so the
// underlying detail remains available via errors.Unwrap/errors.As while the
// category stays stable.
//
// Note: these are distinct from the action-execution sentinels in action.go
// (ErrInputNotFound / ErrInvalidInput / ErrExecutionFailed), which describe a
// different domain (an Action's runtime behavior, not store/persistence I/O).
// The two sets are intentionally NOT aliased: ErrNotFound (a workflow is not on
// disk) is a different concept from ErrInputNotFound (a data key is absent).
var (
	// ErrNotFound indicates the requested workflow does not exist (e.g. no file
	// on disk for the given ID, or no in-memory entry). Distinct from a
	// permission or other I/O failure, which is ErrIO.
	ErrNotFound = errors.New("workflow not found")

	// ErrValidation indicates invalid input rejected before any I/O — an empty
	// or unsafe workflow ID, nil data, or an otherwise malformed request.
	ErrValidation = errors.New("validation failed")

	// ErrCorruptData indicates persisted data could not be decoded: a malformed,
	// truncated, or version-skewed FlatBuffers/JSON payload. The wrapped detail
	// is kept generic at the boundary so it does not leak file paths or raw
	// panic internals.
	ErrCorruptData = errors.New("corrupt workflow data")

	// ErrIO indicates a transient or environmental I/O failure that is neither
	// a not-found nor a corruption: a read/write/delete that failed for reasons
	// such as permissions, a full disk, or an unavailable directory.
	ErrIO = errors.New("workflow I/O error")
)

// ioError is the error returned at the I/O boundary. Its Error() string is
// deliberately path-free — a generic headline plus the validated workflowID —
// because the underlying os error (an *os.PathError) embeds the absolute file
// path, which must not leak across the public boundary (M3-SEC-01). The cause
// stays reachable for debugging via errors.As / errors.Is (e.g. for
// *os.PathError or fs.ErrNotExist) through the multi-error Unwrap.
type ioError struct {
	op         string // verb, e.g. "read"/"write"/"delete"
	workflowID string
	cause      error // underlying os error; reachable via Unwrap, NOT rendered in Error()
}

// Error renders a generic, path-free message. The underlying os error (which
// embeds the absolute path) is intentionally omitted from this string.
func (e *ioError) Error() string {
	return fmt.Sprintf("%s: failed to %s workflow %q", ErrIO.Error(), e.op, e.workflowID)
}

// Unwrap exposes both the ErrIO sentinel (so errors.Is(err, ErrIO) holds) and
// the underlying cause (so errors.As / errors.Is can reach the os error) —
// without ever putting the cause's path-bearing string into Error().
func (e *ioError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrIO}
	}
	return []error{ErrIO, e.cause}
}

// newIOError builds a path-free I/O boundary error that still carries the cause
// for debugging via errors.As / errors.Unwrap.
func newIOError(op, workflowID string, cause error) error {
	return &ioError{op: op, workflowID: workflowID, cause: cause}
}
