package workflow

// M16 ph76 — the MP Locker adapter: wires the cross-process ClaimStore into the executor's
// existing Locker seam (workflow.go:215), so a real Execute drive in MP mode CLAIMS a durable
// lease at drive-start, runs (the checkpoint CAS + renew-on-checkpoint ride the ph74/ph75
// machinery), and RELEASES at drive-end. This is the whole executor wiring for competing
// consumers — the executor CORE is unchanged; only the injected Locker impl differs.
//
// DESIGN (the ph76 fork, MP-Locker-adapter — the recommended shape): the Locker contract
// (Acquire(ctx, workflowID) (release, err)) is a perfect fit for Claim→run→Release. Acquire →
// Claim (which sets the per-process tokenState the checkpoint fencing CAS already reads, the
// ph74 bridge); the returned release → Release. The default in-process Locker (a per-WorkflowID
// mutex) is unchanged and remains the single-process default — the MP Locker only engages when
// a caller injects it via WithLocker, so det-tax and the single-process path are untouched.

import (
	"context"
	"errors"
)

// claimLocker is a Locker backed by a ClaimStore — it acquires the drive lease by CLAIMing a
// durable cross-process lease, and releases it by Releasing the claim. Injected via WithLocker.
type claimLocker struct {
	store   ClaimStore
	ownerID string // this process's opaque identity (caller-supplied; see WithMultiProcessLocker)
}

// newMultiProcessLocker builds a Locker that claims a durable cross-process lease from `store` on
// behalf of `ownerID` for each drive. INTERNAL (unexported, F-M16-SEC-1): the ONLY public way to
// enable MP competing consumers is Workflow.WithMultiProcessLocker(ownerID), which DERIVES the
// Locker from w.Store so the same-instance requirement below is guaranteed by construction. The raw
// constructor is NOT exported because passing a `store` that is not the IDENTICAL instance the drive
// checkpoints through is a silent SAFETY footgun (see the CRITICAL note) — an easy-to-misuse public
// surface security's worst-failure class; 1.0 would freeze it.
//
// CRITICAL (reviewer m16-ph76-F1): `store` MUST be the IDENTICAL `*SQLiteStore` INSTANCE (same
// pointer) as the `Workflow.Store` the drive checkpoints through — NOT merely another handle on
// the same DB file. The fencing token this Locker's Claim sets lives in that store instance's
// in-memory tokenState, and the checkpoint CAS reads tokenState on `Workflow.Store`. If the two
// are different instances, Claim sets tokenState on one and the CAS reads the other (held == 0 →
// fencing silently no-ops → a superseded zombie's write lands with no error). WithMultiProcessLocker
// makes this mismatch unrepresentable.
//
// ownerID is the caller-supplied OPAQUE process identity (the library does not generate it —
// identity policy belongs to the host: a hostname+pid, a container id, a uuid). It must be
// STABLE for the life of a process and DISTINCT across competing worker processes.
//
// A drive whose workflow is already owned by a LIVE lease held by another owner gets ErrClaimLost
// from Acquire (the competing consumer that lost the race does not run); the caller can
// errors.Is(err, ErrClaimLost) and move to the next workflow. A LAPSED lease is re-claimed (a
// bumped token) — the re-claim-after-death path. IN-PROCESS SCOPE: the MP Locker does NOT
// serialize concurrent same-(WorkflowID, ownerID) drives within one process (re-entrant Claim
// returns the held token without blocking) — drive one (WorkflowID, owner) at a time per process,
// or compose over an in-process Locker if you fan out one owner across goroutines (F4).
func newMultiProcessLocker(store ClaimStore, ownerID string) Locker {
	return &claimLocker{store: store, ownerID: ownerID}
}

// Acquire claims the durable lease for workflowID. On success it returns a release func that
// Releases the claim (best-effort clean handoff). On ErrClaimLost (a LIVE foreign lease) it
// returns the error — unlike the in-process mutex it does NOT block waiting for a live foreign
// lease (that is a different worker legitimately running; blocking would defeat competing
// consumers). ctx is accepted for interface symmetry; Claim is a fast single-txn call.
func (l *claimLocker) Acquire(_ context.Context, workflowID string) (func(), error) {
	token, err := l.store.Claim(workflowID, l.ownerID)
	if err != nil {
		return nil, err // ErrClaimLost (owned by another) or a store error — the drive does not run.
	}
	return func() {
		// Best-effort Release on drive end. A Release that is itself fenced (a re-claim already
		// superseded us) is a no-op by token mismatch — harmless. We swallow the release error:
		// the successor already owns a higher token, so a failed release cannot cause a safety
		// issue (the fencing token, not the lease-row presence, is the arbiter).
		_ = l.store.Release(workflowID, token) //nolint:errcheck // best-effort clean handoff
	}, nil
}

// WithMultiProcessLocker injects an MP Locker DERIVED FROM this workflow's own Store — the
// SAFE, mismatch-proof way to enable cross-process competing consumers (reviewer m16-ph76-F1). It
// asserts `w.Store` is a `ClaimStore` and builds the Locker over THAT SAME instance, so the
// token this Locker's Claim sets and the token the checkpoint CAS reads are guaranteed to be the
// same in-memory tokenState — the instance-mismatch footgun of an explicit-store constructor is
// unrepresentable. This is the ONLY public entry to MP mode (F-M16-SEC-1: the raw
// newMultiProcessLocker(store,…) is unexported precisely so this footgun can't reach a caller).
// Panics if w.Store is not a ClaimStore (a programmer error: MP mode needs a WithMultiProcess
// SQLiteStore) — fail-loud, never a silently-unfenced drive. ownerID is the opaque process identity
// (stable per process, distinct across competing workers). Returns the workflow for chaining.
func (w *Workflow) WithMultiProcessLocker(ownerID string) *Workflow {
	cs, ok := w.Store.(ClaimStore)
	if !ok {
		panic("WithMultiProcessLocker: Workflow.Store must be a ClaimStore (a multi-process SQLiteStore via WithMultiProcess)")
	}
	w.Locker = &claimLocker{store: cs, ownerID: ownerID}
	return w
}

// Ensure claimLocker satisfies Locker.
var _ Locker = (*claimLocker)(nil)

// isSupersededError reports whether err means "this worker was superseded" (ErrFencedOut) — the
// abort-vs-retry discriminator for a competing consumer at the Execute return. A superseded
// worker must NOT retry (a re-claim owns the workflow now); a transient ErrBusy/IO MAY retry.
func isSupersededError(err error) bool { return errors.Is(err, ErrFencedOut) }
