package workflow

// M17 ph81 — the dispatch mechanism: a type->DAG-factory Registry + RunNext, which turns a claimed
// WorkItem (ph80's ClaimNext) into an actual RUN. A worker Registers the workflow types it can build
// (actions stay CODE — the factory's closures — so only `type` + `input` are DATA; the moat holds),
// then RunNext claims the oldest pending item of a registered type, rebuilds its DAG, seeds its input,
// drives Execute, and terminalizes the queue row (MarkDone / MarkFailed). One worker, one item at a
// time; the worker POOL + retry + reclaim-after-death are ph82. DISP-01.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DAGFactory builds the *DAG for a workflow type. It carries the CODE (actions as closures) — the
// registry maps a DATA `type` string to it. Input is seeded separately as KV (it is DATA the nodes
// read, not graph structure), so the factory takes no arguments; a type whose input shapes the graph
// would add an additive input-taking variant later (not needed for ph81's KV-input model).
type DAGFactory func() (*DAG, error)

// Registry maps a workflow `type` to the DAGFactory that builds it. Per-worker, in-process, explicit
// registration. A type with no registered factory is un-runnable by THIS worker (RunNext's type
// filter leaves it pending+visible in the queue rather than claim-fail-releasing it).
type Registry struct {
	mu        sync.RWMutex
	factories map[string]DAGFactory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]DAGFactory)}
}

// Register maps `typ` to `factory`. Returns an error on an empty type, a nil factory, or a duplicate
// registration (fail-loud — a silent overwrite would make dispatch depend on registration order).
func (r *Registry) Register(typ string, factory DAGFactory) error {
	if typ == "" {
		return fmt.Errorf("%w: Register requires a non-empty type", ErrValidation)
	}
	if factory == nil {
		return fmt.Errorf("%w: Register requires a non-nil factory for type %q", ErrValidation, typ)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[typ]; dup {
		return fmt.Errorf("%w: type %q is already registered", ErrValidation, typ)
	}
	r.factories[typ] = factory
	return nil
}

// lookup returns the factory for `typ` (ok=false if unregistered).
func (r *Registry) lookup(typ string) (DAGFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[typ]
	return f, ok
}

// Types returns the registered type names — the ClaimNext type filter (DEC-M17-TYPEFILTER), so a
// worker claims only types it can build. Order is unspecified.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for t := range r.factories {
		out = append(out, t)
	}
	return out
}

// defaultMaxAttempts bounds the pool retry loop when RunNext is called standalone (the Pool overrides it
// via WithMaxAttempts). 3 matches the M12 compensation-retry cadence — a small, bounded budget so a
// transient-fault workflow gets a few tries before dead-lettering, never an infinite re-claim.
const defaultMaxAttempts = 3

// RunNext claims the oldest claimable work item of a REGISTERED type for ownerID, rebuilds its DAG, seeds
// its input (on a fresh run), drives Execute on the SAME store instance, and terminalizes the queue row.
// Returns ran=false (no error) when there is no claimable work (ErrNoWork), ran=true otherwise (execErr
// carries an Execute failure; the row is retried-or-dead-lettered per the DF-4 policy — see runNext).
// Standalone RunNext uses defaultMaxAttempts; the Pool drives runNext with its WithMaxAttempts budget.
//
// SAME-INSTANCE (C3, the M16 tokenState bridge — reviewer m16-ph76-F1): the *Workflow drives Execute on
// the IDENTICAL *SQLiteStore ClaimNext used, so the fencing token ClaimNext set (in this store's
// in-memory tokenState) is the one Execute's checkpoint CAS reads — a superseded worker's write is
// fenced. A different store instance would silently un-fence the drive.
func RunNext(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID string) (ran bool, err error) {
	return runNext(ctx, store, reg, ownerID, defaultMaxAttempts)
}

