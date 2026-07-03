package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
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

	// Create workflow data
	data := NewWorkflowData(w.WorkflowID)

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

	// Validate DAG
	if err := w.DAG.Validate(); err != nil {
		return fmt.Errorf("workflow validation failed: %w", err)
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
	if cp, ok := w.Store.(Checkpointer); ok {
		ctx = withCheckpoint(ctx, func(d *WorkflowData) error {
			return cp.SaveCheckpoint(d)
		})
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
		// Save failed state. Do NOT ack here: the run failed, so a consumed signal
		// stays INERT in the mailbox (the node is Completed in the saved state; a
		// retry skips it — no re-consume, no double-apply). It is reclaimed by
		// Store.Delete (no background GC; ph37 F2).
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
	dag, err := builder.Build()
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
