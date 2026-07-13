package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow/metrics"
)

// Workflow represents a workflow execution.
// It combines a DAG with execution context like ID and persistence.
type Workflow struct {
	// DAG is the directed acyclic graph representing the workflow structure
	DAG *DAG

	// WorkflowID uniquely identifies this workflow
	WorkflowID string

	// Store is used for persisting workflow state
	Store WorkflowStore

	// Clock is the source of "now" for durable timers (M10). When nil it defaults
	// to the system clock; tests inject a FakeClock to drive durable-time
	// scenarios deterministically and instantly. Execute injects this clock into
	// the context so TimerNode actions read time through it (never time.Now()
	// directly), and Tick overrides it per-invocation with the host-supplied now.
	Clock Clock

	// Locker serializes concurrent drives of this WorkflowID within the process
	// (M10 ph37, D37-07b). When nil it defaults to a process-wide in-process
	// per-WorkflowID lease, so two concurrent Execute/Tick/DeliverAndResume calls
	// for the same WorkflowID take turns (an event handler + a poller + a startup
	// re-arm cannot interleave a load→run→save). Set a shared Locker via WithLocker
	// to coordinate across *Workflow instances, or a future cross-process Locker
	// (SQLite claimed_at) for multi-process serialization.
	Locker Locker

	// RollbackTimeout bounds a saga rollback (M12 ph47, DEC-M12-P47-DEADLINE). When
	// zero it defaults to DefaultRollbackTimeout; a negative value makes the rollback
	// explicitly unbounded. The whole reverse compensation pass shares this deadline;
	// a compensation still blocked past it is recorded CompensationFailed so a hung
	// compensation never hangs the run. Set via WithRollbackTimeout.
	RollbackTimeout time.Duration

	// MetricsConfig opts this workflow's internally-built WorkflowData into metrics
	// collection (M14 ph60 / REM-02). When nil (the default) metrics stay disabled
	// — the frozen zero-tax hot path is unchanged. When set (e.g.
	// metrics.ProductionConfig() or metrics.NewConfig().WithEnabled(true)), the
	// WorkflowData created by Execute collects operation stats, readable after the
	// run via GetMetrics() and exportable through metrics.OTelBridge. Set via
	// WithMetrics on the builder or directly on the struct.
	MetricsConfig *metrics.Config

	// metrics retains the last run's collector so a caller reaching only the
	// Execute(ctx) API can read stats back via GetMetrics() after Execute. Written
	// once per drive at data construction; a nil MetricsConfig leaves it holding a
	// disabled collector (GetMetrics still returns non-nil, stats simply zero).
	metrics *metrics.MetricsCollector
}

// NewWorkflow creates a new workflow with the given workflow store
func NewWorkflow(store WorkflowStore) *Workflow {
	return &Workflow{
		DAG:        NewDAG("workflow"),
		WorkflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
		Store:      store,
	}
}

// AddNode adds a node to the workflow.
// Returns an error if a node with the same name already exists.
func (w *Workflow) AddNode(node *Node) error {
	return w.DAG.AddNode(node)
}

// AddDependency adds a dependency between nodes.
// Returns an error if either node doesn't exist or if adding the dependency would create a cycle.
func (w *Workflow) AddDependency(from, to string) error {
	return w.DAG.AddDependency(from, to)
}

// WithWorkflowID sets the workflow ID.
// Returns the workflow for method chaining.
func (w *Workflow) WithWorkflowID(id string) *Workflow {
	w.WorkflowID = id
	return w
}

// WithClock sets the clock used for durable timers (M10). Passing nil restores
// the default system clock. Tests inject a FakeClock to drive durable-time
// scenarios deterministically. Returns the workflow for method chaining.
func (w *Workflow) WithClock(c Clock) *Workflow {
	w.Clock = c
	return w
}