// runNext is RunNext's body with an explicit maxAttempts retry budget (DF-4). Internal so RunNext's public
// signature stays frozen while the Pool injects its own budget.
func runNext(ctx context.Context, store *SQLiteStore, reg *Registry, ownerID string, maxAttempts int) (ran bool, err error) {
	types := reg.Types()
	if len(types) == 0 {
		// EMPTY REGISTRY (review ph81-F1): an empty type filter would make ClaimNext claim ANY pending
		// type (buildTypeFilter emits no `type IN (…)` clause on an empty slice) — the exact opposite of
		// D3 ("claim ONLY registered types"). A worker with nothing registered has no runnable work, so
		// short-circuit to ErrNoWork's outcome BEFORE claiming — never steal + strand a peer's item.
		return false, nil
	}
	item, err := store.ClaimNext(ownerID, types...) // claim ONLY registered types (D3)
	if err != nil {
		if errors.Is(err, ErrNoWork) {
			return false, nil // nothing claimable — the poller backs off (ph82).
		}
		return false, err
	}

	// POST-RECLAIM CANCEL RE-READ (M18 ph87, BLOCKER-2 defense-in-depth): ClaimNext's in-txn cancel-gate
	// terminalizes a LAPSED cancel-set row before offering it, but a cancel can land on a FRESH-pending
	// claim in the narrow window BETWEEN ClaimNext's scan and here. Re-check under this worker's own (just-
	// granted) token: if cancel was requested, terminalize `cancelled` NOW rather than running it. Fencing-
	// clean (we hold the current token → flipTerminalFenced lands). Cheap (one indexed read per claim).
	if cancelled, cerr := store.isCancelRequested(ctx, item.WorkflowID); cerr == nil && cancelled {
		_, _ = store.flipTerminalFenced(item.WorkflowID, wqCancelled) //nolint:errcheck // best-effort; token-guarded
		// A cancelled child is TERMINAL → wake its parent so the parent renders the outcome (INV-01)
		// and does not park forever (F-P94-02). deliverSubWorkflowCompletion re-reads the now-cancelled
		// state + fires only for a sub-workflow child (ParentID != "").
		deliverSubWorkflowCompletion(store, item)
		return true, nil // ran=true (we handled the item — terminalized cancelled, did not Execute)
	}

	factory, ok := reg.lookup(item.Type)
	if !ok {
		// Claimed a type with no factory. Given the len(types)>0 guard + the type filter, this is an
		// invariant violation (a registry mutated between Types() and lookup — a programmer error in the
		// single-worker model). TERMINALIZE the claimed row (review ph81-F1: do NOT leak it `claimed` —
		// every RunNext error path terminalizes so a live worker never strands a runnable item).
		_, _ = store.MarkFailed(item.WorkflowID) //nolint:errcheck // terminalize the un-runnable row
		return true, fmt.Errorf("%w: claimed type %q is not registered (registry mutated mid-claim?)", ErrValidation, item.Type)
	}

	dag, ferr := factory()
	if ferr != nil {
		_, _ = store.MarkFailed(item.WorkflowID) //nolint:errcheck // a broken factory is a terminal failure
		return true, fmt.Errorf("%w: factory for type %q failed: %w", ErrValidation, item.Type, ferr)
	}

	// Seed the input as KV BEFORE Execute — but ONLY on a truly-FRESH run (no existing journal). The
	// data-only pre-Save mechanism (grounded in executeLocked's fresh-run init: there is no external hook
	// to the internally-created WorkflowData, so the input must reach the run through the durable journal).
	// A data-only WorkflowData (KV, NO node statuses) passes checkGraphIdentity (it checks node statuses
	// only), so Execute Loads it and the first node reads the seeded values. Skipped when there is no input.
	//
	// CONDITIONAL-ON-FRESH is LOAD-BEARING (architect-pinned; review ph81-F3): Store.Save is a BLIND
	// OVERWRITE of the row-set. If we re-seeded UNCONDITIONALLY, a RE-CLAIM of an item whose journal already
	// has partial/complete progress (the first worker seeded + ran, then died) would WIPE that journal → the
	// re-claimer re-runs everything → LOST WORK / double-apply, breaking at-least-once-from-committed-frontier
	// AND the BLOCKER-1 seam's no-op property (a re-claimed COMPLETE journal must NOT be re-run). So we seed
	// ONLY when Load finds no prior journal (ErrNotFound); on a re-claim we skip the seed and Execute resumes
	// from the committed frontier. (ph82's reclaim-after-death rides exactly this gate.)
	if len(item.Input) > 0 {
		if _, lerr := store.Load(item.WorkflowID); errors.Is(lerr, ErrNotFound) {
			// FRESH run — no prior journal → safe to seed the input.
			seed := NewWorkflowData(item.WorkflowID)
			if serr := seedInput(seed, item.Input); serr != nil {
				_, _ = store.MarkFailed(item.WorkflowID) //nolint:errcheck // bad input is terminal
				return true, fmt.Errorf("%w: seed input for %q: %w", ErrValidation, item.WorkflowID, serr)
			}
			if sverr := store.Save(seed); sverr != nil {
				// A transient seed-Save failure (ErrBusy/ErrIO under MP contention) must TERMINALIZE the row,
				// not leak it `claimed` (review ph81-F2 — consistent with every other error path; retry is ph82).
				_, _ = store.MarkFailed(item.WorkflowID) //nolint:errcheck // terminalize on seed-Save failure
				return true, fmt.Errorf("seed save for %q: %w", item.WorkflowID, sverr)
			}
		} else if lerr != nil && !errors.Is(lerr, ErrNotFound) {
			// A non-not-found Load error (corrupt/IO) is a terminal failure — don't seed over a bad read.
			_, _ = store.MarkFailed(item.WorkflowID) //nolint:errcheck // terminalize on a corrupt/IO Load
			return true, fmt.Errorf("seed freshness-check load for %q: %w", item.WorkflowID, lerr)
		}
		// else: a prior journal EXISTS (re-claim) → do NOT re-seed; Execute resumes from the committed frontier.
	}

	// CANCEL DELIVERY (M18 ph87, DEC-M18-CANCEL-DELIVERY): a ctx-watcher observes an in-flight cancel and
	// cancels the Execute ctx, so a RUNNING workflow stops at its next level barrier (dag.go's existing
	// ctx.Err() guard — Execute/dag.go BYTE-UNCHANGED). We derive a cancellable child ctx, poll
	// cancel_requested every cancelPollInterval, and cancel() on observing SET. LEAK-FREE: the watcher
	// exits on EITHER the flag firing OR Execute returning (the `done` channel closed below) — no per-drive
	// goroutine leak in the Pool loop. Poll interval << lease TTL so cancel wins the reclaim race; the
	// watcher is dispatch-layer only (it calls the store flag-read + the ctx cancel, never into Execute/dag).
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go cancelWatcher(runCtx, cancel, done, store, item.WorkflowID)

	// M19 ph95 (F-P94-04): a queue child runs in THIS worker — a separate drive from its parent, so the
	// parent's Registry + drive-stack ctx did NOT cross the dispatch. Thread both back: w.Registry=reg so a
	// GRANDCHILD queue-spawn can resolve its type (else ErrSubWorkflowRequiresRegistry), and seed the drive
	// ctx to item.Depth so the child's own spawns see the accumulated nesting depth — a type-ref chain is
	// bounded by the ceiling across the dispatch, not reset to 0 at each worker. depth 0 (a plain M17
	// dispatch) seeds nothing → byte-unchanged.
	w := &Workflow{DAG: dag, WorkflowID: item.WorkflowID, Store: store, Registry: reg}
	execErr := w.Execute(withDepthSeed(runCtx, item.Depth))
	close(done) // Execute returned → tell the watcher to exit (leak-free).
	cancel()    // release the child ctx (idempotent; harmless if the watcher already cancelled).

	// A PARK is not a disposition (M19 ph94 — a SUSPENDABLE dispatched workflow, e.g. a queue
	// sub-workflow child with its own ph90 approval). ErrSuspended means the child durably parked
	// (Waiting), NOT a failure — dead-lettering it (the disposeExecErr default) would terminalize a
	// child that is legitimately awaiting a signal and can never resume. Leave the queue row + the
	// lease intact: the child's own signal (e.g. its approval) + a re-drive (RunNext/reclaim-after-
	// lease-lapse) resumes it from the committed frontier, exactly the M10 durable-suspend contract.
	// No completion signal is delivered (the child is NOT terminal — the parent stays parked until the
	// child actually terminalizes). Release the drive lease so the reclaim scan can re-offer the row
	// after the TTL lapses (a Released lease would fall out of the reclaim scan — leave it to lapse).
	if errors.Is(execErr, ErrSuspended) {
		// A park is genuine DURABLE PROGRESS (the child reached a checkpoint-persisted suspend point),
		// not a failed attempt — so RESET attempts to 0 (F-P94-01). Otherwise each park-reclaim cycle
		// bumps attempts (ClaimNext's flip), and a long-parked approval child would silently spend its
		// transient-infra retry budget → a coincident ErrBusy/ErrIO on its eventual terminalizing drive
		// would wrongly dead-letter it instead of retrying. The retry budget is for FAILURES; a park
		// (like a successful checkpoint) is not one, so it does not consume it. Best-effort.
		_, _ = store.resetWorkQueueAttempts(item.WorkflowID) //nolint:errcheck // best-effort; a failed reset only risks over-counting, never a lost child
		return true, nil                                     // ran=true (we drove it; it parked). The row stays claimed; TTL-lapse → reclaim → resume.
	}

	if execErr != nil {
		dispErr := disposeExecErr(store, item.WorkflowID, maxAttempts, execErr)
		// A sub-workflow child that reached a genuine TERMINAL disposition (done/failed/cancelled)
		// must wake its parent — including on FAILURE, so the parent renders the fail verdict and does
		// not park forever. A leave-claimed drain/reclaim does NOT fire (the child is not terminal; a
		// later reclaim re-runs it and fires then). Signal-after-terminal (F-92-04): the disposition is
		// durable before we deliver.
		deliverSubWorkflowCompletion(store, item)
		return true, dispErr
	}
	_, _ = store.MarkDone(item.WorkflowID) //nolint:errcheck // ph80 CAS flip claimed->done (idempotent)
	// The child is durably `done` → deliver the completion signal to the parent (if a sub-workflow).
	deliverSubWorkflowCompletion(store, item)
	return true, nil
}

