package workflow

import (
	"context"
	"fmt"
)

// M19 ph94 — the QUEUE-dispatch producer half of the sub-workflow await. Where ph91 runs a
// definition-value non-suspendable child INLINE (blocks) and ph92 parks on a manually-signaled
// child, the QUEUE path is for a TYPE-REF (and/or suspendable) child: the parent node ENQUEUES the
// child to the M17 work_queue (via EnqueueSubWorkflow, carrying the parent's mailbox address in the
// trusted control-plane columns) and PARKS; a pool worker claims + runs the child via RunNext; on
// child-terminal RunNext delivers the completion signal to the parent's ph93 SQLite mailbox → the
// parent wakes (the ph92 park-gate) and reads the child DATA key + childRunFailed verdict — the
// uniform contract. This closes the composition pillar's queue path.

// ErrSubWorkflowRequiresRegistry is returned when a queue-dispatched sub-workflow node is reached but
// no type→DAG Registry is in scope (the workflow was driven without a Registry). The queue path needs
// the Registry to resolve the child TYPE → DAG (for the coe-verdict on wake) — the honesty analog of
// ErrWaitRequiresSignalStore / ErrSubWorkflowRequiresStore.
var ErrSubWorkflowRequiresRegistry = fmt.Errorf("%w: a queue-dispatched sub-workflow requires a Registry (inject it on the Workflow.Registry / at Execute)", ErrValidation)

// --- Registry ctx injection (M19 ph94; mirrors withParentStore/withClock/withSignalStore) ---
//
// The DAG references the child by TYPE (a data string); the Registry (which carries CODE — the
// DAGFactory closures) is the EXECUTION ENVIRONMENT's, injected on ctx at executeLocked. So the
// workflow definition stays pure DATA (the type string), exactly as RunNext takes the Registry as a
// parameter rather than baking it into the workflow.

type registryCtxKey struct{}

func withRegistry(ctx context.Context, reg *Registry) context.Context {
	if reg == nil {
		return ctx
	}
	return context.WithValue(ctx, registryCtxKey{}, reg)
}

func registryFrom(ctx context.Context) *Registry {
	if r, ok := ctx.Value(registryCtxKey{}).(*Registry); ok {
		return r
	}
	return nil
}

// queueSubWorkflowAction enqueues a TYPE-REF child + parks, then (on wake) renders the terminal
// verdict from the child journal + the child DAG (resolved from the ctx-injected Registry — symmetric
// with the inline/parked paths' need for the DAG). It reuses the ph92 park/wake gate for the wake half.
type queueSubWorkflowAction struct {
	nodeName   string // parent node name — keys the deterministic child ID + the completion-signal name
	childType  string // the M17 Registry type; resolved to the child DAG via the ctx Registry
	input      []byte // the child's seeded input (KV JSON); the parent address is carried SEPARATELY (control columns)
	resultKey  string // parent data key the child's result is written to (may be empty)
	resultFrom string // child DATA key whose value is the result (ph91 WithResult shape; may be empty)
}

func (a *queueSubWorkflowAction) suspendable() {}

// Execute: resolve the child DAG from the Registry (loud if none), enqueue the child (idempotent by
// the deterministic child ID) then park; on a wake where the child journal is terminal, render the
// verdict (reusing the ph92 parked-action logic — ONE wake path). No SignalStore →
// ErrWaitRequiresSignalStore; no Registry → ErrSubWorkflowRequiresRegistry; a non-multi-process store
// → EnqueueSubWorkflow fails loudly (the ¬{P} guard — the queue path needs the MP SQLite store + a Pool).
func (a *queueSubWorkflowAction) Execute(ctx context.Context, parentData *WorkflowData) error {
	// Nesting-DoS ceiling (M19 ph95): refuse a spawn at or past the ceiling BEFORE enqueuing.
	// On the queue path the drive-stack size is seeded from the carried work_queue.depth (Slice B),
	// so a type-ref chain A->B->C... is bounded even though each child runs in a fresh worker.
	if depthExceeded(ctx) {
		return fmt.Errorf("%w: node %q at depth %d", ErrSubWorkflowMaxDepth, a.nodeName, len(driveStackFrom(ctx)))
	}
	ss := signalStoreFrom(ctx)
	if ss == nil {
		return ErrWaitRequiresSignalStore
	}
	store := parentStoreFrom(ctx)
	if store == nil {
		return ErrSubWorkflowRequiresStore
	}
	sqlStore, ok := store.(*SQLiteStore)
	if !ok {
		return fmt.Errorf("%w: a queue-dispatched sub-workflow requires a multi-process *SQLiteStore", ErrValidation)
	}
	reg := registryFrom(ctx)
	if reg == nil {
		return ErrSubWorkflowRequiresRegistry
	}
	// Resolve the child type → DAG (the SAME factory the worker uses) for the coe-verdict on wake, and
	// to VALIDATE the type exists before enqueuing (a loud error beats a pending-forever unregistered row).
	factory, known := reg.lookup(a.childType)
	if !known {
		return fmt.Errorf("%w: queue sub-workflow %q: child type %q is not registered", ErrValidation, a.nodeName, a.childType)
	}
	childDAG, ferr := factory()
	if ferr != nil {
		return fmt.Errorf("%w: queue sub-workflow %q: child factory for type %q failed: %w", ErrValidation, a.nodeName, a.childType, ferr)
	}

	childID := subWorkflowChildID(parentData.GetWorkflowID(), a.nodeName)

	// Enqueue the child (idempotent by childID — a re-drive of the parked parent does not re-enqueue).
	// The parent address (this workflow ID + the completion-signal name) rides the trusted control
	// columns, NEVER the input BLOB (DEC-P94-PARENT-ADDRESS-COLUMN).
	//
	// The child's nesting depth is ENGINE-SET = this parent's depth + 1 (M19 ph95). The parent's depth is
	// the drive-stack size (seeded from the carried depth on the queue path — see RunNext); +1 for the child
	// about to be spawned. NEVER user-supplied — same defense-by-construction as the address columns: a user
	// cannot forge a low depth to bypass the ceiling. RunNext re-seeds this into the child's drive ctx.
	childDepth := len(driveStackFrom(ctx)) + 1
	if _, err := sqlStore.EnqueueSubWorkflow(childID, a.childType, a.input,
		parentData.GetWorkflowID(), completionSignalName(a.nodeName), childDepth); err != nil {
		return fmt.Errorf("queue sub-workflow %q: enqueue child %q: %w", a.nodeName, childID, err)
	}

	// Now behave exactly like the ph92 parked-await: check the child journal, park while non-terminal,
	// render the verdict on terminal. Delegate to the parked action (ONE wake path — no third result path).
	parked := &parkedSubWorkflowAction{nodeName: a.nodeName, child: childDAG, resultKey: a.resultKey, resultFrom: a.resultFrom}
	return parked.Execute(ctx, parentData)
}

