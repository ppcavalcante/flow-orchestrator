package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// NodeBuilder provides a fluent API for configuring workflow nodes.
// It is part of the builder pattern for creating workflows.
type NodeBuilder struct {
	name            string
	action          Action
	actionErr       error // stores error if WithAction received an unsupported type
	dependencies    []string
	retryCount      int
	timeout         time.Duration
	continueOnError bool
	compensation    Action // M12 saga: optional compensating action (WithCompensation)
	compensationErr error  // stores error if WithCompensation received an unsupported type
	workflow        *WorkflowBuilder
}

// WorkflowBuilder provides a fluent API for creating workflow definitions.
// It simplifies the process of defining workflows with dependencies between nodes.
type WorkflowBuilder struct {
	nodes           []*NodeBuilder
	startNodes      []string
	workflowID      string
	store           WorkflowStore
	executionConfig *ExecutionConfig     // nil => DAG uses DefaultConfig()
	tracerProvider  trace.TracerProvider // nil => tracing off
	clock           Clock                // nil => system clock (M10 durable timers)
	choiceEdges     []choiceEdge         // ChoiceNode branch edges, folded in Build (M11)
	mergeEdges      []mergeEdge          // MergeNode join edges, folded in Build (M11 ph42)
}

// NewWorkflowBuilder creates a new workflow builder.
func NewWorkflowBuilder() *WorkflowBuilder {
	return &WorkflowBuilder{
		nodes:      make([]*NodeBuilder, 0),
		startNodes: make([]string, 0),
		workflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
	}
}

// WithWorkflowID sets the workflow ID.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithWorkflowID(id string) *WorkflowBuilder {
	b.workflowID = id
	return b
}

// WithStore sets the workflow store for persisting workflow state.
// Returns the builder for method chaining.
//
// The store engages only via FromBuilder (which returns a store-backed *Workflow).
// A bare Build() returns a *DAG that cannot carry a store, so since M14/REM-04
// Build() REJECTS a store-configured builder rather than silently running non-durable.
func (b *WorkflowBuilder) WithStore(store WorkflowStore) *WorkflowBuilder {
	b.store = store
	return b
}

// WithExecutionConfig sets the execution configuration (e.g. per-level
// concurrency) applied to the DAG produced by Build.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithExecutionConfig(config ExecutionConfig) *WorkflowBuilder {
	b.executionConfig = &config
	return b
}

// WithTracerProvider sets the OpenTelemetry trace provider used to emit a span
// per executed node (with a parent span per workflow run) on the DAG produced
// by Build. Passing nil disables tracing (the default). API-only: the host
// owns the SDK/exporter (DEC-CHUNK5).
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithTracerProvider(tp trace.TracerProvider) *WorkflowBuilder {
	b.tracerProvider = tp
	return b
}

// WithClock sets the clock used for durable timers on the workflow produced by
// FromBuilder (M10). Passing nil keeps the default system clock. Tests inject a
// FakeClock to drive durable-time scenarios deterministically.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithClock(c Clock) *WorkflowBuilder {
	b.clock = c
	return b
}

// AddTimer adds a declared durable TimerNode (M10 chunk 2): when reached it sleeps
// until clock.Now()+d (an absolute due-time frozen at the first encounter and
// persisted in the checkpoint), parking the run (Waiting) so the process can exit;
// on resume or a host Tick once the due-time has passed it fires and the run
// converges. The timer is durable DATA, not a live time.Timer — it survives
// crash/suspend and an overdue timer fires immediately on resume. Returns a
// NodeBuilder for dependency wiring (DependsOn). The timer action is set directly,
// so do NOT also call WithAction on the returned builder (that would replace the
// timer); retry/timeout are not meaningful on a timer (a park bypasses both).
func (b *WorkflowBuilder) AddTimer(name string, d time.Duration) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &timerAction{nodeName: name, duration: d}
	return node
}

// AddWaitForSignal adds a declared WaitForSignalNode (M10 ph37): when reached it
// parks the run (Waiting) until a signal named signalName is delivered to the
// workflow's durable mailbox (via DeliverSignal / DeliverAndResume), then applies
// the payload idempotently and converges. Requires a Store implementing
// SignalStore. Returns a NodeBuilder for dependency wiring; the action is set
// directly, so do NOT also call WithAction (that would replace it) — retry/timeout
// are not meaningful on a park.
func (b *WorkflowBuilder) AddWaitForSignal(name, signalName string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &waitForSignalAction{nodeName: name, signalName: signalName}
	return node
}

