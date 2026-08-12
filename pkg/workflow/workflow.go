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
	// dag is the directed acyclic graph representing the workflow structure. SEALED by
	// M23 SEAL-06 (BYPASS-10); read it with the DAG() accessor.
	//
	// UNEXPORTING THIS DID NOT CLOSE BYPASS-10, AND NOBODY SHOULD READ IT THAT WAY.
	// workflow_dispatch.go is IN THIS PACKAGE and still fills this field from a
	// consumer-supplied DAGFactory result (the `&Workflow{dag: dag, …}` literal in
	// runNext) — the rename is invisible to it. What
	// actually refuses an unvalidated graph on the M17 dispatch path is the BUILDER
	// TOKEN checked at drive time; the seal only stops an out-of-package caller from
	// assigning the field, which is a smaller and different property.
	//
	// The accessor alone would not have sufficed either: there are in-package WRITES to
	// this field, so exposing a read-only DAG() while leaving the field exported would
	// have left the write path open and merely looked closed.
	dag *DAG

	// WorkflowID uniquely identifies this workflow
	WorkflowID string

	// Store is used for persisting workflow state
	Store WorkflowStore

	// Registry maps a workflow TYPE (a data string) to its DAGFactory (M17). A queue-dispatched
	// sub-workflow node (M19 ph94, AddSubWorkflowQueued) reads it off ctx to resolve the child type
	// → DAG (for the coe-verdict on wake) + validate the type exists at enqueue. Nil when the
	// workflow uses no queue sub-workflows; a queue node reached with a nil Registry returns
	// ErrSubWorkflowRequiresRegistry. It is the EXECUTION-ENVIRONMENT's Registry (carries CODE),
	// injected at Execute — the DAG references only the type STRING (keeping the workflow pure DATA),
	// exactly as RunNext takes the Registry as a parameter, not baked into the workflow.
	//
	// SEALED by M23 SEAL-06. The census had this filed among the sanctioned config knobs;
	// the architect ruled it INTO the seal (R-03) and the doc comment above is the
	// argument: it CARRIES CODE and is read at execute time to resolve a child type to a
	// DAG. That makes it the same class as dag — a consumer re-points what code runs —
	// rather than a value knob like Clock or RollbackTimeout. Read it with Registry().
	registry *Registry

	// MaxSubWorkflowDepth bounds sub-workflow nesting (M19 ph95, the COMP-CLOSE DoS ceiling).
	// A sub-workflow spawn reached at nesting depth d >= this ceiling is refused with
	// ErrSubWorkflowMaxDepth (loud, never a park, never a silent cap). Zero => the package
	// default (defaultMaxSubWorkflowDepth = 8). Injected on ctx at executeLocked.
	//
	// SCOPE (F-P95-02): this override governs the INLINE path. On the QUEUE path a child runs in a
	// SEPARATE RunNext drive that does not carry this field — only the depth COUNT crosses the
	// dispatch — so a queue child enforces the package DEFAULT ceiling regardless of this override.
	// The unbounded-nesting DoS invariant holds on BOTH paths (the queue path is always bounded at
	// the default); only a non-default override is inline-only. Change the package default if you
	// need a uniform queue-path bound.
	MaxSubWorkflowDepth int

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
	// run via GetMetrics() and exportable through metrics.OTelBridge. Set this field
	// DIRECTLY on the *Workflow before Execute — there is no builder setter (AUD-042:
	// an earlier comment named a WithMetrics builder method that does not exist).
	MetricsConfig *metrics.Config

	// metrics retains the last run's collector so a caller reaching only the
	// Execute(ctx) API can read stats back via GetMetrics() after Execute. Written
	// once per drive at data construction; a nil MetricsConfig leaves it holding a
	// disabled collector (GetMetrics still returns non-nil, stats simply zero).
	metrics *metrics.MetricsCollector

	// migration, when set (via WithDefinitionMigration, AUD-070), is consulted on a
	// definition-digest mismatch on resume instead of the default hard reject: the
	// handler may accept the changed graph, transform the loaded state, or reject.
	migration DefinitionMigration
}