// ErrSubWorkflowTypeCycle is returned by ValidateNoTypeCycles when the registered types form a
// statically-declarable spawn cycle (A queues B queues A). A build-time nicety, NOT the DoS guard.
var ErrSubWorkflowTypeCycle = fmt.Errorf("%w: registered sub-workflow types form a spawn cycle", ErrValidation)

// ValidateNoTypeCycles builds the type->type spawn graph over the registered factories and rejects a
// declarable cycle (A->B->A) with ErrSubWorkflowTypeCycle.
//
// EMBEDDER CONTRACT (opt-in, caller's responsibility — DEC-P95-CYCLE-CHECK-OPT-IN): this is a build-time
// validation you call ONCE, at Registry ASSEMBLY time — after registering all types and BEFORE dispatch
// (before any RunNext/Pool run). It is NOT auto-invoked: the library does not own a construction hook and
// will not add per-dispatch cost or change M17 dispatch behavior (a registry a cycle would now reject at a
// point it didn't before). The runtime depth ceiling (ErrSubWorkflowMaxDepth) is ALWAYS enforced on both
// paths and is the load-bearing DoS guarantee; this check is a build-time fail-fast on top of it, so
// skipping it weakens fail-fast diagnostics but NEVER the DoS bound. Recommended usage:
//
//	reg := NewRegistry()
//	// ... reg.Register(...) all types ...
//	if err := reg.ValidateNoTypeCycles(); err != nil { return err } // before the first RunNext/Pool run
//
// HONESTLY SCOPED (the load-bearing caveat, F-P95-04): this extracts only the queueSubWorkflowAction
// edges from each factory's TOP-LEVEL dag.Nodes. It does NOT recurse into a nested inline
// subWorkflowAction's child DAG, and a type-ref child is an opaque DAGFactory whose runtime-computed
// child type is invisible — so a cycle reachable only through an inline wrapper or a runtime-computed
// type is NOT caught here. This is a fail-fast on the COMMON directly-declared type-ref cycle, NOT the
// DoS boundary: the runtime depth ceiling (ErrSubWorkflowMaxDepth) is the load-bearing backstop that
// bounds EVERY chain, declarable or not. A factory that errors is skipped (edges unknowable); nil DAG likewise.
func (r *Registry) ValidateNoTypeCycles() error {
	// edges[t] = the set of types t statically spawns (via a queue sub-workflow node in t's DAG).
	edges := make(map[string][]string)
	for _, typ := range r.Types() {
		factory, ok := r.lookup(typ)
		if !ok {
			continue
		}
		dag, err := factory()
		if err != nil || dag == nil {
			continue // opaque/failing factory → its edges are unknowable; the depth ceiling backstops it.
		}
		for _, node := range dag.Nodes {
			if q, ok := node.Action.(*queueSubWorkflowAction); ok && q.childType != "" {
				edges[typ] = append(edges[typ], q.childType)
			}
		}
	}
	// DFS with a recursion stack (grey/black coloring) → a back-edge to a grey node is a cycle.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[string]int)
	var dfs func(t string) bool
	dfs = func(t string) bool {
		color[t] = grey
		for _, next := range edges[t] {
			switch color[next] {
			case grey:
				return true // back-edge → cycle
			case white:
				if dfs(next) {
					return true
				}
			}
		}
		color[t] = black
		return false
	}
	for _, typ := range r.Types() {
		if color[typ] == white && dfs(typ) {
			return fmt.Errorf("%w (start type %q)", ErrSubWorkflowTypeCycle, typ)
		}
	}
	return nil
}

// (compile-time interface guards for the two new suspendable actions.)
var (
	_ suspendableAction = (*queueSubWorkflowAction)(nil)
	_ suspendableAction = (*parkedSubWorkflowAction)(nil)
)