// AddSubWorkflow adds a declared sub-workflow node (M19 ph91): when reached it spawns and
// awaits the definition-value child DAG IN-PROCESS under a deterministic child ID
// (f(parentID, name)) with the child's own journal — parent and child are DISTINCT
// workflows (one-writer preserved). Child success writes the child's result (see
// WithResult) into parent data; child failure fails this node (INV-01 fail-fast). The
// spawn is idempotent (a re-drive after the child completed does not re-run it). Requires
// the run to have a Store (else the node returns ErrSubWorkflowRequiresStore).
//
// The child's whole spawn-closure is scanned AT BUILD for any suspendable node (an inline
// child BLOCKS the parent, so it can never park): a suspendable node anywhere in the
// closure fails Build with ErrSubWorkflowSuspendableChild — route such a child to the
// queue path (ph94) instead. The action is set directly, so do NOT also call WithAction.
// Returns a NodeBuilder for dependency wiring and result declaration (WithResult).
func (b *WorkflowBuilder) AddSubWorkflow(name string, child *DAG) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &subWorkflowAction{nodeName: name, child: child}
	// Scan the child's spawn-closure NOW; a suspendable node (direct or transitive) is a
	// build error surfaced through actionErr (Build reports it — builder.go Build()).
	if err := scanChildInlineSafe(child); err != nil {
		node.actionErr = err
	}
	return node
}

// WithResult declares that this sub-workflow node's result is the child's DATA KEY
// childDataKey, written into parent data under parentKey on child success (M19 ph91). The
// child must Set(childDataKey, ...) its result; a data key (not a node output) is read
// because data keys carry the store's typed columns, so a SCALAR result (int64 via value_long,
// plus string/bool/float) round-trips type-faithfully on all three stores (an int64 reloads
// as an int64), whereas node outputs reload as strings on FB/SQLite. A COMPLEX result
// (map/slice/nil) is NOT backend-uniform — it reloads typed on InMemory but as a JSON string
// on FB/SQLite (the same pre-existing store-wide property that governs every complex data
// value); declare a scalar result key when backend-uniformity matters. A collision with a
// pre-existing parent key (foreign value) is a loud ErrSubWorkflowResultKeyCollision at run
// time, not last-writer-wins. Only valid on a node created by AddSubWorkflow; a no-op (with a
// deferred error) otherwise.
func (n *NodeBuilder) WithResult(parentKey, childDataKey string) *NodeBuilder {
	switch a := n.action.(type) {
	case *subWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	case *parkedSubWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	case *queueSubWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	default:
		n.actionErr = fmt.Errorf("%w: WithResult is only valid on an AddSubWorkflow/AddSubWorkflowParked/AddSubWorkflowQueued node", ErrValidation)
	}
	return n
}

// WithInput sets the seeded KV input for a QUEUE-dispatched sub-workflow child (M19 ph94): the map is
// JSON-encoded into the work_queue row's input, and RunNext's seedInput sets each key as a child data
// key on the fresh run (so the child's first nodes read it). Only valid on an AddSubWorkflowQueued node
// (the inline/parked children run in-process and read the parent data directly, so they need no queue
// input). A nil/empty map is a no-op (no seed).
func (n *NodeBuilder) WithInput(kv map[string]any) *NodeBuilder {
	sub, ok := n.action.(*queueSubWorkflowAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithInput is only valid on an AddSubWorkflowQueued node", ErrValidation)
		return n
	}
	if len(kv) == 0 {
		return n
	}
	b, err := json.Marshal(kv)
	if err != nil {
		n.actionErr = fmt.Errorf("%w: cannot encode sub-workflow input: %w", ErrValidation, err)
		return n
	}
	sub.input = b
	return n
}