// ErrWorkflowNoDAG is returned when a *Workflow is driven with no graph at all.
// M23 SEAL-06 — but it guards the PARTIAL SEAL, not the token.
//
// T6 seals Workflow's graph handle; the seven config knobs beside it stay exported by
// deliberate disposition, so `&workflow.Workflow{WorkflowID: "x", Store: s}` remains
// legal from outside the package and yields a Workflow with a nil graph. Before this
// guard EVERY public drive entry panicked on that value with a nil pointer
// dereference — measured on all four, not argued. Sealing the graph handle without
// this would have removed the WORKING idiom and left the BROKEN one legal, which is
// the inverse of what the phase claims to do.
//
// Deliberately distinct from ErrDAGNotBuilt: "there is no graph" and "this graph did
// not come from Build" are different consumer mistakes, and a reader who gets the
// wrong one looks in the wrong place. The remedy happens to be the same — build with
// WorkflowBuilder and drive it via FromBuilder — so both messages say so.
var ErrWorkflowNoDAG = errors.New("workflow has no DAG (M23 SEAL-06: build one with WorkflowBuilder.Build and drive it via FromBuilder, rather than assembling a Workflow value directly)")

// DAG returns the graph this workflow drives, or nil if it has none. It replaces the
// exported field sealed by M23 SEAL-06 (BYPASS-10) and restores the only capability the
// field genuinely offered a consumer: inspecting the graph a Workflow was built from.
//
// "READ-ONLY" IS THE INTENT AND IT IS ALMOST TRUE — the exact residual, stated rather
// than glossed. What comes back is the live *DAG, not a copy, so the caller can reach
// every exported DAG method. After T6 those are Execute, GetLevels, GetNode, Name,
// TopologicalSort, Validate and the With* config setters. NONE of them writes topology
// — nodes, dependsOn and action have no exported writer left, and GetDependencies
// returns a defensive copy (BYPASS-05) — so the graph's SHAPE is not reachable through
// this. The With* setters do mutate execution CONFIG, and Execute drives the graph.
// Neither is new: both were already reachable by any holder of a *DAG, which is what
// Build() hands every consumer.
//
// So the honest claim is: an external caller cannot change what this workflow's graph
// IS. It is not that the returned pointer is inert.
//
// # F117-T6-03 — adding this method SILENTLY VOIDED an existing nil check
//
// Sealing a field while keeping an exported method of the SAME NAME does not break the
// old expression; it RETARGETS it. `w.DAG == nil` stopped comparing a pointer and began
// comparing a METHOD VALUE, which is never nil — always false, compiling clean. A test
// asserting "this Workflow has a graph" would have passed forever, on any Workflow, with
// or without one. The compiler had no objection; `go vet` is the only thing that caught
// it ("comparison of function DAG == nil is always false"), so vet is load-bearing for
// this class and not optional hygiene.
//
// THE HAZARD IS CONFINED TO NIL-COMPARABLE RETURNS, and that bound is measured, not
// assumed — an external probe compiled both shapes:
//
//	d.Name == ""   COMPILER ERROR: mismatched types func() string and untyped string
//	n.Name == ""   COMPILER ERROR: same
//	w.DAG == nil   COMPILES; vet-only
//
// So T1c's DAG.Name and T3's Node.Name accessors are the SAFE shape — a string
// comparison against a method value is a type error, loud and immediate. This one is not,
// because *DAG is nil-comparable. Every field-to-accessor seal that keeps the exported
// name owes the same audit, and only the pointer/interface/map/slice/func/chan returns
// need worry about it.
func (w *Workflow) DAG() *DAG { return w.dag }

// checkGraph refuses a drive on a Workflow whose graph is absent, so the public drive
// entries return a named error where they previously dereferenced nil.
//
// PLACEMENT IS AN ENUMERATION, NOT AN INSTINCT, and the obvious reading is wrong
// twice. "Guard the three public drive entries" (Execute, Tick, DeliverAndResume)
// misses that DueTimers is a FOURTH public entry which reaches the graph on its own
// via runHasHardFailure; and Tick reaches THAT deref BEFORE executeLocked, so a guard
// sited in the single funnel — the natural home, since every drive passes through it —
// leaves Tick panicking anyway. Both were established by driving the panic and reading
// the stack, not by reading the call graph.
//
// So the guard sits at the two sites that actually dereference — executeLocked and
// DueTimers — which is two statements covering all four public entries with no path
// left open. Guarding the entries as named would have been three statements covering
// three of four.
func (w *Workflow) checkGraph() error {
	if w.dag == nil {
		return fmt.Errorf("%w: workflow %q", ErrWorkflowNoDAG, w.WorkflowID)
	}
	return nil
}