// deliverSubWorkflowCompletion delivers a bare completion trigger to a sub-workflow child's PARENT
// mailbox (M19 ph94, DEC-P94-PARENT-ADDRESS-COLUMN + DEC-P92-SIGNAL-IS-TRIGGER). It fires ONLY when:
//   - the item carries a parent address (item.ParentID != "" — the TRUSTED engine-set column, never the
//     input BLOB → attacker input cannot forge the target); AND
//   - the child is genuinely TERMINAL (state ∈ {done, failed, cancelled}) — a leave-claimed drain/reclaim
//     does not fire (the child will re-run + fire on the reclaim). Signal-after-terminal ordering (F-92-04):
//     called AFTER the terminal disposition is durable, so the woken parent reads a COMPLETE child journal.
//
// The signal is a bare trigger (no payload — the parent reads the child DATA key + childRunFailed verdict,
// the uniform contract). The sig.ID is deterministic (f(childID)) so a re-run RunNext / re-delivered signal
// is idempotent (one mailbox entry). Best-effort: a delivery error does not fail the (already-terminal)
// child — the parent can be re-driven and will re-check the child journal; a lost signal degrades to a
// host re-drive, not a lost result. The parent's ph92 journal-gate is the validation: it completes ONLY for
// ITS OWN deterministic child ID, so a misrouted/spurious signal → the parent re-parks, never a false wake.
func deliverSubWorkflowCompletion(store *SQLiteStore, item WorkItem) {
	if item.ParentID == "" {
		return // a plain M17 dispatch (no parent) → no completion signal.
	}
	// Read the authoritative terminal state (the disposition may have left the row `claimed` on a
	// drain/reclaim — do NOT signal then).
	var state string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT state FROM work_queue WHERE workflow_id=?`, item.WorkflowID).Scan(&state); err != nil {
		return // best-effort: a read error → the parent can re-drive later.
	}
	if state != wqDone && state != wqFailed && state != wqCancelled {
		return // not terminal (leave-claimed) → a reclaim will re-run + fire.
	}
	sigID := "subworkflow-complete:" + item.WorkflowID                                 // deterministic → idempotent by sig.ID
	_ = store.DeliverSignal(item.ParentID, Signal{ID: sigID, Name: item.ParentSignal}) //nolint:errcheck // best-effort; a lost signal degrades to a parent re-drive
}

// cancelPollInterval is how often the ctx-watcher polls cancel_requested. 500ms: well under the default
// 30s lease TTL (so an operator cancel is observed + terminalized long before any reclaimer could take the
// row = cancel wins the reclaim race) and >> a single indexed SELECT (so the poll is negligible, off the
// hot Execute path). A long single node mid-level is NOT interrupted — cancel takes effect at the next
// level barrier + ≤ this interval (cooperative, rides dag.go's existing guard).
const cancelPollInterval = 500 * time.Millisecond

// cancelPollIntervalForTest is the poll interval the watcher actually uses; it defaults to
// cancelPollInterval and is overridden ONLY by tests (to avoid a 500ms wait). Production reads the const.
var cancelPollIntervalForTest = cancelPollInterval

// cancelWatcher polls cancel_requested for workflowID and cancels the run ctx on observing SET. It exits
// on `done` (Execute returned) OR runCtx cancellation OR the flag firing — leak-free. A read error is
// ignored (best-effort; the next poll retries; the store is authoritative on the terminal disposition).
func cancelWatcher(runCtx context.Context, cancel context.CancelFunc, done <-chan struct{}, store *SQLiteStore, workflowID string) {
	ticker := time.NewTicker(cancelPollIntervalForTest)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return // Execute finished — nothing to watch.
		case <-runCtx.Done():
			return // ctx already cancelled (by us or a parent) — done.
		case <-ticker.C:
			if cancelled, err := store.isCancelRequested(runCtx, workflowID); err == nil && cancelled {
				cancel() // operator asked to cancel → cancel the Execute ctx → it stops at the next level barrier.
				return
			}
		}
	}
}

// disposeExecErr applies the DF-4 retry policy to a failed Execute: RETRY a transient infra fault (bounded
// by maxAttempts) or DEAD-LETTER a poison/permanent failure. It always returns execErr (the caller surfaces
// the run's failure regardless of disposition — retry is a queue-state decision, not a suppressed error).
//
// Classification (fail-CLOSED — anything not provably transient is dead-lettered, so no error class can
// spin a hot re-claim loop):
//   - RETRYABLE = a transient INFRA fault: ErrBusy (write-lock contention) or ErrIO (a transient store
//     fault). MarkForRetry requeues claimed→pending IFF attempts<maxAttempts (the attempts bumped on the
//     next claim bounds the loop); if the budget is spent it returns requeued=false → we dead-letter.
//   - POISON = an *ExecutionError (a node's action logic failed). Re-running the same DAG on the same input
//     re-fails — retrying is pointless. Dead-letter immediately.
//   - DRIFT = ErrValidation from checkGraphIdentity (topology-drift, [red-team MAJOR-3]). NOT an
//     *ExecutionError, so it would fall through to the default; we dead-letter it EXPLICITLY (a drifted DAG
//     always re-drifts; under DEC-M17-VERSIONSKEW all live workers share the factory version). Attempt-
//     consuming: MarkFailed, never requeued. (If version-skew ever comes in-scope this flips to
//     release-and-retry-elsewhere — documented for the record.)
//   - DEFAULT = any UNCLASSIFIED error → dead-letter (fail-closed). Never route an unknown error to retry.
func disposeExecErr(store *SQLiteStore, workflowID string, maxAttempts int, execErr error) error {
	// SUPERSEDED (ErrFencedOut) TAKES ABSOLUTE PRECEDENCE — abort SILENTLY, touch NO queue state (review
	// ph82-F1). A fenced worker has LOST this workflow to a re-claimer B (D1's reclaim-broadening: A stalled
	// past TTL, B reclaimed under a bumped token, the row stays `claimed` under B). A's checkpoint returned
	// ErrFencedOut (the journal write was correctly rejected). If A now MarkFailed'd (the fail-closed default
	// below), it would flip B's LIVE `claimed` row to `failed` — and B's later MarkDone would be a 0-row
	// no-op, so a workflow B completed correctly is durably reported `failed`. A superseded worker MUST NOT
	// terminalize OR requeue: B owns the row and will terminalize it under its own token. (The token-guard on
	// the flips themselves — flipTerminalAndRelease / MarkForRetry, ph82-F1 structural — is the belt to this
	// suspenders: even a mis-routed terminal write can't clobber a higher-token owner's row.)
	if isSupersededError(execErr) {
		store.metrics.incSupersededAborts() // OBS-MET-02 (nil-safe: no-op when metrics unset)
		return execErr                      // lost the workflow to a re-claim → abort, leave the queue row to the live owner.
	}
	// GRACEFUL SHUTDOWN / DEADLINE is NOT a failure (review ph82-AF1, HIGH — data loss on the normal drain
	// path). Pool.Run's drain cancels the ctx handed to the in-flight runNext → when the PARENT ctx is
	// cancelled DAG.Execute returns a %w-wrapped bare context.Canceled (dag.go drops the *ExecutionError on
	// the parent-cancel path). Without this guard that cancel falls to the fail-closed MarkFailed below →
	// healthy in-flight work is dead-lettered and never re-scanned (C2) → the moat's "at-least-once
	// INVOCATION from the committed frontier" is violated on ordinary shutdown. Instead: leave the row
	// `claimed`, touch NO queue state, and — critically — do NOT Release the lease. A Released (deleted)
	// lease row would fall OUT of the reclaim scan (`state='claimed' AND EXISTS(leases.expiry < now)` needs
	// the row present) → the item strands `claimed` forever. Leaving the lease intact lets it lapse on TTL →
	// D1's reclaim-broadening discovers it → RunNext resumes from the committed frontier (F3 skips re-seed) —
	// the same C3-proven reclaim-after-death path. (A drain is indistinguishable from a crash at the queue
	// layer, by design — lapsed≠dead, DEC-M16-D3; the eventual reclaim consuming an attempt is the existing,
	// C4-verified reclaim semantics, not new here.)
	//
	// POISON TAKES PRECEDENCE (review ph82-AF1-TAIL-F1, HIGH — mirrors isRetryableInfra's errors.As-FIRST
	// discipline): the errors.As(*ExecutionError) gate is LOAD-BEARING. A node action that returns
	// context.Canceled/DeadlineExceeded from its OWN internal timeout while the PARENT ctx stays LIVE yields
	// an *ExecutionError (dag.go's cancel-drop only fires when the parent ctx.Err()!=nil), and
	// ExecutionError.Unwrap() exposes the node cause to errors.Is → a bare errors.Is here would TRUE-positive
	// on that poison and wrongly leave it `claimed` → an UNBOUNDED TTL-reclaim loop (the reclaim-of-claimed
	// path has no maxAttempts ceiling). So we treat cancel/deadline as shutdown ONLY when it is NOT wrapped
	// in an *ExecutionError (a genuine parent-ctx shutdown, dag.go returns a bare *fmt.wrapError). Node-logic
	// poison — even a node that failed via a deadline — stays poison → dead-lettered below.
	var xe *ExecutionError
	if !errors.As(execErr, &xe) &&
		(errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded)) {
		// DISPOSITION GATE (M18 ph87, BLOCKER-1): a bare parent-ctx cancel is EITHER an operator-cancel
		// (terminalize `cancelled`, never resume) OR an AF1 graceful-drain/crash (leave `claimed`, resume
		// later) — byte-identical context.Canceled here. The durable `cancel_requested` INTENT flag is what
		// splits them. Read it FIRST (ordered BEFORE the leave-claimed return): SET → the operator asked to
		// cancel → terminalize `cancelled` via the token-guarded flip (flipTerminalFenced — under the owner's
		// live token; a superseded worker's cancel-terminalize is a 0-row no-op, fencing-safe). UNSET → fall
		// through to the existing AF1 leave-claimed. (Reads the FLAG, not the error type — so a genuine
		// operator-cancel of a poison-flavored run still terminalizes cancelled, while an unset-flag poison
		// stays poison → dead-lettered below, since poison is caught by the errors.As gate above this block.)
		if cancelled, cerr := store.isCancelRequested(context.Background(), workflowID); cerr == nil && cancelled {
			_, _ = store.flipTerminalFenced(workflowID, wqCancelled) //nolint:errcheck // best-effort terminalize; a superseded flip is a fencing-safe 0-row no-op
			return execErr                                           // operator-cancel → terminal cancelled (NOT a dead-letter; a distinct terminal disposition)
		}
		return execErr // no cancel intent → parent shutdown/deadline (AF1) → leave claimed for TTL-reclaim; no Release.
	}
	if isRetryableInfra(execErr) {
		requeued, rerr := store.MarkForRetry(workflowID, maxAttempts)
		if rerr != nil {
			// The requeue itself faulted (store error). Best-effort dead-letter so the row can't strand
			// `claimed`; surface the ORIGINAL execErr (the run's real failure), not the requeue fault.
			_, _ = store.MarkFailed(workflowID) //nolint:errcheck // terminalize; requeue faulted
			store.metrics.incDeadLetters()      // review ph86-F2: this IS a dead-letter (terminal MarkFailed) — count it (nil-safe)
			return execErr
		}
		if requeued {
			store.metrics.incRetriesAttempted() // OBS-MET-03 retry event (nil-safe)
			return execErr                      // back to pending under budget — a sibling (or this worker) re-claims + retries.
		}
		// budget exhausted → fall through to dead-letter.
	}
	_, _ = store.MarkFailed(workflowID) //nolint:errcheck // poison / drift / unclassified / budget-spent
	store.metrics.incDeadLetters()      // OBS-MET-03 dead-letter event (nil-safe)
	return execErr
}

// isRetryableInfra reports whether an Execute failure is a TRANSIENT INFRASTRUCTURE fault worth retrying
// (ErrBusy or ErrIO) — as opposed to a poison logic failure (*ExecutionError), a topology drift
// (ErrValidation), or an unknown error. Fail-closed: only ErrBusy/ErrIO are retryable.
//
// POISON TAKES PRECEDENCE (load-bearing): an *ExecutionError is checked FIRST and is NEVER retryable, even
// if a failed node's cause wraps ErrIO/ErrBusy. ExecutionError.Unwrap() exposes per-node causes to
// errors.Is, so a node failing with an IO-flavored cause would otherwise LOOK retryable — but a node's
// action failing is POISON (re-running the same DAG on the same input re-fails). Pool-level retry is for
// infra faults OUTSIDE node logic (the checkpoint store write, the claim txn), which surface as a BARE
// ErrBusy/ErrIO, not wrapped in an *ExecutionError.
func isRetryableInfra(err error) bool {
	var execErr *ExecutionError
	if errors.As(err, &execErr) {
		return false // poison — a node's logic failed; do not retry regardless of the node's cause.
	}
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrIO)
}

// seedInput decodes item.Input (a JSON object) into KV Sets on data. A non-object / malformed payload
// is an error (a terminal failure, surfaced by RunNext), never a panic — the input is untrusted data.
//
// FIDELITY (review ph81-F4): the node reads the CODEC-RELOADED value (Execute Loads the seed through the
// store's typed columns), NOT the in-memory decode. So the round trip is SCALAR-faithful only: a JSON
// string→string, number→float64, bool→bool. A nested object/array → a JSON STRING (the store's kvString
// fallback), and JSON null → the string "null". A caller passing structured input should expect the
// node to Get a JSON string / a float64, not a map/slice/int — flatten to scalars or decode the string.
func seedInput(data *WorkflowData, input []byte) error {
	var kv map[string]interface{}
	if err := json.Unmarshal(input, &kv); err != nil {
		return fmt.Errorf("%w: input is not a JSON object: %w", ErrValidation, err)
	}
	for k, v := range kv {
		data.Set(k, v)
	}
	return nil
}