// WithLocker sets the per-WorkflowID drive lease (M10 ph37). Passing nil restores
// the process-wide default in-process locker. Share one Locker across *Workflow
// instances to serialize same-ID drives that span instances. Returns the workflow
// for method chaining.
func (w *Workflow) WithLocker(l Locker) *Workflow {
	w.Locker = l
	return w
}

// WithRollbackTimeout sets the scoped deadline for a saga rollback (M12 ph47). Zero
// restores DefaultRollbackTimeout; a negative duration makes the rollback explicitly
// unbounded. Returns the workflow for method chaining.
func (w *Workflow) WithRollbackTimeout(d time.Duration) *Workflow {
	w.RollbackTimeout = d
	return w
}

// finishRollback drives the compensation pass, persists the compensated state, and
// composes the honest outcome (M12 ph47/48). The rollback pass checkpoints after each
// reverse level (§1), so on a resume it re-runs only the still-Completed compensable
// nodes. The final partition is RECONSTRUCTED from DURABLE statuses (§2) — not just the
// nodes this drive processed — so a resumed rollback reports the whole run's honest
// partition and is NEVER nil (resolves ph47-F5). `cause` is the trigger failure on the
// fail path, or nil on a resume-into-rollback (then reconstructed from persisted Failed
// nodes). When >=1 compensation failed it returns a *SagaError enumerating the exact
// {compensated, failed, skipped} partition wrapping the cause (errors.As reaches both);
// otherwise it returns the (reconstructed) cause — never a false *SagaError, and never
// nil for a rolled-back run. A re-drive of a fully-rolled-back run is a no-op returning
// the reconstructed outcome (§3 rollback-complete = no Completed compensable remains).
func (w *Workflow) finishRollback(data *WorkflowData, cause error) error {
	fresh := w.driveRollback(data)

	// Final authoritative Save (also the ph47-F2 partition-carrying persist). The final
	// Save persists the whole `data`, so a clean final Save subsumes any per-level
	// checkpoint error (fresh.saveErr) — a transient mid-pass checkpoint failure on a
	// fully-persisted run is NOT surfaced (review ph48-F3). Only when the final Save
	// ITSELF fails does the persist genuinely fail.
	var saveErr error
	if w.Store != nil {
		saveErr = w.Store.Save(data)
	}

	// §2 reconstruct the full partition + the forward cause from durable statuses.
	out := w.reconstructOutcome(data, fresh)
	effectiveCause := cause
	if effectiveCause == nil {
		effectiveCause = reconstructCause(data, w.DAG) // resume path: cause was a prior run
	}
	// Never-nil floor (review ph48-F1): a rolled-back run whose trigger cause is not
	// reconstructable (a caller-cancel/deadline leaves no Failed node, and the trigger
	// cause is not journaled) still MUST NOT report success. finishRollback is only ever
	// reached for a rolled-back run, so a nil cause here means "rolled back, cause not
	// journaled" — surface the sentinel, never nil. (Faithful cancel-vs-fail cause
	// recovery across a crash needs a journaled trigger cause — ph48-F2, routed UP.)
	if effectiveCause == nil {
		effectiveCause = ErrRolledBack
	}

	// A partial rollback ALWAYS surfaces as a *SagaError so the operator-critical
	// partition survives — even when a persist fails (ph47-F2): the save error is
	// folded into Cause rather than replacing the partition.
	if len(out.failedToCompensate) > 0 {
		if saveErr != nil {
			effectiveCause = errors.Join(effectiveCause, fmt.Errorf("failed to save rollback state: %w", saveErr))
		}
		return &SagaError{
			Cause:              effectiveCause,
			Compensated:        out.compensated,
			FailedToCompensate: out.failedToCompensate,
			Skipped:            out.skipped,
		}
	}

	// Clean rollback (no compensation failed): mark the return so a caller can detect
	// "this run ROLLED BACK" from the error alone (REM-03 / DEC-M13-V1 Option C).
	// Wrap the reconstructed cause as `ErrRolledBack: cause` (Go 1.20 multi-%w) so
	// BOTH errors.Is(err, ErrRolledBack) AND errors.As(err, &ExecutionError) reach
	// through — a clean rollback with a real cause is no longer indistinguishable
	// from a plain failure. When the cause already IS ErrRolledBack (the un-journaled
	// path @ :155) there is nothing to wrap — it already satisfies Is. The never-nil
	// floor holds (effectiveCause is never nil here). The partial-rollback SagaError
	// path above is UNCHANGED (it carries its own Cause + partition).
	rolledBack := effectiveCause
	if !errors.Is(rolledBack, ErrRolledBack) {
		rolledBack = fmt.Errorf("%w: %w", ErrRolledBack, effectiveCause)
	}

	// A persist failure on the clean path still surfaces, folded into the rolled-back
	// error so Is(ErrRolledBack) AND the save error both remain reachable.
	if saveErr != nil {
		return fmt.Errorf("%w (additionally, failed to save rollback state: %w)", rolledBack, saveErr)
	}
	return rolledBack
}