// AddSubWorkflowParked adds a declared PARKED sub-workflow-await node (M19 ph92): when reached
// it PARKS the run (Waiting) while the child — run OUT-OF-BAND under its deterministic ID
// f(parentID, name) — is not yet terminal; a durable completion signal delivered to the
// workflow's mailbox (SubWorkflowCompletionSignal) + a host DeliverAndResume wakes it; on wake
// it reads the child's declared result DATA key (see WithResult — the uniform ph91 contract, NOT
// the signal payload) and converges, or fails this node if the child terminalized failed
// (INV-01, coe-aware). Requires a Store implementing SignalStore (else ErrWaitRequiresSignalStore).
//
// This is the PARKED counterpart to AddSubWorkflow (which BLOCKS inline). The child is carried by
// definition-value so the parked node can render the child's coe-aware terminal verdict from the
// journal + DAG. The action is set directly (marker visible), so do NOT also call WithAction. The
// ROUTING between inline and parked is ph94; ph92 provides the parked mechanism.
func (b *WorkflowBuilder) AddSubWorkflowParked(name string, child *DAG) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &parkedSubWorkflowAction{nodeName: name, child: child}
	return node
}

// AddSubWorkflowQueued adds a QUEUE-dispatched sub-workflow node (M19 ph94): when reached it ENQUEUES
// a child of TYPE childType to the M17 work_queue (carrying this parent's mailbox address in the
// trusted control columns) and PARKS (Waiting); a pool worker claims + runs the child; on child-terminal
// a completion signal wakes this parent, which reads the child's result DATA key (WithResult) + renders
// the coe-aware verdict. This is the queue counterpart to AddSubWorkflow (inline, ph91) — the explicit
// opt-in for a TYPE-REF and/or SUSPENDABLE child (which the inline path refuses). It structurally
// requires a multi-process *SQLiteStore + a worker Pool + a Registry (the type→DAG map, injected at
// Execute — the DAG carries only the type STRING, keeping the workflow pure DATA). The action is set
// directly (marker visible), so do NOT also call WithAction. Returns a NodeBuilder for dependency wiring
// + WithResult. WithInput sets the child's seeded KV input.
func (b *WorkflowBuilder) AddSubWorkflowQueued(name, childType string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &queueSubWorkflowAction{nodeName: name, childType: childType}
	if childType == "" {
		node.actionErr = fmt.Errorf("%w: AddSubWorkflowQueued requires a non-empty child type", ErrValidation)
	}
	return node
}

// AddApproval adds a declared approval node (M19 ph90): when reached it parks the
// run (Waiting) until an approve/reject decision (an ApprovalDecision payload) is
// delivered to the workflow's durable mailbox under the SIGNAL NAME EQUAL TO THE NODE
// NAME, then acts: approve → apply the decision (persisted to the journal for audit)
// and converge; reject → fail fast with an *ApprovalRejectedError (INV-01, no
// downstream runs). Requires a Store implementing SignalStore (else the node returns
// ErrWaitRequiresSignalStore — a loud failure, never a forever-park). A host builds
// the decision Signal with ApproveSignal / RejectSignal (which derive the same name).
// Returns a NodeBuilder for dependency wiring; the action is set directly, so do NOT
// also call WithAction (that would replace it) — retry/timeout are not meaningful on a
// park.
func (b *WorkflowBuilder) AddApproval(name string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &approvalAction{nodeName: name, signalName: name}
	return node
}

// AddWaitForCondition adds a declared WaitForConditionNode (M10 ph37, "await"):
// when reached it parks the run while predicate(data) is false, re-evaluating on
// each wake (a host re-drive), and converges when it flips. Returns a NodeBuilder
// for dependency wiring; the action is set directly, so do NOT also call WithAction.
func (b *WorkflowBuilder) AddWaitForCondition(name string, predicate func(*WorkflowData) bool) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &waitForConditionAction{predicate: predicate}
	return node
}

// AddNode adds a regular node to the workflow and returns a NodeBuilder for
// further configuration.
func (b *WorkflowBuilder) AddNode(name string) *NodeBuilder {
	node := &NodeBuilder{
		name:         name,
		dependencies: make([]string, 0),
		workflow:     b,
	}
	b.nodes = append(b.nodes, node)
	return node
}

// AddStartNode adds a starting node (no dependencies) to the workflow and
// returns a NodeBuilder for further configuration.
func (b *WorkflowBuilder) AddStartNode(name string) *NodeBuilder {
	node := b.AddNode(name)
	b.startNodes = append(b.startNodes, name)
	return node
}