// newWorkflow creates a new workflow with the given workflow store
func newWorkflow(store WorkflowStore) *Workflow {
	return &Workflow{
		dag:        newDAG("workflow"),
		WorkflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
		Store:      store,
	}
}

// AddNode adds a node to the workflow.
// Returns an error if a node with the same name already exists.
func (w *Workflow) addNode(node *Node) error {
	return w.dag.addNode(node)
}

// addDependency adds a dependency between nodes. It was exported until SEAL-06
// unexported it; `go doc Workflow` reports no AddDependency.
// Returns an error if either node doesn't exist or if adding the dependency would create a cycle.
func (w *Workflow) addDependency(from, to string) error {
	return w.dag.addDependency(from, to)
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
		effectiveCause = reconstructCause(data, w.dag) // resume path: cause was a prior run
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
	// FIRST statement of the drive body, and the position is load-bearing. Three
	// separate sites below dereference the graph — checkGraphIdentity on the resume
	// path, Validate, and the `built` token check — and the token check is the LAST of
	// the three, so it could never have served as the nil guard: a nil graph panicked
	// ~60 lines before reaching it. Anything added ahead of this line must not touch
	// the graph.
	if err := w.checkGraph(); err != nil {
		return err
	}

	// CUR-002/AUD-031: a typed-nil Store (a non-nil interface wrapping a nil concrete pointer)
	// passes every `w.Store != nil` guard below but panics on the first call through it. Reject it
	// at the drive boundary with a typed error instead of crashing the host goroutine. A genuinely
	// nil Store is legitimate (a non-durable run), so ONLY the typed-nil case is rejected.
	if w.Store != nil && interfaceHoldsNil(w.Store) {
		return fmt.Errorf("%w: Workflow.Store holds a typed-nil store value", ErrValidation)
	}

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

	// Inject the parent Store so a sub-workflow node (M19 ph91) can spawn its child under
	// the child's own ID + journal through the same store. Threaded on ctx exactly like the
	// clock — the action is handed only ctx + data, never the Store. A nil store is not
	// injected (subWorkflowAction then returns ErrSubWorkflowRequiresStore). A store already
	// in ctx (a nested child drive re-entering executeLocked) is left intact so the child
	// spawns under the SAME root store, not a re-injected one.
	if parentStoreFrom(ctx) == nil {
		ctx = withParentStore(ctx, w.Store)
	}

	// Inject the type→DAG Registry so a queue-dispatched sub-workflow node (M19 ph94) can resolve +
	// validate its child type. Threaded on ctx like the parent Store — the DAG carries only the type
	// STRING (data); the Registry (CODE) is the execution environment's. Nil is not injected (the
	// queue action then returns ErrSubWorkflowRequiresRegistry). Left intact if already present (a
	// nested child drive re-entering executeLocked shares the SAME root Registry).
	if w.registry != nil && registryFrom(ctx) == nil {
		ctx = withRegistry(ctx, w.registry)
	}

	// Inject the sub-workflow nesting ceiling (M19 ph95). Threaded on ctx like the Registry so both
	// spawn paths read one source of truth. Left intact if already present (a nested child drive
	// re-entering executeLocked inherits the ROOT ceiling — a child cannot widen its own ceiling).
	if maxSubWorkflowDepthFrom(ctx) == 0 {
		ctx = withMaxSubWorkflowDepth(ctx, w.MaxSubWorkflowDepth)
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
			// AUD-037 / P-06: a store must signal "no prior state" with ErrNotFound, never a
			// (nil, nil) return. Treating nil/nil as fresh would start the run over and
			// overwrite the real persisted state on the next Save. Reject it as a typed
			// store-contract violation rather than silently guessing it means "fresh".
			if existingData == nil {
				return fmt.Errorf("%w: store returned (nil, nil) loading workflow %q — a store must return "+
					"ErrNotFound to signal fresh state, never a nil payload with a nil error", ErrCorruptData, w.WorkflowID)
			}
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
		case errors.Is(err, ErrNotFound):
			// No prior state — start fresh with the new data.
		default:
			return fmt.Errorf("failed to load workflow state: %w", err)
		}

		// AUD-010 / C-07: definition-digest guard. The node-name check above rejects
		// a REMOVED node; the digest additionally rejects a resume onto a graph whose
		// topology, per-node retry/timeout/continue-on-error policy, compensation,
		// boundary, action KIND, or suspendability changed — any of which would
		// rehydrate state that no longer matches the graph. Additive and backward-
		// compatible: an old checkpoint carries no digest and keeps the node-name-only
		// behaviour. The current digest is stamped into the data so a future resume can
		// compare it (engine metadata under a reserved key, interim — AUD-018).
		currentDigest := w.dag.DefinitionDigest()
		if persisted, ok := data.Get(defDigestKey); ok {
			if ps, isStr := persisted.(string); isStr && ps != "" && ps != currentDigest {
				// AUD-070: a TYPED mismatch a host can classify, plus an opt-in migration
				// hook. With no handler the default is the same hard reject as before (now
				// carrying the digests); a handler may accept the change or transform the
				// loaded state in place before the drive begins.
				mm := DefinitionMismatch{WorkflowID: w.WorkflowID, PersistedDigest: ps, CurrentDigest: currentDigest}
				if w.migration != nil {
					if merr := w.migration(mm, data); merr != nil {
						return merr // host rejected, or the transform failed
					}
					// accepted: fall through and re-stamp currentDigest below so the NEXT
					// resume matches the graph the state was just migrated onto.
				} else {
					return &DefinitionMismatchError{WorkflowID: w.WorkflowID, PersistedDigest: ps, CurrentDigest: currentDigest}
				}
			}
		}
		data.setReserved(defDigestKey, currentDigest)
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
		// AUD-016 / P-05: re-attach the metrics collector from the Workflow's config.
		// On a fresh run `data` already carries it; on RESUME the loaded data carries a
		// DEFAULT (disabled) collector because JSON/FlatBuffers do not persist metrics
		// config, so without this an enabled workflow silently resumes with metrics
		// OFF. InMemory happened to preserve it through Clone; this makes every store
		// consistent. Then retain the collector so GetMetrics() reads stats back (REM-02).
		data.attachMetricsFromConfig(w.MetricsConfig)
		w.metrics = data.GetMetrics()
	}

	// Validate DAG
	if err := w.dag.Validate(); err != nil {
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
	// M23 SEAL-06 — THE SECOND REQUIRED EXECUTION CHECK, and it is not a duplicate of the
	// one in (*DAG).Execute. The rollback arm below NEVER CALLS DAG.Execute: finishRollback
	// walks w.dag.nodes / GetLevels() directly and invokes consumer compensations via
	// n.compensation.Execute. So a token checked only at DAG.Execute is absent from exactly
	// this path, and the path is live — resume-into-rollback, i.e. a crash after the
	// rolling_back marker is durable but before the final Save (the ph48 scenario),
	// reachable through Tick, DeliverAndResume and dispatch reclaim alike.
	//
	// THE TRAP THIS DEFEATS, worth naming because the arm LOOKS guarded: w.dag.Validate()
	// runs ~10 lines above. It is cycle-detection only — it does not call
	// validateReconvergence, which runs from build() and nowhere else. Passing Validate
	// says nothing about having been built.
	//
	// PLACEMENT IS AN ENUMERATION, NOT AN INFERENCE. All 17 return statements in
	// executeLocked were enumerated: the `data.IsRollingBack()` arm just below this check is
	// the ONLY exit that runs consumer code without DAG.Execute. The other finishRollback
	// call sits INSIDE DAG.Execute's
	// error handling — post-Execute, so the token is already verified there, and a second
	// check at that site would be dead code that merely READS as thoroughness.
	//
	// M23 VB-01 — WHAT THE TOKEN CERTIFIES IS NOW TWO THINGS, AND THIS ARM COVERS BOTH.
	// build() validates the consumer-declared boundaries (validateBoundaries, between
	// validateReconvergence and the stamp), so a stamped graph is one whose declarations
	// were checked against the graph they were declared on. The token is never persisted;
	// a resume re-derives both by rebuilding, which is what keeps the validated set
	// run-constant rather than reloaded.
	//
	// The coverage here is TRANSITIVE, not a second check, and that is the point: this
	// one statement already refuses an unstamped graph, and an unstamped graph is exactly
	// one whose boundaries nobody validated. So the rollback drive — which runs consumer
	// COMPENSATIONS and never calls DAG.Execute — cannot run on a graph carrying an
	// unvalidated declaration. Pinned by boundary_rollback_drive_test.go through Tick and
	// DeliverAndResume, the two entries that reach here without passing public Execute,
	// with a declaration build() is shown to refuse.
	//
	// STATED NARROWLY ON PURPOSE (DEC-M23-NAMING): "the boundaries were validated at
	// build()" is NOT "the boundaries hold over this rollback". Compensations run from
	// saga_rollback.go, outside (*Node).Execute and outside executeNodesInLevel, and
	// whether compensation edges are inside the path universe the predicate quantifies
	// over is an OPEN DECISION (OB-118-MEDIATION-ARMS) — not one this comment may settle
	// by wording.
	if !w.dag.built {
		return fmt.Errorf("%w: workflow %q", ErrDAGNotBuilt, w.WorkflowID)
	}

	if data.IsRollingBack() {
		return w.finishRollback(data, nil)
	}

	// Wire durable mid-run checkpointing when the Store supports it (M9). A Store
	// that implements Checkpointer gets a per-level checkpoint flush inside
	// DAG.Execute; a Store that does not is unaffected (no callback injected →
	// zero overhead, save-at-boundaries only). (DEC-M9, chunk 2.)
	//
	// M10-P37 T1 (MH37-5a): the callback is carried on the per-Execute ctx, NOT
	// written to the shared w.dag.config field. This makes two concurrent Execute
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
		// AUD-025/AF1: re-stamp the definition digest INSIDE the delta-capture window.
		// It was stamped above (for the AUD-010 changed-graph guard) BEFORE capture was
		// armed, so recordDelta no-op'd it — on this incremental store a delta-only PARK
		// checkpoint (SaveDeltaCheckpoint persists only captured keys) would not carry the
		// digest. A parked SQLite workflow then resumed onto a CHANGED graph silently
		// re-parked instead of ErrValidation (the guard reads an absent digest), and a host
		// recomputing the approval nonce from a raw store.Load read an empty digest — an
		// engine/host nonce drift (AUD-025/AF2). Re-stamping here lands the digest in every
		// delta checkpoint too. Idempotent (identical value) and once per drive, off the
		// per-node hot path; only the incremental (durable) path pays it, so the det-tax
		// benchmark (store-less, no capture) is unaffected.
		data.setReserved(defDigestKey, w.dag.DefinitionDigest())
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
	if err := w.dag.Execute(ctx, data); err != nil {
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
	// Trusted internal read-only scan (collect-then-report; GetNode touches the DAG, not
	// this WorkflowData) — use the non-allocating locked iterator.
	data.forEachNodeStatusLocked(func(nodeName string, _ NodeStatus) {
		if _, exists := w.dag.GetNode(nodeName); !exists {
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
	// AUD-031 / C-19: a nil builder must be a typed error, not a nil-deref panic inside
	// build() that would take the host process down.
	if builder == nil {
		return nil, fmt.Errorf("%w: FromBuilder requires a non-nil builder", ErrValidation)
	}
	// Use the guard-free build(): FromBuilder carries builder.store forward onto the
	// *Workflow below, so the store is NOT lost here — the public Build()'s
	// store-set guard (REM-04) would be wrong on this path. (M14 ph62.)
	dag, err := builder.build()
	if err != nil {
		return nil, err
	}

	return &Workflow{
		dag:        dag,
		WorkflowID: builder.workflowID,
		Store:      builder.store,
		Clock:      builder.clock,
	}, nil
}