// locker resolves the drive lease: the workflow's own Locker, or the process-wide
// default in-process locker when unset.
func (w *Workflow) locker() Locker {
	if w.Locker != nil {
		return w.Locker
	}
	return defaultLocker
}

// Execute runs the workflow.
// It loads any existing state, executes the DAG, and persists the final state.
// Returns an error if execution fails.
func (w *Workflow) Execute(ctx context.Context) error {
	// Single-writer lease (M10 ph37, D37-07b): serialize concurrent drives of this
	// WorkflowID for the WHOLE load→run→checkpoint→save→ack span. The lease is
	// acquired at every PUBLIC drive entry (Execute, Tick, DeliverAndResume), each
	// of which then delegates to the unexported, lease-free executeLocked — ONE
	// acquisition per drive, with NO reentrancy assumption (no public entry calls
	// another, so a non-reentrant mutex never double-acquires). Held until the drive
	// returns. (Discharges OBL-M10-P37-LEASE-F1.)
	release, err := w.locker().Acquire(ctx, w.WorkflowID)
	if err != nil {
		return err
	}
	defer release()
	return w.executeLocked(ctx)
}

// disabledMetricsSentinel is the non-nil, disabled collector GetMetrics returns
// when this workflow never enabled metrics (M14 ph61 F2). It is package-level and
// effectively immutable (a disabled collector's TrackOperation is a no-op and its
// stats read zero, so a caller cannot meaningfully mutate it), so sharing it is
// race-free and preserves GetMetrics's documented "non-nil, disabled, stats read
// zero" contract without the per-Execute field write that would race under
// lease-less concurrent Execute.
var disabledMetricsSentinel = metrics.NewMetricsCollectorWithConfig(
	metrics.NewConfig().WithEnabled(false).GetInternalConfig(),
)

// GetMetrics returns the metrics collector of the most recent Execute drive
// (REM-02). When MetricsConfig was set the collector holds real operation stats
// (readable via its GetAllOperationStats / exportable through metrics.OTelBridge).
// When MetricsConfig was nil (the default), it returns a NON-NIL disabled collector
// whose stats read zero — never nil — so a caller can always dereference it. Reading
// it after a run lets an Execute(ctx)-only consumer reach observability without
// touching the data layer.
//
// Call GetMetrics AFTER Execute returns. On the ENABLED path it reads a field
// written during the drive without synchronization, so calling it concurrently with
// an in-flight Execute on the same *Workflow is a data race; the intended contract
// is read-after-run. (The disabled default writes no field — race-free.)
func (w *Workflow) GetMetrics() *metrics.MetricsCollector {
	if w.metrics == nil {
		return disabledMetricsSentinel
	}
	return w.metrics
}