// WithAction sets the action for the node.
// The action can be an Action interface or a function with the signature
// func(ctx context.Context, data *WorkflowData) error.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithAction(action interface{}) *NodeBuilder {
	switch a := action.(type) {
	case Action:
		// Already an Action
		n.action = a
	case func(ctx context.Context, data *WorkflowData) error:
		// Function with new signature
		n.action = ActionFunc(a)
	case func(ctx context.Context, state interface{}) (interface{}, interface{}):
		// Legacy style function, adapt it to new Action
		n.action = ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// This is a simple wrapper that ignores the return values
			// In a real implementation, you might want to handle errors or state deltas
			_, _ = a(ctx, data)
			return nil
		})
	default:
		// Record the error for Build() to report, and create a stub action
		// so callers that test the action directly get a clear error message.
		n.actionErr = fmt.Errorf("unsupported action type: %T", action)
		n.action = ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			return n.actionErr
		})
	}
	return n
}

// DependsOn specifies dependencies for this node by name.
// Returns the builder for method chaining.
func (n *NodeBuilder) DependsOn(deps ...string) *NodeBuilder {
	n.dependencies = append(n.dependencies, deps...)
	return n
}

// WithRetries sets the number of retries for the node.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithRetries(count int) *NodeBuilder {
	n.retryCount = count
	return n
}

// WithTimeout sets a timeout for the node execution.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithTimeout(timeout time.Duration) *NodeBuilder {
	n.timeout = timeout
	return n
}

// WithContinueOnError marks the node so that a failure does not fail the
// workflow. The node is recorded as Failed and the rest of the DAG continues;
// dependents may inspect the node's Failed status (via WorkflowData.GetNodeStatus)
// and branch on it. Default (unset) preserves the fail-fast behavior.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithContinueOnError() *NodeBuilder {
	n.continueOnError = true
	return n
}

// WithCompensation sets the compensating action for this node (M12 saga). If the
// workflow fails with a hard error and rolls back, a Completed node's compensation
// is invoked in reverse-topological order under a FRESH context to durably undo its
// effect, and the node is then marked Compensated. Accepts an Action or a
// func(ctx, *WorkflowData) error (the same forms as WithAction). An unsupported
// type is recorded and reported by Build(). A compensation MUST be idempotent — it
// may be re-invoked after a crash mid-rollback (at-least-once); the executor passes
// it a stable IdempotencyKey handle. A node with no compensation is a rollback
// no-op. Returns the builder for method chaining.
func (n *NodeBuilder) WithCompensation(action interface{}) *NodeBuilder {
	switch a := action.(type) {
	case Action:
		n.compensation = a
	case func(ctx context.Context, data *WorkflowData) error:
		n.compensation = ActionFunc(a)
	default:
		n.compensationErr = fmt.Errorf("unsupported compensation type: %T", action)
	}
	return n
}

// Build creates a DAG from the workflow definition.
// Returns an error if the workflow definition is invalid (e.g., has cycles).
//
// M14 ph62 (REM-04): Build REFUSES when a store was configured via WithStore. A
// bare *DAG does NOT carry a store — DAG.Execute has no persistence — so
// WithStore(s).Build().Execute would run SILENTLY NON-DURABLE, discarding the store
// the caller explicitly set (a silent durability-loss footgun). To build a durable,
// store-backed run, use FromBuilder (returns a *Workflow whose Execute uses the
// store) or construct a *Workflow directly. This guard turns the silent lie into a
// loud, self-documenting error; the store-less Build() path is unchanged.
func (b *WorkflowBuilder) Build() (*DAG, error) {
	if b.store != nil {
		return nil, fmt.Errorf(
			"%w: WithStore configures a durable Workflow, but Build returns a bare DAG that cannot carry a store (Execute would silently run non-durable) — build with FromBuilder(b) to get a store-backed *Workflow instead",
			ErrValidation,
		)
	}
	return b.build()
}

// build is the guard-free DAG construction, used by Build (after the store guard)
// and by FromBuilder (which DOES carry the store forward onto the *Workflow, so the
// store is not lost — the guard would be wrong there). (M14 ph62 REM-04.)
func (b *WorkflowBuilder) build() (*DAG, error) {
	// Create a new DAG with capacity hints based on the number of nodes
	nodeCount := len(b.nodes)
	dag := NewDAGWithCapacity(b.workflowID, nodeCount)

	// Apply a custom execution config if one was provided; otherwise the DAG
	// keeps its DefaultConfig().
	if b.executionConfig != nil {
		dag.WithExecutionConfig(*b.executionConfig)
	}

	// Apply a tracer provider if one was set. Done after WithExecutionConfig so
	// it survives a custom config (which would otherwise reset the field) — the
	// builder's WithTracerProvider is the source of truth for tracing here.
	if b.tracerProvider != nil {
		dag.WithTracerProvider(b.tracerProvider)
	}

	// Fold ChoiceNode branch edges (choice -> target) and MergeNode join edges
	// (merge -> tail) into the dependency lists. Done FIRST so the existing
	// count/wire/cycle-check passes treat them like any other edge, and so the
	// When/Otherwise/From wiring is independent of node-declaration order (a
	// target/tail may be declared before or after the call). (M11.)
	if len(b.choiceEdges) > 0 || len(b.mergeEdges) > 0 {
		builderByName := make(map[string]*NodeBuilder, nodeCount)
		for _, nb := range b.nodes {
			builderByName[nb.name] = nb
		}
		for _, e := range b.choiceEdges {
			target, ok := builderByName[e.target]
			if !ok {
				return nil, fmt.Errorf("choice %q routes to unknown branch target %q", e.choice, e.target)
			}
			target.dependencies = append(target.dependencies, e.choice)
		}
		for _, e := range b.mergeEdges {
			merge, ok := builderByName[e.merge]
			if !ok {
				return nil, fmt.Errorf("merge %q not found while wiring its tails", e.merge)
			}
			if _, ok := builderByName[e.tail]; !ok {
				return nil, fmt.Errorf("merge %q joins unknown branch tail %q", e.merge, e.tail)
			}
			merge.dependencies = append(merge.dependencies, e.tail)
		}
	}

	// Map to track node dependency counts for capacity hints
	nodeDependencyCounts := make(map[string]int, nodeCount)

	// First pass: count dependencies per node
	for _, builder := range b.nodes {
		for _, depName := range builder.dependencies {
			nodeDependencyCounts[depName]++
		}
	}

	// Create real nodes from builders
	for _, builder := range b.nodes {
		if builder.actionErr != nil {
			return nil, fmt.Errorf("node %s has invalid action: %w", builder.name, builder.actionErr)
		}
		if builder.compensationErr != nil {
			return nil, fmt.Errorf("node %s has invalid compensation: %w", builder.name, builder.compensationErr)
		}
		if builder.action == nil {
			return nil, fmt.Errorf("node %s has no action defined", builder.name)
		}

		// Use capacity hints for dependencies
		depCapacity := len(builder.dependencies)
		node := NewNodeWithCapacity(builder.name, builder.action, depCapacity)

		if builder.retryCount > 0 {
			node.WithRetries(builder.retryCount)
		}
		if builder.timeout > 0 {
			node.WithTimeout(builder.timeout)
		}
		if builder.continueOnError {
			node.WithContinueOnError()
		}
		node.Compensation = builder.compensation // M12 saga: nil when no WithCompensation

		// Add node to DAG
		if err := dag.AddNode(node); err != nil {
			return nil, fmt.Errorf("failed to add node %s: %w", builder.name, err)
		}
	}

	// Add dependencies
	for _, builder := range b.nodes {
		if len(builder.dependencies) == 0 {
			continue
		}

		node, exists := dag.GetNode(builder.name)
		if !exists {
			return nil, fmt.Errorf("node %s not found", builder.name)
		}

		deps := make([]*Node, 0, len(builder.dependencies))

		// Collect all dependencies first
		for _, depName := range builder.dependencies {
			depNode, exists := dag.GetNode(depName)
			if !exists {
				return nil, fmt.Errorf("dependency %s for node %s not found",
					depName, builder.name)
			}
			deps = append(deps, depNode)
		}

		// Add all dependencies in one operation
		node.AddDependencies(deps...)
	}

	// Validate the DAG
	if err := dag.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	// Strict reconvergence validation (M11 ph42, D-P42-STRICT): only structured,
	// single-Choice, local OR-joins are expressible. Runs after the cycle-check
	// so a rejected graph never reaches the executor, and so the runtime OR-join
	// semantics can rely on "a merge reconverges exactly one Choice".
	if err := validateReconvergence(dag); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	return dag, nil
}