// executeLocked is the drive body — load → run the DAG → checkpoint/save → ack —
// WITHOUT acquiring the lease. Callers (Execute, Tick, DeliverAndResume) hold the
// per-WorkflowID lease for its duration. It is the single funnel so the lease is
// acquired exactly once per drive (no reentrancy).
func (w *Workflow) executeLocked(ctx context.Context) error {
	// Inject the durable-timer clock so TimerNode actions read "now" through it
	// (never time.Now() directly — the no-determinism-tax discipline, D36-07). A
	// clock already present in ctx (Tick pins one to the host-supplied now) is
	// left intact; otherwise the workflow's Clock is used, defaulting to the
	// system clock. This is the single injection point for the whole run.
	if _, ok := ctx.Value(clockCtxKey{}).(Clock); !ok {
		clock := w.Clock
		if clock == nil {
			clock = systemClock{}
		}
		ctx = withClock(ctx, clock)
	}

	// Create workflow data. When MetricsConfig is set (REM-02 enable-hook), thread
	// it into the internally-built WorkflowData so an Execute(ctx)-only consumer can
	// enable metrics without reaching the data layer directly; retain the collector
	// on the Workflow so GetMetrics() reads real stats back after the run. A nil
	// MetricsConfig keeps the frozen disabled default (zero determinism tax).
	var data *WorkflowData
	if w.MetricsConfig != nil {
		data = NewWorkflowDataWithConfig(w.WorkflowID, DefaultWorkflowDataConfig().WithMetricsConfig(w.MetricsConfig))
	} else {
		data = NewWorkflowData(w.WorkflowID)
	}

	// Load existing state if available.
	//
	// A missing workflow (ErrNotFound) is the expected "no prior state" case —
	// resume simply starts fresh with the newly created data. Any OTHER error
	// (e.g. ErrCorruptData from a malformed/truncated persisted payload, or an
	// I/O failure) must NOT be swallowed: silently discarding it would start
	// fresh and overwrite the persisted state on the next Save, losing it.
	// Propagate it so a corrupt resume surfaces instead of masquerading as a
	// clean first run.
	if w.Store != nil {
		existingData, err := w.Store.Load(w.WorkflowID)
		switch {
		case err == nil:
			if existingData != nil {
				data = existingData
				// Graph-identity guard (M9 crash-resume): the persisted state was
				// produced by SOME DAG under this WorkflowID; on resume it must be
				// consistent with the CURRENT DAG, or we would rehydrate node
				// statuses/outputs that no longer correspond to the graph and
				// silently mis-resume. Validate that every persisted node name
				// still exists in this DAG; reject loudly on a mismatch rather than
				// guessing. This is the tractable, node-identity analog of
				// Temporal's workflow-versioning problem (we check graph identity,
				// not code shape). (DEC-M9, chunk 2.)
				if err := w.checkGraphIdentity(data); err != nil {
					return err
				}
			}
		case errors.Is(err, ErrNotFound):
			// No prior state — start fresh with the new data.
		default:
			return fmt.Errorf("failed to load workflow state: %w", err)
		}
	}

	// Retain the collector of the data actually driven this run (fresh OR loaded)
	// so GetMetrics() reads it back after Execute (REM-02). On the InMemoryStore
	// resume path the loaded data's collector is the Clone()-preserved one, so the
	// enabled STATE survives the checkpoint (the N1 invariant — the resumed run's
	// tracking is not silently skipped; stats are per-drive, not cumulative, since
	// Clone resets the counters). FB/JSON do not persist metrics, so a loaded run
	// there starts a fresh collector matching the loaded data's config.
	//
	// Guarded on MetricsConfig != nil: the retention is only meaningful when metrics
	// are enabled, and skipping the field write on the disabled default means two
	// deliberately-lease-less concurrent Execute (the adversarial double-apply tests)
	// do not write-race on w.metrics — the field is untouched unless the caller opted
	// into metrics (in which case GetMetrics-after-Execute is the documented,
	// non-concurrent contract). (M14 ph61: closes the ph60 F1 race on the default path.)
	if w.MetricsConfig != nil {
		w.metrics = data.GetMetrics()
	}

	// Validate DAG
	if err := w.DAG.Validate(); err != nil {
		return fmt.Errorf("workflow validation failed: %w", err)
	}

	// M12 saga forward-vs-rollback switch. A run persisted mid-rollback (its
	// rolling_back marker set by the trigger below) resumes into the ROLLBACK drive
	// instead of the forward DAG.Execute — the durable re-entry seam. In ph46/47 this
	// branch is exercised WITHOUT a crash (a direct "load a rolling_back snapshot ->
	// resume compensates, does NOT re-run forward" test); ph48 adds the crash points
	// that make a real resume land here. finishRollback runs the compensation pass,
	// persists the compensated state, and returns a *SagaError if any compensation
	// failed (nil cause — the original trigger failure was a prior run). Placed before
	// the forward-only checkpointer/signal wiring (a rollback neither per-level
	// checkpoints nor consumes signals).
	if data.IsRollingBack() {
		return w.finishRollback(data, nil)
	}

	// Wire durable mid-run checkpointing when the Store supports it (M9). A Store
	// that implements Checkpointer gets a per-level checkpoint flush inside
	// DAG.Execute; a Store that does not is unaffected (no callback injected →
	// zero overhead, save-at-boundaries only). (DEC-M9, chunk 2.)
	//
	// M10-P37 T1 (MH37-5a): the callback is carried on the per-Execute ctx, NOT
	// written to the shared w.DAG.config field. This makes two concurrent Execute
	// on one *Workflow memory-safe — each call has its own ctx-scoped callback, so
	// there is no shared-field write to race and no `defer …=nil` that one run
	// could use to nil out another run's callback. (DEC-M10-P37-LEASE(a).)
	//
	// M15 ph69: when the Store ALSO implements IncrementalCheckpointer, drive the fast
	// O(N) path — turn on per-Execute delta capture (the mutators then record touched keys
	// O(1); zero-alloc + no-op when off, so non-incremental runs and the hot path are
	// unaffected — det-tax stays exact), and the per-level callback drains the changed-set
	// and calls SaveDeltaCheckpoint. A level whose drain is inactive/first-warm falls back
	// to the full SaveCheckpoint (byte-identical), so correctness never rides the fast path
	// alone. Capture is disarmed when this drive returns. Forward-drive only: M12 rollback
	// (handled above via finishRollback→Save) never reaches here.
	if inc, ok := w.Store.(IncrementalCheckpointer); ok {
		data.beginDeltaCapture()
		defer data.endDeltaCapture()
		cp, isCp := w.Store.(Checkpointer) // an IncrementalCheckpointer is expected to also be a Checkpointer (the fallback)
		ctx = withCheckpoint(ctx, func(d *WorkflowData) error {
			changed, active := d.drainDeltaCapture()
			if active {
				return inc.SaveDeltaCheckpoint(changed, d)
			}
			if isCp {
				return cp.SaveCheckpoint(d)
			}
			return nil
		})
	} else if cp, ok := w.Store.(Checkpointer); ok {
		ctx = withCheckpoint(ctx, func(d *WorkflowData) error {
			return cp.SaveCheckpoint(d)
		})
	}

	// M14 ph61: inject the durability-floor callback when the Store is a Syncer
	// (group-commit). The park forces it so a suspended run is fsync-durable even
	// under Batched(K). A NON-Syncer store injects nothing (syncFrom → nil). A Strict
	// FlatBuffersStore IS a Syncer, so the callback IS injected — but its Sync()
	// no-ops (nothing is ever deferred under Strict), so the floor is preserved at
	// negligible cost. (Completion goes through Save, which is always fsync-durable.)
	if sy, ok := w.Store.(Syncer); ok {
		wfID := w.WorkflowID
		ctx = withSync(ctx, func() error { return sy.Sync(wfID) })
	}

	// Wire the durable signal mailbox when the Store supports it (M10 ph37). The
	// SignalStore is injected on ctx so a WaitForSignalNode can take its mailbox; a
	// fresh consumed-signals collector gathers the sig.IDs the run consumes so they
	// can be acked AFTER the consuming completion is durable — the
	// take→apply→Completed→checkpoint→ack ordering that IS the correctness core
	// (D37-04). A non-SignalStore Store injects neither (a WaitForSignalNode then
	// fails loudly with ErrWaitRequiresSignalStore rather than parking forever).
	var consumed *consumedSignals
	var signals SignalStore
	if ss, ok := w.Store.(SignalStore); ok {
		signals = ss
		consumed = &consumedSignals{}
		ctx = withSignalStore(ctx, ss)
		ctx = withConsumedSignals(ctx, consumed)
	}

	// ackConsumed drains the consumed collector and removes those signals from the
	// mailbox. Called ONLY after the consuming completion is durable (after the
	// final Save on success; after the executor's barrier checkpoint flush on
	// suspend). It is BEST-EFFORT: a failed ack leaves an INERT unacked signal —
	// the node is already Completed and durable, so it never re-consumes — so an ack
	// failure must never fail the run (D37-04). The stray entry is reclaimed by
	// Store.Delete(workflowID) (it removes the whole <id>.signals/ mailbox); there
	// is no background GC (ph37 F2).
	ackConsumed := func() {
		if signals == nil || consumed == nil {
			return
		}
		ids := consumed.drain()
		if len(ids) == 0 {
			return
		}
		//nolint:errcheck,gosec // best-effort drain (D37-04): a failed ack leaves a harmless unacked signal
		signals.AckSignals(w.WorkflowID, ids)
	}

	// Execute DAG
	if err := w.DAG.Execute(ctx, data); err != nil {
		// A park is a SUCCESS arm, not a failure: the executor has already
		// durably flushed the checkpoint at the level barrier before returning
		// ErrSuspended (MH-3, durable-flush-before-suspend), so there is no
		// "failed state" to save and the sentinel must reach the caller intact
		// for errors.Is(err, ErrSuspended). Short-circuit before the failure
		// save-and-wrap path. (M10 suspend-arm.)
		if errors.Is(err, ErrSuspended) {
			// Suspend arm: the executor already flushed the barrier checkpoint
			// (D-10), so any signal consumed this run has its Completed status
			// durable — ack now (after durable), then surface the sentinel intact.
			ackConsumed()
			return err
		}

		// M12 saga trigger (§4, red-team MAJOR-1). Rollback fires ONLY on a HARD
		// node failure — a *ExecutionError from fail-fast — OR a caller-cancel
		// (context.Canceled/DeadlineExceeded; a mid-level cancel surfaces a ctx
		// error, not an *ExecutionError — DEC-M12-TRIGGER). It deliberately does NOT
		// fire on: ErrSuspended (handled above — the run intends to resume); a
		// coe-only run (DAG.Execute returns nil, so we never reach this err block); a
		// checkpoint-flush error (a Save/IO error, not an *ExecutionError, so
		// errors.As misses it); a corrupt/IO load error or a validation error (both
		// return BEFORE DAG.Execute). "Execute returned non-nil" is NOT the trigger.
		// hasCompensations gate: a NON-saga DAG (no compensation declared anywhere)
		// takes exactly the pre-M12 failed-state path below — byte-for-byte, no
		// rolling_back marker, a single save (the moat: the machinery is inert unless
		// the workflow actually declares a saga).
		var execErr *ExecutionError
		triggersRollback := errors.As(err, &execErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if triggersRollback && w.hasCompensations() {
			// Set + PERSIST the rolling_back marker BEFORE compensating, so a crash
			// between here and the rollback resumes into the rollback drive (ph48),
			// never re-runs forward. Then run the compensation pass and persist the
			// compensated state. The run still FAILED: return the original error
			// (ph47: finishRollback returns a typed *SagaError if any compensation
			// failed, else the original err — never a false-clean).
			data.SetRollingBack(true)
			// M12 ph49: journal WHY we are rolling back, in the SAME snapshot as the
			// marker (resolves ph48-F2). err is already classified above; a *ExecutionError
			// is a node failure, else a ctx Canceled/DeadlineExceeded. On resume,
			// reconstructCause reads this to return the TRUE cause (a cancel as a cancel),
			// not a node-failure inferred from an incidental Failed node.
			switch {
			case execErr != nil:
				data.SetTriggerCause(TriggerFailure)
			case errors.Is(err, context.Canceled):
				data.SetTriggerCause(TriggerCanceled)
			case errors.Is(err, context.DeadlineExceeded):
				data.SetTriggerCause(TriggerDeadlineExceeded)
			}
			if w.Store != nil {
				if saveErr := w.Store.Save(data); saveErr != nil {
					return fmt.Errorf("%w (additionally, failed to persist rollback marker: %w)", err, saveErr)
				}
			}
			// finishRollback drives best-effort compensation, persists the compensated
			// state, and composes the honest outcome: a *SagaError enumerating the exact
			// {compensated, failed, skipped} partition when >=1 compensation failed, or
			// the original err (`cause`) on a fully-clean rollback.
			return w.finishRollback(data, err)
		}

		// Non-triggering failure (e.g. a checkpoint-flush error): save the failed
		// state, unchanged from the pre-M12 path. Do NOT ack here: the run failed, so
		// a consumed signal stays INERT in the mailbox (the node is Completed in the
		// saved state; a retry skips it — no re-consume, no double-apply). It is
		// reclaimed by Store.Delete (no background GC; ph37 F2).
		if w.Store != nil {
			if saveErr := w.Store.Save(data); saveErr != nil {
				return fmt.Errorf("%w (additionally, failed to save state: %w)", err, saveErr)
			}
		}
		return err
	}

	// Save final state
	if w.Store != nil {
		if err := w.Store.Save(data); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	// Ack consumed signals AFTER the final durable Save (take→apply→Completed→
	// checkpoint→ack, D37-04).
	ackConsumed()

	return nil
}

// checkGraphIdentity verifies that the persisted state being resumed is
// consistent with the current DAG: every node name carrying a persisted status
// must still exist in this DAG. A persisted status for a node the DAG no longer
// has means the graph changed between the original run and this resume, so the
// rehydrated statuses/outputs cannot be trusted — we return an error instead of
// silently mis-resuming. (DEC-M9, chunk 2; the node-identity analog of workflow
// versioning.) Only node IDENTITY is checked, not action/code shape.
func (w *Workflow) checkGraphIdentity(data *WorkflowData) error {
	var unknown []string
	data.ForEachNodeStatus(func(nodeName string, _ NodeStatus) {
		if _, exists := w.DAG.GetNode(nodeName); !exists {
			unknown = append(unknown, nodeName)
		}
	})
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%w: cannot resume workflow %q: persisted state references node(s) %v not present in the current DAG (the graph changed since the checkpoint)",
			ErrValidation, w.WorkflowID, unknown)
	}
	return nil
}

// FromBuilder creates a workflow from a builder.
// Returns an error if the workflow definition is invalid.
func FromBuilder(builder *WorkflowBuilder) (*Workflow, error) {
	// Use the guard-free build(): FromBuilder carries builder.store forward onto the
	// *Workflow below, so the store is NOT lost here — the public Build()'s
	// store-set guard (REM-04) would be wrong on this path. (M14 ph62.)
	dag, err := builder.build()
	if err != nil {
		return nil, err
	}

	return &Workflow{
		DAG:        dag,
		WorkflowID: builder.workflowID,
		Store:      builder.store,
		Clock:      builder.clock,
	}, nil
}
